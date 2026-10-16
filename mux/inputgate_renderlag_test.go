package mux

// The pre-Enter verify is a bounded SETTLE WAIT (V1, the real-Claude e2e):
// Claude Code 2.1.301 can take 300–800 ms to draw typed text into its box, so
// one read after the settle pause saw an empty box for text that was merely
// not drawn yet, the auto-retype typed it again, and both renders landed. These
// tests drive render-lag timelines through the delivery and assert, on the
// child's side of the PTY, how many times the text was typed.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// lagClock answers "how long since the first box check" for a scripted gate.
type lagClock struct {
	once  sync.Once
	first time.Time
}

func (c *lagClock) since() time.Duration {
	c.once.Do(func() { c.first = time.Now() })
	return time.Since(c.first)
}

func boxMismatchCheck(box, detail string, excess bool) BoxCheck {
	return BoxCheck{Readiness: readyNow(), Verdict: BoxMismatch, Box: box, Detail: detail, Excess: excess}
}

func noRetype(t *testing.T, o sendOutcome) {
	t.Helper()
	res := map[string]any{}
	o.rep.addTo(res)
	if _, ok := res["retyped"]; ok {
		t.Errorf("reply %v carries retyped", res)
	}
}

// TestRenderLagVerifiesWithoutRetype: the box shows the placeholder for
// 500 ms (and 800 ms) after the settle pause, then the text. That is a slow
// frame, not lost text: ok, verified exact, the text typed EXACTLY once.
func TestRenderLagVerifiesWithoutRetype(t *testing.T) {
	for _, lag := range []time.Duration{500 * time.Millisecond, 800 * time.Millisecond} {
		t.Run(lag.String(), func(t *testing.T) {
			var clk lagClock
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
				if clk.since() < lag {
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
			if o.rep.verified != verifiedExact {
				t.Errorf("verified %q, want exact", o.rep.verified)
			}
		})
	}
}

// TestRenderLagRealController: the same timeline through the REAL Claude
// controller's parse and verify: the box is drawn 500 ms late.
func TestRenderLagRealController(t *testing.T) {
	const prompt = "Summarise the build log in one line"
	idle := cat(claudeBanner, []string{"⏺ Ready."}) // a short conversation: the banner is on screen
	start := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
	empty := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
	inBox := claudeLines(synthRows, synthCols, idle, prompt, "")
	after := claudeLines(synthRows, synthCols, cat(idle, []string{""}, entryRows(prompt)), "", idlePlaceholder)
	m, s, log := claudeMux(t, start)
	var clk lagClock
	s.beforeCheck = func(int) {
		if clk.since() < 500*time.Millisecond {
			setScreen(start, empty)
		} else {
			setScreen(start, inBox)
		}
	}
	s.beforeAccept = func(int) { setScreen(start, after) }
	o := runSend(t, m, sendRequest{text: prompt, enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != prompt+"\r" {
		t.Errorf("bytes %q, want the text once and one Enter", got)
	}
	noRetype(t, o)
}

// TestTrulyLostTextIsRetypedOnceThenOK: the box stays empty for the whole
// settle interval (the startup reset ate the text); after the retype it is
// drawn. Exactly one retype, then ok.
func TestTrulyLostTextIsRetypedOnceThenOK(t *testing.T) {
	g := &fakeGate{}
	g.checkFn = func(BoxExpect, int) BoxCheck {
		if strings.Count(g.log.String(), "hello") < 2 {
			return lostBoxCheck() // lost: it never draws
		}
		return verifiedBox(verifiedExact)
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hellohello\r" {
		t.Errorf("bytes %q, want the text twice (one retype) and one Enter", got)
	}
	if !o.rep.retyped {
		t.Errorf("report %+v: want retyped", o.rep)
	}
}

// TestDoubleTextAfterRetypeIsMismatchNoSecondRetype: the first attempt's box
// stays empty long enough to retype, then BOTH renders land. The box holds
// more than was typed: text_mismatch, saying so, and never a second retype.
func TestDoubleTextAfterRetypeIsMismatchNoSecondRetype(t *testing.T) {
	g := &fakeGate{}
	g.checkFn = func(BoxExpect, int) BoxCheck {
		if strings.Count(g.log.String(), "hello") < 2 {
			return lostBoxCheck()
		}
		return boxMismatchCheck("hellohello", "the box holds more than was typed: 10 non-space runes where 5 were typed", true)
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if !strings.Contains(o.err.Error(), "more than was typed") {
		t.Errorf("error %q does not say the box holds more than was typed", o.err)
	}
	if got := log.settle(); got != "hellohello" {
		t.Errorf("bytes %q, want the text twice (exactly one retype) and no Enter", got)
	}
}

// TestDoubleTextFirstAttemptIsNeverRetyped: the box holds MORE than typed on
// the first attempt: no retype, no Enter.
func TestDoubleTextFirstAttemptIsNeverRetyped(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		return boxMismatchCheck("hellohello", "the box holds more than was typed: 10 non-space runes where 5 were typed", true)
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once", got)
	}
	noRetype(t, o)
}

// TestExcessSeenOnceForbidsRetype: one read shows MORE than typed, then the
// box goes (and stays) empty. An emptied box would be retyped, but a box that
// ever held more than typed never is.
func TestExcessSeenOnceForbidsRetype(t *testing.T) {
	g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck {
		if n == 0 {
			return boxMismatchCheck("hellohello", "the box holds more than was typed: 10 non-space runes where 5 were typed", true)
		}
		return lostBoxCheck()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if !strings.Contains(o.err.Error(), "more than was typed") {
		t.Errorf("error %q does not say the box holds more than was typed", o.err)
	}
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once", got)
	}
	noRetype(t, o)
}

// TestRenderLagBeyondWindowIsMismatchNoRetype: the box is empty for less than
// the settle interval, then keeps changing (a render that never settles)
// past the verify window. text_mismatch, no retype, no Enter, within the
// window.
func TestRenderLagBeyondWindowIsMismatchNoRetype(t *testing.T) {
	var clk lagClock
	g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck {
		if clk.since() < verifySettle/2 {
			return boxEmptyCheck()
		}
		return boxMismatchCheck(fmt.Sprintf("frame%d", n), "the box holds 1 of 5 non-space runes", false)
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if !strings.Contains(o.err.Error(), "still changing") {
		t.Errorf("error %q does not say the box was still changing", o.err)
	}
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once and no Enter", got)
	}
	noRetype(t, o)
	if o.elapsed > verifyMax+2*time.Second {
		t.Errorf("took %v, want the verify bounded by ~%v", o.elapsed, verifyMax)
	}
}

// TestEmptyShorterThanSettleIsNotRetyped: the box is empty when the window
// closes, but it was not empty for the whole settle interval (it changed
// shortly before). No retype.
func TestEmptyShorterThanSettleIsNotRetyped(t *testing.T) {
	var clk lagClock
	g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck {
		if clk.since() < verifyMax-verifySettle/2 {
			return boxMismatchCheck(fmt.Sprintf("frame%d", n), "partial", false)
		}
		return lostBoxCheck()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once and no Enter", got)
	}
	noRetype(t, o)
}

// TestNotReadyReadRestartsTheSettleClock: an empty box interrupted by a
// not-ready read is not "empty for the whole settle interval" until it has
// been empty, and ready, for a full interval after the interruption.
func TestNotReadyReadRestartsTheSettleClock(t *testing.T) {
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
		if el >= 600*time.Millisecond && el < 700*time.Millisecond {
			return checkNotReady(notReady(protocol.ReasonTerminalNotReady))
		}
		return lostBoxCheck()
	}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil || !o.rep.retyped {
		t.Fatalf("send: %+v %v, want ok after one retype", o.rep, o.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if retypeAt < 700*time.Millisecond+verifySettle {
		t.Errorf("retyped %v after the first check, want no sooner than %v (the clock restarts after the interruption)",
			retypeAt, 700*time.Millisecond+verifySettle)
	}
}

// TestVerifyWindowIsFlooredAtItsMinimum: a short readyTimeoutMs (400 ms)
// caps the settle wait, but never below the verify's own minimum window (r4
// M2) — so the verify DOES run past that deadline, to verifyMin: a box that
// never settles is reported after ~verifyMin, not after verifyMax, and not
// after one read either.
func TestVerifyWindowIsFlooredAtItsMinimum(t *testing.T) {
	g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck {
		return boxMismatchCheck(fmt.Sprintf("frame%d", n), "partial", false)
	}}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 400})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if o.elapsed < verifyMin {
		t.Errorf("took %v, want the verify to run its %v minimum window", o.elapsed, verifyMin)
	}
	if o.elapsed > verifyMin+700*time.Millisecond {
		t.Errorf("took %v, want the verify capped near the %v minimum window", o.elapsed, verifyMin)
	}
}

// TestJudgeBoxSaysTheBoxHoldsMore: the real controller's verify names a
// doubled box as holding MORE than was typed, and CheckBox flags it Excess.
func TestJudgeBoxSaysTheBoxHoldsMore(t *testing.T) {
	const prompt = "Summarise the build log"
	idle := cat(claudeBanner, []string{"⏺ Ready."}) // a short conversation: the banner is on screen
	p := claudeLines(synthRows, synthCols, idle, prompt+prompt, "")
	g, _ := gatedClaudeOn(p)
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: prompt, TypedText: true})
	if !chk.Readiness.Ready {
		t.Fatalf("not ready: %+v", chk.Readiness)
	}
	if chk.Verdict != BoxMismatch || !chk.Excess || !strings.Contains(chk.Detail, "more than was typed") {
		t.Errorf("CheckBox = %+v, want an excess mismatch saying more than was typed", chk)
	}
	short := claudeLines(synthRows, synthCols, idle, "Summ", "")
	g2, _ := gatedClaudeOn(short)
	chk = g2.CheckBox(BoxExpect{Class: ClassSubmit, Exact: prompt, TypedText: true})
	if chk.Verdict != BoxMismatch || chk.Excess || chk.Box == "" {
		t.Errorf("a short box: %+v, want a mismatch that is not excess, with its content", chk)
	}
}
