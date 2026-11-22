package mux

// A gated send whose box stays empty after typing writes NO clear and NO
// second copy, on any pane and for any text. A ctrl-u clear is no cancel for
// bytes Claude Code has not read: one ctrl-u deletes one wrapped row only
// when it reaches Claude Code in a short read of its own while it is reading.
// Inside a read of about 64 bytes or more it is ignored, and early-input
// capture drops it, so a clear plus a second copy queued behind an unread
// first copy lands as two joined copies (TestStalledReaderNeverGetsASecondCopy).

import (
	"strings"
	"testing"
)

// retypeClearCols and retypeClearKeys are the clear a retype would need: one
// ctrl-u per wrapped row of a copy at the box's width (the pane less its
// frame and prompt, never below 10 cells; 14 columns is that floor), doubled
// for word-wrap waste, plus two. magmux writes no such clear; the stalled
// reader model uses the count to show that even a generous clear does not
// keep two copies apart once Claude reads them in one large read.
const retypeClearCols = 14

func retypeClearKeys(text string, cols int) int {
	width := max(cols-4, 10)
	cells := 0
	for _, r := range text {
		cells += runeWidth(r)
	}
	rows := (cells + width - 1) / width
	return 2*rows + 2
}

// TestNoEmptiedBoxSendWritesAClearOrASecondCopy: for every shape the clear
// before a retype once had to special-case — a pane under the 14-column
// floor, a wide pane, a typed multi-line text, a bash-mode "!" text, a text
// too long to clear, an auto text one cell over that — and for both causes
// of an empty box (a termios reset, a box never drawn), the send types the
// text once, writes no ctrl-u and no Enter, and answers text_mismatch saying
// the box stayed empty and the text was not retyped.
func TestNoEmptiedBoxSendWritesAClearOrASecondCopy(t *testing.T) {
	for _, c := range []struct {
		name string
		cols int // 0: the harness's own pane
		req  sendRequest
	}{
		{"narrow pane", retypeClearCols - 4, sendRequest{text: "hello", enter: true}},
		{"wide pane", 160, sendRequest{text: strings.Repeat("abcdefghij", 14), enter: true}},
		{"typed multi-line", 0, sendRequest{mode: sendTyped, text: "first line\nsecond line", enter: true}},
		{"bash mode", 0, sendRequest{text: "!ls -la", enter: true}},
		{"typed too long to clear", 0, sendRequest{mode: sendTyped, text: strings.Repeat("abcdefgh ", 150), enter: true}},
		{"auto one cell over the old cap", 0, sendRequest{text: strings.Repeat("a", 511), enter: true}},
	} {
		for _, cause := range []struct {
			name  string
			check func() BoxCheck
			word  string
		}{
			{"terminal_reset", lostBoxCheck, "reset"},
			{"never_drawn", boxEmptyCheck, "never drawn"},
		} {
			t.Run(c.name+"/"+cause.name, func(t *testing.T) {
				g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return cause.check() }}
				m, p, log := gatedMux(t, g)
				if c.cols > 0 {
					p.mu.Lock()
					p.screen = newScreen(20, c.cols)
					p.mu.Unlock()
				}
				o := runSend(t, m, c.req)
				got := log.settle()
				wantNotRetyped(t, o, cause.word)
				if strings.ContainsRune(got, 0x15) {
					t.Errorf("bytes %q: want no ctrl-u clear", got)
				}
				if strings.Contains(got, "\r") {
					t.Errorf("bytes %q: want no Enter", got)
				}
				want := c.req.text
				if c.req.mode == sendTyped {
					// typed mode writes each newline as ctrl-j
					var b strings.Builder
					for _, w := range typedWrites(c.req.text) {
						b.Write(w)
					}
					want = b.String()
				}
				if got != want {
					t.Errorf("bytes: %d written (%q), want the text once (%d)", len(got), trimForLog(got), len(want))
				}
			})
		}
	}
}

// trimForLog bounds a byte stream for an error message.
func trimForLog(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
