// magmux — Minimal Go Terminal Multiplexer
// Port of MTM (Rob King) from C to Go, zero third-party dependencies.
// Uses only golang.org/x/sys and golang.org/x/term.
package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Set by GoReleaser ldflags
var (
	Version = "dev"
	Commit  = "none"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	scrollbackLines = 1000
	maxParams       = 16
	maxOSC          = 256
	commandKey      = 'g' // Ctrl-G prefix
)

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
	Index int16 // -1=default, 0-255=indexed
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
}

func newScreen(rows, cols int) *Screen {
	s := &Screen{
		rows:      rows,
		cols:      cols,
		fg:        defaultColor,
		bg:        defaultColor,
		scrollBot: rows,
		autoWrap:  true,
	}
	s.cells = makeGrid(rows+scrollbackLines, cols)
	return s
}

func makeGrid(rows, cols int) [][]Cell {
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
	totalRows := rows + scrollbackLines
	s.cells = makeGrid(totalRows, cols)
	// Copy what fits
	copyRows := min(oldRows, totalRows)
	for i := 0; i < copyRows; i++ {
		copyCols := min(oldCols, cols)
		for j := 0; j < copyCols; j++ {
			s.cells[i][j] = old[i][j]
		}
	}
	s.rows = rows
	s.cols = cols
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

func (s *Screen) scrollUp(top, bot int) {
	if top >= bot || top < 0 || bot > len(s.cells) {
		return
	}
	// Shift rows up by 1 within [top, bot)
	save := s.cells[top]
	copy(s.cells[top:bot-1], s.cells[top+1:bot])
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
	state  vtState
	inter  rune
	narg   int
	args   [maxParams]int
	nosc   int
	oscbuf [maxOSC]rune
	node   *Pane // back-reference to pane
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
	for len(data) > 0 {
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
			// Terminate the OSC
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
			// OSC complete — ignore for POC
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
					if set {
						if s.altScreen == nil {
							s.altScreen = newScreen(s.rows, s.cols)
						}
						vt.node.screen = s.altScreen
						vt.node.altMode = true
					} else if s.altScreen != nil {
						vt.node.screen = vt.node.primaryScreen
						vt.node.altMode = false
					}
				case 1000, 1002, 1003, 1006: // Mouse tracking — consumed by magmux
				case 1004: // Focus events
					vt.node.focusEvents = set
				case 1047: // Alt screen (variant 2)
					if set {
						if s.altScreen == nil {
							s.altScreen = newScreen(s.rows, s.cols)
						}
						vt.node.screen = s.altScreen
						vt.node.altMode = true
					} else if s.altScreen != nil {
						vt.node.screen = vt.node.primaryScreen
						vt.node.altMode = false
					}
				case 1049: // Alt screen buffer + cursor save
					vt.node.altMode = set
					if set {
						if s.altScreen == nil {
							s.altScreen = newScreen(s.rows, s.cols)
						}
						vt.node.screen = s.altScreen
					} else if s.altScreen != nil {
						vt.node.screen = vt.node.primaryScreen
					}
				case 2004: // Bracketed paste mode
					vt.node.bracketPaste = set
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
		top := vt.p1(0) - 1
		bot := vt.p1(1)
		if bot == 0 || bot > s.rows {
			bot = s.rows
		}
		if top < bot {
			s.scrollTop = top
			s.scrollBot = bot
		}
		s.curX = 0
		s.curY = 0
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
	splitType     SplitType
	y, x, h, w   int // position and size in host terminal
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
}

func newPane(y, x, h, w int, command string, args ...string) (*Pane, error) {
	p := &Pane{
		y:         y,
		x:         x,
		h:         h,
		w:         w,
		ratio:     0.5,
		charsetG0: 'B', // ASCII
		charsetG1: 'B',
	}
	p.screen = newScreen(h, w)
	p.primaryScreen = p.screen
	p.vt.node = p

	if err := p.spawnPTY(command, args...); err != nil {
		return nil, fmt.Errorf("spawn PTY: %w", err)
	}
	return p, nil
}

func (p *Pane) spawnPTY(command string, args ...string) error {
	ptmx, pts, err := openPTY()
	if err != nil {
		return err
	}

	// Set initial size
	setWinSize(ptmx, p.h, p.w)

	cmd := exec.Command(command, args...)
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}
	cmd.Env = append(os.Environ(),
		"TERM=screen-256color",
		fmt.Sprintf("COLUMNS=%d", p.w),
		fmt.Sprintf("LINES=%d", p.h),
	)

	if err := cmd.Start(); err != nil {
		pts.Close()
		ptmx.Close()
		return err
	}
	pts.Close() // parent doesn't need slave side

	p.ptmx = ptmx
	p.cmd = cmd
	return nil
}

func (p *Pane) writePTY(data []byte) {
	if p.ptmx != nil {
		p.ptmx.Write(data)
	}
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
		p.screen.resize(h, w)
		if p.screen.altScreen != nil {
			p.screen.altScreen.resize(h, w)
		}
		p.mu.Unlock()
		if p.ptmx != nil {
			setWinSize(p.ptmx, h, w)
		}
	} else {
		p.reshapeChildren()
	}
}

func (p *Pane) reshapeChildren() {
	if p.splitType == SplitHorizontal {
		w1 := int(float64(p.w) * p.ratio)
		w2 := p.w - w1 - 1 // -1 for border
		p.child1.resize(p.y, p.x, p.h, w1)
		p.child2.resize(p.y, p.x+w1+1, p.h, w2)
	} else if p.splitType == SplitVertical {
		h1 := int(float64(p.h) * p.ratio)
		h2 := p.h - h1 - 1 // -1 for border
		p.child1.resize(p.y, p.x, h1, p.w)
		p.child2.resize(p.y+h1+1, p.x, h2, p.w)
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

func (r *Renderer) renderPane(p *Pane) {
	if p.splitType != SplitNone {
		r.renderPane(p.child1)
		r.renderBorder(p)
		r.renderPane(p.child2)
		return
	}

	p.mu.Lock()
	s := p.screen
	for row := 0; row < s.rows && row < p.h; row++ {
		r.moveTo(p.y+row, p.x)
		for col := 0; col < s.cols && col < p.w; col++ {
			c := s.cells[row][col]
			if c.Cont {
				continue // skip continuation cells
			}
			r.setAttr(c.Fg, c.Bg, c.Attr)
			if c.Ch == 0 || c.Ch == ' ' {
				r.buf.WriteByte(' ')
			} else {
				r.buf.WriteRune(c.Ch)
			}
		}
	}
	p.mu.Unlock()
}

func (r *Renderer) renderBorder(p *Pane) {
	r.setAttr(Color{Index: 8}, defaultColor, AttrDim) // gray dim border
	if p.splitType == SplitHorizontal {
		// Border at the exact column between child1 and child2
		// child1 ends at p.x + child1.w, border is at that position
		bx := p.child1.x + p.child1.w
		for row := 0; row < p.h; row++ {
			r.moveTo(p.y+row, bx)
			r.buf.WriteString("│")
		}
	} else if p.splitType == SplitVertical {
		// Border at the exact row between child1 and child2
		by := p.child1.y + p.child1.h
		r.moveTo(by, p.x)
		for col := 0; col < p.w; col++ {
			r.buf.WriteString("─")
		}
	}
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

func (r *Renderer) renderStatusBar(row, cols int, text string) {
	r.moveTo(row, 0)
	// Dark background for status bar
	r.setAttr(defaultColor, defaultColor, AttrDim)
	fmt.Fprintf(&r.buf, "\x1b[48;5;236m")
	r.prevBg = Color{Index: -2} // force reset next

	// Parse and render status segments (tab-separated, COLOR:text)
	segments := strings.Split(text, "\t")
	col := 1
	for _, seg := range segments {
		if col > 1 {
			r.buf.WriteString(" ")
			col++
		}
		parts := strings.SplitN(seg, ":", 2)
		if len(parts) == 2 {
			color := strings.TrimSpace(parts[0])
			txt := strings.TrimSpace(parts[1])
			switch color {
			case "M": // Magenta
				fmt.Fprintf(&r.buf, "\x1b[1;35m")
			case "C": // Cyan
				fmt.Fprintf(&r.buf, "\x1b[1;36m")
			case "G": // Green
				fmt.Fprintf(&r.buf, "\x1b[1;32m")
			case "R": // Red
				fmt.Fprintf(&r.buf, "\x1b[1;31m")
			case "Y": // Yellow
				fmt.Fprintf(&r.buf, "\x1b[1;33m")
			case "W": // White
				fmt.Fprintf(&r.buf, "\x1b[37m")
			case "D": // Dim
				fmt.Fprintf(&r.buf, "\x1b[2;37m")
			default:
				fmt.Fprintf(&r.buf, "\x1b[37m")
			}
			r.buf.WriteString(txt)
			col += len(txt)
		} else {
			r.buf.WriteString(seg)
			col += len(seg)
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

func (r *Renderer) flush() {
	os.Stdout.WriteString(r.buf.String())
}

// ── Multiplexer ───────────────────────────────────────────────────────────────

type Magmux struct {
	root       *Pane
	focused    *Pane
	allPanes   []*Pane // leaf panes only
	rows, cols int
	statusText string
	renderer   Renderer
	rawState   *term.State
	quit       chan struct{}
	wg         sync.WaitGroup
}

func (m *Magmux) init() error {
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

	return nil
}

func (m *Magmux) restore() {
	// Disable mouse + show cursor + exit alternate screen
	os.Stdout.WriteString("\x1b[?1006l\x1b[?1002l\x1b[?1000l\x1b[?25h\x1b[?1049l")
	if m.rawState != nil {
		term.Restore(int(os.Stdin.Fd()), m.rawState)
	}
}

func (m *Magmux) buildLayout(commands []PaneConfig) error {
	statusH := 1
	availH := m.rows - statusH

	if len(commands) == 0 {
		return fmt.Errorf("no commands specified")
	}

	// Special layout for POC: top half split horizontal, bottom pane, status bar
	switch len(commands) {
	case 1:
		p, err := newPane(0, 0, availH, m.cols, commands[0].Cmd, commands[0].Args...)
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
		p1, err := newPane(0, 0, availH, w1, commands[0].Cmd, commands[0].Args...)
		if err != nil {
			return err
		}
		p2, err := newPane(0, w1+1, availH, w2, commands[1].Cmd, commands[1].Args...)
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
		p1, err := newPane(0, 0, topH, w1, commands[0].Cmd, commands[0].Args...)
		if err != nil {
			return err
		}
		p2, err := newPane(0, w1+1, topH, w2, commands[1].Cmd, commands[1].Args...)
		if err != nil {
			return err
		}
		topPane.child1 = p1
		topPane.child2 = p2
		p1.parent = topPane
		p2.parent = topPane

		// Bottom pane
		p3, err := newPane(topH+1, 0, botH, m.cols, commands[2].Cmd, commands[2].Args...)
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

	return nil
}

func (m *Magmux) startReadLoops() {
	for _, p := range m.allPanes {
		m.wg.Add(1)
		go p.readLoop(&m.wg)
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
				m.rows = h
				m.cols = w
				statusH := 1
				m.root.resize(0, 0, h-statusH, w)
			case <-m.quit:
				return
			}
		}
	}()
}

func (m *Magmux) focusNext() {
	for i, p := range m.allPanes {
		if p == m.focused {
			m.focused = m.allPanes[(i+1)%len(m.allPanes)]
			return
		}
	}
}

// findPaneAt returns the leaf pane at terminal coordinates (row, col)
func (m *Magmux) findPaneAt(row, col int) *Pane {
	return findPaneAtRecursive(m.root, row, col)
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

func (m *Magmux) inputLoop() {
	// Buffered input reader — accumulates partial reads so escape sequences
	// that span multiple read() calls are handled correctly.
	inbuf := make([]byte, 0, 4096)
	raw := make([]byte, 4096)
	commandMode := false

	for {
		n, err := os.Stdin.Read(raw)
		if err != nil {
			return
		}
		inbuf = append(inbuf, raw[:n]...)

		for len(inbuf) > 0 {
			b := inbuf[0]

			if commandMode {
				commandMode = false
				switch b {
				case 'q':
					close(m.quit)
					return
				case '\t', 'o':
					m.focusNext()
				default:
					m.focused.writePTY([]byte{0x07, b})
				}
				inbuf = inbuf[1:]
				continue
			}

			if b == commandKey&0x1f { // Ctrl-G
				commandMode = true
				inbuf = inbuf[1:]
				continue
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
				m.focused.writePTY(inbuf[:1])
				inbuf = inbuf[1:]
				continue
			}

			// Regular byte — pass through to focused pane
			// Find the extent of non-escape bytes to batch-write
			end := 1
			for end < len(inbuf) && inbuf[end] != 0x1b && inbuf[end] != commandKey&0x1f {
				end++
			}
			m.focused.writePTY(inbuf[:end])
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
				m.focused.writePTY(buf[:end+1])
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
		m.focused.writePTY(buf[:3])
		return 3, true
	}

	// Default: ESC + char, forward both
	m.focused.writePTY(buf[:2])
	return 2, true
}

// ── Selection state (matches MTM's sel_* globals) ─────────────────────────────

type Selection struct {
	active bool
	pane   *Pane
	sy, sx int // start (pane-relative)
	ey, ex int // end (pane-relative)
}

var sel Selection

func (m *Magmux) selClear() {
	sel.active = false
	sel.pane = nil
}

func (m *Magmux) selCopy() {
	if sel.pane == nil {
		return
	}
	s := sel.pane.screen

	// Normalize start/end
	sy, sx, ey, ex := sel.sy, sel.sx, sel.ey, sel.ex
	if sy > ey || (sy == ey && sx > ex) {
		sy, sx, ey, ex = ey, ex, sy, sx
	}

	// Extract text line by line from screen buffer
	var lines []string
	sel.pane.mu.Lock()
	for r := sy; r <= ey && r < s.rows; r++ {
		cs := 0
		ce := s.cols - 1
		if r == sy {
			cs = sx
		}
		if r == ey {
			ce = ex
		}
		var line strings.Builder
		for c := cs; c <= ce && c < s.cols; c++ {
			ch := s.cells[r][c].Ch
			if ch == 0 {
				ch = ' '
			}
			if !s.cells[r][c].Cont {
				line.WriteRune(ch)
			}
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	sel.pane.mu.Unlock()

	content := strings.Join(lines, "\n")
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

	// Deselect after copy
	sel.pane = nil
	sel.active = false
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

	// Always: left click press switches focus (even in alt mode)
	if press && btn == 0 {
		if target := m.findPaneAt(row0, col0); target != nil {
			m.focused = target
		}
	}

	// If focused pane is in alternate screen (vim, htop, Claude Code, OpenCode),
	// forward ALL mouse events to it — like tmux does.
	if m.focused != nil && m.focused.altMode {
		localRow := row0 - m.focused.y + 1
		localCol := col0 - m.focused.x + 1
		if localRow < 1 {
			localRow = 1
		}
		if localCol < 1 {
			localCol = 1
		}
		fwd := fmt.Sprintf("\x1b[<%d;%d;%d%c", btn, localCol, localRow, termChar)
		m.focused.writePTY([]byte(fwd))
		return end + 1, true
	}

	// Normal mode (bash, etc.): handle mouse ourselves for selection
	switch {
	case press && btn == 0: // Left click → start selection
		m.selClear()
		if m.focused != nil {
			sel.pane = m.focused
			sel.active = true
			sel.sy = row0 - m.focused.y
			sel.sx = col0 - m.focused.x
			sel.ey = sel.sy
			sel.ex = sel.sx
		}

	case press && btn == 32: // Drag
		if sel.active && sel.pane != nil {
			sel.ey = clamp(row0-sel.pane.y, 0, sel.pane.h-1)
			sel.ex = clamp(col0-sel.pane.x, 0, sel.pane.w-1)
		}

	case !press && btn == 0: // Release → copy
		if sel.active && sel.pane != nil {
			sel.ey = clamp(row0-sel.pane.y, 0, sel.pane.h-1)
			sel.ex = clamp(col0-sel.pane.x, 0, sel.pane.w-1)
			if sel.sy != sel.ey || sel.sx != sel.ex {
				m.selCopy()
			}
			sel.active = false
		}
	}

	return end + 1, true
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

func (m *Magmux) render() {
	// Check if any pane has new content
	anyDirty := false
	for _, p := range m.allPanes {
		p.mu.Lock()
		if p.dirty {
			anyDirty = true
			p.dirty = false
		}
		p.mu.Unlock()
	}
	if !anyDirty {
		// Even if no content changed, update cursor position (cheap)
		if m.focused != nil && m.focused.screen != nil {
			s := m.focused.screen
			fmt.Fprintf(os.Stdout, "\x1b[%d;%dH", m.focused.y+s.curY+1, m.focused.x+s.curX+1)
		}
		return
	}

	r := &m.renderer
	r.reset()
	r.hideCursor()
	r.renderPane(m.root)

	// Selection highlight overlay
	if sel.pane != nil && (sel.active || (sel.sy != sel.ey || sel.sx != sel.ex)) {
		r.renderSelection(sel.pane)
	}

	// Status bar
	if m.statusText == "" {
		m.statusText = "C: magmux\tD: press Ctrl-G then q to quit\tD: Ctrl-G Tab to switch panes"
	}
	r.renderStatusBar(m.rows-1, m.cols, m.statusText)

	// Show cursor at focused pane position
	if m.focused != nil && m.focused.screen != nil {
		s := m.focused.screen
		r.showCursor(m.focused.y+s.curY, m.focused.x+s.curX)
	}

	r.flush()
}

func (m *Magmux) cleanup() {
	for _, p := range m.allPanes {
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
	// Handle --version / -v
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-v" {
			fmt.Printf("magmux %s (%s)\n", Version, Commit)
			os.Exit(0)
		}
		if arg == "--help" || arg == "-h" {
			fmt.Println("magmux — Minimal Go Terminal Multiplexer")
			fmt.Printf("Version: %s (%s)\n\n", Version, Commit)
			fmt.Println("Usage: magmux [options]")
			fmt.Println("       magmux -e 'command1' -e 'command2' -e 'command3'")
			fmt.Println()
			fmt.Println("Options:")
			fmt.Println("  -e CMD    Run CMD in a pane (can be repeated)")
			fmt.Println("  -v        Show version")
			fmt.Println("  -h        Show this help")
			fmt.Println()
			fmt.Println("Controls:")
			fmt.Println("  Ctrl-G q      Quit")
			fmt.Println("  Ctrl-G Tab    Switch focus to next pane")
			fmt.Println("  Mouse click   Switch focus to clicked pane")
			fmt.Println("  Mouse drag    Select text (auto-copies to clipboard)")
			fmt.Println()
			fmt.Println("Environment:")
			fmt.Println("  MAGMUX_SEL_FG   Selection foreground (256-color index, default: 0)")
			fmt.Println("  MAGMUX_SEL_BG   Selection background (256-color index, default: 220)")
			fmt.Println("  MAGMUX_DEBUG    Enable debug logging to /tmp/magmux-debug.log")
			os.Exit(0)
		}
	}

	shell := getUserShell()

	// Default: 3 panes running user's login shell
	commands := []PaneConfig{
		{Cmd: shell, Args: []string{"-l"}},
		{Cmd: shell, Args: []string{"-l"}},
		{Cmd: shell, Args: []string{"-l"}},
	}

	// Parse -e flags for custom commands
	args := os.Args[1:]
	var customCmds []PaneConfig
	for i := 0; i < len(args); i++ {
		if args[i] == "-e" && i+1 < len(args) {
			i++
			customCmds = append(customCmds, PaneConfig{
				Cmd:  shell,
				Args: []string{"-l", "-c", args[i]},
			})
		}
	}
	if len(customCmds) > 0 {
		commands = customCmds
	}

	mux := &Magmux{}
	if err := mux.init(); err != nil {
		fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
		os.Exit(1)
	}
	defer mux.restore()

	if err := mux.buildLayout(commands); err != nil {
		mux.restore()
		fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
		os.Exit(1)
	}

	mux.startReadLoops()
	mux.handleSIGWINCH()

	go mux.renderLoop()
	mux.inputLoop()

	mux.cleanup()
}

// Suppress unused import warning
var _ = io.EOF
var _ = math.MaxInt
