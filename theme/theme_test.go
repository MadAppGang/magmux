package theme

import (
	"bytes"
	"math"
	"os"
	"testing"
	"time"
)

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
		want Kind
	}{
		{"mocha, BEL terminated", "\x1b]11;rgb:1e1e/1e1e/2e2e\x07", Dark},
		{"latte, ST terminated", "\x1b]11;rgb:efef/f1f1/f5f5\x1b\\", Light},
		{"two hex digits, white", "\x1b]11;rgb:ff/ff/ff\x07", Light},
		{"two hex digits, black", "\x1b]11;rgb:00/00/00\x07", Dark},
		{"one hex digit, white", "\x1b]11;rgb:f/f/f\x07", Light},
		{"one hex digit, black", "\x1b]11;rgb:0/0/0\x07", Dark},
		{"three hex digits, solarized light", "\x1b]11;rgb:fdd/f66/e33\x1b\\", Light},
		{"rgba, alpha ignored", "\x1b]11;rgba:ffff/ffff/ffff/ffff\x07", Light},
		{"unparseable body", "\x1b]11;banana\x07", Dark},
		{"not a reply at all", "hello", Dark},
		{"no reply", "", Dark},
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
		kind Kind
	}{
		{"keystroke before the reply", "q" + reply, "q", Dark},
		{"keystroke after the reply", reply + "\x1b[A", "\x1b[A", Dark},
		{"keystrokes either side", "ab" + reply + "cd", "abcd", Dark},
		{"a whole chord around a light reply",
			"\x07q" + "\x1b]11;rgb:efef/f1f1/f5f5\x1b\\" + "\x1b[B",
			"\x07q\x1b[B", Light},
		{"no reply, only keystrokes", "hello world", "hello world", Dark},
		{"reply we cannot parse still yields the keys", "x" + "\x1b]11;banana\x07" + "y", "xy", Dark},
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
		if kind != Dark {
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
	probe := func() ProbeResult {
		probed++
		return ProbeResult{Kind: Dark, OK: true}
	}

	t.Run("env light", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "light")
		probed = 0
		res := Resolve(Inputs{Env: os.Getenv("MAGMUX_THEME")}, probe)
		if res.Kind != Light {
			t.Errorf("MAGMUX_THEME=light gave %s", res.Kind)
		}
		if probed != 0 {
			t.Errorf("probed the terminal %d times despite an explicit setting", probed)
		}
		if len(res.Leftover) != 0 {
			t.Errorf("an unprobed terminal produced %q of leftover input", res.Leftover)
		}
	})

	t.Run("env dark", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "dark")
		probed = 0
		if res := Resolve(Inputs{Env: os.Getenv("MAGMUX_THEME")}, probe); res.Kind != Dark {
			t.Errorf("MAGMUX_THEME=dark gave %s", res.Kind)
		}
		if probed != 0 {
			t.Errorf("probed the terminal %d times despite an explicit setting", probed)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("MAGMUX_THEME", "dark")
		probed = 0
		if res := Resolve(Inputs{Flag: "light", Env: os.Getenv("MAGMUX_THEME")}, probe); res.Kind != Light {
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
		Resolve(Inputs{}, probe)
		Resolve(Inputs{Flag: "banana", Env: "chartreuse"}, probe)
		if probed != 2 {
			t.Errorf("auto probed %d times, want 2", probed)
		}
		if ValidSetting("banana") {
			t.Error("banana is not a theme")
		}
	})

	// Word is the one normaliser. Only light and dark are answers; auto,
	// empty and garbage are all the same "no opinion".
	t.Run("words normalise", func(t *testing.T) {
		for in, want := range map[string]struct {
			kind Kind
			ok   bool
		}{
			"":       {Dark, false},
			"LIGHT":  {Light, true},
			" dark ": {Dark, true},
			"auto":   {Dark, false},
			"nope":   {Dark, false},
		} {
			got, ok := Word(in)
			if ok != want.ok || (ok && got != want.kind) {
				t.Errorf("Word(%q) = (%s, %v), want (%s, %v)", in, got, ok, want.kind, want.ok)
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
	latte := RGB{R: 0xEF, G: 0xF1, B: 0xF5}
	mocha := RGB{R: 0x1E, G: 0x1E, B: 0x2E}
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
		wantKind       Kind
		wantSource     Source
		wantProbeCalls int
	}{
		{"flag beats all", "light", "dark", "dark", probeLight, "15;0", Light, SourceFlag, 0},
		{"flag auto is no opinion", "auto", "dark", "light", probeLight, "", Dark, SourceEnv, 0},
		{"flag garbage falls to env", "banana", "light", "", probeNone, "", Light, SourceEnv, 0},
		{"env auto falls to TERM_THEME", "", "auto", "light", probeDark, "15;0", Light, SourceTermTheme, 0},
		{"TERM_THEME is case-insensitive and skips the probe", "", "", "DARK", probeLight, "0;15", Dark, SourceTermTheme, 0},
		{"TERM_THEME is trimmed", "", "", " Light\t", probeDark, "", Light, SourceTermTheme, 0},
		{"TERM_THEME auto probes", "", "", "auto", probeLight, "", Light, SourceProbe, 1},
		{"TERM_THEME garbage probes, probe beats COLORFGBG", "", "", "system", probeDark, "0;15", Dark, SourceProbe, 1},
		{"silent probe falls to COLORFGBG", "", "", "", probeSilent, "0;15", Light, SourceColorFGBG, 1},
		{"nil probe falls to COLORFGBG light", "", "", "", probeNone, "0;15", Light, SourceColorFGBG, 0},
		{"nil probe falls to COLORFGBG dark", "", "", "", probeNone, "15;0", Dark, SourceColorFGBG, 0},
		{"silent probe, garbage COLORFGBG, default", "", "", "", probeSilent, "garbage", Dark, SourceDefault, 1},
		{"nothing at all", "", "", "", probeNone, "", Dark, SourceDefault, 0},
		{"auto at all three word levels falls to COLORFGBG", "AUTO", "AUTO", "AUTO", probeNone, "0;15", Light, SourceColorFGBG, 0},
		{"answered probe beats a contradicting COLORFGBG", "", "", "", probeLight, "15;0", Light, SourceProbe, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			var probe func() ProbeResult
			switch c.probe {
			case probeLight:
				probe = func() ProbeResult {
					calls++
					return ProbeResult{Kind: Light, Color: latte, OK: true}
				}
			case probeDark:
				probe = func() ProbeResult {
					calls++
					return ProbeResult{Kind: Dark, Color: mocha, OK: true}
				}
			case probeSilent:
				probe = func() ProbeResult {
					calls++
					return ProbeResult{OK: false, Leftover: []byte("q")}
				}
			}
			in := Inputs{Flag: c.flag, Env: c.env, TermTheme: c.termTheme, ColorFGBG: c.colorFGBG}
			res := Resolve(in, probe)
			if res.Kind != c.wantKind || res.Source != c.wantSource {
				t.Errorf("got %s via %s, want %s via %s", res.Kind, res.Source, c.wantKind, c.wantSource)
			}
			if calls != c.wantProbeCalls {
				t.Errorf("probe called %d times, want %d", calls, c.wantProbeCalls)
			}
			if res.ProbeRan != (calls > 0) {
				t.Errorf("probeRan=%v with %d calls", res.ProbeRan, calls)
			}
			if res.ProbedOK != (res.Source == SourceProbe) {
				t.Errorf("probedOK=%v but source=%s", res.ProbedOK, res.Source)
			}
			if res.ProbedOK {
				want := latte
				if c.probe == probeDark {
					want = mocha
				}
				if res.Probed != want {
					t.Errorf("probed colour %v, want %v", res.Probed, want)
				}
			}
			switch {
			case c.probe == probeSilent && calls > 0:
				// Keystrokes survive a probe that fell through.
				if string(res.Leftover) != "q" {
					t.Errorf("leftover %q, want the keystroke %q", res.Leftover, "q")
				}
			case calls == 0:
				if res.Leftover != nil {
					t.Errorf("leftover %q from a probe that never ran", res.Leftover)
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
		kind Kind
		ok   bool
	}{
		{"15;0", Dark, true},
		{"0;15", Light, true},
		{"7;0", Dark, true},
		{"0;7", Light, true},
		{"0;8", Dark, true},
		{"0;9", Light, true},
		{"0;6", Dark, true},
		{"0;7;15", Light, true},
		{"15;default;8", Dark, true},
		{"15;default;0", Dark, true},
		{"0;default;15", Light, true},
		{" 0;15 ", Light, true},
		{"0; 15", Light, true},
		{"default;default", Dark, false},
		{"0;16", Dark, false},
		{"0;255", Dark, false},
		{"0;-1", Dark, false},
		{"0", Dark, false},
		{"0;1;2;3", Dark, false},
		{"", Dark, false},
		{";", Dark, false},
		{"0;", Dark, false},
		{"garbage", Dark, false},
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
	probe := func() ProbeResult {
		k, c, ok, rest := probeThemeColor(&out, in, 150*time.Millisecond)
		return ProbeResult{Kind: k, Color: c, OK: ok, Leftover: rest}
	}
	res := Resolve(Inputs{TermTheme: os.Getenv("TERM_THEME")}, probe)
	if res.Kind != Dark || res.Source != SourceTermTheme {
		t.Fatalf("got %s via %s, want dark via TERM_THEME", res.Kind, res.Source)
	}
	if out.Len() != 0 {
		t.Errorf("TERM_THEME set, yet %d bytes were written to the terminal: %q", out.Len(), out.String())
	}
	if res.Leftover != nil {
		t.Errorf("leftover %q from a probe that must not have run", res.Leftover)
	}
	if res.ProbeRan {
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
// weighted. Distinct from ScreenLuminance, which classifies a background and
// deliberately does not linearise — see the comment there.
func wcagLuminance(c RGB) float64 {
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
func contrastRatio(a, b RGB) float64 {
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
		p    Palette
	}{{"dark", DarkPalette}, {"light", LightPalette}} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			body := map[string]RGB{
				"text":    p.Text,
				"success": p.Success,
				"running": p.Running,
				"warn":    p.Warn,
				"fail":    p.Fail,
				"accent":  p.Accent,
			}
			chrome := map[string]RGB{
				"subtle": p.Subtle,
				"debug":  p.Debug,
				"dead":   p.Dead,
				"border": p.Border,
			}
			backs := map[string]RGB{
				"the terminal background this palette assumes": p.AssumedBack,
				"the status bar's own background":              p.Bar,
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
			for name, c := range map[string]RGB{
				"success": p.Success, "running": p.Running, "warn": p.Warn,
				"fail": p.Fail, "accent": p.Accent, "subtle": p.Subtle, "dead": p.Dead,
			} {
				if got := contrastRatio(p.Ink, c); got < bodyMin {
					t.Errorf("badge ink on %s is %.2f:1, want >= %.1f:1", name, got, bodyMin)
				}
			}
			// The hierarchy the panel is designed around: chrome recedes
			// relative to body text, and rules recede relative to labels.
			if contrastRatio(p.Border, p.AssumedBack) > contrastRatio(p.Subtle, p.AssumedBack) {
				t.Error("rules are louder than labels; the chrome hierarchy is inverted")
			}
			if contrastRatio(p.Subtle, p.AssumedBack) > contrastRatio(p.Text, p.AssumedBack) {
				t.Error("labels are louder than body text; the hierarchy is inverted")
			}
			// The bar is the one surface magmux fills, so it has to belong to
			// the theme it is filled for — not merely be legible. A dark slab
			// on a light terminal is the bug this replaced.
			if (ScreenLuminance(p.Bar) >= lightThreshold) !=
				(ScreenLuminance(p.AssumedBack) >= lightThreshold) {
				t.Errorf("the %s theme's status bar is on the wrong side of the "+
					"light/dark line from the terminal it is drawn in", tc.name)
			}
		})
	}

	// And the bug itself, stated as an assertion: the palette magmux always
	// shipped is unreadable on a light terminal. This is why there are two.
	if got := contrastRatio(DarkPalette.Text, LightPalette.AssumedBack); got >= 4.5 {
		t.Errorf("dark body text on a light background is %.2f:1 — if that is now "+
			"legible, the palettes have drifted and this test has stopped meaning anything", got)
	}
	if got := contrastRatio(LightPalette.Text, DarkPalette.AssumedBack); got >= 4.5 {
		t.Errorf("light body text on a dark background is %.2f:1 — the two palettes "+
			"are supposed to be non-interchangeable in both directions", got)
	}
}

// ── resolution order: the gaps the table does not state on its own ───────────

// TestThemeWord is the section-3.1 normalisation table for the three
// word-valued inputs. Only light and dark are answers; auto, empty and garbage
// are all one "no opinion". Without the feature, auto came back ok==true as a
// third value (the old themeSetting), and this reports Word("auto") =
// (_, true).
func TestThemeWord(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		ok   bool
	}{
		{"light", Light, true},
		{"Light", Light, true},
		{" LIGHT ", Light, true},
		{"dark", Dark, true},
		{"DARK", Dark, true},
		{"dark\t", Dark, true},
		{"auto", Dark, false},
		{"AUTO", Dark, false},
		{"", Dark, false},
		{"system", Dark, false},
		{"banana", Dark, false},
		{"1", Dark, false},
		{"lightish", Dark, false},
		{"light dark", Dark, false},
	}
	for _, c := range cases {
		got, ok := Word(c.in)
		if ok != c.ok || (ok && got != c.kind) {
			t.Errorf("Word(%q) = (%s, %v), want (%s, %v)", c.in, got, ok, c.kind, c.ok)
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
		in   Inputs
		want Source
		kind Kind
	}{
		{"--theme dark", Inputs{Flag: "dark"}, SourceFlag, Dark},
		{"MAGMUX_THEME=light", Inputs{Env: "light"}, SourceEnv, Light},
		{"TERM_THEME=dark", Inputs{TermTheme: "dark"}, SourceTermTheme, Dark},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probe := func() ProbeResult {
				t.Errorf("probe invoked although %s answered", c.name)
				return ProbeResult{Kind: Light, OK: true, Leftover: []byte("q")}
			}
			res := Resolve(c.in, probe)
			if res.Kind != c.kind || res.Source != c.want {
				t.Errorf("got %s via %s, want %s via %s", res.Kind, res.Source, c.kind, c.want)
			}
			if res.ProbeRan {
				t.Error("probeRan is set")
			}
			if res.ProbedOK {
				t.Error("probedOK is set for a word-sourced resolution")
			}
			if res.Leftover != nil {
				t.Errorf("leftover %q from a probe that must not have run", res.Leftover)
			}
		})
	}
}

// TestResolveThemeDefaultSourceIsZeroValue pins the vocabulary the debug line
// is built from, and that a zero Resolution reads as "dark, nobody
// answered" — which is what every &Magmux{} struct literal in this package
// silently relies on. Without the feature the constants do not exist or the
// strings differ, and the debug-line test could not name a source.
func TestResolveThemeDefaultSourceIsZeroValue(t *testing.T) {
	var zero Resolution
	if zero.Source != SourceDefault {
		t.Errorf("zero Resolution has source %s, want default", zero.Source)
	}
	if zero.Kind != Dark {
		t.Errorf("zero Resolution has kind %s, want dark", zero.Kind)
	}
	for src, want := range map[Source]string{
		SourceDefault:   "default",
		SourceFlag:      "--theme",
		SourceEnv:       "MAGMUX_THEME",
		SourceTermTheme: "TERM_THEME",
		SourceProbe:     "OSC 11",
		SourceColorFGBG: "COLORFGBG",
	} {
		if got := src.String(); got != want {
			t.Errorf("Source(%d).String() = %q, want %q", int(src), got, want)
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
	mocha := RGB{R: 0x2B, G: 0x30, B: 0x3B}
	in := Inputs{ColorFGBG: "0;15"} // says light

	t.Run("answered probe wins over COLORFGBG", func(t *testing.T) {
		calls := 0
		res := Resolve(in, func() ProbeResult {
			calls++
			return ProbeResult{Kind: Dark, Color: mocha, OK: true}
		})
		if res.Kind != Dark || res.Source != SourceProbe {
			t.Errorf("got %s via %s, want dark via OSC 11", res.Kind, res.Source)
		}
		if calls != 1 || !res.ProbeRan || !res.ProbedOK || res.Probed != mocha {
			t.Errorf("calls=%d probeRan=%v probedOK=%v probed=%v", calls, res.ProbeRan, res.ProbedOK, res.Probed)
		}
	})

	t.Run("silent probe falls to COLORFGBG", func(t *testing.T) {
		calls := 0
		res := Resolve(in, func() ProbeResult {
			calls++
			return ProbeResult{OK: false, Leftover: []byte("q")}
		})
		if res.Kind != Light || res.Source != SourceColorFGBG {
			t.Errorf("got %s via %s, want light via COLORFGBG", res.Kind, res.Source)
		}
		if calls != 1 || !res.ProbeRan {
			t.Errorf("calls=%d probeRan=%v, want the probe to have run once", calls, res.ProbeRan)
		}
		if res.ProbedOK {
			t.Error("probedOK set for a probe that did not answer")
		}
		if string(res.Leftover) != "q" {
			t.Errorf("leftover %q, want the keystroke %q", res.Leftover, "q")
		}
	})

	t.Run("nil probe falls to COLORFGBG", func(t *testing.T) {
		res := Resolve(in, nil)
		if res.Kind != Light || res.Source != SourceColorFGBG {
			t.Errorf("got %s via %s, want light via COLORFGBG", res.Kind, res.Source)
		}
		if res.ProbeRan || res.ProbedOK {
			t.Errorf("probeRan=%v probedOK=%v with a nil probe", res.ProbeRan, res.ProbedOK)
		}
		if res.Leftover != nil {
			t.Errorf("leftover %q with a nil probe", res.Leftover)
		}
	})
}
