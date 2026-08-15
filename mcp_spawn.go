package main

// Session discovery for `magmux mcp`.
//
// magmux binds /tmp/magmux-<pid>.sock by default, or /tmp/magmux-<name>.sock
// with --id, so discovery is a directory scan plus a probe. The probe is
// genuinely free: a connection's first line is guaranteed to be the aggregate
// pane snapshot, so one dial and one read tells us the pane count and every
// pane's state — and it works against a legacy magmux, which is the point.
//
// Deliberately absent: spawning. We never fork a magmux and we never exec
// tmux. A magmux that nobody is watching defeats the whole design, in which a
// human sees every pane the agent drives; request_session hands the command to
// the human (or to the client's own tmux tooling) instead.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// sockDir is where magmux binds its sockets (main.go:3091).
const sockDir = "/tmp"

// sockNamePattern matches both shapes: the pid default and a --id name. The
// name half is deliberately permissive — it is whatever the human typed.
var sockNamePattern = regexp.MustCompile(`^magmux-(.+)\.sock$`)

// SessionInfo is one candidate socket and what we could learn about it without
// committing to it.
//
// Stale and inaccessible sockets are surfaced rather than hidden: a SIGKILLed
// magmux leaves its socket file behind, and /tmp is shared between users on
// macOS, so "there is a file but you cannot use it" is a real and confusing
// state that the agent should be able to explain to the human. We never unlink
// a socket — we do not own those files.
type SessionInfo struct {
	ID        string
	SockPath  string
	PID       int
	Alive     bool // the pid in the name is running (pid-named sockets only)
	Reachable bool // dialled and answered with a snapshot
	Stale     bool // the file exists but nothing is listening
	Self      bool // this is the magmux we are running inside
	Panes     []map[string]any
	Err       string
}

// discoverSessions scans for magmux sockets and probes them in parallel,
// returning within roughly budget.
func discoverSessions(ctx context.Context, budget time.Duration) []SessionInfo {
	if budget <= 0 {
		budget = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	entries, err := os.ReadDir(sockDir)
	if err != nil {
		return nil
	}

	hostSock := os.Getenv("MAGMUX_SOCK")

	var candidates []SessionInfo
	for _, e := range entries {
		m := sockNamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		path := filepath.Join(sockDir, e.Name())
		info := SessionInfo{ID: m[1], SockPath: path}

		// A pid-named socket can be checked for liveness before we dial: a
		// dead pid means the file is definitely stale, and saying so is more
		// useful than a bare connection refusal.
		if pid, err := strconv.Atoi(m[1]); err == nil {
			info.PID = pid
			// EPERM means the process exists but belongs to another user,
			// which is still alive for our purposes.
			kerr := syscall.Kill(pid, 0)
			info.Alive = kerr == nil || errors.Is(kerr, syscall.EPERM)
		}
		if hostSock != "" && path == hostSock {
			info.Self = true
		}
		if fi, err := e.Info(); err == nil && fi.Mode()&fs.ModeSocket == 0 {
			continue // a regular file that happens to be named like a socket
		}
		candidates = append(candidates, info)
	}

	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func(ci *SessionInfo) {
			defer wg.Done()
			panes, err := probeSocket(ctx, ci.SockPath, budget)
			if err != nil {
				ci.Err = err.Error()
				switch {
				case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ENOENT):
					ci.Stale = true
				case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
					ci.Err = "permission denied — the socket belongs to another user"
				}
				return
			}
			ci.Reachable = true
			ci.Panes = panes
		}(&candidates[i])
	}
	wg.Wait()

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Reachable != candidates[j].Reachable {
			return candidates[i].Reachable
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates
}

// probeSocket dials, reads exactly ONE line, and closes.
//
// That one line is the connect-time aggregate snapshot magmux writes before it
// registers the connection for broadcasts, so it is guaranteed to be first and
// guaranteed to describe every pane. Reading further would make the probe a
// subscriber, which is not what a probe should be.
func probeSocket(ctx context.Context, path string, timeout time.Duration) ([]map[string]any, error) {
	if timeout <= 0 {
		timeout = time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	// The reader may buffer past the newline; that is harmless because we
	// close the connection immediately and never become a subscriber.
	br := bufio.NewReaderSize(conn, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}

	var ev struct {
		Type  string           `json:"type"`
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("not a magmux socket: %w", err)
	}
	if ev.Type != "snapshot" || ev.Panes == nil {
		return nil, fmt.Errorf("not a magmux socket: first line was %q", ev.Type)
	}
	return ev.Panes, nil
}
