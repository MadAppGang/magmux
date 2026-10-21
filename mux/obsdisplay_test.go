package mux

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/obsview"
)

// Registry text reaching a person. Fix round 2, item 3.
//
// A session's name, its raw status and waiting words, and the reasons built
// from its transcript come out of files magmux does not write. A name is not
// always typed by a person: Claude Code can generate one from conversation
// content, so text an attacker can influence — a crafted repository, a prompt
// — can reach it. Until this round two of the three surfaces a person reads
// printed that text raw: `magmux agents` wrote it to the terminal byte for byte,
// OSC 52 clipboard write included, and the attention view painted it into the
// panel's own parser. The notifier already neutralised it, but only for C0 and
// DEL; an 8-bit CSI (U+009B) went through.
//
// The rule itself, the table subtest and the `magmux agents --json` subtest
// live with the renderer, in obsview/display_test.go, under the same test
// names. The subtests here are the ones that need a mux surface: the attention
// panel, the notifier, and `magmux agents focus --json`.
//
// Nothing here reads ~/.claude, probes a pid, or runs osascript.

// obsEpoch is the document's instant in every observation test in mux.
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
func hexRune(r rune) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[(r>>12)&0xf], digits[(r>>8)&0xf], digits[(r>>4)&0xf], digits[r&0xf]})
}

// jsonEscaped is the six characters JSON writes for r: a backslash, a u and
// four hex digits. Built rather than typed, so that this file holds no escape
// sequence that could be turned into the control character it names.
func jsonEscaped(r rune) string { return string(rune(92)) + "u" + hexRune(r) }

// magmuxSGR is the ONLY escape the panel's own painting emits: a colour or
// attribute, ESC [ digits m. It is stripped before the frame is checked, and
// nothing else is. A stripper that also ate CSI 2J or an OSC would hide the
// defect this test exists to catch — which is why obssink_test's stripANSI,
// which stops at any 'm', 'H' or 'J', is not used here.
var magmuxSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// attentionFrame paints the attention view for a fleet, exactly as the panel
// would, and returns it with magmux's own SGR removed.
func attentionFrame(fleet obs.Fleet) string {
	cp := newControlPanel()
	cp.setAttention(attentionOf(fleet, obsEpoch, pipeline.Delivery{}))
	cp.mu.Lock()
	snap := cp.snapshotLocked()
	cp.mu.Unlock()
	frame := strings.Join(cp.frameAttention(snap, 200, 40), "\n")
	return magmuxSGR.ReplaceAllString(frame, "")
}

// hostileSession is one session whose every registry-derived string is
// hostile: its name, its raw status and waiting words, the reason the verdict
// wrote (which embeds a transcript's own error text), and a conflict detail.
func hostileSession() obs.Session {
	since := obsEpoch.Add(-3 * time.Minute)
	return obs.Session{
		ID: "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b", PID: 4242,
		Name:         hostileText,
		PaneID:       obs.Ptr(1),
		StatusRaw:    hostileText,
		WaitingRaw:   hostileText,
		StatusSince:  &since,
		ProcessAlive: obs.LivenessYes,
		Attention: obs.Attention{
			Needed: obs.Ptr(true),
			Reason: "it has hit an API-level error: " + hostileText,
		},
		Conflict: &obs.Conflict{
			Tier:   obs.TierInferred,
			Status: obs.StatusIdle,
			Detail: hostileText,
		},
	}
}

func hostileFleet() obs.Fleet {
	return obs.Fleet{Epoch: obsEpoch, Sessions: []obs.Session{hostileSession()}}
}

// cleanFleet is the same shape with nothing hostile in it — the control that
// proves a surface's OWN output passes the check, so that a failure below is
// about the registry text and not about magmux's framing.
func cleanFleet() obs.Fleet {
	s := hostileSession()
	s.Name, s.StatusRaw, s.WaitingRaw = "plain-name", "compacting", "dialog open"
	s.Attention.Reason = "it has hit an API-level error: overloaded"
	s.Conflict.Detail = "the pane has produced nothing for 45m"
	return obs.Fleet{Epoch: obsEpoch, Sessions: []obs.Session{s}}
}

// TestRegistryTextReachesNoTerminalWithItsControlsIntact is item 3's test:
// one hostile session through all three human-facing renderers, and none of
// the control bytes survive any of them.
func TestRegistryTextReachesNoTerminalWithItsControlsIntact(t *testing.T) {
	t.Run("the attention panel", func(t *testing.T) {
		if got := controlsIn(attentionFrame(cleanFleet()), true); len(got) != 0 {
			t.Fatalf("the panel's own frame carries %v once its SGR is removed; the check is "+
				"miscalibrated", got)
		}
		// Where registry text ENTERS the panel. An injected SGR would survive
		// the frame check below, because it looks like magmux's own; it cannot
		// survive this one.
		for _, r := range attentionOf(hostileFleet(), obsEpoch, pipeline.Delivery{}).rows {
			for field, v := range map[string]string{
				"name": r.name, "status": r.status, "waiting": r.waiting, "reason": r.reason,
			} {
				if got := controlsIn(v, false); len(got) != 0 {
					t.Errorf("the panel's %s carries %v: %q", field, got, v)
				}
			}
		}
		frame := attentionFrame(hostileFleet())
		if got := controlsIn(frame, true); len(got) != 0 {
			t.Errorf("the attention view painted %v into the panel's parser:\n%q", got, frame)
		}
		if !strings.Contains(frame, inertRemnant) {
			t.Errorf("the panel dropped the name rather than neutralising it:\n%s", frame)
		}
	})

	t.Run("the notification", func(t *testing.T) {
		var script string
		n := &osaNotifier{run: func(_ context.Context, s string) error {
			script = s
			return nil
		}}
		note := notify.Note{Session: hostileSession(), On: act.NotifyOnWaiting, For: 3 * time.Minute}
		if err := n.Send(context.Background(), note); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got := controlsIn(script, false); len(got) != 0 {
			t.Errorf("the osascript argument carries %v: %q", got, script)
		}
		if !strings.Contains(script, inertRemnant) {
			t.Errorf("the notification dropped the name rather than neutralising it: %q", script)
		}
		// The AppleScript quoting still holds: one literal for the body, one
		// for the title, and no quote from the name ends either early.
		if strings.Count(script, `"`)-strings.Count(script, `\"`) != 4 {
			t.Errorf("the script is not two well-formed literals: %q", script)
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

	t.Run("magmux agents focus --json", func(t *testing.T) {
		var b bytes.Buffer
		writeAuditJSON(&b, []act.Record{{
			Verb:   act.VerbFocus,
			Tier:   act.TierNavigate,
			Target: act.Target{SessionID: "id-a", PID: 4242, Name: hostileText},
		}})
		check(t, b.String())
		var back act.Record
		if err := json.Unmarshal(b.Bytes(), &back); err != nil {
			t.Fatalf("the output is not a record: %v", err)
		}
		if back.Target.Name != hostileText {
			t.Errorf("the audit record's name came back as %q, want %q", back.Target.Name, hostileText)
		}
	})
}

// TestTheNotifierHasNoPrivateCopyOfTheRule — the defect was two renderers
// that forgot a rule a third one followed privately. The notifier's own
// control-character branch is deleted; this keeps it deleted. The rule now
// lives below mux, in obsview, so the notifier must call it there.
func TestTheNotifierHasNoPrivateCopyOfTheRule(t *testing.T) {
	src := readSource(t, "obsnotify.go")
	for _, private := range []string{"r < 0x20", "0x7f", "0x9f", "unicode.IsControl"} {
		if strings.Contains(src, private) {
			t.Errorf("obsnotify.go tests %q itself; it must call obsview.DisplaySafe, the one rule", private)
		}
	}
	if !strings.Contains(src, "obsview.DisplaySafe(") {
		t.Error("obsnotify.go does not call obsview.DisplaySafe")
	}
}

// TestFormatDurationAgreesWithObsview holds the two copies of the duration
// formatter to one answer. mux's is the terminal core's, obsview's is the
// renderer's, and the panel and the table print the same IN STATUS FOR.
func TestFormatDurationAgreesWithObsview(t *testing.T) {
	for _, d := range []time.Duration{
		0, time.Millisecond, 999 * time.Millisecond, time.Second, 1500 * time.Millisecond,
		59*time.Second + 999*time.Millisecond, time.Minute, 4*time.Minute + 12*time.Second,
		59*time.Minute + 59*time.Second, time.Hour, time.Hour + 3*time.Minute, 49*time.Hour + 7*time.Minute,
	} {
		if mine, theirs := formatDuration(d), obsview.FormatDuration(d); mine != theirs {
			t.Errorf("formatDuration(%v) = %q but obsview.FormatDuration = %q", d, mine, theirs)
		}
	}
}

// readSource reads one file of this package, for the assertions that can only
// be made about the SOURCE — an absence, which no behavioural test can prove.
func readSource(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}
