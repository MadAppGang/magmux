package mux

// r3 review H1/M2: what a box that emptied after typing is reported as — a
// termios change after the first byte, a box empty for the whole verify
// window, or neither — and that it is never retyped, nor reported as nothing
// submitted when somebody else's Enter may have submitted it.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// TestEmptyWithoutTermiosChangeIsNotRetyped: the box stays empty for the
// whole settle interval, but the terminal shows no stdin reset since the
// first byte, and the deadline leaves less than the full verify window, so
// "never drawn" cannot be said either (the text may still be on its way to
// the screen): text_mismatch, typed once. With the full window the same box
// is reported as never drawn (TestNeverDrawnIsTypedOnceAndNotRetyped).
func TestEmptyWithoutTermiosChangeIsNotRetyped(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return boxEmptyCheck() }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 1500})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if !strings.Contains(o.err.Error(), "no stdin reset") {
		t.Errorf("error %q does not say why it was not retyped", o.err)
	}
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once and no Enter", got)
	}
	noRetype(t, o)
}

// TestCheckBoxReportsTermiosChangeSinceTheFirstByte: the real controller
// flags a termios change after TypedAt, and only after it.
func TestCheckBoxReportsTermiosChangeSinceTheFirstByte(t *testing.T) {
	p := claudeLines(synthRows, synthCols, []string{"⏺ Ready."}, "", idlePlaceholder)
	c, mode := gatedClaudeOn(p)
	want := BoxExpect{Class: ClassSubmit, Exact: "hello", TypedText: true, TypedAt: time.Now()}
	if chk := c.CheckBox(want); chk.TermiosChanged {
		t.Fatalf("no change yet: %+v", chk)
	}
	cooked := modeCooked
	mode.Store(&cooked)
	if chk := c.CheckBox(want); !chk.TermiosChanged {
		t.Errorf("a cooked sample after the first byte: %+v, want TermiosChanged", chk)
	}
	later := want
	later.TypedAt = time.Now().Add(time.Second)
	if chk := c.CheckBox(later); chk.TermiosChanged {
		t.Errorf("a change before TypedAt counted: %+v", chk)
	}
}

// TestRealControllerNeverRetypesAnEmptiedBox: through the REAL controller,
// an empty box after a terminal reset (answered at the settle interval), one
// empty for the whole verify window, and one whose window the deadline cut
// short are all typed once, with no clear and no Enter, and each answer
// names what was seen.
func TestRealControllerNeverRetypesAnEmptiedBox(t *testing.T) {
	const prompt = "Summarise the build log in one line"
	idle := cat(claudeBanner, []string{"⏺ Ready."}) // a short conversation: the banner is on screen
	for _, tc := range []struct {
		reset   bool
		readyMs int
		want    []string
	}{
		{true, 0, []string{"stayed empty", "not retyped", "reset"}},
		{false, 0, []string{"stayed empty", "not retyped", "never drawn"}},
		{false, 1500, []string{"stayed empty", "not retyped", "no stdin reset", "deadline"}},
	} {
		reset := tc.reset
		t.Run(fmt.Sprintf("reset=%v,readyMs=%d", reset, tc.readyMs), func(t *testing.T) {
			p := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
			empty := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
			c, mode := gatedClaudeOn(p)
			s := &scriptedClaude{ClaudeCodeController: c, win: 300 * time.Millisecond}
			p.controller = s
			log := logChild(t, attachPipe(t, p))
			m := &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel()}
			m.root, m.focused = p, p
			m.allPanes = []*Pane{p}
			m.stampPaneIDs()
			s.beforeCheck = func(n int) {
				setScreen(p, empty)
				if reset && n == 0 {
					// The stdin reset: a cooked blip after the first byte.
					cooked := modeCooked
					mode.Store(&cooked)
					time.AfterFunc(100*time.Millisecond, func() { raw := modeRaw; mode.Store(&raw) })
				}
			}
			o := runSend(t, m, sendRequest{text: prompt, enter: true, readyTimeoutMs: tc.readyMs})
			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			if o.err != nil {
				for _, w := range tc.want {
					if !strings.Contains(o.err.Error(), w) {
						t.Errorf("error %q does not contain %q", o.err, w)
					}
				}
			}
			if got := log.settle(); got != prompt {
				t.Errorf("bytes %q, want the text once, no clear and no Enter", got)
			}
			noRetype(t, o)
		})
	}
}

// TestSubmittedByAnotherEnterIsNeverRetyped (fake gate): the box empties
// because somebody else's Enter submitted the text. Even with a termios
// change, the pre-typing baseline shows it was taken: ok from that evidence,
// no retype and no Enter of ours.
func TestSubmittedByAnotherEnterIsNeverRetyped(t *testing.T) {
	g := &fakeGate{
		checkFn: func(BoxExpect, int) BoxCheck { return lostBoxCheck() },
		acceptFn: func(b AcceptBaseline, _ int) AcceptVerdict {
			if b.SentKey == fakePreKey {
				return AcceptVerdict{State: AcceptTaken, Evidence: evidenceTranscript}
			}
			return AcceptVerdict{State: AcceptNone}
		},
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if o.rep.accept != acceptSubmittedByOther || o.rep.evidence != evidenceTranscript {
		t.Errorf("report %+v, want submitted_by_other by the transcript", o.rep)
	}
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once and no Enter of ours", got)
	}
	noRetype(t, o)
}

// TestSubmittedByAnotherEnterRealController: the same through the real
// controller — after the text went in, the screen shows a NEW `❯` entry for
// it and an empty box. The pre-typing baseline (taken from the read that
// authorised the first byte) attributes the entry to this text.
func TestSubmittedByAnotherEnterRealController(t *testing.T) {
	const prompt = "Summarise the build log in one line"
	idle := cat(claudeBanner, []string{"⏺ Ready."}) // a short conversation: the banner is on screen
	start := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
	submitted := claudeLines(synthRows, synthCols, cat(idle, []string{""}, entryRows(prompt)), "", idlePlaceholder)
	m, s, log := claudeMux(t, start)
	s.beforeCheck = func(int) { setScreen(start, submitted) }
	o := runSend(t, m, sendRequest{text: prompt, enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if o.rep.accept != acceptSubmittedByOther || o.rep.evidence != evidenceHistory {
		t.Errorf("report %+v, want submitted_by_other by history", o.rep)
	}
	if got := log.settle(); got != prompt {
		t.Errorf("bytes %q, want the text once and no Enter of ours", got)
	}
	noRetype(t, o)
}
