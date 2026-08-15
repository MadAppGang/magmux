// magmux — Minimal Go Terminal Multiplexer
// Port of MTM (Rob King) from C to Go, zero third-party dependencies.
// Uses only golang.org/x/sys and golang.org/x/term.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Set by GoReleaser ldflags. Default value used for local builds.
var (
	Version = "dev"
	Commit  = "none"
)

// magmuxLabel returns the status-bar app label, e.g. "magmux v3.4.2".
// Falls back to "magmux" when the version is the unreleased "dev" value.
func magmuxLabel() string {
	if Version == "" || Version == "dev" {
		return "magmux"
	}
	if strings.HasPrefix(Version, "v") {
		return "magmux " + Version
	}
	return "magmux v" + Version
}

// approxStatusWidth estimates the on-screen width of a tab-separated
// "CODE:text" status-bar string. Used to decide whether the status bar
// has enough room for the attribution tail. Not exact — overestimates
// slightly to stay on the safe side.
func approxStatusWidth(s string) int {
	segments := strings.Split(s, "\t")
	w := 1 // leading padding
	for i, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		parts := strings.SplitN(seg, ":", 2)
		txt := seg
		if len(parts) == 2 {
			txt = strings.TrimSpace(parts[1])
		}
		if i > 0 {
			w += 3 // " │ " divider
		}
		w += utf8.RuneCountInString(txt)
		if len(parts) == 2 {
			code := strings.TrimSpace(parts[0])
			switch code {
			case "P", "Pr", "Py":
				w += 2 // the pill's padding spaces
			case "*":
				w += 2 // renderStatusBar writes "* " in front of the label
			}
		}
	}
	return w
}

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	scrollbackLines = 1000
	maxParams       = 16
	maxOSC          = 256
	commandKey      = 'g' // Ctrl-G prefix
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

// ── Debug logging ─────────────────────────────────────────────────────────────
var dbgFile *os.File

// ── Selection color config ────────────────────────────────────────────────────
// Override with MAGMUX_SEL_FG / MAGMUX_SEL_BG env vars (256-color index)
var (
	selFg = 0   // black text
	selBg = 220 // yellow background (256-color)
)

// ── Cell & Attributes ─────────────────────────────────────────────────────────

type Attr uint16

const (
	AttrBold      Attr = 1 << 0
	AttrDim       Attr = 1 << 1
	AttrItalic    Attr = 1 << 2
	AttrBlink     Attr = 1 << 3
	AttrReverse   Attr = 1 << 4
	AttrInvis     Attr = 1 << 5
	AttrUnderline Attr = 1 << 6
	AttrStrike    Attr = 1 << 7
	AttrOverline  Attr = 1 << 8
)

// Color represents a terminal color: default (-1), 256-color (0-255), or truecolor
type Color struct {
	Index   int16 // -1=default, 0-255=indexed
	R, G, B uint8
	True    bool // if true, use R/G/B instead of Index
}

var defaultColor = Color{Index: -1}

type Cell struct {
	Ch   rune
	Fg   Color
	Bg   Color
	Attr Attr
	Wide bool // is this cell the left half of a wide char?
	Cont bool // is this a continuation (right half) of a wide char?
}

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

// ── VT Parser ─────────────────────────────────────────────────────────────────
// Port of vtparser.c — DEC ANSI parser state machine (Paul Flo Williams)

type vtState int

const (
	stGround vtState = iota
	stEscape
	stEscapeIntermediate
	stCSIEntry
	stCSIParam
	stCSIIntermediate
	stCSIIgnore
	stOSCString
)

type VTParser struct {
	state    vtState
	inter    rune
	narg     int
	args     [maxParams]int
	nosc     int
	oscbuf   [maxOSC]rune
	oscTerm  rune    // how the OSC being dispatched ended: BEL (0x07) or ST (0x1b)
	node     *Pane   // back-reference to pane
	partial  [4]byte // buffered incomplete UTF-8 bytes from previous read
	npartial int     // number of valid bytes in partial
}

func (vt *VTParser) reset() {
	vt.inter = 0
	vt.narg = 0
	vt.nosc = 0
	for i := range vt.args {
		vt.args[i] = 0
	}
}

func (vt *VTParser) param(w rune) {
	if vt.narg == 0 {
		vt.narg = 1
	}
	if w == ';' {
		if vt.narg < maxParams {
			vt.narg++
		}
	} else if vt.narg <= maxParams {
		idx := vt.narg - 1
		if vt.args[idx] < 9999 {
			vt.args[idx] = vt.args[idx]*10 + int(w-'0')
		}
	}
}

func (vt *VTParser) write(data []byte) {
	// Prepend any incomplete UTF-8 bytes buffered from the previous call.
	if vt.npartial > 0 {
		combined := make([]byte, vt.npartial+len(data))
		copy(combined, vt.partial[:vt.npartial])
		copy(combined[vt.npartial:], data)
		data = combined
		vt.npartial = 0
	}

	for len(data) > 0 {
		// If the remaining bytes might contain an incomplete trailing UTF-8
		// sequence, stash those bytes and stop. This prevents splitting a
		// multi-byte rune across two reads from being misdecoded as '?'.
		if len(data) < 4 && !utf8.FullRune(data) {
			vt.npartial = copy(vt.partial[:], data)
			return
		}

		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size <= 1 {
			r = '?'
			size = 1
		}
		data = data[size:]
		vt.handleChar(r)
	}
}

func (vt *VTParser) handleChar(w rune) {
	p := vt.node
	s := p.screen

	// C0 controls that apply in ALL states
	switch {
	case w == 0x1b: // ESC
		if vt.state == stOSCString {
			// ESC in OSC string — next char should be '\' (ST)
			// Terminate the OSC now (the '\' will be consumed by stEscape)
			vt.oscTerm = 0x1b
			vt.handleOSC()
			vt.state = stEscape
			return
		}
		vt.state = stEscape
		vt.reset()
		return
	case w == 0x18 || w == 0x1a: // CAN, SUB
		vt.state = stGround
		return
	}

	switch vt.state {
	case stGround:
		switch {
		case w < 0x20: // C0 control
			vt.doControl(w)
		default: // Printable
			vt.doPrint(w)
		}

	case stEscape:
		switch {
		case w >= 0x20 && w <= 0x2f:
			vt.inter = w
			vt.state = stEscapeIntermediate
		case w == '[':
			vt.state = stCSIEntry
			vt.reset()
		case w == ']' || w == 'P' || w == '_' || w == '^':
			vt.state = stOSCString
			vt.reset()
		case w == '!': // workaround: ESC ! p = soft reset (DECSTR-ish)
			vt.state = stOSCString
		default:
			vt.doEscape(w)
			vt.state = stGround
		}

	case stEscapeIntermediate:
		switch {
		case w >= 0x20 && w <= 0x2f:
			vt.inter = w
		case w >= 0x30 && w <= 0x7e:
			vt.doEscape(w)
			vt.state = stGround
		}

	case stCSIEntry:
		switch {
		case w >= '0' && w <= '9':
			vt.param(w)
			vt.state = stCSIParam
		case w == ';':
			vt.param(w)
			vt.state = stCSIParam
		case w == ':':
			vt.state = stCSIIgnore
		case w >= 0x20 && w <= 0x2f:
			vt.inter = w
			vt.state = stCSIIntermediate
		case w >= '<' && w <= '?':
			vt.inter = w
			vt.state = stCSIParam
		case w >= 0x40 && w <= 0x7e:
			vt.doCSI(w)
			vt.state = stGround
		}

	case stCSIParam:
		switch {
		case w >= '0' && w <= '9':
			vt.param(w)
		case w == ';':
			vt.param(w)
		case w == ':':
			vt.state = stCSIIgnore
		case w >= '<' && w <= '?':
			vt.state = stCSIIgnore
		case w >= 0x20 && w <= 0x2f:
			vt.inter = w
			vt.state = stCSIIntermediate
		case w >= 0x40 && w <= 0x7e:
			vt.doCSI(w)
			vt.state = stGround
		}

	case stCSIIntermediate:
		switch {
		case w >= 0x20 && w <= 0x2f:
			vt.inter = w
		case w >= 0x30 && w <= 0x3f:
			vt.state = stCSIIgnore
		case w >= 0x40 && w <= 0x7e:
			vt.doCSI(w)
			vt.state = stGround
		}

	case stCSIIgnore:
		if w >= 0x40 && w <= 0x7e {
			vt.state = stGround
		}

	case stOSCString:
		if w == 0x07 || w == '\\' { // BEL or ST
			// Which one matters: a reply must end the way the query did, or the
			// leftover terminator lands in the child's input as garbage.
			vt.oscTerm = w
			vt.handleOSC()
			vt.state = stGround
		} else if w >= 0x20 && vt.nosc < maxOSC {
			vt.oscbuf[vt.nosc] = w
			vt.nosc++
		}
	}
	_ = s // suppress unused
}

// P1 returns param i with default 1
func (vt *VTParser) p1(i int) int {
	if i >= vt.narg || vt.args[i] == 0 {
		return 1
	}
	return vt.args[i]
}

// P0 returns param i with default 0
func (vt *VTParser) p0(i int) int {
	if i >= vt.narg {
		return 0
	}
	return vt.args[i]
}

// isNotificationOSC9 returns true if an OSC 9 body looks like an iTerm2-style
// textual notification (e.g. "9;Claude is waiting for input") and NOT one of
// the common collision cases:
//   - OSC 9;4;... — ConEmu progress bar protocol (`9;4;<state>;<value>`)
//   - OSC 9;<digits>;<digits> — numeric-only bodies used by various extensions
func isNotificationOSC9(osc string) bool {
	// body is everything after "9;"
	if !strings.HasPrefix(osc, "9;") {
		return false
	}
	body := osc[2:]
	if body == "" {
		return false
	}
	// ConEmu progress: 9;4;<state>;<value>
	if strings.HasPrefix(body, "4;") {
		return false
	}
	// Numeric-only body (e.g. "9;0", "9;1;2"): not a text notification.
	allDigitsOrSemicolon := true
	for _, r := range body {
		if (r < '0' || r > '9') && r != ';' {
			allDigitsOrSemicolon = false
			break
		}
	}
	if allDigitsOrSemicolon {
		return false
	}
	return true
}

// colorQueryCode returns the OSC code of a colour QUERY — "10" (foreground),
// "11" (background) or "12" (cursor) followed by a bare "?" — and whether the
// body was one at all.
//
// The "?" is the whole distinction and it is not cosmetic: `OSC 11;#ff0000` is
// a child SETTING the background, and answering that would put bytes into the
// input of a program that is not reading any. Only a question gets an answer.
func colorQueryCode(osc string) (string, bool) {
	code, arg, ok := strings.Cut(osc, ";")
	if !ok || strings.TrimSpace(arg) != "?" {
		return "", false
	}
	switch code {
	case "10", "11", "12":
		return code, true
	}
	return "", false
}

// answerColorQuery tells the child what colour the terminal is.
//
// magmux IS the terminal to its children, and this is the one question it used
// to leave hanging: theme-aware TUIs (Claude Code among them) query OSC 11 at
// startup, block on the reply, and draw nothing at all until it arrives — which
// is why Claude Code rendered as a blank pane in magmux and rendered fine in
// tmux. The colours come from theme.go: the terminal's real background when the
// startup probe read one, the active palette's assumed background otherwise. A
// plausible answer that matches magmux's own chrome is a small inaccuracy; no
// answer is a blank screen.
//
// The reply goes out through writePTY on the parser's own goroutine, exactly as
// the DA and DSR replies below do, and under the same lock they hold.
func (vt *VTParser) answerColorQuery(code string) {
	c, ok := terminalColor(code)
	if !ok {
		return
	}
	// Echo the requester's terminator. xterm answers BEL to a BEL-terminated
	// query and ST to an ST-terminated one; getting it wrong leaves a stray
	// BEL or a stray ESC \ in the application's input stream.
	end := "\x1b\\"
	if vt.oscTerm == 0x07 {
		end = "\x07"
	}
	resp := "\x1b]" + code + ";" + xColorString(c) + end
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[OSC] colour query %s → %q\n", code, resp)
	}
	vt.node.writePTY([]byte(resp))
}

// handleOSC processes completed OSC sequences.
// Detects notification sequences that signal "waiting for input":
//   - OSC 9;...  — iTerm2-style notification
//   - OSC 777;notify;... — rxvt-style notification
//   - OSC 633;B  — VS Code shell integration "prompt started"
//
// And answers the colour queries a child blocks on:
//   - OSC 10;? / 11;? / 12;? — foreground / background / cursor colour
func (vt *VTParser) handleOSC() {
	if vt.nosc == 0 {
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[OSC:end] empty OSC\n")
		}
		return
	}
	osc := string(vt.oscbuf[:vt.nosc])
	p := vt.node

	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[OSC:end] %q\n", osc)
	}

	if code, ok := colorQueryCode(osc); ok {
		vt.answerColorQuery(code)
		return
	}

	switch {
	// OSC 9 with a textual notification body (iTerm2-style "growl" notification).
	// Excludes OSC 9;4;... (ConEmu progress bar protocol) and OSC 9 with numeric-
	// only prefixes (various terminal extensions) because those aren't "waiting
	// for input" signals.
	case strings.HasPrefix(osc, "9;") && isNotificationOSC9(osc),
		strings.HasPrefix(osc, "777;notify;"),
		osc == "633;B":
		// Gate to TUI-ish panes so plain shell commands that happen to emit
		// OSC 9 notifications (e.g. a build script's completion bell) don't
		// trigger false positives.
		if p.altMode || p.controller != nil {
			p.inputReady = true
			p.inputSignal = "osc"
			p.inputReadyAt = time.Now()
			if dbgFile != nil {
				fmt.Fprintf(dbgFile, "[OSC] notification: %q → inputReady=true\n", osc)
			}
		} else if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[OSC] notification ignored (no TUI/controller): %q\n", osc)
		}

	case strings.HasPrefix(osc, "0;"):
		// Window title set. Claude Code uses title to signal state:
		//   "✳ ..." = idle/ready for input
		//   "⠂ ..." / "⠐ ..." = working (spinner)
		// Title can briefly flash ✳ during transitions (e.g. between model
		// response and stop hooks), so we debounce: record the time the title
		// became idle, and the render loop fires inputReady only after the
		// title has been stably idle for >2s without any spinner reappearing.
		title := osc[2:]
		if strings.HasPrefix(title, "\u2733") { // ✳ = idle
			if p.titleWasWorking && p.titleIdleAt.IsZero() {
				p.titleIdleAt = time.Now()
				if dbgFile != nil {
					fmt.Fprintf(dbgFile, "[OSC] title idle started: %q\n", title)
				}
			}
		} else {
			// Non-idle title — mark as working. Reset idle timer.
			p.titleWasWorking = true
			p.titleIdleAt = time.Time{}
			if p.inputReady && p.inputSignal == "title" {
				p.inputReady = false
				if dbgFile != nil {
					fmt.Fprintf(dbgFile, "[OSC] title working: %q → inputReady=false\n", title)
				}
			}
		}
	}
}

func (vt *VTParser) doControl(w rune) {
	p := vt.node
	s := p.screen
	switch w {
	case 0x07: // BEL - ignore
	case 0x08: // BS - cursor back
		if s.curX > 0 {
			s.curX--
		}
		s.xenl = false
	case 0x09: // HT - horizontal tab
		s.curX = min(((s.curX/8)+1)*8, s.cols-1)
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		vt.index()
	case 0x0d: // CR
		s.curX = 0
		s.xenl = false
	case 0x0e: // SO — shift out (activate G1 charset)
		p.useG1 = true
	case 0x0f: // SI — shift in (activate G0 charset)
		p.useG1 = false
	}
}

func (vt *VTParser) index() {
	s := vt.node.screen
	if s.curY == s.scrollBot-1 {
		s.scrollUp(s.scrollTop, s.scrollBot)
	} else if s.curY < s.rows-1 {
		s.curY++
	}
}

func (vt *VTParser) reverseIndex() {
	s := vt.node.screen
	if s.curY == s.scrollTop {
		s.scrollDown(s.scrollTop, s.scrollBot)
	} else if s.curY > 0 {
		s.curY--
	}
}

func (vt *VTParser) doEscape(w rune) {
	s := vt.node.screen
	switch w {
	case 'c': // RIS - full reset
		s.fg = defaultColor
		s.bg = defaultColor
		s.attr = 0
		s.curX = 0
		s.curY = 0
		s.scrollTop = 0
		s.scrollBot = s.rows
		s.originMode = false
		s.autoWrap = true
	case 'D': // IND - index
		vt.index()
	case 'M': // RI - reverse index
		vt.reverseIndex()
	case 'E': // NEL - next line
		s.curX = 0
		vt.index()
	case '7': // DECSC - save cursor
		s.savedY = s.curY
		s.savedX = s.curX
		s.savedFg = s.fg
		s.savedBg = s.bg
		s.savedAttr = s.attr
	case '8': // DECRC - restore cursor
		s.curY = s.savedY
		s.curX = s.savedX
		s.fg = s.savedFg
		s.bg = s.savedBg
		s.attr = s.savedAttr
	case '=', '>': // DECKPAM/DECKPNM - keypad modes (ignore)
	case 'H': // HTS - set horizontal tab stop at current column
		// Tab stop management would go here — ignore for now
	case '\\': // ST - string terminator (handled in state machine)
	}

	// Character set designation: ESC ( X or ESC ) X
	if vt.inter == '(' {
		switch w {
		case '0':
			vt.node.charsetG0 = '0' // line drawing
		case 'B':
			vt.node.charsetG0 = 'B' // ASCII
		}
		return
	}
	if vt.inter == ')' {
		switch w {
		case '0':
			vt.node.charsetG1 = '0'
		case 'B':
			vt.node.charsetG1 = 'B'
		}
		return
	}
}

// setAltScreen moves a pane on or off the alternate screen. It is the single
// implementation behind DEC 47, 1047 and 1049, which differ only in what else
// they save and had drifted into three copies of the same six lines.
//
// Everything here is anchored on node.primaryScreen rather than on the CURRENT
// screen, and that is the fix as much as the deduplication. doCSI resolves
// `s := vt.node.screen`, so on the way OUT `s` is the ALTERNATE screen — whose
// own .altScreen is nil — and the old `else if s.altScreen != nil` guard was
// therefore never true. A pane that entered the alt screen never came back: the
// shell's prompt after `:q` was painted into vim's buffer, on top of vim's last
// frame. Symmetrically, a second 1049h while already on the alt screen used to
// hang a THIRD screen off the alt one and switch to that.
//
// It was invisible for as long as magmux's main tenant was Claude Code, which
// enters the alt screen at startup and leaves it by exiting. It stops being
// invisible the moment a pane has scrollback worth returning to.
func (vt *VTParser) setAltScreen(on bool) {
	p := vt.node
	if p.primaryScreen == nil {
		p.primaryScreen = p.screen
	}
	prim := p.primaryScreen
	if on {
		if prim.altScreen == nil {
			prim.altScreen = newAltScreen(prim.rows, prim.cols)
		}
		p.screen = prim.altScreen
		p.altMode = true
		return
	}
	p.screen = prim
	p.altMode = false
}

func (vt *VTParser) doCSI(w rune) {
	s := vt.node.screen

	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "CSI inter=0x%02x narg=%d args=%v final='%c' curY=%d curX=%d\n", vt.inter, vt.narg, vt.args[:max(vt.narg, 1)], w, s.curY, s.curX)
		dbgFile.Sync()
	}

	// Private mode sequences (CSI ? ...)
	if vt.inter == '?' {
		set := w == 'h'
		if w == 'h' || w == 'l' {
			for i := 0; i < max(vt.narg, 1); i++ {
				switch vt.p0(i) {
				case 1: // DECCKM - cursor keys (ignore)
				case 6: // DECOM - origin mode
					s.originMode = set
					s.curY = 0
					s.curX = 0
				case 7: // DECAWM - auto-wrap
					s.autoWrap = set
				case 12: // Cursor blink (cosmetic, ignore)
				case 25: // DECTCEM - cursor visibility (ignore for now)
				case 47: // Alt screen (legacy)
					vt.setAltScreen(set)
				case 1000, 1002, 1003, 1006: // Mouse tracking — consumed by magmux
				case 1004: // Focus events
					vt.node.focusEvents = set
				case 1047: // Alt screen (variant 2)
					vt.setAltScreen(set)
				case 1049: // Alt screen buffer + cursor save
					vt.setAltScreen(set)
				case 2004: // Bracketed paste mode
					wasPaste := vt.node.bracketPaste
					vt.node.bracketPaste = set
					if dbgFile != nil {
						fmt.Fprintf(dbgFile, "[2004] set=%v wasPaste=%v pasteWasOff=%v textSincePasteOff=%v inputReady=%v\n",
							set, wasPaste, vt.node.pasteWasOff, vt.node.textSincePasteOff, vt.node.inputReady)
					}
					if !set {
						// Paste turning OFF — agent is working
						vt.node.pasteWasOff = true
						vt.node.textSincePasteOff = false
						vt.node.inputReady = false
					} else if set && !wasPaste && vt.node.pasteWasOff && vt.node.textSincePasteOff &&
						(vt.node.altMode || vt.node.controller != nil) {
						// Paste turning back ON after being off AND we saw real text
						// in between — TUI app done, waiting for input.
						//
						// Gated to alt-screen or controller-managed panes because plain
						// Node-based shell commands (e.g. `bun run`) also cycle paste
						// mode during startup/shutdown and would trigger false positives.
						vt.node.inputReady = true
						vt.node.inputSignal = "2004"
						vt.node.inputReadyAt = time.Now()
					}
					if dbgFile != nil {
						fmt.Fprintf(dbgFile, "[2004] → inputReady=%v pasteWasOff=%v\n",
							vt.node.inputReady, vt.node.pasteWasOff)
					}
				}
			}
		}
		return
	}

	// CSI > ... sequences (modifier key modes, etc.) — handle known ones, ignore rest
	if vt.inter == '>' {
		switch w {
		case 'c': // DA2
			vt.node.writePTY([]byte("\x1b[>1;10;0c"))
		case 'm', 'n': // MODSET/MODOFF — xterm key modification modes, ignore
		case 'u': // Push keyboard enhancement (Kitty protocol), ignore
		case 'q': // xterm query, ignore
		}
		return
	}

	// CSI < ... sequences (Kitty keyboard protocol pop, etc.)
	if vt.inter == '<' {
		// CSI < u = pop keyboard enhancement — ignore
		return
	}

	// CSI = ... sequences (Kitty keyboard protocol set, etc.)
	if vt.inter == '=' {
		return
	}

	// CSI SP ... sequences
	if vt.inter == ' ' {
		switch w {
		case 'q': // DECSCUSR - set cursor shape
			vt.node.cursorShape = vt.p0(0)
		}
		return
	}

	switch w {
	case 'A': // CUU - cursor up
		s.curY = max(s.scrollTop, s.curY-vt.p1(0))
		s.xenl = false
	case 'B': // CUD - cursor down
		s.curY = min(s.scrollBot-1, s.curY+vt.p1(0))
		s.xenl = false
	case 'C': // CUF - cursor forward
		s.curX = min(s.cols-1, s.curX+vt.p1(0))
		s.xenl = false
	case 'D': // CUB - cursor back
		s.curX = max(0, s.curX-vt.p1(0))
		s.xenl = false
	case 'E': // CNL - cursor next line
		s.curY = min(s.scrollBot-1, s.curY+vt.p1(0))
		s.curX = 0
		s.xenl = false
	case 'F': // CPL - cursor previous line
		s.curY = max(s.scrollTop, s.curY-vt.p1(0))
		s.curX = 0
		s.xenl = false
	case 'G': // CHA - cursor horizontal absolute
		s.curX = clamp(vt.p1(0)-1, 0, s.cols-1)
		s.xenl = false
	case 'H', 'f': // CUP - cursor position
		row := vt.p1(0) - 1
		col := vt.p1(1) - 1
		if s.originMode {
			row += s.scrollTop
		}
		s.curY = clamp(row, 0, s.rows-1)
		s.curX = clamp(col, 0, s.cols-1)
		s.xenl = false
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "CUP: row=%d col=%d (param %d;%d)\n", s.curY, s.curX, vt.p1(0), vt.p1(1))
		}
	case 'J': // ED - erase display
		switch vt.p0(0) {
		case 0: // from cursor to end
			s.clearLine(s.curY, s.curX, s.cols)
			for i := s.curY + 1; i < s.rows; i++ {
				s.clearLine(i, 0, s.cols)
			}
		case 1: // from start to cursor
			for i := 0; i < s.curY; i++ {
				s.clearLine(i, 0, s.cols)
			}
			s.clearLine(s.curY, 0, s.curX+1)
		case 2: // entire screen
			for i := 0; i < s.rows; i++ {
				s.clearLine(i, 0, s.cols)
			}
		}
	case 'K': // EL - erase line
		switch vt.p0(0) {
		case 0:
			s.clearLine(s.curY, s.curX, s.cols)
		case 1:
			s.clearLine(s.curY, 0, s.curX+1)
		case 2:
			s.clearLine(s.curY, 0, s.cols)
		}
	case 'L': // IL - insert lines
		n := vt.p1(0)
		for i := 0; i < n; i++ {
			s.scrollDown(s.curY, s.scrollBot)
		}
	case 'M': // DL - delete lines
		n := vt.p1(0)
		for i := 0; i < n; i++ {
			s.scrollUp(s.curY, s.scrollBot)
		}
	case 'P': // DCH - delete characters
		row := s.cells[s.curY]
		n := min(vt.p1(0), s.cols-s.curX)
		copy(row[s.curX:], row[s.curX+n:])
		for j := s.cols - n; j < s.cols; j++ {
			row[j] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
		}
	case '@': // ICH - insert characters
		row := s.cells[s.curY]
		n := min(vt.p1(0), s.cols-s.curX)
		copy(row[s.curX+n:], row[s.curX:s.cols-n])
		for j := s.curX; j < s.curX+n; j++ {
			row[j] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
		}
	case 'X': // ECH - erase characters
		n := min(vt.p1(0), s.cols-s.curX)
		for j := s.curX; j < s.curX+n; j++ {
			s.cells[s.curY][j] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
		}
	case 'd': // VPA - vertical position absolute
		s.curY = clamp(vt.p1(0)-1, 0, s.rows-1)
		s.xenl = false
	case 'r': // DECSTBM - set scrolling region
		// The two parameters do NOT share a default, and conflating them is what
		// made Claude Code render as a blank pane. An omitted top is line 1; an
		// omitted bottom is the LAST line of the page. Reading the bottom with p1
		// — which turns an absent parameter into 1 — made the bare `CSI r` that
		// opens Claude Code's output mean "the scrolling region is row 1 to row
		// 1". Every subsequent LF then scrolled that one-line region and blanked
		// it, so the application painted its entire UI onto row 0, one line at a
		// time, each line erasing the one before. The `bot == 0` guard below was
		// written to catch exactly this and could never fire, because p1 had
		// already rewritten the 0 as a 1.
		//
		// p0 keeps the absent/zero distinction intact, and 0 is also the explicit
		// "use the default" value DEC assigns, so both spellings land here.
		top := vt.p1(0)
		bot := vt.p1(1)
		if bot == 0 || bot > s.rows {
			bot = s.rows
		}
		// A scrolling region must be at least two lines: a one-line region has
		// nowhere to scroll to, so every newline inside it erases the screen —
		// the failure above, reached by a different route. An inverted or
		// degenerate region is ignored outright (as xterm ignores it), leaving
		// the previous region and the cursor untouched.
		if top >= bot {
			break
		}
		s.scrollTop = top - 1
		s.scrollBot = bot
		// DECSTBM homes the cursor, and only when it was accepted. In origin
		// mode home is the top of the new region, not the top of the page.
		s.curX = 0
		s.curY = 0
		if s.originMode {
			s.curY = s.scrollTop
		}
		s.xenl = false
	case 's': // SCP - save cursor position
		s.savedY = s.curY
		s.savedX = s.curX
	case 'u': // RCP - restore cursor position
		s.curY = s.savedY
		s.curX = s.savedX
	case 'S': // SU - scroll up
		n := vt.p1(0)
		for i := 0; i < n; i++ {
			s.scrollUp(s.scrollTop, s.scrollBot)
		}
	case 'T': // SD - scroll down
		n := vt.p1(0)
		for i := 0; i < n; i++ {
			s.scrollDown(s.scrollTop, s.scrollBot)
		}
	case 'm': // SGR - select graphic rendition
		vt.doSGR()
	case 'n': // DSR - device status report
		if vt.p0(0) == 6 { // CPR - cursor position report
			cur := vt.node.screen // re-read in case screen changed
			resp := fmt.Sprintf("\x1b[%d;%dR", cur.curY+1, cur.curX+1)
			// Debug log DSR
			if dbgFile != nil {
				fmt.Fprintf(dbgFile, "DSR: curY=%d curX=%d altMode=%v rows=%d cols=%d\n",
					cur.curY, cur.curX, vt.node.altMode, cur.rows, cur.cols)
			}
			vt.node.writePTY([]byte(resp))
		} else if vt.p0(0) == 5 { // Device status - report OK
			vt.node.writePTY([]byte("\x1b[0n"))
		}
	case 'Z': // CBT - cursor backward tabulation
		n := vt.p1(0)
		for i := 0; i < n; i++ {
			s.curX = max(0, ((s.curX-1)/8)*8)
		}
		s.xenl = false
	case '`': // HPA alt (same as CHA/G)
		s.curX = clamp(vt.p1(0)-1, 0, s.cols-1)
		s.xenl = false
	case 'b': // REP - repeat last printed character
		n := vt.p1(0)
		for i := 0; i < n; i++ {
			vt.doPrint(vt.node.lastChar)
		}
	case 'c': // DA - primary device attributes
		vt.node.writePTY([]byte("\x1b[?1;2c"))
	case 'g': // TBC - tab clear
		// Ignore for now (would need tab stop tracking)
	case 'h': // SM - set mode
		if vt.p0(0) == 4 {
			s.insert = true
		}
	case 'l': // RM - reset mode
		if vt.p0(0) == 4 {
			s.insert = false
		}
	case 't': // WINOPS - window operations
		switch vt.p0(0) {
		case 18: // Report terminal size in characters
			resp := fmt.Sprintf("\x1b[8;%d;%dt", vt.node.h, vt.node.w)
			vt.node.writePTY([]byte(resp))
		case 14: // Report window size in pixels (fake it)
			resp := fmt.Sprintf("\x1b[4;%d;%dt", vt.node.h*16, vt.node.w*8)
			vt.node.writePTY([]byte(resp))
		}
	}
}

func (vt *VTParser) doSGR() {
	s := vt.node.screen
	argc := max(vt.narg, 1)
	if vt.narg == 0 {
		s.attr = 0
		s.fg = defaultColor
		s.bg = defaultColor
		return
	}
	for i := 0; i < argc; i++ {
		p := vt.p0(i)
		switch {
		case p == 0:
			s.attr = 0
			s.fg = defaultColor
			s.bg = defaultColor
		case p == 1:
			s.attr |= AttrBold
		case p == 2:
			s.attr |= AttrDim
		case p == 3:
			s.attr |= AttrItalic
		case p == 4: // underline — suppress like MTM (renders as visible lines in multiplexer)
		case p == 5:
			s.attr |= AttrBlink
		case p == 7:
			s.attr |= AttrReverse
		case p == 8:
			s.attr |= AttrInvis
		case p == 22:
			s.attr &^= (AttrBold | AttrDim)
		case p == 23:
			s.attr &^= AttrItalic
		case p == 9:
			s.attr |= AttrStrike
		case p == 21: // double underline (treat as underline)
			s.attr |= AttrUnderline
		case p == 24:
			s.attr &^= AttrUnderline
		case p == 25:
			s.attr &^= AttrBlink
		case p == 27:
			s.attr &^= AttrReverse
		case p == 28:
			s.attr &^= AttrInvis
		case p == 29:
			s.attr &^= AttrStrike
		case p == 53:
			s.attr |= AttrOverline
		case p == 55:
			s.attr &^= AttrOverline
		case p >= 30 && p <= 37:
			s.fg = Color{Index: int16(p - 30)}
		case p == 38: // extended fg color
			if i+1 < argc {
				switch vt.p0(i + 1) {
				case 5: // 256-color: 38;5;N
					if i+2 < argc {
						s.fg = Color{Index: int16(vt.p0(i + 2))}
						i += 2
					}
				case 2: // truecolor: 38;2;R;G;B
					if i+4 < argc {
						s.fg = Color{True: true, R: uint8(vt.p0(i + 2)), G: uint8(vt.p0(i + 3)), B: uint8(vt.p0(i + 4))}
						i += 4
					}
				}
			}
		case p == 39:
			s.fg = defaultColor
		case p >= 40 && p <= 47:
			s.bg = Color{Index: int16(p - 40)}
		case p == 48: // extended bg color
			if i+1 < argc {
				switch vt.p0(i + 1) {
				case 5: // 256-color: 48;5;N
					if i+2 < argc {
						s.bg = Color{Index: int16(vt.p0(i + 2))}
						i += 2
					}
				case 2: // truecolor: 48;2;R;G;B
					if i+4 < argc {
						s.bg = Color{True: true, R: uint8(vt.p0(i + 2)), G: uint8(vt.p0(i + 3)), B: uint8(vt.p0(i + 4))}
						i += 4
					}
				}
			}
		case p == 49:
			s.bg = defaultColor
		case p >= 90 && p <= 97:
			s.fg = Color{Index: int16(p - 90 + 8)}
		case p >= 100 && p <= 107:
			s.bg = Color{Index: int16(p - 100 + 8)}
		}
	}
}

// lineDrawingMap maps ASCII 0x60-0x7e to Unicode box-drawing when G0='0'
var lineDrawingMap = map[rune]rune{
	'j': '┘', 'k': '┐', 'l': '┌', 'm': '└', 'n': '┼',
	'q': '─', 't': '├', 'u': '┤', 'v': '┴', 'w': '┬',
	'x': '│', 'a': '▒', 'f': '°', 'g': '±', 'h': '░',
	'o': '⎺', 'p': '⎻', 'r': '⎼', 's': '⎽', '0': '◆',
	'`': '◆', '+': '→', ',': '←', '-': '↑', '.': '↓',
	'~': '·', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠',
	'}': '£', 'i': '⎽', 'e': ' ',
}

func (vt *VTParser) doPrint(w rune) {
	p := vt.node
	s := p.screen

	// Apply charset translation (line drawing)
	cs := p.charsetG0
	if p.useG1 {
		cs = p.charsetG1
	}
	if cs == '0' {
		if mapped, ok := lineDrawingMap[w]; ok {
			w = mapped
		}
	}
	p.lastChar = w
	p.lastTextAt = time.Now()
	p.hadTextOutput = true
	p.textSincePasteOff = true

	cw := runeWidth(w)
	if cw <= 0 {
		return
	}

	if s.insert {
		// Shift right
		row := s.cells[s.curY]
		copy(row[s.curX+cw:], row[s.curX:s.cols-cw])
	}

	if s.xenl {
		s.xenl = false
		if s.autoWrap {
			s.curX = 0
			vt.index()
		}
	}

	if s.curX+cw > s.cols {
		// Would go past edge
		if s.autoWrap {
			s.curX = 0
			vt.index()
		} else {
			return
		}
	}

	// Guard against zero-sized screens (e.g. when the controlling terminal
	// reports 0x0 rows/cols during startup or under `script`).
	if s.rows <= 0 || s.cols <= 0 || s.curY >= len(s.cells) || s.curY < 0 {
		return
	}
	if s.curX < 0 || s.curX >= len(s.cells[s.curY]) {
		return
	}

	s.cells[s.curY][s.curX] = Cell{
		Ch:   w,
		Fg:   s.fg,
		Bg:   s.bg,
		Attr: s.attr,
		Wide: cw > 1,
	}
	if cw > 1 && s.curX+1 < s.cols {
		s.cells[s.curY][s.curX+1] = Cell{
			Ch:   ' ',
			Fg:   s.fg,
			Bg:   s.bg,
			Attr: s.attr,
			Cont: true,
		}
	}

	if s.curX+cw >= s.cols {
		s.xenl = true
	} else {
		s.curX += cw
	}
}

// ── Pane (NODE equivalent) ────────────────────────────────────────────────────

type SplitType int

const (
	SplitNone       SplitType = iota // leaf VIEW
	SplitHorizontal                  // left | right
	SplitVertical                    // top / bottom
)

type Pane struct {
	// id is this pane's permanent index into Magmux.allPanes. It is stamped
	// once, before the pane is published, and never changes — close_pane
	// tombstones the slot rather than compacting the slice, because the socket
	// protocol's only addressing mode is an integer and a renumbering would
	// silently redirect a `send` into a different session. Immutable after
	// publication, so it may be read without treeMu.
	id int
	// closed marks a pane detached from the layout by close_pane. Its allPanes
	// slot is retained forever so later ids never shift. Guarded by treeMu:
	// every "for every pane" loop must skip it, or tint/overlay/keystrokes
	// land in a pane nobody can see.
	closed bool
	// label is the short name open_pane was given, for clients that address
	// panes by name. Immutable after publication.
	label string
	// Structural fields — guarded by treeMu, NOT by mu. Everything below
	// screen is content state and stays under mu exactly as before.
	splitType     SplitType
	y, x, h, w    int // position and size in host terminal
	ratio         float64
	child1        *Pane
	child2        *Pane
	parent        *Pane
	screen        *Screen
	primaryScreen *Screen
	vt            VTParser
	ptmx          *os.File // master side of PTY
	cmd           *exec.Cmd
	mu            sync.Mutex
	dead          bool
	dirty         bool // content changed since last render
	altMode       bool // child is in alternate screen (vim, htop, etc.)
	bracketPaste  bool // child requested bracketed paste mode (2004)
	focusEvents   bool // child requested focus events (1004)
	cursorShape   int  // DECSCUSR: 0=default, 1=block blink, 2=block, 3=underline blink, 4=underline, 5=bar blink, 6=bar
	charsetG0     byte // 0='B' (ASCII), '0' (line drawing)
	charsetG1     byte
	useG1         bool // SO (shift out) active — use G1 instead of G0
	lastChar      rune // last printed character (for REP command)
	// Grid mode fields
	gridMode bool // pane is in grid mode (don't delete on exit)
	// reaped is set by waitForChild once cmd.Wait has returned, i.e. once the
	// child's pid has been collected and is free for the OS to reuse. The
	// force-kill path in reapPane checks it before signalling the process
	// GROUP, so a delayed SIGKILL can never land on a stranger. Guarded by mu.
	reaped       bool
	exitCode     int       // exit code of child process
	startedAt    time.Time // when the child process was started (for exec duration)
	tint         string    // "green", "red", "" — border/indicator color
	overlayText  string    // centered overlay text, may contain \n for multi-line (e.g. "✓ DONE")
	overlayStyle string    // "success", "error", "info"
	// Idle/completion detection
	inputReady  bool   // TUI app is waiting for user input
	inputSignal string // what triggered inputReady: "osc", "2004", "title", "idle", "ctrl", "perm"
	// inputReadyAt is when inputReady was last set true. Controllers order it
	// against their own transcript progress to decide which signal is fresher
	// (see ClaudeCodeController.applyTerminalIdle). Every path that sets
	// inputReady true must set this too.
	inputReadyAt      time.Time
	pasteWasOff       bool      // bracketed paste was disabled at least once (filters initial setup)
	textSincePasteOff bool      // printable text was written after the last paste-off (filters startup 2004 cycle)
	lastTextAt        time.Time // last time a printable character was output (for idle detection)
	hadTextOutput     bool      // true once any text has been printed (filters initial empty state)
	titleWasWorking   bool      // title showed a non-idle indicator at least once (filters startup ✳)
	titleIdleAt       time.Time // time the window title became idle (✳); zero if currently working
	// Agent status (set via IPC "agent" messages from coding tool hooks)
	agentStatus  string // "", "working", "idle", "waiting_input", "waiting_permission", "compacting"
	agentProject string // project name from hook
	agentTool    string // last tool being used
	agentPrompt  string // last user prompt
	// isControl marks the pane as magmux's own control panel: no PTY, no
	// child process, painted by ControlPanel.render. Every path that assumes
	// a pane owns a process (read loops, child waits, done-counting) must
	// skip it.
	isControl bool
	// hidden is the THIRD pane state, and it is neither `dead` nor `closed`:
	// the pane is alive and keeps every byte of its history, it is simply not
	// spliced into the layout tree and therefore occupies no columns. Only the
	// control panel is ever hidden (Ctrl-G p), and it starts that way unless -c
	// asked for it.
	//
	// A hidden pane is still in m.allPanes, still holds its id, and is still
	// reported by buildPaneResults — so `results` keeps saying state:"panel"
	// whether the panel is on screen or not. What it must be excluded from is
	// anything that reads GEOMETRY or paints: its y/x/h/w are whatever they
	// were when it was taken out, so largestLiveLeafLocked would happily pick
	// it as a split target and the dirty sweep would repaint a pane that is not
	// on screen. Guarded by treeMu — it is structural state, like `parent`.
	hidden bool
	// Interactive tool controller (e.g. ClaudeCodeController). Optional.
	controller     ToolController
	controllerSnap Snapshot
	// Back-reference to the owning Magmux. Set by attachControllers so
	// controllers can coordinate (e.g. claim shared resources).
	mux *Magmux
}

func newPane(y, x, h, w int, cfg PaneConfig) (*Pane, error) {
	p := &Pane{
		y:         y,
		x:         x,
		h:         h,
		w:         w,
		ratio:     0.5,
		charsetG0: 'B', // ASCII
		charsetG1: 'B',
		label:     cfg.Label,
	}
	p.screen = newScreen(h, w)
	p.primaryScreen = p.screen
	p.vt.node = p

	if err := p.spawnPTY(cfg); err != nil {
		return nil, fmt.Errorf("spawn PTY: %w", err)
	}
	return p, nil
}

// newControlPane builds a pane with no PTY and no child. It still gets a
// Screen and a VT parser, because that is how it is painted: ControlPanel
// writes ANSI into the parser exactly as a child process would, so the pane
// renders, scrolls, and selects through the ordinary pane path.
func newControlPane(y, x, h, w int, label string) *Pane {
	p := &Pane{
		y: y, x: x, h: h, w: w,
		ratio:     0.5,
		charsetG0: 'B',
		charsetG1: 'B',
		isControl: true,
		label:     label,
	}
	p.screen = newScreen(h, w)
	p.primaryScreen = p.screen
	p.vt.node = p
	return p
}

// newPaneFor builds either a normal child-process pane or the control pane,
// depending on the config. It is the single pane constructor: every layout
// builder and OpenPane goes through it, so a field added to PaneConfig cannot
// be honoured on one path and silently dropped on another.
func newPaneFor(y, x, h, w int, cfg PaneConfig) (*Pane, error) {
	if cfg.Control {
		return newControlPane(y, x, h, w, cfg.Label), nil
	}
	return newPane(y, x, h, w, cfg)
}

func (p *Pane) spawnPTY(cfg PaneConfig) error {
	ptmx, pts, err := openPTY()
	if err != nil {
		return err
	}

	// Set initial size
	setWinSize(ptmx, p.h, p.w)

	cmd := exec.Command(cfg.Cmd, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}
	env := append(os.Environ(),
		"TERM=screen-256color",
		fmt.Sprintf("COLUMNS=%d", p.w),
		fmt.Sprintf("LINES=%d", p.h),
	)
	// Export socket path so children can discover it
	if sockPath := os.Getenv("MAGMUX_SOCK"); sockPath != "" {
		env = append(env, "MAGMUX_SOCK="+sockPath)
	}
	// Caller-supplied entries go last so they win over everything above,
	// including MAGMUX_SOCK — a pane deliberately pointed at another magmux is
	// a legitimate thing to ask for.
	env = append(env, cfg.Env...)
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		pts.Close()
		ptmx.Close()
		return err
	}
	pts.Close() // parent doesn't need slave side

	p.ptmx = ptmx
	p.cmd = cmd
	p.startedAt = time.Now()
	return nil
}

// lastNonEmptyLine returns the most recent meaningful visible line from the screen,
// trimmed and truncated to maxLen runes. Skips Claude Code status lines (anything
// containing "tokens" or starting with "* Opus"/"* Sonnet"/etc.) and the bare ❯
// prompt, preferring lines that look like response content. Caller must hold p.mu.
func (p *Pane) lastNonEmptyLine(maxLen int) string {
	if p.screen == nil {
		return ""
	}
	s := p.screen

	// Guard against uninitialized or zero-sized screens that can arise when
	// the controlling terminal reports 0x0 dimensions during startup.
	if s.rows <= 0 || s.cols <= 0 || len(s.cells) == 0 {
		return ""
	}

	// Read all rows into a slice of trimmed strings
	lines := make([]string, s.rows)
	for row := 0; row < s.rows && row < len(s.cells); row++ {
		var sb strings.Builder
		rowCells := s.cells[row]
		for c := 0; c < s.cols && c < len(rowCells); c++ {
			ch := rowCells[c].Ch
			if ch == 0 {
				sb.WriteByte(' ')
			} else {
				sb.WriteRune(ch)
			}
		}
		lines[row] = strings.TrimSpace(sb.String())
	}

	// Heuristic: skip lines that look like status/UI chrome
	isChrome := func(l string) bool {
		if l == "" {
			return true
		}
		// Bare or near-bare prompt lines
		if l == "\u276f" || strings.HasPrefix(l, "\u276f ") {
			return true
		}
		// Horizontal rules (mostly box drawing chars)
		nonRule := 0
		for _, r := range l {
			if r != '\u2500' && r != '\u2501' && r != '\u2014' && r != '-' && r != ' ' {
				nonRule++
			}
		}
		if nonRule == 0 {
			return true
		}
		// Claude Code status bar markers
		if strings.Contains(l, "tokens") {
			return true
		}
		if strings.Contains(l, "/effort") {
			return true
		}
		// Common status bar pattern: starts with "*" then model name
		if strings.HasPrefix(l, "* ") || strings.HasPrefix(l, "*  ") {
			return true
		}
		return false
	}

	// Scan from bottom to top, return first non-chrome line
	for row := s.rows - 1; row >= 0; row-- {
		l := lines[row]
		if isChrome(l) {
			continue
		}
		// Strip leading response bullet (⏺) for cleaner display
		l = strings.TrimPrefix(l, "\u23fa ")
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if utf8.RuneCountInString(l) > maxLen {
			runes := []rune(l)
			l = string(runes[:maxLen-1]) + "\u2026"
		}
		return l
	}
	return ""
}

// formatDuration renders a time.Duration compactly (e.g. "1.2s", "4m 12s", "1h 3m").
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

func (p *Pane) writePTY(data []byte) {
	if p.ptmx == nil {
		return
	}
	// In grid mode, don't forward input to dead or completed panes
	if p.gridMode && (p.dead || p.inputReady) {
		return
	}
	// User input resets idle state — pane is no longer "done"
	if p.inputReady {
		p.inputReady = false
		p.tint = ""
		p.overlayText = ""
		p.overlayStyle = ""
		p.hadTextOutput = false // require new output before re-detecting idle
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[input] user keystroke → inputReady reset\n")
		}
	}
	p.ptmx.Write(data)
}

func (p *Pane) readLoop(wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 8192)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			p.mu.Lock()
			p.vt.write(buf[:n])
			p.dirty = true
			p.mu.Unlock()
		}
		if err != nil {
			p.mu.Lock()
			p.dead = true
			p.mu.Unlock()
			return
		}
	}
}

func (p *Pane) resize(y, x, h, w int) {
	p.y = y
	p.x = x
	p.h = h
	p.w = w
	if p.splitType == SplitNone {
		p.mu.Lock()
		// Both screens, reached through the PRIMARY. `p.screen` is whichever one
		// the child is currently on, and its .altScreen is nil when that is the
		// alt screen — so resizing through it left the primary at the old size
		// for the whole life of a full-screen app, and the shell came back to a
		// screen the wrong shape. Same root cause as setAltScreen's.
		prim := p.primaryScreen
		if prim == nil {
			prim = p.screen
		}
		if prim != nil {
			prim.resize(h, w)
			if prim.altScreen != nil {
				prim.altScreen.resize(h, w)
			}
		}
		p.mu.Unlock()
		if p.ptmx != nil {
			setWinSize(p.ptmx, h, w)
		}
	} else {
		p.reshapeChildren()
	}
}

// reshapeChildren reflows a split node's two children.
//
// The clamps are not defensive noise. w2 = p.w - w1 - 1 has no natural floor:
// three splits deep on an 80-column terminal, or one SIGWINCH shrinking a tree
// that was legal when it was built, yields zero or NEGATIVE dimensions. The
// zero-size guards downstream handle zero; negative reaches Screen.resize with
// a negative row count and every geometry assumption below it. Clamping here
// rather than in OpenPane is deliberate — OpenPane only covers the moment of
// creation, and the terminal can shrink at any time afterwards.
//
// Caller holds treeMu.Lock (geometry is structural state).
func (p *Pane) reshapeChildren() {
	if p.splitType == SplitHorizontal {
		w1 := clamp(int(float64(p.w)*p.ratio), 0, maxInt(0, p.w-1))
		w2 := maxInt(0, p.w-w1-1) // -1 for border
		h := maxInt(0, p.h)
		p.child1.resize(p.y, p.x, h, w1)
		p.child2.resize(p.y, p.x+w1+1, h, w2)
	} else if p.splitType == SplitVertical {
		h1 := clamp(int(float64(p.h)*p.ratio), 0, maxInt(0, p.h-1))
		h2 := maxInt(0, p.h-h1-1) // -1 for border
		w := maxInt(0, p.w)
		p.child1.resize(p.y, p.x, h1, w)
		p.child2.resize(p.y+h1+1, p.x, h2, w)
	}
}

// ── PTY helpers — see pty_darwin.go / pty_linux.go ────────────────────────────

func setWinSize(f *os.File, rows, cols int) {
	unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}

// ── ANSI Renderer ─────────────────────────────────────────────────────────────

type Renderer struct {
	buf      strings.Builder
	prevFg   Color
	prevBg   Color
	prevAttr Attr
}

func (r *Renderer) reset() {
	r.buf.Reset()
	r.prevFg = Color{Index: -2} // force first setAttr to emit
	r.prevBg = Color{Index: -2}
	r.prevAttr = 0
}

func (r *Renderer) hideCursor() {
	r.buf.WriteString("\x1b[?25l")
}

func (r *Renderer) showCursor(row, col int) {
	fmt.Fprintf(&r.buf, "\x1b[%d;%dH\x1b[?25h", row+1, col+1)
}

func (r *Renderer) moveTo(row, col int) {
	fmt.Fprintf(&r.buf, "\x1b[%d;%dH", row+1, col+1)
}

func colorEqual(a, b Color) bool {
	if a.True != b.True {
		return false
	}
	if a.True {
		return a.R == b.R && a.G == b.G && a.B == b.B
	}
	return a.Index == b.Index
}

func (r *Renderer) writeColor(c Color, isBg bool) {
	if c.True {
		if isBg {
			fmt.Fprintf(&r.buf, ";48;2;%d;%d;%d", c.R, c.G, c.B)
		} else {
			fmt.Fprintf(&r.buf, ";38;2;%d;%d;%d", c.R, c.G, c.B)
		}
	} else if c.Index >= 0 && c.Index < 8 {
		if isBg {
			fmt.Fprintf(&r.buf, ";%d", 40+c.Index)
		} else {
			fmt.Fprintf(&r.buf, ";%d", 30+c.Index)
		}
	} else if c.Index >= 8 && c.Index < 16 {
		if isBg {
			fmt.Fprintf(&r.buf, ";%d", 100+c.Index-8)
		} else {
			fmt.Fprintf(&r.buf, ";%d", 90+c.Index-8)
		}
	} else if c.Index >= 16 {
		if isBg {
			fmt.Fprintf(&r.buf, ";48;5;%d", c.Index)
		} else {
			fmt.Fprintf(&r.buf, ";38;5;%d", c.Index)
		}
	}
	// Index == -1 means default — don't emit anything (reset handles it)
}

func (r *Renderer) setAttr(fg, bg Color, attr Attr) {
	if colorEqual(fg, r.prevFg) && colorEqual(bg, r.prevBg) && attr == r.prevAttr {
		return
	}
	r.buf.WriteString("\x1b[0") // reset
	if attr&AttrBold != 0 {
		r.buf.WriteString(";1")
	}
	if attr&AttrDim != 0 {
		r.buf.WriteString(";2")
	}
	if attr&AttrItalic != 0 {
		r.buf.WriteString(";3")
	}
	if attr&AttrBlink != 0 {
		r.buf.WriteString(";5")
	}
	if attr&AttrReverse != 0 {
		r.buf.WriteString(";7")
	}
	if attr&AttrInvis != 0 {
		r.buf.WriteString(";8")
	}
	if attr&AttrUnderline != 0 {
		r.buf.WriteString(";4")
	}
	if attr&AttrStrike != 0 {
		r.buf.WriteString(";9")
	}
	if attr&AttrOverline != 0 {
		r.buf.WriteString(";53")
	}
	r.writeColor(fg, false)
	r.writeColor(bg, true)
	r.buf.WriteString("m")
	r.prevFg = fg
	r.prevBg = bg
	r.prevAttr = attr
}

// renderPane paints one node of the layout tree. Caller holds treeMu.RLock.
//
// The nil guard is load-bearing: closing the last pane leaves an empty tree
// with no m.root at all, and every render pass would dereference it.
func (r *Renderer) renderPane(p *Pane) {
	if p == nil {
		return
	}
	if p.splitType != SplitNone {
		r.renderPane(p.child1)
		r.renderBorder(p)
		r.renderPane(p.child2)
		return
	}

	p.mu.Lock()
	s := p.screen
	// The pane's cells are reproduced verbatim: fg, bg and attributes exactly
	// as the child wrote them.
	//
	// There used to be a completion "tint wash" here — a background colour
	// substituted into every cell whose background was default, to mark a pane
	// as finished. It cannot work, in either direction, and the reason is
	// structural: magmux replaces the BACKGROUND under text whose FOREGROUND it
	// does not know and cannot recolour. Claude Code writes light foregrounds
	// because it assumes a dark terminal; a near-black wash therefore kept them
	// legible but turned a finished pane on a light terminal into a black box,
	// and a pale wash (the previous fix) turned it into a blank green rectangle
	// with the session's own output invisible on top of it. There is no third
	// colour that is safe, because the foreground is the child's to choose.
	//
	// Completion is marked with the two surfaces magmux owns outright instead:
	// the pane BORDER (borderColorForPane) and the centred overlay badge, which
	// sets its own foreground and background together.
	//
	// The rows come from viewRow rather than straight out of cells, which is
	// what makes a scrolled-back pane paint history in place. At off == 0 that
	// is cells[row] and the walk below is what it always was; further back the
	// row may be a scrollback line, and a scrollback line keeps the width it had
	// when it was evicted — so the walk is bounded by len(row) and pads the rest
	// with blanks. Padding rather than stopping early matters: the renderer
	// never clears, so a short row that simply stopped writing would leave the
	// previous frame's characters standing to its right.
	off := s.sbOff
	for row := 0; row < s.rows && row < p.h; row++ {
		cells := s.viewRow(off, row)
		r.moveTo(p.y+row, p.x)
		for col := 0; col < s.cols && col < p.w; col++ {
			c := Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
			if col < len(cells) {
				c = cells[col]
			}
			if c.Cont {
				continue
			}
			r.setAttr(c.Fg, c.Bg, c.Attr)
			if c.Ch == 0 || c.Ch == ' ' {
				r.buf.WriteByte(' ')
			} else {
				r.buf.WriteRune(c.Ch)
			}
		}
	}
	scrollBadge := ""
	if off > 0 {
		scrollBadge = scrollBadgeText(off, s.sbLen)
	}
	overlayText := p.overlayText
	overlayStyle := p.overlayStyle
	p.mu.Unlock()

	// The scroll badge is painted over the pane's own top-right corner, and it
	// is the only thing on screen that says a pane is not showing live output.
	// It has to be here rather than on the status bar: the bar is optional
	// (--no-status / Ctrl-G s) and there is one of it for N panes, whereas a
	// scrolled pane is a per-pane state a human needs pointed at directly. It
	// carries the way out for the same reason a modal dialog carries a Cancel.
	if scrollBadge != "" {
		r.renderScrollBadge(p, scrollBadge)
	}

	// Render overlay if present
	if overlayText != "" {
		r.renderOverlay(&Pane{
			y: p.y, x: p.x, h: p.h, w: p.w,
			overlayText: overlayText, overlayStyle: overlayStyle,
		})
	}
}

// scrollBadgeText is what a scrolled-back pane says about itself: how far back
// it is, how far back it CAN go, and the key that puts it live again. The
// denominator is not decoration — without it "40 lines back" gives a reader no
// way to tell a nearly-full ring from a nearly-empty one, which is the same
// question `capture` answers with its scrollback count.
func scrollBadgeText(off, have int) string {
	return fmt.Sprintf(" SCROLL %d/%d · q live ", off, have)
}

// renderScrollBadge paints the badge into the pane's top-right corner. It is
// truncated from the LEFT on a narrow pane so the exit key is the last thing to
// go, and skipped entirely when the pane is too narrow to hold it without
// covering more than it explains.
func (r *Renderer) renderScrollBadge(p *Pane, text string) {
	w := utf8.RuneCountInString(text)
	if p.w < 12 || p.h < 1 {
		return
	}
	if w > p.w {
		runes := []rune(text)
		runes = runes[len(runes)-p.w:]
		text = string(runes)
		w = p.w
	}
	r.moveTo(p.y, p.x+p.w-w)
	// Ink on the warn colour: the badge sets both halves of its own contrast,
	// like the overlay badge and unlike the tint wash that had to be removed.
	r.setAttr(toColor(pal.ink), toColor(pal.warn), AttrBold)
	r.buf.WriteString(text)
	r.setAttr(defaultColor, defaultColor, 0)
}

// borderColorForPane returns the split's rule colour from the tints under it.
//
// The border is one of the two things magmux fully controls on a session pane
// (the other is the overlay badge), and since the interior wash was removed it
// is the ambient half of how a finished pane announces itself: a green rule
// around a done pane, red around a failed one, amber around one that is blocked
// on a permission prompt. It is a foreground on the terminal's own background,
// so it is legible on any terminal — which is exactly what the wash was not.
func borderColorForPane(p *Pane) Color {
	// The loudest tint under either child wins. The old code took child1's and
	// only fell back to child2's when it was empty, which is not what its own
	// comment claimed: a green pane on the left hid a failure on the right.
	switch worseTint(leafTint(p.child1), leafTint(p.child2)) {
	case "red":
		return toColor(pal.fail)
	case "yellow":
		return toColor(pal.warn)
	case "green":
		return toColor(pal.success)
	default:
		// The palette's rule colour, not ANSI 8. Index 8 is "bright black",
		// which a light terminal renders as a pale grey — and renderBorder
		// then draws it dim, on the terminal's own light background. The
		// splits simply disappeared. pal.border is theme-picked and holds
		// 3:1 against its background by test.
		return toColor(pal.border)
	}
}

// tintSeverity orders the tints so a split can show the one that needs a human.
func tintSeverity(t string) int {
	switch t {
	case "red":
		return 3
	case "yellow":
		return 2
	case "green":
		return 1
	}
	return 0
}

// worseTint returns whichever of two tints is more severe.
func worseTint(a, b string) string {
	if tintSeverity(b) > tintSeverity(a) {
		return b
	}
	return a
}

// leafTint returns the most severe tint of any leaf pane under p.
// Caller holds treeMu.RLock; tint is content state, so the leaf read takes
// p.mu — the `tint` verb writes it from a socket goroutine.
func leafTint(p *Pane) string {
	if p == nil {
		return ""
	}
	if p.splitType == SplitNone {
		p.mu.Lock()
		t := p.tint
		p.mu.Unlock()
		return t
	}
	return worseTint(leafTint(p.child1), leafTint(p.child2))
}

func (r *Renderer) renderBorder(p *Pane) {
	bc := borderColorForPane(p)
	// Never dim. Every border colour is now a palette truecolor picked at the
	// contrast it should be drawn at, and dimming is what made the old indexed
	// ANSI 8 vanish on a light terminal. A tinted border is a completion
	// marker; halving its contrast defeats the point of having one.
	r.setAttr(bc, defaultColor, 0)
	if p.splitType == SplitHorizontal {
		bx := p.child1.x + p.child1.w
		for row := 0; row < p.h; row++ {
			r.moveTo(p.y+row, bx)
			r.buf.WriteString("│")
		}
	} else if p.splitType == SplitVertical {
		by := p.child1.y + p.child1.h
		r.moveTo(by, p.x)
		for col := 0; col < p.w; col++ {
			r.buf.WriteString("─")
		}
	}
}

// overlayAccent is the palette colour that carries an overlay's meaning: the
// border, and the header line inside it.
//
// The overlay is the completion marker — the pane interior is the child's and
// must not be recoloured (see renderPane) — so this is one of the few colours
// magmux states outright, and it is a palette token for the same reason the
// border and the status bar are: an index means whatever the user's terminal
// decided it means, and 46-on-22 meant "dark theme" on every terminal.
//
// "info" is amber rather than blue: its only user is the permission overlay,
// whose pane border is already tinted amber (borderColorForPane), and a box in
// a different colour from the rule around it says two things at once.
func overlayAccent(style string) rgb {
	switch style {
	case "success":
		return pal.success
	case "error":
		return pal.fail
	case "info":
		return pal.warn
	default:
		return pal.text
	}
}

// renderOverlay draws a centered popup window on a pane with a rounded border
// and a drop shadow. The overlayText may contain \n for multi-line content;
// the first line is rendered as a bold header.
//
// Every cell it paints sets BOTH a foreground and a background. That is not
// tidiness — it is the whole reason the overlay, and not a background wash, is
// the completion marker: it sits on top of a child's output whose colours
// magmux does not know, and a cell that sets only one half inherits the other
// from whatever the child last left in force. So: the box interior is pal.bar,
// the surface magmux already owns and paints the status bar with, and every
// glyph on it is a palette foreground measured against it.
//
// Hierarchy without SGR 2: the header is bold in the state colour, the detail
// lines are plain body text. The old code said "dim white" (SGR 2 on 37) over
// a dark green fill — 4.38:1 before the terminal's own idea of dim halved it
// again, and unreadable on a light terminal. De-emphasis is a colour here,
// because a colour is a value the palette can state and a test can measure;
// dim is a hint the terminal renders however it likes.
func (r *Renderer) renderOverlay(p *Pane) {
	if p.overlayText == "" {
		return
	}

	lines := strings.Split(p.overlayText, "\n")

	// Compute box dimensions: inner width = widest line, plus 2 cols padding + 2 cols border.
	innerW := 0
	for _, ln := range lines {
		if l := utf8.RuneCountInString(ln); l > innerW {
			innerW = l
		}
	}
	// Clamp inner width so popup fits with room for border + shadow
	maxInner := p.w - 6
	if maxInner < 6 {
		maxInner = 6
	}
	if innerW > maxInner {
		innerW = maxInner
	}
	boxW := innerW + 4     // 1 border + 1 pad on each side
	boxH := len(lines) + 2 // 1 border top + 1 border bottom

	// Need room for drop shadow (1 col right + 1 row bottom)
	if boxW+1 > p.w || boxH+1 > p.h {
		// Fall back to single-line pill for tiny panes
		r.renderOverlayPill(p, lines[0])
		return
	}

	// Center within pane (biased slightly upward)
	bx := p.x + (p.w-boxW)/2
	by := p.y + (p.h-boxH)/2
	if by < p.y {
		by = p.y
	}

	// Style selection — the box surface is the palette's, the border and header
	// carry the state.
	bgCode := bg(pal.bar)
	borderFg := fg(overlayAccent(p.overlayStyle))
	bodyFg := fg(pal.text)
	reset := "\x1b[0m"

	// Drop shadow: cells 1 row below and 1 col right of the box, filled with
	// the palette's shadow. Foreground AND background, both pal.shadow: the
	// cell paints a space, so making the two agree means it is a solid block
	// whatever the terminal does with the glyph, and it can never inherit a
	// foreground from the child underneath.
	shadowCode := bg(pal.shadow) + fg(pal.shadow)
	// Right-side shadow column (skip the very top row so it looks like light from top-left)
	for row := 0; row < boxH; row++ {
		ry := by + row + 1
		rx := bx + boxW
		if ry >= p.y+p.h || rx >= p.x+p.w {
			continue
		}
		r.moveTo(ry, rx)
		r.buf.WriteString(shadowCode)
		r.buf.WriteString(" ")
		r.buf.WriteString(reset)
	}
	// Bottom shadow row
	{
		ry := by + boxH
		if ry < p.y+p.h {
			for col := 0; col < boxW; col++ {
				rx := bx + col + 1
				if rx >= p.x+p.w {
					break
				}
				r.moveTo(ry, rx)
				r.buf.WriteString(shadowCode)
				r.buf.WriteString(" ")
				r.buf.WriteString(reset)
			}
		}
	}

	// Top border: ╭───╮
	r.moveTo(by, bx)
	r.buf.WriteString(bgCode)
	r.buf.WriteString(borderFg)
	r.buf.WriteString("\u256d")
	for i := 0; i < boxW-2; i++ {
		r.buf.WriteString("\u2500")
	}
	r.buf.WriteString("\u256e")
	r.buf.WriteString(reset)

	// Content rows
	for i, ln := range lines {
		ry := by + 1 + i
		if ry >= p.y+p.h {
			break
		}
		// Truncate line to innerW runes
		runes := []rune(ln)
		if len(runes) > innerW {
			if innerW > 1 {
				runes = append(runes[:innerW-1], '\u2026')
			} else {
				runes = runes[:innerW]
			}
		}
		padded := string(runes)
		// Right-pad with spaces
		for j := utf8.RuneCountInString(padded); j < innerW; j++ {
			padded += " "
		}

		r.moveTo(ry, bx)
		r.buf.WriteString(bgCode)
		r.buf.WriteString(borderFg)
		r.buf.WriteString("\u2502") // left │
		// Content: the header carries the state colour and bold; the detail
		// lines are body text. Never dim — see the function comment.
		if i == 0 {
			r.buf.WriteString("\x1b[1m")
			r.buf.WriteString(borderFg)
		} else {
			r.buf.WriteString("\x1b[22m")
			r.buf.WriteString(bodyFg)
		}
		r.buf.WriteString(" ")
		r.buf.WriteString(padded)
		r.buf.WriteString(" ")
		r.buf.WriteString("\x1b[22m") // reset bold/dim
		r.buf.WriteString(borderFg)
		r.buf.WriteString("\u2502") // right │
		r.buf.WriteString(reset)
	}

	// Bottom border: ╰───╯
	ry := by + boxH - 1
	if ry < p.y+p.h {
		r.moveTo(ry, bx)
		r.buf.WriteString(bgCode)
		r.buf.WriteString(borderFg)
		r.buf.WriteString("\u2570")
		for i := 0; i < boxW-2; i++ {
			r.buf.WriteString("\u2500")
		}
		r.buf.WriteString("\u256f")
		r.buf.WriteString(reset)
	}

	// Reset renderer tracking after raw escape codes
	r.prevFg = Color{Index: -2}
	r.prevBg = Color{Index: -2}
	r.prevAttr = 0
}

// renderOverlayPill draws a single-line fallback overlay for very small panes.
func (r *Renderer) renderOverlayPill(p *Pane, text string) {
	text = " " + text + " "
	textLen := utf8.RuneCountInString(text)
	cx := p.x + (p.w-textLen)/2
	cy := p.y + p.h/2
	if cx < p.x {
		cx = p.x
	}
	if cy < p.y || cy >= p.y+p.h {
		return
	}

	// Too small for a box, so the pill is the badge idiom instead: filled with
	// the state colour, written in ink. That pair is the one magmux already
	// imposes everywhere else (badge(), the status bar's pills) and the one
	// TestPaletteContrast measures ink against, so the fallback inherits the
	// same guarantee as the full overlay rather than inventing colours at the
	// size where legibility matters most.
	fill := overlayAccent(p.overlayStyle)
	if p.overlayStyle != "success" && p.overlayStyle != "error" && p.overlayStyle != "info" {
		fill = pal.subtle // "text on text" is not a pill
	}

	r.moveTo(cy, cx)
	r.buf.WriteString(bg(fill))
	r.buf.WriteString(fg(pal.ink))
	r.buf.WriteString("\x1b[1m")
	r.buf.WriteString(text)
	r.buf.WriteString("\x1b[0m")
	r.prevFg = Color{Index: -2}
	r.prevBg = Color{Index: -2}
	r.prevAttr = 0
}

func (r *Renderer) renderSelection(p *Pane) {
	sy, sx, ey, ex := sel.sy, sel.sx, sel.ey, sel.ex
	if sy > ey || (sy == ey && sx > ex) {
		sy, sx, ey, ex = ey, ex, sy, sx
	}
	// Set selection color
	r.buf.WriteString("\x1b[0")
	if selFg >= 0 {
		fmt.Fprintf(&r.buf, ";38;5;%d", selFg)
	}
	if selBg >= 0 {
		fmt.Fprintf(&r.buf, ";48;5;%d", selBg)
	} else {
		r.buf.WriteString(";7") // fallback: reverse video
	}
	r.buf.WriteString("m")

	for row := sy; row <= ey && row < p.h; row++ {
		cs := 0
		ce := p.w - 1
		if row == sy {
			cs = sx
		}
		if row == ey {
			ce = ex
		}
		r.moveTo(p.y+row, p.x+cs)
		p.mu.Lock()
		// `row < p.h` above is a PANE bound, not a screen bound, and the two part
		// company whenever geometry has changed and the screen has not been
		// resized yet. The old cells grid was rows+1000 tall so an overrun landed
		// in the dead tail; now it would be out of range.
		if row >= len(p.screen.cells) {
			p.mu.Unlock()
			continue
		}
		for c := cs; c <= ce && c < p.screen.cols; c++ {
			ch := p.screen.cells[row][c].Ch
			if ch == 0 || ch == ' ' {
				r.buf.WriteByte(' ')
			} else if !p.screen.cells[row][c].Cont {
				r.buf.WriteRune(ch)
			}
		}
		p.mu.Unlock()
	}
	r.buf.WriteString("\x1b[0m")
	r.prevAttr = 0
	r.prevFg = defaultColor
	r.prevBg = defaultColor
}

// barBase is the status bar's ground state: no attributes, the bar's own
// background, body foreground. It is the bar's equivalent of the panel's
// sgrBase, and exists for the same reason — a bare "\x1b[0m" drops the bar's
// background as well as its colour, and "\x1b[39m" drops the foreground to the
// terminal's default, which on the bar's own background is a colour nobody
// chose. Every segment ends by returning here.
func barBase() string { return sgrReset + bg(pal.bar) + fg(pal.text) }

// renderStatusBar paints the bottom status line: the bar's own background,
// accent labels, coloured segments separated by thin vertical rules. Segments
// use the "CODE:text" format; consult the switch below for the full mapping.
//
// This is the one full-width surface magmux fills with a colour of its own, and
// it is the exception that proves FIX 1's rule: a status bar that separates
// itself from the pane above is a convention worth keeping, but the background
// has to belong to the active theme (pal.bar) and every foreground written on
// it is held to its contrast against THAT — see TestPaletteContrast. It used to
// be hardcoded 256-colour (48;5;236 under 38;5;51 cyan, 220 yellow, …), which
// stayed a dark slab with saturated text on a light terminal.
func (r *Renderer) renderStatusBar(row, cols int, text string) {
	var (
		barBg   = bg(pal.bar)
		reset   = barBase()
		divider = fg(pal.border) + "│" + reset
	)

	r.moveTo(row, 0)
	r.buf.WriteString(barBg)
	r.prevBg = Color{Index: -2}

	// Initial padding
	r.buf.WriteString(" ")
	col := 1

	segments := strings.Split(text, "\t")
	for i, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if i > 0 {
			r.buf.WriteString(" ")
			r.buf.WriteString(divider)
			r.buf.WriteString(" ")
			col += 3
		}
		parts := strings.SplitN(seg, ":", 2)
		code := ""
		txt := seg
		if len(parts) == 2 {
			code = strings.TrimSpace(parts[0])
			txt = strings.TrimSpace(parts[1])
		}

		// pill is a saturated chip: palette ink on a state colour, both halves
		// set, so it is legible whatever the terminal is — the same contract
		// the panel's badge() has.
		pill := func(c rgb) {
			r.buf.WriteString(bg(c) + fg(pal.ink) + sgrBold + " " + txt + " " + reset)
			col += utf8.RuneCountInString(txt) + 2
		}
		// label writes a coloured run and returns to the bar's ground state.
		label := func(c rgb, bold bool, s string) {
			if bold {
				r.buf.WriteString(sgrBold)
			}
			r.buf.WriteString(fg(c) + s + reset)
			col += utf8.RuneCountInString(s)
		}

		switch code {
		case "*": // Accent bold asterisk + label (used for "* Opus" style)
			label(pal.accent, true, "* "+txt)
		case "C": // Accent bold label
			label(pal.accent, true, txt)
		case "P": // Success pill
			pill(pal.success)
		case "Pr": // Failure pill
			pill(pal.fail)
		case "Py": // Warning pill
			pill(pal.warn)
		case "$", "Y": // Warning bold (money / running counts)
			label(pal.warn, true, txt)
		case "M": // Secondary data (elapsed time)
			label(pal.debug, true, txt)
		case "G": // Success bold
			label(pal.success, true, txt)
		case "R": // Failure bold
			label(pal.fail, true, txt)
		case "W": // Body text, emphasised
			label(pal.text, true, txt)
		case "D": // Help text — recedes, but is not dimmed on top of that:
			// pal.subtle is already picked to sit at the chrome bar against
			// pal.bar, and SGR 2 on top of it puts it back under.
			label(pal.subtle, false, txt)
		default:
			// Unknown code — render as plain text
			label(pal.text, false, txt)
		}
	}

	// Fill rest of line
	for col < cols {
		r.buf.WriteByte(' ')
		col++
	}
	r.buf.WriteString("\x1b[0m")
	r.prevFg = defaultColor
	r.prevBg = defaultColor
	r.prevAttr = 0
}

// frame returns the painted bytes. It does NOT write them: the renderer runs
// under treeMu.RLock and the terminal write must not (see render()). The string
// stays valid across the next reset() — strings.Builder.Reset drops its buffer
// rather than reusing it — and only the render goroutine ever touches r.buf.
func (r *Renderer) frame() string {
	return r.buf.String()
}

// ── Multiplexer ───────────────────────────────────────────────────────────────

type Magmux struct {
	// treeMu guards the LAYOUT: m.root, m.allPanes (its header, its elements,
	// and each element's id/closed/label), m.focused, m.statusText, m.rows,
	// m.cols, m.closeAt, the package-level `sel`, and the structural fields of
	// every Pane (splitType, y, x, h, w, ratio, child1, child2, parent).
	// Content fields (screen, dirty, dead, tint, inputReady, …) stay under
	// Pane.mu exactly as before.
	//
	// Lock order:
	//
	//	treeMu -> p.mu -> sockClientsMu
	//	treeMu -> cp.mu
	//	treeMu -> claimedMu
	//
	// Three rules:
	//
	//  1. Never hold treeMu across blocking I/O — ptmx.Write, conn.Write,
	//     cmd.Start, os.Stdout.Write, controller.Poll, exec of pbcopy. Resolve
	//     the pointer under RLock, release, then do the I/O. There is no
	//     exception: render() paints into a buffer and writes it after the
	//     unlock, and pollControllers snapshots the pane list under RLock and
	//     polls with the lock released.
	//  2. sync.RWMutex is NOT reentrant. A second RLock on one goroutine
	//     deadlocks if a writer is queued between them, and the failure mode is
	//     a silent hang rather than a race report. Every function reachable
	//     from a site that already holds treeMu has a …Locked twin —
	//     allPanesDoneLocked above all, because renderLocked holds RLock
	//     throughout.
	//  3. Never acquire treeMu while holding p.mu, cp.mu, sockClientsMu or
	//     claimedMu.
	//
	// One deliberate exception to the usual reader/writer split: render()
	// WRITES m.statusText while holding only RLock. That is safe because
	// render is the single render-loop goroutine and every other writer of
	// statusText (the `status` verb, updateAgentStatusBar) takes the full
	// Lock, which excludes it. Anything else that wants to write under RLock
	// has to make the same argument or it is a race.
	treeMu sync.RWMutex
	// closing is set by cleanup() under treeMu.Lock BEFORE it calls wg.Wait().
	// OpenPane checks it under the same lock, because an open_pane arriving
	// during teardown would otherwise panic with "WaitGroup misuse: Add called
	// concurrently with Wait".
	closing    bool
	root       *Pane
	focused    *Pane
	allPanes   []*Pane // leaf panes only; append-only, index == Pane.id
	rows, cols int
	statusText string
	renderer   Renderer
	// out is where a painted frame goes. Nil means os.Stdout, which is what
	// every real run uses; it exists so a test can substitute a writer it
	// controls — both to keep thousands of ANSI frames out of the test log and
	// to PROVE the frame is written with treeMu released (a writer that blocks
	// for 200ms must not delay a treeMu.Lock by anything like 200ms).
	//
	// Written once before renderLoop starts and never again, so it needs no
	// lock; treat it as immutable from the moment the render loop exists.
	out           io.Writer
	rawState      *term.State
	quit          chan struct{}
	quitOnce      sync.Once
	wg            sync.WaitGroup
	gridMode      bool       // -g flag was used
	autoExit      bool       // -w flag: quit automatically when all panes done
	sockPath      string     // /tmp/magmux-{pid}.sock, or {name} with --id
	sockID        string     // --id NAME: bind a named socket instead of the pid one
	sockClients   []net.Conn // currently-connected socket subscribers (for push events)
	sockClientsMu sync.Mutex
	// finalEvents holds the marshaled shutdown payloads (results, then
	// shutdown) once teardown begins. Guarded by sockClientsMu. Non-empty
	// means "shutdown has started": a client connecting from that moment on
	// is never registered for broadcasts, and is instead replayed these
	// events directly before being closed. That makes "every subscriber
	// receives results before EOF" hold no matter when it connects — see
	// handleSocketConn and recordFinalEvents.
	finalEvents [][]byte
	// sockDone is closed once the socket server has finished its teardown —
	// final results/shutdown broadcast, subscribers flushed and closed,
	// listener removed. main waits on it before exiting, because that
	// teardown is what delivers `results`; exiting first drops it entirely.
	sockDone     chan struct{}
	sockDoneOnce sync.Once
	// layoutReady is closed once the layout EXISTS: buildGrid/buildLayout have
	// run, read loops are running and controllers are attached. The socket
	// binds long before that (MAGMUX_SOCK has to be in the environment of the
	// very first child), so without this a client that connects inside that
	// window is served against an empty m.allPanes — and the first thing
	// handleSocketConn writes is the connect-time aggregate snapshot, which
	// every subscriber seeds its entire pane map from precisely because magmux
	// only pushes per-pane snapshots on CHANGE. An empty aggregate therefore
	// does not merely arrive early, it strands that client forever waiting for
	// state it will never be told again. Verbs served in the window were just
	// as wrong, and reported it as `no_such_pane`, which reads as "your index
	// is bad" rather than "there is no layout yet".
	//
	// Nil means "no wait": the in-process tests build their layout
	// synchronously before anything can look at it.
	layoutReady     chan struct{}
	layoutReadyOnce sync.Once
	lastDoneCount   int       // track status bar updates to avoid redundant rewrites
	startedAt       time.Time // when magmux started (for status bar timer)
	completedAt     time.Time // when all panes reached "done" (freezes timer)
	lastTimerTick   int       // elapsed seconds at last forced status redraw
	// Interactive tool controllers
	controllerFactories []ControllerFactory
	// pollMu serialises pollControllers and guards lastControllerPoll. It is
	// its OWN lock rather than treeMu because the poll must not hold treeMu:
	// ClaudeCodeController.Poll walks ~/.claude/projects on every tick until it
	// finds a transcript. pollControllers takes treeMu.RLock inside pollMu to
	// snapshot the pane list, so nothing may ever take pollMu while holding
	// treeMu.
	pollMu             sync.Mutex
	lastControllerPoll time.Time
	ctx                context.Context
	// claimedSessions maps controller-managed session file paths to the
	// pane that owns them. Used so sibling controllers don't both pick the
	// same JSONL file when running in the same project directory.
	claimedSessions map[string]*Pane
	claimedMu       sync.Mutex
	// control is the controlled-session panel. Always non-nil so the record*
	// methods are safe to call unconditionally; it only paints if a control
	// pane was built for it (magmux -c).
	control *ControlPanel
	// autoCloseAfter is how long to wait after a pilot declares the run over
	// before quitting (-x). Zero means wait for an explicit keypress, which
	// is the default: a finished run that vanishes before it is read is
	// worse than one that lingers.
	autoCloseAfter time.Duration
	closeAt        time.Time // when the armed countdown fires; zero if not armed
	// themePref is --theme ("", "auto", "light" or "dark"). Empty falls back
	// to MAGMUX_THEME, which falls back to auto-detection. See theme.go.
	themePref string
	// pendingInput is input that arrived DURING startup and has not been
	// handled yet — bytes the OSC 11 theme probe read off stdin that were not
	// part of the terminal's reply.
	//
	// The probe is the only reader of stdin that runs before inputLoop's own
	// goroutine, and it cannot tell the terminal to answer without also
	// draining whatever the user typed in the meantime. Dropping those bytes
	// would mean magmux eats keystrokes at startup — including the ones the
	// PTY-driven tests send — so they are parked here and replayed into the
	// input loop ahead of everything read later, in order.
	//
	// Written in init(), read once by inputLoop, and the two cannot overlap:
	// init() returns before any goroutine exists. No lock, deliberately.
	pendingInput []byte
	// stdin is where keystrokes come from. Nil means os.Stdin, which is every
	// real run; a test points it at a pipe so it can drive inputLoop without
	// swapping a package-level variable out from under the goroutine that is
	// blocked reading it.
	stdin *os.File
	// themeAskedAt is when the OSC 11 background query was written to the
	// terminal, or the zero time if it never was (--theme/MAGMUX_THEME said
	// which palette to use, or stdin is not a tty). It opens the window in
	// which inputLoop swallows a late reply; see themeReplyWindow.
	//
	// Same discipline as pendingInput: written in init(), read by inputLoop on
	// the same goroutine, and nothing else touches it.
	themeAskedAt time.Time

	// ── chrome (Ctrl-G p / Ctrl-G s) ────────────────────────────────────────
	//
	// Both of these are stated as NEGATIVES so the zero value is today's
	// behaviour: a Magmux built by a test literal still gets its status row and
	// still reserves it in buildGrid/buildLayout. Guarded by treeMu — the
	// status row is layout, and so is where the panel sits.

	// hideStatus removes the bottom status row and gives it to the layout
	// (--no-status, Ctrl-G s).
	hideStatus bool

	// panel is the control-panel pane, on screen or hidden. Nil when this
	// magmux has none (every test literal, and any future mode that opts out).
	// A pointer rather than "the first isControl pane in allPanes", because the
	// PTY-less pane is also what the unit tests build their fake sessions from
	// and a search would find one of those instead.
	panel *Pane

	// panelAnchor / panelSplit / panelRatio / panelFirst remember where the
	// panel was when it was hidden, so showing it puts it BACK rather than
	// somewhere plausible.
	//
	// The anchor is the sibling node that inherited the panel's space, because
	// that is exactly what removeLeafLocked leaves behind: it collapses the
	// parent into the sibling and hands the sibling the parent's exact
	// geometry. Splitting that same node again with the same type and ratio, on
	// the same side, is the inverse operation and restores the tree byte for
	// byte. It is a POINTER and not an id because the sibling is usually an
	// internal node, which has no id; showPanelLocked therefore re-verifies it
	// is still reachable from m.root before using it and falls back to the root
	// if an agent closed it in the meantime.
	//
	// panelFirst is stated as the negative for the usual reason: the zero value
	// has to be the right answer for a panel that has never been hidden, and
	// every layout builder puts the panel LAST — child2, the right-hand column.
	panelAnchor *Pane
	panelSplit  SplitType
	panelRatio  float64
	panelFirst  bool // the panel was child1 (the left-hand / upper column)

	// chromeNote is a transient refusal shown in the status bar — "no room for
	// the panel" on a terminal too narrow to split. It is deliberately NOT part
	// of m.statusText: it belongs to the keystroke that caused it, not to the
	// run, and it expires on its own.
	chromeNote   string
	chromeNoteAt time.Time

	// chordArmed is set for exactly as long as Ctrl-G has been pressed and
	// magmux is waiting for the second key. The status row shows the chord's
	// own second keys for that beat, which is the only way a prefix key can
	// teach itself on a terminal that otherwise shows nothing. Written by
	// inputLoop under treeMu.Lock, read by renderLocked under RLock.
	chordArmed bool
}

// chromeNoteTTL is how long a refusal stays in the status bar. It clears on
// the next repaint after that; an idle magmux paints nothing, which is the
// whole rendering model, so the note can outstay this on a still screen.
const chromeNoteTTL = 4 * time.Second

// claimSession atomically attempts to mark `path` as owned by `p`. Returns
// true if the claim succeeded (path was free). Used by controllers that
// resolve their target file by scanning a directory.
func (m *Magmux) claimSession(path string, p *Pane) bool {
	m.claimedMu.Lock()
	defer m.claimedMu.Unlock()
	if m.claimedSessions == nil {
		m.claimedSessions = make(map[string]*Pane)
	}
	if owner, ok := m.claimedSessions[path]; ok && owner != p {
		return false
	}
	m.claimedSessions[path] = p
	return true
}

// isSessionClaimed returns true if `path` is already owned by a pane other
// than `p`. Used by controllers to skip files claimed by siblings during
// directory scanning.
func (m *Magmux) isSessionClaimed(path string, p *Pane) bool {
	m.claimedMu.Lock()
	defer m.claimedMu.Unlock()
	if owner, ok := m.claimedSessions[path]; ok && owner != p {
		return true
	}
	return false
}

// releaseSessions drops every transcript claim held by p.
//
// claimedSessions was never cleaned, which was harmless while panes only ever
// appeared. Once a pane can be closed it is not: a later pane in the same
// project can never claim that transcript, so its controller sits in `starting`
// silently and forever with nothing logged anywhere.
//
// Caller must NOT hold claimedMu.
func (m *Magmux) releaseSessions(p *Pane) {
	if p == nil {
		return
	}
	m.claimedMu.Lock()
	for path, owner := range m.claimedSessions {
		if owner == p {
			delete(m.claimedSessions, path)
		}
	}
	m.claimedMu.Unlock()
}

// ── Pane identity ────────────────────────────────────────────────────────────
//
// m.allPanes is an append-only slot table and a pane's id IS its index. Nothing
// ever renumbers: close_pane tombstones the slot instead of compacting, because
// the socket protocol's only addressing mode is an integer, so compacting would
// make `send` to pane 1 quietly hit a different session with no error anywhere.
//
// The two resolvers below are the ONLY way to turn an int into a *Pane. Any
// surviving raw subscript of allPanes writes tint, overlay or keystrokes into
// a detached pane — no error, no repaint, no log — which is why the rule is
// enforced by grepping for a subscript of allPanes and expecting no hit
// outside paneByIDLocked.

// paneByIDLocked returns the live pane with this id, or nil if the id is
// negative, out of range, or tombstoned. Caller holds at least treeMu.RLock.
func (m *Magmux) paneByIDLocked(id int) *Pane {
	if id < 0 || id >= len(m.allPanes) {
		return nil
	}
	p := m.allPanes[id]
	if p == nil || p.closed {
		return nil
	}
	return p
}

// livePanesLocked appends every live (non-tombstoned) pane to dst and returns
// it. Pass a nil dst for a fresh slice, or a reused one to avoid allocating in
// the render path. Caller holds at least treeMu.RLock.
func (m *Magmux) livePanesLocked(dst []*Pane) []*Pane {
	dst = dst[:0]
	for _, p := range m.allPanes {
		if p == nil || p.closed {
			continue
		}
		dst = append(dst, p)
	}
	return dst
}

// statusRowsLocked is how many rows the bottom status bar takes off the
// layout: 1 normally, 0 when it is hidden (--no-status / Ctrl-G s). Every
// place that used to write a bare `statusH := 1` goes through it, so showing
// and hiding the bar is one number rather than three that can drift apart.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) statusRowsLocked() int {
	if m.hideStatus {
		return 0
	}
	return 1
}

// reflowLocked resizes the whole tree to the current terminal minus the status
// row. It is the SIGWINCH path, reused verbatim by the two chrome toggles:
// showing or hiding either the panel or the status bar is a reflow and nothing
// more, and a second copy of this arithmetic is how the two would drift.
//
// Caller holds treeMu.Lock.
func (m *Magmux) reflowLocked() {
	if m.root == nil {
		return
	}
	m.root.resize(0, 0, maxInt(0, m.rows-m.statusRowsLocked()), maxInt(0, m.cols))
}

// panelLocked returns the control-panel pane whether it is on screen or
// hidden, or nil if this magmux has none or an agent closed it.
// Caller holds at least treeMu.RLock.
func (m *Magmux) panelLocked() *Pane {
	if m.panel == nil || m.panel.closed {
		return nil
	}
	return m.panel
}

// panelHiddenLocked reports whether the panel exists and is currently out of
// the tree. Caller holds at least treeMu.RLock.
func (m *Magmux) panelHiddenLocked() bool {
	p := m.panelLocked()
	return p != nil && p.hidden
}

// nodeInTreeLocked reports whether n is still reachable from m.root. The panel
// anchor is a raw pointer to a node that may since have been collapsed away by
// a close_pane, and splicing onto a detached node would attach the panel to a
// subtree nothing paints — invisible and undismissable, the same failure
// OpenPane re-verifies against.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) nodeInTreeLocked(n *Pane) bool {
	if n == nil {
		return false
	}
	var walk func(*Pane) bool
	walk = func(p *Pane) bool {
		if p == nil {
			return false
		}
		if p == n {
			return true
		}
		return walk(p.child1) || walk(p.child2)
	}
	return walk(m.root)
}

// markAllDirtyLocked forces a full repaint. Geometry changed under every pane,
// and the dirty-flag model means a frame is only painted when some pane says
// its CONTENT changed — so without this a reflow can sit unpainted until the
// next keystroke. Caller holds at least treeMu.RLock.
func (m *Magmux) markAllDirtyLocked() {
	for _, p := range m.livePanesLocked(nil) {
		if p.hidden {
			continue
		}
		p.mu.Lock()
		p.dirty = true
		p.mu.Unlock()
	}
}

// noteChromeLocked parks a transient message for the status bar.
// Caller holds treeMu.Lock.
func (m *Magmux) noteChromeLocked(s string) {
	m.chromeNote = s
	m.chromeNoteAt = time.Now()
}

// armChord arms or disarms the Ctrl-G menu on the status row. It marks the
// screen dirty because an idle magmux paints nothing at all, so the menu would
// otherwise never appear — and would never leave. Caller must NOT hold treeMu.
func (m *Magmux) armChord(on bool) {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	if m.chordArmed == on {
		return
	}
	m.chordArmed = on
	m.markAllDirtyLocked()
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

// chromeNoteLocked returns the live refusal message, or "" once it has expired.
// Caller holds at least treeMu.RLock.
func (m *Magmux) chromeNoteLocked() string {
	if m.chromeNote == "" || time.Since(m.chromeNoteAt) > chromeNoteTTL {
		return ""
	}
	return m.chromeNote
}

// paneByID is the locking twin of paneByIDLocked, for callers that hold nothing.
// Caller must NOT hold treeMu.
func (m *Magmux) paneByID(id int) *Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.paneByIDLocked(id)
}

// livePanes is the locking twin of livePanesLocked.
// Caller must NOT hold treeMu.
func (m *Magmux) livePanes() []*Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.livePanesLocked(nil)
}

func (m *Magmux) init() error {
	m.startedAt = time.Now()
	// Debug log
	if os.Getenv("MAGMUX_DEBUG") != "" {
		dbgFile, _ = os.Create("/tmp/magmux-debug.log")
	}

	// Parse selection color config from env
	if v := os.Getenv("MAGMUX_SEL_FG"); v != "" {
		fmt.Sscanf(v, "%d", &selFg)
	}
	if v := os.Getenv("MAGMUX_SEL_BG"); v != "" {
		fmt.Sscanf(v, "%d", &selBg)
	}

	// Enter raw mode
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	m.rawState = state

	// Get terminal size
	w, h, err := term.GetSize(fd)
	if err != nil {
		m.restore()
		return fmt.Errorf("get size: %w", err)
	}
	m.rows = h
	m.cols = w
	m.quit = make(chan struct{})

	// Alternate screen + hide cursor + enable SGR mouse tracking
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[?1000h\x1b[?1002h\x1b[?1006h")

	// Pick the palette. Runs here and nowhere else: after MakeRaw (so the
	// terminal will not echo the query, and the reply arrives byte-for-byte),
	// on the alternate screen (so a terminal that prints the query instead of
	// answering it scribbles on a screen we are about to paint over anyway),
	// and before any child is spawned or any goroutine reads stdin.
	m.initTheme(fd)

	return nil
}

// initTheme resolves --theme / MAGMUX_THEME, probing the terminal only when
// neither says. Any keystrokes the probe swallowed are parked in
// m.pendingInput for inputLoop; see the field's comment for why that matters.
func (m *Magmux) initTheme(fd int) {
	pref := themeSetting(m.themePref, os.Getenv("MAGMUX_THEME"))
	// The colour the probe read, if it read one. It outlives the classification
	// because children ask the same question magmux just asked, and are owed the
	// real answer rather than the palette's stand-in for it.
	var probed rgb
	var probedOK bool
	kind, leftover := resolveTheme(pref, func() (themeKind, []byte) {
		// Nothing to ask and nobody to answer: a piped stdin has no
		// background colour, and a dumb terminal has no OSC at all. Both
		// would otherwise cost every run the probe timeout for nothing.
		if !term.IsTerminal(fd) || os.Getenv("TERM") == "dumb" {
			return themeDark, nil
		}
		// Recorded before the probe writes, and only on the path that
		// actually writes: an explicit --theme never asks the terminal
		// anything, so inputLoop must not then go looking for an answer.
		m.themeAskedAt = time.Now()
		k, c, ok, rest := detectThemeColor(os.Stdin, themeProbeTimeout)
		probed, probedOK = c, ok
		return k, rest
	})
	setTheme(kind)
	// After setTheme, which resets the reported colours to the palette's
	// assumptions. An explicit --theme never probes, so it keeps those — a
	// coherent guess, which is all a child needs.
	if probedOK {
		setDetectedBackground(probed)
	}
	m.pendingInput = append(m.pendingInput, leftover...)
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "theme: pref=%s -> %s (%d bytes of input preserved, background %s)\n",
			pref, kind, len(leftover), xColorString(termBack))
	}
}

func (m *Magmux) restore() {
	// Disable mouse + show cursor + exit alternate screen
	os.Stdout.WriteString("\x1b[?1006l\x1b[?1002l\x1b[?1000l\x1b[?25h\x1b[?1049l")
	if m.rawState != nil {
		term.Restore(int(os.Stdin.Fd()), m.rawState)
	}
}

func (m *Magmux) buildLayout(commands []PaneConfig) error {
	statusH := m.statusRowsLocked()
	availH := m.rows - statusH

	if len(commands) == 0 {
		return fmt.Errorf("no commands specified")
	}

	// Special layout for POC: top half split horizontal, bottom pane, status bar
	switch len(commands) {
	case 1:
		p, err := newPaneFor(0, 0, availH, m.cols, commands[0])
		if err != nil {
			return err
		}
		m.root = p
		m.allPanes = []*Pane{p}
		m.focused = p

	case 2:
		// Horizontal split
		m.root = &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: 0.5,
		}
		w1 := m.cols / 2
		w2 := m.cols - w1 - 1
		p1, err := newPaneFor(0, 0, availH, w1, commands[0])
		if err != nil {
			return err
		}
		p2, err := newPaneFor(0, w1+1, availH, w2, commands[1])
		if err != nil {
			return err
		}
		m.root.child1 = p1
		m.root.child2 = p2
		p1.parent = m.root
		p2.parent = m.root
		m.allPanes = []*Pane{p1, p2}
		m.focused = p1

	default: // 3+ panes: top row horizontal split, bottom pane(s)
		topH := availH * 2 / 3
		botH := availH - topH - 1

		// Top: horizontal split of first two commands
		topPane := &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: topH, w: m.cols,
			ratio: 0.5,
		}
		w1 := m.cols / 2
		w2 := m.cols - w1 - 1
		p1, err := newPaneFor(0, 0, topH, w1, commands[0])
		if err != nil {
			return err
		}
		p2, err := newPaneFor(0, w1+1, topH, w2, commands[1])
		if err != nil {
			return err
		}
		topPane.child1 = p1
		topPane.child2 = p2
		p1.parent = topPane
		p2.parent = topPane

		// Bottom pane
		p3, err := newPaneFor(topH+1, 0, botH, m.cols, commands[2])
		if err != nil {
			return err
		}

		// Root: vertical split (top | bottom)
		m.root = &Pane{
			splitType: SplitVertical,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: float64(topH) / float64(availH),
		}
		m.root.child1 = topPane
		m.root.child2 = p3
		topPane.parent = m.root
		p3.parent = m.root

		m.allPanes = []*Pane{p1, p2, p3}
		m.focused = p1
	}

	m.stampPaneIDs()
	return nil
}

// stampPaneIDs assigns each pane its permanent id — its index in m.allPanes.
// Called by both layout builders as the last step, before any goroutine can
// see a pane. Ids are written once and never touched again, which is what lets
// every other goroutine read Pane.id without treeMu.
func (m *Magmux) stampPaneIDs() {
	for i, p := range m.allPanes {
		p.id = i
		p.mux = m
	}
}

// buildColumn recursively splits a slice of commands into a vertical binary tree.
func buildColumn(cmds []PaneConfig, y, x, h, w int) (*Pane, []*Pane, error) {
	if len(cmds) == 1 {
		p, err := newPaneFor(y, x, h, w, cmds[0])
		if err != nil {
			return nil, nil, err
		}
		return p, []*Pane{p}, nil
	}
	// Split: top half gets ceil(N/2), bottom gets rest
	topN := (len(cmds) + 1) / 2
	topH := h * topN / len(cmds)
	botH := h - topH - 1 // -1 for border

	topPane, topLeaves, err := buildColumn(cmds[:topN], y, x, topH, w)
	if err != nil {
		return nil, nil, err
	}
	botPane, botLeaves, err := buildColumn(cmds[topN:], y+topH+1, x, botH, w)
	if err != nil {
		return nil, nil, err
	}

	parent := &Pane{
		splitType: SplitVertical,
		y:         y, x: x, h: h, w: w,
		ratio: float64(topH) / float64(h),
	}
	parent.child1 = topPane
	parent.child2 = botPane
	topPane.parent = parent
	botPane.parent = parent

	leaves := append(topLeaves, botLeaves...)
	return parent, leaves, nil
}

// buildGrid creates a balanced 2-column grid layout from a list of commands.
func (m *Magmux) buildGrid(commands []PaneConfig) error {
	statusH := m.statusRowsLocked()
	availH := m.rows - statusH

	if len(commands) == 0 {
		return fmt.Errorf("no commands specified")
	}

	switch len(commands) {
	case 1:
		// Single pane — fullscreen
		p, err := newPaneFor(0, 0, availH, m.cols, commands[0])
		if err != nil {
			return err
		}
		m.root = p
		m.allPanes = []*Pane{p}
		m.focused = p

	case 2:
		// Horizontal split: left | right
		w1 := m.cols / 2
		w2 := m.cols - w1 - 1
		p1, err := newPaneFor(0, 0, availH, w1, commands[0])
		if err != nil {
			return err
		}
		p2, err := newPaneFor(0, w1+1, availH, w2, commands[1])
		if err != nil {
			return err
		}
		m.root = &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: 0.5,
		}
		m.root.child1 = p1
		m.root.child2 = p2
		p1.parent = m.root
		p2.parent = m.root
		m.allPanes = []*Pane{p1, p2}
		m.focused = p1

	default:
		// 3+ commands: left column gets ceil(N/2), right gets rest
		leftN := (len(commands) + 1) / 2
		leftW := m.cols / 2
		rightW := m.cols - leftW - 1

		leftPane, leftLeaves, err := buildColumn(commands[:leftN], 0, 0, availH, leftW)
		if err != nil {
			return err
		}
		rightPane, rightLeaves, err := buildColumn(commands[leftN:], 0, leftW+1, availH, rightW)
		if err != nil {
			return err
		}

		m.root = &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: float64(leftW) / float64(m.cols),
		}
		m.root.child1 = leftPane
		m.root.child2 = rightPane
		leftPane.parent = m.root
		rightPane.parent = m.root

		leaves := append(leftLeaves, rightLeaves...)
		m.allPanes = leaves
		m.focused = leaves[0]
	}

	// Mark all panes as grid mode
	for _, p := range m.allPanes {
		p.gridMode = true
	}

	m.stampPaneIDs()
	return nil
}

// installHiddenPanel creates the control panel and gives it an id, but leaves
// it OUT of the layout tree.
//
// It is deliberately not a config appended to the layout builders. Handing them
// an extra command would change the shape they build — buildLayout's 3+ branch
// only ever builds three panes and would silently drop it — and the panel would
// then have to be surgically removed again to start hidden. Building the
// session layout exactly as it has always been built and adding the panel to
// the id table afterwards means the visible layout of a hidden-panel magmux is
// byte-identical to a magmux that has no panel at all.
//
// The panel still lands LAST in m.allPanes, so session panes keep the indices a
// controller would naturally use (pane 0 is the first -e command).
//
// Caller must NOT hold treeMu.
func (m *Magmux) installHiddenPanel() *Pane {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	if p := m.panelLocked(); p != nil {
		return p
	}
	// Plausible geometry so nothing reads a zero-sized screen before the first
	// Ctrl-G p; showPanelLocked resizes it for real through reshapeChildren.
	h := maxInt(1, m.rows-m.statusRowsLocked())
	w := maxInt(1, m.cols/2)
	p := newControlPane(0, maxInt(0, m.cols-w), h, w, "")
	p.hidden = true
	p.gridMode = m.gridMode
	m.allPanes = append(m.allPanes, p)
	m.panel = p
	m.stampPaneIDs()
	return p
}

func (m *Magmux) startReadLoops() {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	for _, p := range m.livePanesLocked(nil) {
		if p.isControl {
			continue // no PTY to read from
		}
		m.wg.Add(1)
		go p.readLoop(&m.wg)
	}
}

// attachControllers walks every leaf pane and attaches a ToolController if
// any registered factory recognizes the pane's command. Called once after
// the layout is built and panes are spawned.
func (m *Magmux) attachControllers() {
	for _, p := range m.livePanes() {
		m.attachController(p)
	}
}

// attachController attaches a ToolController to one pane if a factory
// recognizes its command.
//
// It exists separately from attachControllers because OpenPane calls it while
// the new pane is still PRIVATE — before it is appended to m.allPanes — so
// p.controller is written before any other goroutine can observe the pane, and
// Start()'s filesystem work happens off treeMu entirely.
//
// Caller must NOT hold treeMu (Start touches the filesystem) or p.mu.
func (m *Magmux) attachController(p *Pane) {
	if m.ctx == nil {
		m.ctx = context.Background()
	}
	p.mux = m
	if p.controller != nil {
		return
	}
	for _, factory := range m.controllerFactories {
		c := factory(p)
		if c == nil {
			continue
		}
		p.controller = c
		if err := c.Start(m.ctx); err != nil {
			if dbgFile != nil {
				fmt.Fprintf(dbgFile, "[ctrl] %s.Start error: %v\n", c.Name(), err)
			}
			p.controller = nil
			continue
		}
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl] attached %s to pane %d\n", c.Name(), p.id)
		}
		return
	}
}

// pollControllers polls each attached controller and translates the resulting
// Snapshot into pane state (inputReady, tint, overlayText). Throttled to ~4Hz;
// `force` skips the throttle for the final poll before teardown.
//
// It COLLECTS its snapshot events and returns them rather than broadcasting
// inline. Broadcasting here would put conn.Write — with its 100ms-per-client
// deadline — on the render goroutine, so one wedged subscriber would stall the
// frame. The caller broadcasts.
//
// Caller must NOT hold treeMu. Poll is filesystem work, not memory work:
// ClaudeCodeController.Poll tails a transcript and, until it has found one,
// re-scans every directory under ~/.claude/projects on EVERY tick. Under
// treeMu.RLock that scan blocks the next queued writer — a keystroke, SIGWINCH,
// or any socket open_pane/close_pane/focus — for its whole duration, which is
// how a healthy magmux misses a 10s MCP lifecycle timeout. So the pane list is
// snapshotted under RLock and the lock is released before any Poll runs.
//
// A pane closed between the snapshot and its Poll is polled anyway: the poll
// only reads a transcript, and applying its snapshot writes p.mu-guarded
// content fields on a pane nothing paints any more. Harmless, and cheaper than
// re-resolving every pane under the lock.
func (m *Magmux) pollControllers(force bool) []any {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()

	now := time.Now()
	if !force && !m.lastControllerPoll.IsZero() && now.Sub(m.lastControllerPoll) < 250*time.Millisecond {
		return nil
	}
	m.lastControllerPoll = now

	m.treeMu.RLock()
	panes := m.livePanesLocked(nil)
	m.treeMu.RUnlock()

	var events []any
	for _, p := range panes {
		if p.controller == nil {
			continue
		}
		snap, err := p.controller.Poll()
		if err != nil {
			if dbgFile != nil {
				fmt.Fprintf(dbgFile, "[ctrl] %s.Poll error: %v\n", p.controller.Name(), err)
			}
			continue
		}
		p.mu.Lock()
		prev := p.controllerSnap
		p.controllerSnap = snap
		applyControllerSnapshot(p, snap)
		p.mu.Unlock()

		// Broadcast on meaningful change (state transition, new response,
		// tool change, or new prompt). Skip no-op polls.
		if snapshotChanged(prev, snap) {
			// Feed the control panel from what magmux actually observed,
			// not from what the pilot claims it asked for — the panel has
			// to be able to show the two disagreeing.
			m.control.recordObserved(p.id, snap.State.String(), snap.LastResponse, snap.LastTool)
			events = append(events, map[string]any{
				"type":        "snapshot",
				"pane":        p.id,
				"controller":  p.controller.Name(),
				"state":       snap.State.String(),
				"project":     snap.Project,
				"model":       snap.Model,
				"prompt":      snap.LastUserPrompt,
				"response":    snap.LastResponse,
				"tool":        snap.LastTool,
				"startedAt":   timeStringOrEmpty(snap.StartedAt),
				"completedAt": timeStringOrEmpty(snap.CompletedAt),
			})
		}
	}
	return events
}

// snapshotChanged returns true if the meaningful fields of a controller
// snapshot differ from the previous one.
func snapshotChanged(a, b Snapshot) bool {
	if a.State != b.State {
		return true
	}
	if a.LastResponse != b.LastResponse {
		return true
	}
	if a.LastUserPrompt != b.LastUserPrompt {
		return true
	}
	if a.LastTool != b.LastTool {
		return true
	}
	return false
}

func timeStringOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// applyControllerSnapshot translates a Snapshot into pane visual state.
// Caller must hold p.mu.
func applyControllerSnapshot(p *Pane, s Snapshot) {
	switch s.State {
	case CtrlAwaitingInput:
		if !p.inputReady {
			p.inputReady = true
			p.inputSignal = "ctrl"
			p.inputReadyAt = time.Now()
			p.tint = "green"
			p.overlayStyle = "success"
			lines := []string{"\u2713 DONE"}
			if !s.StartedAt.IsZero() && !s.CompletedAt.IsZero() {
				lines = append(lines, "took "+formatDuration(s.CompletedAt.Sub(s.StartedAt)))
			}
			if s.LastResponse != "" {
				msg := s.LastResponse
				// Collapse newlines and truncate to 40 runes
				msg = strings.ReplaceAll(msg, "\n", " ")
				msg = strings.TrimSpace(msg)
				if utf8.RuneCountInString(msg) > 40 {
					runes := []rune(msg)
					msg = string(runes[:39]) + "\u2026"
				}
				lines = append(lines, msg)
			}
			p.overlayText = strings.Join(lines, "\n")
			p.dirty = true
		}

	case CtrlAwaitingPermission:
		if !p.inputReady || p.inputSignal != "perm" {
			p.inputReady = true
			p.inputSignal = "perm"
			p.inputReadyAt = time.Now()
			p.tint = "yellow"
			p.overlayText = "\u26a0 NEEDS PERMISSION"
			p.overlayStyle = "info"
			p.dirty = true
		}

	case CtrlError:
		p.tint = "red"
		if s.Error != nil {
			p.overlayText = "\u2717 " + s.Error.Error()
		} else {
			p.overlayText = "\u2717 ERROR"
		}
		p.overlayStyle = "error"
		p.dirty = true

	case CtrlWorking, CtrlStarting:
		// A new turn started, so clear any stale "done" state. Reaching here
		// means applyTerminalIdle already declined to promote — the transcript
		// advanced more recently than the terminal went idle — so a lingering
		// terminal-derived inputReady is stale and would otherwise make
		// buildPaneResults report awaiting_input mid-turn. "perm" is left
		// alone: a permission prompt is a genuine block, not a finished turn.
		if p.inputReady && p.inputSignal != "perm" {
			p.inputReady = false
			p.tint = ""
			p.overlayText = ""
			p.overlayStyle = ""
			p.dirty = true
		}
	}
}

func (m *Magmux) handleSIGWINCH() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-sigCh:
				w, h, err := term.GetSize(int(os.Stdin.Fd()))
				if err != nil {
					continue
				}
				// Geometry is structural, and m.root.resize writes it on
				// every node in the tree — this is a writer, not a reader.
				m.treeMu.Lock()
				m.rows = h
				m.cols = w
				m.reflowLocked()
				m.treeMu.Unlock()
				// resize rebuilds each pane's Screen. A child process
				// redraws itself on SIGWINCH; the control panel has no
				// process to do that, so it must be repainted here or it
				// comes back blank.
				m.control.markDirty()
			case <-m.quit:
				return
			}
		}
	}()
}

// focusNext moves focus to the next live pane, wrapping. Tombstoned panes are
// skipped: they are not on screen, so focusing one would look like focus
// vanishing. Caller must NOT hold treeMu.
func (m *Magmux) focusNext() {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	// A hidden pane is not on screen, so cycling onto it would look exactly
	// like focus vanishing — the same reason tombstones are skipped.
	var live []*Pane
	for _, p := range m.livePanesLocked(nil) {
		if !p.hidden {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return
	}
	// The lock order is treeMu -> cp.mu, so telling the panel from in here is
	// legal; it marks which route row the user is actually looking at.
	for i, p := range live {
		if p == m.focused {
			m.focused = live[(i+1)%len(live)]
			m.control.setFocused(m.focused.id)
			return
		}
	}
	m.focused = live[0]
	m.control.setFocused(m.focused.id)
}

// findPaneAt returns the leaf pane at terminal coordinates (row, col).
// Caller must NOT hold treeMu.
func (m *Magmux) findPaneAt(row, col int) *Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.findPaneAtLocked(row, col)
}

// findPaneAtLocked is the twin for callers already holding treeMu.
func (m *Magmux) findPaneAtLocked(row, col int) *Pane {
	return findPaneAtRecursive(m.root, row, col)
}

// focusedPane resolves m.focused to a pointer the caller can use once treeMu
// is released. Every keystroke path goes through it: writePTY blocks on the
// PTY, and rule 1 says the lock must not be held across that.
//
// Caller must NOT hold treeMu.
func (m *Magmux) focusedPane() *Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	if m.focused != nil && m.focused.closed {
		return nil
	}
	return m.focused
}

// typeToFocused delivers raw keystrokes to whichever pane has focus, or drops
// them if the layout has none left. Caller must NOT hold treeMu.
func (m *Magmux) typeToFocused(data []byte) {
	if p := m.focusedPane(); p != nil {
		p.writePTY(data)
	}
}

func findPaneAtRecursive(p *Pane, row, col int) *Pane {
	if p == nil {
		return nil
	}
	// Check if point is inside this pane's bounds
	if row < p.y || row >= p.y+p.h || col < p.x || col >= p.x+p.w {
		return nil
	}
	if p.splitType == SplitNone {
		return p
	}
	if found := findPaneAtRecursive(p.child1, row, col); found != nil {
		return found
	}
	return findPaneAtRecursive(p.child2, row, col)
}

// themeReplyWindow is how long after the OSC 11 query inputLoop keeps
// swallowing the terminal's answer to it.
//
// detectTheme waits themeProbeTimeout (150ms) and then gives up, but giving up
// is a decision about the palette, not about the bytes: a terminal behind an
// ssh hop or another multiplexer can answer a second or two later, and those
// bytes land in stdin, where inputLoop would type them into the focused pane as
// if the user had. magmux asked the question, so magmux eats the answer
// whenever it arrives.
//
// Three seconds is the bound: it is an order of magnitude over the slowest
// round trip a terminal reply plausibly takes, and short enough that a user
// cannot reach it by hand — the window only ever swallows a *well formed* OSC
// 11 reply, and typing one of those on purpose within three seconds of startup
// is not a thing that happens. Past the window every byte is forwarded exactly
// as before.
const themeReplyWindow = 3 * time.Second

// osc11MaxHold bounds how many bytes inputLoop will hold back while it decides
// whether it is looking at an OSC 11 reply. A real one is under 30 bytes
// ("\x1b]11;rgb:ffff/ffff/eeee\x1b\\" is 27); anything longer is not a reply,
// and holding it would let an odd or hostile stream park the user's input
// indefinitely by opening a sequence it never terminates.
const osc11MaxHold = 64

// takeLateOSC11 classifies the head of the input buffer during the theme-reply
// window. It returns the number of bytes to DISCARD (a complete reply), or
// hold=true meaning buf is a strict prefix of one and the decision has to wait
// for more bytes — which is what stops half a reply being forwarded when it
// spans two reads.
//
// It is deliberately narrow. Only ESC ] 1 1 ; qualifies, only with a printable
// body, and only terminated by BEL or ST. Every other OSC, every other escape
// sequence and every ordinary keystroke returns (0, false) and is handled by
// the untouched path below it: swallowing real input would be a far worse bug
// than the one this fixes.
func takeLateOSC11(buf []byte) (n int, hold bool) {
	const prefix = "\x1b]11;"
	if len(buf) < 2 {
		// A BARE ESC is never held, and that is a deliberate asymmetry. ESC is
		// a key the user can press, and in a finished grid it is the key that
		// dismisses the window; holding it until some other byte happens to
		// arrive makes magmux look hung. Nothing is lost by letting it
		// through: the loop below already parks a lone ESC waiting for the
		// rest of its sequence, so a reply split after its first byte is still
		// reassembled here on the next read — everywhere except the finished
		// grid, where ESC has always meant quit and still does.
		return 0, false
	}
	if len(buf) < len(prefix) {
		// A partial opening is held; anything else is somebody's keystroke.
		return 0, strings.HasPrefix(prefix, string(buf))
	}
	if !strings.HasPrefix(string(buf), prefix) {
		return 0, false
	}
	for i := len(prefix); i < len(buf); i++ {
		switch c := buf[i]; {
		case c == '\x07': // BEL terminator
			return i + 1, false
		case c == '\x1b': // ST terminator, ESC \
			if i+1 >= len(buf) {
				return 0, len(buf) < osc11MaxHold
			}
			if buf[i+1] == '\\' {
				return i + 2, false
			}
			return 0, false // ESC anything-else: not a reply
		case c < 0x20 || c > 0x7e:
			return 0, false // a control byte in the body: not a reply
		}
	}
	// Well formed so far, no terminator yet.
	return 0, len(buf) < osc11MaxHold
}

func (m *Magmux) inputLoop() {
	// Buffered input reader — accumulates partial reads so escape sequences
	// that span multiple read() calls are handled correctly.
	inbuf := make([]byte, 0, 4096)
	commandMode := false

	// The theme-reply window, resolved once. Zero if the probe never ran, in
	// which case nothing is ever swallowed and this loop behaves exactly as it
	// always has. Local rather than a field because only this goroutine cares,
	// and it latches shut on the first byte that arrives after it expires.
	var swallowUntil time.Time
	if !m.themeAskedAt.IsZero() {
		swallowUntil = m.themeAskedAt.Add(themeReplyWindow)
	}

	// Startup input first. The theme probe (init()) is the only other reader
	// stdin ever has, and anything it read that was not the terminal's OSC 11
	// reply is a keystroke that has not been handled yet. It goes in front of
	// everything that arrives from here on, and it is handled WITHOUT waiting
	// for the next key: a `q` typed into a finished grid during startup must
	// quit then, not when something else is pressed.
	inbuf = append(inbuf, m.pendingInput...)
	m.pendingInput = nil
	pending := len(inbuf) > 0

	// Stdin is read on a background goroutine so the main loop can also wake
	// on m.quit. Without this, a renderLoop-driven close(m.quit) (e.g. -w
	// auto-exit) cannot unblock the main goroutine, and magmux hangs.
	//
	// Resolved once, here, rather than dereferenced per read: this goroutine
	// outlives the loop below (it can still be parked in Read when m.quit
	// closes), so reading a variable somebody else might reassign is a race
	// with no upside.
	in := m.stdin
	if in == nil {
		in = os.Stdin
	}
	stdinCh := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				stdinCh <- chunk
			}
			if err != nil {
				close(stdinCh)
				return
			}
		}
	}()

	for {
		var chunk []byte
		if pending {
			// One pass over the startup bytes, then never again: if they were
			// an incomplete escape sequence the inner loop leaves them in
			// inbuf and we block for the rest, exactly as for live input.
			pending = false
		} else {
			select {
			case <-m.quit:
				return
			case c, ok := <-stdinCh:
				if !ok {
					return
				}
				chunk = c
			}
		}
		inbuf = append(inbuf, chunk...)

		for len(inbuf) > 0 {
			b := inbuf[0]

			if commandMode {
				commandMode = false
				m.armChord(false)
				switch b {
				case 'q':
					m.quitOnce.Do(func() { close(m.quit) })
					return
				case '\t', 'o':
					m.focusNext()
				case 'p':
					m.togglePanel()
				case 's':
					m.toggleStatusBar()
				case '[':
					// tmux's copy-mode binding, and an ACTION rather than a
					// toggle: it scrolls back one page, which both enters the
					// mode and shows something. A toggle that entered a mode
					// with an unchanged screen would look like a key that did
					// nothing.
					m.treeMu.RLock()
					page := 20
					if f := m.focused; f != nil {
						page = scrollPage(f.h)
					}
					m.treeMu.RUnlock()
					m.scrollFocusedBy(page)
				default:
					m.typeToFocused([]byte{0x07, b})
				}
				inbuf = inbuf[1:]
				continue
			}

			// The terminal's late answer to our own OSC 11 question, eaten
			// before anything downstream can mistake it for typing. It has to
			// come before the auto-close cancel below as well as before the
			// pane write: a reply that dismissed the completion countdown
			// would be the same bug wearing a different hat.
			if !swallowUntil.IsZero() {
				if time.Now().After(swallowUntil) {
					swallowUntil = time.Time{} // latched shut; never checked again
				} else if n, hold := takeLateOSC11(inbuf); n > 0 {
					inbuf = inbuf[n:]
					if dbgFile != nil {
						fmt.Fprintf(dbgFile, "[input] swallowed a late OSC 11 reply (%d bytes)\n", n)
					}
					continue
				} else if hold {
					// Only part of a reply so far. Wait for the rest rather
					// than forwarding a fragment we would then have to
					// un-send.
					goto needMore
				}
			}

			// Any keystroke cancels an armed auto-close: someone is here and
			// reading, so the window must not disappear under them.
			//
			// Read under RLock and escalate only when it is actually armed.
			// The countdown needs -x AND a pilot's finish, so in every other
			// run this is the zero value and taking the WRITE lock to
			// re-zero it — once per keystroke — put a queued writer in front
			// of the render loop's next RLock for no reason at all. Go's
			// RWMutex blocks later readers behind a waiting writer, so that
			// one keystroke cost a whole frame of latency.
			m.treeMu.RLock()
			armed := !m.closeAt.IsZero()
			m.treeMu.RUnlock()
			if armed {
				m.treeMu.Lock()
				m.closeAt = time.Time{}
				m.treeMu.Unlock()
				m.control.cancelClose()
			}

			// Keys aimed at the control panel scroll it. The panel has no PTY,
			// so without this every key pressed while it is focused is silently
			// swallowed and the pane looks broken.
			if f := m.focusedPane(); f != nil && f.isControl {
				if n := m.consumeControlKey(inbuf); n > 0 {
					inbuf = inbuf[n:]
					continue
				}
			}

			// Grid mode: when all panes are done (dead or idle), q/Esc/Ctrl-C exits
			if m.gridMode && m.allPanesDone() {
				if b == 0x03 || b == 'q' || b == 0x1b { // Ctrl-C, q, or Esc
					m.quitOnce.Do(func() { close(m.quit) })
					return
				}
				inbuf = inbuf[1:]
				continue
			}

			if b == commandKey&0x1f { // Ctrl-G
				commandMode = true
				m.armChord(true)
				inbuf = inbuf[1:]
				continue
			}

			// Scroll mode. It comes AFTER the Ctrl-G arm above, so the prefix —
			// and therefore quitting — keeps working while a pane is scrolled
			// back, and before the escape parsing below, so arrows and PgUp/PgDn
			// move the viewport instead of reaching the child. It is entered
			// only by Ctrl-G [ or the wheel, so nothing here can steal a key
			// from a TUI that has not been deliberately put aside.
			if m.focusedScrollOff() > 0 {
				if n := m.consumeScrollKey(inbuf); n > 0 {
					inbuf = inbuf[n:]
					continue
				}
				if len(inbuf) < 4 {
					goto needMore
				}
			}

			// ESC — could be start of mouse sequence or other escape
			if b == 0x1b {
				consumed, handled := m.tryParseEscape(inbuf)
				if consumed > 0 {
					inbuf = inbuf[consumed:]
					_ = handled
					continue
				}
				// Not enough data yet — might be partial escape sequence.
				// If this is the only data, wait for more. If there's plenty
				// of data and it's not a recognized sequence, pass it through.
				if len(inbuf) < 3 {
					// Need more data — break and read again
					goto needMore
				}
				// Not a recognized escape sequence, pass ESC through
				m.typeToFocused(inbuf[:1])
				inbuf = inbuf[1:]
				continue
			}

			// Regular byte — pass through to focused pane
			// Find the extent of non-escape bytes to batch-write
			end := 1
			for end < len(inbuf) && inbuf[end] != 0x1b && inbuf[end] != commandKey&0x1f {
				end++
			}
			m.typeToFocused(inbuf[:end])
			inbuf = inbuf[end:]
		}
		continue
	needMore:
		// Keep remaining bytes in inbuf, read more
	}
}

// tryParseEscape attempts to parse an escape sequence starting at buf[0]==ESC.
// Returns (bytes consumed, true) if handled, (0, false) if incomplete/not recognized.
func (m *Magmux) tryParseEscape(buf []byte) (int, bool) {
	if len(buf) < 2 {
		return 0, false // need more data
	}

	// CSI sequence: ESC [
	if buf[1] == '[' {
		if len(buf) < 3 {
			return 0, false // need more
		}

		// SGR mouse: ESC [ < params M/m
		if buf[2] == '<' {
			return m.parseSGRMouse(buf)
		}

		// Other CSI sequences (arrow keys, function keys, etc.)
		// Find the terminator: a byte in 0x40-0x7e range
		end := 2
		for end < len(buf) {
			if buf[end] >= 0x40 && buf[end] <= 0x7e {
				// Complete CSI sequence — forward to focused pane
				m.typeToFocused(buf[:end+1])
				return end + 1, true
			}
			end++
		}
		return 0, false // incomplete CSI
	}

	// OSC or other ESC sequences — forward as-is
	// ESC + single char (like ESC O for SS3)
	if buf[1] == 'O' {
		if len(buf) < 3 {
			return 0, false
		}
		m.typeToFocused(buf[:3])
		return 3, true
	}

	// Default: ESC + char, forward both
	m.typeToFocused(buf[:2])
	return 2, true
}

// ── Selection state (matches MTM's sel_* globals) ─────────────────────────────

type Selection struct {
	active bool
	pane   *Pane
	sy, sx int // start (pane-relative)
	ey, ex int // end (pane-relative)
}

// sel is package-level state, so it is treeMu-guarded like the rest of the
// layout: sel.pane is a *Pane that close_pane can detach at any moment, and
// the renderer reads the whole struct on every frame.
var sel Selection

// selClear drops the current selection. Caller holds treeMu.Lock.
func (m *Magmux) selClear() {
	sel.active = false
	sel.pane = nil
}

// selTextLocked extracts the selected text and clears the selection. It does
// NO I/O: putting the text on the clipboard runs pbcopy, and rule 1 forbids
// holding treeMu across that. Returns "" when there is nothing to copy.
//
// Caller holds treeMu.Lock.
func (m *Magmux) selTextLocked() string {
	if sel.pane == nil {
		return ""
	}
	s := sel.pane.screen

	// Normalize start/end
	sy, sx, ey, ex := sel.sy, sel.sx, sel.ey, sel.ex
	if sy > ey || (sy == ey && sx > ex) {
		sy, sx, ey, ex = ey, ex, sy, sx
	}

	// Extract text line by line from the screen buffer. A selection is up to
	// three segments — the anchor row from sx to the right edge, whole rows
	// between, and the cursor row from the left edge to ex — which is why this
	// is three rowsText calls and not one. rowsText clamps rows past s.rows,
	// so a selection left over from before a shrink drops them exactly as the
	// old `r < s.rows` loop guard did.
	var lines []string
	sel.pane.mu.Lock()
	if sy == ey {
		lines = s.rowsText(sy, sy+1, sx, ex)
	} else {
		lines = append(lines, s.rowsText(sy, sy+1, sx, s.cols-1)...)
		lines = append(lines, s.rowsText(sy+1, ey, 0, s.cols-1)...)
		lines = append(lines, s.rowsText(ey, ey+1, 0, ex)...)
	}
	sel.pane.mu.Unlock()

	content := strings.Join(lines, "\n")
	if content == "" {
		return ""
	}

	// Deselect after copy
	sel.pane = nil
	sel.active = false
	return content
}

// putClipboard pushes text to the terminal's clipboard. Blocking (it execs
// pbcopy), so it must run with no lock held.
func putClipboard(content string) {
	if content == "" {
		return
	}
	// Method 1: OSC 52 clipboard escape (works over SSH)
	encoded := encodeBase64(content)
	os.Stdout.WriteString(fmt.Sprintf("\x1b]52;c;%s\x07", encoded))

	// Method 2: pbcopy fallback (local macOS)
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(content)
	cmd.Run()
}

func encodeBase64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var buf strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var b0, b1, b2 byte
		b0 = data[i]
		if i+1 < len(data) {
			b1 = data[i+1]
		}
		if i+2 < len(data) {
			b2 = data[i+2]
		}
		buf.WriteByte(alphabet[b0>>2])
		buf.WriteByte(alphabet[((b0&3)<<4)|(b1>>4)])
		if i+1 < len(data) {
			buf.WriteByte(alphabet[((b1&0xf)<<2)|(b2>>6)])
		} else {
			buf.WriteByte('=')
		}
		if i+2 < len(data) {
			buf.WriteByte(alphabet[b2&0x3f])
		} else {
			buf.WriteByte('=')
		}
	}
	return buf.String()
}

// parseSGRMouse handles ESC [ < btn ; col ; row M/m
// Mouse events are consumed by magmux (never forwarded to children).
// Matches MTM behavior: click = focus, drag = selection, release = copy.
func (m *Magmux) parseSGRMouse(buf []byte) (int, bool) {
	// buf starts at ESC, buf[1]=='[', buf[2]=='<'
	end := 3
	for end < len(buf) {
		if buf[end] == 'M' || buf[end] == 'm' {
			break
		}
		if buf[end] < 0x20 || buf[end] > 0x7e {
			return end + 1, false
		}
		end++
	}
	if end >= len(buf) {
		return 0, false // incomplete
	}

	params := string(buf[3:end])
	press := buf[end] == 'M'

	var btn, col, row int
	parts := strings.Split(params, ";")
	if len(parts) >= 1 {
		fmt.Sscanf(parts[0], "%d", &btn)
	}
	if len(parts) >= 2 {
		fmt.Sscanf(parts[1], "%d", &col)
	}
	if len(parts) >= 3 {
		fmt.Sscanf(parts[2], "%d", &row)
	}

	row0 := row - 1 // 0-indexed
	col0 := col - 1
	termChar := buf[end]

	// All of the state below (m.focused, sel, pane geometry) is treeMu's, so
	// the whole decision runs under one write lock and the two blocking
	// consequences — forwarding to a PTY, and putting text on the clipboard —
	// are deferred until after it is released.
	var (
		scrollPanel int    // 0 = no, else lines to scroll
		forwardTo   *Pane  // alt-screen mouse forwarding target
		forward     string // the sequence to forward
		copyText    string
	)

	m.treeMu.Lock()

	// Always: left click press switches focus (even in alt mode)
	if press && btn == 0 {
		if target := m.findPaneAtLocked(row0, col0); target != nil {
			m.focused = target
			m.control.setFocused(target.id)
		}
	}

	// Wheel over the control panel scrolls its exchange, whichever pane has
	// focus. Checked before the alt-screen forward below: the panel has no
	// PTY, so forwarding would drop the event entirely.
	if btn == 64 || btn == 65 {
		if target := m.findPaneAtLocked(row0, col0); target != nil && target.isControl {
			if btn == 64 {
				scrollPanel = 3
			} else {
				scrollPanel = -3
			}
		}
	}

	if scrollPanel == 0 {
		// If focused pane is in alternate screen (vim, htop, Claude Code,
		// OpenCode), forward ALL mouse events to it — like tmux does.
		if f := m.focused; f != nil && f.altMode {
			localRow := maxInt(1, row0-f.y+1)
			localCol := maxInt(1, col0-f.x+1)
			forwardTo = f
			forward = fmt.Sprintf("\x1b[<%d;%d;%d%c", btn, localCol, localRow, termChar)
		} else {
			// Normal mode (bash, etc.): handle mouse ourselves for selection
			switch {
			case btn == 64 || btn == 65: // wheel up / down
				// Scrolling a non-alt pane with the wheel is what every terminal
				// does and it cannot collide with anything: the alt-screen
				// branch above already claimed the wheel for TUIs, and this
				// branch is only reached when the focused pane is not one. It
				// targets the pane UNDER THE POINTER rather than the focused
				// one, like the panel's wheel handling directly above.
				target := m.findPaneAtLocked(row0, col0)
				if target == nil {
					target = m.focused
				}
				if target != nil && !target.isControl && target.screen != nil {
					target.mu.Lock()
					if !target.altMode && (target.screen.sbLen > 0 || target.screen.sbOff > 0) {
						if btn == 64 {
							target.screen.scrollBackBy(3)
						} else {
							target.screen.scrollBackBy(-3)
						}
						target.dirty = true
					}
					target.mu.Unlock()
				}

			case press && btn == 0: // Left click → start selection
				m.selClear()
				if f := m.focused; f != nil {
					sel.pane = f
					sel.active = true
					sel.sy = row0 - f.y
					sel.sx = col0 - f.x
					sel.ey = sel.sy
					sel.ex = sel.sx
				}

			case press && btn == 32: // Drag
				if sel.active && sel.pane != nil {
					sel.ey = clamp(row0-sel.pane.y, 0, maxInt(0, sel.pane.h-1))
					sel.ex = clamp(col0-sel.pane.x, 0, maxInt(0, sel.pane.w-1))
				}

			case !press && btn == 0: // Release → copy
				if sel.active && sel.pane != nil {
					sel.ey = clamp(row0-sel.pane.y, 0, maxInt(0, sel.pane.h-1))
					sel.ex = clamp(col0-sel.pane.x, 0, maxInt(0, sel.pane.w-1))
					if sel.sy != sel.ey || sel.sx != sel.ex {
						copyText = m.selTextLocked()
					}
					sel.active = false
				}
			}
		}
	}

	m.treeMu.Unlock()

	if scrollPanel != 0 {
		m.control.scrollBy(scrollPanel)
		return end + 1, true
	}
	if forwardTo != nil {
		forwardTo.writePTY([]byte(forward))
		return end + 1, true
	}
	putClipboard(copyText)

	return end + 1, true
}

// ── Grid File Parser ─────────────────────────────────────────────────────────

func parseGridFile(path string) ([]PaneConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open grid file: %w", err)
	}
	defer f.Close()

	shell := getUserShell()
	var cmds []PaneConfig
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmds = append(cmds, PaneConfig{
			Cmd:  shell,
			Args: []string{"-l", "-c", line},
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read grid file: %w", err)
	}
	return cmds, nil
}

// ── Unix Domain Socket IPC ──────────────────────────────────────────────────

type sockMsg struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Pane  any    `json:"pane,omitempty"` // int or "*"
	Color string `json:"color,omitempty"`
	Style string `json:"style,omitempty"`
	// Agent hook event fields (type="agent")
	Event            string `json:"event,omitempty"`             // hook event name
	Tool             string `json:"tool,omitempty"`              // tool name from PreToolUse
	Prompt           string `json:"prompt,omitempty"`            // from UserPromptSubmit
	Project          string `json:"project,omitempty"`           // project name
	NotificationType string `json:"notification_type,omitempty"` // idle_prompt, permission_prompt, etc.
	// Controlled-session fields (type="send" / type="pilot")
	Keys    []string `json:"keys,omitempty"`    // named keys to press after Text
	Enter   *bool    `json:"enter,omitempty"`   // submit after Text; defaults true
	Label   string   `json:"label,omitempty"`   // short tag for the control log ("step 2/5")
	Goal    string   `json:"goal,omitempty"`    // the task the pilot is driving
	Steps   int      `json:"steps,omitempty"`   // planned step count, 0 if open-ended
	Model   string   `json:"model,omitempty"`   // model the pilot itself is running
	Summary string   `json:"summary,omitempty"` // pilot's closing summary
	// Client is the controller's identity for the panel header
	// ("claude-code/2.1"). The ONE field MCP adds to the pilot protocol —
	// everything else an MCP client does arrives as an ordinary socket verb.
	Client string `json:"client,omitempty"`
	// Request/response fields. All optional and purely additive — a message
	// that omits them behaves exactly as it did before they existed.
	//
	// ID is json.RawMessage rather than a string so a numeric or string id
	// round-trips verbatim: a client that sent 7 gets 7 back, not "7".
	// Presence of ID is the *only* thing that makes a message answerable.
	ID        json.RawMessage `json:"id,omitempty"`
	Lines     int             `json:"lines,omitempty"`     // capture: keep the last N rows
	Offset    int             `json:"offset,omitempty"`    // capture: rows of scrollback to reach back through
	Cursor    bool            `json:"cursor,omitempty"`    // capture: caller wants the cursor position
	Cmd       string          `json:"cmd,omitempty"`       // pane lifecycle: command to run
	Cwd       string          `json:"cwd,omitempty"`       // pane lifecycle: working directory
	Dir       string          `json:"dir,omitempty"`       // pane lifecycle: synonym for cwd
	Env       []string        `json:"env,omitempty"`       // pane lifecycle: extra KEY=VALUE entries
	Target    any             `json:"target,omitempty"`    // open_pane: pane to split; absent = focused
	Direction string          `json:"split,omitempty"`     // open_pane: auto | horizontal | vertical
	Ratio     float64         `json:"ratio,omitempty"`     // open_pane: first half's share, 0 = 0.5
	Force     bool            `json:"force,omitempty"`     // pane lifecycle: escalate to SIGKILL
	Focus     *bool           `json:"focus,omitempty"`     // pane lifecycle: focus the result
	TimeoutMs int             `json:"timeoutMs,omitempty"` // per-request budget, 0 = the verb's default
}

// validSocketID reports whether a --id NAME may be interpolated into the
// socket path. Restricted to [A-Za-z0-9_-]+ so a name can carry no path
// separator and no "..": the socket is created with this process's own
// privileges, and `--id ../../home/me/.ssh/agent` must not be able to reach
// out of /tmp and unlink something. The length cap keeps the result inside the
// ~104-byte sun_path limit on darwin.
func validSocketID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// socketPath is where the IPC socket is bound. The pid-based default is
// documented as stable and stays byte-identical; --id only substitutes the
// name, which is what lets a caller know the path *before* magmux starts and
// poll for it rather than having to discover a pid.
func (m *Magmux) socketPath() string {
	if m.sockID != "" {
		return "/tmp/magmux-" + m.sockID + ".sock"
	}
	return fmt.Sprintf("/tmp/magmux-%d.sock", os.Getpid())
}

func (m *Magmux) socketServer() {
	sockPath := m.socketPath()
	m.sockPath = sockPath

	// Set env so children inherit it
	os.Setenv("MAGMUX_SOCK", sockPath)

	// Clean up stale socket
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "socket listen error: %v\n", err)
		}
		m.markSocketDone() // nothing to tear down; don't make main wait
		return
	}

	// Cleanup on exit
	go func() {
		<-m.quit
		// Push a final aggregated results event with the last-known state of
		// every pane. Subscribers (e.g. claudish) use this as the authoritative
		// final state — no file-based fallback needed.
		results := map[string]any{
			"type":    "results",
			"panes":   m.buildPaneResults(),
			"endedAt": time.Now().UTC().Format(time.RFC3339),
		}
		shutdown := map[string]any{"type": "shutdown"}
		// Record before broadcasting. From this point a connection either wins
		// the race and is registered (so the broadcasts below reach it), or it
		// arrives after and is replayed these same events by handleSocketConn.
		// Both paths deliver exactly once, because a client that sees
		// finalEvents is never added to sockClients.
		m.recordFinalEvents(results, shutdown)
		m.broadcastEvent(results)
		// Push a shutdown event so clients know the socket is closing.
		m.broadcastEvent(shutdown)
		// Deterministically flush and close each subscriber connection instead
		// of racing a fixed drain sleep. Closing the connection after the two
		// broadcasts above (which write synchronously under sockClientsMu) gives
		// each subscriber a clean EOF *after* it has received the final results
		// — the ordering guarantee integrators rely on. Bound the whole teardown
		// so a single wedged subscriber can't hang magmux's exit.
		m.closeSockClients(2 * time.Second)
		ln.Close()
		os.Remove(sockPath)
		m.markSocketDone()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go m.handleSocketConn(conn)
	}
}

func (m *Magmux) handleSocketConn(conn net.Conn) {
	// Wait for the layout before serving anything on this connection. The
	// listener is bound before the first child forks, on purpose — MAGMUX_SOCK
	// must be in that child's environment — so connections can and do arrive
	// while m.root and m.allPanes are still nil. Serving one there writes an
	// EMPTY aggregate snapshot as the connection's first line, and a subscriber
	// seeds its whole pane map from that line, so it would then wait forever
	// for per-pane snapshots that only ever fire on change.
	//
	// Done BEFORE sockClientsMu is taken: that lock is what makes the write and
	// the registration below atomic against the shutdown broadcast, and holding
	// it across a wait would block every other client's `results`.
	m.waitLayoutReady(layoutReadyTimeout)

	// Immediately send the current pane-state snapshot so a subscriber that
	// connects *after* some panes have already exited still receives full
	// state (the live exit/snapshot events it missed are folded into this one
	// aggregate event). Written before registering the connection for
	// broadcasts so it is always the first line this subscriber sees.
	snapshot := map[string]any{
		"type":  "snapshot",
		"panes": m.buildPaneResults(),
	}
	// NOTE: distinct from the per-pane live "snapshot" event (which carries a
	// singular "pane" field). This connect-time aggregate carries a "panes"
	// array — subscribers disambiguate on that field.
	//
	// Build the payload BEFORE taking sockClientsMu: buildPaneResults locks
	// each pane, and the established lock order is p.mu -> sockClientsMu
	// (pollControllers releases p.mu before broadcasting). Taking them the
	// other way round here would invert it.
	data, err := json.Marshal(snapshot)
	if err != nil {
		conn.Close()
		return
	}
	data = append(data, '\n')

	// Write the aggregate and register for broadcasts ATOMICALLY, under the
	// same lock broadcastEvent uses. Writing first and registering after left
	// a window in which a shutdown broadcast (results/shutdown) could run
	// against a client list that did not yet contain this connection: the
	// subscriber then saw a clean EOF with no `results` event, violating the
	// ordering guarantee integrators rely on. That was rare but real — it is
	// what made TestSocketSubscriberContract flaky.
	m.sockClientsMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = conn.Write(data)
	_ = conn.SetWriteDeadline(time.Time{})
	if len(m.finalEvents) > 0 {
		// Teardown already began, so this connection will never be broadcast
		// to. Replay the final events it missed, then give it a clean EOF.
		// Registering instead would either lose `results` or leave the
		// connection dangling.
		for _, ev := range m.finalEvents {
			_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			_, _ = conn.Write(ev)
		}
		m.sockClientsMu.Unlock()
		conn.Close()
		return
	}
	m.sockClients = append(m.sockClients, conn)
	m.sockClientsMu.Unlock()

	defer func() {
		conn.Close()
		m.sockClientsMu.Lock()
		for i, c := range m.sockClients {
			if c == conn {
				m.sockClients = append(m.sockClients[:i], m.sockClients[i+1:]...)
				break
			}
		}
		m.sockClientsMu.Unlock()
	}()

	scanner := bufio.NewScanner(conn)
	// Raise the token limit off the 64KB default. An oversized line is not
	// skipped: Scan returns false and this loop ends, so one long message
	// (a pasted instruction, a large payload) silently drops the *whole*
	// client connection rather than one line — the subscriber stops receiving
	// broadcasts with no error anywhere. 4MB matches the transcript scanners
	// in controller_claude.go.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	// Whether this connection ever DROVE anything, as opposed to subscribing
	// and tinting. It is the only thing the panel needs to report a controller
	// arriving and going away: the fd close below is already the disconnect.
	// An operator staring at a frozen panel needs to know the controller went
	// away rather than got slow.
	driving := false
	for scanner.Scan() {
		line := scanner.Text()
		var msg sockMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if m.handleSocketMsg(msg, conn) && !driving {
			driving = true
			m.control.noteController(true)
		}
	}
	if driving {
		m.control.noteController(false)
	}
}

// layoutReadyTimeout bounds how long a connection waits for the layout. It
// exists so a magmux that fails to build one (or dies between binding the
// socket and finishing startup) hands its client an honest `not_ready` instead
// of a connection that never says anything — a hang is the one failure mode an
// agent cannot report on.
const layoutReadyTimeout = 5 * time.Second

// markLayoutReady publishes the layout to the socket. Safe to call repeatedly,
// and called from a defer in main so no failure path can leave a connection
// parked on the channel while magmux exits underneath it.
func (m *Magmux) markLayoutReady() {
	if m.layoutReady == nil {
		return
	}
	m.layoutReadyOnce.Do(func() { close(m.layoutReady) })
}

// layoutIsReady is the non-blocking form, for a verb that has already waited.
func (m *Magmux) layoutIsReady() bool {
	if m.layoutReady == nil {
		return true
	}
	select {
	case <-m.layoutReady:
		return true
	default:
		return false
	}
}

// waitLayoutReady blocks until the layout exists, magmux starts shutting down,
// or the timeout expires; it reports whether the layout is actually there.
//
// Callers must hold NO lock — least of all sockClientsMu, which serialises
// every broadcast: waiting under it would stall the shutdown broadcast that
// delivers `results` behind a client that connected during startup.
func (m *Magmux) waitLayoutReady(timeout time.Duration) bool {
	if m.layoutIsReady() {
		return true
	}
	select {
	case <-m.layoutReady:
		return true
	case <-m.quit:
		// Teardown never waits on a subscriber. The connection is still served:
		// handleSocketConn's finalEvents replay is what gives it results →
		// shutdown → EOF, and that path must stay reachable.
		return false
	case <-time.After(timeout):
		return false
	}
}

// markSocketDone signals that socket teardown is complete (or will never
// happen). Safe to call repeatedly.
func (m *Magmux) markSocketDone() {
	if m.sockDone == nil {
		return
	}
	m.sockDoneOnce.Do(func() { close(m.sockDone) })
}

// waitSocketShutdown blocks until the socket server has flushed its final
// results/shutdown broadcasts and closed every subscriber, or until timeout.
//
// Without this, main can return from inputLoop and exit while the teardown
// goroutine is still running, so subscribers see the connection drop with no
// `results` event at all — the ordering guarantee integrators rely on,
// broken by process exit rather than by anything on the socket path. Bounded
// so a wedged subscriber cannot stop magmux from exiting.
func (m *Magmux) waitSocketShutdown(timeout time.Duration) {
	if m.sockDone == nil {
		return
	}
	select {
	case <-m.sockDone:
	case <-time.After(timeout):
	}
}

// recordFinalEvents marshals the shutdown payloads and stores them so a
// subscriber connecting during or after teardown can still be given them.
// Must be called before the corresponding broadcasts.
func (m *Magmux) recordFinalEvents(events ...any) {
	var encoded [][]byte
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		encoded = append(encoded, append(data, '\n'))
	}
	m.sockClientsMu.Lock()
	m.finalEvents = encoded
	m.sockClientsMu.Unlock()
}

// broadcastEvent serializes an event as JSON and pushes it to all connected
// socket clients. Best-effort: failed writes drop the client silently.
func (m *Magmux) broadcastEvent(event any) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	data = append(data, '\n')

	m.sockClientsMu.Lock()
	defer m.sockClientsMu.Unlock()
	// Iterate in reverse so we can splice out dead clients.
	for i := len(m.sockClients) - 1; i >= 0; i-- {
		c := m.sockClients[i]
		_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.Write(data); err != nil {
			c.Close()
			m.sockClients = append(m.sockClients[:i], m.sockClients[i+1:]...)
		}
	}
}

// closeSockClients flushes and closes every connected subscriber connection so
// each receives a clean EOF *after* the final results/shutdown broadcasts. The
// broadcasts run synchronously under sockClientsMu with a per-write deadline, so
// by the time this runs the payload has already been written to the OS socket
// buffer; closing the fd then signals end-of-stream. Bounded by timeout so a
// wedged peer (never draining its receive buffer) can't block magmux's exit.
func (m *Magmux) closeSockClients(timeout time.Duration) {
	m.sockClientsMu.Lock()
	conns := make([]net.Conn, len(m.sockClients))
	copy(conns, m.sockClients)
	m.sockClients = nil
	m.sockClientsMu.Unlock()

	if len(conns) == 0 {
		return
	}

	done := make(chan struct{})
	go func() {
		for _, c := range conns {
			// A write deadline in the past forces any buffered write to flush or
			// error immediately rather than block, then Close signals EOF.
			_ = c.SetWriteDeadline(time.Now().Add(timeout))
			_ = c.Close()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		// Best effort: force-close whatever is left so nothing lingers.
		for _, c := range conns {
			_ = c.Close()
		}
	}
}

// dispatchSocketMsg runs one verb and reports its outcome. Behaviour when no
// reply was requested is unchanged: a caller that ignores both returns gets
// exactly the old semantics, which is what every legacy client relies on.
func (m *Magmux) dispatchSocketMsg(msg sockMsg) (map[string]any, error) {
	return m.dispatchSocketVerb(msg, nil)
}

// dispatchSocketVerb runs one verb.
//
// done, if non-nil, is how a verb whose work outlives this call answers: it
// returns errReplyDeferred and calls done exactly once, later, on whatever
// goroutine finished the work. Only `send` does that — its writes are paced
// across hundreds of milliseconds and the socket reader must not block on them.
// A nil done means nobody is listening, so those verbs keep their old
// fire-and-forget shape.
func (m *Magmux) dispatchSocketVerb(msg sockMsg, done func(map[string]any, error)) (map[string]any, error) {
	switch msg.Type {
	case "capabilities":
		return m.sockCapabilities()

	case "list":
		return m.sockList()

	case "capture":
		return m.sockCapture(msg)

	case "open_pane":
		return m.sockOpenPane(msg)

	case "close_pane":
		return m.sockClosePane(msg)

	case "focus":
		return m.sockFocus(msg)

	case "status":
		m.treeMu.Lock()
		m.statusText = msg.Text
		// Force a redraw
		for _, p := range m.livePanesLocked(nil) {
			p.mu.Lock()
			p.dirty = true
			p.mu.Unlock()
		}
		m.treeMu.Unlock()
		return nil, nil

	case "tint":
		color := msg.Color
		if color == "reset" {
			color = ""
		}
		paneIdx := m.parsePaneIndex(msg.Pane)
		if paneIdx == paneAll {
			// "*" — apply to every LIVE pane. A tombstoned pane is not on
			// screen, so tinting it would be a write nobody can ever see.
			for _, p := range m.livePanes() {
				p.mu.Lock()
				p.tint = color
				p.dirty = true
				p.mu.Unlock()
			}
			return nil, nil
		}
		p, err := m.paneForMsg(paneIdx)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.tint = color
		p.dirty = true
		p.mu.Unlock()
		return nil, nil

	case "overlay":
		paneIdx := m.parsePaneIndex(msg.Pane)
		p, err := m.paneForMsg(paneIdx)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.overlayText = msg.Text
		p.overlayStyle = msg.Style
		p.dirty = true
		p.mu.Unlock()
		return nil, nil

	case "send":
		// Drive a pane from outside — the inbound half of a controlled
		// session. Defaults to the pilot's target pane so a pilot that has
		// announced itself need not repeat the index on every instruction.
		//
		// Only an *absent* pane takes that default. "*" and an unparseable
		// pane both refuse: there is no such thing as typing an instruction
		// into every pane, and guessing a target types someone's next
		// instruction into the wrong session, which is both expensive and
		// invisible.
		paneIdx := m.parsePaneIndex(msg.Pane)
		switch {
		case paneIdx == paneUnspecified:
			// One route means the single-session case, so this is free. Several
			// open routes means the default would be a guess, and targetPane
			// refuses rather than typing the next instruction into whichever
			// Claude Code session happened to be first.
			var err error
			if paneIdx, err = m.control.targetPane(); err != nil {
				return nil, err
			}
		case paneIdx == paneAll:
			return nil, sockErrf(sockCodeBadRequest, `send has no fan-out: "*" is not a target`)
		case paneIdx < 0:
			return nil, sockErrf(sockCodeBadRequest, "pane is not an index")
		}
		enter := true
		if msg.Enter != nil {
			enter = *msg.Enter
		}
		if done == nil {
			return nil, m.sendToPane(paneIdx, msg.Text, msg.Keys, enter, msg.Label, nil)
		}
		// Delivery outlives this call, so the reply does too: it means "the
		// bytes reached the PTY", which is the failure mode that used to live
		// and die inside sendToPane's goroutine.
		result := map[string]any{"pane": paneIdx, "bytes": len(msg.Text), "keys": len(msg.Keys), "enter": enter}
		if err := m.sendToPane(paneIdx, msg.Text, msg.Keys, enter, msg.Label, func(err error) {
			done(result, err)
		}); err != nil {
			return nil, err
		}
		return nil, errReplyDeferred

	case "pilot":
		return nil, m.dispatchPilotMsg(msg)

	case "agent":
		paneIdx := m.parsePaneIndex(msg.Pane)
		p, err := m.paneForMsg(paneIdx)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()

		oldStatus := p.agentStatus
		if msg.Project != "" {
			p.agentProject = msg.Project
		}

		// State machine: derive status from hook event (matches cctop transitions)
		switch msg.Event {
		case "UserPromptSubmit":
			p.agentStatus = "working"
			p.agentTool = ""
			if msg.Prompt != "" {
				p.agentPrompt = msg.Prompt
			}
		case "PreToolUse":
			p.agentStatus = "working"
			if msg.Tool != "" {
				p.agentTool = msg.Tool
			}
		case "PostToolUse", "PostToolUseFailure":
			p.agentStatus = "working"
		case "Stop":
			p.agentStatus = "waiting_input"
			p.agentTool = ""
		case "Notification":
			switch msg.NotificationType {
			case "idle_prompt":
				p.agentStatus = "waiting_input"
			case "permission_prompt":
				p.agentStatus = "waiting_permission"
			}
		case "PermissionRequest":
			p.agentStatus = "waiting_permission"
		case "PreCompact":
			p.agentStatus = "compacting"
		case "PostCompact":
			p.agentStatus = "idle"
		case "SessionStart":
			p.agentStatus = "idle"
			p.agentTool = ""
			p.agentPrompt = ""
		case "SessionEnd":
			p.agentStatus = ""
			p.agentProject = ""
			p.agentTool = ""
			p.agentPrompt = ""
		}

		newStatus := p.agentStatus
		p.dirty = true

		// Visual feedback for attention-needed states
		if newStatus == "waiting_input" || newStatus == "waiting_permission" {
			label := "INPUT"
			if newStatus == "waiting_permission" {
				label = "PERMISSION"
			}
			p.overlayText = fmt.Sprintf("⚡ AWAITING %s", label)
			p.overlayStyle = "error"
			p.tint = "red"
		} else if oldStatus == "waiting_input" || oldStatus == "waiting_permission" {
			// Clear attention indicators when agent resumes
			p.overlayText = ""
			p.overlayStyle = ""
			p.tint = ""
		}

		p.mu.Unlock()

		// Update aggregated status bar
		if oldStatus != newStatus {
			m.updateAgentStatusBar()
		}
		return nil, nil
	}

	// An unknown verb stays silent without an id, which is the behaviour every
	// client written before replies existed depends on — and is exactly what
	// makes `capabilities` a usable version probe: an older magmux answers a
	// verb it does not know with nothing at all.
	return nil, sockErrf(sockCodeUnknownVerb, "unknown verb %q", msg.Type)
}

// Sentinels parsePaneIndex returns in place of an index. All negative, so the
// existing `idx >= 0 && idx < len(m.allPanes)` bounds checks reject them
// without needing a special case.
const (
	paneAll         = -1 // "*" — fan out to every pane
	paneInvalid     = -2 // the field was present but is not a pane index
	paneUnspecified = -3 // the field was absent; the verb's own default applies
)

// parsePaneIndex resolves a socket message's `pane` field to a pane index or
// to one of the sentinels above.
//
// The string branch used to be fmt.Sscanf with its error dropped, which left
// idx at its zero value on failure — so {"pane":"api"} silently drove *pane 0*,
// the wrong session, with no diagnostic anywhere. strconv.Atoi rejects the
// whole string (Sscanf would have taken the "1" out of "1x"), so anything that
// is neither a bare integer nor "*" is paneInvalid and every caller drops it.
//
// An absent field is distinct from a bad one: it is how a pilot says "the pane
// I already announced", so it gets its own sentinel rather than sharing the one
// that means "refuse this message".
func (m *Magmux) parsePaneIndex(v any) int {
	switch val := v.(type) {
	case nil:
		return paneUnspecified
	case float64:
		return int(val)
	case string:
		s := strings.TrimSpace(val)
		if s == "*" {
			return paneAll
		}
		idx, err := strconv.Atoi(s)
		if err != nil {
			return paneInvalid
		}
		return idx
	}
	return paneInvalid
}

// TODO: Add native macOS kqueue-based file watcher (kqueue_darwin.go) for monitoring
// ~/.cctop/sessions/ as a fallback when agents don't send IPC messages directly.
// Also consider Linux inotify equivalent (inotify_linux.go).

// updateAgentStatusBar rebuilds the status bar from the agent hook state of
// every live pane. Caller must NOT hold treeMu.
func (m *Magmux) updateAgentStatusBar() {
	var parts []string
	needsAttention := 0

	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	live := m.livePanesLocked(nil)
	for _, p := range live {
		p.mu.Lock()
		status := p.agentStatus
		name := p.agentProject
		p.mu.Unlock()

		if status == "" {
			continue // not an agent pane
		}

		if name == "" {
			name = "agent"
		}
		if len(name) > 15 {
			name = name[:15]
		}

		var colorCode, icon string
		switch status {
		case "working":
			colorCode = "G"
			icon = "●"
		case "idle":
			colorCode = "D"
			icon = "○"
		case "waiting_input":
			colorCode = "R"
			icon = "⚡"
			needsAttention++
		case "waiting_permission":
			colorCode = "Y"
			icon = "⚠"
			needsAttention++
		case "compacting":
			colorCode = "C"
			icon = "◐"
		default:
			colorCode = "D"
			icon = "?"
		}
		parts = append(parts, fmt.Sprintf("%s:%s %s", colorCode, icon, name))
	}

	if len(parts) == 0 {
		return
	}

	statusText := strings.Join(parts, "\t")
	if needsAttention > 0 {
		statusText = fmt.Sprintf("R:⚡ %d NEED INPUT\t", needsAttention) + statusText
	}

	m.statusText = statusText
	for _, p := range live {
		p.mu.Lock()
		p.dirty = true
		p.mu.Unlock()
	}
}

// ── Status File Polling ─────────────────────────────────────────────────────

// ── Grid Mode Exit Handling ─────────────────────────────────────────────────

// reapChild waits for a pane's child to exit and records the outcome. It is
// the half of waitForChild that has nothing to do with presentation, and it
// must run for EVERY spawned child, in every mode, or the child becomes a
// zombie for the life of magmux:
//
//   - nobody else calls cmd.Wait. readLoop notices the PTY closing and sets
//     p.dead, which looks like reaping and is not — the process entry survives
//     until it is waited on.
//   - p.reaped stays false without it, so reapPane's delayed SIGKILL always
//     fires on the force path, and the check that stops that kill landing on a
//     recycled pid becomes decorative.
//
// It is safe to call on a pane that was never published (an OpenPane unwind):
// it touches only that pane's own fields.
//
// Reports whether it actually waited, so waitForChild can skip painting a
// tombstone for a pane that had no child.
func (m *Magmux) reapChild(p *Pane) bool {
	if p == nil || p.cmd == nil {
		return false
	}
	err := p.cmd.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	// The pid has now been collected and is free for the OS to reuse, so no
	// delayed force-kill may target its process group from here on.
	p.reaped = true
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			p.exitCode = exitErr.ExitCode()
		} else {
			p.exitCode = 1
		}
	} else {
		p.exitCode = 0
	}
	return true
}

// waitForChild reaps a pane's child and, in grid mode, turns its exit into the
// tombstone overlay and the `exit` event.
//
// The reaping is unconditional; only the PRESENTATION is grid-mode's. Outside
// grid mode there is no ✓ DONE overlay to paint and never has been — a plain
// multiplexer pane whose shell exits just goes quiet — but the child still has
// to be collected, which is why this is started for every pane rather than only
// for grid ones.
func (m *Magmux) waitForChild(p *Pane) {
	if !m.reapChild(p) {
		return
	}
	if !p.gridMode {
		return
	}
	p.mu.Lock()

	// Build completion overlay with duration + last output line
	var duration string
	if !p.startedAt.IsZero() {
		duration = formatDuration(time.Since(p.startedAt))
	}
	lastMsg := p.lastNonEmptyLine(40)

	var header string
	if p.exitCode == 0 {
		header = "\u2713 DONE"
		p.overlayStyle = "success"
		p.tint = "green"
	} else {
		header = fmt.Sprintf("\u2717 FAIL (exit %d)", p.exitCode)
		p.overlayStyle = "error"
		p.tint = "red"
	}

	var lines []string
	lines = append(lines, header)
	if duration != "" {
		lines = append(lines, "took "+duration)
	}
	if lastMsg != "" {
		lines = append(lines, lastMsg)
	}
	p.overlayText = strings.Join(lines, "\n")
	p.dirty = true
	// Capture fields for broadcast under the lock, then release before I/O.
	exitCode := p.exitCode
	snap := p.controllerSnap
	p.mu.Unlock()

	// Push an exit event over the IPC socket so subscribers (e.g. claudish)
	// learn about per-pane completion without polling files. p.id replaces the
	// old linear scan of m.allPanes: the id IS the index and never changes, so
	// the scan was both slower and — once panes can be appended concurrently —
	// a read of the slice header from the wrong goroutine.
	m.broadcastEvent(map[string]any{
		"type":     "exit",
		"pane":     p.id,
		"exitCode": exitCode,
		"duration": duration,
		"lastLine": lastMsg,
		"response": snap.LastResponse,
		"prompt":   snap.LastUserPrompt,
		"tool":     snap.LastTool,
		"model":    snap.Model,
	})
}

// buildPaneResults collects the state of every pane into a serializable slice.
// It backs three things — the connect-time aggregate `snapshot`, the shutdown
// `results` event, and the `list` verb — and that is deliberate: one code path
// means a client polling `list` and a subscriber reading `results` can never be
// told different things about the same pane.
//
// Fields beyond the original state/exitCode/dead set are added only when they
// can be sourced; an observer that cannot see a pane's pid is better served by
// the key being absent than by a zero. Unknown fields are documented as
// ignorable (README), so the additions are safe for existing subscribers.
func (m *Magmux) buildPaneResults() []map[string]any {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.buildPaneResultsLocked()
}

// buildPaneResultsLocked is the twin for callers already holding treeMu.
func (m *Magmux) buildPaneResultsLocked() []map[string]any {
	results := make([]map[string]any, 0, len(m.allPanes))
	focused := m.focused
	for _, p := range m.allPanes {
		if p == nil {
			continue
		}
		if p.closed {
			// Tombstones are reported, not omitted. Ids are sparse after a
			// close and a subscriber that treated results as a dense array
			// would otherwise silently shift every pane after the hole.
			entry := map[string]any{
				"pane": p.id, "state": "closed", "closed": true, "dead": true,
			}
			if p.label != "" {
				entry["label"] = p.label
			}
			results = append(results, entry)
			continue
		}
		if p.isControl {
			// Reported rather than omitted, so a subscriber walking results
			// still sees every pane index and can tell why this one has no
			// session state of its own.
			//
			// state stays "panel" whether the panel is on screen or hidden.
			// Hidden is not a state of the SESSION — there is no session — it
			// is a fact about magmux's own chrome, so it rides as its own
			// field. test/ui/case3.ts asserts state == "panel"; a magmux
			// started without -c must not answer that differently.
			results = append(results, map[string]any{
				"pane": p.id, "state": "panel", "control": true,
				"hidden": p.hidden,
			})
			continue
		}
		p.mu.Lock()
		snap := p.controllerSnap
		var state string
		switch {
		case p.dead && p.exitCode == 0:
			state = "completed"
		case p.dead && p.exitCode != 0:
			state = "failed"
		case p.inputReady:
			state = "awaiting_input"
		default:
			state = "running"
		}
		entry := map[string]any{
			"pane":     p.id,
			"state":    state,
			"exitCode": p.exitCode,
			"dead":     p.dead,
			"focused":  p == focused,
			"altMode":  p.altMode,
		}
		if p.label != "" {
			entry["label"] = p.label
		}
		if s := p.screen; s != nil {
			// The geometry an observer needs to read the text `capture`
			// returns: without the width there is no telling a wrapped line
			// from a hard one.
			entry["rows"], entry["cols"] = s.rows, s.cols
		}
		if p.inputSignal != "" {
			// Which heuristic called the pane idle — "osc", "2004", "title",
			// "idle", "ctrl", "perm". Two of those are far weaker evidence than
			// the others, and a reader deciding whether to trust awaiting_input
			// has no way to tell them apart from the state alone.
			entry["inputSignal"] = p.inputSignal
		}
		if c := p.cmd; c != nil {
			if len(c.Args) > 0 {
				entry["cmd"] = strings.Join(c.Args, " ")
			} else if c.Path != "" {
				entry["cmd"] = c.Path
			}
			if c.Dir != "" {
				entry["cwd"] = c.Dir
			}
			if c.Process != nil {
				entry["pid"] = c.Process.Pid
			}
		}
		if p.controller != nil {
			entry["controller"] = p.controller.Name()
		}
		if snap.Model != "" {
			entry["model"] = snap.Model
		}
		if snap.Project != "" {
			entry["project"] = snap.Project
		}
		if snap.LastUserPrompt != "" {
			entry["prompt"] = snap.LastUserPrompt
		}
		if snap.LastResponse != "" {
			entry["response"] = snap.LastResponse
		}
		if snap.LastTool != "" {
			entry["tool"] = snap.LastTool
		}
		if !snap.StartedAt.IsZero() {
			entry["startedAt"] = snap.StartedAt.UTC().Format(time.RFC3339)
		}
		if !snap.CompletedAt.IsZero() {
			entry["completedAt"] = snap.CompletedAt.UTC().Format(time.RFC3339)
		}
		p.mu.Unlock()
		results = append(results, entry)
	}
	return results
}

// allPanesDone returns true if every session pane is either dead or
// inputReady. The control panel is not a session — it is never "done" and has
// no process to exit, so counting it would deadlock -w forever.
func (m *Magmux) allPanesDone() bool {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.allPanesDoneLocked()
}

// allPanesDoneLocked is the twin render() calls, which holds treeMu.RLock for
// its whole body. Taking a second RLock there would deadlock the moment a
// writer queued between the two — silently, with no race report, which is why
// the twin is mandatory rather than a style preference.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) allPanesDoneLocked() bool {
	sessions := 0
	for _, p := range m.allPanes {
		// A tombstone is not an unfinished session — counting one would mean
		// -w could never fire after an agent closed a pane.
		if p == nil || p.closed || p.isControl {
			continue
		}
		sessions++
		p.mu.Lock()
		busy := !p.dead && !p.inputReady
		p.mu.Unlock()
		if busy {
			return false
		}
	}
	return sessions > 0
}

// ── Dynamic panes ────────────────────────────────────────────────────────────

// A pane below these is unusable: no room for a prompt plus a line of output,
// and no room for a path. Enforced on BOTH halves of a split, because the
// interesting failure is the one that shrinks the pane you were splitting.
const (
	minPaneRows = 3
	minPaneCols = 20
)

// OpenPaneRequest.Target sentinels.
const (
	targetFocused = -1 // split whichever pane has focus
	targetLargest = -2 // split the largest live leaf
)

var (
	// errPaneTooSmall carries the code mcp_tools.go's openPaneHint branches on.
	errPaneTooSmall = sockErrf("too_small",
		"no room to split: each half needs at least %d rows and %d columns", minPaneRows, minPaneCols)
	errTargetGone = sockErrf(sockCodeNoSuchPane,
		"the pane to split is no longer part of the layout")
	errClosing = sockErrf(sockCodeUnsupported, "magmux is shutting down")
)

type OpenPaneRequest struct {
	PaneConfig
	Target int       // pane id to split; -1 = focused, -2 = largest live leaf
	Split  SplitType // SplitNone = auto (split the longer axis)
	Ratio  float64   // 0 => 0.5
	Focus  bool
}

// OpenPane splits an existing leaf and spawns a child in the new half.
//
// The ordering below is the whole design: the slow part (openPTY, fork/exec,
// the controller's filesystem probing) runs with NO lock held, and treeMu is
// taken only for the pointer surgery and reflow, which are microseconds. That
// is why this can run straight on the socket goroutine instead of being queued
// onto the render loop.
//
// Safe from any goroutine. Caller must NOT hold treeMu or p.mu.
func (m *Magmux) OpenPane(req OpenPaneRequest) (int, error) {
	// 1. Resolve the target and read its geometry, then let go.
	m.treeMu.RLock()
	if m.closing {
		m.treeMu.RUnlock()
		return -1, errClosing
	}
	t := m.resolveSplitTargetLocked(req.Target)
	if t == nil {
		m.treeMu.RUnlock()
		if req.Target >= 0 {
			return -1, sockErrf(sockCodeNoSuchPane, "no pane %d", req.Target)
		}
		return -1, errTargetGone
	}
	st := req.Split
	if st == SplitNone {
		// Split the longer axis. The 2:1 bias reproduces buildGrid's
		// two-column shape organically: on an 80x24 terminal a full pane is
		// 80 >= 48, so the first split is vertical-border/side-by-side and the
		// halves then split top/bottom, exactly like the static grid.
		if t.w >= 2*t.h {
			st = SplitHorizontal
		} else {
			st = SplitVertical
		}
	}
	ratio := req.Ratio
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.5
	}
	ty, tx, th, tw := t.y, t.x, t.h, t.w
	gridMode := m.gridMode
	m.treeMu.RUnlock()

	// 2. Refuse before spawning anything if either half would be unusable.
	var ny, nx, nh, nw int
	if st == SplitHorizontal {
		w1 := int(float64(tw) * ratio)
		w2 := tw - w1 - 1
		if w1 < minPaneCols || w2 < minPaneCols || th < minPaneRows {
			return -1, errPaneTooSmall
		}
		ny, nx, nh, nw = ty, tx+w1+1, th, w2
	} else {
		h1 := int(float64(th) * ratio)
		h2 := th - h1 - 1
		if h1 < minPaneRows || h2 < minPaneRows || tw < minPaneCols {
			return -1, errPaneTooSmall
		}
		ny, nx, nh, nw = ty+h1+1, tx, h2, tw
	}

	// 3. The slow part, outside every lock.
	np, err := newPaneFor(ny, nx, nh, nw, req.PaneConfig)
	if err != nil {
		return -1, sockErrf(sockCodeInternal, "could not start %q: %v", req.Cmd, err)
	}

	// 4. Still private — nothing else can reach np yet, so this needs no lock,
	// and attaching the controller here means p.controller is written before
	// any other goroutine could read it (and Start's filesystem work stays off
	// treeMu). Missing gridMode here would give the pane different writePTY
	// suppression and no DONE overlay compared with its siblings.
	np.gridMode = gridMode
	m.attachController(np)

	// 5. Publish.
	m.treeMu.Lock()
	if m.closing {
		m.treeMu.Unlock()
		m.unwindPane(np)
		return -1, errClosing
	}
	// Re-verify: a concurrent close_pane may have detached t while we were
	// forking. Splicing onto a detached node would attach the new pane to a
	// subtree that is no longer reachable from m.root — invisible, undismissable.
	if m.paneByIDLocked(t.id) != t {
		m.treeMu.Unlock()
		m.unwindPane(np)
		return -1, errTargetGone
	}
	np.id = len(m.allPanes)
	m.allPanes = append(m.allPanes, np)
	m.splitLeafLocked(t, np, st, ratio)
	if !np.isControl {
		// Under the same lock cleanup() sets m.closing, which is what keeps
		// this Add from racing wg.Wait.
		m.wg.Add(1)
	}
	if req.Focus {
		m.focused = np
	}
	id := np.id
	m.treeMu.Unlock()

	// 6. Start the pane's goroutines now that it is reachable. The waiter is
	// NOT conditional on grid mode: it is the only caller of cmd.Wait, so
	// without it every close_pane in a non-grid session leaves a zombie and
	// p.reaped never becomes true. What grid mode gates is the tombstone it
	// paints, inside waitForChild.
	if !np.isControl {
		go np.readLoop(&m.wg)
		go m.waitForChild(np)
	}

	// 7. Tell subscribers.
	ev := map[string]any{"type": "pane_opened", "pane": id, "cmd": req.Cmd}
	if req.Dir != "" {
		ev["cwd"] = req.Dir
	}
	if req.Label != "" {
		ev["label"] = req.Label
	}
	m.broadcastEvent(ev)
	return id, nil
}

// ClosePane detaches a pane from the layout and reaps its child. The allPanes
// slot is retained as a tombstone so ids never shift.
//
// Note the two-state model this preserves: `dead` means the process is gone but
// the pane is still on screen with its ✓ DONE / ✗ FAIL overlay — a self-exit
// never auto-collapses, because in grid mode the finished grid IS the report.
// `closed` means the pane itself is gone. close_pane on a dead pane is the
// normal way to reclaim its space.
//
// Caller must NOT hold treeMu or p.mu.
func (m *Magmux) ClosePane(id int, force bool) error {
	m.treeMu.Lock()
	p := m.paneByIDLocked(id)
	if p == nil {
		m.treeMu.Unlock()
		return sockErrf(sockCodeNoSuchPane, "no pane %d", id)
	}

	// Compute the sibling BEFORE the surgery: focus prefers a leaf under it,
	// which is what tmux does and what keeps focus near where you were.
	var sib *Pane
	if p.parent != nil {
		sib = p.parent.child1
		if sib == p {
			sib = p.parent.child2
		}
	}

	p.closed = true
	m.removeLeafLocked(p)

	refocused := -1
	if m.focused == p || m.focused == nil || m.focused.closed {
		m.focused = firstLiveLeaf(sib)
		if m.focused == nil {
			m.focused = firstLiveLeaf(m.root)
		}
		if m.focused != nil {
			refocused = m.focused.id
		}
	}
	if sel.pane == p {
		m.selClear()
	}

	// "Was that the last one?" is answered under the same lock that removed
	// it, so two concurrent closes cannot both decide it was not.
	//
	// Two conditions, because the control panel is chrome rather than a
	// session: an empty layout obviously has nothing left to show, and so does
	// a magmux whose last SESSION is gone — with only the panel left, -w can
	// never fire (allPanesDone needs at least one session) and the window would
	// sit there forever. The everSession guard keeps a panel-only magmux, which
	// never had a session to lose, from quitting on its first close.
	liveTotal, liveSessions, everSession := 0, 0, false
	for _, q := range m.allPanes {
		if q == nil {
			continue
		}
		if !q.isControl {
			everSession = true
		}
		if q.closed {
			continue
		}
		liveTotal++
		if !q.isControl {
			liveSessions++
		}
	}
	lastOne := liveTotal == 0 || (everSession && liveSessions == 0)
	m.treeMu.Unlock()

	// Everything below is blocking or takes another lock, so it runs after the
	// unlock.
	//
	// The panel stops painting rather than dereferencing a detached pane, and
	// its focus marker moves with the focus we just fixed — focusNext and
	// sockFocus both tell it, and a close that did not would leave the table's
	// ▸ pointing at a pane that no longer exists until the next focus change.
	m.control.detach(p)
	if refocused >= 0 {
		m.control.setFocused(refocused)
	}

	// Releasing the transcript claim is not optional: claimedSessions is never
	// otherwise cleaned, and a stranded entry leaves the next pane in the same
	// project stuck in `starting` silently and forever.
	m.releaseSessions(p)
	m.reapPane(p, force)
	m.broadcastEvent(map[string]any{"type": "pane_closed", "pane": id})

	if lastOne {
		m.quitOnce.Do(func() { close(m.quit) })
	}
	return nil
}

// resolveSplitTargetLocked turns a Target into a live LEAF pane, or nil.
// Caller holds at least treeMu.RLock.
func (m *Magmux) resolveSplitTargetLocked(target int) *Pane {
	if target >= 0 {
		// A hidden pane has no place in the tree to splice onto, and its
		// geometry is whatever it was when it was taken out.
		p := m.paneByIDLocked(target)
		if p == nil || p.hidden || p.splitType != SplitNone {
			return nil
		}
		return p
	}
	if target == targetFocused {
		if p := m.focused; p != nil && !p.closed && !p.hidden && p.splitType == SplitNone {
			return p
		}
		// Focus can be nil after the last pane closed, or on a pane that was
		// just detached. Falling back beats refusing.
	}
	return m.largestLiveLeafLocked()
}

// largestLiveLeafLocked returns the live leaf with the most cells, which is the
// one a split hurts least. Caller holds at least treeMu.RLock.
func (m *Magmux) largestLiveLeafLocked() *Pane {
	var best *Pane
	bestArea := -1
	for _, p := range m.allPanes {
		// Hidden panes carry stale geometry — the size they had when they left
		// the tree — so an unqualified h*w would happily nominate one.
		if p == nil || p.closed || p.hidden || p.splitType != SplitNone {
			continue
		}
		if a := p.h * p.w; a > bestArea {
			best, bestArea = p, a
		}
	}
	return best
}

// firstLiveLeaf returns the first live leaf under node, preferring a real
// session over the control panel — focus on the panel does nothing visible and
// looks broken.
func firstLiveLeaf(node *Pane) *Pane {
	var fallback *Pane
	var walk func(*Pane) *Pane
	walk = func(p *Pane) *Pane {
		if p == nil || p.closed {
			return nil
		}
		if p.splitType == SplitNone {
			if p.isControl {
				if fallback == nil {
					fallback = p
				}
				return nil
			}
			return p
		}
		if f := walk(p.child1); f != nil {
			return f
		}
		return walk(p.child2)
	}
	if f := walk(node); f != nil {
		return f
	}
	return fallback
}

// splitLeafLocked splices a FRESH internal node in place of leaf t, with t as
// child1 and np as child2, then reflows.
//
// The leaf is never converted in place. It owns screen, ptmx, cmd and
// vt.node == p, and it is pointed at from outside the tree by
// ClaudeCodeController.pane, ControlPanel.pane, sel.pane and m.claimedSessions;
// turning it into an internal node would strand every one of them.
//
// Reflow is the existing reshapeChildren → resize path, which for a leaf does
// screen.resize + setWinSize. No new geometry code exists anywhere in this file.
//
// Caller holds treeMu.Lock.
func (m *Magmux) splitLeafLocked(t, np *Pane, st SplitType, ratio float64) {
	m.splitNodeLocked(t, np, st, ratio, false)
}

// splitNodeLocked is splitLeafLocked with the side chosen by the caller, and
// with `t` allowed to be an internal node.
//
// Both generalisations exist for one caller: re-showing the control panel.
// removeLeafLocked collapsed the panel's parent into its SIBLING, and that
// sibling is usually a whole column rather than a leaf; putting the panel back
// where it was means splitting that node again, on the side it was on. Nothing
// else here changes — the parent is still a FRESH node and `t` is still never
// converted in place, which is the invariant that keeps ControlPanel.pane,
// sel.pane and the controllers' back-pointers valid.
//
// Caller holds treeMu.Lock.
func (m *Magmux) splitNodeLocked(t, np *Pane, st SplitType, ratio float64, npFirst bool) {
	par := &Pane{
		splitType: st,
		y:         t.y, x: t.x, h: t.h, w: t.w,
		ratio:  ratio,
		parent: t.parent,
		child1: t,
		child2: np,
	}
	if npFirst {
		par.child1, par.child2 = np, t
	}
	switch {
	case t.parent == nil:
		m.root = par
	case t.parent.child1 == t:
		t.parent.child1 = par
	default:
		t.parent.child2 = par
	}
	t.parent = par
	np.parent = par
	par.reshapeChildren()
}

// removeLeafLocked detaches leaf t and collapses its parent into its sibling,
// which inherits the parent's EXACT geometry — so the space t occupied plus the
// border between them goes to the sibling with no gap and no overlap.
//
// Caller holds treeMu.Lock.
func (m *Magmux) removeLeafLocked(t *Pane) {
	par := t.parent
	if par == nil {
		// t was the whole tree. An empty tree is legal for exactly as long as
		// it takes ClosePane to quit; renderPane's nil guard covers the frames
		// in between.
		if m.root == t {
			m.root = nil
		}
		return
	}
	sib := par.child1
	if sib == t {
		sib = par.child2
	}
	sib.parent = par.parent
	switch {
	case par.parent == nil:
		m.root = sib
	case par.parent.child1 == par:
		par.parent.child1 = sib
	default:
		par.parent.child2 = sib
	}
	t.parent = nil
	sib.resize(par.y, par.x, par.h, par.w)
}

// ── Chrome: the control panel and the status bar ─────────────────────────────
//
// magmux's default is now to show nothing of itself. `magmux -e claude` is a
// bare terminal running Claude Code: one leaf, no border (renderBorder only
// ever paints a SPLIT node, so a lone leaf has nothing to draw), the whole
// terminal minus the status row. Ctrl-G p reveals the panel, Ctrl-G s drops
// the status row, and -c means "start with the panel already visible" so every
// invocation that predates this looks exactly as it did.

// panelTooNarrow is what the status bar says when the terminal cannot carry
// both a session and a panel at the minimum a pane is usable at.
const panelTooNarrow = "no room for the panel — widen the terminal"

// splitFits reports whether splitting t leaves both halves usable. Same
// arithmetic and the same floor as OpenPane, deliberately: "usable" cannot
// mean one thing for an agent opening a pane and another for the panel.
func splitFits(t *Pane, st SplitType, ratio float64) bool {
	if t == nil {
		return false
	}
	if st == SplitHorizontal {
		w1 := int(float64(t.w) * ratio)
		return w1 >= minPaneCols && t.w-w1-1 >= minPaneCols && t.h >= minPaneRows
	}
	h1 := int(float64(t.h) * ratio)
	return h1 >= minPaneRows && t.h-h1-1 >= minPaneRows && t.w >= minPaneCols
}

// hidePanelLocked takes the panel out of the layout and gives its space back.
//
// This is NOT a close. `closed` is a tombstone and is permanent; `dead` is a
// child that exited. A hidden panel is alive, keeps every row of its OUT/IN
// ledger, keeps its id, and is still reported by buildPaneResults as
// state:"panel" — it is merely not in the tree, so it costs no columns and the
// sessions reflow over it.
//
// Caller holds treeMu.Lock.
func (m *Magmux) hidePanelLocked(p *Pane) {
	if par := p.parent; par != nil {
		sib := par.child1
		if sib == p {
			sib = par.child2
		}
		m.panelAnchor = sib
		m.panelSplit = par.splitType
		m.panelRatio = par.ratio
		m.panelFirst = par.child1 == p
	} else {
		// The panel was the whole tree (magmux -c with no -e). There is no
		// sibling to anchor to; showing it again makes it the root.
		m.panelAnchor, m.panelSplit, m.panelRatio, m.panelFirst = nil, SplitHorizontal, 0.5, false
	}
	m.removeLeafLocked(p)
	p.hidden = true
	// A selection anchored in a pane that is no longer on screen would paint
	// its highlight over whatever grew into that space.
	if sel.pane == p {
		m.selClear()
	}
	// Focus must not be left on something invisible: every keystroke would go
	// to a pane nobody can see. firstLiveLeaf prefers a real session over the
	// panel, which is exactly what is wanted here.
	if m.focused == p {
		m.focused = firstLiveLeaf(m.root)
	}
}

// showPanelLocked splices the panel back where it was, or reports that there
// is no room. It does NOT touch focus: the human is typing into their agent,
// and revealing an instrument must not take the keyboard away from them —
// which is the same reason main() moves focus off the panel at startup.
//
// Caller holds treeMu.Lock.
func (m *Magmux) showPanelLocked(p *Pane) bool {
	st, ratio := m.panelSplit, m.panelRatio
	if st == SplitNone {
		st = SplitHorizontal
	}
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.5
	}

	t := m.panelAnchor
	if !m.nodeInTreeLocked(t) {
		// The node the panel was taken from is gone (an agent closed the pane
		// under it). The root is the honest fallback: the panel comes back as a
		// column beside everything else, which is where the layout builders put
		// it in the first place.
		t = m.root
	}
	if t == nil {
		// Empty tree — the panel is the layout.
		p.hidden = false
		p.parent = nil
		m.root = p
		m.reflowLocked()
		return true
	}
	if !splitFits(t, st, ratio) {
		// Refuse rather than reshape into a 0-column pane. reshapeChildren
		// clamps at zero, so the layout would survive — as a panel with no
		// columns in it, which is worse than not showing it at all.
		return false
	}
	p.hidden = false
	m.splitNodeLocked(t, p, st, ratio, m.panelFirst)
	return true
}

// togglePanel reveals or hides the control panel. Caller must NOT hold treeMu.
func (m *Magmux) togglePanel() {
	m.treeMu.Lock()
	p := m.panelLocked()
	if p == nil {
		m.treeMu.Unlock()
		return
	}
	shown, refocus := false, -1
	if p.hidden {
		if shown = m.showPanelLocked(p); !shown {
			m.noteChromeLocked(panelTooNarrow)
		}
	} else {
		m.hidePanelLocked(p)
		if m.focused != nil {
			refocus = m.focused.id
		}
	}
	m.markAllDirtyLocked()
	m.treeMu.Unlock()

	// cp.mu is taken with treeMu released. The documented order treeMu -> cp.mu
	// would allow it above, but nothing here needs the two held together and
	// the shorter treeMu hold is free.
	if refocus >= 0 {
		m.control.setFocused(refocus)
	}
	if shown {
		// The panel has no child process to redraw it, so a reflow leaves it
		// blank unless it is told — the same reason SIGWINCH marks it dirty.
		m.control.markDirty()
	}
}

// toggleStatusBar shows or hides the bottom status row, giving the row to the
// layout or taking it back. Caller must NOT hold treeMu.
func (m *Magmux) toggleStatusBar() {
	m.treeMu.Lock()
	m.hideStatus = !m.hideStatus
	m.reflowLocked()
	m.markAllDirtyLocked()
	m.treeMu.Unlock()
	m.control.markDirty()
}

// unwindPane throws away a pane that was spawned but never published.
//
// OpenPane forks before it takes the write lock, so a teardown or a concurrent
// close of the split target can leave a fully-started child that no goroutine
// owns: wg.Add, readLoop and waitForChild all happen after publication, so
// nothing would ever call cmd.Wait and the child would sit as a zombie for the
// life of magmux. reapPane alone is not enough — SIGHUP and closing the PTY
// end the process, they do not collect it.
//
// The wait runs in its own goroutine, and NOT under m.wg: this pane was never
// added to the WaitGroup, and adding it here would race the wg.Wait that the
// m.closing unwind path exists to keep clear of.
func (m *Magmux) unwindPane(p *Pane) {
	m.reapPane(p, false)
	if p != nil && p.cmd != nil {
		go m.reapChild(p)
	}
}

// reapPane terminates a pane's child. Called with NO lock held: signalling and
// closing the PTY are blocking operations, and closing ptmx is precisely what
// unblocks readLoop's Read — which is what sets p.dead and calls wg.Done.
func (m *Magmux) reapPane(p *Pane, force bool) {
	if p == nil || p.isControl {
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGHUP)
	}
	p.mu.Lock()
	ptmx := p.ptmx
	p.mu.Unlock()
	if ptmx != nil {
		_ = ptmx.Close()
	}
	if !force || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	pid := p.cmd.Process.Pid
	go func() {
		time.Sleep(2 * time.Second)
		// The NEGATIVE pid is the process GROUP, which is correct because
		// spawnPTY sets Setsid — a shell that ignored SIGHUP would otherwise
		// leave its own children behind. The reaped check is what stops a
		// delayed kill landing on a stranger: only after cmd.Wait has returned
		// is the pid free for the OS to hand to somebody else.
		p.mu.Lock()
		reaped := p.reaped
		p.mu.Unlock()
		if reaped {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()
}

func (m *Magmux) renderLoop() {
	for {
		select {
		case <-m.quit:
			return
		default:
			m.render()
			sleepMs(16) // ~60fps max, but render() skips when nothing dirty
		}
	}
}

// render paints one frame.
//
// It holds treeMu.RLock for the WHOLE of renderLocked — the layout must not
// change mid-frame, and a pane cannot be spliced out from under the tree walk.
// That is also why every helper it reaches has a …Locked twin: a second RLock
// on this goroutine deadlocks if a writer queued in between, and that failure
// is a silent hang.
//
// Everything that can BLOCK stays outside that lock, and there are three such
// things, not one:
//
//   - the controller poll, which is filesystem work (see pollControllers) and
//     runs before the lock is taken, so its snapshots still take precedence
//     over the screen-scraping heuristics in the same frame;
//   - the snapshot events, collected under the lock and broadcast after it
//     (conn.Write has a 100ms-per-client deadline, so a wedged subscriber would
//     otherwise stall every writer);
//   - the frame itself. renderLocked BUILDS the bytes and hands them back; this
//     is the only place they are written. A tty whose buffer is full blocks
//     that write for as long as it likes, and with RLock held that is a
//     keystroke, a SIGWINCH or a socket verb blocked for exactly as long.
func (m *Magmux) render() {
	events := m.pollControllers(false)

	m.treeMu.RLock()
	frame, out, quit := m.renderLocked()
	m.treeMu.RUnlock()
	events = append(events, frame...)

	if out != "" {
		m.writeTerm(out)
	}

	if quit {
		// Final unthrottled controller poll before teardown. allPanesDone
		// became true because a pane went idle, and that same transition is
		// what a controller would promote to awaiting_input — but the regular
		// poll is throttled to 250ms, so quitting here can drop the last
		// snapshot and leave subscribers to infer the state from `results`
		// alone (issue #2). Polling once more guarantees it is broadcast.
		events = append(events, m.pollControllers(true)...)
	}

	for _, ev := range events {
		m.broadcastEvent(ev)
	}
	// Quitting is signalled back rather than done under the lock, and only
	// AFTER the events are out. Closing m.quit wakes the socket teardown,
	// which broadcasts `results` and then closes every subscriber; a snapshot
	// still sitting in this slice at that moment is simply lost. That is
	// exactly the final awaiting_input snapshot -w exists to deliver (issue #2).
	if quit {
		m.quitOnce.Do(func() { close(m.quit) })
	}
}

// writeTerm puts a painted frame on the terminal. Caller must NOT hold treeMu:
// this is a write to a tty, and a tty with a full buffer blocks.
func (m *Magmux) writeTerm(s string) {
	if m.out != nil {
		_, _ = io.WriteString(m.out, s)
		return
	}
	_, _ = os.Stdout.WriteString(s)
}

// ── the status bar's panel digest ────────────────────────────────────────────
//
// The panel starts hidden, so the status bar is the only place a run announces
// itself until somebody asks for the panel. What it carries is the panel's own
// two counters — `▶` what the controller asked for, `◀` what magmux observed —
// plus the newest signal, degrading to the counters alone as the terminal
// narrows. It invents no third number: a status bar that reconciled the two
// would be a second, quieter provenance model beside the panel's.
//
// The done/running counts stay. They answer a different question ("how much of
// this grid has finished") from the counters ("how many instructions has the
// controller issued, and how many turns has magmux seen close"), and on a run
// with several panes and one controller they routinely disagree — which is
// exactly the disagreement worth seeing.

// statusSeg joins two tab-separated status-bar segments.
func statusSeg(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\t" + b
}

// digestStateSeg is "p0 working · Bash 14s" — which pane, doing what, since
// when. Empty when the controller has not touched a pane yet.
func digestStateSeg(d ctrlDigest) string {
	if d.pane < 0 || d.state == "" {
		return ""
	}
	s := fmt.Sprintf("p%d %s", d.pane, d.state)
	if d.tool != "" {
		s += " · " + d.tool
	}
	if !d.stateAt.IsZero() {
		s += " " + formatDuration(time.Since(d.stateAt))
	}
	return "M: " + s
}

// digestSignalSeg is the newest row of the panel's stream, cut to `budget`
// columns. Empty when there is no room for a useful amount of it — a signal
// truncated to three characters and an ellipsis is noise, not information.
func digestSignalSeg(d ctrlDigest, budget int) string {
	if d.sigVerb == "" && d.sigText == "" {
		return ""
	}
	glyph := "•"
	switch d.sigDir {
	case "out":
		glyph = "▶"
	case "in":
		glyph = "◀"
	}
	head := glyph + " " + d.sigVerb
	if d.sigVerb == "" {
		head = glyph
	}
	body := oneLine(d.sigText, maxInt(0, budget-utf8.RuneCountInString(head)-4))
	if body != "" {
		head += ` "` + body + `"`
	}
	if utf8.RuneCountInString(head) < 6 {
		return ""
	}
	return "D: " + head
}

// appendPanelDigestLocked adds as much of the digest as fits in m.cols, widest
// part first out. Caller holds at least treeMu.RLock; d was taken with cp.mu
// already released.
func (m *Magmux) appendPanelDigestLocked(segs string, d ctrlDigest, reserve int) string {
	if !d.active {
		return segs
	}
	fits := func(s string) bool { return approxStatusWidth(s) <= m.cols }

	// The counters are the digest, so they are the LAST thing dropped — but
	// they are still dropped. A status bar that overruns m.cols wraps onto the
	// pane above it, and corrupting a session's output to announce that magmux
	// is here is the exact opposite of what this change is for.
	out := statusSeg(segs, fmt.Sprintf("W: ▶%d ◀%d", d.sent, d.observed))
	if !fits(out) {
		return segs
	}

	// The key hints are what make a hidden panel discoverable, so their room is
	// reserved before the state and the signal are measured rather than
	// competing with them. The caller appends the hints themselves — see
	// appendKeyHint — once the digest has taken what it can.
	if st := digestStateSeg(d); st != "" && approxStatusWidth(statusSeg(out, st))+reserve <= m.cols {
		out = statusSeg(out, st)
	}
	if budget := m.cols - approxStatusWidth(out) - 3 - reserve; budget >= 14 {
		// Re-measured rather than trusted: digestSignalSeg cuts the signal's
		// TEXT to the budget, but its head (the direction glyph and the verb)
		// is whatever the verb is, so a long verb can overrun a budget that was
		// only ever applied to the quoted body — and it would overrun it into
		// the hints' floor.
		if sig := digestSignalSeg(d, budget); sig != "" {
			if cand := statusSeg(out, sig); approxStatusWidth(cand)+reserve <= m.cols {
				out = cand
			}
		}
	}
	return out
}

// ── the status bar's key hints ───────────────────────────────────────────────
//
// magmux is invisible by default: no panel, no border, one status row. That row
// is therefore the ONLY place a user can find out that a control panel exists
// at all — the panel is not on screen to advertise itself, and `--help` is not
// on screen either. So `p` is the discovery-critical hint and outranks
// everything on the bar except `q`, the way out.
//
// Survival order as the terminal narrows, last dropped first: q, p, Tab, s.
// `Tab` only matters with more than one pane on screen, and `s` is the least
// useful column magmux can spend — hiding the bar hides its own hint, so the
// one person who wants it gone can find it in `--help`.
//
// The hints are not appended after everything else has taken its room: the run
// summary yields to them (see statusForms / fitStatusBase) and the digest
// reserves their floor before it measures its optional parts. A hint that only
// showed up on a wide terminal would be a hint for the people who least need
// it.

// hintItem is one key hint and the rank at which it is dropped: the lowest rank
// goes first, and rank 0 is reserved for "keep everything".
type hintItem struct {
	text string
	rank int
}

const (
	hintRankStatus = iota + 1 // s: hiding the bar hides this hint with it
	hintRankScroll            // [: only offered when there is history to reach
	hintRankTab               // Tab: nothing to switch to with one pane on screen
	hintRankPanel             // p: the surface nothing else can announce
	hintRankQuit              // q: the way out
)

// keyHintItems is the chord's full hint set, in reading order. panelVisible
// flips "p panel" to "p hide" so the key's effect is unambiguous in both
// states; multiPane drops Tab when there is nothing to switch to; canScroll
// offers "[ back" only when the focused pane actually has something behind it.
//
// The scroll hint is conditional rather than permanent because most of what
// magmux runs is on the ALTERNATE screen — Claude Code, vim, htop — and those
// panes keep no scrollback at all by design. Advertising a key that does
// nothing on the pane you are looking at teaches the wrong thing about the
// feature; offering it the moment a shell pane has scrolled teaches the right
// one.
//
// It ranks below Tab so the floor the rest of the bar yields to (panel + quit)
// is unchanged: the hint appears when there is room, and is the second thing to
// go when there is not.
func keyHintItems(panelVisible, multiPane, canScroll bool) []hintItem {
	panel := "p panel"
	if panelVisible {
		panel = "p hide"
	}
	items := []hintItem{{panel, hintRankPanel}}
	if multiPane {
		items = append(items, hintItem{"Tab", hintRankTab})
	}
	if canScroll {
		items = append(items, hintItem{"[ back", hintRankScroll})
	}
	return append(items, hintItem{"s bar", hintRankStatus}, hintItem{"q quit", hintRankQuit})
}

// keyHintLocked is the hint set for the current layout, or nil when the chord
// is inert — a finished grid swallows every key but q/Esc/Ctrl-C, so
// advertising p there would be advertising a key that does nothing.
// Caller holds at least treeMu.RLock.
func (m *Magmux) keyHintLocked() []hintItem {
	if m.gridMode && m.allPanesDoneLocked() {
		return nil
	}
	onScreen := 0
	for _, p := range m.livePanesLocked(nil) {
		if !p.hidden {
			onScreen++
		}
	}
	// treeMu -> p.mu is the legal order, so reading the focused pane's history
	// depth from here is fine.
	canScroll := false
	if f := m.focused; f != nil && !f.isControl && f.screen != nil {
		f.mu.Lock()
		canScroll = f.screen.sbLen > 0
		f.mu.Unlock()
	}
	return keyHintItems(!m.panelHiddenLocked(), onScreen > 1, canScroll)
}

// keyHintList joins the hints, dropping everything ranked at or below `cut`.
func keyHintList(items []hintItem, cut int) string {
	var parts []string
	for _, it := range items {
		if it.rank > cut {
			parts = append(parts, it.text)
		}
	}
	return strings.Join(parts, " · ")
}

// hintFloor is the form the rest of the bar yields to: the panel and the way
// out. Everything above it (Tab, s) competes for leftovers like any other
// segment.
func hintFloor(items []hintItem) string { return keyHintList(items, hintRankTab) }

// hintFloorWidth is what hintFloor costs on the bar, divider included. Zero
// when there are no hints to place.
func hintFloorWidth(items []hintItem) int {
	f := hintFloor(items)
	if f == "" {
		return 0
	}
	return utf8.RuneCountInString("ctrl-g "+f) + 3
}

// appendKeyHint puts as much of the hint set on the bar as `cols` allows,
// dropping items in reverse priority. It never returns a bar wider than cols:
// a status row that overruns wraps onto the pane above it, which is the one
// thing magmux's chrome must never do to a session's output.
func appendKeyHint(segs string, items []hintItem, cols int) string {
	for cut := 0; cut <= hintRankQuit; cut++ {
		list := keyHintList(items, cut)
		if list == "" {
			break
		}
		if out := statusSeg(segs, "D: ctrl-g "+list); approxStatusWidth(out) <= cols {
			return out
		}
	}
	// Last resort. A terminal too narrow for "ctrl-g q quit" is still a
	// terminal somebody has to be able to get out of.
	if len(items) > 0 {
		if out := statusSeg(segs, "D: ^g q"); approxStatusWidth(out) <= cols {
			return out
		}
	}
	return segs
}

// fitStatusBase picks the widest run summary that still leaves the hints their
// floor. The forms are widest first, and each drops the least valuable segment
// of the one before it: the running count is derivable from the done pill, the
// timer is a nice-to-have, and magmux's own name is the first thing a narrow
// terminal should stop spending columns on.
func fitStatusBase(budget int, forms ...string) string {
	for _, f := range forms {
		if approxStatusWidth(f) <= budget {
			return f
		}
	}
	return ""
}

// chordMenuLocked is what the bar says while Ctrl-G has been pressed and magmux
// is waiting for the second key: the chord teaching itself, for one keystroke,
// on the one row magmux already owns. Empty when there is nothing to offer.
// Caller holds at least treeMu.RLock.
func (m *Magmux) chordMenuLocked(items []hintItem) string {
	for cut := 0; cut <= hintRankQuit; cut++ {
		list := keyHintList(items, cut)
		if list == "" {
			break
		}
		if s := "C: ctrl-g …\tD: " + list; approxStatusWidth(s) <= m.cols {
			return s
		}
	}
	return ""
}

// renderLocked paints one frame into a buffer and returns it along with the
// events to broadcast and whether magmux should quit. It writes NOTHING to the
// terminal — see render(), which does that with the lock released.
//
// The controller poll deliberately happens in render() BEFORE this is called,
// so that controller state still takes precedence over the screen-scraping
// heuristics below when both are available.
//
// Caller holds treeMu.RLock.
func (m *Magmux) renderLocked() (events []any, out string, quit bool) {
	// Grid mode: idle/completion detection
	if m.gridMode {
		now := time.Now()
		for _, p := range m.livePanesLocked(nil) {
			p.mu.Lock()
			// Title idle debounce: fire inputReady after the window title has been
			// showing ✳ (idle) for at least 2 seconds without any spinner title
			// reappearing. Targets long-running TUI apps (Claude Code interactive)
			// that transition ✳ → spinner → ✳ between model response and stop hooks.
			//
			// Only active on alt-screen panes or panes with an attached tool
			// controller. Plain shell commands that happen to update xterm
			// titles (git, tmux, shell prompts) should NOT be mis-detected as
			// "idle" just because they set a title briefly.
			if !p.inputReady && !p.dead && !p.titleIdleAt.IsZero() &&
				now.Sub(p.titleIdleAt) > 2*time.Second &&
				(p.altMode || p.controller != nil) {
				p.inputReady = true
				p.inputSignal = "title"
				p.inputReadyAt = now
				if dbgFile != nil {
					fmt.Fprintf(dbgFile, "[title] idle for %.1fs → inputReady=true\n",
						now.Sub(p.titleIdleAt).Seconds())
				}
			}
			// Text idle detection: if no printable text for 5s, mark as input-ready.
			// This catches TUI apps that stay running while waiting for input
			// (like Claude Code interactive) without emitting a clear signal.
			//
			// For non-TUI / non-controlled panes, we deliberately do NOT fire
			// text-idle because a non-interactive command that sleeps (e.g. an
			// API call, a long compile) would be wrongly marked "done". The
			// right signal for those is process exit, which lands in waitForChild.
			//
			// The heuristic therefore only activates when either:
			//   - the pane is running in an alternate screen (a clear TUI marker), or
			//   - a controller is attached but hasn't produced a snapshot yet (rare).
			if !p.inputReady && !p.dead && p.hadTextOutput &&
				!p.lastTextAt.IsZero() && now.Sub(p.lastTextAt) > 5*time.Second &&
				(p.altMode || p.controller != nil) {
				p.inputReady = true
				p.inputSignal = "idle"
				p.inputReadyAt = now
				if dbgFile != nil {
					fmt.Fprintf(dbgFile, "[idle] pane text idle for %.1fs → inputReady=true\n",
						now.Sub(p.lastTextAt).Seconds())
				}
			}
			// TUI app waiting for input → tint green + overlay (task complete)
			if p.inputReady && p.tint == "" {
				p.tint = "green"
				p.overlayStyle = "success"
				// Build multi-line popup with duration + last output line
				var lines []string
				lines = append(lines, "\u2713 DONE")
				if !p.startedAt.IsZero() {
					lines = append(lines, "took "+formatDuration(time.Since(p.startedAt)))
				}
				if msg := p.lastNonEmptyLine(40); msg != "" {
					lines = append(lines, msg)
				}
				p.overlayText = strings.Join(lines, "\n")
				p.dirty = true
			}
			p.mu.Unlock()
		}
	}

	// The key hints, resolved once for this frame: the run summary, the digest
	// and the chord menu below all have to agree about what the bar is going to
	// advertise and how much room it needs.
	hints := m.keyHintLocked()

	// Update status bar with done/running counts (grid mode)
	if m.gridMode {
		done, running := 0, 0
		for _, p := range m.livePanesLocked(nil) {
			if p.isControl {
				continue // the panel is chrome, not a tracked session
			}
			p.mu.Lock()
			if p.dead || p.inputReady {
				done++
			} else {
				running++
			}
			p.mu.Unlock()
		}
		// Rebuild status text every render so the timer stays current
		// while work is in progress; freeze it the moment everything
		// finishes so the user sees the final elapsed time.
		total := done + running
		allDone := total > 0 && running == 0 && done == total
		if allDone && m.completedAt.IsZero() {
			m.completedAt = time.Now()
		}
		if !allDone && !m.completedAt.IsZero() {
			// A pane came back to life — reset the freeze
			m.completedAt = time.Time{}
		}

		var elapsed string
		if !m.completedAt.IsZero() {
			elapsed = formatDuration(m.completedAt.Sub(m.startedAt))
		} else {
			elapsed = formatDuration(time.Since(m.startedAt))
		}

		// The run summary, widest form first: every later form drops the least
		// valuable segment of the one before it, so that the key hints keep
		// their floor on a narrow terminal instead of being pushed off it.
		var forms []string
		if allDone {
			forms = []string{
				fmt.Sprintf("*: %s\tP: %d/%d done\tG: ✓ complete\tM: %s\tD: q or Esc to quit",
					magmuxLabel(), total, total, elapsed),
				fmt.Sprintf("*: %s\tP: %d/%d done\tG: ✓ complete\tD: q or Esc to quit",
					magmuxLabel(), total, total),
				fmt.Sprintf("P: %d/%d done\tG: ✓ complete\tD: q quit", total, total),
				fmt.Sprintf("P: %d/%d done\tD: q quit", total, total),
				"D: q quit",
			}
		} else if total > 0 {
			forms = []string{
				fmt.Sprintf("*: %s\tP: %d/%d done\tY: %d running\tM: %s",
					magmuxLabel(), done, total, running, elapsed),
				fmt.Sprintf("*: %s\tP: %d/%d done\tM: %s", magmuxLabel(), done, total, elapsed),
				fmt.Sprintf("*: %s\tP: %d/%d done", magmuxLabel(), done, total),
				fmt.Sprintf("P: %d/%d done", done, total),
			}
		}
		// cp.mu is taken and released inside digest(); the order
		// treeMu -> cp.mu is the documented one, and nothing here holds it
		// across the rendering that follows.
		dg := m.control.digest()
		if len(forms) == 0 && dg.active {
			// No session pane to count — a controlled run against panes the
			// grid counter skips, or a panel-only magmux — but a controller IS
			// driving, so the bar needs a label for the digest to hang off.
			// Untouched panels still leave the default text alone.
			forms = []string{"*: " + magmuxLabel()}
		}
		if len(forms) > 0 {
			segs := fitStatusBase(m.cols-hintFloorWidth(hints), forms...)

			// The panel's digest goes in BEFORE the hints and the attribution,
			// so a run with something to say takes the room and the credit
			// yields — but it reserves the hints' floor before it does.
			segs = m.appendPanelDigestLocked(segs, dg, hintFloorWidth(hints))
			segs = appendKeyHint(segs, hints, m.cols)

			// Append "by MadAppGang" attribution if there's room.
			// Rough visible width: sum of segment text lengths + dividers +
			// padding. We don't need precision; a permissive threshold is fine.
			const attribution = "by MadAppGang"
			if m.cols > 100 && approxStatusWidth(segs)+len(attribution)+6 < m.cols {
				segs += "\tD: " + attribution
			}
			m.statusText = segs
		}

		// Force a redraw once per second so the timer updates visibly even
		// when no pane has new content — but only while the timer is live.
		if m.completedAt.IsZero() {
			curSec := int(time.Since(m.startedAt).Seconds())
			if curSec != m.lastTimerTick {
				m.lastTimerTick = curSec
				if m.focused != nil {
					m.focused.mu.Lock()
					m.focused.dirty = true
					m.focused.mu.Unlock()
				}
			}
		}
		m.lastDoneCount = done
	}

	// Auto-exit: quit when all panes done (-w flag). render() answers the quit
	// with one final UNTHROTTLED controller poll before it wakes the teardown —
	// that poll cannot happen here, because it is filesystem work and this runs
	// under RLock.
	if m.autoExit && m.gridMode && m.allPanesDoneLocked() {
		return events, "", true
	}

	// Auto-close countdown, armed when a pilot declares the run over. Feed
	// the remaining time to the panel so the countdown is visible rather
	// than the window disappearing without warning.
	if !m.closeAt.IsZero() {
		remain := time.Until(m.closeAt)
		if remain <= 0 {
			return events, "", true
		}
		m.control.setCloseIn(remain)
	}

	// Repaint the control panel before the dirty sweep below, so the frame it
	// produces is picked up in this same render pass rather than one later.
	// render() no-ops unless the panel actually has a pane.
	//
	// A HIDDEN panel is skipped: painting it would set its dirty flag, and the
	// sweep below would then order a full repaint every second for a pane that
	// is not on screen — which is the dirty-flag model inverted. It is marked
	// dirty on the way back in (togglePanel), so nothing is lost.
	if !m.panelHiddenLocked() {
		m.control.render()
	}

	// Check if any pane has new content
	anyDirty := false
	for _, p := range m.livePanesLocked(nil) {
		if p.hidden {
			continue // not on screen; its dirty flag is not a reason to paint
		}
		p.mu.Lock()
		if p.dirty {
			anyDirty = true
			p.dirty = false
		}
		p.mu.Unlock()
	}
	if !anyDirty {
		// Even if no content changed, update cursor position (cheap). Cheap to
		// BUILD, that is — it still goes out through render(), because a write
		// to a blocked tty is not cheap at all.
		if f := m.focused; f != nil && f.screen != nil {
			s := f.screen
			f.mu.Lock()
			y, x, scrolled := s.curY, s.curX, s.sbOff > 0
			f.mu.Unlock()
			// A scrolled-back pane has no cursor to show: the child's cursor is
			// on a line that is not on screen, and parking the terminal's cursor
			// where that line WOULD be points at somebody else's text.
			if scrolled {
				return events, "", false
			}
			return events, fmt.Sprintf("\x1b[%d;%dH", f.y+y+1, f.x+x+1), false
		}
		return events, "", false
	}

	r := &m.renderer
	r.reset()
	r.hideCursor()
	r.renderPane(m.root)

	// Selection highlight overlay
	if sel.pane != nil && (sel.active || (sel.sy != sel.ey || sel.sx != sel.ex)) {
		r.renderSelection(sel.pane)
	}

	// Status bar. Hidden by --no-status / Ctrl-G s, in which case the row it
	// would have taken already belongs to the layout (reflowLocked).
	if m.statusText == "" {
		// magmux's name and the chord, held to the terminal's width like every
		// other form this row can take.
		m.statusText = appendKeyHint(
			fitStatusBase(m.cols-hintFloorWidth(hints), "*: "+magmuxLabel()), hints, m.cols)
	}
	if !m.hideStatus {
		text := m.statusText
		// While Ctrl-G is armed the row belongs to the chord. Nothing else on
		// screen can teach its second keys, and it is gone again on the very
		// next keystroke — the panes are not touched either way.
		if m.chordArmed {
			if menu := m.chordMenuLocked(hints); menu != "" {
				text = menu
			}
		}
		// The refusal note rides in front of the run's own text rather than
		// being written into it: it belongs to the keystroke that caused it.
		// If the two together would overrun, the keystroke wins — the run's
		// summary is back on the next frame, the refusal is not.
		if n := m.chromeNoteLocked(); n != "" {
			note := "R: " + n
			if text != "" && approxStatusWidth(note+"\t"+text) <= m.cols {
				note += "\t" + text
			}
			text = note
		}
		r.renderStatusBar(m.rows-1, m.cols, text)
	}

	// Show cursor at focused pane position — unless it is scrolled back, in
	// which case the cursor belongs to a line that is not on screen and the
	// frame's opening hideCursor is left to stand.
	if f := m.focused; f != nil && f.screen != nil {
		s := f.screen
		f.mu.Lock()
		y, x, scrolled := s.curY, s.curX, s.sbOff > 0
		f.mu.Unlock()
		if !scrolled {
			r.showCursor(f.y+y, f.x+x)
		}
	}

	// The bytes go back to render(), which writes them once the lock is off.
	return events, r.frame(), false
}

// cleanup reaps every child and waits for its read loop to finish.
//
// m.closing is set under treeMu BEFORE wg.Wait, and OpenPane refuses under the
// same lock: an open_pane landing between the two would call wg.Add while
// wg.Wait was running, which panics with "WaitGroup misuse: Add called
// concurrently with Wait".
//
// Caller must NOT hold treeMu.
func (m *Magmux) cleanup() {
	m.treeMu.Lock()
	m.closing = true
	panes := m.livePanesLocked(nil)
	m.treeMu.Unlock()

	// Signalling and closing PTYs is blocking I/O, so it happens off the lock.
	for _, p := range panes {
		if p.cmd != nil && p.cmd.Process != nil {
			p.cmd.Process.Signal(syscall.SIGHUP)
		}
		if p.ptmx != nil {
			p.ptmx.Close()
		}
	}
	m.wg.Wait()
}

// ── PaneConfig ────────────────────────────────────────────────────────────────

type PaneConfig struct {
	Cmd  string
	Args []string
	// Dir is the child's working directory; empty inherits magmux's own.
	// Setting it is what makes `open_pane` with a cwd real — and it is also
	// why ClaudeCodeController.Start consults p.cmd.Dir before os.Getwd(),
	// which is no longer this pane's directory.
	Dir string
	// Env is appended after magmux's own TERM/COLUMNS/LINES/MAGMUX_SOCK block,
	// so a caller can override any of them.
	Env []string
	// Label is a short human name for the pane, echoed back in `list` so a
	// client can address panes by name instead of by index.
	Label string
	// Control makes this a control-panel pane instead of a child process:
	// magmux paints it itself and Cmd/Args are ignored.
	Control bool
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func runeWidth(r rune) int {
	if r < 0x20 || r == 0x7f {
		return 0
	}
	// Common fast path: ASCII
	if r < 0x80 {
		return 1
	}
	// CJK ranges (rough check for wide chars)
	if (r >= 0x1100 && r <= 0x115f) ||
		r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x20000 && r <= 0x2fffd) ||
		(r >= 0x30000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// ── Main ──────────────────────────────────────────────────────────────────────

// getUserShell returns the user's preferred shell (matching MTM's getshell())
func getUserShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

func main() {
	// The MCP server hook must be the FIRST statement here: it has to come
	// before the --version/--help scan below (which walks every argument and
	// would claim a --help meant for `magmux mcp`), and long before init()
	// puts the tty in raw mode or anything writes to stdout — a single stray
	// byte on stdout desynchronises a JSON-RPC client for the rest of the
	// session.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(runMCP(os.Args[2:]))
	}

	// Handle --version / -v / --help / -h first pass
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-v" {
			fmt.Printf("magmux %s (%s)\n", Version, Commit)
			os.Exit(0)
		}
		if arg == "--help" || arg == "-h" {
			fmt.Println("magmux — Minimal Go Terminal Multiplexer")
			fmt.Printf("Version: %s (%s)\n\n", Version, Commit)
			fmt.Println("Usage: magmux [options]")
			fmt.Println("       magmux -e 'command1' -e 'command2'")
			fmt.Println("       magmux -g gridfile.txt")
			fmt.Println()
			fmt.Println("Options:")
			fmt.Println("  -g FILE   Grid file (one command per line, overrides -e)")
			fmt.Println("  -e CMD    Run CMD in a pane (can be repeated)")
			fmt.Println("  -w        Auto-exit when all panes are done (dead or idle)")
			fmt.Println("  -c        Start with the control panel visible (it always exists;")
			fmt.Println("            without -c it starts hidden — Ctrl-G p reveals it)")
			fmt.Println("  --no-status   Start with the status bar hidden (Ctrl-G s toggles)")
			fmt.Println("  -x SECS   Close SECS after a pilot finishes (default: wait for a keypress)")
			fmt.Println("  --id NAME Bind /tmp/magmux-NAME.sock instead of the pid socket ([A-Za-z0-9_-])")
			fmt.Println("  --theme MODE  light | dark | auto (default: auto, via OSC 11)")
			fmt.Println("  -v        Show version")
			fmt.Println("  -h        Show this help")
			fmt.Println()
			fmt.Println("Grid file format:")
			fmt.Println("  # Lines starting with # are comments")
			fmt.Println("  # Blank lines are skipped")
			fmt.Println("  echo 'hello world'")
			fmt.Println("  sleep 5 && echo done")
			fmt.Println()
			fmt.Println("Controls:")
			fmt.Println("  Ctrl-G q      Quit (always)")
			fmt.Println("  ↑/↓ PgUp/PgDn Scroll the control panel (when focused); wheel works anywhere")
			fmt.Println("  End / G       Control panel: resume following the newest exchange")
			fmt.Println("  q / Esc       Quit (when all panes done, grid mode)")
			fmt.Println("  Ctrl-G Tab    Switch focus to next pane")
			fmt.Println("  Ctrl-G p      Show / hide the control panel (keeps its history)")
			fmt.Println("  Ctrl-G s      Show / hide the status bar")
			fmt.Println("  Ctrl-G [      Scroll the focused pane back through its scrollback.")
			fmt.Println("                Then k/j ↑/↓ PgUp/PgDn g/G move, q / Enter / Esc go live.")
			fmt.Println("                Alternate-screen panes (Claude Code, vim, htop) keep none.")
			fmt.Println("  Mouse wheel   Scroll a non-alt pane's scrollback; forwarded to TUIs")
			fmt.Println("  Mouse click   Switch focus to clicked pane")
			fmt.Println("  Mouse drag    Select text (auto-copies to clipboard)")
			fmt.Println()
			fmt.Println("IPC Socket:")
			fmt.Println("  /tmp/magmux-{pid}.sock — JSON line protocol")
			fmt.Println("  /tmp/magmux-{name}.sock with --id NAME (known before magmux starts)")
			fmt.Println("  Env var MAGMUX_SOCK exported to child processes")
			fmt.Println()
			fmt.Println("Agent Status Monitoring:")
			fmt.Println("  Send agent hook events via IPC socket:")
			fmt.Println("  {\"type\":\"agent\",\"pane\":0,\"event\":\"Stop\",\"project\":\"myapp\"}")
			fmt.Println()
			fmt.Println("Controlled Sessions:")
			fmt.Println("  An external agent (the \"pilot\") reads pane state off the socket and")
			fmt.Println("  pushes the next instruction back in. Run with -c to watch the traffic.")
			fmt.Println()
			fmt.Println("  {\"type\":\"pilot\",\"event\":\"start\",\"pane\":0,\"goal\":\"...\",\"steps\":3}")
			fmt.Println("  {\"type\":\"send\",\"pane\":0,\"text\":\"run the tests\",\"label\":\"step 1/3\"}")
			fmt.Println("  {\"type\":\"send\",\"pane\":0,\"keys\":[\"escape\"],\"enter\":false}")
			fmt.Println("  {\"type\":\"pilot\",\"event\":\"finish\",\"summary\":\"all green\"}")
			fmt.Println()
			fmt.Println("  `send` writes to a pane even when it is idle — steering a finished")
			fmt.Println("  turn is the point — and clears its done state like a real keystroke.")
			fmt.Println()
			fmt.Println("Pane lifecycle (a message with an \"id\" gets one `reply` back):")
			fmt.Println("  {\"type\":\"open_pane\",\"id\":1,\"cmd\":\"claude\",\"cwd\":\"/proj\",\"split\":\"vertical\"}")
			fmt.Println("  {\"type\":\"close_pane\",\"id\":2,\"pane\":1,\"force\":false}")
			fmt.Println("  {\"type\":\"focus\",\"id\":3,\"pane\":0}")
			fmt.Println()
			fmt.Println("  Pane indices are permanent: closing a pane leaves a tombstone rather")
			fmt.Println("  than renumbering, so ids go sparse and never move under a caller.")
			fmt.Println()
			fmt.Println("  Events: SessionStart, UserPromptSubmit, PreToolUse, PostToolUse,")
			fmt.Println("          Stop, Notification, PermissionRequest, PreCompact, PostCompact,")
			fmt.Println("          SessionEnd")
			fmt.Println()
			fmt.Println("  Claude Code hook example (.claude/settings.json):")
			fmt.Println("    {\"hooks\":{\"Stop\":[{\"matcher\":\".*\",\"hooks\":[{")
			fmt.Println("      \"type\":\"command\",")
			fmt.Println("      \"command\":\"echo '{\\\"type\\\":\\\"agent\\\",\\\"pane\\\":0,\\\"event\\\":\\\"Stop\\\"}' | socat - UNIX:$MAGMUX_SOCK\"")
			fmt.Println("    }]}]}}")
			fmt.Println()
			fmt.Println("Environment:")
			fmt.Println("  MAGMUX_THEME    light | dark | auto (default: auto). magmux asks the")
			fmt.Println("                  terminal for its background colour (OSC 11) and picks a")
			fmt.Println("                  palette; set this when a terminal answers wrongly.")
			fmt.Println("                  --theme wins over it.")
			fmt.Println("  MAGMUX_SCROLLBACK  Lines of history kept per pane (default: 1000, 0 off).")
			fmt.Println("                  Only the primary screen records; the ring fills lazily,")
			fmt.Println("                  so a pane that never scrolls costs nothing.")
			fmt.Println("  MAGMUX_SEL_FG   Selection foreground (256-color index, default: 0)")
			fmt.Println("  MAGMUX_SEL_BG   Selection background (256-color index, default: 220)")
			fmt.Println("  MAGMUX_DEBUG    Enable debug logging to /tmp/magmux-debug.log")
			os.Exit(0)
		}
	}

	shell := getUserShell()

	// Parse flags
	args := os.Args[1:]
	var gridFile string
	var customCmds []PaneConfig
	autoExit := false
	withControl := false
	noStatus := false
	var autoClose time.Duration
	var sockID string
	var themePref string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-g":
			if i+1 < len(args) {
				i++
				gridFile = args[i]
			}
		case "-e":
			if i+1 < len(args) {
				i++
				customCmds = append(customCmds, PaneConfig{
					Cmd:  shell,
					Args: []string{"-l", "-c", args[i]},
				})
			}
		case "-w":
			autoExit = true
		case "-c", "--control":
			// Now means "start with the panel VISIBLE". Every session gets a
			// panel either way; without this it starts hidden and the sessions
			// have the whole terminal. Kept on the same flag deliberately, so
			// `magmux -c …` looks exactly as it always has.
			withControl = true
		case "--no-status":
			noStatus = true
		case "-x", "--close-after":
			if i+1 < len(args) {
				i++
				var secs int
				if _, err := fmt.Sscanf(args[i], "%d", &secs); err == nil && secs > 0 {
					autoClose = time.Duration(secs) * time.Second
				}
			}
		case "--theme":
			if i+1 < len(args) {
				i++
				if validThemeSetting(args[i]) {
					themePref = args[i]
				} else {
					// Ignored rather than fatal, like --id: auto-detection
					// still runs, so a typo costs the chosen palette and
					// nothing else. Said out loud, because "my --theme did
					// nothing" is otherwise silent.
					fmt.Fprintf(os.Stderr, "magmux: ignoring --theme %q (want light, dark or auto)\n", args[i])
				}
			}
		case "--id":
			if i+1 < len(args) {
				i++
				if validSocketID(args[i]) {
					sockID = args[i]
				} else {
					// Ignored rather than fatal: the pid socket still binds, so
					// a bad name costs the caller its chosen path and nothing
					// else. Said out loud, because an agent polling
					// /tmp/magmux-<name>.sock would otherwise just hang.
					fmt.Fprintf(os.Stderr, "magmux: ignoring --id %q (allowed: A-Z a-z 0-9 _ -, max 64)\n", args[i])
				}
			}
		}
	}

	// Determine commands and mode
	useGrid := false
	var commands []PaneConfig

	if gridFile != "" {
		// -g overrides -e
		absPath, err := filepath.Abs(gridFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
		cmds, err := parseGridFile(absPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
		if len(cmds) == 0 {
			fmt.Fprintf(os.Stderr, "magmux: grid file has no commands\n")
			os.Exit(1)
		}
		commands = cmds
		useGrid = true
	} else if len(customCmds) > 0 {
		commands = customCmds
		useGrid = true
	} else if withControl {
		// -c with no commands is a bare control panel: useful for watching a
		// pilot drive nothing yet, and it keeps the flag from silently
		// falling through to the default 3-shell layout.
		commands = nil
		useGrid = true
	} else {
		// Default: 3 panes running user's login shell
		commands = []PaneConfig{
			{Cmd: shell, Args: []string{"-l"}},
			{Cmd: shell, Args: []string{"-l"}},
			{Cmd: shell, Args: []string{"-l"}},
		}
	}

	// The control panel is the last pane, so session panes keep the indices a
	// pilot would naturally use (pane 0 is the first -e command).
	//
	// Only when it starts VISIBLE. A panel that starts hidden is installed
	// after the layout is built (installHiddenPanel) rather than handed to the
	// builders, so the visible layout is byte-identical to a magmux with no
	// panel at all — see that function for why the builders cannot take it.
	if withControl && useGrid {
		commands = append(commands, PaneConfig{Control: true})
	}

	mux := &Magmux{
		gridMode:       useGrid,
		autoExit:       autoExit,
		hideStatus:     noStatus,
		autoCloseAfter: autoClose,
		sockID:         sockID,
		themePref:      themePref,
		sockDone:       make(chan struct{}),
		layoutReady:    make(chan struct{}),
		control:        newControlPanel(),
		controllerFactories: []ControllerFactory{
			claudeCodeFactory,
		},
	}

	if err := mux.init(); err != nil {
		fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
		os.Exit(1)
	}
	defer mux.restore()

	// Publish MAGMUX_SOCK synchronously. socketServer does this too, but it
	// runs on its own goroutine and the sleep below is a delay, not a
	// synchronisation primitive: on a loaded machine that goroutine can still
	// be unscheduled when the first child forks, and the child then inherits an
	// empty MAGMUX_SOCK with nothing to say why. The path is derived from the
	// pid or --id, so it is known before the listener exists.
	mux.sockPath = mux.socketPath()
	os.Setenv("MAGMUX_SOCK", mux.sockPath)

	// Start socket server before spawning children (so MAGMUX_SOCK is set)
	go mux.socketServer()
	// A connection is accepted from this moment on, but the layout does not
	// exist yet, so nothing is served until markLayoutReady below. The defer is
	// the safety net: an early os.Exit takes the waiters with the process, but
	// any other way out of main must release them or a client sits on the
	// timeout for no reason.
	defer mux.markLayoutReady()
	// Give the socket a moment to bind before spawning children
	sleepMs(10)

	if useGrid {
		if err := mux.buildGrid(commands); err != nil {
			mux.restore()
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := mux.buildLayout(commands); err != nil {
			mux.restore()
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
	}

	// Every session gets a panel, whether or not -c was passed; without -c it
	// starts hidden and Ctrl-G p reveals it. This runs after the layout so the
	// panel takes the last id and the session layout is untouched.
	if !withControl {
		mux.installHiddenPanel()
	}

	// Bind the panel to whichever pane was built for it, and keep focus on a
	// real session — typing into the panel does nothing, so starting there
	// would look broken.
	//
	// Under treeMu because the socket server is already accepting connections:
	// a client that connects right now runs buildPaneResults, which reads
	// m.focused. The panel's own pane pointer is cp.mu's (attach takes it), and
	// the order treeMu -> cp.mu is the documented one.
	mux.treeMu.Lock()
	live := mux.livePanesLocked(nil)
	for _, p := range live {
		if p.isControl {
			mux.control.attach(p)
			// The pane Ctrl-G p toggles. Already set on the hidden path; this
			// is the -c one, where the builders made it as an ordinary pane.
			mux.panel = p
			if mux.focused == p {
				for _, q := range live {
					if !q.isControl {
						mux.focused = q
						break
					}
				}
			}
		}
	}
	mux.treeMu.Unlock()
	mux.control.markDirty()

	mux.startReadLoops()
	mux.attachControllers()

	// The layout is now real AND live: panes exist, their read loops are
	// running and their controllers are attached. Only now is the connect-time
	// aggregate worth anything — published here rather than straight after
	// buildGrid so a client's first snapshot describes panes that can actually
	// change state, not a layout still being wired up.
	mux.markLayoutReady()

	// Start a waiter for every child, grid mode or not: waitForChild is the
	// only caller of cmd.Wait, so gating it on grid mode left the default
	// three-shell layout leaking a zombie per exited shell. The grid-only part
	// (the ✓ DONE / ✗ FAIL tombstone and the `exit` event) is gated inside
	// waitForChild by p.gridMode, so nothing about non-grid rendering changes.
	for _, p := range live {
		if p.isControl {
			continue // no child to wait on
		}
		go mux.waitForChild(p)
	}

	mux.handleSIGWINCH()

	go mux.renderLoop()
	mux.inputLoop()

	// The socket server broadcasts the final `results` event and then closes
	// each subscriber, giving it a clean EOF *after* that event. That happens
	// in a goroutine woken by m.quit, so we must let it finish — exiting here
	// races it and drops results entirely.
	mux.waitSocketShutdown(3 * time.Second)

	// Cleanup socket (normally already removed by the teardown above)
	if mux.sockPath != "" {
		os.Remove(mux.sockPath)
	}

	mux.cleanup()
}

// Suppress unused import warning
var _ = io.EOF
var _ = math.MaxInt
