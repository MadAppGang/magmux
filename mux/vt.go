package mux

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/theme"
)

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
// The reply goes out through replyLocked on the parser's own goroutine, exactly
// as the DA and DSR replies below do, and under the same p.mu they hold. NOT
// through writePTY: that is the human-input path, which refuses a dead pane in
// grid mode and clears the completion state of a settled one — see replyLocked
// for what each of those cost.
func (vt *VTParser) answerColorQuery(code string) {
	c, ok := theme.TerminalColor(code)
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
	resp := "\x1b]" + code + ";" + theme.XColorString(c) + end
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[OSC] colour query %s → %q\n", code, resp)
	}
	vt.node.replyLocked([]byte(resp))
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
		// DECTCEM is pane state (see the 25 case in doCSI), and a full reset is
		// the one thing that puts it back: `reset` after a crashed TUI must give
		// the cursor back to the shell.
		vt.node.curHidden = false
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
				case 25: // DECTCEM - cursor visibility
					// Recorded on the PANE, not on the Screen, which is xterm's
					// behaviour: visibility survives an alternate-screen switch. A
					// per-screen flag would hand the shell back a cursor magmux
					// believes is hidden, every time a full-screen app that hid it
					// exited.
					//
					// magmux's own renderer does not consult it — it parks the real
					// terminal cursor over the focused pane and lets the terminal
					// draw it — so this is state magmux OBSERVES, for the frame
					// stream, rather than state it acts on. Ignoring it meant every
					// remote viewer drew a cursor in the middle of a TUI that had
					// deliberately taken it away.
					vt.node.curHidden = !set
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
			vt.node.replyLocked([]byte("\x1b[>1;10;0c"))
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
		//
		// The bottom parameter MUST be read with p0 and the guard below must stay
		// reachable. This comment once described that fix while the line under it
		// still said p1, so `bot` could never be 0, `top >= bot` fired instead,
		// and a bare `CSI r` was IGNORED — leaving whatever region was already in
		// force. TestDECSTBMDefaultsToTheWholePage now starts every case from a
		// non-default region so it can tell "reset" from "ignored"; the version
		// that started at the default could not, which is how this shipped.
		top := vt.p1(0)
		bot := vt.p0(1)
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
			vt.node.replyLocked([]byte(resp))
		} else if vt.p0(0) == 5 { // Device status - report OK
			vt.node.replyLocked([]byte("\x1b[0n"))
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
		vt.node.replyLocked([]byte("\x1b[?1;2c"))
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
			vt.node.replyLocked([]byte(resp))
		case 14: // Report window size in pixels (fake it)
			resp := fmt.Sprintf("\x1b[4;%d;%dt", vt.node.h*16, vt.node.w*8)
			vt.node.replyLocked([]byte(resp))
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
