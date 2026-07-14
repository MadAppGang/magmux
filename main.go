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
		// Pills get +2 for padding spaces around text
		if len(parts) == 2 {
			code := strings.TrimSpace(parts[0])
			if code == "P" || code == "Pr" || code == "Py" {
				w += 2
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
	}
	s.cells = makeGrid(rows+scrollbackLines, cols)
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
	state    vtState
	inter    rune
	narg     int
	args     [maxParams]int
	nosc     int
	oscbuf   [maxOSC]rune
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

// handleOSC processes completed OSC sequences.
// Detects notification sequences that signal "waiting for input":
//   - OSC 9;...  — iTerm2-style notification
//   - OSC 777;notify;... — rxvt-style notification
//   - OSC 633;B  — VS Code shell integration "prompt started"
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
	gridMode     bool      // pane is in grid mode (don't delete on exit)
	exitCode     int       // exit code of child process
	startedAt    time.Time // when the child process was started (for exec duration)
	tint         string    // "green", "red", "" — border/indicator color
	overlayText  string    // centered overlay text, may contain \n for multi-line (e.g. "✓ DONE")
	overlayStyle string    // "success", "error", "info"
	// Idle/completion detection
	inputReady    bool      // TUI app is waiting for user input
	inputSignal   string    // what triggered inputReady: "osc", "2004", "idle"
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
	// Interactive tool controller (e.g. ClaudeCodeController). Optional.
	controller     ToolController
	controllerSnap Snapshot
	// Back-reference to the owning Magmux. Set by attachControllers so
	// controllers can coordinate (e.g. claim shared resources).
	mux *Magmux
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
	env := append(os.Environ(),
		"TERM=screen-256color",
		fmt.Sprintf("COLUMNS=%d", p.w),
		fmt.Sprintf("LINES=%d", p.h),
	)
	// Export socket path so children can discover it
	if sockPath := os.Getenv("MAGMUX_SOCK"); sockPath != "" {
		env = append(env, "MAGMUX_SOCK="+sockPath)
	}
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
	tint := p.tint
	// Tint wash: blend a subtle color into the background of every cell
	var tintBg Color
	var hasTint bool
	switch tint {
	case "green":
		tintBg = Color{R: 12, G: 24, B: 12, True: true} // subtle dark green wash
		hasTint = true
	case "red":
		tintBg = Color{R: 24, G: 12, B: 12, True: true} // subtle dark red wash
		hasTint = true
	}
	for row := 0; row < s.rows && row < p.h; row++ {
		r.moveTo(p.y+row, p.x)
		for col := 0; col < s.cols && col < p.w; col++ {
			c := s.cells[row][col]
			if c.Cont {
				continue
			}
			bg := c.Bg
			if hasTint && (bg.Index == -1 || (!bg.True && bg.Index == 0)) {
				// Replace default/black background with tint wash
				bg = tintBg
			}
			r.setAttr(c.Fg, bg, c.Attr)
			if c.Ch == 0 || c.Ch == ' ' {
				r.buf.WriteByte(' ')
			} else {
				r.buf.WriteRune(c.Ch)
			}
		}
	}
	overlayText := p.overlayText
	overlayStyle := p.overlayStyle
	p.mu.Unlock()

	// Render overlay if present
	if overlayText != "" {
		r.renderOverlay(&Pane{
			y: p.y, x: p.x, h: p.h, w: p.w,
			overlayText: overlayText, overlayStyle: overlayStyle,
		})
	}
}

// borderColorForPane returns the border color based on child tint settings.
func borderColorForPane(p *Pane) Color {
	// Check if any leaf pane under each child has a tint
	tint1 := leafTint(p.child1)
	tint2 := leafTint(p.child2)
	// Use the more "severe" tint for border color
	tint := tint1
	if tint == "" {
		tint = tint2
	}
	switch tint {
	case "green":
		return Color{Index: 2} // green
	case "red":
		return Color{Index: 1} // red
	default:
		return Color{Index: 8} // gray
	}
}

// leafTint returns the tint of the first leaf pane found (depth-first).
func leafTint(p *Pane) string {
	if p == nil {
		return ""
	}
	if p.splitType == SplitNone {
		return p.tint
	}
	if t := leafTint(p.child1); t != "" {
		return t
	}
	return leafTint(p.child2)
}

func (r *Renderer) renderBorder(p *Pane) {
	bc := borderColorForPane(p)
	r.setAttr(bc, defaultColor, AttrDim)
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

// renderOverlay draws a centered popup window on a pane with a rounded border
// and a drop shadow. The overlayText may contain \n for multi-line content;
// the first line is rendered as a bold header.
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
	boxW := innerW + 4 // 1 border + 1 pad on each side
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

	// Style selection
	var bgCode, borderFg string
	switch p.overlayStyle {
	case "success":
		bgCode = "\x1b[48;5;22m"  // dark green background
		borderFg = "\x1b[38;5;46m" // bright green border
	case "error":
		bgCode = "\x1b[48;5;52m"  // dark red background
		borderFg = "\x1b[38;5;203m" // bright red border
	case "info":
		bgCode = "\x1b[48;5;17m"  // dark blue background
		borderFg = "\x1b[38;5;75m" // bright blue border
	default:
		bgCode = "\x1b[48;5;236m" // dark gray background
		borderFg = "\x1b[38;5;250m" // light gray border
	}
	reset := "\x1b[0m"

	// Drop shadow: dim cells 1 row below and 1 col right of the box.
	// Uses 256-color index 236 on whatever text is underneath.
	shadowCode := "\x1b[48;5;235m\x1b[38;5;238m"
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
		// Content: first line bold bright white, subsequent dim
		if i == 0 {
			r.buf.WriteString("\x1b[1;97m")
		} else {
			r.buf.WriteString("\x1b[22;2;37m")
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

	var bgCode string
	switch p.overlayStyle {
	case "success":
		bgCode = "\x1b[1;37;48;5;22m"
	case "error":
		bgCode = "\x1b[1;37;48;5;52m"
	case "info":
		bgCode = "\x1b[37;48;5;17m"
	default:
		bgCode = "\x1b[1;37;48;5;236m"
	}

	r.moveTo(cy, cx)
	r.buf.WriteString(bgCode)
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

// renderStatusBar paints the bottom status line in a Claude-Code-inspired
// style: dark background, bold-cyan labels, colored segments separated by
// thin dim vertical bars. Segments use the "CODE:text" format; consult
// the switch below for the full palette.
func (r *Renderer) renderStatusBar(row, cols int, text string) {
	const (
		bg      = "\x1b[48;5;236m" // dark gray background for the whole bar
		reset   = "\x1b[39m\x1b[48;5;236m"
		divider = "\x1b[38;5;240m│\x1b[0m" + "\x1b[48;5;236m"
	)

	r.moveTo(row, 0)
	r.buf.WriteString(bg)
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

		switch code {
		case "*": // Cyan bold asterisk + label (used for "* Opus" style)
			r.buf.WriteString("\x1b[1;38;5;51m*\x1b[0m" + bg + "\x1b[1;38;5;51m " + txt + reset)
			col += 2 + utf8.RuneCountInString(txt)
		case "C": // Cyan bold label
			r.buf.WriteString("\x1b[1;38;5;51m" + txt + reset)
			col += utf8.RuneCountInString(txt)
		case "P": // Green pill (bold bright white on dark green)
			r.buf.WriteString("\x1b[48;5;22m\x1b[1;97m " + txt + " \x1b[0m" + bg)
			col += utf8.RuneCountInString(txt) + 2
		case "Pr": // Red pill
			r.buf.WriteString("\x1b[48;5;52m\x1b[1;97m " + txt + " \x1b[0m" + bg)
			col += utf8.RuneCountInString(txt) + 2
		case "Py": // Yellow pill
			r.buf.WriteString("\x1b[48;5;94m\x1b[1;97m " + txt + " \x1b[0m" + bg)
			col += utf8.RuneCountInString(txt) + 2
		case "$", "Y": // Yellow bold (money / warnings)
			r.buf.WriteString("\x1b[1;38;5;220m" + txt + reset)
			col += utf8.RuneCountInString(txt)
		case "M": // Magenta bold
			r.buf.WriteString("\x1b[1;38;5;213m" + txt + reset)
			col += utf8.RuneCountInString(txt)
		case "G": // Green bold
			r.buf.WriteString("\x1b[1;38;5;82m" + txt + reset)
			col += utf8.RuneCountInString(txt)
		case "R": // Red bold
			r.buf.WriteString("\x1b[1;38;5;203m" + txt + reset)
			col += utf8.RuneCountInString(txt)
		case "W": // White
			r.buf.WriteString("\x1b[1;97m" + txt + reset)
			col += utf8.RuneCountInString(txt)
		case "D": // Dim gray (help text)
			r.buf.WriteString("\x1b[2;38;5;245m" + txt + "\x1b[22;39m" + bg)
			col += utf8.RuneCountInString(txt)
		default:
			// Unknown code — render as plain text
			r.buf.WriteString("\x1b[38;5;250m" + txt + reset)
			col += utf8.RuneCountInString(txt)
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
	root            *Pane
	focused         *Pane
	allPanes        []*Pane // leaf panes only
	rows, cols      int
	statusText      string
	renderer        Renderer
	rawState        *term.State
	quit            chan struct{}
	quitOnce        sync.Once
	wg              sync.WaitGroup
	gridMode        bool   // -g flag was used
	autoExit        bool   // -w flag: quit automatically when all panes done
	sockPath        string    // /tmp/magmux-{pid}.sock
	sockClients     []net.Conn // currently-connected socket subscribers (for push events)
	sockClientsMu   sync.Mutex
	lastDoneCount   int       // track status bar updates to avoid redundant rewrites
	startedAt       time.Time // when magmux started (for status bar timer)
	completedAt    time.Time // when all panes reached "done" (freezes timer)
	lastTimerTick   int       // elapsed seconds at last forced status redraw
	// Interactive tool controllers
	controllerFactories []ControllerFactory
	lastControllerPoll  time.Time
	ctx                 context.Context
	// claimedSessions maps controller-managed session file paths to the
	// pane that owns them. Used so sibling controllers don't both pick the
	// same JSONL file when running in the same project directory.
	claimedSessions map[string]*Pane
	claimedMu       sync.Mutex
}

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

// buildColumn recursively splits a slice of commands into a vertical binary tree.
func buildColumn(cmds []PaneConfig, y, x, h, w int) (*Pane, []*Pane, error) {
	if len(cmds) == 1 {
		p, err := newPane(y, x, h, w, cmds[0].Cmd, cmds[0].Args...)
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
	statusH := 1
	availH := m.rows - statusH

	if len(commands) == 0 {
		return fmt.Errorf("no commands specified")
	}

	switch len(commands) {
	case 1:
		// Single pane — fullscreen
		p, err := newPane(0, 0, availH, m.cols, commands[0].Cmd, commands[0].Args...)
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
		p1, err := newPane(0, 0, availH, w1, commands[0].Cmd, commands[0].Args...)
		if err != nil {
			return err
		}
		p2, err := newPane(0, w1+1, availH, w2, commands[1].Cmd, commands[1].Args...)
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

		m.allPanes = append(leftLeaves, rightLeaves...)
		m.focused = m.allPanes[0]
	}

	// Mark all panes as grid mode
	for _, p := range m.allPanes {
		p.gridMode = true
	}

	return nil
}

func (m *Magmux) startReadLoops() {
	for _, p := range m.allPanes {
		m.wg.Add(1)
		go p.readLoop(&m.wg)
	}
}

// attachControllers walks every leaf pane and attaches a ToolController if
// any registered factory recognizes the pane's command. Called once after
// the layout is built and panes are spawned.
func (m *Magmux) attachControllers() {
	if m.ctx == nil {
		m.ctx = context.Background()
	}
	for _, p := range m.allPanes {
		p.mux = m
		if p.controller != nil {
			continue
		}
		for _, factory := range m.controllerFactories {
			if c := factory(p); c != nil {
				p.controller = c
				if err := c.Start(m.ctx); err != nil {
					if dbgFile != nil {
						fmt.Fprintf(dbgFile, "[ctrl] %s.Start error: %v\n", c.Name(), err)
					}
					p.controller = nil
					continue
				}
				if dbgFile != nil {
					fmt.Fprintf(dbgFile, "[ctrl] attached %s to pane\n", c.Name())
				}
				break
			}
		}
	}
}

// pollControllers polls each attached controller and translates the
// resulting Snapshot into pane state (inputReady, tint, overlayText).
// Throttled to ~4Hz from the render loop.
func (m *Magmux) pollControllers() {
	now := time.Now()
	if !m.lastControllerPoll.IsZero() && now.Sub(m.lastControllerPoll) < 250*time.Millisecond {
		return
	}
	m.lastControllerPoll = now

	for idx, p := range m.allPanes {
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
			m.broadcastEvent(map[string]any{
				"type":        "snapshot",
				"pane":        idx,
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
		// If we previously fired DONE via a controller signal, clear it
		// (a new turn started). Don't touch state set by other code paths.
		if p.inputReady && p.inputSignal == "ctrl" {
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
	commandMode := false

	// Stdin is read on a background goroutine so the main loop can also wake
	// on m.quit. Without this, a renderLoop-driven close(m.quit) (e.g. -w
	// auto-exit) cannot unblock the main goroutine, and magmux hangs.
	stdinCh := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
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
		select {
		case <-m.quit:
			return
		case c, ok := <-stdinCh:
			if !ok {
				return
			}
			chunk = c
		}
		inbuf = append(inbuf, chunk...)

		for len(inbuf) > 0 {
			b := inbuf[0]

			if commandMode {
				commandMode = false
				switch b {
				case 'q':
					m.quitOnce.Do(func() { close(m.quit) })
					return
				case '\t', 'o':
					m.focusNext()
				default:
					m.focused.writePTY([]byte{0x07, b})
				}
				inbuf = inbuf[1:]
				continue
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
}

func (m *Magmux) socketServer() {
	sockPath := fmt.Sprintf("/tmp/magmux-%d.sock", os.Getpid())
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
		return
	}

	// Cleanup on exit
	go func() {
		<-m.quit
		// Push a final aggregated results event with the last-known state of
		// every pane. Subscribers (e.g. claudish) use this as the authoritative
		// final state — no file-based fallback needed.
		m.broadcastEvent(map[string]any{
			"type":    "results",
			"panes":   m.buildPaneResults(),
			"endedAt": time.Now().UTC().Format(time.RFC3339),
		})
		// Push a shutdown event so clients know the socket is closing.
		m.broadcastEvent(map[string]any{
			"type": "shutdown",
		})
		// Deterministically flush and close each subscriber connection instead
		// of racing a fixed drain sleep. Closing the connection after the two
		// broadcasts above (which write synchronously under sockClientsMu) gives
		// each subscriber a clean EOF *after* it has received the final results
		// — the ordering guarantee integrators rely on. Bound the whole teardown
		// so a single wedged subscriber can't hang magmux's exit.
		m.closeSockClients(2 * time.Second)
		ln.Close()
		os.Remove(sockPath)
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
	if data, err := json.Marshal(snapshot); err == nil {
		data = append(data, '\n')
		_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		_, _ = conn.Write(data)
		_ = conn.SetWriteDeadline(time.Time{})
	}

	// Track this connection so we can push snapshots/exit events to it.
	m.sockClientsMu.Lock()
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
	for scanner.Scan() {
		line := scanner.Text()
		var msg sockMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		m.dispatchSocketMsg(msg)
	}
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

func (m *Magmux) dispatchSocketMsg(msg sockMsg) {
	switch msg.Type {
	case "status":
		m.statusText = msg.Text
		// Force a redraw
		for _, p := range m.allPanes {
			p.mu.Lock()
			p.dirty = true
			p.mu.Unlock()
		}

	case "tint":
		paneIdx := m.parsePaneIndex(msg.Pane)
		if paneIdx == -1 {
			// "*" — apply to all panes
			color := msg.Color
			if color == "reset" {
				color = ""
			}
			for _, p := range m.allPanes {
				p.mu.Lock()
				p.tint = color
				p.dirty = true
				p.mu.Unlock()
			}
		} else if paneIdx >= 0 && paneIdx < len(m.allPanes) {
			color := msg.Color
			if color == "reset" {
				color = ""
			}
			p := m.allPanes[paneIdx]
			p.mu.Lock()
			p.tint = color
			p.dirty = true
			p.mu.Unlock()
		}

	case "overlay":
		paneIdx := m.parsePaneIndex(msg.Pane)
		if paneIdx >= 0 && paneIdx < len(m.allPanes) {
			p := m.allPanes[paneIdx]
			p.mu.Lock()
			p.overlayText = msg.Text
			p.overlayStyle = msg.Style
			p.dirty = true
			p.mu.Unlock()
		}

	case "agent":
		paneIdx := m.parsePaneIndex(msg.Pane)
		if paneIdx < 0 || paneIdx >= len(m.allPanes) {
			return
		}
		p := m.allPanes[paneIdx]
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
	}
}

func (m *Magmux) parsePaneIndex(v any) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case string:
		if val == "*" {
			return -1
		}
		var idx int
		fmt.Sscanf(val, "%d", &idx)
		return idx
	}
	return -2 // invalid
}

// TODO: Add native macOS kqueue-based file watcher (kqueue_darwin.go) for monitoring
// ~/.cctop/sessions/ as a fallback when agents don't send IPC messages directly.
// Also consider Linux inotify equivalent (inotify_linux.go).

func (m *Magmux) updateAgentStatusBar() {
	var parts []string
	needsAttention := 0

	for _, p := range m.allPanes {
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
	for _, p := range m.allPanes {
		p.mu.Lock()
		p.dirty = true
		p.mu.Unlock()
	}
}

// ── Status File Polling ─────────────────────────────────────────────────────

// ── Grid Mode Exit Handling ─────────────────────────────────────────────────

// waitForChild waits for a pane's child process to exit and sets exit state.
func (m *Magmux) waitForChild(p *Pane) {
	if p.cmd == nil {
		return
	}
	err := p.cmd.Wait()
	p.mu.Lock()
	p.dead = true
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			p.exitCode = exitErr.ExitCode()
		} else {
			p.exitCode = 1
		}
	} else {
		p.exitCode = 0
	}

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

	// Find pane index (stable across the lifetime of Magmux).
	paneIdx := -1
	for i, pp := range m.allPanes {
		if pp == p {
			paneIdx = i
			break
		}
	}

	// Push an exit event over the IPC socket so subscribers (e.g. claudish)
	// learn about per-pane completion without polling files.
	m.broadcastEvent(map[string]any{
		"type":     "exit",
		"pane":     paneIdx,
		"exitCode": exitCode,
		"duration": duration,
		"lastLine": lastMsg,
		"response": snap.LastResponse,
		"prompt":   snap.LastUserPrompt,
		"tool":     snap.LastTool,
		"model":    snap.Model,
	})
}

// buildPaneResults collects the final state of every pane into a serializable
// slice. Used to push an authoritative final state to subscribers right before
// the socket closes.
func (m *Magmux) buildPaneResults() []map[string]any {
	results := make([]map[string]any, 0, len(m.allPanes))
	for i, p := range m.allPanes {
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
			"pane":     i,
			"state":    state,
			"exitCode": p.exitCode,
			"dead":     p.dead,
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

// allPanesDone returns true if every pane is either dead or inputReady.
func (m *Magmux) allPanesDone() bool {
	for _, p := range m.allPanes {
		if !p.dead && !p.inputReady {
			return false
		}
	}
	return len(m.allPanes) > 0
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
	// Poll attached interactive tool controllers (throttled internally to ~4Hz).
	// This must run BEFORE the heuristic detection below so that controller
	// state takes precedence over screen-scraping when both are available.
	m.pollControllers()

	// Grid mode: idle/completion detection
	if m.gridMode {
		now := time.Now()
		for _, p := range m.allPanes {
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

	// Update status bar with done/running counts (grid mode)
	if m.gridMode {
		done, running := 0, 0
		for _, p := range m.allPanes {
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
		total := len(m.allPanes)
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

		// Build segments, then maybe append attribution if there's room
		var segs string
		if done == 0 && running == 0 {
			// No panes tracked yet — leave default text alone
			segs = ""
		} else if allDone {
			segs = fmt.Sprintf("*: %s\tP: %d/%d done\tG: ✓ complete\tM: %s\tD: q or Esc to quit",
				magmuxLabel(), total, total, elapsed)
		} else {
			segs = fmt.Sprintf("*: %s\tP: %d/%d done\tY: %d running\tM: %s\tD: ctrl-g Tab · q quit",
				magmuxLabel(), done, total, running, elapsed)
		}
		if segs != "" {
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

	// Auto-exit: quit when all panes done (-w flag)
	if m.autoExit && m.gridMode && m.allPanesDone() {
		m.quitOnce.Do(func() { close(m.quit) })
		return
	}

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
		m.statusText = "*: " + magmuxLabel() + "\tD: ctrl-g q quit\tD: ctrl-g Tab switch"
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
			fmt.Println("  q / Esc       Quit (when all panes done, grid mode)")
			fmt.Println("  Ctrl-G Tab    Switch focus to next pane")
			fmt.Println("  Mouse click   Switch focus to clicked pane")
			fmt.Println("  Mouse drag    Select text (auto-copies to clipboard)")
			fmt.Println()
			fmt.Println("IPC Socket:")
			fmt.Println("  /tmp/magmux-{pid}.sock — JSON line protocol")
			fmt.Println("  Env var MAGMUX_SOCK exported to child processes")
			fmt.Println()
			fmt.Println("Agent Status Monitoring:")
			fmt.Println("  Send agent hook events via IPC socket:")
			fmt.Println("  {\"type\":\"agent\",\"pane\":0,\"event\":\"Stop\",\"project\":\"myapp\"}")
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
	} else {
		// Default: 3 panes running user's login shell
		commands = []PaneConfig{
			{Cmd: shell, Args: []string{"-l"}},
			{Cmd: shell, Args: []string{"-l"}},
			{Cmd: shell, Args: []string{"-l"}},
		}
	}

	mux := &Magmux{
		gridMode: useGrid,
		autoExit: autoExit,
		controllerFactories: []ControllerFactory{
			claudeCodeFactory,
		},
	}

	if err := mux.init(); err != nil {
		fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
		os.Exit(1)
	}
	defer mux.restore()

	// Start socket server before spawning children (so MAGMUX_SOCK is set)
	go mux.socketServer()
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

	mux.startReadLoops()
	mux.attachControllers()

	// In grid mode, start waiters for each child process
	if useGrid {
		for _, p := range mux.allPanes {
			go mux.waitForChild(p)
		}
	}

	mux.handleSIGWINCH()

	go mux.renderLoop()
	mux.inputLoop()

	// Cleanup socket
	if mux.sockPath != "" {
		os.Remove(mux.sockPath)
	}

	mux.cleanup()
}

// Suppress unused import warning
var _ = io.EOF
var _ = math.MaxInt
