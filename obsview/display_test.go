package obsview

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Registry text reaching a person. Fix round 2, item 3, ported from mux's
// obsdisplay_test.go when the renderer moved below mux. The subtests that
// need a mux surface — the attention panel, the notifier, `agents focus
// --json` — stay in mux's obsdisplay_test.go under the same test names; the
// ones here are the renderer's own.
//
// Nothing here reads ~/.claude, probes a pid, or runs osascript.

// obsEpoch is the document's instant in every test here (mux's obssink_test
// uses the same one).
var obsEpoch = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

//
// Nothing here reads ~/.claude, probes a pid, or runs osascript.

// hostileText is every class of control the rule covers, in one valid UTF-8
// string: an OSC 52 clipboard write (ESC ] 52 … BEL), a CSI that clears the
// screen, a bare ESC, an 8-bit CSI encoded as UTF-8 (U+009B), DEL, and a
// newline that would forge a table row. It is VALID UTF-8 on purpose, so that
// --json can be asked to give it back exactly.
const hostileText = "ok-\x1b]52;c;aGVsbG8=\x07-\x1b[2J-\x1b-\xc2\x9b31m-\x7f-\nforged-row-end"

// inertRemnant is what must still be visible after neutralising: the payload
// with its introducer and terminator gone. A sanitiser that dropped the whole
// string would pass every "no control byte" assertion and leave the person
// with nothing, so each surface is also asked to show this.
const inertRemnant = "]52;c;aGVsbG8="

// controlsIn reports every control character in s, as a person's terminal
// would meet it: C0 (except the newline a caller allows), DEL, C1, and any
// byte that is not valid UTF-8 — a raw 0x9B is an 8-bit CSI to a terminal
// that honours them.
func controlsIn(s string, allowNewline bool) []string {
	var found []string
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			found = append(found, "invalid byte "+strings.ToUpper(hexByte(s[i])))
		case r == '\n' && allowNewline:
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			found = append(found, "U+"+strings.ToUpper(hexRune(r)))
		}
		i += size
	}
	return found
}

func hexByte(b byte) string { return hexRune(rune(b)) }

// jsonEscaped is the six characters JSON writes for r: a backslash, a u and
// four hex digits. Built rather than typed, so that this file holds no escape
// sequence that could be turned into the control character it names.
func jsonEscaped(r rune) string { return string(rune(92)) + "u" + hexRune(r) }

func hexRune(r rune) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[(r>>12)&0xf], digits[(r>>8)&0xf], digits[(r>>4)&0xf], digits[r&0xf]})
}

// hostileSession is one session whose every registry-derived string is
// hostile: its name, its raw status and waiting words, the reason the verdict
// wrote (which embeds a transcript's own error text), and a conflict detail.
func hostileSession() Session {
	since := obsEpoch.Add(-3 * time.Minute)
	return Session{
		ID: "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b", PID: 4242,
		Name:         hostileText,
		PaneID:       ptr(1),
		StatusRaw:    hostileText,
		WaitingRaw:   hostileText,
		StatusSince:  &since,
		ProcessAlive: LivenessYes,
		Attention: Attention{
			Needed: ptr(true),
			Reason: "it has hit an API-level error: " + hostileText,
		},
		Conflict: &Conflict{
			Tier:   TierInferred,
			Status: StatusIdle,
			Detail: hostileText,
		},
	}
}

func hostileFleet() Fleet {
	return Fleet{Epoch: obsEpoch, Sessions: []Session{hostileSession()}}
}

// cleanFleet is the same shape with nothing hostile in it — the control that
// proves a surface's OWN output passes the check, so that a failure below is
// about the registry text and not about magmux's framing.
func cleanFleet() Fleet {
	s := hostileSession()
	s.Name, s.StatusRaw, s.WaitingRaw = "plain-name", "compacting", "dialog open"
	s.Attention.Reason = "it has hit an API-level error: overloaded"
	s.Conflict.Detail = "the pane has produced nothing for 45m"
	return Fleet{Epoch: obsEpoch, Sessions: []Session{s}}
}

// TestRegistryTextReachesNoTerminalWithItsControlsIntact is item 3's test:
// one hostile session through all three human-facing renderers, and none of
// the control bytes survive any of them.
func TestRegistryTextReachesNoTerminalWithItsControlsIntact(t *testing.T) {
	t.Run("the magmux agents table, which list_agents also returns", func(t *testing.T) {
		var clean bytes.Buffer
		WriteFleetTable(&clean, cleanFleet())
		if got := controlsIn(clean.String(), true); len(got) != 0 {
			t.Fatalf("the table's own output carries controls %v; the check is miscalibrated", got)
		}

		var b bytes.Buffer
		WriteFleetTable(&b, hostileFleet())
		out := b.String()
		if got := controlsIn(out, true); len(got) != 0 {
			t.Errorf("magmux agents wrote %v to the terminal from a session's own record:\n%q", got, out)
		}
		t.Logf("what a person sees:\n%s", out)
		if !strings.Contains(out, inertRemnant) || !strings.Contains(out, "forged-row-end") {
			t.Errorf("the name was dropped rather than neutralised; a person must still see it:\n%s", out)
		}
		// The newline in the name must not have become a row of its own.
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "forged-row-end") {
				t.Errorf("a newline in the name forged a line of the table: %q", line)
			}
		}
	})

}

// TestJSONCarriesRegistryTextExactAndEscaped is the other half of the rule:
// --json must NOT neutralise, because it is the document and a consumer is
// owed the exact bytes the tool wrote — and it must not put a control on the
// terminal either.
//
// The brief for this round said encoding/json already escapes control
// characters and asked for a test to assert it rather than assume it. The
// assertion found the assumption half true: encoding/json escapes C0 (ESC and
// BEL become six-character \u00XX escapes) but writes DEL and C1 RAW, so `magmux agents
// --json` put U+009B, an 8-bit CSI, on the terminal. Escaping those two
// classes as \u00XX changes no decoded value, which is what this asserts.
func TestJSONCarriesRegistryTextExactAndEscaped(t *testing.T) {
	check := func(t *testing.T, out string) {
		t.Helper()
		if got := controlsIn(out, true); len(got) != 0 {
			t.Errorf("--json wrote %v raw:\n%q", got, out)
		}
		if !strings.Contains(out, jsonEscaped(0x1b)+`]52;c;aGVsbG8=`+jsonEscaped(0x07)) {
			t.Errorf("the OSC 52 sequence is not escaped in the JSON:\n%s", out)
		}
	}

	t.Run("magmux agents --json", func(t *testing.T) {
		var b bytes.Buffer
		WriteFleetJSON(&b, hostileFleet())
		check(t, b.String())
		var back Fleet
		if err := json.Unmarshal(b.Bytes(), &back); err != nil {
			t.Fatalf("the output is not a document: %v", err)
		}
		s := back.Sessions[0]
		if s.Name != hostileText || s.StatusRaw != hostileText || s.WaitingRaw != hostileText {
			t.Errorf("--json did not carry the record exactly:\n got name %q\nwant      %q", s.Name, hostileText)
		}
	})

}

// TestDisplaySafeIsTheOneRule pins the rule itself, class by class. It is the
// function all three renderers call; a second copy of it anywhere is how this
// defect happened.
func TestDisplaySafeIsTheOneRule(t *testing.T) {
	rep := string(utf8.RuneError)
	for _, tt := range []struct{ in, want string }{
		{"", ""},
		{"plain-name", "plain-name"},
		{"café · 日本語 · emoji \xf0\x9f\x98\x80", "café · 日本語 · emoji \xf0\x9f\x98\x80"},
		// Whitespace controls become a SPACE: a multi-line error text stays
		// readable on one line, and cannot forge a row.
		{"a\tb\nc\rd\ve\ff", "a b c d e f"},
		// Every other control becomes U+FFFD, VISIBLY: a person sees that the
		// name held something, rather than a name that silently lost bytes.
		{"\x1b[2J", rep + "[2J"},
		{"\x1b]52;c;aGk=\x07", rep + "]52;c;aGk=" + rep},
		{"a\x00b", "a" + rep + "b"},
		{"a\x7fb", "a" + rep + "b"},
		{"a\xc2\x9bb", "a" + rep + "b"}, // C1 CSI, as UTF-8
		{"a\xc2\x85b", "a" + rep + "b"}, // C1 NEL
		{"a\x9bb", "a" + rep + "b"},     // a raw 8-bit CSI, which is not UTF-8 at all
		{"a\xc2\xa0b", "a\xc2\xa0b"},    // U+00A0 is the first character past C1, and is kept
	} {
		if got := DisplaySafe(tt.in); got != tt.want {
			t.Errorf("DisplaySafe(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got := controlsIn(DisplaySafe(tt.in), false); len(got) != 0 {
			t.Errorf("DisplaySafe(%q) still carries %v", tt.in, got)
		}
	}
}

func ptr[T any](v T) *T { return &v }
