package mux

// r3 review H1/M2: the auto-retype needs POSITIVE evidence that the text was
// lost (a termios change after the first byte, and a box empty for the whole
// settle interval), and it never runs when the text may already have been
// submitted by somebody else's Enter.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// TestEmptyWithoutTermiosChangeIsNotRetyped: the box stays empty for the
// whole settle interval, but the terminal shows no stdin reset since the
// first byte. That is not evidence the text was lost (it may still be on its
// way to the screen): text_mismatch, typed once, no retype.
func TestEmptyWithoutTermiosChangeIsNotRetyped(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return boxEmptyCheck() }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
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

// TestRealControllerRetypesOnlyAfterAReset: through the REAL controller, an
// empty box is retyped when (and only when) the terminal reset after the
// first byte.
func TestRealControllerRetypesOnlyAfterAReset(t *testing.T) {
	const prompt = "Summarise the build log in one line"
	idle := cat(claudeBanner, []string{"⏺ Ready."}) // a short conversation: the banner is on screen
	for _, reset := range []bool{true, false} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			p := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
			empty := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
			inBox := claudeLines(synthRows, synthCols, idle, prompt, "")
			after := claudeLines(synthRows, synthCols, cat(idle, []string{""}, entryRows(prompt)), "", idlePlaceholder)
			c, mode := gatedClaudeOn(p)
			s := &scriptedClaude{ClaudeCodeController: c, win: 300 * time.Millisecond}
			p.controller = s
			log := logChild(t, attachPipe(t, p))
			m := &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel()}
			m.root, m.focused = p, p
			m.allPanes = []*Pane{p}
			m.stampPaneIDs()
			s.beforeCheck = func(n int) {
				if strings.Count(log.String(), prompt) >= 2 {
					setScreen(p, inBox)
					return
				}
				setScreen(p, empty)
				if reset && n == 0 {
					// The stdin reset swallows the text: a cooked blip.
					cooked := modeCooked
					mode.Store(&cooked)
					time.AfterFunc(100*time.Millisecond, func() { raw := modeRaw; mode.Store(&raw) })
				}
			}
			s.beforeAccept = func(int) {
				// The submit is drawn only once OUR Enter went in; the
				// pre-retype check must see the screen as it is.
				if strings.Contains(log.String(), "\r") {
					setScreen(p, after)
				}
			}
			o := runSend(t, m, sendRequest{text: prompt, enter: true})
			if reset {
				if o.err != nil || !o.rep.retyped {
					t.Fatalf("send: %+v %v, want ok after one retype", o.rep, o.err)
				}
				if got := log.settle(); got != prompt+prompt+"\r" {
					t.Errorf("bytes %q, want the text twice and one Enter", got)
				}
				return
			}
			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			if got := log.settle(); got != prompt {
				t.Errorf("bytes %q, want the text once and no Enter", got)
			}
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
	if o.rep.accept != acceptConfirmed || o.rep.evidence != evidenceTranscript {
		t.Errorf("report %+v, want confirmed by the transcript", o.rep)
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
	if o.rep.accept != acceptConfirmed || o.rep.evidence != evidenceHistory {
		t.Errorf("report %+v, want confirmed by history", o.rep)
	}
	if got := log.settle(); got != prompt {
		t.Errorf("bytes %q, want the text once and no Enter of ours", got)
	}
	noRetype(t, o)
}
