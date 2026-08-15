package main

// Screen → plain text.
//
// magmux has always had exactly one cell-walk that turns a Screen back into
// characters: the one inside selCopy, written for mouse selection. Anything
// else that wants text — an exit event's last line, an external observer
// asking what a pane looks like, a row read back out of the scrollback ring —
// needs the same walk, and a second copy of it would drift on the first
// wide-character or NUL-cell fix. So the walk lives here once, as rowText, and
// selCopy is one of its callers.
//
// Three readers now sit on it: rowsText (a rectangle of the live screen, which
// is what selection asks for), viewText (a screenful at a scrollback offset,
// which is what capture asks for), and through them selCopy and the socket.

import "strings"

// rowsText extracts rows [y0,y1) columns [x0,x1] (inclusive of x1) as
// right-trimmed strings, using the cell walk selCopy has always used:
//
//   - a zero rune renders as a space (cells a child never wrote),
//   - Cont cells — the right half of a double-width character — are skipped,
//     so the rune is emitted once and the string is not padded to the cell
//     count,
//   - each line is right-trimmed of spaces.
//
// Bounds are clamped rather than rejected. Callers pass selection coordinates
// and pane geometry, both of which can outrun the buffer after a resize, and
// clamping is exactly what selCopy's `r < s.rows` / `c < s.cols` guards did.
//
// Caller must hold the owning pane's mu.
func (s *Screen) rowsText(y0, y1, x0, x1 int) []string {
	if y0 < 0 {
		y0 = 0
	}
	if y1 > s.rows {
		y1 = s.rows
	}
	if y1 > len(s.cells) {
		y1 = len(s.cells)
	}
	if x0 < 0 {
		x0 = 0
	}
	lines := make([]string, 0, maxInt(0, y1-y0))
	for r := y0; r < y1; r++ {
		end := x1
		if end > s.cols-1 {
			end = s.cols - 1
		}
		lines = append(lines, rowText(s.cells[r], x0, end))
	}
	return lines
}

// rowText is THE cell walk — the loop selCopy has always used, now that
// scrollback rows are read by it too. It is extracted rather than duplicated for
// the reason stated at the top of this file: a second copy drifts on the first
// wide-character or NUL-cell fix, and history and selection disagreeing about
// what a row said is a bug nobody would look for.
//
// x1 is inclusive and both ends are clamped to the row. A SCROLLBACK row keeps
// the width it had when it was evicted (Screen.resize reflows nothing), so it
// can be wider or narrower than the screen showing it; clamping to len(row)
// here is what lets one walk serve both.
func rowText(row []Cell, x0, x1 int) string {
	if x0 < 0 {
		x0 = 0
	}
	if x1 > len(row)-1 {
		x1 = len(row) - 1
	}
	var line strings.Builder
	for c := x0; c <= x1; c++ {
		ch := row[c].Ch
		if ch == 0 {
			ch = ' '
		}
		if !row[c].Cont {
			line.WriteRune(ch)
		}
	}
	return strings.TrimRight(line.String(), " ")
}

// viewText renders the screenful that is `off` rows above the live bottom: off
// 0 is the visible screen, off == s.rows is the screenful directly above it, and
// off == s.sbLen is the oldest history magmux still holds.
//
// It goes through Screen.viewRow, the same mapping renderPane paints and the
// scroll keys move, so what an agent reads back at a given offset is exactly
// what a human scrolled to that offset would see. Rows past the top of history
// come back as empty strings rather than being dropped, so the returned slice is
// always one screenful and a caller's row arithmetic does not change at the
// boundary.
//
// A history row is emitted at its FULL width, not the screen's: it is text that
// was printed, and truncating it to a window that has since been made narrower
// would lose characters that are still in the buffer.
//
// Caller must hold the owning pane's mu.
func (s *Screen) viewText(off int) []string {
	lines := make([]string, 0, maxInt(0, s.rows))
	for i := 0; i < s.rows; i++ {
		row := s.viewRow(off, i)
		if row == nil {
			lines = append(lines, "")
			continue
		}
		end := len(row) - 1
		if i-off >= 0 && end > s.cols-1 {
			end = s.cols - 1 // a live row is bounded by the screen, as ever
		}
		lines = append(lines, rowText(row, 0, end))
	}
	return lines
}

// PaneCapture is a screenful of a pane as text, plus the geometry needed to
// interpret it: an observer that does not know the pane is 40 columns wide
// cannot tell a wrapped line from a hard one.
//
// Offset / Scrollback / AtTop are what make a capture navigable. Offset is how
// far back this screenful was taken, Scrollback is how many rows of history
// exist above the live screen, and AtTop says the oldest of them is in view —
// so a caller walking backwards knows both how much further it can go and when
// it has arrived, without inferring either from a short answer.
type PaneCapture struct {
	Text       string
	Rows, Cols int
	CurY, CurX int
	Alt        bool
	Truncated  bool
	Offset     int
	Scrollback int
	AtTop      bool
}

// capture renders the pane's visible screen to plain text.
//
// stripBlankTail drops trailing blank rows *before* the lastN cut, so a 48-row
// alt screen holding 20 rows of content returns those 20 rather than 28 blanks
// and nothing else. lastN > 0 keeps the LAST N rows — an interactive session's
// prompt lives at the bottom — and Truncated says whether anything was dropped
// to make that fit. lastN <= 0 means the whole screen.
//
// Caller must NOT hold p.mu.
func (p *Pane) capture(lastN int, stripBlankTail bool) PaneCapture {
	return p.captureAt(0, lastN, stripBlankTail)
}

// captureAt is capture with a scrollback offset: `offset` rows further back than
// the live screen, 0 being the live screen itself.
//
// The offset counts ROWS, not screenfuls, and it is measured from the bottom of
// the live screen — so offset == Rows is the screenful directly above what is on
// display, and successive reads at Rows, 2*Rows, 3*Rows walk history backwards
// without overlap. It is clamped to the history that exists rather than
// rejected: a caller asking for 5000 rows back on a pane holding 300 gets the
// oldest screenful and AtTop, which is the answer it was going to have to
// discover anyway.
//
// Scrollback accumulates from the PRIMARY screen only, so an alternate-screen
// pane (Claude Code, vim, htop) reports Scrollback 0 and every offset returns
// the live screen. That is not a limitation to work around — it is what a
// terminal does, and for those panes the session's own transcript is the
// history that exists.
//
// Caller must NOT hold p.mu.
func (p *Pane) captureAt(offset, lastN int, stripBlankTail bool) PaneCapture {
	p.mu.Lock()
	out := PaneCapture{Alt: p.altMode}
	var lines []string
	if s := p.screen; s != nil {
		out.Rows, out.Cols = s.rows, s.cols
		out.CurY, out.CurX = s.curY, s.curX
		out.Scrollback = s.sbLen
		out.Offset = clamp(offset, 0, s.sbLen)
		out.AtTop = out.Offset >= s.sbLen
		lines = s.viewText(out.Offset)
	}
	p.mu.Unlock()

	// Everything below is pure string work on our own slice, so it runs with
	// the pane unlocked — a capture must never hold p.mu while the read loop
	// wants it.
	if stripBlankTail {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
	}
	if lastN > 0 && len(lines) > lastN {
		lines = lines[len(lines)-lastN:]
		out.Truncated = true
	}
	out.Text = strings.Join(lines, "\n")
	return out
}
