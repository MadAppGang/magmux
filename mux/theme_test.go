package mux

// Theme tests.
//
// Two things are under test, and only one of them is about colour:
//
//   - the OSC 11 probe must classify what terminals actually reply, and must
//     hand back every byte it read that was not part of that reply. It reads
//     stdin, and stdin is where the user types; a probe that loses a keystroke
//     is a worse bug than a wrong palette.
//   - both palettes must be legible against their own background. That is the
//     test the original bug would have failed: the dark palette's body text
//     against a light terminal is 1.3:1.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/theme"
)

// useTheme swaps the palette for the duration of a test and puts it back.
// Tests do not run in parallel in this package (nothing calls t.Parallel), so
// the global is safe to move.
func useTheme(k theme.Kind) func() {
	prev := theme.Current
	theme.Set(k)
	return func() { theme.Set(prev) }
}

// ── the completion marker ─────────────────────────────────────────────────────

// renderLeaf renders one PTY-less pane through the real Renderer and returns
// the bytes it emitted.
func renderLeaf(p *Pane) string {
	var r Renderer
	r.reset()
	r.renderPane(p)
	return r.frame()
}

// replay feeds a rendered frame back through the VT parser into a fresh screen,
// so a test can ask what the terminal would actually be showing. The renderer
// positions absolutely, so a pane at 0,0 lands cell-for-cell.
func replay(frame string, h, w int) *Pane {
	out := newControlPane(0, 0, h, w, "replay")
	out.vt.write([]byte(frame))
	return out
}

// TestTintDoesNotTouchThePaneInterior is the invisible-session bug, stated as
// the rule that prevents it.
//
// renderPane used to substitute a "tint wash" background into every cell of a
// finished pane while leaving the child's FOREGROUND alone. That cannot work in
// either direction, because the foreground is the child's to choose: a
// near-black wash made the pane a black box on a light terminal, and the pale
// wash that replaced it made Claude Code's light foregrounds invisible — the
// user's screenshot showed a finished pane as a blank green rectangle.
//
// The rule is therefore absolute: whatever the tint, the pane's cells render
// byte-for-byte identically. Completion is announced by the border and the
// overlay badge, both of which magmux controls outright.
func TestTintDoesNotTouchThePaneInterior(t *testing.T) {
	const h, w = 6, 24

	for _, kind := range []theme.Kind{theme.Dark, theme.Light} {
		t.Run(kind.String(), func(t *testing.T) {
			defer useTheme(kind)()

			p := newControlPane(0, 0, h, w, "leaf")
			// A child that assumes a dark terminal, which is what Claude Code
			// is: light foreground, no background of its own.
			p.vt.write([]byte("\x1b[38;2;205;214;244mrunning tests\r\n\x1b[38;5;250mall green"))
			plain := renderLeaf(p)

			for _, tint := range []string{"green", "red", "yellow"} {
				p.mu.Lock()
				p.tint = tint
				p.mu.Unlock()
				if got := renderLeaf(p); got != plain {
					t.Fatalf("tint %q changed the pane's own cells.\n with tint: %q\n"+
						"without:   %q\nmagmux does not know the child's foreground and "+
						"cannot recolour it, so no background it substitutes can be safe",
						tint, got, plain)
				}
			}
		})
	}
}

// TestTintedPaneKeepsTheChildsForeground is the regression case in the form the
// user reported it: a session that wrote LIGHT text, marked done, must still be
// readable. It asserts on the cells a terminal would end up with, not on bytes.
func TestTintedPaneKeepsTheChildsForeground(t *testing.T) {
	const h, w = 4, 20
	defer useTheme(theme.Light)()

	p := newControlPane(0, 0, h, w, "leaf")
	p.vt.write([]byte("\x1b[38;2;205;214;244mDONE-MARKER"))
	p.mu.Lock()
	p.tint = "green"
	p.mu.Unlock()

	out := replay(renderLeaf(p), h, w)
	want := Color{R: 205, G: 214, B: 244, True: true}
	for col, ch := range "DONE-MARKER" {
		c := out.screen.cells[0][col]
		if c.Ch != ch {
			t.Fatalf("cell 0,%d is %q, want %q", col, string(c.Ch), string(ch))
		}
		if !colorEqual(c.Fg, want) {
			t.Errorf("cell 0,%d foreground is %+v, want the child's %+v", col, c.Fg, want)
		}
		if c.Bg.True || c.Bg.Index != -1 {
			t.Fatalf("cell 0,%d background is %+v, not the terminal's default — a wash "+
				"under a foreground magmux did not choose is exactly how a finished "+
				"pane became a blank rectangle", col, c.Bg)
		}
	}
}

// TestTintColoursTheBorder is the other half: having stopped washing the
// interior, the tint has to still be visible somewhere magmux owns. The split
// rule is it, and it is a foreground on the terminal's own background, so it
// works on any terminal.
func TestTintColoursTheBorder(t *testing.T) {
	for _, kind := range []theme.Kind{theme.Dark, theme.Light} {
		t.Run(kind.String(), func(t *testing.T) {
			defer useTheme(kind)()

			left := newControlPane(0, 0, 6, 10, "l")
			right := newControlPane(0, 11, 6, 10, "r")
			split := &Pane{splitType: SplitHorizontal, h: 6, w: 21,
				child1: left, child2: right}

			set := func(p *Pane, tint string) {
				p.mu.Lock()
				p.tint = tint
				p.mu.Unlock()
			}

			for _, tc := range []struct {
				l, r string
				want theme.RGB
				why  string
			}{
				{"", "", theme.Pal.Border, "untinted panes get the palette's rule colour"},
				{"green", "", theme.Pal.Success, "a finished pane"},
				{"", "green", theme.Pal.Success, "…on either side of the split"},
				{"red", "", theme.Pal.Fail, "a failed pane"},
				{"yellow", "", theme.Pal.Warn, "a pane blocked on a permission prompt"},
				{"green", "red", theme.Pal.Fail, "the loudest tint wins, whichever side it is on"},
				{"red", "green", theme.Pal.Fail, "…and the old code took child1's, hiding it"},
				{"green", "yellow", theme.Pal.Warn, "amber outranks green"},
			} {
				set(left, tc.l)
				set(right, tc.r)
				got := borderColorForPane(split)
				if !colorEqual(got, toColor(tc.want)) {
					t.Errorf("tints %q/%q gave border %+v, want %+v (%s)",
						tc.l, tc.r, got, toColor(tc.want), tc.why)
				}
			}

			// And the colour actually reaches the terminal, undimmed: a
			// completion marker at half contrast is what made the old indexed
			// border disappear on a light terminal.
			set(left, "green")
			set(right, "")
			var r Renderer
			r.reset()
			r.renderBorder(split)
			frame := r.frame()
			if !strings.Contains(frame, fmt.Sprintf(";38;2;%d;%d;%dm",
				theme.Pal.Success.R, theme.Pal.Success.G, theme.Pal.Success.B)) {
				t.Errorf("the border frame does not carry the success colour: %q", frame)
			}
			if strings.Contains(frame, "\x1b[0;2;") || strings.Contains(frame, "\x1b[0;2m") {
				t.Errorf("the border is drawn dim: %q", frame)
			}
		})
	}
}

// ── the status bar ────────────────────────────────────────────────────────────

// TestStatusBarFollowsThePalette pins the third of the three: the bar used to
// be hardcoded 256-colour (a 48;5;236 slab under 38;5;51 cyan and 38;5;220
// yellow), so on a light terminal it stayed a dark strip with saturated text
// no matter which palette was in force.
func TestStatusBarFollowsThePalette(t *testing.T) {
	const text = "*: magmux\tP: 2/3 done\tY: 1 running\tM: 1m 4s\tD: ctrl-g q quit"

	render := func(kind theme.Kind) string {
		defer useTheme(kind)()
		var r Renderer
		r.reset()
		r.renderStatusBar(0, 60, text)
		return r.frame()
	}

	dark, light := render(theme.Dark), render(theme.Light)

	if dark == light {
		t.Error("the status bar is byte-identical in both themes; it is not reading the palette")
	}

	for _, tc := range []struct {
		name  string
		frame string
		p     theme.Palette
	}{{"dark", dark, theme.DarkPalette}, {"light", light, theme.LightPalette}} {
		t.Run(tc.name, func(t *testing.T) {
			must := map[string]string{
				"the bar's own background": theme.Bg(tc.p.Bar),
				"the accent label":         theme.Fg(tc.p.Accent),
				"the success pill":         theme.Bg(tc.p.Success),
				"the warning colour":       theme.Fg(tc.p.Warn),
				"the help text":            theme.Fg(tc.p.Subtle),
				"the divider rule":         theme.Fg(tc.p.Border),
			}
			for what, seq := range must {
				if !strings.Contains(tc.frame, seq) {
					t.Errorf("%s (%q) is missing from the bar", what, seq)
				}
			}
			// The hardcoded 256-colour values, named so a reintroduction says
			// what it broke rather than just failing.
			for _, seq := range []string{"48;5;236", "38;5;51", "38;5;220",
				"38;5;213", "38;5;82", "38;5;203", "38;5;245", "38;5;250",
				"48;5;22", "48;5;52", "48;5;94"} {
				if strings.Contains(tc.frame, seq) {
					t.Errorf("the bar still emits the hardcoded %q; it cannot follow the theme", seq)
				}
			}
			// SGR 39 drops the foreground to the terminal's default while the
			// bar's own background is still in force — a colour nobody chose,
			// on a surface magmux painted. Harmless only for as long as every
			// segment happens to set a colour before writing anything.
			if strings.Contains(tc.frame, "\x1b[39m") {
				t.Errorf("the bar leaves the foreground at the terminal default over its "+
					"own background: %q", tc.frame)
			}
		})
	}
}

// ── the completion overlay ────────────────────────────────────────────────────
//
// The overlay was the last piece of magmux's own chrome that was hardcoded
// 256-colour. A capture of the shipped build shows what it emitted:
//
//	ESC[38;5;46m ESC[48;5;22m   the box   — bright green on dark green
//	ESC[1m ESC[97m              the header
//	ESC[2m ESC[37m              the detail lines
//	ESC[38;5;238m ESC[48;5;235m the drop shadow
//
// which is a dark-themed box wherever it is drawn, with detail text at 4.38:1
// BEFORE the terminal applies its own idea of what SGR 2 means, and a "shadow"
// that is lighter than Mocha's base — a highlight.
//
// These tests measure what lands on the SCREEN rather than what the code says,
// because that is the only level at which "48;5;22" and a palette token are
// comparable at all: the frame is replayed through magmux's own VT parser and
// the resulting cells are what get asserted on.

// xterm256 resolves an indexed colour to what a terminal actually shows: the 16
// system colours, the 6x6x6 cube, and the 24-step grey ramp.
func xterm256(i int) (theme.RGB, bool) {
	switch {
	case i < 0 || i > 255:
		return theme.RGB{}, false
	case i < 16:
		sys := []theme.RGB{
			{R: 0, G: 0, B: 0}, {R: 128, G: 0, B: 0}, {R: 0, G: 128, B: 0}, {R: 128, G: 128, B: 0},
			{R: 0, G: 0, B: 128}, {R: 128, G: 0, B: 128}, {R: 0, G: 128, B: 128}, {R: 192, G: 192, B: 192},
			{R: 128, G: 128, B: 128}, {R: 255, G: 0, B: 0}, {R: 0, G: 255, B: 0}, {R: 255, G: 255, B: 0},
			{R: 0, G: 0, B: 255}, {R: 255, G: 0, B: 255}, {R: 0, G: 255, B: 255}, {R: 255, G: 255, B: 255},
		}
		return sys[i], true
	case i < 232:
		lv := []uint8{0, 95, 135, 175, 215, 255}
		i -= 16
		return theme.RGB{R: lv[i/36], G: lv[(i/6)%6], B: lv[i%6]}, true
	default:
		v := uint8(8 + (i-232)*10)
		return theme.RGB{R: v, G: v, B: v}, true
	}
}

// cellRGB resolves half a rendered cell to a colour, reporting whether the cell
// set that half AT ALL. A cell that did not is showing whatever was in force
// underneath it — which, for an overlay, is the child's output.
func cellRGB(c Color) (theme.RGB, bool) {
	if c.True {
		return theme.RGB{R: c.R, G: c.G, B: c.B}, true
	}
	if c.Index >= 0 {
		return xterm256(int(c.Index))
	}
	return theme.RGB{}, false
}

// The child's own colours, chosen to be in neither palette so that a cell
// wearing them can only have inherited them.
var (
	childFg = theme.RGB{R: 0xFF, G: 0x00, B: 0xFF}
	childBg = theme.RGB{R: 0x00, G: 0x80, B: 0x80}
)

// overlayCells renders a pane twice — once with the overlay, once without — and
// returns the cells the overlay actually painted. Diffing is what keeps the
// test independent of the box's size and position arithmetic: whatever moved is
// the overlay's, by definition.
func overlayCells(t *testing.T, style string, h, w int) []Cell {
	t.Helper()

	build := func(text string) *Pane {
		p := newControlPane(0, 0, h, w, "leaf")
		// A child that has painted every cell in colours of its own. The
		// overlay lands on top of this, and every half-cell it fails to set is
		// a half-cell of the child's showing through.
		p.vt.write([]byte(theme.Fg(childFg) + theme.Bg(childBg) + strings.Repeat("x", h*w-1)))
		p.overlayText = text
		p.overlayStyle = style
		return p
	}

	plain := replay(renderLeaf(build("")), h, w)
	over := replay(renderLeaf(build("✓ DONE\ntook 30.2s\n42 tests passed")), h, w)

	var out []Cell
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if a, b := plain.screen.cells[y][x], over.screen.cells[y][x]; a != b {
				out = append(out, b)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("the %s overlay painted nothing at %dx%d", style, h, w)
	}
	return out
}

// TestOverlayContrast is the sibling of TestPaletteContrast for the one surface
// the palette did not reach.
//
// Four properties, each of which the shipped overlay breaks in at least one
// theme:
//
//  1. every cell sets BOTH halves. This is why the overlay, and not a
//     background wash, is the completion marker (see renderPane): it is drawn
//     over colours magmux does not know, so a cell that sets only one half
//     inherits the other from the child.
//  2. every glyph clears 4.5:1 against the cell it is drawn in — the same bar
//     TestPaletteContrast holds body text to. The detail lines were 4.38:1.
//  3. nothing is dim. SGR 2 is a hint the terminal renders however it likes,
//     which is not a property a contrast test can measure; de-emphasis has to
//     be a colour.
//  4. the box's own backgrounds belong to the terminal they are painted in — a
//     shade of it, not a slab on top of it — and the darkest of them, the drop
//     shadow, is darker than that background rather than lighter.
func TestOverlayContrast(t *testing.T) {
	const (
		bodyMin    = 4.5
		surfaceMax = 3.0 // a panel magmux paints is a shade of the terminal
	)

	for _, kind := range []theme.Kind{theme.Dark, theme.Light} {
		for _, style := range []string{"success", "error"} {
			t.Run(kind.String()+"/"+style, func(t *testing.T) {
				defer useTheme(kind)()

				for _, box := range []struct {
					name string
					h, w int
					// isBox is false for the small-pane fallback pill: a pill
					// is a BADGE — a saturated fill with ink on it,
					// deliberately loud — where the box is a PANEL. Different
					// idiom, different rule for the background.
					isBox bool
				}{
					{"box", 12, 40, true},
					{"pill", 3, 16, false},
				} {
					t.Run(box.name, func(t *testing.T) {
						cells := overlayCells(t, style, box.h, box.w)

						darkestBg := theme.RGB{R: 0xFF, G: 0xFF, B: 0xFF}
						for _, c := range cells {
							f, okF := cellRGB(c.Fg)
							b, okB := cellRGB(c.Bg)
							if !okF || !okB {
								t.Fatalf("cell %q sets fg=%v bg=%v — the overlay left half a "+
									"cell to the child underneath it", string(c.Ch), okF, okB)
							}
							if f == childFg || b == childBg {
								t.Fatalf("cell %q inherited the child's colours (fg %+v bg %+v); "+
									"the overlay is drawn over output whose colours magmux does "+
									"not know", string(c.Ch), f, b)
							}
							if c.Attr&AttrDim != 0 {
								t.Errorf("cell %q is dim; SGR 2 is not a contrast a test can "+
									"measure or a terminal must honour", string(c.Ch))
							}
							if c.Ch != 0 && c.Ch != ' ' {
								if got := contrastRatio(f, b); got < bodyMin {
									t.Errorf("%q is %.2f:1 against its own cell, want >= %.1f:1",
										string(c.Ch), got, bodyMin)
								}
							}
							if box.isBox {
								if got := contrastRatio(b, theme.Pal.AssumedBack); got > surfaceMax {
									t.Errorf("the overlay paints %+v, which is %.2f:1 against "+
										"this theme's terminal background — a slab, not a "+
										"surface belonging to it", b, got)
								}
								if theme.ScreenLuminance(b) < theme.ScreenLuminance(darkestBg) {
									darkestBg = b
								}
							}
						}
						if box.isBox &&
							theme.ScreenLuminance(darkestBg) >= theme.ScreenLuminance(theme.Pal.AssumedBack) {
							// The darkest thing the box paints is its shadow.
							t.Errorf("the drop shadow (%+v, luminance %.3f) is not darker than "+
								"the terminal it falls on (%+v, %.3f) — that is a highlight",
								darkestBg, theme.ScreenLuminance(darkestBg),
								theme.Pal.AssumedBack, theme.ScreenLuminance(theme.Pal.AssumedBack))
						}
					})
				}
			})
		}
	}
}

// TestOverlayFollowsThePalette is the same argument at the level of the bytes:
// the overlay must READ the palette, which means it cannot be byte-identical in
// the two themes and cannot contain an indexed colour at all.
func TestOverlayFollowsThePalette(t *testing.T) {
	render := func(kind theme.Kind, style string) string {
		defer useTheme(kind)()
		p := newControlPane(0, 0, 12, 40, "leaf")
		p.overlayText = "✓ DONE\ntook 30.2s\n42 tests passed"
		p.overlayStyle = style
		return renderLeaf(p)
	}

	for _, style := range []string{"success", "error", "info", ""} {
		dark, light := render(theme.Dark, style), render(theme.Light, style)
		if dark == light {
			t.Errorf("the %q overlay is byte-identical in both themes; it is not reading "+
				"the palette", style)
		}
		for _, frame := range []string{dark, light} {
			for _, seq := range []string{"38;5;", "48;5;"} {
				if strings.Contains(frame, seq) {
					t.Errorf("the %q overlay still emits an indexed colour (%q): an index "+
						"means whatever the user's terminal decided it means", style, seq)
				}
			}
			// The dim idiom, in the forms the old code emitted it.
			for _, seq := range []string{"\x1b[2m", ";2;37m", "\x1b[22;2;"} {
				if strings.Contains(frame, seq) {
					t.Errorf("the %q overlay still de-emphasises with SGR 2 (%q)", style, seq)
				}
			}
		}
	}
}

// ── the late reply ────────────────────────────────────────────────────────────
//
// detectTheme writes ESC ] 11 ; ? ESC \ and waits 150ms. A terminal behind an
// ssh hop, or another multiplexer, can answer after that — and those bytes then
// arrive in stdin, where inputLoop forwarded them to the focused pane as if the
// user had typed them. The child received a raw
// ESC]11;rgb:ffff/ffff/eeee ESC\ as INPUT, which for a REPL is a line of
// garbage and for Claude Code is a prompt it did not deserve.
//
// magmux asked the question, so magmux eats the answer whenever it arrives.
// The regression risk points the other way, and is much worse: a filter that
// swallowed anything else would be magmux silently eating the user's typing.
// Every test here is therefore paired with an assertion about what still gets
// through.

// TestTakeLateOSC11Classifies is the unit level: exactly what is a reply, what
// is a prefix of one, and what is somebody typing.
func TestTakeLateOSC11Classifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		hold bool
	}{
		{"BEL-terminated reply", "\x1b]11;rgb:1e1e/1e1e/2e2e\x07", 24, false},
		{"ST-terminated reply", "\x1b]11;rgb:ffff/ffff/eeee\x1b\\", 25, false},
		{"a reply with a keystroke behind it", "\x1b]11;rgb:f/f/e\x07q", 15, false},
		// A bare ESC is deliberately NOT held — see takeLateOSC11.
		{"a bare ESC, which is also a key the user can press", "\x1b", 0, false},
		{"the opening, two bytes in", "\x1b]", 0, true},
		{"…four", "\x1b]11", 0, true},
		{"…the whole opening", "\x1b]11;", 0, true},
		{"an unterminated body", "\x1b]11;rgb:ffff/ff", 0, true},
		{"a body that never ends", "\x1b]11;" + strings.Repeat("r", osc11MaxHold), 0, false},
		{"another OSC entirely", "\x1b]0;a window title\x07", 0, false},
		{"an OSC 1 (icon name), which starts the same way", "\x1b]1;x\x07", 0, false},
		{"an OSC 110, which starts the same way for longer", "\x1b]110;\x07", 0, false},
		{"an arrow key", "\x1b[A", 0, false},
		{"a bare escape", "\x1b\x1b", 0, false},
		{"a keystroke", "q", 0, false},
		{"a control byte in the body is not a reply", "\x1b]11;rgb:\x01\x07", 0, false},
		{"nothing at all", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, hold := takeLateOSC11([]byte(tc.in))
			if n != tc.n || hold != tc.hold {
				t.Errorf("takeLateOSC11(%q) = (%d, %v), want (%d, %v)",
					tc.in, n, hold, tc.n, tc.hold)
			}
		})
	}
}

// inputHarness runs the real inputLoop against a stdin the test controls, with
// a focused pane whose PTY is a pipe the test can read.
//
// It is deliberately the whole loop and not the filter: the property under test
// is "this never reaches a CHILD", and every cheaper level would be asserting
// on the filter rather than on what the filter protects.
type inputHarness struct {
	mux   *Magmux
	stdin *os.File // write end — what the terminal and the user send magmux
	pane  *os.File // read end — what magmux typed into the focused pane
	done  chan struct{}
}

// newInputHarness starts inputLoop with the theme query recorded as having been
// written at `asked`. The zero time means it never was (an explicit --theme),
// which must leave the loop behaving exactly as it always has. Any setup
// functions run BEFORE the loop starts, because everything they touch is read
// from it.
func newInputHarness(t *testing.T, asked time.Time, setup ...func(*Magmux)) *inputHarness {
	t.Helper()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	paneR, paneW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pane pipe: %v", err)
	}

	m := newTestMux(t, PaneConfig{Control: true})
	m.gridMode = false
	leaf := m.allPanes[0]
	// A pipe for a PTY: writePTY does not care which, and this is a test about
	// what reaches the child rather than about ptys. isControl is cleared so
	// the keys are routed to the pane instead of scrolling the panel.
	leaf.isControl = false
	leaf.gridMode = false
	leaf.ptmx = paneW
	m.themeAskedAt = asked
	m.stdin = stdinR
	if m.focusedPane() != leaf {
		t.Fatalf("the harness's pane is not focused; nothing typed would reach it")
	}
	for _, fn := range setup {
		fn(m)
	}

	h := &inputHarness{mux: m, stdin: stdinW, pane: paneR, done: make(chan struct{})}
	go func() { m.inputLoop(); close(h.done) }()

	t.Cleanup(func() {
		stdinW.Close() // EOF → the loop's reader closes its channel → it returns
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Error("inputLoop did not return after stdin closed")
		}
		// stdinR is left to the reader goroutine, which may still be parked in
		// it when the loop returns through m.quit rather than through EOF.
		paneW.Close()
		paneR.Close()
	})
	return h
}

// send writes to magmux's stdin, as the terminal or the user would.
func (h *inputHarness) send(t *testing.T, s string) {
	t.Helper()
	if _, err := h.stdin.Write([]byte(s)); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
}

// typed returns everything magmux typed into the pane within d.
func (h *inputHarness) typed(t *testing.T, d time.Duration) string {
	t.Helper()
	if err := h.pane.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("read deadline: %v", err)
	}
	var out []byte
	buf := make([]byte, 512)
	for {
		n, err := h.pane.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return string(out)
		}
	}
}

const osc11Reply = "\x1b]11;rgb:ffff/ffff/eeee\x1b\\"

// TestLateOSC11ReplyNeverReachesAPane is the bug.
func TestLateOSC11ReplyNeverReachesAPane(t *testing.T) {
	h := newInputHarness(t, time.Now())
	h.send(t, osc11Reply)
	h.send(t, "A") // …and a keystroke behind it, which must still land
	if got := h.typed(t, 300*time.Millisecond); got != "A" {
		t.Errorf("the pane received %q, want %q — magmux asked the terminal for its "+
			"background and then typed the answer into a child that never asked", got, "A")
	}
}

// TestSplitOSC11ReplyIsNeverPartiallyForwarded is the case a naive filter gets
// wrong: a reply that spans two reads must not have its first half forwarded
// while magmux is still making up its mind about the second.
func TestSplitOSC11ReplyIsNeverPartiallyForwarded(t *testing.T) {
	h := newInputHarness(t, time.Now())
	h.send(t, osc11Reply[:11]) // "\x1b]11;rgb:f"
	if got := h.typed(t, 200*time.Millisecond); got != "" {
		t.Fatalf("half a reply reached the pane: %q", got)
	}
	h.send(t, osc11Reply[11:]+"B")
	if got := h.typed(t, 300*time.Millisecond); got != "B" {
		t.Errorf("the pane received %q, want %q", got, "B")
	}
}

// TestOrdinaryInputSurvivesTheOSC11Window is the regression guard, and it is
// the important one: whatever the window does, it must not be possible for
// magmux to swallow real input.
func TestOrdinaryInputSurvivesTheOSC11Window(t *testing.T) {
	// Typing, an arrow key, an ESC that is not ours, and a DIFFERENT OSC —
	// terminated with ST rather than BEL, because BEL is Ctrl-G, which is
	// magmux's own command key and would be consumed by the loop's existing
	// (and unrelated) prefix handling.
	const keys = "hello\rq\x1b[A\x1b]0;a window title\x1b\\"
	h := newInputHarness(t, time.Now())
	h.send(t, keys)
	if got := h.typed(t, 300*time.Millisecond); got != keys {
		t.Errorf("the pane received %q, want %q — the theme-reply window is eating "+
			"input that was never a reply", got, keys)
	}
}

// TestFinishedGridStillQuitsOnEscInsideTheWindow is the second half of the
// regression guard, and it is the case that made the filter's shape what it is.
//
// The obvious implementation holds a bare ESC while it waits to see whether an
// OSC 11 reply is arriving behind it. But ESC is the key that dismisses a
// finished grid, and holding it until some unrelated byte turns up makes magmux
// look hung at exactly the moment the user is trying to leave — with the window
// open for the first seconds of the run, which is when a `-w` grid of quick
// commands finishes.
func TestFinishedGridStillQuitsOnEscInsideTheWindow(t *testing.T) {
	h := newInputHarness(t, time.Now(), func(m *Magmux) {
		m.gridMode = true
		p := m.allPanes[0]
		p.mu.Lock()
		p.dead = true // the run is over; ESC dismisses the window
		p.mu.Unlock()
	})
	h.send(t, "\x1b")
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Esc did not quit a finished grid — the theme-reply window is holding " +
			"the one key that dismisses it")
	}
}

// TestOSC11ShapedInputOutsideTheWindowIsForwarded pins the other edge: the
// swallow is a short, bounded consequence of a question magmux asked, not a
// permanent filter on the input stream.
func TestOSC11ShapedInputOutsideTheWindowIsForwarded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asked time.Time
	}{
		{"after the window has closed", time.Now().Add(-time.Hour)},
		{"when magmux never asked (--theme was explicit)", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newInputHarness(t, tc.asked)
			h.send(t, osc11Reply)
			if got := h.typed(t, 300*time.Millisecond); got != osc11Reply {
				t.Errorf("the pane received %q, want the whole sequence %q",
					got, osc11Reply)
			}
		})
	}
}

// ── debug line ───────────────────────────────────────────────────────────────

// themeDebugLine assembles the line initTheme logs, from the same pieces
// (section 4.3): "theme: <kind> via <source> (probe <state>; N bytes of
// input preserved; background rgb:...)". The pieces are what the test owns;
// the fully assembled line is also captured from initTheme through dbgFile
// in TestThemeDebugLineNamesSource's last subtest.
func themeDebugLine(res theme.Resolution, back theme.RGB) string {
	return fmt.Sprintf("theme: %s via %s (probe %s; %d bytes of input preserved; background %s)",
		res.Kind, res.Source, probeState(res), len(res.Leftover), theme.XColorString(back))
}

// TestThemeDebugLineNamesSource: the line names who decided, and never says
// the theme was not detected — "probe skipped" is a fact about the probe, not
// about the theme. Without the feature there is no source in the line, or a
// TERM_THEME run reads as not detected, and the must-not-contain assertions
// report it.
func TestThemeDebugLineNamesSource(t *testing.T) {
	latte := theme.RGB{R: 0xEF, G: 0xF1, B: 0xF5}
	cases := []struct {
		name     string
		res      theme.Resolution
		contains []string
	}{
		{"dark via TERM_THEME, probe skipped",
			theme.Resolution{Kind: theme.Dark, Source: theme.SourceTermTheme},
			[]string{"dark via TERM_THEME", "probe skipped", "0 bytes of input preserved"}},
		{"light via OSC 11, probe answered, one keystroke kept",
			theme.Resolution{Kind: theme.Light, Source: theme.SourceProbe, Probed: latte, ProbedOK: true, ProbeRan: true, Leftover: []byte("q")},
			[]string{"light via OSC 11", "probe answered", "1 bytes of input preserved"}},
		{"light via COLORFGBG, probe ran and did not answer",
			theme.Resolution{Kind: theme.Light, Source: theme.SourceColorFGBG, ProbeRan: true},
			[]string{"light via COLORFGBG", "probe no answer"}},
		{"dark via default, probe skipped",
			theme.Resolution{Kind: theme.Dark, Source: theme.SourceDefault},
			[]string{"dark via default", "probe skipped"}},
		{"dark via default, probe ran",
			theme.Resolution{Kind: theme.Dark, Source: theme.SourceDefault, ProbeRan: true},
			[]string{"probe no answer"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := themeDebugLine(c.res, latte)
			assertThemeLine(t, line, c.contains)
		})
	}

	// The real line, as initTheme writes it, captured through dbgFile: a
	// headless TERM_THEME=dark run must be reported as dark via TERM_THEME
	// with the probe skipped, and never as a theme nobody detected.
	t.Run("initTheme writes the line through dbgFile", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "")
		t.Setenv("TERM_THEME", "dark")
		t.Setenv("COLORFGBG", "0;15")
		defer useTheme(theme.Current)()

		f, err := os.CreateTemp(t.TempDir(), "dbg")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		prev := dbgFile
		dbgFile = f
		defer func() { dbgFile = prev }()

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		m := &Magmux{stdin: r, headless: true}
		m.initTheme(int(r.Fd()))

		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		var line string
		for _, l := range strings.Split(string(data), "\n") {
			if i := strings.Index(l, "theme: "); i >= 0 {
				line = l[i:]
			}
		}
		if line == "" {
			t.Fatalf("initTheme logged no \"theme: \" line through dbgFile; log was %q", data)
		}
		assertThemeLine(t, line, []string{"dark via TERM_THEME", "probe skipped", "0 bytes of input preserved"})
	})
}

// assertThemeLine is the shared shape check for the debug line.
func assertThemeLine(t *testing.T, line string, contains []string) {
	t.Helper()
	if !strings.HasPrefix(line, "theme: ") {
		t.Errorf("line %q does not begin with \"theme: \"", line)
	}
	if !strings.Contains(line, "background rgb:") {
		t.Errorf("line %q does not name the background", line)
	}
	for _, want := range contains {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	for _, banned := range []string{"undetec" + "ted", "could not " + "detect"} {
		if strings.Contains(line, banned) {
			t.Errorf("line %q says %q; the source that answered is the whole story", line, banned)
		}
	}
}

// TestNoCouldNotDetectString is VC-10 run under go test rather than by a
// one-off grep: no file in the package prints a "could not ..." detection
// warning (the needle is split below so this file does not match itself).
// It reports the file and line of any offender.
func TestNoCouldNotDetectString(t *testing.T) {
	needles := []string{"could not " + "detect", "undetec" + "ted"}
	// The leaf packages R3 moved out of this one are scanned too, so the test
	// still covers every line it covered when they were files in package mux.
	var files []string
	for _, pat := range []string{"*.go", "../theme/*.go", "../sockdir/*.go", "../pty/*.go", "../proc/*.go", "../mcp/*.go"} {
		m, err := filepath.Glob(pat)
		if err != nil || len(m) == 0 {
			t.Fatalf("glob %s: %v (%d files)", pat, err, len(m))
		}
		files = append(files, m...)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(data), "\n") {
			for _, n := range needles {
				if strings.Contains(strings.ToLower(l), n) {
					t.Errorf("%s:%d contains %q", f, i+1, n)
				}
			}
		}
	}
}

// ── FR7: the environment, and only the environment ───────────────────────────

// TestThemeEnvReadsNoFile is VC-11 at the source level: theme.go opens no
// file, and neither theme.go nor main.go mentions a ".env" outside a comment.
// main.go is now the section files test/reorg/r2-ranges.txt split it into,
// which together hold exactly its lines, so all of them are scanned.
// The files are read as TEXT for a grep, nothing more. Reports the offending
// line if a .env loader or a file read ever lands on the theme path.
func TestThemeEnvReadsNoFile(t *testing.T) {
	stripComment := func(l string) string {
		if i := strings.Index(l, "//"); i >= 0 {
			return l[:i]
		}
		return l
	}
	// theme.go is package theme's since R3, so it is read from there; the
	// thirteen section files are the same lines as before.
	for _, f := range []string{"../theme/theme.go", "cell.go", "screen.go", "scrollback.go", "vt.go", "pane.go", "render.go",
		"mux.go", "selection.go", "chrome.go", "socket.go", "grid.go", "dynpanes.go", "cli.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(data), "\n") {
			code := stripComment(l)
			// A ".env" FILE: quoted or path-joined. (A field named env, as in
			// in.Env, is not a file.)
			for _, needle := range []string{`".env`, `/.env`, `.env"`} {
				if strings.Contains(code, needle) {
					t.Errorf("%s:%d names a .env file outside a comment: %q", f, i+1, l)
				}
			}
			if f != "../theme/theme.go" {
				continue
			}
			for _, bad := range []string{"os.Open", "os.ReadFile", "ioutil.ReadFile", "godotenv", "bufio.NewScanner"} {
				if strings.Contains(code, bad) {
					t.Errorf("theme.go:%d uses %s; the theme path must read the process env only: %q", i+1, bad, l)
				}
			}
			if strings.Contains(code, "TERM_THEME") || strings.Contains(code, "COLORFGBG") {
				if !strings.Contains(code, "os.Getenv(") && !strings.Contains(code, `"`) {
					t.Errorf("theme.go:%d reads TERM_THEME/COLORFGBG without os.Getenv: %q", i+1, l)
				}
			}
		}
	}
}

// TestThemeEnvSeamReadsProcessEnv proves FR7 positively: the seam hands back
// the raw process environment (untrimmed — normalisation is theme.Word's job),
// and initTheme reads through the seam and nowhere else. Without the seam this
// does not compile; with an initTheme that calls os.Getenv directly, the
// second half reports "dark from the process env" although the seam said light.
func TestThemeEnvSeamReadsProcessEnv(t *testing.T) {
	t.Setenv("MAGMUX_THEME", "auto")
	t.Setenv("TERM_THEME", " Dark ")
	t.Setenv("COLORFGBG", "0;15")

	got := theme.Env()
	want := theme.Inputs{Env: "auto", TermTheme: " Dark ", ColorFGBG: "0;15"}
	if got != want {
		t.Errorf("theme.Env() = %+v, want %+v", got, want)
	}

	old := theme.Env
	theme.Env = func() theme.Inputs { return theme.Inputs{TermTheme: "light"} }
	defer func() { theme.Env = old }()
	defer useTheme(theme.Current)()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	m := &Magmux{stdin: r, headless: true}
	m.initTheme(int(r.Fd()))
	if theme.Current != theme.Light {
		t.Errorf("theme is %s: initTheme read the process env (TERM_THEME=%q) instead of the seam", theme.Current, os.Getenv("TERM_THEME"))
	}
}

// TestChildIsToldTheResolvedTheme pins the contract that a pane's child learns
// which background magmux settled on, as MAGMUX_THEME=light|dark.
//
// A TUI child does not need it — it queries OSC 11 and answerColorQuery replies
// from the same resolution. A child that is not a TUI has no way to ask, and
// pilot/pilot.ts is the case that proved it: a plain script writing ANSI, which
// hardcoded one background's palette and was illegible on the other.
//
// The third case is the load-bearing one. The variable is an INPUT to magmux as
// well as an output, so a value inherited from the shell must lose to what
// magmux actually resolved — otherwise `--theme light` under a dark shell tells
// every child the opposite of what magmux is drawing. It holds because the
// export is appended after os.Environ() and os/exec keeps the last occurrence.
func TestChildIsToldTheResolvedTheme(t *testing.T) {
	cases := []struct {
		name      string
		inherited string // MAGMUX_THEME in magmux's own environment
		flag      string // --theme
		want      string
	}{
		{name: "flag_light", flag: "light", want: "light"},
		{name: "flag_dark", flag: "dark", want: "dark"},
		{name: "flag_beats_inherited", inherited: "dark", flag: "light", want: "light"},
		{name: "inherited_when_no_flag", inherited: "light", want: "light"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "theme")
			args := []string{"-w", "-e", "printf '%s' \"$MAGMUX_THEME\" > " + out}
			if c.flag != "" {
				args = append([]string{"--theme", c.flag}, args...)
			}
			// Cleared, not left to the ambient environment: TERM_THEME or
			// COLORFGBG on the developer's machine would otherwise decide the
			// no-flag case and the test would pass or fail by whose terminal
			// ran it.
			t.Setenv("MAGMUX_THEME", c.inherited)
			t.Setenv("TERM_THEME", "")
			t.Setenv("COLORFGBG", "")
			h := startHeadlessMagmux(t, args...)
			if code := h.wait(20 * time.Second); code != 0 {
				t.Fatalf("exit code %d, want 0\nstderr: %s", code, h.stderr.String())
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("child wrote no MAGMUX_THEME: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("child saw MAGMUX_THEME=%q, want %q", got, c.want)
			}
		})
	}
}

// ── contrast ─────────────────────────────────────────────────────────────────

// wcagLuminance is the WCAG 2.x relative luminance: sRGB linearised, then
// weighted. Distinct from theme.ScreenLuminance, which classifies a background and
// deliberately does not linearise — see the comment there.
func wcagLuminance(c theme.RGB) float64 {
	lin := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// contrastRatio is WCAG's (L1+0.05)/(L2+0.05), lighter over darker.
func contrastRatio(a, b theme.RGB) float64 {
	la, lb := wcagLuminance(a), wcagLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
