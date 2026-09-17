package mux

// ── Screen Buffer ─────────────────────────────────────────────────────────────

type Screen struct {
	rows, cols int
	cells      [][]Cell
	curY, curX int
	savedY     int
	savedX     int
	savedFg    Color
	savedBg    Color
	savedAttr  Attr
	fg, bg     Color
	attr       Attr
	scrollTop  int
	scrollBot  int // exclusive (equal to rows initially)
	originMode bool
	autoWrap   bool
	insert     bool
	xenl       bool // cursor past last column flag
	altScreen  *Screen

	// ── scrollback ──────────────────────────────────────────────────────────
	//
	// A bounded ring of rows that have scrolled off the TOP of this screen, and
	// a separate allocation from `cells` on purpose. The old shape was one
	// `rows+scrollbackLines` grid whose tail was never written and never read —
	// megabytes of blank cells per pane that scrollUp walked past on its way to
	// blanking the evicted row. A ring says what it is, fills lazily, and drops
	// the oldest row when it is full, which the tail could not express at all.
	//
	// sb[sbHead] is the next slot to be written, so the OLDEST kept row is
	// sb[(sbHead-sbLen+sbCap)%sbCap]; sbRow indexes it from oldest and is the
	// only thing that should do that arithmetic.
	//
	// sbCap is 0 on an ALTERNATE screen, and that is the load-bearing part of
	// the feature: a real terminal does not record the alt screen into history,
	// which is why quitting vim does not leave its buffer in your scrollback.
	// The alt screen is a whole separate *Screen (newAltScreen), so switching to
	// it and back cannot touch the primary's ring — see the 1049/47/1047 cases
	// in doCSI.
	//
	// Rows in the ring keep the width they had when they were evicted. Nothing
	// reflows them (see resize), and nothing paints them from `cells`-width
	// assumptions: viewRow hands back a row of whatever length it is, and both
	// readers — rowsText and renderPane — bound their walk by len(row).
	//
	// Guarded by the owning Pane's mu, like every other field here.
	sb     [][]Cell
	sbHead int
	sbLen  int
	sbCap  int
	// sbOff is how many rows the VIEWPORT has been scrolled back: 0 is live,
	// and it is also the scroll-mode flag — a pane is in scroll mode exactly
	// when this is non-zero, so there is no second piece of state that can
	// disagree with what is painted. Bounded by sbLen.
	sbOff int
}

func newScreen(rows, cols int) *Screen {
	// Clamp negative/zero dimensions so a PTY that reports 0x0 (or a layout
	// that produces underflow on extreme terminal sizes) doesn't panic in
	// makeGrid's underlying `make([][]Cell, rows)` call.
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	s := &Screen{
		rows:      rows,
		cols:      cols,
		fg:        defaultColor,
		bg:        defaultColor,
		scrollBot: rows,
		autoWrap:  true,
		sbCap:     scrollbackLimit,
	}
	// Exactly the viewport, and nothing behind it. History lives in the ring
	// above, which allocates as it fills.
	s.cells = makeGrid(rows, cols)
	return s
}

// newAltScreen builds the alternate screen for a pane. It is an ordinary Screen
// with its scrollback capacity set to zero, which is the one difference that
// matters: alt-screen content must never enter history. Every DEC 1049/47/1047
// site goes through here so that rule cannot be honoured on one of them and
// forgotten on another.
func newAltScreen(rows, cols int) *Screen {
	s := newScreen(rows, cols)
	s.sbCap = 0
	return s
}

func makeGrid(rows, cols int) [][]Cell {
	if rows < 0 {
		rows = 0
	}
	if cols < 0 {
		cols = 0
	}
	grid := make([][]Cell, rows)
	for i := range grid {
		grid[i] = make([]Cell, cols)
		for j := range grid[i] {
			grid[i][j] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
		}
	}
	return grid
}

func (s *Screen) resize(rows, cols int) {
	old := s.cells
	oldRows := len(old)
	oldCols := 0
	if oldRows > 0 {
		oldCols = len(old[0])
	}
	s.cells = makeGrid(rows, cols)
	// Copy what fits
	copyRows := min(oldRows, rows)
	for i := 0; i < copyRows; i++ {
		copyCols := min(oldCols, cols)
		for j := 0; j < copyCols; j++ {
			s.cells[i][j] = old[i][j]
		}
	}
	s.rows = rows
	s.cols = cols
	// The scrollback SURVIVES a resize, at the width each row had when it was
	// evicted, and nothing reflows. Two reasons, and the first is the practical
	// one: a resize happens every time the human reveals the control panel or
	// nudges their window, and destroying every line of history at that moment
	// would make the feature untrustworthy. The second is that reflowing is a
	// guess — magmux does not record which rows were soft-wrapped continuations
	// of one logical line, so re-wrapping 200-column history into 80 columns
	// would invent line breaks the child never wrote. Handing the row back at
	// its original width is at least the truth about what was printed.
	//
	// The only thing a narrower screen changes is the viewport clamp below.
	if s.sbOff > s.sbLen {
		s.sbOff = s.sbLen
	}
	if s.scrollBot > rows || s.scrollBot == 0 {
		s.scrollBot = rows
	}
	if s.scrollTop >= rows {
		s.scrollTop = 0
	}
	s.curY = min(s.curY, rows-1)
	s.curX = min(s.curX, cols-1)
}

func (s *Screen) clearLine(row, from, to int) {
	if row < 0 || row >= len(s.cells) {
		return
	}
	for j := from; j < to && j < len(s.cells[row]); j++ {
		s.cells[row][j] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
	}
}

// scrollUp shifts [top,bot) up by one and blanks the new bottom row.
//
// This is the hottest path in the VT parser — every newline at the bottom of a
// screen lands here — so it must not allocate per line. It does not: the row
// evicted from the top and the row that becomes the new bottom are SWAPPED, as
// they always were. The only change is where the evicted row goes. When the
// scroll is the whole screen it goes into the scrollback ring, and the ring
// hands back a row to recycle in its place; otherwise it is recycled directly,
// byte for byte as before.
func (s *Screen) scrollUp(top, bot int) {
	if top >= bot || top < 0 || bot > len(s.cells) {
		return
	}
	// Shift rows up by 1 within [top, bot)
	save := s.cells[top]
	copy(s.cells[top:bot-1], s.cells[top+1:bot])
	// Only a FULL-SCREEN scroll writes history, which is what every terminal
	// does: a child that set a scrolling region (a pager's header, a TUI's
	// status line, DECSTBM in general) is animating part of its own frame, and
	// recording those rows would fill the ring with fragments of a redraw
	// rather than with output that ever "scrolled off".
	if s.sbCap > 0 && top == 0 && bot == s.rows {
		save = s.pushScrollback(save)
	}
	// Clear the bottom row
	for j := range save {
		save[j] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
	}
	s.cells[bot-1] = save
}

func (s *Screen) scrollDown(top, bot int) {
	if top >= bot || top < 0 || bot > len(s.cells) {
		return
	}
	save := s.cells[bot-1]
	copy(s.cells[top+1:bot], s.cells[top:bot-1])
	for j := range save {
		save[j] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
	}
	s.cells[top] = save
}
