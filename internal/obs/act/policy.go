package act

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// Stance is what the policy says about one [Tier]. It is requirements.md R6's
// `[actions]` value set, plus a zero value for "nothing said anything".
//
// The order is by permissiveness and the zero value is at the bottom on
// purpose; see the package doc.
type Stance uint8

const (
	// StanceUnknown is the zero value: this tier was never configured. It
	// REFUSES, and it is a different refusal from [StanceNever] — "nobody
	// said" and "the user said no" are different facts, and the audit trail
	// prints which one refused.
	StanceUnknown Stance = iota

	// StanceNever is the user saying no. Nothing overrides it.
	StanceNever

	// StanceAsk means the action may happen only after whoever asked for it
	// obtains consent. The gate returns [ErrNeedsConfirmation] and performs
	// nothing; it never prompts.
	StanceAsk

	// StanceRule means the action is allowed only where a rule the user wrote
	// matches this session. No match is a refusal, and it is the same
	// refusal a matching rule would have to have earned — R6.2: nothing acts
	// unattended without a rule the user wrote.
	StanceRule

	// StanceAlways is unconditional permission. R6.1 gives it to
	// [TierNavigate] and to nothing else, because navigate touches the view
	// and nothing else.
	StanceAlways
)

var stanceEnum = enumSpec{"Stance", []string{
	"unset",
	"never",
	"ask",
	"rule",
	"always",
}}

// String returns the wire word.
func (s Stance) String() string { return stanceEnum.name(int(s)) }

// MarshalJSON writes null for [StanceUnknown] and the wire word otherwise.
func (s Stance) MarshalJSON() ([]byte, error) { return stanceEnum.marshal(int(s)) }

// UnmarshalJSON reads null as [StanceUnknown]. An unrecognised name is an
// error.
func (s *Stance) UnmarshalJSON(b []byte) error {
	i, err := stanceEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = Stance(i)
	return nil
}

// Actions is one stance per tier: requirements.md R6's `[actions]` table.
//
// Four named fields and not a map[Tier]Stance, for two reasons. A map is a
// pointer to mutable state, and v4 decision 7 says the policy is a plain
// struct copied by value precisely so that "who may change this while an
// action is being decided" never arises — two copies of a Policy would share
// one map. And a missing key and a zero value would then be two spellings of
// the same thing, where a field has exactly one.
type Actions struct {
	Navigate  Stance
	Approve   Stance
	Send      Stance
	Lifecycle Stance
}

// Of returns the stance for one tier. An unrecognised tier — including
// [TierUnknown] — is [StanceUnknown], which refuses.
func (a Actions) Of(t Tier) Stance {
	switch t {
	case TierNavigate:
		return a.Navigate
	case TierApprove:
		return a.Approve
	case TierSend:
		return a.Send
	case TierLifecycle:
		return a.Lifecycle
	}
	return StanceUnknown
}

// Rule is one entry of requirements.md R6's `[[actions.rules]]`: a match and
// the tier it authorises.
//
// # What it can match on today, and why a tool match matches nothing
//
// The sketched schema matches on `cwd` and `tool`. Only the first has anything
// to match against in this build: a session's launch directory is observed and
// is on the wire, and the `tool` of a permission prompt belongs to R5.2, which
// is deferred and produces no field.
//
// Phase 6 did not model [Rule.Tool] at all, on the grounds that a rule which
// can never fire is worse than one that does not exist. Phase 7 added it when
// the file format arrived, and REVERSED the conclusion rather than the
// reasoning: a config that names a tool has to load, because the user wrote
// one and the alternative is refusing their whole file — but it must not
// silently become a rule that matches on cwd ALONE, which is broader than
// what they wrote. So the term is kept and it matches nothing until R5.2
// ships. Narrower than written is a safe kind of wrong; broader is not.
//
// # An empty match matches NOTHING
//
// The dangerous reading of an absent pattern is "match everything", which
// turns a partially written rule into blanket permission. R6.2 says nothing
// acts unattended without a rule the user WROTE, and a rule with no match
// clause was not written for any case.
type Rule struct {
	// Do is the tier this rule authorises when it matches. A rule for a
	// deferred tier still cannot execute; the deferral is checked first and
	// this changes nothing about that.
	Do Tier

	// LaunchDir matches [model.Session.LaunchDir]. A trailing "/**" matches the
	// directory and everything beneath it; otherwise it is a path.Match
	// pattern, so "*" does not cross a separator. No tilde expansion and no
	// environment substitution happen HERE — the loader owns the file's
	// syntax, and doing it in two places is how the two spellings drift. The
	// loader does expand a leading `~` (expandHome, config.go), so by the
	// time a rule reaches this field it is the absolute spelling. It did not
	// until fix round 2, and R6's own `~/mag/**` could never match.
	LaunchDir string

	// Tool is R6's `match.tool`, and a rule that names one matches NOTHING in
	// this build. See the type comment: the term is carried so that a config
	// written for R5.2 loads and narrows rather than broadens, and the day
	// R5.2 ships this is where its match goes.
	Tool string

	// Note is the user's own description, shown in the audit trail so that a
	// line can say which rule authorised an action in the user's words rather
	// than in a reconstructed pattern.
	Note string
}

// Matches reports whether this rule applies to a session.
func (r Rule) Matches(s model.Session) bool {
	if r.Tool != "" {
		// Nothing in this build observes which tool a permission prompt is
		// for, so there is nothing this term could be true of. Refusing here
		// rather than ignoring the term is what keeps the rule no broader
		// than the user wrote it.
		return false
	}
	if r.LaunchDir == "" {
		return false
	}
	return matchPath(r.LaunchDir, s.LaunchDir)
}

// matchPath is the one place a rule's pattern is compared with a path.
//
// Two shapes only: a "/**" suffix meaning "this subtree", and path.Match for
// everything else. A malformed pattern is a non-match rather than an error,
// because the alternative — a rule that errors at decision time — would have
// to either refuse (surprising, if other rules match) or allow (unthinkable).
func matchPath(pattern, dir string) bool {
	if dir == "" {
		return false
	}
	pattern = path.Clean(pattern)
	dir = path.Clean(dir)
	if strings.HasSuffix(pattern, "/**") {
		root := strings.TrimSuffix(pattern, "/**")
		return dir == root || strings.HasPrefix(dir, root+"/")
	}
	ok, err := path.Match(pattern, dir)
	return err == nil && ok
}

// DayMinute is minutes since midnight, local time. It is how a quiet window
// says "22:00" without carrying a date that would be wrong on every other day.
type DayMinute int

// NewDayMinute builds a [DayMinute] from an hour and a minute.
func NewDayMinute(hour, minute int) DayMinute { return DayMinute(hour*60 + minute) }

// String prints HH:MM.
func (d DayMinute) String() string { return fmt.Sprintf("%02d:%02d", int(d)/60%24, int(d)%60) }

// QuietWindow is one entry of requirements.md R6's `notify.quiet`: a span of
// the day during which a notification is suppressed.
//
// A window whose end is before its start WRAPS MIDNIGHT, which is what
// "22:00-08:00" means and is the only reason this type has a method at all. A
// reader that compared two minute counts directly would treat the sketched
// example as an empty window and notify all night.
type QuietWindow struct {
	From DayMinute
	To   DayMinute
}

// Contains reports whether an instant falls inside the window, in the
// instant's own location. The start is inclusive and the end is exclusive, so
// two adjacent windows do not overlap.
func (q QuietWindow) Contains(t time.Time) bool {
	m := DayMinute(t.Hour()*60 + t.Minute())
	if q.From == q.To {
		// A zero-width window is no window. It is the zero value of this
		// struct, and the zero value must not silence everything.
		return false
	}
	if q.From < q.To {
		return m >= q.From && m < q.To
	}
	return m >= q.From || m < q.To
}

// NotifyOn is one trigger from requirements.md R6's `notify.on`.
type NotifyOn uint8

const (
	// NotifyOnUnknown is the zero value: no trigger. It fires for nothing.
	NotifyOnUnknown NotifyOn = iota
	// NotifyOnWaiting is a session that has stopped and is waiting for a
	// human.
	NotifyOnWaiting
	// NotifyOnError is a session that has hit an API-level error.
	NotifyOnError
)

var notifyOnEnum = enumSpec{"NotifyOn", []string{
	"unknown",
	"waiting",
	"error",
}}

// String returns the wire word.
func (n NotifyOn) String() string { return notifyOnEnum.name(int(n)) }

// MarshalJSON writes null for [NotifyOnUnknown] and the wire word otherwise.
func (n NotifyOn) MarshalJSON() ([]byte, error) { return notifyOnEnum.marshal(int(n)) }

// UnmarshalJSON reads null as [NotifyOnUnknown]. An unrecognised name is an
// error.
func (n *NotifyOn) UnmarshalJSON(b []byte) error {
	i, err := notifyOnEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*n = NotifyOn(i)
	return nil
}

// Notify is requirements.md R6's `[notify]` block.
//
// NOTHING IN THIS BUILD READS IT. It is carried so that phase 7's notifier is
// additive — a new consumer of an existing value rather than a change to the
// policy, the gate and every caller that builds one. The two triggers modelled
// are the two the requirement sketches; phase 7 adds whatever its surface
// needs.
//
// The zero value is silence: Enabled false, no triggers, no delay, no quiet
// window. A notifier built from a struct literal in a test therefore says
// nothing, which is the same safe-by-default rule the rest of this package
// follows.
type Notify struct {
	// Enabled is the master switch. False means no notification is ever sent,
	// whatever else is set.
	Enabled bool

	// On is which states raise a notification. Empty means none.
	On []NotifyOn

	// Delay is how long a session must have been in a notifying state before
	// it is announced. Zero is "immediately". It is a duration rather than a
	// string because parsing belongs to the loader.
	Delay time.Duration

	// Quiet is the spans of the day during which nothing is announced.
	Quiet []QuietWindow
}

// Fires reports whether this trigger is in On.
func (n Notify) Fires(on NotifyOn) bool { return n.Enabled && slices.Contains(n.On, on) }

// Silent reports whether an instant falls inside any quiet window.
func (n Notify) Silent(t time.Time) bool {
	for _, q := range n.Quiet {
		if q.Contains(t) {
			return true
		}
	}
	return false
}

// Policy is the whole of requirements.md R6, as a plain Go value.
//
// # It is copied, never shared
//
// v4 decision 7: the policy is a struct copied by value, with no
// atomic.Pointer and no pointer to mutable state. Reads copy; a future hot
// reload replaces a whole value rather than mutating one. The only pointer-ish
// fields are the two slices, which is what [Policy.Clone] is for, and the one
// boundary that must clone is [Load].
//
// # The zero value refuses everything, including navigate
//
// Deliberate, and the single most important property in this package. Every
// unit test in this repository builds its structs as literals, so a permissive
// zero value would be invisible in the tests that would otherwise catch it.
// [Default] — not the zero value — is R6.1's posture, and a caller that wants
// it must say so.
type Policy struct {
	// Source names where this policy came from, and is quoted verbatim in
	// every audit record it decides. An audit line that cannot say what
	// authorised an action does not answer the question the trail exists for,
	// and "which policy was in force" is half of that answer.
	Source string

	// Actions is the stance per tier.
	Actions Actions

	// Rules are evaluated in order, first match wins, and only for a tier
	// whose stance is [StanceRule].
	Rules []Rule

	// Notify is phase 7's, carried here so that it is additive.
	Notify Notify
}

// Default is requirements.md R6.1's posture: navigate freely, everything else
// asks.
//
// "Everything else asks" rather than "everything else never", because `ask` is
// the honest description of a tier that WOULD be available with consent. It
// makes no practical difference in this build — the other three tiers are
// hard-disabled and are refused before any stance is read — and it means the
// day a tier ships, the default is the one the requirement names rather than
// one nobody revisited.
func Default() Policy {
	return Policy{
		Source: "the built-in default posture (R6.1)",
		Actions: Actions{
			Navigate:  StanceAlways,
			Approve:   StanceAsk,
			Send:      StanceAsk,
			Lifecycle: StanceAsk,
		},
	}
}

// Clone returns a copy that shares no slice with the original.
func (p Policy) Clone() Policy {
	c := p
	c.Rules = slices.Clone(p.Rules)
	c.Notify.On = slices.Clone(p.Notify.On)
	c.Notify.Quiet = slices.Clone(p.Notify.Quiet)
	return c
}

// Decision is what the policy decided about one action, and it is what the
// audit record's `policy` object carries.
//
// Rule is never empty. R5.A asks the trail to say by which rule an action
// happened, and a decision that cannot name its own authority is not one; see
// TestEveryDecisionNamesWhatAuthorisedIt, which walks every tier and stance.
type Decision struct {
	// Stance is the stance that decided, for the tier that was asked about.
	Stance Stance `json:"stance"`

	// Rule names what authorised or refused, in words a person can act on:
	// the policy's own source and the line within it, or the matching rule,
	// or the code that hard-disables a tier.
	Rule string `json:"rule"`

	// Allowed is whether the action may proceed. False with Confirm true is
	// "not yet"; false with Confirm false is "no".
	Allowed bool `json:"allowed"`

	// Confirm is the `ask` stance: the caller must obtain consent and ask
	// again. Nothing here prompts.
	Confirm bool `json:"needsConfirmation,omitempty"`
}

// Decide resolves one tier against this policy for one session.
//
// It does NOT consider whether the tier is deferred. That check belongs to the
// gate and happens before this is called, because a deferral is not a policy
// decision — no configuration can change it, and reporting it as a stance
// would suggest one could.
func (p Policy) Decide(t Tier, s model.Session) Decision {
	src := p.Source
	if src == "" {
		// A policy built as a literal with no source is still a policy and
		// still decides; the audit record says that nobody named it rather
		// than leaving the field empty.
		src = "an unnamed policy"
	}
	stance := p.Actions.Of(t)
	switch stance {
	case StanceAlways:
		return Decision{
			Stance:  stance,
			Rule:    fmt.Sprintf("%s: actions.%s = %s", src, t, stance),
			Allowed: true,
		}
	case StanceAsk:
		return Decision{
			Stance:  stance,
			Rule:    fmt.Sprintf("%s: actions.%s = %s", src, t, stance),
			Confirm: true,
		}
	case StanceRule:
		for i, r := range p.Rules {
			if r.Do != t || !r.Matches(s) {
				continue
			}
			note := r.Note
			if note == "" {
				note = fmt.Sprintf("launchDir matches %q", r.LaunchDir)
			}
			return Decision{
				Stance:  stance,
				Rule:    fmt.Sprintf("%s: actions.rules[%d] — %s", src, i, note),
				Allowed: true,
			}
		}
		return Decision{
			Stance: stance,
			Rule: fmt.Sprintf("%s: actions.%s = rule, and none of the %d rules match this session",
				src, t, len(p.Rules)),
		}
	case StanceNever:
		return Decision{
			Stance: stance,
			Rule:   fmt.Sprintf("%s: actions.%s = never", src, t),
		}
	default:
		return Decision{
			Stance: StanceUnknown,
			Rule: fmt.Sprintf("%s: actions.%s is unset, and an unset stance refuses",
				src, t),
		}
	}
}

// Loader is where a policy comes from. It is an injected function and not a
// path, because there is no file format in this build: phase 7 adds the file,
// its syntax and the concrete unparseable case, and it does so by writing one
// of these.
//
// A loader that succeeds owns the WHOLE policy. [Load] does not merge its
// result with [Default], so a loader that reads a partial file should start
// from Default and overlay what the file said — in one place, where a reader
// can see which keys defaulted.
type Loader func() (Policy, error)

// Load resolves the policy, and is requirements.md R6.3 expressed as a
// property of the loader rather than of a parser:
//
//	an absent or failing loader yields the DEFAULT posture, never a
//	more-permissive one.
//
// The returned policy is ALWAYS usable and always safe. The error is
// informational and beside it, so a caller that ignores the error still gets
// the safe posture and a caller that reports it can say why the file it
// expected is not in force. A degradation that nobody can see is the failure
// NFR4 names.
//
// Three cases, and all three are tested:
//
//   - nil loader — there is no configuration in this build. Default.
//   - loader returns an error — unreadable, unparseable, whatever phase 7's
//     format can fail at. Default, and the loaded value is discarded
//     ENTIRELY rather than partially applied: half a config is a posture
//     nobody wrote.
//   - loader succeeds — that policy, cloned, so the caller's own slices
//     cannot be mutated underneath a decision.
func Load(l Loader) (Policy, error) {
	if l == nil {
		return Default(), nil
	}
	p, err := l()
	if err != nil {
		return Default(), fmt.Errorf("reading the action policy, so the default posture is in force: %w", err)
	}
	return p.Clone(), nil
}
