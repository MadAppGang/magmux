package main

import (
	"strings"
	"testing"
)

// writeRow paints a string into row r starting at column 0, marking the right
// half of every double-width rune Cont exactly as the VT parser does.
func writeRow(s *Screen, r int, text string) {
	c := 0
	for _, ch := range text {
		if c >= s.cols {
			return
		}
		s.cells[r][c] = Cell{Ch: ch, Fg: defaultColor, Bg: defaultColor}
		if runeWidth(ch) == 2 {
			s.cells[r][c].Wide = true
			if c+1 < s.cols {
				s.cells[r][c+1] = Cell{Ch: ch, Fg: defaultColor, Bg: defaultColor, Cont: true}
			}
			c += 2
			continue
		}
		c++
	}
}

// TestRowsTextMatchesSelCopySemantics pins the cell walk selCopy has always
// used, now that capture shares it: a NUL cell is a space (not a lost column),
// the right half of a wide character is skipped (not doubled), and every line
// is right-trimmed. It then covers what capture adds on top — the blank-tail
// strip, the last-N cut, and the Truncated flag that reports it.
func TestRowsTextMatchesSelCopySemantics(t *testing.T) {
	s := newScreen(6, 12)
	writeRow(s, 0, "hi 日本")    // wide runes: two cells each, one rune of text
	writeRow(s, 1, "trail   ") // trailing spaces must not survive
	writeRow(s, 2, "abc")
	// A cell the child never wrote: the parser leaves Ch at 0, which must
	// render as a space rather than a NUL byte in the middle of the line.
	s.cells[2][0] = Cell{Ch: 0, Fg: defaultColor, Bg: defaultColor}
	// Rows 3-5 stay blank — the tail capture is expected to drop.

	got := s.rowsText(0, s.rows, 0, s.cols-1)
	want := []string{"hi 日本", "trail", " bc", "", "", ""}
	if len(got) != len(want) {
		t.Fatalf("rowsText returned %d rows %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Sub-ranges: the shapes selCopy asks for (anchor row from x0 to the right
	// edge, cursor row from the left edge to x1), plus clamping past the buffer.
	if r := s.rowsText(1, 2, 2, s.cols-1); len(r) != 1 || r[0] != "ail" {
		t.Errorf("rowsText(1,2,2,cols-1) = %q, want [\"ail\"]", r)
	}
	if r := s.rowsText(2, 3, 0, 1); len(r) != 1 || r[0] != " b" {
		t.Errorf("rowsText(2,3,0,1) = %q, want [\" b\"]", r)
	}
	if r := s.rowsText(4, s.rows+40, 0, s.cols+40); len(r) != 2 {
		t.Errorf("rowsText past the buffer returned %d rows, want 2 (clamped to s.rows)", len(r))
	}

	p := &Pane{screen: s}

	full := p.capture(0, false)
	if full.Rows != 6 || full.Cols != 12 {
		t.Errorf("capture geometry = %dx%d, want 6x12", full.Rows, full.Cols)
	}
	if strings.Count(full.Text, "\n") != 5 {
		t.Errorf("capture(0,false) returned %d lines, want 6 (no strip, no cut): %q",
			strings.Count(full.Text, "\n")+1, full.Text)
	}
	if full.Truncated {
		t.Error("capture(0,false) reported Truncated with nothing to cut")
	}

	stripped := p.capture(0, true)
	if stripped.Text != "hi 日本\ntrail\n bc" {
		t.Errorf("capture(0,true) = %q, want the three non-blank rows", stripped.Text)
	}
	if stripped.Truncated {
		t.Error("capture(0,true) reported Truncated for a blank-tail strip; only the lastN cut truncates")
	}

	// The tail is stripped BEFORE the cut, so asking for 2 lines of a 6-row
	// screen holding 3 rows of content returns the last two of those three —
	// not two blanks off the bottom.
	tail := p.capture(2, true)
	if tail.Text != "trail\n bc" {
		t.Errorf("capture(2,true) = %q, want the last two content rows", tail.Text)
	}
	if !tail.Truncated {
		t.Error("capture(2,true) dropped a row without reporting Truncated")
	}

	// lastN larger than the content is not a truncation.
	if all := p.capture(99, true); all.Truncated || all.Text != stripped.Text {
		t.Errorf("capture(99,true) = %q truncated=%v, want the full stripped text untruncated",
			all.Text, all.Truncated)
	}
}

// TestCaptureReportsAltMode covers the one field an observer cannot infer from
// the text: which of the two screens it is looking at.
func TestCaptureReportsAltMode(t *testing.T) {
	p := &Pane{screen: newScreen(3, 8)}
	if p.capture(0, true).Alt {
		t.Error("capture reported Alt on a primary-screen pane")
	}
	p.altMode = true
	if !p.capture(0, true).Alt {
		t.Error("capture did not report Alt on an alt-screen pane")
	}
}
