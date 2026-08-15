package main

// Theme — which palette magmux paints its own chrome in.
//
// Everything magmux draws itself (the control panel, pane borders, the idle
// tint) used to be picked for a dark terminal and painted onto whatever
// background the user's terminal happened to have. On a light terminal the
// result was a near-white #CDD6F4 body text on cream, and chrome that was
// documented as "low-contrast and recedes" receding to literally nothing.
//
// The fix has two halves, and both live here:
//
//  1. detectTheme asks the terminal what its background actually is (OSC 11)
//     and classifies it by luminance.
//  2. `pal` is the selected palette. It is a VALUE, chosen once at startup, so
//     "what colour is body text" has exactly one answer per run and adding a
//     second theme costs a struct literal rather than eleven package vars.
//
// Fallback is always dark: an unanswered query, a malformed reply, a
// non-terminal stdin and TERM=dumb all land on the palette magmux has always
// shipped, so the failure mode is "unchanged", never "unreadable".
//
// ── The sharp edge ────────────────────────────────────────────────────────────
//
// The probe READS STDIN, which is also where the user's keystrokes arrive. It
// therefore runs exactly once, synchronously, inside mux.init() — after
// term.MakeRaw and before inputLoop's stdin goroutine exists — so there is
// never more than one reader of stdin at a time. Any byte it reads that is not
// part of the reply is handed back to the caller (see probeTheme's second
// return value) and replayed into the input loop ahead of everything else. A
// theme probe that eats a keystroke is a worse bug than a wrong palette.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type themeKind int

const (
	themeDark themeKind = iota
	themeLight
)

func (k themeKind) String() string {
	if k == themeLight {
		return "light"
	}
	return "dark"
}

// rgb is a 24-bit colour. Panel colours are truecolor rather than indexed
// because the palette has to be able to state its own contrast ratios, and an
// index means whatever the user's theme decided it means.
type rgb struct{ r, g, b uint8 }

func fg(c rgb) string {
	return "\x1b[38;2;" + itoa(c.r) + ";" + itoa(c.g) + ";" + itoa(c.b) + "m"
}

func bg(c rgb) string {
	return "\x1b[48;2;" + itoa(c.r) + ";" + itoa(c.g) + ";" + itoa(c.b) + "m"
}

func itoa(v uint8) string { return strconv.Itoa(int(v)) }

// toColor converts a palette entry to the renderer's Color, so main.go's own
// chrome can be driven from the same palette as the panel.
func toColor(c rgb) Color { return Color{R: c.r, G: c.g, B: c.b, True: true} }

// ── Palettes ──────────────────────────────────────────────────────────────────
//
// Semantic, not decorative: one colour means one thing, and the two palettes
// carry the SAME semantics — only the values differ.
//
// The bug these palettes exist for was a FOREGROUND bug: every value was
// Catppuccin Mocha, so on a light terminal the panel's #CDD6F4 body text sat on
// cream at 1.31:1. The cure is a second set of foregrounds, not a background of
// our own — magmux is a multiplexer and the terminal's background belongs to
// the user. Only two surfaces here are painted by magmux: a `badge`'s chip and
// the status bar. Everything else is a foreground on whatever the terminal
// already has.
//
// Contrast is a contract, not a matter of taste, and TestPaletteContrast
// enforces it: body colours ≥ 4.5:1 and chrome ≥ 3:1 against BOTH the
// terminal background this palette assumes and the status bar's own
// background, and `ink` ≥ 4.5:1 against every colour a badge is ever filled
// with. The dark palette's border/subtle/debug were lifted a step to meet that
// bar — they still read as chrome, they are just no longer invisible, which was
// half the reported bug.
type palette struct {
	// assumedBack is the terminal background this palette is designed for.
	// magmux never paints it: it is the yardstick the contrast test measures
	// every foreground against, and the reason there are two palettes at all.
	assumedBack rgb

	// bar is the surface magmux paints on: the status bar's background, and —
	// since the completion overlay stopped being hardcoded 256-colour — the
	// inside of that box too. One surface, not two, so a foreground proven
	// legible on the bar is legible in the overlay by the same measurement;
	// every foreground written on either is measured against THIS, not against
	// assumedBack. A surface that sets its own background is a convention worth
	// keeping — it separates magmux's own pixels from the child's — but the
	// colour has to belong to the active theme.
	bar rgb

	// shadow is the overlay's drop shadow: a *shade* of the terminal's own
	// background, in both directions. It is the one palette entry that is not a
	// foreground and never has a glyph on it (the shadow paints spaces in its
	// own colour), so its contract is not a contrast ratio but a direction —
	// darker than assumedBack, and near enough to it to read as a shadow rather
	// than a hole. The old hardcoded 48;5;235 was a near-black slab, which on a
	// light terminal was 13.4:1 against the background: not a shadow, a smear.
	shadow rgb

	success rgb // turn completed / session idle
	running rgb // controller instruction in flight
	warn    rgb // tool working
	fail    rgb // error / permission block
	accent  rgb // titles, focus
	text    rgb // body text
	subtle  rgb // labels, timestamps
	border  rgb // rules, pane splits
	ink     rgb // text on a saturated badge
	dead    rgb // absent / not applicable
	debug   rgb // secondary data (tool names)
}

// darkPalette is Catppuccin Mocha, the palette magmux has always shipped.
var darkPalette = palette{
	assumedBack: rgb{0x1E, 0x1E, 0x2E}, // Mocha base
	bar:         rgb{0x18, 0x18, 0x25}, // Mocha mantle — a step under the panes
	shadow:      rgb{0x11, 0x11, 0x1B}, // Mocha crust — a step under the bar
	success:     rgb{0x2E, 0xCC, 0x71},
	running:     rgb{0x34, 0x98, 0xDB},
	warn:        rgb{0xFF, 0xB4, 0x54},
	fail:        rgb{0xFF, 0x6B, 0x6B},
	accent:      rgb{0x89, 0xB4, 0xFA},
	text:        rgb{0xCD, 0xD6, 0xF4},
	subtle:      rgb{0x7F, 0x84, 0x97},
	border:      rgb{0x6A, 0x6D, 0x82},
	ink:         rgb{0x11, 0x11, 0x1B},
	dead:        rgb{0x7F, 0x84, 0x97},
	debug:       rgb{0x94, 0x9A, 0xAF},
}

// lightPalette is Catppuccin Latte's ground, with the saturated states pulled
// darker than Latte's own: Latte picks its accents for large type, and these
// are single glyphs and 4-column badges on a cream background.
var lightPalette = palette{
	assumedBack: rgb{0xEF, 0xF1, 0xF5}, // Latte base
	bar:         rgb{0xE6, 0xE9, 0xEF}, // Latte mantle
	// Latte surface1. A shadow is dark in both themes — but on cream, "dark"
	// means a grey a step under the page, not the near-black the dark theme
	// uses. Getting this wrong in the other direction is what the old shadow
	// did.
	shadow:  rgb{0xBC, 0xC0, 0xCC},
	success: rgb{0x14, 0x72, 0x2F},
	running: rgb{0x0C, 0x63, 0xB4},
	// Darker than Latte's peach by two steps: it has to clear 4.5:1 on the
	// status bar's own background as well as on the terminal's.
	warn:   rgb{0x8F, 0x54, 0x00},
	fail:   rgb{0xB3, 0x26, 0x1E},
	accent: rgb{0x0B, 0x57, 0xD0},
	text:   rgb{0x4C, 0x4F, 0x69},
	subtle: rgb{0x6C, 0x6F, 0x85},
	border: rgb{0x7C, 0x80, 0x95},
	ink:    rgb{0xFF, 0xFF, 0xFF},
	dead:   rgb{0x6C, 0x6F, 0x85},
	debug:  rgb{0x5C, 0x5F, 0x77},
}

// pal is the palette in force. Written once at startup (setTheme) before any
// goroutine that paints exists, and read-only from then on — which is why it
// needs no lock, and why setTheme must never be called from a render path.
var pal = darkPalette

// currentTheme is which palette pal holds, for the status line and for tests
// that swap themes and put the old one back.
var currentTheme = themeDark

// termBack / termFore are what magmux answers when a CHILD asks what colour the
// terminal is (OSC 11 / OSC 10 / OSC 12 — see answerColorQuery in main.go).
//
// magmux is the terminal as far as a child is concerned, so it owes an answer
// to a question it has always ignored, and theme-aware TUIs block on it: Claude
// Code queries OSC 11 at startup to pick light or dark, and rendered nothing at
// all inside magmux because nothing ever replied.
//
// They track the palette, so the answer is always self-consistent with what
// magmux itself is drawing, and they are OVERWRITTEN by the real value when the
// probe managed to read one (setDetectedBackground). A guess that matches our
// own chrome is a fine answer; no answer is the bug.
var termBack, termFore = darkPalette.assumedBack, darkPalette.text

func setTheme(k themeKind) {
	currentTheme = k
	if k == themeLight {
		pal = lightPalette
	} else {
		pal = darkPalette
	}
	// The assumed values, not the measured one: setTheme is also the reset,
	// which is why initTheme calls setDetectedBackground *after* it.
	termBack, termFore = pal.assumedBack, pal.text
}

// setDetectedBackground records the background the probe actually read off the
// real terminal, so children are told the truth rather than the palette's
// stand-in for it. Must be called after setTheme, which resets it.
func setDetectedBackground(c rgb) { termBack = c }

// terminalColor answers "what colour is the terminal's X" for the OSC codes a
// child may query: 10 foreground, 11 background, 12 cursor. The cursor gets the
// foreground, which is xterm's own default and the only answer we can give that
// is certain to be visible against the background we just reported.
func terminalColor(code string) (rgb, bool) {
	switch code {
	case "10", "12":
		return termFore, true
	case "11":
		return termBack, true
	}
	return rgb{}, false
}

// xColorString renders c the way terminals answer OSC 10/11/12: X11's
// "rgb:RRRR/GGGG/BBBB" with 16-bit components. Each 8-bit value is doubled
// rather than shifted so that 0xFF is 0xFFFF and full white stays full white —
// and so parseXColor round-trips it exactly.
func xColorString(c rgb) string {
	return fmt.Sprintf("rgb:%02x%02x/%02x%02x/%02x%02x", c.r, c.r, c.g, c.g, c.b, c.b)
}

// ── Detection ─────────────────────────────────────────────────────────────────

// themeProbeTimeout is how long detectTheme waits for the terminal to answer.
// Terminals that implement OSC 11 answer in single-digit milliseconds; the
// ones that do not never answer at all, and this is the whole cost of asking
// them. It is paid once, before the first child is spawned.
const themeProbeTimeout = 150 * time.Millisecond

// osc11Query asks for the background colour. ST-terminated, because a terminal
// that does not understand the sequence must not be left waiting for a
// terminator it will never see; the REPLY is accepted with either terminator.
const osc11Query = "\x1b]11;?\x1b\\"

// detectTheme asks the terminal for its background colour and classifies it.
//
// The second return value is every byte read that was NOT part of the reply —
// keystrokes that arrived while the terminal was thinking. It is not optional
// and it is not droppable: the caller must feed it to the input loop, in
// order, ahead of anything read later. A signature that returned only the
// theme would be a signature that silently ate input, so there isn't one.
func detectTheme(f *os.File, timeout time.Duration) (themeKind, []byte) {
	kind, _, _, rest := detectThemeColor(f, timeout)
	return kind, rest
}

// detectThemeColor is detectTheme that also hands back the background it read,
// and whether it read one at all. The colour is not just an input to the
// light/dark decision: it is the answer magmux owes any child that asks the
// same question (OSC 11), and a classification alone cannot be turned back into
// one. Keep it.
func detectThemeColor(f *os.File, timeout time.Duration) (themeKind, rgb, bool, []byte) {
	return probeThemeColor(f, f, timeout)
}

// probeTheme is detectTheme with the two halves of the tty separated, so a
// test can drive it with a pipe it controls and assert on what was written.
func probeTheme(out io.Writer, in *os.File, timeout time.Duration) (themeKind, []byte) {
	kind, _, _, rest := probeThemeColor(out, in, timeout)
	return kind, rest
}

func probeThemeColor(out io.Writer, in *os.File, timeout time.Duration) (themeKind, rgb, bool, []byte) {
	if out == nil || in == nil {
		return themeDark, rgb{}, false, nil
	}
	if _, err := io.WriteString(out, osc11Query); err != nil {
		return themeDark, rgb{}, false, nil
	}

	fd := int(in.Fd())
	deadline := time.Now().Add(timeout)
	var buf []byte
	chunk := make([]byte, 256)
	for {
		ready, err := waitReadable(fd, deadline)
		if err != nil || !ready {
			break
		}
		n, rerr := in.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if body, rest, ok := cutOSC11(buf); ok {
			if kind, c, ok := classifyOSC11(body); ok {
				return kind, c, true, rest
			}
			// A reply we cannot parse is still a reply: it is consumed, and
			// the fallback is dark, but the keystrokes around it survive.
			return themeDark, rgb{}, false, rest
		}
		if rerr != nil {
			break
		}
		// A terminal that streams without ever terminating must not be able to
		// grow this without bound, deadline or no deadline.
		if len(buf) > 8192 {
			break
		}
	}
	return themeDark, rgb{}, false, dropPartialOSC11(buf)
}

// waitReadable blocks until fd has bytes or the deadline passes.
//
// This is select(2) and not a goroutine parked in Read for a reason: a
// goroutine that is still blocked on stdin when the timeout fires goes on to
// swallow the user's next keystroke, which is exactly the failure this whole
// file is arranged to avoid. EINTR is retried because Go's own async
// preemption (SIGURG) interrupts select routinely.
func waitReadable(fd int, deadline time.Time) (bool, error) {
	if fd < 0 {
		return false, nil
	}
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false, nil
		}
		var set unix.FdSet
		set.Zero()
		set.Set(fd)
		tv := unix.NsecToTimeval(left.Nanoseconds())
		n, err := unix.Select(fd+1, &set, nil, nil, &tv)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return false, err
		}
		return n > 0, nil
	}
}

// cutOSC11 finds a complete OSC 11 reply in buf. It returns the reply body,
// everything else in order (what came before it followed by what came after),
// and whether a complete reply was found at all.
//
// Accepts both terminators: xterm answers BEL, others answer ST.
func cutOSC11(buf []byte) (body string, rest []byte, ok bool) {
	const prefix = "\x1b]11;"
	s := string(buf)
	i := strings.Index(s, prefix)
	if i < 0 {
		return "", nil, false
	}
	tail := s[i+len(prefix):]
	end, after := -1, 0
	if j := strings.IndexByte(tail, '\x07'); j >= 0 {
		end, after = j, j+1
	}
	if j := strings.Index(tail, "\x1b\\"); j >= 0 && (end < 0 || j < end) {
		end, after = j, j+2
	}
	if end < 0 {
		return "", nil, false
	}
	leftover := s[:i] + tail[after:]
	return tail[:end], []byte(leftover), true
}

// dropPartialOSC11 is the timeout path's leftover: everything read, minus a
// trailing fragment that had started an OSC 11 reply and never finished it.
//
// Handing that fragment to the input loop would be worse than dropping it — it
// begins with ESC, and an ESC in grid mode with every pane done is the quit
// key. Anything that is not a truncated reply is preserved untouched, which is
// the case that matters: keystrokes never look like one.
func dropPartialOSC11(buf []byte) []byte {
	const prefix = "\x1b]11;"
	s := string(buf)
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '\x1b' {
			continue
		}
		frag := s[i:]
		// A prefix of the reply's own opening, or an opening with an
		// unterminated body behind it.
		if strings.HasPrefix(frag, prefix) || strings.HasPrefix(prefix, frag) {
			s = s[:i]
			break
		}
	}
	if s == "" {
		return nil
	}
	return []byte(s)
}

// classifyOSC11 turns a reply body ("rgb:1e1e/1e1e/2e2e") into a theme, and
// hands back the colour it parsed so the caller can serve it to children.
func classifyOSC11(body string) (themeKind, rgb, bool) {
	c, ok := parseXColor(body)
	if !ok {
		return themeDark, rgb{}, false
	}
	if screenLuminance(c) >= lightThreshold {
		return themeLight, c, true
	}
	return themeDark, c, true
}

// lightThreshold is the luminance at which a background stops being something
// you put light text on. The midpoint is the honest choice: it is where a
// background stops being darker than a mid grey, and real terminal themes are
// nowhere near it — Mocha's base sits at 0.13 and Latte's at 0.94, so the
// classification is stable to within a factor of three either way.
const lightThreshold = 0.5

// screenLuminance is the Rec.709 weighting on plain 0..1 channel values.
//
// Deliberately NOT the gamma-corrected WCAG luminance used to check contrast
// (see TestPaletteContrast): that one linearises, which drags every mid tone
// down — plain #808080 scores 0.216 and would be classified as a dark
// background, when a viewer would call it neither. For "which way round should
// the text be", the perceptual value is the right one.
func screenLuminance(c rgb) float64 {
	r := float64(c.r) / 255
	g := float64(c.g) / 255
	b := float64(c.b) / 255
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// parseXColor parses X11's "rgb:RRRR/GGGG/BBBB" as terminals actually emit it.
// Components may be 1 to 4 hex digits and the widths in one reply need not
// agree; each is scaled to 8 bits rather than truncated, so "f" is 0xFF and
// not 0x0F.
func parseXColor(s string) (rgb, bool) {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	switch {
	case strings.HasPrefix(low, "rgba:"):
		s = s[len("rgba:"):]
	case strings.HasPrefix(low, "rgb:"):
		s = s[len("rgb:"):]
	default:
		return rgb{}, false
	}
	parts := strings.Split(s, "/")
	if len(parts) < 3 {
		return rgb{}, false
	}
	var out [3]uint8
	for i := 0; i < 3; i++ {
		v, ok := scaleHex(parts[i])
		if !ok {
			return rgb{}, false
		}
		out[i] = v
	}
	return rgb{out[0], out[1], out[2]}, true
}

// scaleHex reads a 1-4 digit hex component and scales it to 0..255.
func scaleHex(s string) (uint8, bool) {
	if len(s) < 1 || len(s) > 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	max := uint64(1)<<(4*len(s)) - 1
	return uint8((v*255 + max/2) / max), true
}

// ── Preference ────────────────────────────────────────────────────────────────

// themeSetting normalises the requested mode. The flag wins over the
// environment, and anything unrecognised is "auto" — a typo must not be able
// to pin the wrong palette silently.
func themeSetting(flagVal, envVal string) string {
	for _, v := range []string{flagVal, envVal} {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "light":
			return "light"
		case "dark":
			return "dark"
		case "auto":
			return "auto"
		}
	}
	return "auto"
}

// validThemeSetting reports whether v is a mode a user could have meant, so
// main can say so on stderr instead of silently falling back to auto.
func validThemeSetting(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "light", "dark", "auto":
		return true
	}
	return false
}

// resolveTheme applies the preference. probe is called ONLY for "auto": an
// explicit setting is the escape hatch for a terminal that answers OSC 11
// wrongly, so it must not write to that terminal or read from it at all.
func resolveTheme(pref string, probe func() (themeKind, []byte)) (themeKind, []byte) {
	switch pref {
	case "light":
		return themeLight, nil
	case "dark":
		return themeDark, nil
	}
	if probe == nil {
		return themeDark, nil
	}
	return probe()
}
