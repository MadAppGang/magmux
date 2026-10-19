package mux

// Code review r4, MEDIUMs 1–3:
//
//  1. A send that may collapse into a `[Pasted text #N]` token is never
//     auto-retyped (two tokens would satisfy the paste-token rule and Enter
//     would submit both), and a box holding more than one NEW paste token is a
//     mismatch on every verify.
//  2. The settle wait has its own minimum window (verifyMin), which the
//     readiness deadline does not cut short.
//  3. Somebody else's Enter: the reply says so (accept:"submitted_by_other",
//     enter:false), and a box that held the text and then emptied with no
//     evidence is never reported as "nothing was submitted".

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// ── 1. pastes ───────────────────────────────────────────────────────────────

// TestPasteIsNeverRetyped: a paste-mode send, a bracketed auto send and an
// auto send above the collapse threshold each find the box empty after a
// reset (or never drawn). None of them is retyped: text_mismatch, written
// once, no Enter.
func TestPasteIsNeverRetyped(t *testing.T) {
	long := strings.Repeat("x", pasteCollapseRunes+50)
	for _, tc := range []struct {
		name      string
		req       sendRequest
		bracketed bool // the child enabled bracketed paste
		check     func() BoxCheck
	}{
		{"paste-reset", sendRequest{text: "hello", mode: sendPaste, enter: true}, false, lostBoxCheck},
		{"paste-never-drawn", sendRequest{text: "hello", mode: sendPaste, enter: true}, false, boxEmptyCheck},
		{"auto-bracketed", sendRequest{text: "a\nb", enter: true}, true, lostBoxCheck},
		{"auto-long", sendRequest{text: long, enter: true}, false, lostBoxCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return tc.check() }}
			m, p, log := gatedMux(t, g)
			p.mu.Lock()
			p.bracketPaste = tc.bracketed
			p.mu.Unlock()
			o := runSend(t, m, tc.req)
			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			got := log.settle()
			if strings.Contains(got, "\r") {
				t.Errorf("an Enter was pressed: %q", got)
			}
			if n := strings.Count(got, tc.req.text); n != 1 {
				t.Errorf("the text was written %d times, want exactly once", n)
			}
			noRetype(t, o)
		})
	}
}

// TestTwoNewPasteTokensAreAMismatch: the real controller's verify refuses a
// box holding two paste tokens newer than the baseline, and flags it Excess;
// one new token still verifies.
func TestTwoNewPasteTokensAreAMismatch(t *testing.T) {
	idle := cat(claudeBanner, []string{"⏺ Ready."})
	want := BoxExpect{Class: ClassSubmit, Exact: "a\nb\nc\nd", TypedText: true, Pasted: true}
	two := claudeLines(synthRows, synthCols, idle, "[Pasted text #1 +3 lines][Pasted text #2 +3 lines]", "")
	g, _ := gatedClaudeOn(two)
	chk := g.CheckBox(want)
	if !chk.Readiness.Ready {
		t.Fatalf("not ready: %+v", chk.Readiness)
	}
	if chk.Verdict != BoxMismatch || !chk.Excess || !strings.Contains(chk.Detail, "paste tokens") {
		t.Errorf("two tokens: %+v, want an excess mismatch naming the paste tokens", chk)
	}
	one := claudeLines(synthRows, synthCols, idle, "[Pasted text #1 +3 lines]", "")
	g1, _ := gatedClaudeOn(one)
	if chk := g1.CheckBox(want); chk.Verdict != BoxVerified || chk.Verified != verifiedPasteToken {
		t.Errorf("one token: %+v, want verified paste_token", chk)
	}
	// An OLD token (at or below PasteAbove) does not count against the new one.
	old := want
	old.PasteAbove = 1
	if chk := g.CheckBox(old); chk.Verdict != BoxVerified {
		t.Errorf("one old + one new token: %+v, want verified", chk)
	}
}

// ── 2. the minimum verify window ────────────────────────────────────────────

// TestSmallReadyTimeoutStillWaitsForTheDraw: readyTimeoutMs 400 and a draw
// 800 ms late. The verify must not collapse into one read: ok, typed once.
func TestSmallReadyTimeoutStillWaitsForTheDraw(t *testing.T) {
	var clk lagClock
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		if clk.since() < 800*time.Millisecond {
			return boxEmptyCheck()
		}
		return verifiedBox(verifiedExact)
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 400})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hello\r" {
		t.Errorf("bytes %q, want the text once and one Enter", got)
	}
	noRetype(t, o)
}

// TestReadyAtTheDeadlineStillWaitsForTheDraw: the gate opens 100 ms before
// the deadline; the box is drawn 700 ms after typing. ok, typed once.
func TestReadyAtTheDeadlineStillWaitsForTheDraw(t *testing.T) {
	start := time.Now()
	var clk lagClock
	g := &fakeGate{
		readyFn: func(ReadinessQuery, int) InputReadiness {
			if time.Since(start) < 900*time.Millisecond {
				return notReady(protocol.ReasonStarting)
			}
			return readyNow()
		},
		checkFn: func(BoxExpect, int) BoxCheck {
			if clk.since() < 700*time.Millisecond {
				return boxEmptyCheck()
			}
			return verifiedBox(verifiedExact)
		},
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 1000})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hello\r" {
		t.Errorf("bytes %q, want the text once and one Enter", got)
	}
}

// ── 3. somebody else's Enter ────────────────────────────────────────────────

// TestForeignSubmitIsReportedAsSuch: the box empties because another writer's
// Enter submitted the text. For a SUBMIT and for a TYPE alike the reply is ok
// with accept:"submitted_by_other" and enter:false — magmux pressed nothing.
func TestForeignSubmitIsReportedAsSuch(t *testing.T) {
	for _, enter := range []bool{true, false} {
		t.Run(map[bool]string{true: "submit", false: "type"}[enter], func(t *testing.T) {
			g := &fakeGate{
				checkFn: func(BoxExpect, int) BoxCheck { return lostBoxCheck() },
				acceptFn: func(b AcceptBaseline, _ int) AcceptVerdict {
					if b.SentKey == fakePreKey {
						return AcceptVerdict{State: AcceptTaken, Evidence: evidenceHistory}
					}
					return AcceptVerdict{State: AcceptNone}
				},
			}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, sendRequest{text: "hello", enter: enter})
			if o.err != nil {
				t.Fatalf("send: %v", o.err)
			}
			res := replyOf(o)
			if res["accept"] != acceptSubmittedByOther || res["enter"] != false || res["evidence"] != evidenceHistory {
				t.Errorf("reply %v: want accept:%q, enter:false, evidence:history", res, acceptSubmittedByOther)
			}
			if got := log.settle(); got != "hello" {
				t.Errorf("bytes %q, want the text once and no Enter of ours", got)
			}
			if ack := lastAck(m); !strings.Contains(ack, "submitted by another writer") || strings.Contains(ack, "+ enter") {
				t.Errorf("ack %q: want it to say another writer submitted, and no enter of ours", ack)
			}
			noRetype(t, o)
		})
	}
}

// TestTextThatLeftTheBoxIsNotNothingSubmitted: the box showed the text, then
// emptied (with a termios change), and no evidence of a submit appeared. It
// may have been submitted by another Enter, so it is never retyped and the
// error never says "nothing was submitted".
func TestTextThatLeftTheBoxIsNotNothingSubmitted(t *testing.T) {
	var clk lagClock
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		if clk.since() < 200*time.Millisecond {
			return boxMismatchCheck("hel", "the box holds 3 of 5 non-space runes", false)
		}
		return lostBoxCheck()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	msg := o.err.Error()
	if strings.Contains(msg, "nothing was submitted") || !strings.Contains(msg, "unknown whether") {
		t.Errorf("error %q: must say it is unknown whether another Enter submitted it", msg)
	}
	if !protocol.MaybeSubmittedOf(o.err) {
		t.Error("the reply does not carry maybeSubmitted: a caller can only find this case by matching the wording")
	}
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once and no Enter", got)
	}
	noRetype(t, o)
}
