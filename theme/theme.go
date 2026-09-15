package theme

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
//  2. `Pal` is the selected palette. It is a VALUE, chosen once at startup, so
//     "what colour is body text" has exactly one answer per run and adding a
//     second theme costs a struct literal rather than eleven package vars.
//
// The palette is resolved ONCE at startup, first answer wins:
//
//	--theme > MAGMUX_THEME > TERM_THEME > OSC 11 probe > COLORFGBG > dark
//
// "auto" is no opinion at EVERY level, --theme and MAGMUX_THEME included: it
// falls through to the next source rather than forcing the probe. (This is a
// change from the earlier rule, where `--theme auto` beat a set MAGMUX_THEME
// and probed.) See Resolve for the walk and Word for what counts as
// an answer.
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

type Kind int

const (
	Dark Kind = iota
	Light
)

func (k Kind) String() string {
	if k == Light {
		return "light"
	}
	return "dark"
}

// RGB is a 24-bit colour. Panel colours are truecolor rather than indexed
// because the palette has to be able to state its own contrast ratios, and an
// index means whatever the user's theme decided it means.
type RGB struct{ R, G, B uint8 }

func Fg(c RGB) string {
	return "\x1b[38;2;" + itoa(c.R) + ";" + itoa(c.G) + ";" + itoa(c.B) + "m"
}

func Bg(c RGB) string {
	return "\x1b[48;2;" + itoa(c.R) + ";" + itoa(c.G) + ";" + itoa(c.B) + "m"
}

func itoa(v uint8) string { return strconv.Itoa(int(v)) }

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
type Palette struct {
	// assumedBack is the terminal background this palette is designed for.
	// magmux never paints it: it is the yardstick the contrast test measures
	// every foreground against, and the reason there are two palettes at all.
	AssumedBack RGB

	// bar is the surface magmux paints on: the status bar's background, and —
	// since the completion overlay stopped being hardcoded 256-colour — the
	// inside of that box too. One surface, not two, so a foreground proven
	// legible on the bar is legible in the overlay by the same measurement;
	// every foreground written on either is measured against THIS, not against
	// assumedBack. A surface that sets its own background is a convention worth
	// keeping — it separates magmux's own pixels from the child's — but the
	// colour has to belong to the active theme.
	Bar RGB

	// shadow is the overlay's drop shadow: a *shade* of the terminal's own
	// background, in both directions. It is the one palette entry that is not a
	// foreground and never has a glyph on it (the shadow paints spaces in its
	// own colour), so its contract is not a contrast ratio but a direction —
	// darker than assumedBack, and near enough to it to read as a shadow rather
	// than a hole. The old hardcoded 48;5;235 was a near-black slab, which on a
	// light terminal was 13.4:1 against the background: not a shadow, a smear.
	Shadow RGB

	Success RGB // turn completed / session idle
	Running RGB // controller instruction in flight
	Warn    RGB // tool working
	Fail    RGB // error / permission block
	Accent  RGB // titles, focus
	Text    RGB // body text
	Subtle  RGB // labels, timestamps
	Border  RGB // rules, pane splits
	Ink     RGB // text on a saturated badge
	Dead    RGB // absent / not applicable
	Debug   RGB // secondary data (tool names)
}

// DarkPalette is Catppuccin Mocha, the palette magmux has always shipped.
var DarkPalette = Palette{
	AssumedBack: RGB{0x1E, 0x1E, 0x2E}, // Mocha base
	Bar:         RGB{0x18, 0x18, 0x25}, // Mocha mantle — a step under the panes
	Shadow:      RGB{0x11, 0x11, 0x1B}, // Mocha crust — a step under the bar
	Success:     RGB{0x2E, 0xCC, 0x71},
	Running:     RGB{0x34, 0x98, 0xDB},
	Warn:        RGB{0xFF, 0xB4, 0x54},
	Fail:        RGB{0xFF, 0x6B, 0x6B},
	Accent:      RGB{0x89, 0xB4, 0xFA},
	Text:        RGB{0xCD, 0xD6, 0xF4},
	Subtle:      RGB{0x7F, 0x84, 0x97},
	Border:      RGB{0x6A, 0x6D, 0x82},
	Ink:         RGB{0x11, 0x11, 0x1B},
	Dead:        RGB{0x7F, 0x84, 0x97},
	Debug:       RGB{0x94, 0x9A, 0xAF},
}

// LightPalette is Catppuccin Latte's ground, with the saturated states pulled
// darker than Latte's own: Latte picks its accents for large type, and these
// are single glyphs and 4-column badges on a cream background.
var LightPalette = Palette{
	AssumedBack: RGB{0xEF, 0xF1, 0xF5}, // Latte base
	Bar:         RGB{0xE6, 0xE9, 0xEF}, // Latte mantle
	// Latte surface1. A shadow is dark in both themes — but on cream, "dark"
	// means a grey a step under the page, not the near-black the dark theme
	// uses. Getting this wrong in the other direction is what the old shadow
	// did.
	Shadow:  RGB{0xBC, 0xC0, 0xCC},
	Success: RGB{0x14, 0x72, 0x2F},
	Running: RGB{0x0C, 0x63, 0xB4},
	// Darker than Latte's peach by two steps: it has to clear 4.5:1 on the
	// status bar's own background as well as on the terminal's.
	Warn:   RGB{0x8F, 0x54, 0x00},
	Fail:   RGB{0xB3, 0x26, 0x1E},
	Accent: RGB{0x0B, 0x57, 0xD0},
	Text:   RGB{0x4C, 0x4F, 0x69},
	Subtle: RGB{0x6C, 0x6F, 0x85},
	Border: RGB{0x7C, 0x80, 0x95},
	Ink:    RGB{0xFF, 0xFF, 0xFF},
	Dead:   RGB{0x6C, 0x6F, 0x85},
	Debug:  RGB{0x5C, 0x5F, 0x77},
}

// Pal is the palette in force. Written once at startup (Set) before any
// goroutine that paints exists, and read-only from then on — which is why it
// needs no lock, and why Set must never be called from a render path.
var Pal = DarkPalette

// Current is which palette Pal holds, for the status line and for tests
// that swap themes and put the old one back.
var Current = Dark

// TermBack / termFore are what magmux answers when a CHILD asks what colour the
// terminal is (OSC 11 / OSC 10 / OSC 12 — see answerColorQuery in main.go).
//
// magmux is the terminal as far as a child is concerned, so it owes an answer
// to a question it has always ignored, and theme-aware TUIs block on it: Claude
// Code queries OSC 11 at startup to pick light or dark, and rendered nothing at
// all inside magmux because nothing ever replied.
//
// They track the palette, so the answer is always self-consistent with what
// magmux itself is drawing, and they are OVERWRITTEN by the real value when the
// probe managed to read one (SetDetectedBackground). A guess that matches our
// own chrome is a fine answer; no answer is the bug.
var TermBack, termFore = DarkPalette.AssumedBack, DarkPalette.Text

func Set(k Kind) {
	Current = k
	if k == Light {
		Pal = LightPalette
	} else {
		Pal = DarkPalette
	}
	// The assumed values, not the measured one: Set is also the reset,
	// which is why initTheme calls SetDetectedBackground *after* it.
	TermBack, termFore = Pal.AssumedBack, Pal.Text
}

// SetDetectedBackground records the background the probe actually read off the
// real terminal, so children are told the truth rather than the palette's
// stand-in for it. Must be called after Set, which resets it.
func SetDetectedBackground(c RGB) { TermBack = c }

// TerminalColor answers "what colour is the terminal's X" for the OSC codes a
// child may query: 10 foreground, 11 background, 12 cursor. The cursor gets the
// foreground, which is xterm's own default and the only answer we can give that
// is certain to be visible against the background we just reported.
func TerminalColor(code string) (RGB, bool) {
	switch code {
	case "10", "12":
		return termFore, true
	case "11":
		return TermBack, true
	}
	return RGB{}, false
}

// XColorString renders c the way terminals answer OSC 10/11/12: X11's
// "rgb:RRRR/GGGG/BBBB" with 16-bit components. Each 8-bit value is doubled
// rather than shifted so that 0xFF is 0xFFFF and full white stays full white —
// and so ParseXColor round-trips it exactly.
func XColorString(c RGB) string {
	return fmt.Sprintf("rgb:%02x%02x/%02x%02x/%02x%02x", c.R, c.R, c.G, c.G, c.B, c.B)
}

// ── Detection ─────────────────────────────────────────────────────────────────

// ProbeTimeout is how long detectTheme waits for the terminal to answer.
// Terminals that implement OSC 11 answer in single-digit milliseconds; the
// ones that do not never answer at all, and this is the whole cost of asking
// them. It is paid once, before the first child is spawned.
const ProbeTimeout = 150 * time.Millisecond

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
func detectTheme(f *os.File, timeout time.Duration) (Kind, []byte) {
	kind, _, _, rest := DetectColor(f, timeout)
	return kind, rest
}

// DetectColor is detectTheme that also hands back the background it read,
// and whether it read one at all. The colour is not just an input to the
// light/dark decision: it is the answer magmux owes any child that asks the
// same question (OSC 11), and a classification alone cannot be turned back into
// one. Keep it.
func DetectColor(f *os.File, timeout time.Duration) (Kind, RGB, bool, []byte) {
	return probeThemeColor(f, f, timeout)
}

// probeTheme is detectTheme with the two halves of the tty separated, so a
// test can drive it with a pipe it controls and assert on what was written.
func probeTheme(out io.Writer, in *os.File, timeout time.Duration) (Kind, []byte) {
	kind, _, _, rest := probeThemeColor(out, in, timeout)
	return kind, rest
}

func probeThemeColor(out io.Writer, in *os.File, timeout time.Duration) (Kind, RGB, bool, []byte) {
	if out == nil || in == nil {
		return Dark, RGB{}, false, nil
	}
	if _, err := io.WriteString(out, osc11Query); err != nil {
		return Dark, RGB{}, false, nil
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
			if kind, c, ok := ClassifyOSC11(body); ok {
				return kind, c, true, rest
			}
			// A reply we cannot parse is still a reply: it is consumed, and
			// the fallback is dark, but the keystrokes around it survive.
			return Dark, RGB{}, false, rest
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
	return Dark, RGB{}, false, dropPartialOSC11(buf)
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

// ClassifyOSC11 turns a reply body ("rgb:1e1e/1e1e/2e2e") into a theme, and
// hands back the colour it parsed so the caller can serve it to children.
func ClassifyOSC11(body string) (Kind, RGB, bool) {
	c, ok := ParseXColor(body)
	if !ok {
		return Dark, RGB{}, false
	}
	if ScreenLuminance(c) >= lightThreshold {
		return Light, c, true
	}
	return Dark, c, true
}

// lightThreshold is the luminance at which a background stops being something
// you put light text on. The midpoint is the honest choice: it is where a
// background stops being darker than a mid grey, and real terminal themes are
// nowhere near it — Mocha's base sits at 0.13 and Latte's at 0.94, so the
// classification is stable to within a factor of three either way.
const lightThreshold = 0.5

// ScreenLuminance is the Rec.709 weighting on plain 0..1 channel values.
//
// Deliberately NOT the gamma-corrected WCAG luminance used to check contrast
// (see TestPaletteContrast): that one linearises, which drags every mid tone
// down — plain #808080 scores 0.216 and would be classified as a dark
// background, when a viewer would call it neither. For "which way round should
// the text be", the perceptual value is the right one.
func ScreenLuminance(c RGB) float64 {
	r := float64(c.R) / 255
	g := float64(c.G) / 255
	b := float64(c.B) / 255
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// ParseXColor parses X11's "rgb:RRRR/GGGG/BBBB" as terminals actually emit it.
// Components may be 1 to 4 hex digits and the widths in one reply need not
// agree; each is scaled to 8 bits rather than truncated, so "f" is 0xFF and
// not 0x0F.
func ParseXColor(s string) (RGB, bool) {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	switch {
	case strings.HasPrefix(low, "rgba:"):
		s = s[len("rgba:"):]
	case strings.HasPrefix(low, "rgb:"):
		s = s[len("rgb:"):]
	default:
		return RGB{}, false
	}
	parts := strings.Split(s, "/")
	if len(parts) < 3 {
		return RGB{}, false
	}
	var out [3]uint8
	for i := 0; i < 3; i++ {
		v, ok := scaleHex(parts[i])
		if !ok {
			return RGB{}, false
		}
		out[i] = v
	}
	return RGB{out[0], out[1], out[2]}, true
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

// Source is which step of the resolution order answered. It exists so the
// debug line can say who decided, and so a test can assert on it instead of on
// a side effect. The zero value is "nothing answered; dark", so a zero
// Resolution reads the way every Magmux{} test literal expects.
type Source int

const (
	SourceDefault   Source = iota // nothing answered; dark
	SourceFlag                    // --theme
	SourceEnv                     // MAGMUX_THEME
	SourceTermTheme               // TERM_THEME
	SourceProbe                   // OSC 11 reply from the terminal
	SourceColorFGBG               // COLORFGBG
)

func (s Source) String() string {
	switch s {
	case SourceFlag:
		return "--theme"
	case SourceEnv:
		return "MAGMUX_THEME"
	case SourceTermTheme:
		return "TERM_THEME"
	case SourceProbe:
		return "OSC 11"
	case SourceColorFGBG:
		return "COLORFGBG"
	}
	return "default"
}

// Inputs is every non-tty input to the order, as raw strings. Resolve
// takes them as a value so it reads no environment itself: the os.Getenv calls
// live in Env and nowhere else, and a table test can cover the full order
// without touching the process environment.
type Inputs struct {
	Flag      string // --theme, "" if not given
	Env       string // MAGMUX_THEME
	TermTheme string // TERM_THEME
	ColorFGBG string // COLORFGBG
}

// Env reads the three environment-valued inputs. It is a variable so a
// test that drives init() can pin them without touching the process
// environment; production reads os.Getenv and only os.Getenv — no file is
// ever opened for TERM_THEME or COLORFGBG. The flag is not here: initTheme
// fills it from m.themePref.
var Env = func() Inputs {
	return Inputs{
		Env:       os.Getenv("MAGMUX_THEME"),
		TermTheme: os.Getenv("TERM_THEME"),
		ColorFGBG: os.Getenv("COLORFGBG"),
	}
}

// ProbeResult is what the OSC 11 step hands back. ok is false when the
// terminal did not answer or answered something unparseable; leftover is every
// byte read that was not the reply, and is returned whether or not ok is set.
type ProbeResult struct {
	Kind     Kind
	Color    RGB
	OK       bool
	Leftover []byte
}

// Resolution is the answer. probedOK is true iff source ==
// SourceProbe, and only then is probed meaningful. leftover is non-nil
// only if the probe ran; probeRan says whether it did, so the debug line can
// tell "skipped" from "no answer".
type Resolution struct {
	Kind     Kind
	Source   Source
	Probed   RGB
	ProbedOK bool
	Leftover []byte
	ProbeRan bool
}

// Word reads one of the three word-valued inputs (--theme, MAGMUX_THEME,
// TERM_THEME). Trimmed, case-insensitive. Only "light" and "dark" are answers;
// "auto", "" and anything else are "no opinion" and the caller moves on.
//
// It deliberately does NOT distinguish auto from garbage: the chain treats both
// as fall-through. The distinction matters only for the --theme warning, which
// ValidSetting makes at flag-parse time.
func Word(v string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "light":
		return Light, true
	case "dark":
		return Dark, true
	}
	return Dark, false
}

// ValidSetting reports whether v is a mode a user could have meant. Its
// ONLY caller is the --theme flag parser, which warns on stderr for anything
// else; MAGMUX_THEME and TERM_THEME are never warned about (the latter is not
// magmux's variable to police), and the chain itself never consults this.
func ValidSetting(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "light", "dark", "auto":
		return true
	}
	return false
}

// classifyColorFGBG reads rxvt/konsole/iTerm2's COLORFGBG ("fg;bg" or
// "fg;x;bg", ANSI colour indexes) and classifies the LAST field, which is the
// background. Index 0-6 and 8 are dark, 7 and 9-15 light; anything else —
// "default", an index outside 0-15, the wrong number of fields — is no opinion.
func classifyColorFGBG(v string) (Kind, bool) {
	parts := strings.Split(strings.TrimSpace(v), ";")
	if len(parts) != 2 && len(parts) != 3 {
		return Dark, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil {
		return Dark, false
	}
	switch {
	case n >= 0 && n <= 6, n == 8:
		return Dark, true
	case n == 7, n >= 9 && n <= 15:
		return Light, true
	}
	return Dark, false
}

// Resolve walks the order and stops at the first answer. It is pure: it
// reads no environment and touches no tty. The probe is invoked only when
// steps 1-3 gave no opinion, and only when it is non-nil; a nil probe means
// "cannot ask" (headless, non-tty, TERM=dumb) and the walk continues to
// COLORFGBG. The last step always answers, so source is never left unset.
//
// The order IS the slice literal below, read top to bottom. There is no second
// copy of it anywhere.
func Resolve(in Inputs, probe func() ProbeResult) Resolution {
	var res Resolution
	type themeStep struct {
		source Source
		answer func() (Kind, bool)
	}
	steps := []themeStep{
		{SourceFlag, func() (Kind, bool) { return Word(in.Flag) }},
		{SourceEnv, func() (Kind, bool) { return Word(in.Env) }},
		{SourceTermTheme, func() (Kind, bool) { return Word(in.TermTheme) }},
		{SourceProbe, func() (Kind, bool) {
			if probe == nil {
				return Dark, false
			}
			pr := probe()
			res.ProbeRan = true
			// Kept even when the probe did not answer: these are keystrokes.
			res.Leftover = pr.Leftover
			if !pr.OK {
				return Dark, false
			}
			res.Probed, res.ProbedOK = pr.Color, true
			return pr.Kind, true
		}},
		{SourceColorFGBG, func() (Kind, bool) { return classifyColorFGBG(in.ColorFGBG) }},
		{SourceDefault, func() (Kind, bool) { return Dark, true }},
	}
	for _, st := range steps {
		if k, ok := st.answer(); ok {
			res.Kind, res.Source = k, st.source
			return res
		}
	}
	panic("unreachable: the default step always answers")
}
