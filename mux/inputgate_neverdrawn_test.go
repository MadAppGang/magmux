package mux

// The second loss signal (V1 rerun): Claude Code sometimes drops typed text
// right after the gate opens WITHOUT any termios change — the box never draws
// any of it. Render lag tops out well under a second (300–800 ms), so a box
// that is COMPLETELY empty on every read for the whole verify window
// (verifyMax, 3 s) is a silent drop, not a slow draw, and licenses the one
// retype (`retypeCause:"never_drawn"`). Everything else about the retype is
// unchanged: no retype after partial text or excess, a second loss is
// text_mismatch, and the window must not have been cut short by the deadline.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// replyOf is the gated reply's result map for o.
func replyOf(o sendOutcome) map[string]any {
	res := map[string]any{}
	o.rep.addTo(res)
	return res
}

// TestNeverDrawnIsRetypedOnceThenOK: the box shows the placeholder on every
// read for the whole 3 s window, with no terminal reset. One retype, then the
// text is drawn: ok, retyped:true, retypeCause:"never_drawn", and the retype
// came no sooner than the window closed.
func TestNeverDrawnIsRetypedOnceThenOK(t *testing.T) {
	var clk lagClock
	var mu sync.Mutex
	var retypeAt time.Duration
	g := &fakeGate{}
	g.checkFn = func(BoxExpect, int) BoxCheck {
		el := clk.since()
		if strings.Count(g.log.String(), "hello") >= 2 {
			mu.Lock()
			if retypeAt == 0 {
				retypeAt = el
			}
			mu.Unlock()
			return verifiedBox(verifiedExact)
		}
		return boxEmptyCheck() // dropped silently: no termios change, never drawn
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hellohello\r" {
		t.Errorf("bytes %q, want the text twice (one retype) and one Enter", got)
	}
	res := replyOf(o)
	if res["retyped"] != true || res["retypeCause"] != retypeNeverDrawn {
		t.Errorf("reply %v: want retyped:true, retypeCause:%q", res, retypeNeverDrawn)
	}
	if ack := lastAck(m); !strings.Contains(ack, "never drawn") {
		t.Errorf("ack %q does not name the cause", ack)
	}
	mu.Lock()
	defer mu.Unlock()
	if retypeAt < verifyMax {
		t.Errorf("retyped %v after the first check, want no sooner than the %v window", retypeAt, verifyMax)
	}
}

// TestTerminalResetRetypeNamesItsCause: the reset signal still retypes at the
// settle interval (it does not wait out the whole window), and says so.
func TestTerminalResetRetypeNamesItsCause(t *testing.T) {
	var clk lagClock
	var mu sync.Mutex
	var retypeAt time.Duration
	g := &fakeGate{}
	g.checkFn = func(BoxExpect, int) BoxCheck {
		el := clk.since()
		if strings.Count(g.log.String(), "hello") >= 2 {
			mu.Lock()
			if retypeAt == 0 {
				retypeAt = el
			}
			mu.Unlock()
			return verifiedBox(verifiedExact)
		}
		return lostBoxCheck()
	}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	res := replyOf(o)
	if res["retyped"] != true || res["retypeCause"] != retypeTerminalReset {
		t.Errorf("reply %v: want retyped:true, retypeCause:%q", res, retypeTerminalReset)
	}
	mu.Lock()
	defer mu.Unlock()
	if retypeAt >= verifyMax {
		t.Errorf("retyped %v after the first check: the reset signal must not wait out the %v window", retypeAt, verifyMax)
	}
}

// TestDrawnLateAt2500msIsNotRetyped: the box is empty for 2.5 s — far past
// the settle interval, inside the window — and then the text is drawn. That
// is a (very) slow draw: ok, typed exactly once, no retype.
func TestDrawnLateAt2500msIsNotRetyped(t *testing.T) {
	var clk lagClock
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		if clk.since() < 2500*time.Millisecond {
			return boxEmptyCheck()
		}
		return verifiedBox(verifiedExact)
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hello\r" {
		t.Errorf("bytes %q, want the text once and one Enter", got)
	}
	noRetype(t, o)
}

// TestDrawnPartiallyIsNotRetyped: a box that showed ANY of the text is never
// a silent drop, whether the partial text stays or the box empties again.
func TestDrawnPartiallyIsNotRetyped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check func(el time.Duration) BoxCheck
	}{
		{"empty-then-partial", func(el time.Duration) BoxCheck {
			if el < 500*time.Millisecond {
				return boxEmptyCheck()
			}
			return boxMismatchCheck("hel", "the box holds 3 of 5 non-space runes", false)
		}},
		{"partial-then-empty", func(el time.Duration) BoxCheck {
			if el < 200*time.Millisecond {
				return boxMismatchCheck("hel", "the box holds 3 of 5 non-space runes", false)
			}
			return boxEmptyCheck()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clk lagClock
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return tc.check(clk.since()) }}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, sendRequest{text: "hello", enter: true})
			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			if got := log.settle(); got != "hello" {
				t.Errorf("bytes %q, want the text once and no Enter", got)
			}
			noRetype(t, o)
		})
	}
}

// TestDeadlineShorterThanWindowIsNotRetypedNeverDrawn: with a readyTimeoutMs
// that leaves less than the full verify window, a box that stays empty with
// no reset cannot be told from a slow draw: text_mismatch, typed once, and
// the error says the window was cut short.
func TestDeadlineShorterThanWindowIsNotRetypedNeverDrawn(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return boxEmptyCheck() }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 2000})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if !strings.Contains(o.err.Error(), "no stdin reset") || !strings.Contains(o.err.Error(), "deadline") {
		t.Errorf("error %q does not say why it was not retyped", o.err)
	}
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once and no Enter", got)
	}
	noRetype(t, o)
}

// TestNeverDrawnTwiceIsTextMismatch: the retyped text is never drawn either.
// A second loss is text_mismatch, typed exactly twice, no Enter, and the
// error says it was already retyped once.
func TestNeverDrawnTwiceIsTextMismatch(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return boxEmptyCheck() }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if !strings.Contains(o.err.Error(), "retyped once") {
		t.Errorf("error %q does not say it was already retyped", o.err)
	}
	if got := log.settle(); got != "hellohello" {
		t.Errorf("bytes %q, want the text twice and no Enter", got)
	}
}

// TestNeverDrawnInterruptedByANotReadyReadIsNotADrop: the box is empty on
// every ready read, but one read in the window was not ready (the box was not
// observed). "Every read" is then false; with no reset either, nothing proves
// the text was lost: no retype.
func TestNeverDrawnInterruptedByANotReadyReadIsNotADrop(t *testing.T) {
	var clk lagClock
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		if el := clk.since(); el >= 600*time.Millisecond && el < 700*time.Millisecond {
			return checkNotReady(notReady(protocol.ReasonNoInputBox))
		}
		return boxEmptyCheck()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once and no Enter", got)
	}
	noRetype(t, o)
}
