package mux

// The resume between writes, case by case: every way it stops for good, the
// typing token it keeps, the Enter a stopped send never presses, and the one
// NotifyInput a resumed send makes.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// blinkAt answers not ready (no_input_box) at readiness call n == at, and
// ready otherwise: one box-less frame between two writes.
// fromWrite3 reports whether a box check of a typed send of text is made
// before its third write or later (the 64-rune prefix onward), and is not
// the final verify of the whole text.
func fromWrite3(text string, want BoxExpect) bool {
	return want.Exact != text && len(want.Exact) >= 64
}

func blinkAt(at int) func(ReadinessQuery, int) InputReadiness {
	return func(_ ReadinessQuery, n int) InputReadiness {
		if n == at {
			return notReady(protocol.ReasonNoInputBox)
		}
		return readyNow()
	}
}

// wantTyping checks the typing keys of a failed send's wire reply.
func wantTyping(t *testing.T, m *Magmux, o sendOutcome, writes, total int, stopped string) {
	t.Helper()
	reply := wireReply(t, m, o)
	w, okW := wireInt(reply, "writes")
	wt, okT := wireInt(reply, "writesTotal")
	if !okW || !okT || w != writes || wt != total {
		t.Errorf("reply writes=%v writesTotal=%v, want %d of %d: %v", reply["writes"], reply["writesTotal"], writes, total, reply)
	}
	if reply["typingStopped"] != stopped {
		t.Errorf("reply typingStopped = %v, want %q: %v", reply["typingStopped"], stopped, reply)
	}
	if ty, ok := protocol.TypingOf(o.err); !ok || ty.Writes != writes || ty.WritesTotal != total || ty.Stopped != stopped {
		t.Errorf("TypingOf = %+v, %v; want %d/%d %q", ty, ok, writes, total, stopped)
	}
}

// noEnter fails when the child received a Return.
func noEnter(t *testing.T, got string) {
	t.Helper()
	if strings.Contains(got, "\r") {
		t.Errorf("an Enter was written after typing stopped: %q", got)
	}
}

func TestResumeStopsOnATerminalReset(t *testing.T) {
	text := strings.Repeat("x", 100)
	var log *byteLog
	g := &fakeGate{
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			c := childBox(&log, want)
			if fromWrite3(text, want) {
				c.TermiosChanged = true // the resume's read: the terminal reset since the first byte
			}
			return c
		},
	}
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	got := log.settle()
	noEnter(t, got)
	if got != text[:64] {
		t.Errorf("the child received %d bytes, want the first two writes (64)", len(got))
	}
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedTerminalReset)
	if !strings.Contains(o.err.Error(), "typing stopped after 2 of 4 writes (terminal_reset)") {
		t.Errorf("the error does not name the stop: %v", o.err)
	}
}

func TestResumeStopsOnASettledDifferentBox(t *testing.T) {
	text := strings.Repeat("x", 100)
	var log *byteLog
	g := &fakeGate{
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			if fromWrite3(text, want) {
				return boxMismatchCheck("yyyy", "the box holds 4 of 64 non-space runes", false)
			}
			return childBox(&log, want)
		},
	}
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	got := log.settle()
	noEnter(t, got)
	if got != text[:64] {
		t.Errorf("the child received %d bytes, want 64", len(got))
	}
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedBoxChanged)
	if ack := lastAck(m); !strings.Contains(ack, "typing stopped 2/4 (box_changed)") {
		t.Errorf("the panel ack does not show the stop: %q", ack)
	}
	if o.rep.waited < verifySettle {
		t.Errorf("waitedMs %v does not count the %v the resume waited", o.rep.waited, verifySettle)
	}
}

func TestResumeStopsAtTheDeadlineOnABoxStillChanging(t *testing.T) {
	// A ready box whose mismatching prefix never holds still for the settle
	// interval has not been shown to hold something else: at the deadline the
	// stop is "deadline", not "box_changed".
	text := strings.Repeat("x", 100)
	var log *byteLog
	g := &fakeGate{
		checkFn: func(want BoxExpect, n int) BoxCheck {
			if fromWrite3(text, want) {
				box := strings.Repeat("y", 1+n%7)
				return boxMismatchCheck(box, fmt.Sprintf("the box holds %d of 64 non-space runes", len(box)), false)
			}
			return childBox(&log, want)
		},
	}
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true, readyTimeoutMs: 600})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	got := log.settle()
	noEnter(t, got)
	if got != text[:64] {
		t.Errorf("the child received %d bytes, want 64", len(got))
	}
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedDeadline)
}

func TestResumeWaitsForAPrefixDrawnLate(t *testing.T) {
	text := strings.Repeat("x", 100)
	var log *byteLog
	var clk lagClock
	g := &fakeGate{
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			if want.Exact != text && clk.since() < 500*time.Millisecond {
				return boxMismatchCheck(want.Exact[:32], "the box holds 32 of 64 non-space runes", false)
			}
			return childBox(&log, want)
		},
	}
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	if o.err != nil {
		t.Fatalf("send: %v; a prefix slow to draw must be waited for", o.err)
	}
	if got := log.settle(); got != text+"\r" {
		t.Errorf("the child received %q, want the text once and one Enter", got)
	}
	if g.notified.Load() != 1 {
		t.Errorf("NotifyInput called %d times across a resume, want 1", g.notified.Load())
	}
}

func TestResumeAsksForExactlyTheWrittenPrefix(t *testing.T) {
	text := strings.Repeat("x", 100)
	var log *byteLog
	g := &fakeGate{checkFn: func(want BoxExpect, _ int) BoxCheck { return childBox(&log, want) }}
	m, _, l := gatedMux(t, g)
	log = l
	if o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true}); o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Before each write after the first, the box is asked for exactly what
	// was written before it, in order; the final verify asks for the whole.
	var asked []string
	for _, e := range g.expects {
		if e.TypedAt.IsZero() || !e.TypedText {
			t.Fatalf("expect %+v lacks the first byte's time or TypedText", e)
		}
		if n := len(asked); n == 0 || asked[n-1] != e.Exact {
			asked = append(asked, e.Exact)
		}
	}
	want := []string{text[:32], text[:64], text[:96], text}
	if strings.Join(asked, "|") != strings.Join(want, "|") {
		t.Fatalf("the box was asked for %d prefixes %q, want the 32/64/96-rune prefixes then the whole text", len(asked), asked)
	}
}

func TestKeysLoopStopsWhenTheBoxIsUnknowable(t *testing.T) {
	// 0 gate, 1 post-token, 2 the first key's re-ask, 3 before "left", 4 before "b".
	g := &fakeGate{readyFn: blinkAt(4)}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{keys: []string{"a", "left", "b", "c"}, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	got := log.settle()
	noEnter(t, got)
	left, _ := keyBytes("left")
	if got != "a"+string(left) {
		t.Errorf("the child received %q, want the first two keys only", got)
	}
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedNotReady)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.expects {
		if e.Exact != "" {
			t.Errorf("an unknowable box was checked for %q: nothing is knowable to wait for", e.Exact)
		}
	}
}

func TestKeysLoopResumesWhilePrintable(t *testing.T) {
	var log *byteLog
	// 0 gate, 1 post-token, 2 the first key's re-ask, 3 before "b".
	g := &fakeGate{readyFn: blinkAt(3), checkFn: func(want BoxExpect, _ int) BoxCheck { return childBox(&log, want) }}
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{keys: []string{"a", "b", "c"}, enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "abc\r" {
		t.Errorf("the child received %q, want abc and one Enter", got)
	}
	res := map[string]any{}
	o.rep.addTo(res)
	if res["writes"] != 3 || res["writesTotal"] != 3 {
		t.Errorf("result %v, want writes 3 of 3", res)
	}
}

func TestATokenHeldThroughTheResume(t *testing.T) {
	a := strings.Repeat("A", 100)
	var (
		log      *byteLog
		resuming = make(chan struct{})
		once     sync.Once
		clk      lagClock
	)
	g := &fakeGate{
		// One blink before A's third write; the second send's own
		// readiness calls all come later and are ready.
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			if want.Exact != a { // the resume's prefix check
				once.Do(func() { close(resuming) })
				if clk.since() < 600*time.Millisecond {
					return checkNotReady(notReady(protocol.ReasonNoInputBox))
				}
			}
			return childBox(&log, want)
		},
	}
	m, _, l := gatedMux(t, g)
	log = l
	ca := startSend(t, m, context.Background(), sendRequest{mode: sendTyped, text: a, enter: true})
	select {
	case <-resuming:
	case <-time.After(5 * time.Second):
		t.Fatal("the first send never began to resume")
	}
	ob := runSend(t, m, sendRequest{text: "B", enter: true, readyTimeoutMs: 200})
	wantErr(t, ob, protocol.CodeBusy, protocol.ReasonTypingInProgress, false)
	oa := awaitSend(t, ca, 10*time.Second)
	if oa.err != nil {
		t.Fatalf("the resumed send: %v", oa.err)
	}
	if got := log.settle(); got != a+"\r" {
		t.Errorf("the child received %q; a second delivery must not type into a resume", got)
	}
}

func TestAStoppedSendWithABlankTailPressesNoEnter(t *testing.T) {
	text := strings.Repeat("x", 64) + "\n" // writes: 32, 32, newline
	var log *byteLog
	g := &fakeGate{
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			c := BoxCheck{Readiness: readyNow(), Verdict: BoxMismatch, Detail: "short"}
			if inputKey(log.String()) == inputKey(want.Exact) {
				c = verifiedBox(verifiedExact)
			}
			if fromWrite3(text, want) {
				c.TermiosChanged = true
			}
			return c
		},
	}
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	got := log.settle()
	noEnter(t, got)
	if got != text[:64] {
		t.Errorf("the child received %q, want the two chunks only", got)
	}
	wantTyping(t, m, o, 2, 3, protocol.TypingStoppedTerminalReset)
	if empty, stated := protocol.BoxEmptyOf(o.err); !stated || empty {
		t.Errorf("BoxEmptyOf = %v, %v; want false, stated", empty, stated)
	}
}

func TestADialogDuringTheResumeStopsTyping(t *testing.T) {
	text := strings.Repeat("x", 100)
	var log *byteLog
	g := &fakeGate{
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			if fromWrite3(text, want) {
				return checkNotReady(dialogUp("permission", false))
			}
			return childBox(&log, want)
		},
	}
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	wantErr(t, o, protocol.CodeBusy, "dialog:permission", true)
	got := log.settle()
	noEnter(t, got)
	if got != text[:64] {
		t.Errorf("the child received %d bytes, want 64", len(got))
	}
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedDialog)
}

func TestADialogBetweenWritesStatesTheStop(t *testing.T) {
	text := strings.Repeat("x", 100)
	g := &fakeGate{checkFn: func(want BoxExpect, _ int) BoxCheck {
		if fromWrite3(text, want) {
			return checkNotReady(dialogUp("question", true))
		}
		return verifiedBox(verifiedExact)
	}}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	wantErr(t, o, protocol.CodeBusy, "dialog:question", true)
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedDialog)
	if e := g.lastExpect(); e.Exact != text[:64] {
		t.Errorf("a dialog between writes was followed by a box check for %d runes, want none after it", len(e.Exact))
	}
}

func TestAPaneThatDiesDuringTheResume(t *testing.T) {
	text := strings.Repeat("x", 100)
	var p *Pane
	g := &fakeGate{
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			if !fromWrite3(text, want) {
				return verifiedBox(verifiedExact)
			}
			p.mu.Lock()
			p.dead = true
			p.mu.Unlock()
			return checkNotReady(notReady(protocol.ReasonNoInputBox))
		},
	}
	m, pane, log := gatedMux(t, g)
	p = pane
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	if got := verbErrCode(o.err); got != protocol.CodePaneDead {
		t.Fatalf("code = %q (%v), want pane_dead", got, o.err)
	}
	noEnter(t, log.settle())
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedPaneDead)
}

func TestAResumeCancelledByQuiesce(t *testing.T) {
	text := strings.Repeat("x", 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &fakeGate{
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			if !fromWrite3(text, want) {
				return verifiedBox(verifiedExact)
			}
			cancel()
			return checkNotReady(notReady(protocol.ReasonNoInputBox))
		},
	}
	m, _, log := gatedMux(t, g)
	o := awaitSend(t, startSend(t, m, ctx, sendRequest{mode: sendTyped, text: text, enter: true}), 10*time.Second)
	if o.err == nil || !protocol.TypedOf(o.err) {
		t.Fatalf("send: %v, want a typed cancellation", o.err)
	}
	noEnter(t, log.settle())
	wantTyping(t, m, o, 2, 4, protocol.TypingStoppedCancelled)
}
