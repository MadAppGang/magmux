package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/internal/difftest"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/proclive"
)

// Live mode (§8.2, validation V2). It reads the real Claude root READ-ONLY:
// tt is given it as CLAUDE_CONFIG_DIR and the library as RootAt, so both
// compare a linkScanPath against the same lexical projects path. tt's HOME,
// state and temp directories are the sandbox's, a tmux stub stands IN FRONT
// of the real tmux (nothing is removed from PATH, so node stays reachable),
// and by default a claude stub exits 127 so both sides report the listing
// unavailable. -real-agents drops that stub and keeps the real HOME; tt's
// agents cache and lock then land under the sandbox's XDG_STATE_HOME, and the
// library is fed the same payload from that cache, so one capture feeds both.
//
// Every registry and job file's mtime is recorded before tt runs and after
// the library has run. A row whose file moved is RACED: its differences are
// reported and do not count as uncited. Processes are probed with kill(pid, 0)
// only.

func liveRoot() (string, error) {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return filepath.Clean(d), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".claude"), nil
}

// mtimes is every registry entry's and every job record's modification time.
func mtimes(root string) map[string]time.Time {
	out := map[string]time.Time{}
	fsys := os.DirFS(root)
	if es, err := fs.ReadDir(fsys, "sessions"); err == nil {
		for _, e := range es {
			if info, err := e.Info(); err == nil {
				out["sessions/"+e.Name()] = info.ModTime()
			}
		}
	}
	if es, err := fs.ReadDir(fsys, "jobs"); err == nil {
		for _, e := range es {
			if !e.IsDir() {
				continue
			}
			if info, err := fs.Stat(fsys, "jobs/"+e.Name()+"/state.json"); err == nil {
				out["jobs/"+e.Name()+"/state.json"] = info.ModTime()
			}
		}
	}
	return out
}

func moved(before, after map[string]time.Time) map[string]bool {
	out := map[string]bool{}
	for k, t := range before {
		if a, ok := after[k]; !ok || !a.Equal(t) {
			out[k] = true
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			out[k] = true
		}
	}
	return out
}

func runLive(sb *sandbox, o options) int {
	root, err := liveRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	const dir = "live"
	for _, d := range []string{"/home", "/state", "/tmp", "/bin"} {
		if err := sb.mkdir(dir + d); err != nil {
			fmt.Fprintln(os.Stderr, "_diff:", err)
			return exitSetup
		}
	}
	if err := sb.writeFile(dir+"/bin/tmux", []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	if !o.realAgents {
		if err := sb.writeFile(dir+"/bin/claude", []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "_diff:", err)
			return exitSetup
		}
	}
	var env []string
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		switch name {
		case "PATH", "XDG_STATE_HOME", "TMPDIR", "CLAUDE_CONFIG_DIR", "TMUX", "TMUX_PANE":
			continue
		case "HOME":
			if !o.realAgents {
				continue
			}
		}
		env = append(env, e)
	}
	env = append(env,
		"PATH="+sb.path(dir+"/bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_STATE_HOME="+sb.path(dir+"/state"),
		"TMPDIR="+sb.path(dir+"/tmp"),
		"CLAUDE_CONFIG_DIR="+root,
	)
	if !o.realAgents {
		env = append(env, "HOME="+sb.path(dir+"/home"))
	}

	before := mtimes(root)
	doc, _, err := runTT(sb, env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	ttRows, observedAt, err := difftest.FromTT(doc, filepath.Join(root, "projects"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	lst := fixedListing{err: errNoClaude}
	if o.realAgents {
		raw, at, err := cachedListing(sb.path(dir + "/state/tt/agents-cache.json"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "_diff: tt's agents cache:", err)
			return exitSetup
		}
		lst = fixedListing{raw: raw, at: at}
	}
	r, err := ports.RootAt(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	d, err := runLibrary(r, proclive.New(), lst, observedAt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "_diff:", err)
		return exitSetup
	}
	changed := moved(before, mtimes(root))

	libRows := difftest.FromModel(d.Doc, d.Problems)
	racedKeys := map[difftest.RowKey]bool{}
	for _, l := range libRows {
		if (l.Lib.RegistryFile != "" && changed["sessions/"+l.Lib.RegistryFile]) ||
			(l.Lib.JobDir != "" && changed["jobs/"+l.Lib.JobDir+"/state.json"]) {
			racedKeys[l.Key] = true
		}
	}
	diffs := difftest.Compare(ttRows, libRows, d.Problems)
	var kept []difftest.Diff
	for _, x := range diffs {
		if !racedKeys[x.Key] {
			kept = append(kept, x)
		}
	}
	rep := difftest.Summarise(kept)
	for k := range racedKeys {
		rep.Raced = append(rep.Raced, k)
	}
	slices.SortFunc(rep.Raced, func(a, b difftest.RowKey) int { return strings.Compare(a.String(), b.String()) })
	fmt.Printf("\n== live %s (%d tt rows, %d library rows, %d files moved during the run)\n", root, len(ttRows), len(libRows), len(changed))
	printReport(rep)
	fmt.Println("   watcher parity: NOT VERIFIED by construction (§8.3)")
	switch {
	case len(rep.Uncited) > 0:
		return exitUncited
	case len(rep.Raced) > 0:
		return exitNotVerified
	}
	return exitPass
}

// cachedListing reads the payload tt cached from its own spawn.
func cachedListing(file string) ([]byte, time.Time, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, time.Time{}, err
	}
	var env struct {
		ObservedAt string          `json:"observedAt"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, time.Time{}, err
	}
	if len(env.Payload) == 0 {
		return nil, time.Time{}, errors.New("no payload")
	}
	at, err := time.Parse(time.RFC3339Nano, env.ObservedAt)
	if err != nil {
		return nil, time.Time{}, err
	}
	return env.Payload, at, nil
}
