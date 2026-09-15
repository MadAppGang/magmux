package client

// The Go plugin SDK, against a fake magmux on a real unix socket.
//
// A fake rather than the real multiplexer because this package must not import
// it — and because what is under test is the SDK's half of the conversation:
// the registration it sends, the invokes it dispatches, the results it returns
// and the cancellation it honours. mux's own TestPlugin* suite covers the other
// half against a real magmux.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// fakeMux is one connection's worth of magmux: it writes the connect-time
// aggregate, answers `capabilities`, records everything it receives, and lets a
// test push lines at the plugin.
type fakeMux struct {
	t    *testing.T
	path string
	ln   net.Listener

	mu    sync.Mutex
	msgs  []map[string]any
	conn  net.Conn
	wake  chan struct{}
	ready chan struct{}
}

func startFakeMux(t *testing.T) *fakeMux {
	t.Helper()
	// Not t.TempDir(): on darwin its path plus a test name overruns sun_path,
	// so the same harness passes or fails according to what the test is called.
	dir, err := os.MkdirTemp("", "magmux-sdk")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMux{t: t, path: path, ln: ln, wake: make(chan struct{}, 64), ready: make(chan struct{})}
	t.Cleanup(func() {
		ln.Close()
		os.RemoveAll(dir)
	})
	go f.serve()
	return f
}

func (f *fakeMux) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	close(f.ready)

	// The aggregate is the first line on every connection, plugin or not.
	fmt.Fprintln(conn, `{"type":"snapshot","panes":[{"pane":0,"state":"running"}]}`)

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var msg map[string]any
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		f.mu.Lock()
		f.msgs = append(f.msgs, msg)
		f.mu.Unlock()
		select {
		case f.wake <- struct{}{}:
		default:
		}

		// Everything with an id gets exactly one reply, which is what the SDK's
		// register, snapshot and EmitSync wait for.
		if id, ok := msg["id"]; ok {
			_ = json.NewEncoder(conn).Encode(map[string]any{
				"type": "reply", "id": id, "ok": true,
				"result": map[string]any{"name": msg["name"], "rev": 3},
			})
		}
	}
}

func (f *fakeMux) send(msg map[string]any) {
	f.t.Helper()
	<-f.ready
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		f.t.Fatalf("write to the plugin: %v", err)
	}
}

func (f *fakeMux) await(what string, want func(map[string]any) bool) map[string]any {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	seen := 0
	for {
		f.mu.Lock()
		for ; seen < len(f.msgs); seen++ {
			if want(f.msgs[seen]) {
				msg := f.msgs[seen]
				seen++
				f.mu.Unlock()
				return msg
			}
		}
		f.mu.Unlock()
		select {
		case <-f.wake:
		case <-deadline:
			f.t.Fatalf("the plugin never sent %s", what)
		}
	}
}

func (f *fakeMux) awaitType(typ string) map[string]any {
	f.t.Helper()
	return f.await(typ, func(m map[string]any) bool { return m["type"] == typ })
}

// ── tests ───────────────────────────────────────────────────────────────────

// TestPluginSDKRegistersAndServesInvocations is the SDK's whole contract in one
// run: what it announces, how a call reaches a handler, and what comes back.
func TestPluginSDKRegistersAndServesInvocations(t *testing.T) {
	f := startFakeMux(t)

	ran := make(chan Invocation, 1)
	p, err := NewPlugin(PluginConfig{
		Name:    "ticket",
		Version: "0.1.0",
		Sock:    f.path,
		Token:   "a-token",
		Events:  []string{"progress"},
		Ops: []PluginOp{{
			Name:        "run_ticket",
			Description: "run one",
			Class:       protocol.ClassControl,
			Handler: func(ctx context.Context, req Invocation) (map[string]any, error) {
				ran <- req
				return map[string]any{"ticket": "t1"}, nil
			},
		}, {
			Name:        "boom",
			Description: "always fails",
			Class:       protocol.ClassRead,
			Handler: func(context.Context, Invocation) (map[string]any, error) {
				return nil, protocol.Errf(protocol.CodeNoSuchPane, "pane 9 is not mine")
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	reg := f.awaitType(protocol.MsgPluginRegister)
	if reg["name"] != "ticket" || reg["token"] != "a-token" {
		t.Errorf("registration = %v", reg)
	}
	ops, _ := reg["ops"].([]any)
	if len(ops) != 2 {
		t.Fatalf("registered %d ops, want 2: %v", len(ops), reg["ops"])
	}
	first, _ := ops[0].(map[string]any)
	if first["name"] != "run_ticket" || first["class"] != "control" {
		t.Errorf("op spec = %v", first)
	}
	if first["schema"] == nil {
		t.Error("an op with no schema must advertise an empty OBJECT schema, not null: an MCP " +
			"client turns it straight into a tool's inputSchema")
	}

	t.Run("an invocation reaches its handler and is answered", func(t *testing.T) {
		f.send(map[string]any{
			"type": protocol.MsgInvoke, "call": "c1", "op": "run_ticket",
			"args":       map[string]any{"title": "build it"},
			"caller":     map[string]any{"transport": "ws", "conn": "ws#3", "client": "web/0.1"},
			"deadlineMs": 30000,
		})
		select {
		case inv := <-ran:
			if inv.Caller.Transport != "ws" || inv.Caller.Client != "web/0.1" {
				t.Errorf("the caller did not reach the handler: %+v", inv.Caller)
			}
			var args map[string]any
			if err := json.Unmarshal(inv.Args, &args); err != nil || args["title"] != "build it" {
				t.Errorf("args = %s (%v)", inv.Args, err)
			}
			if time.Until(inv.Deadline) > 31*time.Second {
				t.Errorf("deadline = %v, want ~30s away", inv.Deadline)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the handler never ran")
		}

		res := f.await("the result for c1", func(m map[string]any) bool {
			return m["type"] == protocol.MsgInvokeResult && m["call"] == "c1"
		})
		if res["ok"] != true {
			t.Fatalf("result = %v", res)
		}
		if r, _ := res["result"].(map[string]any); r["ticket"] != "t1" {
			t.Errorf("the handler's payload did not survive: %v", res["result"])
		}
	})

	t.Run("a failure keeps its code", func(t *testing.T) {
		f.send(map[string]any{"type": protocol.MsgInvoke, "call": "c2", "op": "boom"})
		res := f.await("the result for c2", func(m map[string]any) bool {
			return m["type"] == protocol.MsgInvokeResult && m["call"] == "c2"
		})
		if res["ok"] != false || res["code"] != protocol.CodeNoSuchPane {
			t.Errorf("result = %v, want the handler's own code", res)
		}
	})

	t.Run("an unknown op is answered rather than ignored", func(t *testing.T) {
		// magmux only invokes what the plugin registered, so this is a bug on
		// one side or the other — and answering is what stops magmux waiting out
		// the whole deadline for it.
		f.send(map[string]any{"type": protocol.MsgInvoke, "call": "c3", "op": "nope"})
		res := f.await("the result for c3", func(m map[string]any) bool {
			return m["type"] == protocol.MsgInvokeResult && m["call"] == "c3"
		})
		if res["ok"] != false || res["code"] != protocol.CodeUnknownVerb {
			t.Errorf("result = %v", res)
		}
	})

	t.Run("events and snapshots take their own shapes", func(t *testing.T) {
		pane := 3
		if err := p.Emit("progress", &pane, map[string]any{"step": "sent"}); err != nil {
			t.Fatal(err)
		}
		ev := f.awaitType(protocol.MsgPluginEvent)
		if ev["event"] != "progress" || ev["pane"] != float64(3) {
			t.Errorf("event = %v", ev)
		}
		if _, hasID := ev["id"]; hasID {
			t.Error("Emit is fire and forget; an id would make magmux answer a hot path")
		}

		if err := p.PushSnapshot(context.Background(), protocol.ControllerSnapshot{
			Pane: &pane, State: "awaiting_input", Response: "DONE: t1",
		}); err != nil {
			t.Fatalf("PushSnapshot: %v", err)
		}
		snap := f.awaitType(protocol.MsgControllerSnapshot)
		if snap["state"] != "awaiting_input" || snap["response"] != "DONE: t1" {
			t.Errorf("snapshot = %v", snap)
		}
		if _, hasID := snap["id"]; !hasID {
			t.Error("PushSnapshot waits for an answer, so it must carry an id: a snapshot magmux " +
				"refused is something the plugin has to know about")
		}
	})
}

// TestPluginSDKCancelsAHandler: an invoke_cancel must reach the handler that is
// still running. Without it a plugin keeps working for an answer nobody will
// read, and magmux's own deadline cannot help — the work is on the plugin's
// side of the socket.
func TestPluginSDKCancelsAHandler(t *testing.T) {
	f := startFakeMux(t)

	started := make(chan struct{})
	stopped := make(chan error, 1)
	p, err := NewPlugin(PluginConfig{
		Name:  "slow",
		Sock:  f.path,
		Token: "a-token",
		Ops: []PluginOp{{
			Name:        "wait",
			Description: "waits to be cancelled",
			Class:       protocol.ClassControl,
			Handler: func(ctx context.Context, _ Invocation) (map[string]any, error) {
				close(started)
				<-ctx.Done()
				stopped <- ctx.Err()
				return nil, protocol.Errf(protocol.CodeNotReady, "cancelled")
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	f.awaitType(protocol.MsgPluginRegister)

	f.send(map[string]any{"type": protocol.MsgInvoke, "call": "c9", "op": "wait", "deadlineMs": 60000})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}

	f.send(map[string]any{"type": protocol.MsgInvokeCancel, "call": "c9"})
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the handler's ctx ended with %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invoke_cancel never reached the handler; it is still working for an answer " +
			"nobody will read")
	}
}

// TestNewPluginRefusesWhatMagmuxWould: the SDK checks the same grammar the host
// does, so a mistake is a compile-and-run away rather than a round trip away.
func TestNewPluginRefusesWhatMagmuxWould(t *testing.T) {
	ok := []PluginOp{{Name: "run", Description: "d", Class: protocol.ClassControl,
		Handler: func(context.Context, Invocation) (map[string]any, error) { return nil, nil }}}

	cases := []struct {
		what string
		cfg  PluginConfig
	}{
		{"an underscore in the plugin name", PluginConfig{Name: "ticket_runner", Ops: ok}},
		{"no ops at all", PluginConfig{Name: "ticket"}},
		{"an op with no handler", PluginConfig{Name: "ticket", Ops: []PluginOp{{Name: "run", Class: protocol.ClassRead}}}},
		{"an op with no class", PluginConfig{Name: "ticket", Ops: []PluginOp{{Name: "run",
			Handler: func(context.Context, Invocation) (map[string]any, error) { return nil, nil }}}}},
		{"an undeclarable event", PluginConfig{Name: "ticket", Ops: ok, Events: []string{"Nope!"}}},
	}
	for _, c := range cases {
		c.cfg.Sock, c.cfg.Token = "/dev/null", "t"
		if _, err := NewPlugin(c.cfg); err == nil {
			t.Errorf("NewPlugin accepted %s", c.what)
		}
	}

	// And the two environment defaults, which are what makes a plugin's main
	// three lines long.
	t.Setenv("MAGMUX_SOCK", "/tmp/whatever.sock")
	t.Setenv("MAGMUX_PLUGIN_TOKEN", "issued-by-magmux")
	p, err := NewPlugin(PluginConfig{Name: "ticket", Ops: ok})
	if err != nil {
		t.Fatalf("NewPlugin with the environment magmux provides: %v", err)
	}
	if p.cfg.Sock != "/tmp/whatever.sock" || p.cfg.Token != "issued-by-magmux" {
		t.Errorf("the environment was not used: %+v", p.cfg)
	}
}
