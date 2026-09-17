package mux

// The registry's socket face, and the bridge from broadcastEvent onto the bus.
//
// The in-process tests here run against PTY-less panes for the same reason the
// dynamic-pane tests do: everything under test is dispatch and identity, and a
// fake pane keeps a failure pointing at that rather than at a fixture's shell
// quoting. The two end-to-end cases at the bottom cover what only a real
// socket can show — that a reply reaches the connection that asked, after the
// work it describes actually happened.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

func opsOf(t *testing.T, m *Magmux) ([]protocol.OpSpec, int) {
	t.Helper()
	res, err := m.sockOps()
	if err != nil {
		t.Fatalf("ops: %v", err)
	}
	specs, ok := res["ops"].([]protocol.OpSpec)
	if !ok {
		t.Fatalf("ops reply carried %T, want []protocol.OpSpec", res["ops"])
	}
	rev, _ := res["rev"].(int)
	return specs, rev
}

// TestOpsAdvertisesEveryBuiltinVerb pins the op table against the verb table.
//
// The two are maintained by hand and describe the same thing to different
// audiences — `capabilities` to a client that feature-detects, `ops` to one
// that introspects — so the failure this catches is a verb that exists in one
// list and not the other, which reads to a caller as magmux denying it has a
// feature it has.
func TestOpsAdvertisesEveryBuiltinVerb(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	specs, rev := opsOf(t, m)
	if rev != 1 {
		t.Errorf("rev = %d after registering the built-ins, want 1", rev)
	}
	if len(specs) == 0 {
		t.Fatal("no ops registered")
	}

	byName := map[string]protocol.OpSpec{}
	for _, s := range specs {
		if prev, dup := byName[s.Name]; dup {
			t.Errorf("op %q registered twice (%+v)", s.Name, prev)
		}
		byName[s.Name] = s
		if s.Source != "magmux" {
			t.Errorf("op %q has source %q, want magmux", s.Name, s.Source)
		}
		if s.Description == "" {
			t.Errorf("op %q has no description; `ops` is how a client that has never heard of magmux decides what to call", s.Name)
		}
		if !protocol.ValidClass(s.Class) {
			t.Errorf("op %q has class %q, which the view token cannot be enforced on", s.Name, s.Class)
		}
		var schema map[string]any
		if err := json.Unmarshal(s.Schema, &schema); err != nil || schema["type"] != "object" {
			t.Errorf("op %q schema is %s; it must be an object schema, because an MCP client uses it as inputSchema verbatim", s.Name, s.Schema)
		}
	}

	// Classification: anything that can reach a shell is control, chrome is
	// display, and everything a viewer may use is read.
	for name, want := range map[string]protocol.OpClass{
		"capabilities": protocol.ClassRead,
		"list":         protocol.ClassRead,
		"ops":          protocol.ClassRead,
		"capture":      protocol.ClassRead,
		"transcript":   protocol.ClassRead,
		"open_pane":    protocol.ClassControl,
		"close_pane":   protocol.ClassControl,
		"focus":        protocol.ClassControl,
		"send":         protocol.ClassControl,
		// input is its own class, not control: `send` is a controller's
		// instruction and is recorded as one, input is what a keyboard would
		// have done. The view token is enforced on the class, so collapsing the
		// two would be a permissions decision disguised as a tidy-up.
		"input":   protocol.ClassInput,
		"status":  protocol.ClassDisplay,
		"tint":    protocol.ClassDisplay,
		"overlay": protocol.ClassDisplay,
	} {
		spec, ok := byName[name]
		if !ok {
			t.Errorf("built-in verb %q is not registered as an op, so no transport but this socket can reach it", name)
			continue
		}
		if spec.Class != want {
			t.Errorf("op %q is class %q, want %q", name, spec.Class, want)
		}
		delete(byName, name)
	}
	if len(byName) > 0 {
		t.Errorf("ops registered but not classified by this test: %v", byName)
	}

	// Every op must also be a verb `capabilities` advertises, and the two new
	// verbs must be there too.
	verbs := map[string]bool{}
	for _, v := range sockVerbs {
		verbs[v] = true
	}
	for _, s := range specs {
		if !verbs[s.Name] {
			t.Errorf("op %q is missing from sockVerbs, so a client that feature-detects cannot see it", s.Name)
		}
	}
	for _, v := range []string{"ops", "call", "watch", "unwatch", "resync"} {
		if !verbs[v] {
			t.Errorf("capabilities does not advertise the %q verb", v)
		}
	}
}

// TestCallUsesTheOneDispatchTable is the argument that `call` adds a door and
// not a second implementation: the same request through the op registry and
// through the verb it wraps must produce the same answer, field for field.
func TestCallUsesTheOneDispatchTable(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)

	direct, err := m.dispatchSocketVerbExt(sockMsg{Type: "list"}, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	viaCall, err := m.sockCall(sockMsg{Type: "call", Op: "list"})
	if err != nil {
		t.Fatalf("call list: %v", err)
	}
	if !reflect.DeepEqual(fmt.Sprint(direct["panes"]), fmt.Sprint(viaCall["panes"])) {
		t.Errorf("list through the verb and through call disagree:\n verb: %v\n call: %v", direct["panes"], viaCall["panes"])
	}

	// Args reach the verb: focus is refused for a hidden pane by pane, so a
	// call that named the wrong one would fail differently.
	res, err := m.sockCall(sockMsg{Type: "call", Op: "focus", Args: json.RawMessage(`{"pane":1}`)})
	if err != nil {
		t.Fatalf("call focus: %v", err)
	}
	if fmt.Sprint(res["pane"]) != "1" {
		t.Errorf("call focus returned %v, want pane 1", res)
	}
}

// TestCallRefusals: every refusal is one of protocol's codes, because a caller
// branches on the code and a transport maps it to its own status.
func TestCallRefusals(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	for _, tc := range []struct {
		name string
		msg  sockMsg
		want string
	}{
		{"no op named", sockMsg{Type: "call"}, sockCodeBadRequest},
		{"unknown op", sockMsg{Type: "call", Op: "frobnicate"}, sockCodeUnknownVerb},
		{"args are not an object", sockMsg{Type: "call", Op: "list", Args: json.RawMessage(`"nope"`)}, sockCodeBadRequest},
		{"the verb's own refusal passes through", sockMsg{Type: "call", Op: "capture", Args: json.RawMessage(`{"pane":99}`)}, sockCodeNoSuchPane},
		{"a legacy verb is not an op", sockMsg{Type: "call", Op: "pilot"}, sockCodeUnknownVerb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.sockCall(tc.msg)
			if got := verbErrCode(err); got != tc.want {
				t.Fatalf("code = %q (err %v), want %q", got, err, tc.want)
			}
		})
	}
}

// TestCallFromAReadOnlyCallerIsForbidden. The identity comes from the adapter
// and never from the payload — sockMsg.caller is unexported and untagged — so
// this is what a view-token connection will get in P4, decided on the op's
// class rather than on a list.
func TestCallFromAReadOnlyCallerIsForbidden(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	viewer := hub.Caller{Transport: "ws", Conn: "ws#1", ReadOnly: true}

	if _, err := m.sockCall(sockMsg{Type: "call", Op: "list", caller: viewer}); err != nil {
		t.Errorf("a viewer was refused a read op: %v", err)
	}
	for _, op := range []string{"send", "open_pane", "tint"} {
		_, err := m.sockCall(sockMsg{Type: "call", Op: op, caller: viewer})
		if got := verbErrCode(err); got != sockCodeForbidden {
			t.Errorf("viewer calling %q got %q, want forbidden", op, got)
		}
	}

	// A message that tries to declare its own identity cannot: the field has
	// no JSON tag, so the decoder never touches it.
	var msg sockMsg
	if err := json.Unmarshal([]byte(`{"type":"call","op":"send","caller":{"ReadOnly":false},"plugin":"ticket"}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.caller != (hub.Caller{}) {
		t.Fatalf("the wire set sockMsg.caller to %+v; identity must come from the adapter alone", msg.caller)
	}
}

// TestCallIsDrivingOnlyForNonReadOps keeps an observer out of the panel.
//
// `magmux mcp` reads panes to serve resources and lists ops on attach. If that
// counted as driving, every agent that merely loaded the MCP server inside a
// controlled pane would show up as a controller and zero the ledger of the
// pilot that was actually driving it.
func TestCallIsDrivingOnlyForNonReadOps(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	for op, want := range map[string]bool{
		"list":       false,
		"capture":    false,
		"ops":        false,
		"send":       true,
		"open_pane":  true,
		"close_pane": true,
		"tint":       true,
		"frobnicate": false, // unknown: it is about to be refused
	} {
		if got := m.callIsDriving(sockMsg{Type: "call", Op: op}); got != want {
			t.Errorf("call %q driving = %v, want %v", op, got, want)
		}
	}
	// The verb list itself is untouched: `ops` and `call` are not on it.
	for _, verb := range []string{"ops", "call"} {
		if isControllerVerb(verb) {
			t.Errorf("%q is on isControllerVerb's list; that list is today's and stays today's", verb)
		}
	}
}

// busSink is a Sink that records whole messages. Nothing about it blocks: the
// bus's behaviour under a stalled peer is hub's own test.
type busSink struct {
	mu   sync.Mutex
	msgs []string
}

func (b *busSink) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, string(p))
	return len(p), nil
}
func (b *busSink) SetWriteDeadline(time.Time) error { return nil }
func (b *busSink) Close(string)                     {}
func (b *busSink) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.msgs...)
}

// TestBroadcastEventAlsoReachesTheBus is the bridge, and the shape of it
// matters: the bus gets the SAME bytes the socket clients get, produced once.
// A second marshalling would be a second opportunity for the two to disagree
// about what an event was, which is precisely what N4 forbids.
func TestBroadcastEventAlsoReachesTheBus(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sink := &busSink{}
	sub := m.bus().Session(hub.Caller{Transport: "test"}, sink, nil)
	sub.Start(nil)
	defer sub.Close("test over")

	m.broadcastEvent(map[string]any{"type": "control", "dir": "out", "pane": 3})

	deadline := time.Now().Add(2 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = sink.seen(); len(got) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("the bus received %d messages, want 1: %q", len(got), got)
	}
	want := `{"dir":"out","pane":3,"type":"control"}` + "\n"
	if got[0] != want {
		t.Fatalf("the bus received %q, want the exact line the socket got, %q", got[0], want)
	}
}

// ── over a real socket ──────────────────────────────────────────────────────

// TestOpsAndCallOverTheSocket is the end-to-end half: the two new verbs on a
// real connection, answered to the connection that asked, and silent without
// an id — which is the contract every client written before replies existed
// depends on, and the one thing a new verb could quietly break.
func TestOpsAndCallOverTheSocket(t *testing.T) {
	mux := startRPCMagmux(t, "-e", `sh -c "sleep 30"`)
	c := mux.dial()

	c.send(map[string]any{"type": "ops", "id": "ops-1"})
	ev, _ := c.awaitReply("ops-1", nil)
	res := replyOK(t, ev)
	ops, _ := res["ops"].([]any)
	if len(ops) == 0 {
		t.Fatalf("ops reply carried no ops: %v", res)
	}
	if rev, _ := res["rev"].(float64); rev < 1 {
		t.Errorf("ops rev = %v, want at least 1", res["rev"])
	}
	names := map[string]string{}
	for _, o := range ops {
		spec, _ := o.(map[string]any)
		name, _ := spec["name"].(string)
		class, _ := spec["class"].(string)
		names[name] = class
		if _, ok := spec["schema"].(map[string]any); !ok {
			t.Errorf("op %q crossed the wire with schema %v, which is not an object", name, spec["schema"])
		}
	}
	if names["capture"] != "read" || names["send"] != "control" {
		t.Errorf("ops over the wire: capture=%q send=%q, want read and control", names["capture"], names["send"])
	}

	// A call whose op is a verb: the reply is the verb's own result.
	c.send(map[string]any{"type": "call", "op": "capabilities", "id": "call-1"})
	ev, _ = c.awaitReply("call-1", nil)
	if got := replyOK(t, ev)["protocol"]; got != float64(sockProtocol) {
		t.Errorf("call capabilities returned protocol %v, want %d", got, sockProtocol)
	}

	// `send` answers only once its bytes have reached the PTY, which is what
	// makes the op path worth having: the delivery is paced, and the reply is
	// the one place a caller can learn it finished.
	start := time.Now()
	c.send(map[string]any{"type": "call", "op": "send", "id": "call-2",
		"args": map[string]any{"pane": 0, "text": "echo MAGMUX_RC_OK"}})
	ev, _ = c.awaitReply("call-2", nil)
	res = replyOK(t, ev)
	if elapsed := time.Since(start); elapsed < pilotSendDelay {
		t.Errorf("call send replied after %v, faster than the delivery it describes (%v); it answered before the work", elapsed, pilotSendDelay)
	}
	if fmt.Sprint(res["pane"]) != "0" || fmt.Sprint(res["bytes"]) != "17" {
		t.Errorf("call send result = %v, want pane 0 and 17 bytes", res)
	}

	// An unknown op is refused by code, not by silence, because this caller
	// asked for an answer.
	c.send(map[string]any{"type": "call", "op": "frobnicate", "id": "call-3"})
	ev, _ = c.awaitReply("call-3", nil)
	if code, _ := ev["code"].(string); code != sockCodeUnknownVerb {
		t.Errorf("unknown op code = %q, want unknown_verb", code)
	}

	// Without an id, both new verbs are silent — like every other verb on the
	// legacy path. The probe that follows is how that is proved: the next
	// reply on this connection must be the probe's.
	c.send(map[string]any{"type": "ops"})
	c.send(map[string]any{"type": "call", "op": "list"})
	c.send(map[string]any{"type": "capabilities", "id": "probe"})
	ev, _ = c.awaitReply("probe", func(other map[string]any) {
		if other["type"] == "reply" {
			t.Errorf("a message with no id was answered: %v", other)
		}
	})
	replyOK(t, ev)
}

// TestCallSendRefusesTheControlPanel: the op path is the same path, so it
// refuses the same things with the same words. The panel is magmux's own
// display and has no session to type into.
func TestCallSendRefusesTheControlPanel(t *testing.T) {
	mux := startRPCMagmux(t, "-c", "-e", `sh -c "sleep 30"`)
	c := mux.dial()

	c.send(map[string]any{"type": "list", "id": "l"})
	ev, _ := c.awaitReply("l", nil)
	panes, _ := replyOK(t, ev)["panes"].([]any)
	panel := -1
	for _, p := range panes {
		pane, _ := p.(map[string]any)
		if state, _ := pane["state"].(string); state == "panel" {
			panel = int(pane["pane"].(float64))
		}
	}
	if panel < 0 {
		t.Fatalf("no control panel in %v", panes)
	}

	c.send(map[string]any{"type": "call", "op": "send", "id": "s",
		"args": map[string]any{"pane": panel, "text": "hello"}})
	ev, _ = c.awaitReply("s", nil)
	code, _ := ev["code"].(string)
	msg, _ := ev["error"].(string)
	if code != sockCodePaneIsControl {
		t.Fatalf("call send to the panel: code %q error %q, want pane_is_control", code, msg)
	}
	if !strings.Contains(msg, "control panel") {
		t.Errorf("refusal %q does not say what the pane is", msg)
	}
}
