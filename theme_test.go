package main

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
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useTheme swaps the palette for the duration of a test and puts it back.
// Tests do not run in parallel in this package (nothing calls t.Parallel), so
// the global is safe to move.
func useTheme(k themeKind) func() {
	prev := currentTheme
	setTheme(k)
	return func() { setTheme(prev) }
}

// feedPipe returns a pipe whose read end is handed to the probe, and writes
// `feed` into it. The write end is deliberately LEFT OPEN so an empty feed
// means "the terminal never answered" rather than EOF — the timeout path is
// the one that has to be proven bounded.
func feedPipe(t *testing.T, feed string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	if feed != "" {
		if _, err := w.Write([]byte(feed)); err != nil {
			t.Fatalf("write feed: %v", err)
		}
	}
	return r
}

// TestDetectThemeParsesOSC11 covers the reply shapes terminals really send:
// both terminators, and components of 1, 2 and 4 hex digits.
func TestDetectThemeParsesOSC11(t *testing.T) {
	const timeout = 150 * time.Millisecond

	cases := []struct {
		name string
		feed string
		want themeKind
	}{
		{"mocha, BEL terminated", "\x1b]11;rgb:1e1e/1e1e/2e2e\x07", themeDark},
		{"latte, ST terminated", "\x1b]11;rgb:efef/f1f1/f5f5\x1b\\", themeLight},
		{"two hex digits, white", "\x1b]11;rgb:ff/ff/ff\x07", themeLight},
		{"two hex digits, black", "\x1b]11;rgb:00/00/00\x07", themeDark},
		{"one hex digit, white", "\x1b]11;rgb:f/f/f\x07", themeLight},
		{"one hex digit, black", "\x1b]11;rgb:0/0/0\x07", themeDark},
		{"three hex digits, solarized light", "\x1b]11;rgb:fdd/f66/e33\x1b\\", themeLight},
		{"rgba, alpha ignored", "\x1b]11;rgba:ffff/ffff/ffff/ffff\x07", themeLight},
		{"unparseable body", "\x1b]11;banana\x07", themeDark},
		{"not a reply at all", "hello", themeDark},
		{"no reply", "", themeDark},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := feedPipe(t, tc.feed)
			var out bytes.Buffer

			start := time.Now()
			got, _ := probeTheme(&out, in, timeout)
			elapsed := time.Since(start)

			if got != tc.want {
				t.Errorf("classified %q as %s, want %s", tc.feed, got, tc.want)
			}
			if out.String() != osc11Query {
				t.Errorf("wrote %q to the terminal, want the OSC 11 query %q", out.String(), osc11Query)
			}
			// "Never block longer than the timeout, even if the terminal
			// replies with a partial sequence." The slack is for a loaded
			// machine, not for a second timeout.
			if elapsed > 3*timeout {
				t.Errorf("probe took %v with a %v timeout", elapsed, timeout)
			}
		})
	}
}

// TestDetectThemePreservesNonReplyBytes is the important one.
//
// The probe reads stdin. Everything the user typed while the terminal was
// thinking arrives on the same fd, interleaved with (or instead of) the reply,
// and every one of those bytes has to come back out for the input loop, in
// order. magmux's own PTY-driven tests type into the binary within
// milliseconds of start, so this is not a theoretical case: dropping here
// breaks them, and breaks real typing the same way.
func TestDetectThemePreservesNonReplyBytes(t *testing.T) {
	const (
		timeout = 150 * time.Millisecond
		reply   = "\x1b]11;rgb:1e1e/1e1e/2e2e\x07"
	)

	cases := []struct {
		name string
		feed string
		want string
		kind themeKind
	}{
		{"keystroke before the reply", "q" + reply, "q", themeDark},
		{"keystroke after the reply", reply + "\x1b[A", "\x1b[A", themeDark},
		{"keystrokes either side", "ab" + reply + "cd", "abcd", themeDark},
		{"a whole chord around a light reply",
			"\x07q" + "\x1b]11;rgb:efef/f1f1/f5f5\x1b\\" + "\x1b[B",
			"\x07q\x1b[B", themeLight},
		{"no reply, only keystrokes", "hello world", "hello world", themeDark},
		{"reply we cannot parse still yields the keys", "x" + "\x1b]11;banana\x07" + "y", "xy", themeDark},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := feedPipe(t, tc.feed)
			kind, rest := probeTheme(&bytes.Buffer{}, in, timeout)
			if kind != tc.kind {
				t.Errorf("theme = %s, want %s", kind, tc.kind)
			}
			if string(rest) != tc.want {
				t.Errorf("probe returned %q for the input loop, want %q\n"+
					"bytes the probe read that were not the reply are keystrokes; "+
					"losing them means magmux eats input at startup",
					string(rest), tc.want)
			}
		})
	}

	// Split across reads: the keystroke lands after the query has gone out but
	// before the terminal answers, which is exactly the real interleaving.
	t.Run("split across reads", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		defer r.Close()
		defer w.Close()
		go func() {
			w.Write([]byte("q"))
			time.Sleep(10 * time.Millisecond)
			w.Write([]byte(reply[:6]))
			time.Sleep(10 * time.Millisecond)
			w.Write([]byte(reply[6:]))
			w.Write([]byte("Z"))
		}()
		kind, rest := probeTheme(&bytes.Buffer{}, r, timeout)
		if kind != themeDark {
			t.Errorf("theme = %s, want dark", kind)
		}
		if string(rest) != "q" && string(rest) != "qZ" {
			// "Z" may or may not have arrived in the same read as the
			// terminator; either way "q" must survive and must come first.
			t.Errorf("probe returned %q, want the keystrokes in order", string(rest))
		}
	})

	// A truncated reply is the one thing that is NOT handed back: it starts
	// with ESC, and an ESC replayed into a finished grid is the quit key.
	t.Run("truncated reply is dropped, keystrokes are not", func(t *testing.T) {
		in := feedPipe(t, "q\x1b]11;rgb:1e1e/1e")
		_, rest := probeTheme(&bytes.Buffer{}, in, timeout)
		if string(rest) != "q" {
			t.Errorf("probe returned %q, want just the keystroke %q", string(rest), "q")
		}
	})
}

// TestThemeOverrideSkipsProbe pins the escape hatch. An explicit setting exists
// for terminals that answer OSC 11 wrongly, so it must not ask them: nothing
// written, nothing read.
func TestThemeOverrideSkipsProbe(t *testing.T) {
	probed := 0
	probe := func() themeProbeResult {
		probed++
		return themeProbeResult{kind: themeDark, ok: true}
	}

	t.Run("env light", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "light")
		probed = 0
		res := resolveTheme(themeInputs{env: os.Getenv("MAGMUX_THEME")}, probe)
		if res.kind != themeLight {
			t.Errorf("MAGMUX_THEME=light gave %s", res.kind)
		}
		if probed != 0 {
			t.Errorf("probed the terminal %d times despite an explicit setting", probed)
		}
		if len(res.leftover) != 0 {
			t.Errorf("an unprobed terminal produced %q of leftover input", res.leftover)
		}
	})

	t.Run("env dark", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "dark")
		probed = 0
		if res := resolveTheme(themeInputs{env: os.Getenv("MAGMUX_THEME")}, probe); res.kind != themeDark {
			t.Errorf("MAGMUX_THEME=dark gave %s", res.kind)
		}
		if probed != 0 {
			t.Errorf("probed the terminal %d times despite an explicit setting", probed)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "dark")
		probed = 0
		if res := resolveTheme(themeInputs{flag: "light", env: os.Getenv("MAGMUX_THEME")}, probe); res.kind != themeLight {
			t.Error("--theme light did not override MAGMUX_THEME=dark")
		}
		if probed != 0 {
			t.Errorf("probed the terminal %d times despite an explicit setting", probed)
		}
	})

	// Five calls in four subtests: two of them here. No termTheme and no
	// colorFGBG in the inputs, so the developer's shell cannot leak in.
	t.Run("auto probes, and a typo falls back to auto", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "")
		probed = 0
		resolveTheme(themeInputs{}, probe)
		resolveTheme(themeInputs{flag: "banana", env: "chartreuse"}, probe)
		if probed != 2 {
			t.Errorf("auto probed %d times, want 2", probed)
		}
		if validThemeSetting("banana") {
			t.Error("banana is not a theme")
		}
	})

	// themeWord is the one normaliser. Only light and dark are answers; auto,
	// empty and garbage are all the same "no opinion".
	t.Run("words normalise", func(t *testing.T) {
		for in, want := range map[string]struct {
			kind themeKind
			ok   bool
		}{
			"":       {themeDark, false},
			"LIGHT":  {themeLight, true},
			" dark ": {themeDark, true},
			"auto":   {themeDark, false},
			"nope":   {themeDark, false},
		} {
			got, ok := themeWord(in)
			if ok != want.ok || (ok && got != want.kind) {
				t.Errorf("themeWord(%q) = (%s, %v), want (%s, %v)", in, got, ok, want.kind, want.ok)
			}
		}
	})
}

// TestResolveThemeOrder is the whole order as one table:
//
//	--theme > MAGMUX_THEME > TERM_THEME > OSC 11 probe > COLORFGBG > dark
//
// Pure — no environment, no tty. Each row says who answered, not just what.
func TestResolveThemeOrder(t *testing.T) {
	latte := rgb{0xEF, 0xF1, 0xF5}
	mocha := rgb{0x1E, 0x1E, 0x2E}
	type probeStub int
	const (
		probeNone   probeStub = iota // nil: cannot ask
		probeLight                   // answers light
		probeDark                    // answers dark
		probeSilent                  // ran, no answer, but read a keystroke
	)
	cases := []struct {
		name           string
		flag, env      string
		termTheme      string
		probe          probeStub
		colorFGBG      string
		wantKind       themeKind
		wantSource     themeSource
		wantProbeCalls int
	}{
		{"flag beats all", "light", "dark", "dark", probeLight, "15;0", themeLight, themeSourceFlag, 0},
		{"flag auto is no opinion", "auto", "dark", "light", probeLight, "", themeDark, themeSourceEnv, 0},
		{"flag garbage falls to env", "banana", "light", "", probeNone, "", themeLight, themeSourceEnv, 0},
		{"env auto falls to TERM_THEME", "", "auto", "light", probeDark, "15;0", themeLight, themeSourceTermTheme, 0},
		{"TERM_THEME is case-insensitive and skips the probe", "", "", "DARK", probeLight, "0;15", themeDark, themeSourceTermTheme, 0},
		{"TERM_THEME is trimmed", "", "", " Light\t", probeDark, "", themeLight, themeSourceTermTheme, 0},
		{"TERM_THEME auto probes", "", "", "auto", probeLight, "", themeLight, themeSourceProbe, 1},
		{"TERM_THEME garbage probes, probe beats COLORFGBG", "", "", "system", probeDark, "0;15", themeDark, themeSourceProbe, 1},
		{"silent probe falls to COLORFGBG", "", "", "", probeSilent, "0;15", themeLight, themeSourceColorFGBG, 1},
		{"nil probe falls to COLORFGBG light", "", "", "", probeNone, "0;15", themeLight, themeSourceColorFGBG, 0},
		{"nil probe falls to COLORFGBG dark", "", "", "", probeNone, "15;0", themeDark, themeSourceColorFGBG, 0},
		{"silent probe, garbage COLORFGBG, default", "", "", "", probeSilent, "garbage", themeDark, themeSourceDefault, 1},
		{"nothing at all", "", "", "", probeNone, "", themeDark, themeSourceDefault, 0},
		{"auto at all three word levels falls to COLORFGBG", "AUTO", "AUTO", "AUTO", probeNone, "0;15", themeLight, themeSourceColorFGBG, 0},
		{"answered probe beats a contradicting COLORFGBG", "", "", "", probeLight, "15;0", themeLight, themeSourceProbe, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			var probe func() themeProbeResult
			switch c.probe {
			case probeLight:
				probe = func() themeProbeResult {
					calls++
					return themeProbeResult{kind: themeLight, color: latte, ok: true}
				}
			case probeDark:
				probe = func() themeProbeResult {
					calls++
					return themeProbeResult{kind: themeDark, color: mocha, ok: true}
				}
			case probeSilent:
				probe = func() themeProbeResult {
					calls++
					return themeProbeResult{ok: false, leftover: []byte("q")}
				}
			}
			in := themeInputs{flag: c.flag, env: c.env, termTheme: c.termTheme, colorFGBG: c.colorFGBG}
			res := resolveTheme(in, probe)
			if res.kind != c.wantKind || res.source != c.wantSource {
				t.Errorf("got %s via %s, want %s via %s", res.kind, res.source, c.wantKind, c.wantSource)
			}
			if calls != c.wantProbeCalls {
				t.Errorf("probe called %d times, want %d", calls, c.wantProbeCalls)
			}
			if res.probeRan != (calls > 0) {
				t.Errorf("probeRan=%v with %d calls", res.probeRan, calls)
			}
			if res.probedOK != (res.source == themeSourceProbe) {
				t.Errorf("probedOK=%v but source=%s", res.probedOK, res.source)
			}
			if res.probedOK {
				want := latte
				if c.probe == probeDark {
					want = mocha
				}
				if res.probed != want {
					t.Errorf("probed colour %v, want %v", res.probed, want)
				}
			}
			switch {
			case c.probe == probeSilent && calls > 0:
				// Keystrokes survive a probe that fell through.
				if string(res.leftover) != "q" {
					t.Errorf("leftover %q, want the keystroke %q", res.leftover, "q")
				}
			case calls == 0:
				if res.leftover != nil {
					t.Errorf("leftover %q from a probe that never ran", res.leftover)
				}
			}
		})
	}
}

// TestClassifyColorFGBG: rxvt/konsole's "fg;bg" or "fg;x;bg". The last field is
// the background; 0-6 and 8 are dark, 7 and 9-15 light; everything else is no
// opinion.
func TestClassifyColorFGBG(t *testing.T) {
	cases := []struct {
		in   string
		kind themeKind
		ok   bool
	}{
		{"15;0", themeDark, true},
		{"0;15", themeLight, true},
		{"7;0", themeDark, true},
		{"0;7", themeLight, true},
		{"0;8", themeDark, true},
		{"0;9", themeLight, true},
		{"0;6", themeDark, true},
		{"0;7;15", themeLight, true},
		{"15;default;8", themeDark, true},
		{"15;default;0", themeDark, true},
		{"0;default;15", themeLight, true},
		{" 0;15 ", themeLight, true},
		{"0; 15", themeLight, true},
		{"default;default", themeDark, false},
		{"0;16", themeDark, false},
		{"0;255", themeDark, false},
		{"0;-1", themeDark, false},
		{"0", themeDark, false},
		{"0;1;2;3", themeDark, false},
		{"", themeDark, false},
		{";", themeDark, false},
		{"0;", themeDark, false},
		{"garbage", themeDark, false},
	}
	for _, c := range cases {
		got, ok := classifyColorFGBG(c.in)
		if ok != c.ok || (ok && got != c.kind) {
			t.Errorf("classifyColorFGBG(%q) = (%s, %v), want (%s, %v)", c.in, got, ok, c.kind, c.ok)
		}
	}
}

// TestTermThemeSkipsProbe proves FR1 at the byte level: with TERM_THEME set,
// the probe closure is never invoked, so nothing is written to the terminal
// and the reply a would-be terminal had ready is still sitting in the pipe.
func TestTermThemeSkipsProbe(t *testing.T) {
	t.Setenv("TERM_THEME", "dark")
	const latteReply = "\x1b]11;rgb:efef/f1f1/f5f5\x1b\\"
	in := feedPipe(t, latteReply)
	var out bytes.Buffer
	probe := func() themeProbeResult {
		k, c, ok, rest := probeThemeColor(&out, in, 150*time.Millisecond)
		return themeProbeResult{kind: k, color: c, ok: ok, leftover: rest}
	}
	res := resolveTheme(themeInputs{termTheme: os.Getenv("TERM_THEME")}, probe)
	if res.kind != themeDark || res.source != themeSourceTermTheme {
		t.Fatalf("got %s via %s, want dark via TERM_THEME", res.kind, res.source)
	}
	if out.Len() != 0 {
		t.Errorf("TERM_THEME set, yet %d bytes were written to the terminal: %q", out.Len(), out.String())
	}
	if res.leftover != nil {
		t.Errorf("leftover %q from a probe that must not have run", res.leftover)
	}
	if res.probeRan {
		t.Error("probeRan is set")
	}
	// The reply is unread: the pipe is readable and gives back the Latte reply
	// intact, which it could not if the probe had consumed it.
	ready, err := waitReadable(int(in.Fd()), time.Now().Add(50*time.Millisecond))
	if err != nil || !ready {
		t.Fatalf("the terminal's reply is no longer in the pipe (ready=%v err=%v)", ready, err)
	}
	buf := make([]byte, 64)
	n, _ := in.Read(buf)
	if string(buf[:n]) != latteReply {
		t.Errorf("pipe held %q, want the untouched reply %q", buf[:n], latteReply)
	}
}

// ── contrast ─────────────────────────────────────────────────────────────────

// wcagLuminance is the WCAG 2.x relative luminance: sRGB linearised, then
// weighted. Distinct from screenLuminance, which classifies a background and
// deliberately does not linearise — see the comment there.
func wcagLuminance(c rgb) float64 {
	lin := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

// contrastRatio is WCAG's (L1+0.05)/(L2+0.05), lighter over darker.
func contrastRatio(a, b rgb) float64 {
	la, lb := wcagLuminance(a), wcagLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// TestPaletteContrast is the test that would have caught the reported bug.
//
// Every foreground has to be legible on the background it is actually drawn
// on, and since magmux stopped painting a background of its own that is TWO
// backgrounds, not one:
//
//   - assumedBack — the terminal this palette is for. magmux never paints it;
//     it is where nearly every glyph magmux writes actually lands, so it is the
//     yardstick. A light palette is used on a light terminal, so that is what
//     its foregrounds are measured against.
//   - bar — the status bar's background, which magmux does paint, and where
//     the same foregrounds land instead.
//
// Body text and data carry the WCAG 4.5:1 bar; chrome — rules, labels,
// timestamps — is held to 3:1, which is low enough to still recede and high
// enough to exist. `ink` is measured against every colour a badge is filled
// with, because that is the only place it appears — and a badge's fill is one
// of the two backgrounds magmux legitimately chooses.
func TestPaletteContrast(t *testing.T) {
	const (
		bodyMin   = 4.5
		chromeMin = 3.0
	)

	for _, tc := range []struct {
		name string
		p    palette
	}{{"dark", darkPalette}, {"light", lightPalette}} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			body := map[string]rgb{
				"text":    p.text,
				"success": p.success,
				"running": p.running,
				"warn":    p.warn,
				"fail":    p.fail,
				"accent":  p.accent,
			}
			chrome := map[string]rgb{
				"subtle": p.subtle,
				"debug":  p.debug,
				"dead":   p.dead,
				"border": p.border,
			}
			backs := map[string]rgb{
				"the terminal background this palette assumes": p.assumedBack,
				"the status bar's own background":              p.bar,
			}
			for where, back := range backs {
				for name, c := range body {
					if got := contrastRatio(c, back); got < bodyMin {
						t.Errorf("%s on %s is %.2f:1, want >= %.1f:1",
							name, where, got, bodyMin)
					}
				}
				for name, c := range chrome {
					if got := contrastRatio(c, back); got < chromeMin {
						t.Errorf("%s on %s is %.2f:1, want >= %.1f:1",
							name, where, got, chromeMin)
					}
				}
			}
			// badge() and the status bar's pills fill with a state colour and
			// write ink on top; every one of those fills is a background for
			// ink, and they are the ONLY backgrounds magmux imposes.
			for name, c := range map[string]rgb{
				"success": p.success, "running": p.running, "warn": p.warn,
				"fail": p.fail, "accent": p.accent, "subtle": p.subtle, "dead": p.dead,
			} {
				if got := contrastRatio(p.ink, c); got < bodyMin {
					t.Errorf("badge ink on %s is %.2f:1, want >= %.1f:1", name, got, bodyMin)
				}
			}
			// The hierarchy the panel is designed around: chrome recedes
			// relative to body text, and rules recede relative to labels.
			if contrastRatio(p.border, p.assumedBack) > contrastRatio(p.subtle, p.assumedBack) {
				t.Error("rules are louder than labels; the chrome hierarchy is inverted")
			}
			if contrastRatio(p.subtle, p.assumedBack) > contrastRatio(p.text, p.assumedBack) {
				t.Error("labels are louder than body text; the hierarchy is inverted")
			}
			// The bar is the one surface magmux fills, so it has to belong to
			// the theme it is filled for — not merely be legible. A dark slab
			// on a light terminal is the bug this replaced.
			if (screenLuminance(p.bar) >= lightThreshold) !=
				(screenLuminance(p.assumedBack) >= lightThreshold) {
				t.Errorf("the %s theme's status bar is on the wrong side of the "+
					"light/dark line from the terminal it is drawn in", tc.name)
			}
		})
	}

	// And the bug itself, stated as an assertion: the palette magmux always
	// shipped is unreadable on a light terminal. This is why there are two.
	if got := contrastRatio(darkPalette.text, lightPalette.assumedBack); got >= 4.5 {
		t.Errorf("dark body text on a light background is %.2f:1 — if that is now "+
			"legible, the palettes have drifted and this test has stopped meaning anything", got)
	}
	if got := contrastRatio(lightPalette.text, darkPalette.assumedBack); got >= 4.5 {
		t.Errorf("light body text on a dark background is %.2f:1 — the two palettes "+
			"are supposed to be non-interchangeable in both directions", got)
	}
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

	for _, kind := range []themeKind{themeDark, themeLight} {
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
	defer useTheme(themeLight)()

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
	for _, kind := range []themeKind{themeDark, themeLight} {
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
				want rgb
				why  string
			}{
				{"", "", pal.border, "untinted panes get the palette's rule colour"},
				{"green", "", pal.success, "a finished pane"},
				{"", "green", pal.success, "…on either side of the split"},
				{"red", "", pal.fail, "a failed pane"},
				{"yellow", "", pal.warn, "a pane blocked on a permission prompt"},
				{"green", "red", pal.fail, "the loudest tint wins, whichever side it is on"},
				{"red", "green", pal.fail, "…and the old code took child1's, hiding it"},
				{"green", "yellow", pal.warn, "amber outranks green"},
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
				pal.success.r, pal.success.g, pal.success.b)) {
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

	render := func(kind themeKind) string {
		defer useTheme(kind)()
		var r Renderer
		r.reset()
		r.renderStatusBar(0, 60, text)
		return r.frame()
	}

	dark, light := render(themeDark), render(themeLight)

	if dark == light {
		t.Error("the status bar is byte-identical in both themes; it is not reading the palette")
	}

	for _, tc := range []struct {
		name  string
		frame string
		p     palette
	}{{"dark", dark, darkPalette}, {"light", light, lightPalette}} {
		t.Run(tc.name, func(t *testing.T) {
			must := map[string]string{
				"the bar's own background": bg(tc.p.bar),
				"the accent label":         fg(tc.p.accent),
				"the success pill":         bg(tc.p.success),
				"the warning colour":       fg(tc.p.warn),
				"the help text":            fg(tc.p.subtle),
				"the divider rule":         fg(tc.p.border),
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
func xterm256(i int) (rgb, bool) {
	switch {
	case i < 0 || i > 255:
		return rgb{}, false
	case i < 16:
		sys := []rgb{
			{0, 0, 0}, {128, 0, 0}, {0, 128, 0}, {128, 128, 0},
			{0, 0, 128}, {128, 0, 128}, {0, 128, 128}, {192, 192, 192},
			{128, 128, 128}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0},
			{0, 0, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
		}
		return sys[i], true
	case i < 232:
		lv := []uint8{0, 95, 135, 175, 215, 255}
		i -= 16
		return rgb{lv[i/36], lv[(i/6)%6], lv[i%6]}, true
	default:
		v := uint8(8 + (i-232)*10)
		return rgb{v, v, v}, true
	}
}

// cellRGB resolves half a rendered cell to a colour, reporting whether the cell
// set that half AT ALL. A cell that did not is showing whatever was in force
// underneath it — which, for an overlay, is the child's output.
func cellRGB(c Color) (rgb, bool) {
	if c.True {
		return rgb{c.R, c.G, c.B}, true
	}
	if c.Index >= 0 {
		return xterm256(int(c.Index))
	}
	return rgb{}, false
}

// The child's own colours, chosen to be in neither palette so that a cell
// wearing them can only have inherited them.
var (
	childFg = rgb{0xFF, 0x00, 0xFF}
	childBg = rgb{0x00, 0x80, 0x80}
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
		p.vt.write([]byte(fg(childFg) + bg(childBg) + strings.Repeat("x", h*w-1)))
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

	for _, kind := range []themeKind{themeDark, themeLight} {
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

						darkestBg := rgb{0xFF, 0xFF, 0xFF}
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
								if got := contrastRatio(b, pal.assumedBack); got > surfaceMax {
									t.Errorf("the overlay paints %+v, which is %.2f:1 against "+
										"this theme's terminal background — a slab, not a "+
										"surface belonging to it", b, got)
								}
								if screenLuminance(b) < screenLuminance(darkestBg) {
									darkestBg = b
								}
							}
						}
						if box.isBox &&
							screenLuminance(darkestBg) >= screenLuminance(pal.assumedBack) {
							// The darkest thing the box paints is its shadow.
							t.Errorf("the drop shadow (%+v, luminance %.3f) is not darker than "+
								"the terminal it falls on (%+v, %.3f) — that is a highlight",
								darkestBg, screenLuminance(darkestBg),
								pal.assumedBack, screenLuminance(pal.assumedBack))
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
	render := func(kind themeKind, style string) string {
		defer useTheme(kind)()
		p := newControlPane(0, 0, 12, 40, "leaf")
		p.overlayText = "✓ DONE\ntook 30.2s\n42 tests passed"
		p.overlayStyle = style
		return renderLeaf(p)
	}

	for _, style := range []string{"success", "error", "info", ""} {
		dark, light := render(themeDark, style), render(themeLight, style)
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

// ── resolution order: the gaps the table does not state on its own ───────────

// TestThemeWord is the section-3.1 normalisation table for the three
// word-valued inputs. Only light and dark are answers; auto, empty and garbage
// are all one "no opinion". Without the feature, auto came back ok==true as a
// third value (the old themeSetting), and this reports themeWord("auto") =
// (_, true).
func TestThemeWord(t *testing.T) {
	cases := []struct {
		in   string
		kind themeKind
		ok   bool
	}{
		{"light", themeLight, true},
		{"Light", themeLight, true},
		{" LIGHT ", themeLight, true},
		{"dark", themeDark, true},
		{"DARK", themeDark, true},
		{"dark\t", themeDark, true},
		{"auto", themeDark, false},
		{"AUTO", themeDark, false},
		{"", themeDark, false},
		{"system", themeDark, false},
		{"banana", themeDark, false},
		{"1", themeDark, false},
		{"lightish", themeDark, false},
		{"light dark", themeDark, false},
	}
	for _, c := range cases {
		got, ok := themeWord(c.in)
		if ok != c.ok || (ok && got != c.kind) {
			t.Errorf("themeWord(%q) = (%s, %v), want (%s, %v)", c.in, got, ok, c.kind, c.ok)
		}
	}
}

// TestResolveThemeNeverCallsProbeWhenAWordAnswered is FR1's "the probe must
// not run" stated independently of the order table: for each word level a
// probe that fails the test outright. Without the TERM_THEME step the third
// case reports "probe invoked although TERM_THEME=dark answered".
func TestResolveThemeNeverCallsProbeWhenAWordAnswered(t *testing.T) {
	cases := []struct {
		name string
		in   themeInputs
		want themeSource
		kind themeKind
	}{
		{"--theme dark", themeInputs{flag: "dark"}, themeSourceFlag, themeDark},
		{"MAGMUX_THEME=light", themeInputs{env: "light"}, themeSourceEnv, themeLight},
		{"TERM_THEME=dark", themeInputs{termTheme: "dark"}, themeSourceTermTheme, themeDark},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probe := func() themeProbeResult {
				t.Errorf("probe invoked although %s answered", c.name)
				return themeProbeResult{kind: themeLight, ok: true, leftover: []byte("q")}
			}
			res := resolveTheme(c.in, probe)
			if res.kind != c.kind || res.source != c.want {
				t.Errorf("got %s via %s, want %s via %s", res.kind, res.source, c.kind, c.want)
			}
			if res.probeRan {
				t.Error("probeRan is set")
			}
			if res.probedOK {
				t.Error("probedOK is set for a word-sourced resolution")
			}
			if res.leftover != nil {
				t.Errorf("leftover %q from a probe that must not have run", res.leftover)
			}
		})
	}
}

// TestResolveThemeDefaultSourceIsZeroValue pins the vocabulary the debug line
// is built from, and that a zero themeResolution reads as "dark, nobody
// answered" — which is what every &Magmux{} struct literal in this package
// silently relies on. Without the feature the constants do not exist or the
// strings differ, and the debug-line test could not name a source.
func TestResolveThemeDefaultSourceIsZeroValue(t *testing.T) {
	var zero themeResolution
	if zero.source != themeSourceDefault {
		t.Errorf("zero themeResolution has source %s, want default", zero.source)
	}
	if zero.kind != themeDark {
		t.Errorf("zero themeResolution has kind %s, want dark", zero.kind)
	}
	for src, want := range map[themeSource]string{
		themeSourceDefault:   "default",
		themeSourceFlag:      "--theme",
		themeSourceEnv:       "MAGMUX_THEME",
		themeSourceTermTheme: "TERM_THEME",
		themeSourceProbe:     "OSC 11",
		themeSourceColorFGBG: "COLORFGBG",
	} {
		if got := src.String(); got != want {
			t.Errorf("themeSource(%d).String() = %q, want %q", int(src), got, want)
		}
	}
}

// TestColorFGBGConsultedOnlyWhenProbeCannotAnswer is the placement rule of
// section 3.2 on its own: a measured colour beats an index, and the index is
// read only when the probe was skipped or came back empty. Without the
// feature (b) and (c) report "dark via default" — an unanswered probe used to
// be dark unconditionally — and (a) reports light via COLORFGBG if the index
// were placed before the probe.
func TestColorFGBGConsultedOnlyWhenProbeCannotAnswer(t *testing.T) {
	mocha := rgb{0x2B, 0x30, 0x3B}
	in := themeInputs{colorFGBG: "0;15"} // says light

	t.Run("answered probe wins over COLORFGBG", func(t *testing.T) {
		calls := 0
		res := resolveTheme(in, func() themeProbeResult {
			calls++
			return themeProbeResult{kind: themeDark, color: mocha, ok: true}
		})
		if res.kind != themeDark || res.source != themeSourceProbe {
			t.Errorf("got %s via %s, want dark via OSC 11", res.kind, res.source)
		}
		if calls != 1 || !res.probeRan || !res.probedOK || res.probed != mocha {
			t.Errorf("calls=%d probeRan=%v probedOK=%v probed=%v", calls, res.probeRan, res.probedOK, res.probed)
		}
	})

	t.Run("silent probe falls to COLORFGBG", func(t *testing.T) {
		calls := 0
		res := resolveTheme(in, func() themeProbeResult {
			calls++
			return themeProbeResult{ok: false, leftover: []byte("q")}
		})
		if res.kind != themeLight || res.source != themeSourceColorFGBG {
			t.Errorf("got %s via %s, want light via COLORFGBG", res.kind, res.source)
		}
		if calls != 1 || !res.probeRan {
			t.Errorf("calls=%d probeRan=%v, want the probe to have run once", calls, res.probeRan)
		}
		if res.probedOK {
			t.Error("probedOK set for a probe that did not answer")
		}
		if string(res.leftover) != "q" {
			t.Errorf("leftover %q, want the keystroke %q", res.leftover, "q")
		}
	})

	t.Run("nil probe falls to COLORFGBG", func(t *testing.T) {
		res := resolveTheme(in, nil)
		if res.kind != themeLight || res.source != themeSourceColorFGBG {
			t.Errorf("got %s via %s, want light via COLORFGBG", res.kind, res.source)
		}
		if res.probeRan || res.probedOK {
			t.Errorf("probeRan=%v probedOK=%v with a nil probe", res.probeRan, res.probedOK)
		}
		if res.leftover != nil {
			t.Errorf("leftover %q with a nil probe", res.leftover)
		}
	})
}

// ── debug line ───────────────────────────────────────────────────────────────

// themeDebugLine assembles the line initTheme logs, from the same pieces
// (section 4.3): "theme: <kind> via <source> (probe <state>; N bytes of
// input preserved; background rgb:...)". The pieces are what the test owns;
// the fully assembled line is also captured from initTheme through dbgFile
// in TestThemeDebugLineNamesSource's last subtest.
func themeDebugLine(res themeResolution, back rgb) string {
	return fmt.Sprintf("theme: %s via %s (probe %s; %d bytes of input preserved; background %s)",
		res.kind, res.source, probeState(res), len(res.leftover), xColorString(back))
}

// TestThemeDebugLineNamesSource: the line names who decided, and never says
// the theme was not detected — "probe skipped" is a fact about the probe, not
// about the theme. Without the feature there is no source in the line, or a
// TERM_THEME run reads as not detected, and the must-not-contain assertions
// report it.
func TestThemeDebugLineNamesSource(t *testing.T) {
	latte := rgb{0xEF, 0xF1, 0xF5}
	cases := []struct {
		name     string
		res      themeResolution
		contains []string
	}{
		{"dark via TERM_THEME, probe skipped",
			themeResolution{kind: themeDark, source: themeSourceTermTheme},
			[]string{"dark via TERM_THEME", "probe skipped", "0 bytes of input preserved"}},
		{"light via OSC 11, probe answered, one keystroke kept",
			themeResolution{kind: themeLight, source: themeSourceProbe, probed: latte, probedOK: true, probeRan: true, leftover: []byte("q")},
			[]string{"light via OSC 11", "probe answered", "1 bytes of input preserved"}},
		{"light via COLORFGBG, probe ran and did not answer",
			themeResolution{kind: themeLight, source: themeSourceColorFGBG, probeRan: true},
			[]string{"light via COLORFGBG", "probe no answer"}},
		{"dark via default, probe skipped",
			themeResolution{kind: themeDark, source: themeSourceDefault},
			[]string{"dark via default", "probe skipped"}},
		{"dark via default, probe ran",
			themeResolution{kind: themeDark, source: themeSourceDefault, probeRan: true},
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
		defer useTheme(currentTheme)()

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
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob *.go: %v (%d files)", err, len(files))
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
// The files are read as TEXT for a grep, nothing more. Reports the offending
// line if a .env loader or a file read ever lands on the theme path.
func TestThemeEnvReadsNoFile(t *testing.T) {
	stripComment := func(l string) string {
		if i := strings.Index(l, "//"); i >= 0 {
			return l[:i]
		}
		return l
	}
	for _, f := range []string{"theme.go", "main.go"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(data), "\n") {
			code := stripComment(l)
			// A ".env" FILE: quoted or path-joined. (A field named env, as in
			// in.env, is not a file.)
			for _, needle := range []string{`".env`, `/.env`, `.env"`} {
				if strings.Contains(code, needle) {
					t.Errorf("%s:%d names a .env file outside a comment: %q", f, i+1, l)
				}
			}
			if f != "theme.go" {
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
// the raw process environment (untrimmed — normalisation is themeWord's job),
// and initTheme reads through the seam and nowhere else. Without the seam this
// does not compile; with an initTheme that calls os.Getenv directly, the
// second half reports "dark from the process env" although the seam said light.
func TestThemeEnvSeamReadsProcessEnv(t *testing.T) {
	t.Setenv("MAGMUX_THEME", "auto")
	t.Setenv("TERM_THEME", " Dark ")
	t.Setenv("COLORFGBG", "0;15")

	got := themeEnv()
	want := themeInputs{env: "auto", termTheme: " Dark ", colorFGBG: "0;15"}
	if got != want {
		t.Errorf("themeEnv() = %+v, want %+v", got, want)
	}

	old := themeEnv
	themeEnv = func() themeInputs { return themeInputs{termTheme: "light"} }
	defer func() { themeEnv = old }()
	defer useTheme(currentTheme)()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	m := &Magmux{stdin: r, headless: true}
	m.initTheme(int(r.Fd()))
	if currentTheme != themeLight {
		t.Errorf("theme is %s: initTheme read the process env (TERM_THEME=%q) instead of the seam", currentTheme, os.Getenv("TERM_THEME"))
	}
}
