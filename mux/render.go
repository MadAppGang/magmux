package mux

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/theme"
)

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
	r.setAttr(toColor(theme.Pal.Ink), toColor(theme.Pal.Warn), AttrBold)
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
		return toColor(theme.Pal.Fail)
	case "yellow":
		return toColor(theme.Pal.Warn)
	case "green":
		return toColor(theme.Pal.Success)
	default:
		// The palette's rule colour, not ANSI 8. Index 8 is "bright black",
		// which a light terminal renders as a pale grey — and renderBorder
		// then draws it dim, on the terminal's own light background. The
		// splits simply disappeared. theme.Pal.Border is theme-picked and holds
		// 3:1 against its background by test.
		return toColor(theme.Pal.Border)
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
func overlayAccent(style string) theme.RGB {
	switch style {
	case "success":
		return theme.Pal.Success
	case "error":
		return theme.Pal.Fail
	case "info":
		return theme.Pal.Warn
	default:
		return theme.Pal.Text
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
// from whatever the child last left in force. So: the box interior is theme.Pal.Bar,
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
	bgCode := theme.Bg(theme.Pal.Bar)
	borderFg := theme.Fg(overlayAccent(p.overlayStyle))
	bodyFg := theme.Fg(theme.Pal.Text)
	reset := "\x1b[0m"

	// Drop shadow: cells 1 row below and 1 col right of the box, filled with
	// the palette's shadow. Foreground AND background, both theme.Pal.Shadow: the
	// cell paints a space, so making the two agree means it is a solid block
	// whatever the terminal does with the glyph, and it can never inherit a
	// foreground from the child underneath.
	shadowCode := theme.Bg(theme.Pal.Shadow) + theme.Fg(theme.Pal.Shadow)
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
		fill = theme.Pal.Subtle // "text on text" is not a pill
	}

	r.moveTo(cy, cx)
	r.buf.WriteString(theme.Bg(fill))
	r.buf.WriteString(theme.Fg(theme.Pal.Ink))
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

	// Both ends of both axes, and the lower ends are not theoretical: a click on
	// a split border leaves the anchor one cell outside the focused pane, and an
	// unbounded subscript here is a panic that kills the whole multiplexer. The
	// clamp in the click branch of parseSGRMouse is the first line of defence;
	// this is the second, because sel is package-level state that several paths
	// write and only one of them reads it back.
	if sy < 0 {
		sy = 0
	}
	if sx < 0 {
		sx = 0
	}
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
		if row < 0 || row >= len(p.screen.cells) {
			p.mu.Unlock()
			continue
		}
		for c := cs; c >= 0 && c <= ce && c < p.screen.cols; c++ {
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
func barBase() string { return sgrReset + theme.Bg(theme.Pal.Bar) + theme.Fg(theme.Pal.Text) }

// renderStatusBar paints the bottom status line: the bar's own background,
// accent labels, coloured segments separated by thin vertical rules. Segments
// use the "CODE:text" format; consult the switch below for the full mapping.
//
// This is the one full-width surface magmux fills with a colour of its own, and
// it is the exception that proves FIX 1's rule: a status bar that separates
// itself from the pane above is a convention worth keeping, but the background
// has to belong to the active theme (theme.Pal.Bar) and every foreground written on
// it is held to its contrast against THAT — see TestPaletteContrast. It used to
// be hardcoded 256-colour (48;5;236 under 38;5;51 cyan, 220 yellow, …), which
// stayed a dark slab with saturated text on a light terminal.
func (r *Renderer) renderStatusBar(row, cols int, text string) {
	var (
		barBg   = theme.Bg(theme.Pal.Bar)
		reset   = barBase()
		divider = theme.Fg(theme.Pal.Border) + "│" + reset
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
		pill := func(c theme.RGB) {
			r.buf.WriteString(theme.Bg(c) + theme.Fg(theme.Pal.Ink) + sgrBold + " " + txt + " " + reset)
			col += utf8.RuneCountInString(txt) + 2
		}
		// label writes a coloured run and returns to the bar's ground state.
		label := func(c theme.RGB, bold bool, s string) {
			if bold {
				r.buf.WriteString(sgrBold)
			}
			r.buf.WriteString(theme.Fg(c) + s + reset)
			col += utf8.RuneCountInString(s)
		}

		switch code {
		case "*": // Accent bold asterisk + label (used for "* Opus" style)
			label(theme.Pal.Accent, true, "* "+txt)
		case "C": // Accent bold label
			label(theme.Pal.Accent, true, txt)
		case "P": // Success pill
			pill(theme.Pal.Success)
		case "Pr": // Failure pill
			pill(theme.Pal.Fail)
		case "Py": // Warning pill
			pill(theme.Pal.Warn)
		case "$", "Y": // Warning bold (money / running counts)
			label(theme.Pal.Warn, true, txt)
		case "M": // Secondary data (elapsed time)
			label(theme.Pal.Debug, true, txt)
		case "G": // Success bold
			label(theme.Pal.Success, true, txt)
		case "R": // Failure bold
			label(theme.Pal.Fail, true, txt)
		case "W": // Body text, emphasised
			label(theme.Pal.Text, true, txt)
		case "D": // Help text — recedes, but is not dimmed on top of that:
			// theme.Pal.Subtle is already picked to sit at the chrome bar against
			// theme.Pal.Bar, and SGR 2 on top of it puts it back under.
			label(theme.Pal.Subtle, false, txt)
		default:
			// Unknown code — render as plain text
			label(theme.Pal.Text, false, txt)
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

// stdinFile is where keystrokes and the OSC 11 reply come from. Nil m.stdin
// means os.Stdin, which is every real run; a test points it at a pipe. One
// helper rather than the same three lines in init() and inputLoop, so m.stdin is
// a real seam and not one only inputLoop honours.
func (m *Magmux) stdinFile() *os.File {
	if m.stdin != nil {
		return m.stdin
	}
	return os.Stdin
}

// writeTerm puts a painted frame on the terminal. Caller must NOT hold treeMu:
// this is a write to a tty, and a tty with a full buffer blocks.
func (m *Magmux) writeTerm(s string) {
	// Headless: the frame was BUILT and is thrown away here. Deliberate —
	// renderLocked is also where -w decides to quit and where the panel is
	// repainted, so skipping the build would cost auto-exit. The cost is the
	// paint itself, which the dirty-flag model already reduces to nothing on an
	// idle frame.
	//
	// This is THE suppression point for the whole mode: guarding the callee
	// rather than render()'s `if out != ""` means a future caller cannot bypass
	// it. Unconditional, including when m.out is set: "headless" means "emits no
	// frame", one rule. A test that wants the bytes calls renderLocked, which
	// returns them.
	if m.headless {
		return
	}
	if m.out != nil {
		_, _ = io.WriteString(m.out, s)
		return
	}
	_, _ = os.Stdout.WriteString(s)
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
			// TUI app waiting for input → tint green + overlay (task complete).
			// Suppressed under --no-idle-done: a person driving the agent by
			// hand is looking at its input box, and a ✓ DONE card painted over
			// it between every turn is chrome in the way of the work.
			if p.inputReady && p.tint == "" && !m.noIdleDone {
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
		done, running, exited := 0, 0, 0
		for _, p := range m.livePanesLocked(nil) {
			if p.isControl {
				continue // the panel is chrome, not a tracked session
			}
			p.mu.Lock()
			// Same predicate as allPanesDoneLocked, for the same reason: under
			// --no-idle-done a resting agent is still a running session, and a
			// bar reading "1/1 done · ✓ complete" over a session the person is
			// still working in is the claim the flag exists to withdraw.
			if p.dead || (p.inputReady && !m.noIdleDone) {
				done++
			} else {
				running++
			}
			if p.dead {
				exited++
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
			// Which quit key the bar advertises follows the input loop's own
			// predicate, because the bar is the only place that key is
			// announced. A bare q quits only once every child has EXITED;
			// while a pane is merely idle it is a live agent waiting for a
			// prompt, and telling a person to press q there would put a stray
			// "q" in their agent's input box.
			quitLong, quitShort := "ctrl-g q to quit", "ctrl-g q"
			if allDead := exited == total; allDead {
				quitLong, quitShort = "q or Esc to quit", "q quit"
			}
			forms = []string{
				fmt.Sprintf("*: %s\tP: %d/%d done\tG: ✓ complete\tM: %s\tD: %s",
					magmuxLabel(), total, total, elapsed, quitLong),
				fmt.Sprintf("*: %s\tP: %d/%d done\tG: ✓ complete\tD: %s",
					magmuxLabel(), total, total, quitLong),
				fmt.Sprintf("P: %d/%d done\tG: ✓ complete\tD: %s", total, total, quitShort),
				fmt.Sprintf("P: %d/%d done\tD: %s", total, total, quitShort),
				"D: " + quitShort,
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
		text = m.noteRowLocked(text)
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
