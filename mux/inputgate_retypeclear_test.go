package mux

// Every retype is preceded by a clear: ctrl-u keys, one per write, enough to
// delete a copy of the text that Claude read and has not drawn. These pin the
// count, the cases that are not retyped because a clear is unsafe or too
// long, and the Enter withheld when the box held more than typed after one.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/protocol"
)

// retypeBytes is the byte stream of a send whose text was retyped once: the
// text, the clear (counted at retypeClearCols, whatever the pane's width),
// the text again.
func retypeBytes(text string) string {
	return text + strings.Repeat("\x15", retypeClearKeys(text, retypeClearCols)) + text
}

func TestRetypeClearKeysCoversWrappedRows(t *testing.T) {
	long140 := strings.Repeat("abcdefghij", 14)
	wide := strings.Repeat("日本語", 10) // 30 runes, 60 cells
	for _, c := range []struct {
		name string
		text string
		cols int
		want int
	}{
		{"one row", "hello", 60, 4},
		{"empty", "", 60, 2},
		{"exactly one row at 60", strings.Repeat("a", 56), 60, 4},
		{"one cell over a row at 60", strings.Repeat("a", 57), 60, 6},
		{"140 at the floor", long140, retypeClearCols, 2*14 + 2}, // width 10
		{"140 at 20", long140, 20, 2*9 + 2},                      // width 16
		{"140 at 40", long140, 40, 2*4 + 2},                      // width 36
		{"140 at 60", long140, 60, 2*3 + 2},                      // width 56
		{"140 at 120", long140, 120, 2*2 + 2},                    // width 116
		{"wide runes count two cells", wide, 40, 2*2 + 2},
		{"a tiny pane never wraps below 10", "hello world!", 5, 2*2 + 2},
		{"the longest clearable line at the floor", strings.Repeat("a", 510), retypeClearCols, 2*51 + 2},
		{"one cell over the cap", strings.Repeat("a", 511), retypeClearCols, 2*52 + 2},
	} {
		if got := retypeClearKeys(c.text, c.cols); got != c.want {
			t.Errorf("%s: retypeClearKeys(%d cells, %d cols) = %d, want %d", c.name, len(c.text), c.cols, got, c.want)
		}
	}
	// The floor's box is the narrowest the row model counts, so a wider count
	// can never exceed it.
	if got, want := max(retypeClearCols-4, 10), 10; got != want {
		t.Errorf("retypeClearCols %d gives a %d-cell box, want the %d-cell floor", retypeClearCols, got, want)
	}
	// The cap holds a single-line text up to 51 rows of the floor's box.
	if n := retypeClearKeys(strings.Repeat("a", 510), retypeClearCols); n > protocol.SendRetypeClearMax {
		t.Errorf("a 510-cell text needs %d keys at %d columns; want it within SendRetypeClearMax (%d)",
			n, retypeClearCols, protocol.SendRetypeClearMax)
	}
	if n := retypeClearKeys(strings.Repeat("a", 511), retypeClearCols); n <= protocol.SendRetypeClearMax {
		t.Errorf("a 511-cell text needs %d keys; want it over SendRetypeClearMax (%d)", n, protocol.SendRetypeClearMax)
	}

	// Against the sequential model: copy 1, the clear, copy 2 and an Enter
	// submit exactly one copy at every width the pane may have been resized
	// to after the count, the floor included.
	for _, cols := range []int{retypeClearCols, 15, minPaneCols, 40, 60, 120, 160} {
		for _, text := range []string{"hello", long140, strings.Repeat("word ", 40), strings.Repeat("a", 510)} {
			got := claudeInputModel(retypeBytes(text)+"\r", cols)
			if len(got) != 1 || got[0] != text {
				t.Errorf("%d cols, %d runes: submitted %q, want exactly one copy", cols, len(text), got)
			}
		}
	}
}

// TestRetypeClearIsCountedAtTheNarrowestPane: a retype on a pane of any width
// writes the clear the row model's floor (retypeClearCols) needs, not the
// shorter one its own width needs. The pane can be resized between the count
// and the clear, a SIGWINCH reshape can clamp it below minPaneCols, and a
// count at the old width would leave residue in a box narrowed meanwhile.
func TestRetypeClearIsCountedAtTheNarrowestPane(t *testing.T) {
	text := strings.Repeat("abcdefghij", 14) // 140 cells: 14 rows at the floor, 9 at 20 columns, 1 at 160
	for _, wide := range []int{minPaneCols, 160} {
		t.Run(strconv.Itoa(wide), func(t *testing.T) {
			if floor, own := retypeClearKeys(text, retypeClearCols), retypeClearKeys(text, wide); floor <= own {
				t.Fatalf("test text needs %d keys at %d columns and %d at %d; want the floor's count larger",
					floor, retypeClearCols, own, wide)
			}
			g := &fakeGate{}
			g.checkFn = func(BoxExpect, int) BoxCheck {
				if strings.Count(g.log.String(), text) < 2 {
					return lostBoxCheck()
				}
				return verifiedBox(verifiedExact)
			}
			m, p, log := gatedMux(t, g)
			p.mu.Lock()
			p.screen = newScreen(20, wide)
			p.mu.Unlock()

			o := runSend(t, m, sendRequest{text: text, enter: true})
			stream := log.settle()
			if o.err != nil {
				t.Fatalf("send: %v (bytes %q)", o.err, stream)
			}
			if res := replyOf(o); res["retyped"] != true {
				t.Fatalf("reply %v: want retyped:true (the retype path is what this test drives)", res)
			}
			if got, want := strings.Count(stream, "\x15"), retypeClearKeys(text, retypeClearCols); got != want {
				t.Errorf("the retype wrote %d ctrl-u keys on a %d-column pane, want %d (the %d-column count)",
					got, wide, want, retypeClearCols)
			}
			// Whatever width the pane was resized to after the count, down to
			// the floor, one copy is submitted.
			for _, cols := range []int{retypeClearCols, minPaneCols, 40, wide} {
				if got := claudeInputModel(stream, cols); len(got) != 1 || got[0] != text {
					t.Errorf("at %d columns Claude would submit %q, want exactly [%q]", cols, got, text)
				}
			}
		})
	}
}

// TestPaneNarrowerThanTheClearFloorIsNotRetyped: on a pane narrower than
// retypeClearCols the box is under the row model's floor, so no count can be
// trusted: text_mismatch, typed once, no clear, no Enter.
func TestPaneNarrowerThanTheClearFloorIsNotRetyped(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return lostBoxCheck() }}
	m, p, log := gatedMux(t, g)
	p.mu.Lock()
	p.screen = newScreen(20, retypeClearCols-4)
	p.mu.Unlock()

	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	why := "narrower than " + strconv.Itoa(retypeClearCols) + " columns"
	if msg := o.err.Error(); !strings.Contains(msg, why) || !strings.Contains(msg, "not retyped") {
		t.Errorf("error %q does not say why (%q) it was not retyped", msg, why)
	}
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q: want the text once, no clear and no Enter", got)
	}
	noRetype(t, o)
}

// TestMultiLineTypedOrShellTextIsNotRetyped: a text that would have been
// retyped, but whose clear is unmeasured — a newline in typed mode, or a
// leading "!" (bash mode) — answers text_mismatch, typed once, no clear, no
// Enter.
func TestMultiLineTypedOrShellTextIsNotRetyped(t *testing.T) {
	for _, c := range []struct {
		name string
		req  sendRequest
		why  string
	}{
		{"typed multi-line", sendRequest{mode: sendTyped, text: "first line\nsecond line", enter: true}, "more than one line"},
		{"bash mode", sendRequest{text: "!ls -la", enter: true}, "bash mode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return lostBoxCheck() }}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, c.req)
			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			if msg := o.err.Error(); !strings.Contains(msg, c.why) || !strings.Contains(msg, "not retyped") {
				t.Errorf("error %q does not say why (%q) it was not retyped", msg, c.why)
			}
			got := log.settle()
			if strings.Contains(got, "\x15") || strings.Contains(got, "\r") {
				t.Errorf("bytes %q: want no clear and no Enter", got)
			}
			noRetype(t, o)
		})
	}
}

// TestTooLongToClearIsNotRetyped: a single-line text whose clear would exceed
// SendRetypeClearMax keys at the 10-cell box of retypeClearCols is not
// retyped, on a pane of any width — including an auto text one cell past the
// cap, well under the paste threshold.
func TestTooLongToClearIsNotRetyped(t *testing.T) {
	for _, c := range []struct {
		name string
		req  sendRequest
	}{
		// 1350 cells: 135 rows of the floor's box.
		{"typed", sendRequest{mode: sendTyped, text: strings.Repeat("abcdefgh ", 150), enter: true}},
		// 511 cells: 52 rows, 106 keys.
		{"auto one cell over", sendRequest{text: strings.Repeat("a", 511), enter: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return lostBoxCheck() }}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, c.req)
			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			if msg := o.err.Error(); !strings.Contains(msg, "too long to clear safely") {
				t.Errorf("error %q does not say the text was too long to clear", msg)
			}
			if got := log.settle(); got != c.req.text {
				t.Errorf("bytes: %d written, want the text once (%d), no clear and no Enter", len(got), len(c.req.text))
			}
			noRetype(t, o)
		})
	}
}

// TestExcessAfterRetypeWithholdsEnter: after the clear and the retype the box
// held MORE than was typed on one read and verified on a later one. The clear
// under-counted, so the buffer may still hold part of the first copy: no
// Enter.
func TestExcessAfterRetypeWithholdsEnter(t *testing.T) {
	g := &fakeGate{}
	excessShown := false
	g.checkFn = func(BoxExpect, int) BoxCheck {
		if strings.Count(g.log.String(), "hello") < 2 {
			return lostBoxCheck()
		}
		if !excessShown {
			excessShown = true
			return boxMismatchCheck("hellohel", "the box holds more than was typed: 8 non-space runes where 5 were typed", true)
		}
		return verifiedBox(verifiedExact)
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if !strings.Contains(o.err.Error(), "more than was typed after the retype") {
		t.Errorf("error %q does not say the box held more than typed after the retype", o.err)
	}
	if got := log.settle(); got != retypeBytes("hello") {
		t.Errorf("bytes %q, want %q (one cleared retype) and no Enter", got, retypeBytes("hello"))
	}
}
