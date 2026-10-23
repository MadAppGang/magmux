package act

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// TestTheZeroPolicyRefusesEverythingIncludingNavigate is the most important
// test in this package, and it is written directly rather than as a side
// effect of something else BECAUSE a permissive zero value is invisible in
// every other test.
//
// A WRONG IMPLEMENTATION puts StanceAlways at index 0, or has Decide treat an
// unset stance as "allowed unless told otherwise". Every unit test in this
// repository builds its structs as literals, so `act.Policy{}` appears
// everywhere and says nothing about policy — and with a permissive zero value
// every one of those tests keeps passing while a pipeline built by a caller
// who never configured anything silently authorises actions. Nothing else
// catches it. This does.
func TestTheZeroPolicyRefusesEverythingIncludingNavigate(t *testing.T) {
	var zero Policy
	for _, tier := range []Tier{TierUnknown, TierNavigate, TierApprove, TierSend, TierLifecycle} {
		d := zero.Decide(tier, model.Session{})
		if d.Allowed {
			t.Errorf("the ZERO policy allows %s — a struct literal that says nothing about "+
				"policy must not be able to act", tier)
		}
		if d.Confirm {
			t.Errorf("the ZERO policy offers %s on confirmation; an unset stance is not an "+
				"invitation", tier)
		}
		if d.Stance != StanceUnknown {
			t.Errorf("the ZERO policy's stance for %s is %s, want unset", tier, d.Stance)
		}
	}

	// And the enum itself: index 0 must be the refusing one, because that is
	// what makes every zero-valued Actions field safe.
	if StanceUnknown != 0 {
		t.Fatal("StanceUnknown is not the zero value, so a zero Actions is not the safe posture")
	}
	if Stance(0).String() == StanceAlways.String() {
		t.Fatal("the zero stance and `always` are the same value")
	}
}

// TestDefaultIsNavigateFreelyAndEverythingElseAsks — requirements.md R6.1,
// asserted as written, because "the default" is the posture a machine with no
// configuration runs under and it is the one nobody will re-read the code for.
func TestDefaultIsNavigateFreelyAndEverythingElseAsks(t *testing.T) {
	p := Default()
	if got := p.Actions.Of(TierNavigate); got != StanceAlways {
		t.Errorf("the default navigate stance is %s, want always", got)
	}
	for _, tier := range []Tier{TierApprove, TierSend, TierLifecycle} {
		if got := p.Actions.Of(tier); got != StanceAsk {
			t.Errorf("the default %s stance is %s, want ask", tier, got)
		}
	}
	if p.Source == "" {
		t.Error("the default posture does not name itself, so no audit line can say what " +
			"authorised an action under it")
	}
	// The notify block is carried and is SILENT. Phase 7 reads it; a default
	// that announced things nobody asked for would be found the hard way, on
	// a machine with thirty sessions.
	if p.Notify.Enabled || len(p.Notify.On) != 0 {
		t.Error("the default posture notifies about something")
	}
}

// TestEveryDecisionNamesWhatAuthorisedIt walks every tier against every
// stance and insists the decision can say what decided it.
//
// R5.A asks the trail to record BY WHICH RULE an action happened. A decision
// that cannot name its own authority makes that impossible at the only place
// it could ever be known, and the audit writer refuses such a record — so a
// gap here would turn into a refused action rather than a bad log line, which
// is why this is checked at the source.
func TestEveryDecisionNamesWhatAuthorisedIt(t *testing.T) {
	stances := []Stance{StanceUnknown, StanceNever, StanceAsk, StanceRule, StanceAlways}
	tiers := []Tier{TierNavigate, TierApprove, TierSend, TierLifecycle}
	for _, stance := range stances {
		for _, tier := range tiers {
			p := Policy{Source: "the test's policy"}
			switch tier {
			case TierNavigate:
				p.Actions.Navigate = stance
			case TierApprove:
				p.Actions.Approve = stance
			case TierSend:
				p.Actions.Send = stance
			case TierLifecycle:
				p.Actions.Lifecycle = stance
			}
			d := p.Decide(tier, model.Session{})
			if strings.TrimSpace(d.Rule) == "" {
				t.Errorf("%s at stance %s decided with no rule named", tier, stance)
			}
			if d.Stance != stance {
				t.Errorf("%s at stance %s reported stance %s", tier, stance, d.Stance)
			}
			if stance == StanceAlways && !d.Allowed {
				t.Errorf("%s at always was not allowed", tier)
			}
			if stance != StanceAlways && d.Allowed {
				t.Errorf("%s at %s was allowed", tier, stance)
			}
		}
	}

	// A policy nobody named still decides, and says that nobody named it.
	// An empty rule would be refused by the writer; an honest one is not.
	if d := (Policy{}).Decide(TierNavigate, model.Session{}); !strings.Contains(d.Rule, "unnamed") {
		t.Errorf("an unnamed policy's rule is %q", d.Rule)
	}
}

// TestRuleStanceAllowsOnlyAMatchingSession — R6.2: nothing acts unattended
// without a rule the user wrote.
//
// The empty-pattern case is the one to watch. A WRONG IMPLEMENTATION treats a
// missing match clause as "match everything", which turns a half-written rule
// into blanket permission for a whole tier.
func TestRuleStanceAllowsOnlyAMatchingSession(t *testing.T) {
	p := Policy{
		Source:  "the test's policy",
		Actions: Actions{Navigate: StanceRule},
		Rules: []Rule{
			{Do: TierNavigate, LaunchDir: "/Users/j/mag/**", Note: "anything under mag"},
		},
	}

	for _, tc := range []struct {
		name      string
		launchDir string
		want      bool
	}{
		{"inside the subtree", "/Users/j/mag/magmux", true},
		{"the subtree root itself", "/Users/j/mag", true},
		{"a sibling directory", "/Users/j/other", false},
		{"a prefix that is not a path boundary", "/Users/j/magnificent", false},
		{"no launch directory at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := p.Decide(TierNavigate, model.Session{LaunchDir: tc.launchDir})
			if d.Allowed != tc.want {
				t.Fatalf("allowed=%v want %v (rule: %s)", d.Allowed, tc.want, d.Rule)
			}
			if tc.want && !strings.Contains(d.Rule, "anything under mag") {
				t.Errorf("the authorising rule is not named in %q", d.Rule)
			}
		})
	}

	t.Run("a rule with no match clause matches nothing", func(t *testing.T) {
		open := Policy{
			Source:  "the test's policy",
			Actions: Actions{Navigate: StanceRule},
			Rules:   []Rule{{Do: TierNavigate}},
		}
		if d := open.Decide(TierNavigate, model.Session{LaunchDir: "/anywhere"}); d.Allowed {
			t.Fatal("a rule with no match clause authorised a session; an absent pattern " +
				"must match NOTHING, or a half-written rule is blanket permission")
		}
	})

	t.Run("a rule for another tier does not authorise this one", func(t *testing.T) {
		crossed := Policy{
			Source:  "the test's policy",
			Actions: Actions{Navigate: StanceRule},
			Rules:   []Rule{{Do: TierSend, LaunchDir: "/Users/j/mag/**"}},
		}
		if d := crossed.Decide(TierNavigate, model.Session{LaunchDir: "/Users/j/mag/x"}); d.Allowed {
			t.Fatal("a send rule authorised a navigate")
		}
	})
}

// TestLoadAlwaysYieldsAUsableAndSafePolicy is requirements.md R6.3, expressed
// as a property of the LOADER rather than of a parser — which is what makes it
// testable in a build that has no file format at all.
//
// A WRONG IMPLEMENTATION returns the zero Policy on error (safe, but then the
// default posture is lost and navigate stops working the moment a config file
// is malformed), or returns whatever the loader managed to produce before it
// failed (which is a posture nobody wrote).
func TestLoadAlwaysYieldsAUsableAndSafePolicy(t *testing.T) {
	t.Run("no loader at all is the default posture", func(t *testing.T) {
		p, err := Load(nil)
		if err != nil {
			t.Fatalf("Load(nil) failed: %v", err)
		}
		if p.Actions.Of(TierNavigate) != StanceAlways {
			t.Fatalf("Load(nil) did not yield the default posture: %+v", p.Actions)
		}
	})

	t.Run("a failing loader is the default posture, and says so", func(t *testing.T) {
		boom := errors.New("line 3: what is a lifecycle")
		p, err := Load(func() (Policy, error) {
			// A partially built, MORE PERMISSIVE value, returned beside the
			// error. It must be discarded entirely.
			return Policy{Actions: Actions{Lifecycle: StanceAlways}}, boom
		})
		if err == nil {
			t.Fatal("a failing loader reported no error; a degradation nobody can see is the " +
				"failure NFR4 names")
		}
		if !errors.Is(err, boom) {
			t.Errorf("the underlying error was not preserved: %v", err)
		}
		if p.Actions.Of(TierLifecycle) != StanceAsk {
			t.Fatalf("the failing loader's permissive value survived: lifecycle is %s",
				p.Actions.Of(TierLifecycle))
		}
		if p.Actions.Of(TierNavigate) != StanceAlways {
			t.Fatalf("a failing loader lost the default posture: navigate is %s",
				p.Actions.Of(TierNavigate))
		}
	})

	t.Run("a loader that succeeds owns the whole policy", func(t *testing.T) {
		p, err := Load(func() (Policy, error) {
			return Policy{Source: "a file", Actions: Actions{Navigate: StanceNever}}, nil
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if p.Actions.Of(TierNavigate) != StanceNever {
			t.Fatal("a successful loader's stance was overridden by the default")
		}
	})

	t.Run("the loaded value is cloned, so the loader cannot mutate a decision", func(t *testing.T) {
		rules := []Rule{{Do: TierNavigate, LaunchDir: "/a/**"}}
		p, err := Load(func() (Policy, error) {
			return Policy{Source: "a file", Actions: Actions{Navigate: StanceRule}, Rules: rules}, nil
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		rules[0].LaunchDir = "/**"
		if d := p.Decide(TierNavigate, model.Session{LaunchDir: "/somewhere/else"}); d.Allowed {
			t.Fatal("mutating the caller's slice changed a loaded policy's decision")
		}
	})
}

// TestQuietWindowWrapsMidnight — "22:00-08:00" is the example the requirement
// gives, and a reader that compares two minute counts directly treats it as an
// EMPTY window and notifies all night.
func TestQuietWindowWrapsMidnight(t *testing.T) {
	night := QuietWindow{From: NewDayMinute(22, 0), To: NewDayMinute(8, 0)}
	day := QuietWindow{From: NewDayMinute(9, 0), To: NewDayMinute(17, 0)}

	at := func(h, m int) time.Time { return time.Date(2026, 9, 18, h, m, 0, 0, time.Local) }

	for _, tc := range []struct {
		name string
		win  QuietWindow
		when time.Time
		want bool
	}{
		{"just after the wrapping start", night, at(22, 1), true},
		{"the small hours", night, at(3, 0), true},
		{"just before the wrapping end", night, at(7, 59), true},
		{"the end is exclusive", night, at(8, 0), false},
		{"the middle of the day", night, at(13, 0), false},
		{"inside a plain window", day, at(12, 0), true},
		{"outside a plain window", day, at(20, 0), false},
		{"a zero-width window silences nothing", QuietWindow{}, at(0, 0), false},
	} {
		if got := tc.win.Contains(tc.when); got != tc.want {
			t.Errorf("%s: Contains(%s) is %v, want %v", tc.name, tc.when.Format("15:04"), got, tc.want)
		}
	}
}

// TestTheZeroNotifyBlockIsSilence — the same zero-value rule as the policy,
// for the block phase 7 will read. A notifier built from a struct literal must
// announce nothing.
func TestTheZeroNotifyBlockIsSilence(t *testing.T) {
	var n Notify
	if n.Fires(NotifyOnWaiting) || n.Fires(NotifyOnError) {
		t.Error("the zero notify block fires")
	}
	if n.Silent(time.Now()) {
		t.Error("the zero notify block has a quiet window")
	}
	// Enabled alone is not enough: the trigger list is the other half, and a
	// switch that notified about everything the moment it was turned on would
	// be unusable with thirty sessions.
	n.Enabled = true
	if n.Fires(NotifyOnWaiting) {
		t.Error("an enabled notify block with no triggers fires anyway")
	}
	n.On = []NotifyOn{NotifyOnWaiting}
	if !n.Fires(NotifyOnWaiting) || n.Fires(NotifyOnError) {
		t.Error("the trigger list is not honoured")
	}
}
