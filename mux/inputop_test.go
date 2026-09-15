package mux

// The `input` op: typing into a pane from somewhere that is not this keyboard.
//
// Every test here is about a difference from `send`, because that is the only
// reason the op exists. `send` is a controller's instruction — paced, recorded
// on the panel as an OUT row, and announced to the pane's tool as a new turn.
// `input` is a person at a remote keyboard: the bytes go, the answer says how
// many arrived, the panel learns nothing, and only a submit claims a turn.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// countingNotifier is a controller that does nothing but count the turns it was
// told about.
type countingNotifier struct {
	ToolController
	notified int
}

func (c *countingNotifier) NotifyInput() { c.notified++ }

func callInput(t *testing.T, m *Magmux, args string) (map[string]any, error) {
	t.Helper()
	return m.sockInput(hub.Caller{Transport: "socket", Conn: "test"}, json.RawMessage(args))
}

// TestInputWritesAndReportsTheByteCount. The count is the whole point of the
// reply: a remote keyboard that wrote four of nine bytes into a dying pane has
// to be able to know it.
func TestInputWritesAndReportsTheByteCount(t *testing.T) {
	m, p, child := inputMux(t)
	markIdle(p)

	res, err := callInput(t, m, fmt.Sprintf(`{"pane":%d,"text":"echo hi"}`, p.id))
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if got := drain(t, child); got != "echo hi" {
		t.Errorf("the child saw %q, want %q", got, "echo hi")
	}
	if fmt.Sprint(res["bytes"]) != "7" || fmt.Sprint(res["pane"]) != fmt.Sprint(p.id) {
		t.Errorf("input result = %v, want pane %d and 7 bytes", res, p.id)
	}

	// Same completion state a keystroke clears: a pane somebody is typing into
	// is not a pane that has finished.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inputReady || p.overlayText != "" || p.tint != "" || p.hadTextOutput {
		t.Errorf("input left the pane marked done: inputReady=%v overlay=%q tint=%q hadTextOutput=%v",
			p.inputReady, p.overlayText, p.tint, p.hadTextOutput)
	}
}

// TestInputRefusals: each refusal is its own code, because a client branches on
// it — `pane_dead` means try another pane, `bad_request` means fix the request.
func TestInputRefusals(t *testing.T) {
	m, p, _ := inputMux(t)

	// A control pane and a tombstone, both reachable by index.
	panel := newControlPane(0, 0, 10, 40, "panel")
	m.treeMu.Lock()
	panel.id = len(m.allPanes)
	m.allPanes = append(m.allPanes, panel)
	gone := newControlPane(0, 0, 10, 40, "gone")
	gone.id = len(m.allPanes)
	gone.closed = true
	m.allPanes = append(m.allPanes, gone)
	m.treeMu.Unlock()

	for _, tc := range []struct {
		name string
		args string
		want string
	}{
		{"no pane", `{"text":"x"}`, sockCodeBadRequest},
		{"a fan-out", `{"pane":"*","text":"x"}`, sockCodeBadRequest},
		{"not an index", `{"pane":"api","text":"x"}`, sockCodeBadRequest},
		{"no pane at that index", `{"pane":99,"text":"x"}`, sockCodeNoSuchPane},
		{"a closed pane", fmt.Sprintf(`{"pane":%d,"text":"x"}`, gone.id), sockCodeNoSuchPane},
		{"the control panel", fmt.Sprintf(`{"pane":%d,"text":"x"}`, panel.id), sockCodePaneIsControl},
		{"nothing to type", fmt.Sprintf(`{"pane":%d}`, p.id), sockCodeBadRequest},
		{"an unknown key", fmt.Sprintf(`{"pane":%d,"keys":["hyperspace"]}`, p.id), sockCodeBadRequest},
		{"args that are not an object", `"nope"`, sockCodeBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callInput(t, m, tc.args)
			if got := verbErrCode(err); got != tc.want {
				t.Fatalf("code = %q (err %v), want %q", got, err, tc.want)
			}
		})
	}
}

// TestInputRefusesADeadPane. The pane is alive to the layout and dead to its
// child, which is the state a remote keyboard hits most: the session exited and
// the window is still showing its last screen.
func TestInputRefusesADeadPane(t *testing.T) {
	m, p, _ := inputMux(t)
	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()

	_, err := callInput(t, m, fmt.Sprintf(`{"pane":%d,"text":"x"}`, p.id))
	if got := verbErrCode(err); got != sockCodePaneDead {
		t.Fatalf("code = %q (err %v), want pane_dead", got, err)
	}
	if !strings.Contains(err.Error(), "child has exited") {
		t.Errorf("refusal %q does not say why the pane cannot take input", err)
	}
}

// TestInputWritesNoPanelRow is the provenance rule.
//
// The control panel is the ledger of a CONTROLLER driving a session: `▶ OUT` is
// what the controller asked for, `◀ IN` is what magmux observed. A browser's
// arrow keys are neither, and filling the ledger with them would destroy the
// thing it exists to show. A keystroke is not an instruction.
func TestInputWritesNoPanelRow(t *testing.T) {
	m, p, _ := inputMux(t)
	before := m.control.digest()

	if _, err := callInput(t, m, fmt.Sprintf(`{"pane":%d,"text":"ls","keys":["enter"]}`, p.id)); err != nil {
		t.Fatalf("input: %v", err)
	}

	after := m.control.digest()
	if after.sent != before.sent {
		t.Errorf("input wrote an OUT row: sent went %d -> %d", before.sent, after.sent)
	}
	if after.observed != before.observed {
		t.Errorf("input wrote an IN row: observed went %d -> %d", before.observed, after.observed)
	}

	// …and `send` does, which is what makes the comparison mean something.
	if err := m.sendToPane(p.id, "ls", nil, true, "step 1", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	if m.control.digest().sent == before.sent {
		t.Error("send wrote no OUT row either; this test cannot tell the two paths apart")
	}
}

// TestInputNotifiesOnlyOnASubmit.
//
// A controller's idle state is one-way — only the tool's own transcript moves
// it back to working — so an instruction that arrives while the transcript is
// lagging has to say so. But a keystroke is not an instruction: claiming a turn
// for every arrow key would make the panel's `done` counter answer for
// something nobody asked for. A CR or LF outside a paste wrapper is the line.
func TestInputNotifiesOnlyOnASubmit(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want int
	}{
		{"plain text", `{"pane":%d,"text":"ls"}`, 0},
		{"text with a newline", `{"pane":%d,"text":"ls\n"}`, 1},
		{"text with a carriage return", `{"pane":%d,"text":"ls\r"}`, 1},
		{"an enter key", `{"pane":%d,"text":"ls","keys":["enter"]}`, 1},
		{"a navigation key", `{"pane":%d,"keys":["up"]}`, 0},
		{"a multi-line paste", `{"pane":%d,"text":"one\ntwo\n","paste":true}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, p, _ := inputMux(t)
			notifier := &countingNotifier{}
			p.controller = notifier

			if _, err := callInput(t, m, fmt.Sprintf(tc.args, p.id)); err != nil {
				t.Fatalf("input: %v", err)
			}
			if notifier.notified != tc.want {
				t.Errorf("NotifyInput called %d times, want %d", notifier.notified, tc.want)
			}
		})
	}
}

// TestInputPasteIsWrappedWhenThePaneAskedForIt. Without the wrapper the first
// newline of a multi-line block submits a half-written command — which is the
// whole reason a client asks for a paste rather than sending the text.
func TestInputPasteIsWrappedWhenThePaneAskedForIt(t *testing.T) {
	m, p, child := inputMux(t)
	p.mu.Lock()
	p.bracketPaste = true
	p.mu.Unlock()

	if _, err := callInput(t, m, fmt.Sprintf(`{"pane":%d,"text":"one\ntwo","paste":true}`, p.id)); err != nil {
		t.Fatalf("input: %v", err)
	}
	got := drain(t, child)
	if !strings.HasPrefix(got, "\x1b[200~") || !strings.HasSuffix(got, "\x1b[201~") {
		t.Errorf("the child saw %q, want it wrapped in bracketed-paste markers", got)
	}
	if !strings.Contains(got, "one\ntwo") {
		t.Errorf("the pasted text did not arrive intact: %q", got)
	}
}

// TestInputIsClassInputAndLaneBound.
//
// Two claims, both about routing rather than about bytes. The CLASS is what the
// read-only view token is enforced on, and `input` is not `control`: a
// controller uses `send`. The LANE is what stops two keystrokes from one
// connection to one pane being written out of order.
func TestInputIsClassInputAndLaneBound(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	spec, ok := m.bus().Spec("input")
	if !ok {
		t.Fatal("input is not registered as an op, so no transport can reach it")
	}
	if spec.Class != protocol.ClassInput {
		t.Errorf("input is class %q, want %q", spec.Class, protocol.ClassInput)
	}

	pane, lane, err := m.laneKeyFor("input", json.RawMessage(`{"pane":3,"text":"x"}`))
	if !lane || err != nil || pane != 3 {
		t.Errorf("laneKeyFor(input) = pane %d lane %v err %v, want pane 3 on a lane", pane, lane, err)
	}
	// An absent pane is the caller's mistake for `input`, and is refused at the
	// lane rather than queued onto a pane nobody named.
	if _, lane, err := m.laneKeyFor("input", json.RawMessage(`{"text":"x"}`)); !lane || verbErrCode(err) != sockCodeBadRequest {
		t.Errorf("input with no pane: lane %v err %v, want a bad_request", lane, err)
	}
	// And an ordinary op is not lane-bound at all: it runs concurrently.
	if _, lane, _ := m.laneKeyFor("list", nil); lane {
		t.Error("`list` was put on a pane's lane; only the ops that reach a PTY belong there")
	}
}

// TestInputRefusedByAReadOnlyCaller. Typing into a live shell is the one thing
// a viewer must never be able to do, and it is decided on the op's class rather
// than on a list somebody keeps by hand.
func TestInputRefusedByAReadOnlyCaller(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	viewer := hub.Caller{Transport: "ws", Conn: "ws#1", ReadOnly: true}
	_, err := m.bus().Call(t.Context(), viewer, "input", json.RawMessage(`{"pane":0,"text":"rm -rf /"}`))
	if got := verbErrCode(err); got != sockCodeForbidden {
		t.Fatalf("a viewer calling input got %q, want forbidden", got)
	}
}

// TestInputSurvivesAPaneDyingMidWrite: the pane is alive at the precheck and
// gone by the write, which is the race a remote keyboard hits when a session
// exits underneath it. The answer names the pane, not an internal error.
func TestInputSurvivesAPaneDyingMidWrite(t *testing.T) {
	m, p, child := inputMux(t)
	// Closing the read end makes the next write fail the way a departed child
	// does.
	child.Close()
	time.Sleep(10 * time.Millisecond)

	_, err := callInput(t, m, fmt.Sprintf(`{"pane":%d,"text":"hello"}`, p.id))
	if err == nil {
		t.Skip("the platform accepted a write to a pipe with no reader; the race cannot be forced here")
	}
	if got := verbErrCode(err); got != sockCodePaneDead {
		t.Fatalf("code = %q (err %v), want pane_dead", got, err)
	}
}
