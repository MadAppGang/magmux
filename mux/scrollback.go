package mux

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// scrollbackLimit is how many evicted rows one PRIMARY screen keeps, and it is
// the only bound on magmux's history: rows past it are dropped oldest-first and
// are gone for good.
//
// It is a variable rather than the constant above because the memory is real
// and per-pane. A row costs cols×sizeof(Cell) — about 20 bytes a cell — so a
// 200-column pane that has scrolled 1000 lines holds roughly 4 MB, and eight of
// them hold thirty. The ring fills LAZILY (pushScrollback allocates one row per
// eviction until it is full and recycles from then on), so a pane that never
// scrolls costs nothing at all; the number below is the ceiling, not the
// footprint. MAGMUX_SCROLLBACK=0 turns the whole thing off.
var scrollbackLimit = envScrollback()

func envScrollback() int {
	v := os.Getenv("MAGMUX_SCROLLBACK")
	if v == "" {
		return scrollbackLines
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "magmux: ignoring MAGMUX_SCROLLBACK=%q (want a non-negative integer)\n", v)
		return scrollbackLines
	}
	// A ceiling on the ceiling: a typo with an extra zero should not be able to
	// commit a gigabyte per pane before the first line is printed.
	if n > 100000 {
		n = 100000
	}
	return n
}

// pushScrollback files `row` as the newest scrollback line and returns a row the
// caller may reuse as the new bottom of the viewport.
//
// The swap is what keeps scrollUp allocation-free in steady state: once the ring
// is full, the row being dropped is handed straight back. While it is still
// filling there is nothing to hand back, so one row is allocated per eviction —
// at most sbCap times for the life of the pane.
//
// Caller holds the owning pane's mu.
func (s *Screen) pushScrollback(row []Cell) []Cell {
	if s.sb == nil {
		s.sb = make([][]Cell, s.sbCap)
	}
	var reuse []Cell
	if s.sbLen == s.sbCap {
		reuse = s.sb[s.sbHead] // the oldest line, about to be overwritten
	} else {
		s.sbLen++
	}
	s.sb[s.sbHead] = row
	s.sbHead++
	if s.sbHead == s.sbCap {
		s.sbHead = 0
	}
	// A recycled row is only reusable at the CURRENT width: renderPane walks the
	// viewport by s.cols without a length check, so a row left over from a wider
	// or narrower screen has to be replaced rather than trimmed.
	if len(reuse) != s.cols {
		reuse = make([]Cell, s.cols)
	}
	// Keep a scrolled-back viewport parked on the same content while output
	// keeps arriving, exactly as tmux's copy-mode does. It stops at sbLen
	// because past that the line being looked at is the one just dropped.
	if s.sbOff > 0 && s.sbOff < s.sbLen {
		s.sbOff++
	}
	return reuse
}

// sbRow returns the i-th OLDEST scrollback row, or nil when i is out of range.
// It is the only place the ring's index arithmetic lives.
//
// Caller holds the owning pane's mu.
func (s *Screen) sbRow(i int) []Cell {
	if i < 0 || i >= s.sbLen {
		return nil
	}
	return s.sb[(s.sbHead-s.sbLen+i+s.sbCap)%s.sbCap]
}

// viewRow returns the cells shown at viewport row `i` when the pane is scrolled
// back `off` rows: off == 0 is the live screen, and larger values reach further
// into history. Rows above the oldest kept line come back nil, which every
// caller renders as blank.
//
// Scrollback and viewport are one document here — history rows sit directly on
// top of cells[0] — so this is the single mapping the renderer, capture and the
// scroll keys all share. A second one would drift the moment the ring wrapped.
//
// Caller holds the owning pane's mu.
func (s *Screen) viewRow(off, i int) []Cell {
	d := i - off
	if d >= 0 {
		if d < len(s.cells) {
			return s.cells[d]
		}
		return nil
	}
	return s.sbRow(s.sbLen + d)
}

// scrollBackBy moves the viewport `delta` rows further into history (negative
// moves back toward live) and returns the offset it settled on. Clamped to
// [0, sbLen], so "further back than there is history" parks at the top rather
// than failing.
//
// Caller holds the owning pane's mu.
func (s *Screen) scrollBackBy(delta int) int {
	s.sbOff = clamp(s.sbOff+delta, 0, s.sbLen)
	return s.sbOff
}

// ── scroll mode ──────────────────────────────────────────────────────────────
//
// A focused SESSION pane can be scrolled back through its own history. The
// mechanism has to satisfy one hard constraint: arrows, PageUp/PageDown and the
// wheel must keep reaching the child, because a full-screen TUI needs every one
// of them. So scrolling is a MODE, entered from the Ctrl-G prefix that already
// exists (Ctrl-G [, tmux's copy-mode binding), and it is entered by an action
// rather than by a toggle — Ctrl-G [ scrolls back one page and you are in it.
//
// The mode has no flag of its own. A pane is in scroll mode exactly when
// screen.sbOff > 0, which is also exactly the condition that paints the badge
// and the condition the renderer composes history under. One piece of state
// cannot disagree with itself: scrolling back to live IS leaving.
//
// While the mode is on, keys are consumed by consumeScrollKey and none of them
// reach the child. That is deliberate and it is why entry is deliberate too: a
// user who has not pressed Ctrl-G [ can never lose a keystroke to this.

// scrollFocusedBy moves the focused pane's viewport by delta rows (positive =
// further back) and returns whether anything could be scrolled. It refuses on
// the control panel, which has its own scrolling, and on a pane with no history
// — an alternate-screen pane being the case that matters, since it records none.
//
// Caller must NOT hold treeMu.
func (m *Magmux) scrollFocusedBy(delta int) bool {
	m.treeMu.Lock()
	f := m.focused
	if f == nil || f.isControl || f.screen == nil {
		if f != nil && f.isControl {
			m.noteChromeLocked("the panel scrolls with k/j/g/G")
		}
		m.treeMu.Unlock()
		return false
	}
	f.mu.Lock()
	s := f.screen
	ok := s.sbLen > 0 || s.sbOff > 0
	if ok {
		s.scrollBackBy(delta)
		f.dirty = true
	}
	alt := f.altMode
	f.mu.Unlock()
	if !ok {
		if alt {
			// The single most useful thing magmux can say here. Claude Code, vim
			// and htop all live on the alternate screen, and "nothing happened"
			// would read as a broken key rather than as a property of the app.
			m.noteChromeLocked("no scrollback: this pane is on the alternate screen")
		} else {
			m.noteChromeLocked("nothing has scrolled off this pane yet")
		}
	}
	m.markAllDirtyLocked()
	m.treeMu.Unlock()
	return ok
}

// scrollFocusedTo parks the focused pane at an absolute offset: 0 is live and
// anything past the oldest kept line clamps to it.
//
// Caller must NOT hold treeMu.
func (m *Magmux) scrollFocusedTo(off int) {
	m.treeMu.Lock()
	if f := m.focused; f != nil && !f.isControl && f.screen != nil {
		f.mu.Lock()
		f.screen.sbOff = clamp(off, 0, f.screen.sbLen)
		f.dirty = true
		f.mu.Unlock()
	}
	m.markAllDirtyLocked()
	m.treeMu.Unlock()
}

// focusedScrollOff is how far back the focused pane is, and therefore whether
// the next keystroke belongs to scroll mode. Zero when nothing is focused.
//
// Caller must NOT hold treeMu.
func (m *Magmux) focusedScrollOff() int {
	m.treeMu.RLock()
	f := m.focused
	m.treeMu.RUnlock()
	if f == nil || f.screen == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screen.sbOff
}

// scrollPageLocked is how much one page key moves: a screenful less two rows of
// overlap, so a reader can see where they were. Caller holds at least
// treeMu.RLock.
func scrollPage(h int) int { return maxInt(1, h-2) }

// consumeScrollKey handles a keystroke while the focused pane is scrolled back.
// Returns how many bytes it consumed, or 0 to let the normal path have them.
//
// Nothing here reaches the child. The keys are the ones the control panel
// already uses (k/j/g/G, arrows, PgUp/PgDn) so there is one set to learn, plus
// q/Enter/Esc to return to live — the three keys a human tries when they want
// out of something.
//
// Caller must NOT hold treeMu.
func (m *Magmux) consumeScrollKey(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	m.treeMu.RLock()
	page := 20
	if f := m.focused; f != nil {
		page = scrollPage(f.h)
	}
	m.treeMu.RUnlock()

	switch buf[0] {
	case 'k':
		m.scrollFocusedBy(1)
		return 1
	case 'j':
		m.scrollFocusedBy(-1)
		return 1
	case 'g':
		m.scrollFocusedTo(1 << 30) // clamped to the oldest line kept
		return 1
	case 'G':
		m.scrollFocusedTo(0)
		return 1
	case ' ':
		m.scrollFocusedBy(-page)
		return 1
	case 'b':
		m.scrollFocusedBy(page)
		return 1
	case 'q', '\r', '\n':
		m.scrollFocusedTo(0)
		return 1
	}
	if buf[0] != 0x1b {
		// Any other printable key is swallowed rather than typed. A pane showing
		// history is not showing the prompt the keystroke was aimed at, and
		// letting it through would put text into a session the user cannot see.
		return 1
	}
	// ESC. A lone one is the user pressing Escape — the finished grid already
	// reads it that way — and anything that is not a CSI cannot be a key this
	// mode knows.
	if len(buf) == 1 || (buf[1] != '[' && buf[1] != 'O') {
		m.scrollFocusedTo(0)
		return 1
	}
	if buf[1] != '[' || len(buf) < 3 {
		return 0 // incomplete; wait for the rest rather than guessing
	}
	switch buf[2] {
	case 'A': // up
		m.scrollFocusedBy(1)
		return 3
	case 'B': // down
		m.scrollFocusedBy(-1)
		return 3
	case 'H': // home
		m.scrollFocusedTo(1 << 30)
		return 3
	case 'F': // end
		m.scrollFocusedTo(0)
		return 3
	case '5': // PgUp: ESC [ 5 ~
		if len(buf) < 4 {
			return 0
		}
		if buf[3] == '~' {
			m.scrollFocusedBy(page)
			return 4
		}
	case '6': // PgDn: ESC [ 6 ~
		if len(buf) < 4 {
			return 0
		}
		if buf[3] == '~' {
			m.scrollFocusedBy(-page)
			return 4
		}
	case '<': // an SGR mouse report — the wheel still works in scroll mode
		return 0
	}
	// An unrecognised CSI: consume it whole so a function key cannot leak into
	// a session that is not showing its own prompt.
	end := 2
	for end < len(buf) {
		if buf[end] >= 0x40 && buf[end] <= 0x7e {
			return end + 1
		}
		end++
	}
	return 0
}
