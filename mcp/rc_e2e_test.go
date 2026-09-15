package mcp

// The P6 end-to-end gate: a REAL magmux, a REAL plugin, and `magmux mcp` as a
// separate process speaking JSON-RPC on stdio.
//
// Everything else in this package tests against a fake socket, which is right
// for protocol shape and wrong for this claim. What is being asserted here is
// that the pieces fit: that a plugin started by magmux becomes an MCP TOOL in
// another process, that a pane becomes a RESOURCE, that subscribing to one and
// then typing into it produces a notification, and that a plugin's own events
// reach the client after the call that started them has already returned. A
// fake can be made to do all four by construction; only the real thing proves
// it.
//
// It also enforces the rule the whole server rests on, on the real stream: the
// child writes JSON-RPC to stdout and nothing else. Every line the harness
// reads is checked, so one stray fmt.Println anywhere in main() fails this test.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ── the child MCP server ────────────────────────────────────────────────────

type mcpChild struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	msgs   chan map[string]any
	stderr *strings.Builder

	mu   sync.Mutex
	seen []map[string]any
	raw  []string
	// buf: see rcHarness. Responses and notifications interleave freely here,
	// because tools/call and resources/read each answer from their own
	// goroutine.
	buf []map[string]any
}

func startMCPChild(t *testing.T, bin, sockDir string) *mcpChild {
	t.Helper()
	cmd := exec.Command(bin, "mcp")
	cmd.Env = append(os.Environ(),
		"MAGMUX_SOCK_DIR="+sockDir,
		// The log goes to a file so stderr carries only what the process could
		// not help writing, and stdout is left to be judged on its own.
		"MAGMUX_MCP_LOG="+filepath.Join(sockDir, "mcp.log"),
	)
	// MAGMUX_SOCK would make the child attach to whatever magmux is running
	// THIS test, if there is one. Discovery in sockDir is the path under test.
	cmd.Env = append(cmd.Env, "MAGMUX_SOCK=")

	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errBuf strings.Builder
	var errMu sync.Mutex
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux mcp: %v", err)
	}

	c := &mcpChild{t: t, cmd: cmd, in: in, msgs: make(chan map[string]any, 512), stderr: &errBuf}
	go func() {
		sc := bufio.NewScanner(errPipe)
		for sc.Scan() {
			errMu.Lock()
			errBuf.WriteString(sc.Text() + "\n")
			errMu.Unlock()
		}
	}()
	go func() {
		defer close(c.msgs)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			c.mu.Lock()
			c.raw = append(c.raw, line)
			c.mu.Unlock()
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Errorf("magmux mcp wrote a non-JSON line to stdout: %q", line)
				continue
			}
			if m["jsonrpc"] != "2.0" {
				t.Errorf("magmux mcp wrote a non-JSON-RPC line to stdout: %q", line)
				continue
			}
			c.msgs <- m
		}
	}()
	t.Cleanup(func() {
		_ = in.Close()
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return c
}

func (c *mcpChild) send(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatalf("write to magmux mcp: %v", err)
	}
}

func (c *mcpChild) await(what string, timeout time.Duration, pred func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for i, m := range c.buf {
			if pred(m) {
				c.buf = append(c.buf[:i:i], c.buf[i+1:]...)
				c.mu.Unlock()
				return m
			}
		}
		c.mu.Unlock()
		select {
		case m, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("magmux mcp exited while waiting for %s\nstderr: %s", what, c.stderr.String())
			}
			c.mu.Lock()
			c.seen = append(c.seen, m)
			if !pred(m) {
				c.buf = append(c.buf, m)
			}
			c.mu.Unlock()
			if pred(m) {
				return m
			}
		case <-deadline:
			c.t.Fatalf("never saw %s within %v\nstderr: %s", what, timeout, c.stderr.String())
			return nil
		}
	}
}

func (c *mcpChild) reply(id float64, timeout time.Duration) map[string]any {
	c.t.Helper()
	m := c.await(fmt.Sprintf("a response to id %v", id), timeout, func(m map[string]any) bool {
		got, ok := m["id"].(float64)
		return ok && got == id
	})
	if rerr, ok := m["error"].(map[string]any); ok {
		c.t.Fatalf("request %v failed: %v", id, rerr)
	}
	res, _ := m["result"].(map[string]any)
	return res
}

func (c *mcpChild) notification(method string, timeout time.Duration) map[string]any {
	c.t.Helper()
	return c.await(method, timeout, func(m map[string]any) bool { return m["method"] == method })
}

// ── the real magmux ─────────────────────────────────────────────────────────

// e2eMagmux starts a headless magmux in its own socket directory and waits for
// the socket to accept.
type e2eMagmux struct {
	cmd     *exec.Cmd
	dir     string
	sock    string
	id      string
	stderr  *syncWriter
	stopped chan struct{}
}

type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func startE2EMagmux(t *testing.T, bin string, args ...string) *e2eMagmux {
	t.Helper()
	// /tmp, not t.TempDir(): on darwin TMPDIR plus the test's own name overruns
	// sun_path, and a unix socket under it fails to bind.
	dir, err := os.MkdirTemp("/tmp", "mmx-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	full := append([]string{"--headless", "--sock-dir", dir}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Stdin = nil
	stderr := &syncWriter{}
	cmd.Stdout, cmd.Stderr = io.Discard, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	m := &e2eMagmux{
		cmd: cmd, dir: dir, stderr: stderr, stopped: make(chan struct{}),
		id:   fmt.Sprintf("%d", cmd.Process.Pid),
		sock: filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", cmd.Process.Pid)),
	}
	go func() { _ = cmd.Wait(); close(m.stopped) }()
	t.Cleanup(func() {
		select {
		case <-m.stopped:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-m.stopped:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", m.sock); err == nil {
			conn.Close()
			return m
		}
		select {
		case <-m.stopped:
			t.Fatalf("magmux exited before binding its socket\nstderr: %s", stderr.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("magmux never bound %s\nstderr: %s", m.sock, stderr.String())
	return nil
}

// e2eBinary builds the binary under test. Always a fresh build: `magmux mcp`
// and the multiplexer are the SAME binary, so a stale one in the repo root
// would test the previous version of this very package.
func e2eBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "magmux")
	build := exec.Command("go", "build", "-o", bin, "../cmd/magmux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build magmux: %v\n%s", err, out)
	}
	return bin
}

func demoPluginCommand(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun is not installed; the ticket-runner demo is a TypeScript plugin")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "examples", "plugins", "ticket-runner", "main.ts")
	if _, err := os.Stat(main); err != nil {
		t.Fatalf("the demo plugin is missing: %s", main)
	}
	return "bun " + main
}

// TestMCPServerDrivesARealMagmux is the P6 end-to-end gate. It logs its timings
// because "it worked" and "it worked in under a second" are different claims.
func TestMCPServerDrivesARealMagmux(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("subprocess test requires darwin or linux")
	}
	plugin := demoPluginCommand(t)

	t0 := time.Now()
	bin := e2eBinary(t)
	t.Logf("built magmux in %v", time.Since(t0).Round(time.Millisecond))

	t1 := time.Now()
	m := startE2EMagmux(t, bin, "--plugin", plugin, "-e", "sh")
	t.Logf("magmux accepting after %v (session %s)", time.Since(t1).Round(time.Millisecond), m.id)

	t2 := time.Now()
	c := startMCPChild(t, bin, m.dir)
	c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"clientInfo":      map[string]any{"name": "magmux-e2e", "version": "1.0"},
			"capabilities":    map[string]any{},
		}})
	init := c.reply(1, 15*time.Second)
	t.Logf("initialize answered in %v", time.Since(t2).Round(time.Millisecond))
	caps, _ := init["capabilities"].(map[string]any)
	res, _ := caps["resources"].(map[string]any)
	if res["subscribe"] != true || res["listChanged"] != true {
		t.Fatalf("resources capability = %v", res)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	// 1. tools/list: the nine static tools plus the plugin's op.
	//
	// The plugin is a bun process magmux has just started, so it may not have
	// registered by the time the handshake ends. The DESIGNED answer to that is
	// notifications/tools/list_changed, so this waits for it rather than
	// polling — and that makes the notification part of what is asserted.
	t3 := time.Now()
	c.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	tools := e2eToolNames(c.reply(2, 20*time.Second))
	for _, want := range []string{"list_sessions", "attach_session", "request_session",
		"list_panes", "open_pane", "close_pane", "read_pane", "send_keys", "send_and_wait"} {
		if !tools[want] {
			t.Fatalf("tools/list is missing the static tool %s: %v", want, e2eKeys(tools))
		}
	}
	if !tools["ticket__run_ticket"] {
		c.notification("notifications/tools/list_changed", 45*time.Second)
		c.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
		tools = e2eToolNames(c.reply(3, 20*time.Second))
	}
	if !tools["ticket__run_ticket"] {
		t.Fatalf("the plugin's op never became an MCP tool: %v", e2eKeys(tools))
	}
	t.Logf("ticket__run_ticket visible after %v", time.Since(t3).Round(time.Millisecond))

	// 2. resources/list: a screen resource for the live pane.
	t4 := time.Now()
	c.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "resources/list"})
	uris := e2eResourceURIs(c.reply(4, 20*time.Second))
	paneURI := fmt.Sprintf("magmux://%s/pane/0/screen", m.id)
	if !uris[paneURI] {
		t.Fatalf("resources/list has no %s: %v", paneURI, e2eKeys(uris))
	}
	eventsURI := fmt.Sprintf("magmux://%s/plugin/ticket/events", m.id)
	if !uris[eventsURI] {
		t.Fatalf("resources/list has no %s: %v", eventsURI, e2eKeys(uris))
	}
	t.Logf("resources/list answered in %v", time.Since(t4).Round(time.Millisecond))

	// 3. subscribe, then type into the pane: the screen moves, and the client is
	//    told to re-read it.
	t5 := time.Now()
	c.send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "resources/subscribe",
		"params": map[string]any{"uri": paneURI}})
	c.reply(5, 20*time.Second)
	c.send(map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{"name": "send_keys", "arguments": map[string]any{
			"pane": 0, "text": "echo MAGMUX_RC_OK"}}})
	c.reply(6, 30*time.Second)
	note := c.await("notifications/resources/updated for the pane", 30*time.Second,
		func(msg map[string]any) bool {
			if msg["method"] != "notifications/resources/updated" {
				return false
			}
			p, _ := msg["params"].(map[string]any)
			return p["uri"] == paneURI
		})
	_ = note
	t.Logf("subscribe -> typing -> updated in %v", time.Since(t5).Round(time.Millisecond))

	c.send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "resources/read",
		"params": map[string]any{"uri": paneURI}})
	screen := e2eContentsText(t, c.reply(7, 20*time.Second))
	if !strings.Contains(screen, "MAGMUX_RC_OK") {
		t.Errorf("the pane resource does not show what was typed:\n%s", screen)
	}

	// 4. the plugin op, and its progress events.
	t6 := time.Now()
	c.send(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "resources/subscribe",
		"params": map[string]any{"uri": eventsURI}})
	c.reply(8, 20*time.Second)

	c.send(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "ticket__run_ticket", "arguments": map[string]any{
			"title": "rc-e2e", "body": "prove the loop"}}})
	result := c.reply(9, 60*time.Second)
	if result["isError"] == true {
		t.Fatalf("ticket__run_ticket failed: %v", result)
	}
	t.Logf("run_ticket returned in %v", time.Since(t6).Round(time.Millisecond))

	// The events arrive as NOTIFICATIONS, outside the call — validation
	// criterion 5. run_ticket returns as soon as the ticket is underway, so
	// everything the plugin reports is either a notification or lost.
	//
	// The strict ordering claim (the event written to stdout after the result)
	// belongs to TestPluginEventReachesTheClientAfterTheToolResult, where the
	// plugin's timing is staged. Here the real plugin emits `progress` while
	// run_ticket is still running, so either order is legitimate and asserting
	// one would be asserting a race.
	t7 := time.Now()
	c.await("notifications/resources/updated for the plugin events", 60*time.Second,
		func(msg map[string]any) bool {
			if msg["method"] != "notifications/resources/updated" {
				return false
			}
			p, _ := msg["params"].(map[string]any)
			return p["uri"] == eventsURI
		})
	t.Logf("plugin event notification seen %v after the tool result was read",
		time.Since(t7).Round(time.Millisecond))

	c.send(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "resources/read",
		"params": map[string]any{"uri": eventsURI}})
	body := e2eContentsText(t, c.reply(10, 20*time.Second))
	var ring struct {
		Plugin string `json:"plugin"`
		Epoch  uint64 `json:"epoch"`
		Next   uint64 `json:"next"`
		Events []struct {
			Seq   uint64 `json:"seq"`
			Event string `json:"event"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &ring); err != nil {
		t.Fatalf("the events resource is not JSON: %v\n%s", err, body)
	}
	progress := 0
	for _, e := range ring.Events {
		if e.Event == "progress" {
			progress++
		}
	}
	if progress == 0 {
		t.Fatalf("the plugin's progress events never reached the client: %s", body)
	}
	t.Logf("read %d events (%d progress), epoch %d, next %d", len(ring.Events), progress, ring.Epoch, ring.Next)

	// 5. stdout hygiene, on the real stream. Every line was checked as it was
	//    read; this reports the volume so a silent scanner cannot pass.
	c.mu.Lock()
	lines := len(c.raw)
	c.mu.Unlock()
	if lines < 10 {
		t.Fatalf("only %d lines came back from magmux mcp; the test proved little", lines)
	}
	t.Logf("%d JSON-RPC lines on stdout, none of them anything else", lines)
	t.Logf("total %v", time.Since(t0).Round(time.Millisecond))
}

func e2eToolNames(res map[string]any) map[string]bool {
	out := map[string]bool{}
	raw, _ := res["tools"].([]any)
	for _, rt := range raw {
		m, _ := rt.(map[string]any)
		if name, ok := m["name"].(string); ok {
			out[name] = true
		}
	}
	return out
}

func e2eResourceURIs(res map[string]any) map[string]bool {
	out := map[string]bool{}
	raw, _ := res["resources"].([]any)
	for _, rr := range raw {
		m, _ := rr.(map[string]any)
		if uri, ok := m["uri"].(string); ok {
			out[uri] = true
		}
	}
	return out
}

func e2eKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func e2eContentsText(t *testing.T, res map[string]any) string {
	t.Helper()
	raw, _ := res["contents"].([]any)
	if len(raw) == 0 {
		t.Fatalf("resource read returned no contents: %v", res)
	}
	m, _ := raw[0].(map[string]any)
	text, _ := m["text"].(string)
	return text
}
