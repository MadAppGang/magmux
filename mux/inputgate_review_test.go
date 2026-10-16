package mux

// The gated send's review-round fixes: control runes in text (H1), the
// side-effect-free budget (M2) and dismissal under the typing token (M5b).
// The screen-side fixes (bash mode, the select class) are beside the screen
// tests in claude_screen_test.go and controller_claude_input_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// TestGatedCarriageReturnTakesTheNewlinePath (review H1): a CR inside a gated
// send's text is a newline, never a keystroke Return. Unnormalised, auto mode
// wrote "fix A\rthen B" in one injectPTY, the embedded Return submitted "fix A"
// with no box check, and the reply then said nothing had been submitted. So
// the ONLY \r a gated send writes is its final, verified submit, and the text
// a CR-bearing auto send carries is multi-line text (query().Paste, rule i).
func TestGatedCarriageReturnTakesTheNewlinePath(t *testing.T) {
	for _, c := range []struct {
		name      string
		r         sendRequest
		wantBytes string
		wantExact string
		wantPaste bool
	}{
		{"auto, bare CR, submit", sendRequest{text: "fix A\rthen B", enter: true}, "fix A\nthen B\r", "fix A\nthen B", true},
		{"auto, CRLF, submit", sendRequest{text: "fix A\r\nthen B", enter: true}, "fix A\nthen B\r", "fix A\nthen B", true},
		{"auto, trailing CR, type only", sendRequest{text: "hello\r", enter: false}, "hello\n", "hello\n", true},
		{"typed, bare CR", sendRequest{mode: sendTyped, text: "a\rb", enter: true}, "a\nb\r", "a\nb", false},
		{"paste, bare CR", sendRequest{mode: sendPaste, text: "a\rb", enter: true}, "\x1b[200~a\nb\x1b[201~\r", "a\nb", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &fakeGate{}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, c.r)
			if o.err != nil {
				t.Fatalf("send: %v", o.err)
			}
			if got := log.settle(); got != c.wantBytes {
				t.Errorf("bytes %q, want %q", got, c.wantBytes)
			}
			evs, seen := g.eventLog()
			for i, ev := range evs {
				if ev == "check" && strings.Contains(seen[i], "\r") {
					t.Errorf("a \\r reached the PTY before the box check: %q", seen[i])
				}
			}
			if w := g.lastExpect(); w.Exact != c.wantExact {
				t.Errorf("verified against %q, want %q", w.Exact, c.wantExact)
			}
			if q := g.queries[0]; q.Paste != c.wantPaste {
				t.Errorf("query.Paste = %v, want %v: multi-line text waits for bracketed paste", q.Paste, c.wantPaste)
			}
		})
	}
}

// TestGatedTextControlRuneIsBadRequest (review H1, the other controls): a
// control rune other than tab and newline inside a gated send's text is an
// editing key the box check cannot account for — ctrl-c interrupts, Escape
// dismisses or ends a bracketed paste early, DEL erases — so it is refused
// before any byte or OUT row, like an Enter in a non-final position.
func TestGatedTextControlRuneIsBadRequest(t *testing.T) {
	for _, text := range []string{"a\x03b", "x\x1b[201~y", "\x7f"} {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			g := &fakeGate{}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, sendRequest{text: text, enter: true})
			wantErr(t, o, protocol.CodeBadRequest, "", false)
			if got := log.settle(); got != "" {
				t.Errorf("a refused send wrote %q", got)
			}
			if n := outRows(m); n != 0 {
				t.Errorf("a refused send filed %d OUT rows", n)
			}
		})
	}
	t.Run("tab is text", func(t *testing.T) {
		g := &fakeGate{}
		m, _, log := gatedMux(t, g)
		if o := runSend(t, m, sendRequest{text: "a\tb", enter: true}); o.err != nil {
			t.Fatalf("send: %v", o.err)
		}
		if got := log.settle(); got != "a\tb\r" {
			t.Errorf("bytes %q", got)
		}
	})
}

// TestUngatedCarriageReturnIsUntouched: the normalisation is the GATE's; noGate
// (and so every ungated pane, which never reaches classifySend's gated half)
// still writes the text exactly as sent (R6).
func TestUngatedCarriageReturnIsUntouched(t *testing.T) {
	never := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { t.Error("the gate was consulted"); return readyNow() }}
	m, _, log := gatedMux(t, never)
	if o := runSend(t, m, sendRequest{text: "a\rb\x03", enter: false, noGate: true}); o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "a\rb\x03" {
		t.Errorf("bytes %q, want the text verbatim", got)
	}
}

// TestDismissWaitsForTypingToken (review M5b): the Escape is a write, so it is
// serialised with every other delivery's typing by the pane's token. While
// another delivery holds the token no Escape may land between its chunks.
func TestDismissWaitsForTypingToken(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness {
		if up.Load() {
			return dialogUp("permission", true)
		}
		return readyNow()
	}}
	m, p, log := gatedMux(t, g)
	tok := m.typingToken(p)
	tok <- struct{}{} // another delivery is mid-typing
	ch := startSend(t, m, context.Background(), sendRequest{text: "go", enter: true, dismiss: true, readyTimeoutMs: 5000})
	time.Sleep(600 * time.Millisecond)
	if got := log.String(); got != "" {
		t.Errorf("%q was written while another delivery held the typing token", got)
	}
	<-tok
	waitUntil(t, 3*time.Second, "the Escape once the token was free", func() bool {
		return strings.Contains(log.String(), "\x1b")
	})
	up.Store(false)
	if o := awaitSend(t, ch, 10*time.Second); o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); strings.Trim(strings.TrimSuffix(got, "go\r"), "\x1b") != "" {
		t.Errorf("bytes %q: only Escapes, then the text", got)
	}
}

// TestSendBudgetHasNoSideEffect (review M2): the Budget hook runs before the
// op, on every transport, so it must not file anything. A pane-less send with
// two open routes used to put a second "refused" note on the panel — the
// budget's own — beside the op's.
func TestSendBudgetHasNoSideEffect(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	for _, pane := range []float64{0, 1} {
		if _, err := m.sockCapture(sockMsg{Type: "capture", Pane: pane}); err != nil {
			t.Fatalf("capture %v: %v", pane, err)
		}
	}
	before := len(panelSignals(m.control))
	if d := m.sendBudgetOf(json.RawMessage(`{"text":"x"}`)); d != 0 {
		t.Errorf("budget %v for an ambiguous pane-less send, want 0", d)
	}
	if after := len(panelSignals(m.control)); after != before {
		t.Errorf("the budget filed %d panel rows; it must be a pure lookup", after-before)
	}
	if _, err := m.control.targetPane(); err == nil {
		t.Error("targetPane no longer refuses the ambiguous send")
	}
	if after := len(panelSignals(m.control)); after != before+1 {
		t.Errorf("the op's own refusal filed %d rows, want 1", after-before)
	}
}
