package mux

// Plugins, end to end, over the REAL socket.
//
// Every test here drives a real magmux process with a fake plugin on the other
// end of its own connection, and that is not scene-setting: the whole feature
// is about what one connection may do that another may not. An in-process fake
// could not tell those apart, because the thing being tested — "this pane
// belongs to the plugin that registered on THIS socket" — exists only on real
// connections.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/protocol"
)

// ── harness ─────────────────────────────────────────────────────────────────

// pluginMagmux is a headless magmux with a session token, so a fake plugin can
// register the way a developer's hand-run plugin does.
type pluginMagmux struct {
	*headlessMagmux
	token string
}

func startPluginMagmux(t *testing.T, args ...string) *pluginMagmux {
	t.Helper()
	tok, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// Inherited by the child. MAGMUX_TOKEN with no --listen means exactly one
	// thing — a plugin started by hand may register — which is the shape a
	// developer debugging a plugin uses and the cheapest one to test.
	t.Setenv("MAGMUX_TOKEN", tok)
	h := startHeadlessMagmux(t, append([]string{"--headless"}, args...)...)
	m := &pluginMagmux{headlessMagmux: h, token: tok}
	// The socket exists before the layout does; waitPluginSock only proves the
	// path is connectable, and every connection then waits for the layout on
	// magmux's side.
	m.waitSock(t)
	return m
}

func (m *pluginMagmux) waitSock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", m.sock); err == nil {
			c.Close()
			return
		}
		select {
		case <-m.exited:
			t.Fatalf("magmux exited before its socket appeared\nstderr: %s", m.stderr.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("magmux never bound %s\nstderr: %s", m.sock, m.stderr.String())
}

// fakePlugin is one connection, with a synchronous reader. Synchronous on
// purpose: every assertion here is about ORDER — a registration before an
// open_pane, a reply after a registry write — and a background reader would
// turn each of those into a poll with a timeout.
type fakePlugin struct {
	t    *testing.T
	conn net.Conn
	sc   *bufio.Scanner
	ids  atomic.Int64
}

func dialPlugin(t *testing.T, sock string) *fakePlugin {
	t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if conn, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	f := &fakePlugin{t: t, conn: conn, sc: sc}
	// The connect-time aggregate is the first line on every connection, plugin
	// or not: a plugin is an ordinary client that also registers.
	if ev := f.next(); ev["type"] != "snapshot" {
		t.Fatalf("first line was %v, want the aggregate snapshot", ev["type"])
	}
	return f
}

func (f *fakePlugin) send(msg map[string]any) {
	f.t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.conn.Write(append(b, '\n')); err != nil {
		f.t.Fatalf("write: %v", err)
	}
}

func (f *fakePlugin) nextID() string { return fmt.Sprintf("q%d", f.ids.Add(1)) }

// next reads one line, or fails the test at EOF.
func (f *fakePlugin) next() map[string]any {
	f.t.Helper()
	for f.sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(f.sc.Bytes(), &ev); err != nil {
			continue
		}
		return ev
	}
	if err := f.sc.Err(); err != nil {
		f.t.Fatalf("read: %v", err)
	}
	f.t.Fatal("the connection ended while waiting for a line")
	return nil
}

// await reads until a line satisfies want, failing after a bounded number of
// lines so a wrong assumption is a message rather than a hang.
func (f *fakePlugin) await(what string, want func(map[string]any) bool) map[string]any {
	f.t.Helper()
	for i := 0; i < 200; i++ {
		ev := f.next()
		if want(ev) {
			return ev
		}
	}
	f.t.Fatalf("no %s in 200 lines", what)
	return nil
}

func (f *fakePlugin) awaitReply(id string) map[string]any {
	f.t.Helper()
	return f.await("reply to "+id, func(ev map[string]any) bool {
		return ev["type"] == "reply" && fmt.Sprint(ev["id"]) == id
	})
}

func (f *fakePlugin) awaitType(typ string) map[string]any {
	f.t.Helper()
	return f.await(typ, func(ev map[string]any) bool { return ev["type"] == typ })
}

// request sends a message with an id and returns its reply.
func (f *fakePlugin) request(msg map[string]any) map[string]any {
	f.t.Helper()
	id := f.nextID()
	msg["id"] = id
	f.send(msg)
	return f.awaitReply(id)
}

// serve answers every invoke that arrives, from now until the connection ends.
//
// The connection is DEDICATED to it from that moment: this goroutine owns the
// scanner, and a synchronous read beside it would have the two swallowing each
// other's lines. It fails nothing on its own — a test goroutine may not call
// t.Fatal — so a plugin that stops answering surfaces as the caller's timeout,
// which is what it would be in the field too.
func (f *fakePlugin) serve(result map[string]any) {
	go func() {
		for f.sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(f.sc.Bytes(), &ev) != nil {
				continue
			}
			if ev["type"] != protocol.MsgInvoke {
				continue
			}
			line, err := json.Marshal(map[string]any{
				"type": protocol.MsgInvokeResult, "call": ev["call"], "ok": true, "result": result,
			})
			if err != nil {
				return
			}
			if _, err := f.conn.Write(append(line, '\n')); err != nil {
				return
			}
		}
	}()
}

// register makes a well-formed registration, with whatever overrides the test
// wants patched over it.
func (f *fakePlugin) register(name, token string, patch map[string]any) map[string]any {
	f.t.Helper()
	msg := map[string]any{
		"type":    protocol.MsgPluginRegister,
		"token":   token,
		"name":    name,
		"version": "0.1.0",
		"ops": []any{
			map[string]any{"name": "echo", "description": "echo the args back", "class": "read"},
			map[string]any{"name": "work", "description": "pretend to work", "class": "control"},
		},
		"events": []any{"progress"},
	}
	for k, v := range patch {
		msg[k] = v
	}
	return f.request(msg)
}

func replyFailed(t *testing.T, ev map[string]any, code string) {
	t.Helper()
	if ok, _ := ev["ok"].(bool); ok {
		t.Fatalf("the request succeeded; it should have failed with %s: %v", code, ev)
	}
	if got := fmt.Sprint(ev["code"]); got != code {
		t.Errorf("code = %s, want %s (error: %v)", got, code, ev["error"])
	}
}

// ── registration ────────────────────────────────────────────────────────────

// TestPluginRegistrationIsValidated covers the whole admission decision. Each
// case is a different way a plugin can be refused, and they are refused for
// different reasons — a caller that could not tell them apart would not know
// whether to rename, to re-read its token, or to stop the other copy of itself.
func TestPluginRegistrationIsValidated(t *testing.T) {
	m := startPluginMagmux(t, "-e", "sleep 60")

	t.Run("a name outside the grammar", func(t *testing.T) {
		f := dialPlugin(t, m.sock)
		// Underscore, specifically: it is excluded so that a qualified op name
		// splits unambiguously at its first `__` for MCP.
		replyFailed(t, f.register("ticket_runner", m.token, nil), protocol.CodeBadRequest)
		replyFailed(t, f.register("Ticket", m.token, nil), protocol.CodeBadRequest)
		replyFailed(t, f.register("9lives", m.token, nil), protocol.CodeBadRequest)
	})

	t.Run("magmux's own name", func(t *testing.T) {
		f := dialPlugin(t, m.sock)
		replyFailed(t, f.register("magmux", m.token, nil), protocol.CodeBadRequest)
	})

	t.Run("a wrong token", func(t *testing.T) {
		other, _ := auth.Generate()
		f := dialPlugin(t, m.sock)
		replyFailed(t, f.register("thief", other, nil), protocol.CodeUnauthorized)
		replyFailed(t, f.register("thief", "", nil), protocol.CodeUnauthorized)
	})

	t.Run("an op outside the grammar", func(t *testing.T) {
		f := dialPlugin(t, m.sock)
		replyFailed(t, f.register("shouty", m.token, map[string]any{
			"ops": []any{map[string]any{"name": "RunTicket", "class": "control"}},
		}), protocol.CodeBadRequest)
		replyFailed(t, f.register("classless", m.token, map[string]any{
			"ops": []any{map[string]any{"name": "run", "class": "whatever"}},
		}), protocol.CodeBadRequest)
		replyFailed(t, f.register("opless", m.token, map[string]any{"ops": []any{}}),
			protocol.CodeBadRequest)
		replyFailed(t, f.register("noisy", m.token, map[string]any{"events": []any{"NOT OK"}}),
			protocol.CodeBadRequest)
	})

	t.Run("a good registration, and then a duplicate", func(t *testing.T) {
		f := dialPlugin(t, m.sock)
		res := replyOK(t, f.register("ticket", m.token, nil))
		if res["name"] != "ticket" {
			t.Errorf("the reply does not name the plugin: %v", res)
		}
		if rev, _ := res["rev"].(float64); rev <= 0 {
			t.Errorf("rev = %v, want the op list's revision", res["rev"])
		}

		// The ops are in the table under their qualified names, and the class a
		// plugin declared rides with them.
		ops := replyOK(t, f.request(map[string]any{"type": "ops"}))
		names := map[string]string{}
		for _, raw := range ops["ops"].([]any) {
			spec := raw.(map[string]any)
			names[fmt.Sprint(spec["name"])] = fmt.Sprint(spec["source"])
		}
		if names["ticket.echo"] != "ticket" {
			t.Errorf("ticket.echo is not registered under the plugin's own source: %v", names)
		}
		if names["list"] != protocol.SourceBuiltin {
			t.Errorf("a built-in stopped being magmux's: %v", names)
		}

		// A second connection cannot take the same name. Nor can this one
		// register twice: one plugin per connection.
		g := dialPlugin(t, m.sock)
		replyFailed(t, g.register("ticket", m.token, nil), protocol.CodeBadRequest)
		replyFailed(t, f.register("second", m.token, nil), protocol.CodeBadRequest)
	})
}

// ── invoke ──────────────────────────────────────────────────────────────────

// TestPluginInvokeAnswersTimesOutAndCancels walks one op through its three
// endings. They are three different messages to the caller on purpose: an
// answer, a `timeout` that invites a longer budget, and — for the plugin — the
// `invoke_cancel` that says the work is no longer wanted.
func TestPluginInvokeAnswersTimesOutAndCancels(t *testing.T) {
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))

	client := dialPlugin(t, m.sock)

	t.Run("an answered call", func(t *testing.T) {
		id := client.nextID()
		client.send(map[string]any{
			"type": "call", "op": "ticket.echo", "id": id,
			"args": map[string]any{"say": "hello"},
		})

		inv := p.awaitType(protocol.MsgInvoke)
		if inv["op"] != "echo" {
			t.Errorf("op = %v, want the BARE name: a plugin should not have to strip its own "+
				"name off every request", inv["op"])
		}
		if args, _ := inv["args"].(map[string]any); args["say"] != "hello" {
			t.Errorf("args did not survive: %v", inv["args"])
		}
		caller, _ := inv["caller"].(map[string]any)
		if caller["transport"] != "socket" || caller["conn"] == "" {
			t.Errorf("the caller is not described: %v", inv["caller"])
		}
		if ms, _ := inv["deadlineMs"].(float64); ms <= 0 {
			t.Errorf("deadlineMs = %v; a plugin is told how long it has", inv["deadlineMs"])
		}

		p.send(map[string]any{
			"type": protocol.MsgInvokeResult, "call": inv["call"], "ok": true,
			"result": map[string]any{"said": "hello"},
		})
		res := replyOK(t, client.awaitReply(id))
		if res["said"] != "hello" {
			t.Errorf("result = %v, want the plugin's own payload", res)
		}
	})

	t.Run("a failure keeps the plugin's own code", func(t *testing.T) {
		id := client.nextID()
		client.send(map[string]any{"type": "call", "op": "ticket.work", "id": id})
		inv := p.awaitType(protocol.MsgInvoke)
		p.send(map[string]any{
			"type": protocol.MsgInvokeResult, "call": inv["call"], "ok": false,
			"code": protocol.CodeNoSuchPane, "error": "pane 9 is not one of mine",
		})
		ev := client.awaitReply(id)
		replyFailed(t, ev, protocol.CodeNoSuchPane)
		if !strings.Contains(fmt.Sprint(ev["error"]), "not one of mine") {
			t.Errorf("the plugin's own wording was lost: %v", ev["error"])
		}
	})

	t.Run("a call nobody answers times out and is cancelled", func(t *testing.T) {
		id := client.nextID()
		start := time.Now()
		client.send(map[string]any{
			"type": "call", "op": "ticket.work", "id": id, "timeoutMs": 400,
		})
		inv := p.awaitType(protocol.MsgInvoke)

		replyFailed(t, client.awaitReply(id), protocol.CodeTimeout)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("the timeout took %v; the budget was 400ms", elapsed)
		}
		// And the plugin is told, so it can stop: a handler that keeps working
		// for an answer nobody will read is the one thing a deadline cannot fix
		// on its own.
		cancel := p.awaitType(protocol.MsgInvokeCancel)
		if cancel["call"] != inv["call"] {
			t.Errorf("cancelled call %v, want %v", cancel["call"], inv["call"])
		}
	})
}

// ── events ──────────────────────────────────────────────────────────────────

// TestPluginEventsAreGated pins the declared-event rule and the size cap.
//
// The gate is what makes a plugin's stream describable: a client that has never
// heard of this plugin can be told what it emits, and an event nobody declared
// would be a message type that exists only when it happens.
func TestPluginEventsAreGated(t *testing.T) {
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))
	sub := dialPlugin(t, m.sock)

	t.Run("a declared event reaches every subscriber", func(t *testing.T) {
		p.send(map[string]any{
			"type": protocol.MsgPluginEvent, "event": "progress", "pane": 0,
			"data": map[string]any{"step": "sent"},
		})
		ev := sub.awaitType(protocol.EventPlugin)
		if ev["plugin"] != "ticket" || ev["event"] != "progress" {
			t.Fatalf("event = %v", ev)
		}
		if data, _ := ev["data"].(map[string]any); data["step"] != "sent" {
			t.Errorf("the plugin's own payload did not survive: %v", ev["data"])
		}
		if ev["at"] == nil {
			t.Error("the event carries no timestamp")
		}
	})

	t.Run("an undeclared event is refused", func(t *testing.T) {
		replyFailed(t, p.request(map[string]any{
			"type": protocol.MsgPluginEvent, "event": "surprise",
		}), protocol.CodeBadRequest)
	})

	t.Run("an oversize event is refused", func(t *testing.T) {
		replyFailed(t, p.request(map[string]any{
			"type": protocol.MsgPluginEvent, "event": "progress",
			"data": map[string]any{"blob": strings.Repeat("x", 70*1024)},
		}), protocol.CodeTooLarge)
	})

	t.Run("an unregistered connection cannot emit", func(t *testing.T) {
		replyFailed(t, sub.request(map[string]any{
			"type": protocol.MsgPluginEvent, "event": "progress",
		}), protocol.CodeForbidden)
	})
}

// ── identity ────────────────────────────────────────────────────────────────

// TestPluginSelfOpenPaneNeedsRegistration is the architecture's named test, and
// the whole of the identity model in one place: a connection claims a pane by
// BEING a registered plugin, never by naming one.
func TestPluginSelfOpenPaneNeedsRegistration(t *testing.T) {
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))

	var claimed int
	t.Run("a registered plugin claims its pane", func(t *testing.T) {
		// The demo's exact line.
		res := replyOK(t, p.request(map[string]any{
			"type": "open_pane", "cmd": "sleep 30", "label": "t1", "controller": "self",
		}))
		claimed = int(res["pane"].(float64))

		list := replyOK(t, p.request(map[string]any{"type": "list"}))
		for _, raw := range list["panes"].([]any) {
			pane := raw.(map[string]any)
			if int(pane["pane"].(float64)) != claimed {
				continue
			}
			if pane["controller"] != "plugin:ticket" {
				t.Fatalf("controller = %v, want plugin:ticket", pane["controller"])
			}
			return
		}
		t.Fatalf("pane %d is missing from list: %v", claimed, list["panes"])
	})

	t.Run("an unregistered connection is forbidden", func(t *testing.T) {
		other := dialPlugin(t, m.sock)
		replyFailed(t, other.request(map[string]any{
			"type": "open_pane", "cmd": "sleep 30", "label": "t2", "controller": "self",
		}), protocol.CodeForbidden)

		// The no-id twin opens nothing, and says nothing — the legacy path's
		// contract. Proven by asking afterwards: a pane that had been opened
		// would be there.
		other.send(map[string]any{
			"type": "open_pane", "cmd": "sleep 30", "label": "t3", "controller": "self",
		})
		list := replyOK(t, other.request(map[string]any{"type": "list"}))
		for _, raw := range list["panes"].([]any) {
			if raw.(map[string]any)["label"] == "t3" {
				t.Fatal("a no-id open_pane from an unregistered connection opened a pane anyway")
			}
		}
	})

	t.Run("naming a plugin is not claiming to be one", func(t *testing.T) {
		// Even the CORRECT name of the plugin actually asking. Accepting a name
		// would make the field an identity claim.
		replyFailed(t, p.request(map[string]any{
			"type": "open_pane", "cmd": "sleep 30", "controller": "plugin:ticket",
		}), protocol.CodeBadRequest)
		replyFailed(t, p.request(map[string]any{
			"type": "open_pane", "cmd": "sleep 30", "controller": "claude-code",
		}), protocol.CodeBadRequest)
	})

	t.Run("a snapshot is accepted only from the owning plugin", func(t *testing.T) {
		replyOK(t, p.request(map[string]any{
			"type": protocol.MsgControllerSnapshot, "pane": claimed,
			"state": "working", "prompt": "do the thing",
		}))

		// A second plugin, correctly registered, still may not describe a pane
		// that is not its own.
		q := dialPlugin(t, m.sock)
		replyOK(t, q.register("other", m.token, nil))
		replyFailed(t, q.request(map[string]any{
			"type": protocol.MsgControllerSnapshot, "pane": claimed, "state": "gone",
		}), protocol.CodeForbidden)

		// Nor may a connection that registered nothing at all.
		anon := dialPlugin(t, m.sock)
		replyFailed(t, anon.request(map[string]any{
			"type": protocol.MsgControllerSnapshot, "pane": claimed, "state": "gone",
		}), protocol.CodeForbidden)

		// And a pane nobody claimed cannot be described either.
		replyFailed(t, p.request(map[string]any{
			"type": protocol.MsgControllerSnapshot, "pane": 0, "state": "gone",
		}), protocol.CodeForbidden)
	})
}

// ── agreement ───────────────────────────────────────────────────────────────

// TestPluginSnapshotAndResultsAgree is CLAUDE.md's rule applied to a
// plugin-observed pane: the live `snapshot` event and the shutdown `results`
// must never contradict each other.
//
// They are built from different things — the live event from the controller's
// own state, `results` from the pane's inputReady — and a controller that
// reported a finished turn while results said "running" would leave a driver
// unable to decide which to believe. The bridge is applyControllerSnapshot, and
// this is the test that would notice it going missing for plugins.
func TestPluginSnapshotAndResultsAgree(t *testing.T) {
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))

	res := replyOK(t, p.request(map[string]any{
		"type": "open_pane", "cmd": "sleep 30", "label": "t1", "controller": "self",
	}))
	pane := int(res["pane"].(float64))

	sub := dialPlugin(t, m.sock)
	replyOK(t, p.request(map[string]any{
		"type": protocol.MsgControllerSnapshot, "pane": pane,
		"state": "awaiting_input", "response": "DONE: t1", "project": "ticket",
	}))

	live := sub.await("the live snapshot for the plugin's pane", func(ev map[string]any) bool {
		if ev["type"] != "snapshot" || ev["panes"] != nil {
			return false
		}
		n, ok := ev["pane"].(float64)
		return ok && int(n) == pane
	})
	if live["state"] != "awaiting_input" || live["response"] != "DONE: t1" {
		t.Fatalf("the live snapshot does not carry what the plugin pushed: %v", live)
	}
	if live["controller"] != "plugin:ticket" {
		t.Errorf("controller = %v; provenance is the point of the prefix", live["controller"])
	}

	// Now end the session and read the authoritative report. SIGTERM runs the
	// normal teardown — quiesce, build results, finalize — which is the path
	// `results` is delivered on.
	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signalling magmux: %v", err)
	}
	results := sub.awaitType(protocol.EventResults)
	for _, raw := range results["panes"].([]any) {
		entry := raw.(map[string]any)
		if int(entry["pane"].(float64)) != pane {
			continue
		}
		if entry["state"] != "awaiting_input" {
			t.Fatalf("results says %v and the live snapshot said awaiting_input; the two reports "+
				"of one pane must never disagree: %v", entry["state"], entry)
		}
		if entry["response"] != "DONE: t1" {
			t.Errorf("results lost the plugin's answer: %v", entry)
		}
		if entry["controller"] != "plugin:ticket" {
			t.Errorf("results lost the provenance: %v", entry)
		}
		return
	}
	t.Fatalf("pane %d is missing from results: %v", pane, results["panes"])
}

// ── death ───────────────────────────────────────────────────────────────────

// TestPluginDeathUnregistersAndFailsInFlight covers everything that must happen
// when a plugin goes away, and the ORDER of two of them.
//
// ops_changed is published after the ops are gone, so a client that re-fetches
// on the news cannot be handed the dead plugin's ops again; plugin_exited then
// names the plugin. An in-flight call fails with plugin_gone rather than the
// caller's own timeout, because "the plugin died" and "the plugin is slow" send
// a caller to different places.
func TestPluginDeathUnregistersAndFailsInFlight(t *testing.T) {
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))

	client := dialPlugin(t, m.sock)
	sub := dialPlugin(t, m.sock)

	// A call in flight: the plugin has received the invoke and will never
	// answer it, because it is about to die.
	id := client.nextID()
	client.send(map[string]any{"type": "call", "op": "ticket.work", "id": id, "timeoutMs": 60000})
	p.awaitType(protocol.MsgInvoke)

	start := time.Now()
	p.conn.Close()

	ev := client.awaitReply(id)
	replyFailed(t, ev, protocol.CodePluginGone)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the in-flight call waited %v for a plugin that had already gone; it should "+
			"fail at once rather than burn its 60s budget", elapsed)
	}

	sub.awaitType(protocol.EventOpsChanged)
	exited := sub.awaitType(protocol.EventPluginExited)
	if exited["plugin"] != "ticket" {
		t.Errorf("plugin_exited names %v", exited["plugin"])
	}

	// The ops are gone from the table, and calling one is an ordinary unknown
	// verb from here on.
	ops := replyOK(t, client.request(map[string]any{"type": "ops"}))
	for _, raw := range ops["ops"].([]any) {
		if strings.HasPrefix(fmt.Sprint(raw.(map[string]any)["name"]), "ticket.") {
			t.Fatalf("a dead plugin's ops are still advertised: %v", raw)
		}
	}
	replyFailed(t, client.request(map[string]any{"type": "call", "op": "ticket.work"}),
		protocol.CodeUnknownVerb)
}

// TestPluginDeathLeavesThePaneObservedByTheTerminal is the other half of death,
// and the one that decides whether a pane is merely degraded or actively wrong.
//
// p.controller is write-once, so a pane whose plugin died KEEPS its controller.
// That controller must go on merging what the terminal itself saw — otherwise
// the pane freezes on whatever the plugin last said, and a driver waiting for
// the turn to end waits for a process that no longer exists.
func TestPluginDeathLeavesThePaneObservedByTheTerminal(t *testing.T) {
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))

	// A pane that prints something and then sits idle: the terminal's own
	// heuristics will call it idle a few seconds after its last output.
	res := replyOK(t, p.request(map[string]any{
		"type": "open_pane", "cmd": "echo hello-from-the-pane; sleep 120",
		"label": "t1", "controller": "self",
	}))
	pane := int(res["pane"].(float64))
	replyOK(t, p.request(map[string]any{
		"type": protocol.MsgControllerSnapshot, "pane": pane, "state": "working",
	}))

	// Connected BEFORE the death, not after: plugin_exited is a live broadcast
	// and is never replayed, so a subscriber that arrives afterwards waits for
	// news it has already missed.
	client := dialPlugin(t, m.sock)

	// The observer dies with the pane reported as working.
	p.conn.Close()
	client.awaitType(protocol.EventPluginExited)

	// The pane must still reach awaiting_input on magmux's own evidence. The
	// text-idle sweep needs a few seconds of quiet, so this is a poll with a
	// generous bound rather than a single read.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		list := replyOK(t, client.request(map[string]any{"type": "list"}))
		for _, raw := range list["panes"].([]any) {
			entry := raw.(map[string]any)
			if int(entry["pane"].(float64)) != pane {
				continue
			}
			if entry["controller"] != "plugin:ticket" {
				t.Fatalf("the controller was detached; p.controller is write-once and the pane "+
					"must keep it: %v", entry)
			}
			if entry["state"] == "awaiting_input" {
				if entry["inputSignal"] == "ctrl" {
					t.Errorf("the idle verdict came from a controller snapshot, not from the "+
						"terminal; the plugin is dead and cannot have pushed it: %v", entry)
				}
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("the pane never reached awaiting_input after its plugin died: it is frozen on the " +
		"last thing the plugin said, which is the failure this test exists for")
}

// ── spawning ────────────────────────────────────────────────────────────────

// TestSpawnedPluginLeavesNoOrphan pins the process-group half of shutdown.
//
// A plugin is `/bin/sh -c CMD` with Setpgid, so the shell AND anything it
// started are in one group. Signalling the leader alone leaves the grandchild
// behind — holding the socket and the plugin's own token — and nothing would
// ever collect it, because magmux is the only process that knew it existed.
func TestSpawnedPluginLeavesNoOrphan(t *testing.T) {
	dir := sockTestDir(t)
	// A shell that starts a long-lived grandchild and then waits. The marker in
	// the sleep's argument is what ps is searched for: a plain `sleep 900` would
	// match anything else on the machine.
	marker := fmt.Sprintf("magmux-orphan-probe-%d", time.Now().UnixNano())
	script := fmt.Sprintf(`sh -c 'sleep 900 #%s' & echo $! > %s/child.pid; sleep 900`, marker, dir)

	m := startHeadlessMagmux(t, "--headless", "-w", "-e", "echo done-and-dusted", "--plugin", script)
	// -w ends the run as soon as the -e pane finishes, which is what puts the
	// plugin through the real shutdown path rather than a signal.
	if code := m.wait(20 * time.Second); code != 0 {
		t.Fatalf("magmux exited %d\nstderr: %s", code, m.stderr.String())
	}

	// The whole group must be gone. Give the kernel a moment: SIGKILL is
	// asynchronous, and cmd.Wait only covers the leader.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("ps", "-Ao", "pid=,command=").Output()
		if !strings.Contains(string(out), marker) {
			return
		}
		if time.Now().After(deadline) {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, marker) {
					t.Errorf("orphan survived magmux: %s", strings.TrimSpace(line))
				}
			}
			t.Fatal("a plugin's grandchild outlived magmux; the shutdown signal did not reach " +
				"the process group")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSpawnedPluginLogsToItsOwnFile pins the rule that keeps a plugin's output
// out of the terminal: stdout and stderr go to a file named for the plugin, and
// magmux's own stdout stays empty.
//
// It matters because magmux is usually holding a raw-mode alternate screen. One
// stray line from a plugin corrupts the frame with no way to repaint it, and
// the plugin author would have no idea their console.log did it.
func TestSpawnedPluginLogsToItsOwnFile(t *testing.T) {
	m := startHeadlessMagmux(t, "--headless", "-w",
		"-e", "echo done-and-dusted",
		"--plugin", "echo PLUGIN_SPOKE; echo PLUGIN_COMPLAINED >&2")
	if code := m.wait(20 * time.Second); code != 0 {
		t.Fatalf("magmux exited %d\nstderr: %s", code, m.stderr.String())
	}
	if out := m.stdout.String(); out != "" {
		t.Errorf("a headless magmux wrote %d bytes to stdout: %q", len(out), out)
	}
	if strings.Contains(m.stderr.String(), "PLUGIN_") {
		t.Errorf("the plugin's output reached magmux's stderr: %s", m.stderr.String())
	}

	// The log is named for the plugin's assigned id until it registers; this
	// one never does, which is exactly the case where the file has to exist.
	logs, err := filepath.Glob(filepath.Join(m.dir, "*.plugin-*.log"))
	if err != nil || len(logs) == 0 {
		t.Fatalf("no plugin log in %s (%v)", m.dir, err)
	}
	raw, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatalf("reading %s: %v", logs[0], err)
	}
	body := string(raw)
	for _, want := range []string{"PLUGIN_SPOKE", "PLUGIN_COMPLAINED"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s is missing from %s:\n%s", want, logs[0], body)
		}
	}
}

// ── the view token and a plugin op ──────────────────────────────────────────

// TestViewOpGrantsOnePluginOpToViewers closes the gap P4 left open: --view-op
// had no end-to-end case, because no plugin op existed to grant.
//
// The rule it pins is that a plugin's SELF-DECLARED class never widens the view
// token. A plugin could otherwise mark its `deploy` op class read and hand
// itself to every viewer; the operator's grant list is the only way a plugin op
// reaches one, and it grants exactly the op it names.
func TestViewOpGrantsOnePluginOpToViewers(t *testing.T) {
	dir := sockTestDir(t)
	viewTok, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	viewPath := filepath.Join(dir, "view.token")
	if err := os.WriteFile(viewPath, []byte(viewTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := startRemoteMagmux(t, "--view-token-file", viewPath, "--view-op", "ticket.work", "-e", "sleep 60")

	// The plugin registers with the SESSION token, which is what a hand-run
	// plugin uses. From then on it answers everything with the same result.
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))
	p.serve(map[string]any{"did": "the work"})

	t.Run("the granted op is reachable by a viewer", func(t *testing.T) {
		// ticket.work is class CONTROL and is granted by name, so the grant has
		// to beat the class — twice over, since the hub enforces the class rule
		// again on its own side.
		resp, body := m.req(t, "POST", "/v1/ops/ticket.work", viewTok, `{}`)
		if resp.StatusCode != http.StatusOK || body["ok"] != true {
			t.Fatalf("status %d: %v", resp.StatusCode, body)
		}
		if result, _ := body["result"].(map[string]any); result["did"] != "the work" {
			t.Errorf("result = %v", body["result"])
		}
	})

	t.Run("an ungranted plugin op is not, whatever its class", func(t *testing.T) {
		// ticket.echo is class READ, and still refused: a plugin's own class
		// claim cannot widen the view token.
		resp, body := m.req(t, "POST", "/v1/ops/ticket.echo", viewTok, `{}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %v", resp.StatusCode, body)
		}
		if body["code"] != protocol.CodeForbidden {
			t.Errorf("code = %v", body["code"])
		}
	})

	t.Run("the full token reaches both", func(t *testing.T) {
		for _, op := range []string{"ticket.echo", "ticket.work"} {
			resp, body := m.req(t, "POST", "/v1/ops/"+op, m.token, `{}`)
			if resp.StatusCode != http.StatusOK || body["ok"] != true {
				t.Errorf("%s: status %d: %v", op, resp.StatusCode, body)
			}
		}
	})
}
