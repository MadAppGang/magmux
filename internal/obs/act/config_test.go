package act

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// configData is the migrated test data. See testdata/config/PROVENANCE.md for
// what the translation from TOML changed and what survived it.
const configData = "../testdata/config"

func configPath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(configData, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("test data %s is missing: %v", p, err)
	}
	return p
}

// TestTheConfigFileIsTheDefaultPostureWrittenOut is the non-vacuity check for
// every other test in this file.
//
// default-posture.json is requirements.md R6.1 spelled out, so loading it must
// produce exactly what [Default] produces — everything except Source, which
// names the file rather than the built-in. A loader that silently ignored the
// file would also pass this one, which is why TestALoadedConfigActuallyChanges
// ThePosture sits beside it and asserts the opposite direction.
func TestTheConfigFileIsTheDefaultPostureWrittenOut(t *testing.T) {
	got, err := Load(FileLoader(configPath(t, "default-posture.json")))
	if err != nil {
		t.Fatalf("loading the default-posture file: %v", err)
	}
	want := Default()
	if got.Actions != want.Actions {
		t.Errorf("actions are %+v, want %+v — the file is R6.1 written out and must agree with Default()",
			got.Actions, want.Actions)
	}
	if !strings.Contains(got.Source, "default-posture.json") {
		t.Errorf("Source is %q; a loaded policy must name the file it came from, because every "+
			"audit record it decides quotes this", got.Source)
	}
	// The notify block is where the file says MORE than Default does: Default
	// carries the zero Notify (silence), and this file switches it on.
	if !got.Notify.Enabled || got.Notify.Delay != 2*time.Minute || len(got.Notify.Quiet) != 1 {
		t.Errorf("notify is %+v, want enabled with a 2m delay and one quiet window", got.Notify)
	}
	if !got.Notify.Fires(NotifyOnWaiting) || !got.Notify.Fires(NotifyOnError) {
		t.Errorf("notify.on is %v, want both waiting and error", got.Notify.On)
	}
}

// TestALoadedConfigActuallyChangesThePosture is the seam exercised rather than
// asserted to exist: a WRONG IMPLEMENTATION that accepted the file and threw
// it away would pass every "absent and unparseable yield Default" test in this
// file, because Default is what those expect.
func TestALoadedConfigActuallyChangesThePosture(t *testing.T) {
	got, err := Load(FileLoader(configPath(t, "permissive-but-deferred.json")))
	if err != nil {
		t.Fatalf("loading the permissive file: %v", err)
	}
	if got.Actions.Send != StanceAlways || got.Actions.Lifecycle != StanceAlways {
		t.Fatalf("send/lifecycle are %v/%v, want always/always — the file says always and the "+
			"loader must carry that through, because the DEFERRAL is what refuses it, not the policy",
			got.Actions.Send, got.Actions.Lifecycle)
	}
	if got.Actions.Approve != StanceRule || len(got.Rules) != 1 {
		t.Fatalf("approve is %v with %d rules, want rule with 1", got.Actions.Approve, len(got.Rules))
	}
	if got.Notify.Delay != 0 || len(got.Notify.Quiet) != 0 {
		t.Errorf("notify is %+v, want a zero delay and no quiet windows", got.Notify)
	}
}

// TestAbsentConfigYieldsTheDefaultPostureAndSaysNothing is half of R6.3.
//
// An absent file is the normal state of a machine nobody has configured, so it
// is NOT an error: a line on every run saying "no config file" trains a reader
// to skip the place the real failure will appear.
func TestAbsentConfigYieldsTheDefaultPostureAndSaysNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "there-is-no-file-here.json")
	got, err := Load(FileLoader(missing))
	if err != nil {
		t.Fatalf("an absent config reported %v; absence is the normal case and is not a failure", err)
	}
	if got.Actions != Default().Actions {
		t.Errorf("actions are %+v, want the default posture %+v", got.Actions, Default().Actions)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("%s exists after loading it; the loader must never create a config file", missing)
	}
}

// TestUnparseableConfigYieldsTheDefaultPostureAndSaysSo is the other half of
// R6.3, and the one with damage behind it.
//
// The test data is ordered so that a parser which keeps what it read before
// the error hands back approve/send/lifecycle = "always". Both halves are
// asserted: the posture in force is the default, AND the failure is reported —
// a config the user wrote and mistyped that silently becomes the default is
// how somebody discovers months later that their rules never applied.
func TestUnparseableConfigYieldsTheDefaultPostureAndSaysSo(t *testing.T) {
	path := configPath(t, "unparseable.json")
	got, err := Load(FileLoader(path))
	if err == nil {
		t.Fatal("an unparseable config loaded without an error; R6.3's degradation must be visible")
	}
	if !strings.Contains(err.Error(), filepath.Base(path)) {
		t.Errorf("the error is %q and does not name the file; a user cannot fix a file they "+
			"have not been told about", err)
	}
	if got.Actions != Default().Actions {
		t.Fatalf("actions are %+v, want the default posture %+v — R6.3 forbids a MORE PERMISSIVE "+
			"outcome, and this file's permissive stanza parses cleanly before the error",
			got.Actions, Default().Actions)
	}
	// And directly, without Load's discard in the way: the parser itself must
	// not hand a half-read document back.
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("reading the test data: %v", rerr)
	}
	partial, perr := ParseConfig(raw, path)
	if perr == nil {
		t.Fatal("ParseConfig accepted the unparseable file")
	}
	if partial.Actions.Send == StanceAlways || partial.Actions.Lifecycle == StanceAlways {
		t.Errorf("ParseConfig returned %+v on failure — a half-read config is a posture nobody wrote",
			partial.Actions)
	}
}

// TestAMistypedKeyIsRefusedRatherThanIgnored is the quiet failure R6.3 is
// really about.
//
// encoding/json ignores unknown fields by default, so `"navigte"` would be a
// file that parses, a rule that never applies, and no way at all to find out.
// DisallowUnknownFields turns it into a loud refusal and the default posture.
func TestAMistypedKeyIsRefusedRatherThanIgnored(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"a misspelt stance key", `{"actions":{"navigte":"always"}}`, "navigte"},
		{"a misspelt block", `{"notifiy":{"enabled":true}}`, "notifiy"},
		{"a key from another tool", `{"actions":{"navigate":"always","colour":"blue"}}`, "colour"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParseConfig([]byte(tt.body), "inline.json")
			if err == nil {
				t.Fatalf("%s was accepted; an ignored typo is a rule that never applies", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the error is %q and does not name %q, so it cannot be fixed", err, tt.want)
			}
			if p.Actions != (Actions{}) {
				t.Errorf("a refused config returned %+v rather than the zero value", p.Actions)
			}
		})
	}
}

// TestASecondDocumentInOneFileIsRefused covers the shape json.Decoder makes
// easy to miss: it reads ONE value and stops, so everything after the first
// closing brace is silently unread.
func TestASecondDocumentInOneFileIsRefused(t *testing.T) {
	body := `{"actions":{"navigate":"always"}}` + "\n" + `{"actions":{"navigate":"never"}}`
	if _, err := ParseConfig([]byte(body), "inline.json"); err == nil {
		t.Fatal("two documents in one file were accepted; the second would be invisible")
	}
}

// TestAnAbsentKeyKeepsTheDefaultRatherThanRefusing is the rule that makes a
// partial config safe to write.
//
// A file that mentions notify and nothing else must not leave every action
// stance at StanceUnknown, which REFUSES — a user who configured their
// notifications would find navigate had turned itself off.
func TestAnAbsentKeyKeepsTheDefaultRatherThanRefusing(t *testing.T) {
	p, err := ParseConfig([]byte(`{"notify":{"enabled":true}}`), "inline.json")
	if err != nil {
		t.Fatalf("a notify-only config: %v", err)
	}
	if p.Actions != Default().Actions {
		t.Errorf("actions are %+v, want %+v — an absent key means the default, once, in apply()",
			p.Actions, Default().Actions)
	}
	if !p.Notify.Enabled {
		t.Error("the one key the file DID set was not applied")
	}

	// And the other direction: a key the file sets to the LESS permissive
	// value wins over the default.
	p, err = ParseConfig([]byte(`{"actions":{"navigate":"never"}}`), "inline.json")
	if err != nil {
		t.Fatalf("a navigate-only config: %v", err)
	}
	if p.Actions.Navigate != StanceNever {
		t.Errorf("navigate is %v, want never", p.Actions.Navigate)
	}
	if p.Actions.Approve != StanceAsk {
		t.Errorf("approve is %v, want the default ask", p.Actions.Approve)
	}
}

// TestQuietWindowsCrossMidnight is the case the test data exists for.
//
// It asserts the INTERIOR of each window and points well outside them, never a
// boundary: requirements.md does not say whether the endpoints are inclusive,
// and a test that assumed one reading would silently bless it.
func TestQuietWindowsCrossMidnight(t *testing.T) {
	p, err := Load(FileLoader(configPath(t, "quiet-crossing-midnight.json")))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(p.Notify.Quiet) != 2 {
		t.Fatalf("%d quiet windows, want 2", len(p.Notify.Quiet))
	}
	at := func(hour, min int) time.Time {
		return time.Date(2026, 9, 18, hour, min, 0, 0, time.Local)
	}
	for _, tt := range []struct {
		name  string
		when  time.Time
		quiet bool
	}{
		{"deep inside the midnight-crossing window, before midnight", at(23, 30), true},
		{"deep inside it, after midnight", at(3, 0), true},
		{"inside the same-day window", at(13, 30), true},
		{"the middle of the working day", at(10, 15), false},
		{"late afternoon, between the two windows", at(17, 45), false},
	} {
		if got := p.Notify.Silent(tt.when); got != tt.quiet {
			t.Errorf("%s (%s): silent=%v, want %v", tt.name, tt.when.Format("15:04"), got, tt.quiet)
		}
	}
}

// TestAQuietWindowMustBeSpelledLikeAQuietWindow pins the refusals, because a
// window that failed to parse and was skipped would be an hour of silence the
// user asked for and did not get.
func TestAQuietWindowMustBeSpelledLikeAQuietWindow(t *testing.T) {
	for _, bad := range []string{
		"22:00",        // not a span
		"22:00-",       // half a span
		"25:00-08:00",  // not an hour
		"22:70-08:00",  // not a minute
		"22-08",        // no minutes
		"08:00-08:00",  // zero width, or a whole day — say which
		"24:00-08:00",  // a real spelling of midnight elsewhere, and not here
		"22:00-08:00x", // trailing rubbish
	} {
		if _, err := ParseQuietWindow(bad); err == nil {
			t.Errorf("%q was accepted as a quiet window", bad)
		}
	}
	q, err := ParseQuietWindow(" 22:00 - 08:00 ")
	if err != nil {
		t.Fatalf("a spaced span: %v", err)
	}
	if q.From != NewDayMinute(22, 0) || q.To != NewDayMinute(8, 0) {
		t.Errorf("parsed %s-%s, want 22:00-08:00", q.From, q.To)
	}
}

// TestARuleThatNamesAToolMatchesNothing is the decision phase 7 reversed from
// phase 6, asserted in the direction that matters.
//
// The config is loadable — the R6 schema sketch itself contains `tool`, and
// refusing the file over it would refuse the user's whole configuration — and
// the rule it produces authorises NOTHING, because no session in this build
// has a tool to match against. Ignoring the term instead would widen the rule
// to cwd alone, which is broader than what the user wrote.
//
// The launch directory is ABSOLUTE, and until fix round 2 it was not: it was
// spelled "~/mag/magmux", which production never produces (a launch directory
// comes from cmd.Dir or os.Getwd, and neither writes a `~`), and that is the
// only reason the positive
// control below passed while the loader stored `~` literally. The home is
// pinned so the expansion is deterministic and no real home directory is
// consulted.
func TestARuleThatNamesAToolMatchesNothing(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	p, err := Load(FileLoader(configPath(t, "permissive-but-deferred.json")))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(p.Rules) != 1 || p.Rules[0].Tool != "Read" {
		t.Fatalf("rules are %+v, want one carrying Tool=Read", p.Rules)
	}
	inside := obs.Session{ID: "id-a", LaunchDir: "/home/someone/mag/magmux"}
	if p.Rules[0].Matches(inside) {
		t.Error("a rule naming a tool matched a session; nothing in this build has a tool, so " +
			"the only honest answer is no match")
	}
	// The same rule with the tool term removed DOES match, so the test above
	// is about the tool and not about the pattern.
	bare := p.Rules[0]
	bare.Tool = ""
	if !bare.Matches(inside) {
		t.Errorf("%q does not match %q even with no tool term; the pattern itself is wrong",
			bare.LaunchDir, inside.LaunchDir)
	}
}

// TestARuleMustSayWhatItAuthorisesAndWhatItMatches refuses the two half-written
// rules, at load, where there is somebody to tell.
func TestARuleMustSayWhatItAuthorisesAndWhatItMatches(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
	}{
		{"no do", `{"actions":{"rules":[{"match":{"cwd":"/a/**"}}]}}`, "do"},
		{"no match", `{"actions":{"rules":[{"do":"navigate"}]}}`, "match.cwd"},
		{"an empty match", `{"actions":{"rules":[{"do":"navigate","match":{"cwd":"  "}}]}}`, "match.cwd"},
		{"an unknown tier", `{"actions":{"rules":[{"do":"reboot","match":{"cwd":"/a/**"}}]}}`, "reboot"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tt.body), "inline.json")
			if err == nil {
				t.Fatalf("%s was accepted", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the error is %q and does not mention %q", err, tt.want)
			}
		})
	}
}

// TestTheFileAndTheWireSpellEveryValueTheSameWay is NFR6's argument applied to
// our own vocabulary: the config's words and the audit trail's words are the
// same words, because the file is decoded through the same enums the trail is
// encoded through.
//
// A second table of spellings in the loader is how `send_input` in the trail
// becomes `send` in the file and nobody notices until a config silently fails
// to enable something.
func TestTheFileAndTheWireSpellEveryValueTheSameWay(t *testing.T) {
	for _, tier := range []Tier{TierNavigate, TierApprove, TierSend, TierLifecycle} {
		body := `{"actions":{"rules":[{"do":"` + tier.String() + `","match":{"cwd":"/a/**"}}]}}`
		p, err := ParseConfig([]byte(body), "inline.json")
		if err != nil {
			t.Fatalf("the file cannot spell the tier the trail writes (%q): %v", tier, err)
		}
		if p.Rules[0].Do != tier {
			t.Errorf("%q decoded to %v", tier, p.Rules[0].Do)
		}
	}
	for _, st := range []Stance{StanceNever, StanceAsk, StanceRule, StanceAlways} {
		p, err := ParseConfig([]byte(`{"actions":{"navigate":"`+st.String()+`"}}`), "inline.json")
		if err != nil {
			t.Fatalf("the file cannot spell the stance the trail writes (%q): %v", st, err)
		}
		if p.Actions.Navigate != st {
			t.Errorf("%q decoded to %v", st, p.Actions.Navigate)
		}
	}
	for _, on := range []NotifyOn{NotifyOnWaiting, NotifyOnError} {
		p, err := ParseConfig([]byte(`{"notify":{"on":["`+on.String()+`"]}}`), "inline.json")
		if err != nil {
			t.Fatalf("the file cannot spell the notify trigger (%q): %v", on, err)
		}
		if !reflect.DeepEqual(p.Notify.On, []NotifyOn{on}) {
			t.Errorf("%q decoded to %v", on, p.Notify.On)
		}
	}
	// And the value that is not in the vocabulary is refused rather than
	// silently becoming the zero value, which refuses in a different way and
	// would be blamed on the policy rather than on the typo.
	if _, err := ParseConfig([]byte(`{"actions":{"navigate":"sometimes"}}`), "inline.json"); err == nil {
		t.Error(`"sometimes" was accepted as a stance`)
	}
	if _, err := ParseConfig([]byte(`{"notify":{"on":["done"]}}`), "inline.json"); err == nil {
		t.Error(`"done" was accepted as a notify trigger`)
	}
}

// TestNotifyDelayIsADurationAndNeverNegative covers the one numeric the file
// carries as a string.
func TestNotifyDelayIsADurationAndNeverNegative(t *testing.T) {
	p, err := ParseConfig([]byte(`{"notify":{"delay":"90s"}}`), "inline.json")
	if err != nil {
		t.Fatalf("a valid delay: %v", err)
	}
	if p.Notify.Delay != 90*time.Second {
		t.Errorf("delay is %v, want 90s", p.Notify.Delay)
	}
	for _, bad := range []string{`"2 minutes"`, `"-2m"`, `120`} {
		if _, err := ParseConfig([]byte(`{"notify":{"delay":`+bad+`}}`), "inline.json"); err == nil {
			t.Errorf("delay %s was accepted", bad)
		}
	}
}

// TestConfigPathIsUnderTheConfigDirectoryAndOpensNothing pins where the file
// lives and that resolving it is pure.
func TestConfigPathIsUnderTheConfigDirectoryAndOpensNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	want := filepath.Join(dir, "magmux", ConfigFileName)
	if got != want {
		t.Errorf("ConfigPath is %q, want %q", got, want)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("resolving the config path created %v; it reads environment variables only", entries)
	}
}

// TestALoadedPolicyDecidesWithTheFileAsItsAuthority closes the loop on R5.A:
// the audit trail has to be able to say which rule authorised an action, and
// for a configured magmux that means naming the file and the line.
func TestALoadedPolicyDecidesWithTheFileAsItsAuthority(t *testing.T) {
	path := configPath(t, "default-posture.json")
	p, err := Load(FileLoader(path))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	d := p.Decide(TierNavigate, obs.Session{ID: "id-a"})
	if !d.Allowed {
		t.Fatalf("navigate was refused by a file that says always: %+v", d)
	}
	if !strings.Contains(d.Rule, filepath.Base(path)) {
		t.Errorf("the decision's authority is %q and does not name the file it came from", d.Rule)
	}
}

// TestTheLoaderNeverWritesAnything is NFR1's neighbour: a loader that created
// a skeleton config on first run would be writing to a directory the user did
// not ask it to, and if XDG_CONFIG_HOME ever pointed under ~/.claude it would
// be writing there.
func TestTheLoaderNeverWritesAnything(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sessions.json")
	if _, err := Load(FileLoader(path)); err != nil {
		t.Fatalf("an absent config: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("the loader created %v", entries)
	}
}

// TestEveryTestDataConfigIsStillWhatItSaysItIs is the guard that keeps the
// migrated data honest. unparseable.json in particular is a file somebody will
// one day be tempted to tidy, and a tidied one tests nothing.
func TestEveryTestDataConfigIsStillWhatItSaysItIs(t *testing.T) {
	for _, tt := range []struct {
		file    string
		parses  bool
		because string
	}{
		{"default-posture.json", true, "it is R6.1 written out"},
		{"permissive-but-deferred.json", true, "it must be VALID and still be refused by the deferral"},
		{"quiet-crossing-midnight.json", true, "it carries the midnight-crossing window"},
		{"unparseable.json", false, "R6.3 needs a file that fails AFTER a permissive stanza"},
	} {
		raw, err := os.ReadFile(configPath(t, tt.file))
		if err != nil {
			t.Fatalf("reading %s: %v", tt.file, err)
		}
		_, perr := ParseConfig(raw, tt.file)
		if (perr == nil) != tt.parses {
			t.Errorf("%s parses=%v, want %v — %s", tt.file, perr == nil, tt.parses, tt.because)
		}
		if !tt.parses {
			// The ORDERING is the whole design of the file: the permissive
			// stanza comes first and is well-formed on its own, so a parser
			// that kept what it read before the error would hand back
			// "always". A file rearranged so the error came first would still
			// fail to parse and would catch nothing.
			body := string(raw)
			permissive, broken := strings.Index(body, `"actions"`), strings.Index(body, `"notify"`)
			switch {
			case !strings.Contains(body, `"always"`):
				t.Errorf("%s no longer contains a permissive stanza, so a half-reading parser "+
					"would not be caught by it", tt.file)
			case permissive < 0 || broken < 0 || permissive > broken:
				t.Errorf("%s no longer puts its permissive stanza BEFORE the syntax error "+
					"(actions at %d, notify at %d); that ordering is what the file is for",
					tt.file, permissive, broken)
			}
		}
	}
}

// TestEveryWordAParseErrorRecommendsIsAccepted — fix round 2, item 7.
//
// parseTier's error told a user to write `send_input` or `restart`, and the
// decoder refuses both: the enum's words are `send` and `lifecycle`. So a user
// who wrote `restart`, was told to write `restart`, and wrote it again got the
// same refusal — while the whole file stayed discarded and the default posture
// was in force. Every recommendation is now built from the enum it recommends
// for, so it cannot drift; this asserts the property rather than the words.
func TestEveryWordAParseErrorRecommendsIsAccepted(t *testing.T) {
	for _, tt := range []struct {
		name, template, bad string
	}{
		{"a rule's tier", `{"actions":{"rules":[{"do":%q,"match":{"cwd":"/a/**"}}]}}`, "restart"},
		{"a stance", `{"actions":{"navigate":%q}}`, "sometimes"},
		{"a notify trigger", `{"notify":{"on":[%q]}}`, "idle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(fmt.Sprintf(tt.template, tt.bad)), "inline.json")
			if err == nil {
				t.Fatalf("%q was accepted", tt.bad)
			}
			_, rest, ok := strings.Cut(err.Error(), "write one of ")
			list, _, closed := strings.Cut(rest, ")")
			if !ok || !closed {
				t.Fatalf("the error recommends nothing: %v", err)
			}
			words := strings.Split(list, ", ")
			if len(words) < 2 {
				t.Fatalf("the recommendation %q names fewer than two words", list)
			}
			for _, w := range words {
				if _, err := ParseConfig([]byte(fmt.Sprintf(tt.template, w)), "inline.json"); err != nil {
					t.Errorf("the error recommends %q, and %q is refused: %v", w, w, err)
				}
			}
		})
	}
}
