package act

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ConfigFileName is the policy file's name inside magmux's config directory.
//
// It sits beside AuditFileName's spelling on purpose — `sessions.json` decides
// and `sessions-audit.jsonl` records — so that a person who has found one can
// guess the other.
const ConfigFileName = "sessions.json"

// The config file is JSON, and the reason is REVERSIBILITY rather than merit.
//
// requirements.md R6 sketches this file in TOML, and the test data the test
// architect wrote for it is four TOML files. TOML is the better format for the
// job: this file's whole purpose is recording rules a human wrote and will
// want to annotate, and JSON has no comments. Nobody should read this package
// and conclude JSON was judged superior, because it was not.
//
// What decided it is the dependency pin. magmux's module set is exactly
// {x/sys, x/term, fsnotify} and TestDependencySetIsExactlyWhatWasDecided fails
// the build on a fourth, precisely so that adding one is a deliberate decision
// visible in a diff that says so. A TOML library is that fourth module, the
// decision is the user's, and the user was asked twice and did not answer.
//
// So the two ways of being wrong are not symmetric:
//
//   - JSON now, TOML later, is about a hundred lines against an UNCHANGED
//     schema. [fileSchema] is the only thing that would move, [Policy] and
//     everything that reads it would not, and the tests below are written
//     against Load and FileLoader rather than against a syntax.
//   - A fourth dependency added without the decision has already defeated the
//     gate the pin exists to be, and undoing it means undoing a release.
//
// The migrated test data lives at testdata/config/*.json with the TOML
// originals left where they were; testdata/PROVENANCE.md records the
// translation and what survived it.

// ConfigPath is ${XDG_CONFIG_HOME:-~/.config}/magmux/sessions.json.
//
// It reads environment variables only. Nothing is opened, so resolving a path
// cannot itself touch anything.
//
// There is deliberately NO refusal of a path under ~/.claude here, where
// [CheckAuditPath] has one. NFR1 forbids WRITING under that tree because
// thirty live Claude Code processes own it; reading a file a user pointed at
// is the user's business, and a guard that refused it would be enforcing a
// rule nobody wrote.
func ConfigPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "magmux", ConfigFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory, so cannot find the policy "+
			"file; set XDG_CONFIG_HOME: %w", err)
	}
	return filepath.Join(home, ".config", "magmux", ConfigFileName), nil
}

// FileLoader returns the [Loader] that reads path.
//
// # The three outcomes, which are requirements.md R6.3
//
//   - The file is ABSENT. [Default] and NO error. An absent config is the
//     normal state of a machine nobody has configured, and reporting it every
//     run would train a reader to ignore the line the real failure will
//     appear on.
//   - The file is present and does not parse, or cannot be read. An ERROR,
//     and the policy value beside it is the zero value — which [Load]
//     discards anyway. The user wrote something and it is not in force, and
//     that has to be said out loud: a mistyped config that silently becomes
//     the default is how somebody discovers months later that their rules
//     never applied.
//   - The file is present and parses. [Default] with the file's keys overlaid,
//     and [Policy.Source] naming the file.
//
// # Overlaid onto Default, in ONE place
//
// [Loader]'s own contract: a loader that succeeds owns the whole policy, and
// Load does not merge. So a file that says nothing about `approve` must not
// leave it at StanceUnknown — which refuses, and would be a posture nobody
// wrote — it gets Default's `ask`. Every optional key in [fileSchema] is
// therefore a POINTER, and absent means "whatever Default says" exactly once,
// in [fileSchema.apply].
//
// # An unknown key is an ERROR, not something to ignore
//
// DisallowUnknownFields, deliberately, and it is the half of R6.3 that a
// parser normally gets wrong in the quiet direction. `"navigte": "always"`
// with unknown fields ignored is a file that parses, a rule that never
// applies, and no way at all to find out. It is refused instead, the whole
// file is discarded, and the default posture is in force with a message
// naming the key. That is the conservative direction in both senses: less
// permissive, and louder.
func FileLoader(path string) Loader {
	return func() (Policy, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Not a failure. There is no configuration, so the built-in
				// posture IS the configuration.
				return Default(), nil
			}
			return Policy{}, fmt.Errorf("reading %s: %w", path, err)
		}
		return ParseConfig(data, path)
	}
}

// ParseConfig decodes one config document. source is what [Policy.Source] is
// built from and appears in every audit record the result decides.
//
// It is exported so that a test — and a future `magmux agents config --check` —
// can validate a document without putting one on disk.
func ParseConfig(data []byte, source string) (Policy, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()

	var f fileSchema
	if err := dec.Decode(&f); err != nil {
		return Policy{}, fmt.Errorf("parsing %s: %w", source, err)
	}
	// A second value in the same file is a file that says two different
	// things, and taking the first silently is how the second one becomes
	// invisible. json.Decoder reads one value and stops, so this is the only
	// way to notice.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return Policy{}, fmt.Errorf("parsing %s: there is more than one JSON document in this "+
			"file, and a policy file is exactly one", source)
	}

	p, err := f.apply(Default())
	if err != nil {
		return Policy{}, fmt.Errorf("parsing %s: %w", source, err)
	}
	p.Source = source
	return p, nil
}

// fileSchema is the ON-DISK shape, and it is a different type from [Policy] on
// purpose.
//
// Policy is what the gate decides with; this is what a human wrote. They
// differ in three ways that would be wrong to fold together: every key here is
// OPTIONAL and absence means "keep the default", durations and clock times are
// STRINGS because that is what a person types, and the field names are the
// file's spelling rather than Go's. Decoding straight into Policy would make
// an absent key mean StanceUnknown, which refuses — a file that mentions
// notify and nothing else would silently turn navigate off.
type fileSchema struct {
	Notify  *notifySchema  `json:"notify"`
	Actions *actionsSchema `json:"actions"`
}

type notifySchema struct {
	Enabled *bool     `json:"enabled"`
	On      *[]string `json:"on"`
	Delay   *string   `json:"delay"`
	Quiet   *[]string `json:"quiet"`
}

type actionsSchema struct {
	Navigate  *string       `json:"navigate"`
	Approve   *string       `json:"approve"`
	Send      *string       `json:"send"`
	Lifecycle *string       `json:"lifecycle"`
	Rules     *[]ruleSchema `json:"rules"`
}

// ruleSchema is one `[[actions.rules]]` entry.
//
// `match.tool` is CARRIED, not ignored and not refused, and the reason is in
// [Rule]: it makes the rule match nothing until R5.2 ships. Ignoring the key
// would leave the user with a rule they believe is narrow and which actually
// matches on cwd alone — the more permissive reading of something they wrote —
// and refusing the key would refuse their whole file over a term the R6 sketch
// itself contains.
type ruleSchema struct {
	Match *matchSchema `json:"match"`
	Do    *string      `json:"do"`
	Note  *string      `json:"note"`
}

type matchSchema struct {
	Cwd  *string `json:"cwd"`
	Tool *string `json:"tool"`
}

// apply overlays the file onto base and returns the result. base is [Default],
// so every key the file omits keeps the posture requirements.md R6.1 names.
func (f fileSchema) apply(base Policy) (Policy, error) {
	p := base.Clone()
	if f.Actions != nil {
		a := f.Actions
		for _, set := range []struct {
			key   string
			value *string
			into  *Stance
		}{
			{"navigate", a.Navigate, &p.Actions.Navigate},
			{"approve", a.Approve, &p.Actions.Approve},
			{"send", a.Send, &p.Actions.Send},
			{"lifecycle", a.Lifecycle, &p.Actions.Lifecycle},
		} {
			if set.value == nil {
				continue
			}
			st, err := parseStance(*set.value)
			if err != nil {
				return Policy{}, fmt.Errorf("actions.%s: %w", set.key, err)
			}
			*set.into = st
		}
		if a.Rules != nil {
			rules := make([]Rule, 0, len(*a.Rules))
			for i, rs := range *a.Rules {
				r, err := rs.rule()
				if err != nil {
					return Policy{}, fmt.Errorf("actions.rules[%d]: %w", i, err)
				}
				rules = append(rules, r)
			}
			p.Rules = rules
		}
	}
	if f.Notify != nil {
		n := f.Notify
		if n.Enabled != nil {
			p.Notify.Enabled = *n.Enabled
		}
		if n.On != nil {
			on := make([]NotifyOn, 0, len(*n.On))
			for i, word := range *n.On {
				v, err := parseNotifyOn(word)
				if err != nil {
					return Policy{}, fmt.Errorf("notify.on[%d]: %w", i, err)
				}
				on = append(on, v)
			}
			p.Notify.On = on
		}
		if n.Delay != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*n.Delay))
			if err != nil {
				return Policy{}, fmt.Errorf("notify.delay: %q is not a duration "+
					"(write it like \"2m\" or \"30s\"): %w", *n.Delay, err)
			}
			if d < 0 {
				return Policy{}, fmt.Errorf("notify.delay: %q is negative, and a notification "+
					"cannot be sent before the thing it announces", *n.Delay)
			}
			p.Notify.Delay = d
		}
		if n.Quiet != nil {
			quiet := make([]QuietWindow, 0, len(*n.Quiet))
			for i, span := range *n.Quiet {
				q, err := ParseQuietWindow(span)
				if err != nil {
					return Policy{}, fmt.Errorf("notify.quiet[%d]: %w", i, err)
				}
				quiet = append(quiet, q)
			}
			p.Notify.Quiet = quiet
		}
	}
	return p, nil
}

func (rs ruleSchema) rule() (Rule, error) {
	if rs.Do == nil {
		return Rule{}, fmt.Errorf(`no "do": a rule that does not say which tier it authorises ` +
			`cannot authorise one`)
	}
	tier, err := parseTier(*rs.Do)
	if err != nil {
		return Rule{}, fmt.Errorf("do: %w", err)
	}
	if rs.Match == nil || rs.Match.Cwd == nil || strings.TrimSpace(*rs.Match.Cwd) == "" {
		// An empty match matches NOTHING (see [Rule]), so a rule with no
		// pattern is dead weight the user would never be told about. Refused
		// at load, where there is somebody to tell.
		return Rule{}, fmt.Errorf(`no "match.cwd": a rule with nothing to match on matches ` +
			`nothing, and a rule that can never fire is worse than one that does not exist`)
	}
	pattern, err := expandHome(strings.TrimSpace(*rs.Match.Cwd))
	if err != nil {
		return Rule{}, fmt.Errorf("match.cwd: %w", err)
	}
	r := Rule{Do: tier, LaunchDir: pattern}
	if rs.Match.Tool != nil {
		r.Tool = strings.TrimSpace(*rs.Match.Tool)
	}
	if rs.Note != nil {
		r.Note = *rs.Note
	}
	return r, nil
}

// expandHome resolves a leading `~` in a rule's pattern against the home
// directory, at LOAD time.
//
// requirements.md R6 writes rules as `"cwd": "~/mag/**"`, and a session's
// launch directory never contains a `~` — it comes from cmd.Dir or os.Getwd.
// Until fix round 2 the pattern was stored as written, so the rule the spec
// itself sketches could never match: it failed safe, and it was still a rule
// the user wrote that silently never applied, which is the failure
// DisallowUnknownFields exists to prevent for a key.
//
// Three shapes, and the third is refused rather than kept:
//
//   - no leading `~`: returned as written;
//   - `~` or `~/…`: the home directory, from os.UserHomeDir ($HOME). If it
//     cannot be found, or is not absolute, that is a LOAD ERROR and not a
//     silent non-match — R6.3 then discards the whole file and the default
//     posture is in force, loudly. The lookup happens only when a pattern
//     has a `~`, so a file without one never needs a home;
//   - `~name/…`: a shell's spelling of SOMEONE ELSE's home. Expanding it means
//     reading the user database, which this loader does not do, and keeping
//     it literally is the never-match again. Refused, naming the pattern.
//
// Matching itself stays in one place, matchPath, and still does no expansion:
// the loader owns the file's syntax, so there is one spelling of the rule in
// memory and it is the absolute one.
func expandHome(pattern string) (string, error) {
	if !strings.HasPrefix(pattern, "~") {
		return pattern, nil
	}
	rest := pattern[1:]
	if rest != "" && !strings.HasPrefix(rest, "/") {
		return "", fmt.Errorf("%q names another user's home directory, which is not expanded; "+
			"write the absolute path", pattern)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%q starts with ~, and the home directory cannot be found (%v); "+
			"write the absolute path, or set HOME", pattern, err)
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("%q starts with ~, and the home directory %q is not an absolute "+
			"path, so the rule could never match; write the absolute path", pattern, home)
	}
	return filepath.Join(home, rest), nil
}

// parseStance, parseTier and parseNotifyOn go through the same JSON decoders
// the wire uses, so the file and the audit trail cannot spell a value
// differently. The quoting is what makes that true: the decoders take a JSON
// string, which is what a value read out of a JSON file already was.
//
// The "write one of" hint in each error is built from the SAME enum, for the
// same reason. It was typed by hand until fix round 2, and parseTier's told
// the user to write `send_input` or `restart` — two words the decoder refuses —
// so a user who followed the error's advice got the error again.
func parseStance(word string) (Stance, error) {
	var s Stance
	if err := s.UnmarshalJSON(quoteJSON(word)); err != nil {
		return StanceUnknown, fmt.Errorf("%w (write one of %s)", err, stanceEnum.accepted())
	}
	return s, nil
}

func parseTier(word string) (Tier, error) {
	var t Tier
	if err := t.UnmarshalJSON(quoteJSON(word)); err != nil {
		return TierUnknown, fmt.Errorf("%w (write one of %s)", err, tierEnum.accepted())
	}
	return t, nil
}

func parseNotifyOn(word string) (NotifyOn, error) {
	var n NotifyOn
	if err := n.UnmarshalJSON(quoteJSON(word)); err != nil {
		return NotifyOnUnknown, fmt.Errorf("%w (write one of %s)", err, notifyOnEnum.accepted())
	}
	return n, nil
}

func quoteJSON(s string) []byte {
	b, err := json.Marshal(strings.TrimSpace(s))
	if err != nil {
		// json.Marshal of a string cannot fail; the branch exists so the
		// error is not discarded silently if that ever stops being true.
		return []byte(`""`)
	}
	return b
}

// ParseQuietWindow reads "HH:MM-HH:MM" into a [QuietWindow].
//
// It is exported because the SPELLING is part of the config's contract and a
// test should be able to assert on it directly rather than through a file.
//
// The midnight-crossing case is the whole reason the type has a method: the
// R6 sketch's own example is "22:00-08:00", whose end is before its start. It
// is accepted here and wraps in [QuietWindow.Contains]; a reader that compared
// two minute counts would treat it as empty and notify all night.
//
// A window whose two ends are EQUAL is refused. "08:00-08:00" is either a
// zero-width window (which is nothing, and writing nothing is easier) or a
// whole day (which is `enabled = false`, and says so). Guessing between them
// silently is worse than asking.
func ParseQuietWindow(span string) (QuietWindow, error) {
	s := strings.TrimSpace(span)
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return QuietWindow{}, fmt.Errorf("%q is not a span; write it like \"22:00-08:00\"", span)
	}
	f, err := parseDayMinute(from)
	if err != nil {
		return QuietWindow{}, fmt.Errorf("%q: %w", span, err)
	}
	t, err := parseDayMinute(to)
	if err != nil {
		return QuietWindow{}, fmt.Errorf("%q: %w", span, err)
	}
	if f == t {
		return QuietWindow{}, fmt.Errorf("%q starts and ends at the same minute. If you meant "+
			"\"never notify\", write notify.enabled = false; if you meant \"no quiet hours\", "+
			"leave notify.quiet out", span)
	}
	return QuietWindow{From: f, To: t}, nil
}

// parseDayMinute reads "HH:MM". 24:00 is refused along with everything else
// out of range: it is a real spelling of midnight in some formats and means
// the END of a day, and a window that silently became 00:00 would be the
// opposite of what was written.
func parseDayMinute(s string) (DayMinute, error) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%q is not a time of day; write it like \"22:00\"", s)
	}
	hour, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day; write it like \"22:00\"", s)
	}
	minute, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day; write it like \"22:00\"", s)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("%q is not a time of day; hours are 00-23 and minutes are 00-59", s)
	}
	return NewDayMinute(hour, minute), nil
}
