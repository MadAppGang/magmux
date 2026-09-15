package plugin

// Unit tests for the host, against a bare hub and no terminal at all.
//
// That this is possible is the point of the package boundary: everything the
// multiplexer knows — panes, screens, the layout lock — is behind two
// callbacks, so the registry, the limits and the death paths can be tested
// without a PTY, a process or a socket. The end-to-end behaviour on a real
// socket is mux's TestPlugin* suite.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// fakeConn collects the lines the host writes to a plugin, and lets a test
// answer them.
type fakeConn struct {
	t    *testing.T
	host *Host
	conn *Conn

	mu    sync.Mutex
	lines []map[string]any
	wake  chan struct{}
}

func newFakeConn(t *testing.T, h *Host, id string) *fakeConn {
	t.Helper()
	f := &fakeConn{t: t, host: h, wake: make(chan struct{}, 64)}
	f.conn = h.Conn(id, f.write)
	f.conn.SetSend(f.write)
	return f
}

func (f *fakeConn) write(line []byte) {
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		f.t.Errorf("the host wrote a line that is not JSON: %q", line)
		return
	}
	f.mu.Lock()
	f.lines = append(f.lines, msg)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// handle feeds one message in, exactly as the socket adapter would.
func (f *fakeConn) handle(msg map[string]any) {
	f.t.Helper()
	line, err := json.Marshal(msg)
	if err != nil {
		f.t.Fatal(err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		f.t.Fatal(err)
	}
	if !f.conn.Handle(env, line) {
		f.t.Fatalf("the host did not claim %v, which is a plugin-protocol message", msg["type"])
	}
}

// await waits for a line matching want.
func (f *fakeConn) await(what string, want func(map[string]any) bool) map[string]any {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	seen := 0
	for {
		f.mu.Lock()
		for ; seen < len(f.lines); seen++ {
			if want(f.lines[seen]) {
				msg := f.lines[seen]
				seen++
				f.mu.Unlock()
				return msg
			}
		}
		f.mu.Unlock()
		select {
		case <-f.wake:
		case <-deadline:
			f.t.Fatalf("no %s within 5s; the host wrote %v", what, f.all())
		}
	}
}

func (f *fakeConn) all() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.lines...)
}

// bus is a hub with a recording subscriber, so a test can assert on what was
// published without a transport.
type bus struct {
	*hub.Hub
	mu     sync.Mutex
	events []map[string]any
	wake   chan struct{}
}

func newBus(t *testing.T) *bus {
	t.Helper()
	b := &bus{Hub: hub.New(), wake: make(chan struct{}, 64)}
	sub := b.Session(hub.Caller{Transport: "test", Conn: "test#1"}, b, nil)
	sub.Start(nil)
	t.Cleanup(func() { sub.Close("test over") })
	return b
}

// The recording subscriber is itself the Sink: one fewer moving part than a
// separate type, and Write is the only method with anything to do.
func (b *bus) Write(p []byte) (int, error) {
	var ev map[string]any
	if err := json.Unmarshal(p, &ev); err == nil {
		b.mu.Lock()
		b.events = append(b.events, ev)
		b.mu.Unlock()
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}
func (b *bus) SetWriteDeadline(time.Time) error { return nil }
func (b *bus) Close(string)                     {}

func (b *bus) await(t *testing.T, typ string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		b.mu.Lock()
		for _, ev := range b.events {
			if ev["type"] == typ {
				b.mu.Unlock()
				return ev
			}
		}
		b.mu.Unlock()
		select {
		case <-b.wake:
		case <-deadline:
			t.Fatalf("no %s event within 5s", typ)
		}
	}
}

const testToken = "session-token-for-tests"

func newTestHost(t *testing.T, b *bus) *Host {
	t.Helper()
	return New(Config{Hub: b.Hub, Token: testToken})
}

func registerMsg(name string, patch map[string]any) map[string]any {
	msg := map[string]any{
		"type": protocol.MsgPluginRegister, "token": testToken, "name": name, "id": 1,
		"ops": []any{
			map[string]any{"name": "echo", "class": "read"},
			map[string]any{"name": "work", "class": "control"},
		},
		"events": []any{"progress"},
	}
	for k, v := range patch {
		msg[k] = v
	}
	return msg
}

func replyTo(f *fakeConn, id any) map[string]any {
	return f.await("a reply", func(m map[string]any) bool {
		return m["type"] == protocol.EventReply && m["id"] == id
	})
}

// ── tests ───────────────────────────────────────────────────────────────────

// TestRegistrationIsAllOrNothing: a batch with one bad op registers NOTHING.
// A half-registered plugin would advertise an op list magmux disagrees with,
// and the only way back would be to unregister a source that never fully
// existed.
func TestRegistrationIsAllOrNothing(t *testing.T) {
	b := newBus(t)
	h := newTestHost(t, b)
	f := newFakeConn(t, h, "sock#1")

	f.handle(registerMsg("ticket", map[string]any{
		"ops": []any{
			map[string]any{"name": "good", "class": "read"},
			map[string]any{"name": "BAD", "class": "read"},
		},
	}))
	reply := replyTo(f, float64(1))
	if reply["ok"] == true {
		t.Fatalf("a registration with a malformed op name succeeded: %v", reply)
	}
	if specs, _ := h.cfg.Hub.Ops(); len(specs) != 0 {
		t.Errorf("the good op was registered anyway: %v", specs)
	}
	if names := h.Names(); len(names) != 0 {
		t.Errorf("the plugin is registered despite its ops being refused: %v", names)
	}
	if f.conn.Plugin() != "" {
		t.Error("the connection is a plugin despite the registration failing")
	}
}

// TestRegistrationPublishesOpsChanged: a client caches the op list, so the
// revision changing is news.
func TestRegistrationPublishesOpsChanged(t *testing.T) {
	b := newBus(t)
	h := newTestHost(t, b)
	f := newFakeConn(t, h, "sock#1")
	f.handle(registerMsg("ticket", nil))

	reply := replyTo(f, float64(1))
	if reply["ok"] != true {
		t.Fatalf("register failed: %v", reply)
	}
	if f.conn.Plugin() != "ticket" {
		t.Fatalf("the connection resolves to %q, want ticket", f.conn.Plugin())
	}
	ev := b.await(t, protocol.EventOpsChanged)
	if rev, _ := ev["rev"].(float64); rev <= 0 {
		t.Errorf("ops_changed carries no revision: %v", ev)
	}
	specs, _ := h.cfg.Hub.Ops()
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	if len(names) != 2 || names[0] != "ticket.echo" || names[1] != "ticket.work" {
		t.Errorf("registered ops = %v, want the qualified names", names)
	}
	for _, s := range specs {
		if s.Source != "ticket" {
			t.Errorf("op %s has source %q; the registry stamps it and a plugin cannot claim "+
				"to be magmux", s.Name, s.Source)
		}
		if len(s.Schema) == 0 {
			t.Errorf("op %s has no schema; an absent one must be filled in rather than "+
				"reaching the wire as null", s.Name)
		}
	}
}

// TestInvokeCarriesTheCallerAndIsAnswered walks one invocation both ways.
func TestInvokeCarriesTheCallerAndIsAnswered(t *testing.T) {
	b := newBus(t)
	h := newTestHost(t, b)
	f := newFakeConn(t, h, "sock#1")
	f.handle(registerMsg("ticket", nil))
	replyTo(f, float64(1))

	caller := hub.Caller{Transport: "ws", Conn: "ws#3", Client: "web/0.1"}
	done := make(chan map[string]any, 1)
	go func() {
		res, err := h.cfg.Hub.Call(context.Background(), caller, "ticket.echo",
			json.RawMessage(`{"say":"hi"}`))
		if err != nil {
			t.Errorf("call: %v", err)
		}
		done <- res
	}()

	inv := f.await("the invoke", func(m map[string]any) bool { return m["type"] == protocol.MsgInvoke })
	if inv["op"] != "echo" {
		t.Errorf("op = %v, want the bare name", inv["op"])
	}
	c, _ := inv["caller"].(map[string]any)
	if c["transport"] != "ws" || c["conn"] != "ws#3" || c["client"] != "web/0.1" {
		t.Errorf("caller = %v", inv["caller"])
	}
	if _, named := c["plugin"]; named {
		t.Error("the invoke names the CALLING plugin; a plugin is never told which other " +
			"plugin is calling it, because that would be an authorisation surface")
	}

	f.handle(map[string]any{
		"type": protocol.MsgInvokeResult, "call": inv["call"], "ok": true,
		"result": map[string]any{"said": "hi"},
	})
	if res := <-done; res["said"] != "hi" {
		t.Errorf("result = %v", res)
	}
}

// TestInFlightCapAnswersBusy: past MaxInFlight a caller is told the plugin is
// busy, which is a true statement about the plugin rather than a queue that
// hides how far behind it is.
func TestInFlightCapAnswersBusy(t *testing.T) {
	b := newBus(t)
	h := newTestHost(t, b)
	f := newFakeConn(t, h, "sock#1")
	f.handle(registerMsg("ticket", nil))
	replyTo(f, float64(1))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < MaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.cfg.Hub.Call(ctx, hub.Caller{}, "ticket.work", nil)
		}()
	}
	// Wait until every one of them is really in flight, so the next call is
	// refused for the reason the test is about rather than by a race.
	// Counted in the predicate rather than by re-reading the recorded lines:
	// await already holds the lock it would need, and each line is visited
	// exactly once.
	invokes := 0
	f.await("the last invoke", func(m map[string]any) bool {
		if m["type"] == protocol.MsgInvoke {
			invokes++
		}
		return invokes >= MaxInFlight
	})

	_, err := h.cfg.Hub.Call(context.Background(), hub.Caller{}, "ticket.work", nil)
	if protocol.CodeOf(err) != protocol.CodeBusy {
		t.Errorf("the %dth call failed with %v, want busy", MaxInFlight+1, err)
	}
	cancel()
	wg.Wait()
}

// TestEventRateCapDropsRatherThanQueues. A queue would turn a loop in a plugin
// into unbounded memory in magmux, and every subscriber would be reading
// minutes-old news by the time it drained.
func TestEventRateCapDropsRatherThanQueues(t *testing.T) {
	b := newBus(t)
	h := newTestHost(t, b)
	f := newFakeConn(t, h, "sock#1")
	f.handle(registerMsg("ticket", nil))
	replyTo(f, float64(1))

	refused := 0
	for i := 0; i < MaxEventsPerSecond+10; i++ {
		f.handle(map[string]any{
			"type": protocol.MsgPluginEvent, "event": "progress", "id": 100 + i,
			"data": map[string]any{"n": i},
		})
		reply := replyTo(f, float64(100+i))
		if reply["ok"] != true {
			if reply["code"] != protocol.CodeBusy {
				t.Fatalf("event %d was refused with %v, want busy: %v", i, reply["code"], reply)
			}
			refused++
		}
	}
	if refused != 10 {
		t.Errorf("%d events were dropped, want the 10 past the %d/s cap", refused, MaxEventsPerSecond)
	}
	// The connection stays up: exceeding a rate cap is a plugin being busy, not
	// a plugin being wrong.
	if f.conn.Plugin() != "ticket" {
		t.Error("the plugin was unregistered for exceeding the event rate")
	}
}

// TestDeathUnregistersBeforeItAnnounces pins the order a client depends on: the
// ops are gone BEFORE ops_changed says so, so a client that re-fetches on the
// news cannot be handed the dead plugin's ops again.
func TestDeathUnregistersBeforeItAnnounces(t *testing.T) {
	b := newBus(t)
	var gone []string
	h := New(Config{
		Hub:    b.Hub,
		Token:  testToken,
		OnExit: func(name string) { gone = append(gone, name) },
	})
	f := newFakeConn(t, h, "sock#1")
	f.handle(registerMsg("ticket", nil))
	replyTo(f, float64(1))

	// A call in flight when the connection goes.
	failed := make(chan error, 1)
	go func() {
		_, err := h.cfg.Hub.Call(context.Background(), hub.Caller{}, "ticket.work", nil)
		failed <- err
	}()
	f.await("the invoke", func(m map[string]any) bool { return m["type"] == protocol.MsgInvoke })

	f.conn.Close()

	select {
	case err := <-failed:
		if protocol.CodeOf(err) != protocol.CodePluginGone {
			t.Errorf("the in-flight call failed with %v, want plugin_gone", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight call is still waiting for a plugin that has gone; it would " +
			"burn its whole budget")
	}

	if specs, _ := h.cfg.Hub.Ops(); len(specs) != 0 {
		t.Errorf("a dead plugin's ops are still registered: %v", specs)
	}
	ev := b.await(t, protocol.EventPluginExited)
	if ev["plugin"] != "ticket" {
		t.Errorf("plugin_exited names %v", ev["plugin"])
	}
	if !strings.Contains(strings.Join(gone, ","), "ticket") {
		t.Errorf("OnExit was not called: %v", gone)
	}
	if f.conn.Plugin() != "" {
		t.Error("the closed connection still resolves to a plugin")
	}
	// And the name is free again: a plugin that reconnects is a plugin, not a
	// duplicate.
	g := newFakeConn(t, h, "sock#2")
	g.handle(registerMsg("ticket", nil))
	if reply := replyTo(g, float64(1)); reply["ok"] != true {
		t.Errorf("the name was not released by the death: %v", reply)
	}
}

// TestBudgetOfClampsAndDefaults covers the deadline arithmetic in one place,
// including the past-deadline case: it must still send the invoke, so the
// plugin's own log records that the call existed at all.
func TestBudgetOfClampsAndDefaults(t *testing.T) {
	if got := budgetOf(context.Background()); got != DefaultTimeout {
		t.Errorf("no deadline gave %v, want the default %v", got, DefaultTimeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if got := budgetOf(ctx); got != MaxTimeout {
		t.Errorf("an hour was not clamped to %v: %v", MaxTimeout, got)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), -time.Second)
	defer cancel2()
	if got := budgetOf(ctx2); got <= 0 {
		t.Errorf("an expired deadline gave %v; it must still be positive so the invoke goes "+
			"out and is cancelled rather than refused by arithmetic", got)
	}
}
