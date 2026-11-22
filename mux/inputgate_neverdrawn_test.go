package mux

// Claude Code sometimes leaves typed text undrawn right after the gate opens
// WITHOUT any termios change — the box never draws any of it. Render lag tops
// out well under a second (300–800 ms), so a box that is COMPLETELY empty on
// every read for the whole verify window (verifyMax, 3 s) is reported as
// never drawn. It is NOT retyped: an empty box does not prove Claude Code
// read the text, and a second copy could land behind the first, unread. The
// send answers text_mismatch with the text typed once and no Enter; a box
// that showed partial text, a deadline that cut the window short and a
// not-ready read in the window keep their own details.

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

// wantNotRetyped: the send answered not_accepted / text_mismatch, typed, for
// a box that stayed empty, and the error carries the words a client keys on
// ("stayed empty", "not retyped") plus words. The reply has no retype keys.
func wantNotRetyped(t *testing.T, o sendOutcome, words ...string) {
	t.Helper()
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if o.err == nil {
		return
	}
	for _, w := range append([]string{"stayed empty", "not retyped"}, words...) {
		if !strings.Contains(o.err.Error(), w) {
			t.Errorf("error %q does not contain %q", o.err, w)
		}
	}
	res := replyOf(o)
	for _, k := range []string{"retyped", "retypeCause"} {
		if _, ok := res[k]; ok {
			t.Errorf("reply %v carries %s", res, k)
		}
	}
}

// TestNeverDrawnIsTypedOnceAndNotRetyped: the box shows the placeholder on
// every read for the whole 3 s window, with no terminal reset. The text is
// typed once, with no clear and no Enter, and the send answers text_mismatch
// saying the box was never drawn — no sooner than the window closed.
func TestNeverDrawnIsTypedOnceAndNotRetyped(t *testing.T) {
	var clk lagClock
	var mu sync.Mutex
	var lastCheck time.Duration
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		el := clk.since()
		mu.Lock()
		lastCheck = el
		mu.Unlock()
		return boxEmptyCheck() // never drawn, no termios change
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantNotRetyped(t, o, "never drawn")
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once, no clear and no Enter", got)
	}
	if ack := lastAck(m); strings.Contains(ack, "· retyped") {
		t.Errorf("ack %q claims a retype", ack)
	}
	mu.Lock()
	defer mu.Unlock()
	if lastCheck < verifyMax {
		t.Errorf("concluded %v after the first check, want no sooner than the %v window", lastCheck, verifyMax)
	}
}

// TestTerminalResetIsTypedOnceAndNotRetyped: the box stays empty after a
// termios change. The reset does not prove the text was lost, so it is typed
// once, with no clear and no Enter, and the answer — given at the settle
// interval, not after the whole window — says the terminal reset.
func TestTerminalResetIsTypedOnceAndNotRetyped(t *testing.T) {
	var clk lagClock
	var mu sync.Mutex
	var lastCheck time.Duration
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		el := clk.since()
		mu.Lock()
		lastCheck = el
		mu.Unlock()
		return lostBoxCheck()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantNotRetyped(t, o, "reset")
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once, no clear and no Enter", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if lastCheck >= verifyMax {
		t.Errorf("concluded %v after the first check: a reset must not wait out the %v window", lastCheck, verifyMax)
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
