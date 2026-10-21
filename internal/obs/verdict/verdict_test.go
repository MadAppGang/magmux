package verdict

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/termev"
)

// NOW is the fixed instant every expectation here is written against. Nothing
// in this package reads a clock: the instant arrives as an argument, which is
// what lets a five-minute silence be tested without waiting five minutes and
// without shrinking the threshold to something magmux does not ship.
var NOW = time.Unix(1789545200, 0).UTC()

// since builds a StatusSince d before NOW.
func since(d time.Duration) *time.Time {
	t := NOW.Add(-d)
	return &t
}

// quiet is a screen that has produced nothing for d.
func quiet(d time.Duration) Screen { return Screen{Known: true, Quiet: d} }

// needed renders an attention verdict the way a failure message wants it, with
// the three states spelled apart.
func needed(a obs.Attention) string {
	switch {
	case a.Needed == nil:
		return "nil"
	case *a.Needed:
		return "true"
	default:
		return "false"
	}
}

// ── the two tiers ───────────────────────────────────────────────────────────

// TestAuthoritativeWinsOverInferred is the rule the whole resolution rests on.
//
// A WRONG IMPLEMENTATION lets the inferred tier overwrite the record's status
// when the screen is more recent, or picks whichever tier spoke last. Both
// publish a guess made from the outside in place of the tool's own record of
// itself, which is the one thing "authoritative" means.
func TestAuthoritativeWinsOverInferred(t *testing.T) {
	for _, tt := range []struct {
		name   string
		auth   obs.Status
		screen Screen
	}{
		{"busy_record_silent_screen", obs.StatusBusy, quiet(10 * time.Minute)},
		{"idle_record_live_screen", obs.StatusIdle, quiet(0)},
		{"waiting_record_silent_screen", obs.StatusWaiting, quiet(time.Hour)},
		{"shell_record_live_screen", obs.StatusShell, quiet(time.Second)},
		{"busy_record_typed_into", obs.StatusBusy, Screen{Known: true, Typed: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(obs.Session{Status: tt.auth}, tt.screen, NOW)
			if got.Status != tt.auth {
				t.Errorf("published status %v, want the record's %v", got.Status, tt.auth)
			}
			if got.Confidence != obs.TierAuthoritative {
				t.Errorf("confidence %v, want authoritative", got.Confidence)
			}
		})
	}
}

// TestTheLosingTierIsPublishedAsAConflict is the refinement this phase exists
// for: resolution without publication is silent.
//
// A WRONG IMPLEMENTATION resolves correctly and DROPS the loser, so a consumer
// cannot tell an agreed answer from an overruled one — and the overruled case
// is precisely the one a human is being asked to look at.
func TestTheLosingTierIsPublishedAsAConflict(t *testing.T) {
	got := Resolve(obs.Session{Status: obs.StatusBusy}, quiet(5*time.Minute), NOW)

	if got.Conflict == nil {
		t.Fatal("the record said busy and the pane had been silent for five minutes; " +
			"the losing tier was dropped instead of published")
	}
	if got.Conflict.Tier != obs.TierInferred {
		t.Errorf("conflict names tier %v, want inferred", got.Conflict.Tier)
	}
	if got.Conflict.Status != obs.StatusIdle {
		t.Errorf("conflict carries status %v, want the idle the screen reported", got.Conflict.Status)
	}
	if !strings.Contains(got.Conflict.Detail, "5m 0s") {
		t.Errorf("conflict detail %q does not say how long the pane has been silent", got.Conflict.Detail)
	}
	// The winner is still the winner.
	if got.Status != obs.StatusBusy || got.Confidence != obs.TierAuthoritative {
		t.Errorf("published %v/%v, want busy/authoritative", got.Status, got.Confidence)
	}
}

// TestAConflictLeavesAttentionUndetermined is the third state doing the work
// it exists for.
//
// A WRONG IMPLEMENTATION answers false, which hides a wedged session behind
// "we looked, it is fine", or answers true, which cries wolf over every long
// tool call. Neither is supported by the evidence: the two tiers disagree, and
// that is what nil says.
func TestAConflictLeavesAttentionUndetermined(t *testing.T) {
	got := Resolve(obs.Session{Status: obs.StatusBusy}, quiet(30*time.Minute), NOW)

	if got.Attention.Needed != nil {
		t.Fatalf("attention is %s, want nil — the two tiers disagree and neither verdict is "+
			"supported", needed(got.Attention))
	}
	for _, want := range []string{"record", "working", "30m 0s"} {
		if !strings.Contains(got.Attention.Reason, want) {
			t.Errorf("reason %q does not mention %q; a nil verdict has to explain itself",
				got.Attention.Reason, want)
		}
	}
}

// TestSilenceIsNotAConflictWithAnythingButBusy — a quiet screen is what an
// idle session, a permission prompt and a session waiting on input all look
// like.
//
// A WRONG IMPLEMENTATION compares the two tiers' status words for equality, so
// every waiting session on the machine carries a permanent conflict and the
// field stops meaning anything.
func TestSilenceIsNotAConflictWithAnythingButBusy(t *testing.T) {
	for _, status := range []obs.Status{obs.StatusIdle, obs.StatusWaiting, obs.StatusShell} {
		t.Run(status.String(), func(t *testing.T) {
			got := Resolve(obs.Session{Status: status}, quiet(time.Hour), NOW)
			if got.Conflict != nil {
				t.Fatalf("a %v session with a silent pane raised a conflict: %+v",
					status, *got.Conflict)
			}
		})
	}
}

// TestOutputArrivingIsNeverAConflict pins the asymmetry, which is the half a
// reviewer is most likely to call a bug.
//
// A WRONG IMPLEMENTATION makes the rule symmetric — "the tiers differ, either
// way" — and then raises a conflict against every completed turn on the
// machine for the five seconds between Claude writing "idle" and its pane
// finishing painting.
func TestOutputArrivingIsNeverAConflict(t *testing.T) {
	for _, status := range []obs.Status{obs.StatusIdle, obs.StatusWaiting, obs.StatusShell} {
		t.Run(status.String(), func(t *testing.T) {
			got := Resolve(obs.Session{Status: status}, quiet(100*time.Millisecond), NOW)
			if got.Conflict != nil {
				t.Fatalf("a %v record with a live screen raised a conflict: %+v",
					status, *got.Conflict)
			}
		})
	}
}

// TestAConflictNeedsAFullMinuteOfSilence pins the threshold as a rung, at its
// boundary.
//
// A WRONG IMPLEMENTATION raises the conflict at the first rung, five seconds,
// and then fires against every session doing a slow tool call.
func TestAConflictNeedsAFullMinuteOfSilence(t *testing.T) {
	for _, tt := range []struct {
		quiet time.Duration
		want  bool
	}{
		{0, false}, {QuietIdle, false}, {30 * time.Second, false},
		{QuietConflict - time.Millisecond, false},
		{QuietConflict, true}, {time.Hour, true},
	} {
		t.Run(tt.quiet.String(), func(t *testing.T) {
			got := Resolve(obs.Session{Status: obs.StatusBusy}, quiet(tt.quiet), NOW)
			if (got.Conflict != nil) != tt.want {
				t.Fatalf("silence of %s: conflict=%v, want %v", tt.quiet, got.Conflict != nil, tt.want)
			}
		})
	}
}

// TestTypedIntoIsNotSilence — input CANCELS the measurement rather than
// restarting it, so a pane a human just typed into can never contradict its
// own record.
//
// A WRONG IMPLEMENTATION treats input as another kind of activity and measures
// the silence from it, which reports a pane as stale when what it is really
// doing is waiting for a program to answer a human.
func TestTypedIntoIsNotSilence(t *testing.T) {
	got := Resolve(obs.Session{Status: obs.StatusBusy}, Screen{Known: true, Typed: true}, NOW)
	if got.Conflict != nil {
		t.Fatalf("a pane that was just typed into contradicted its record: %+v", *got.Conflict)
	}
}

// TestTheInferredTierWinsOnlyWhenNothingElseSpoke.
//
// A WRONG IMPLEMENTATION either never lets the inferred tier publish — so a
// session with no readable record is blank rather than described — or lets it
// publish over a record magmux merely failed to DECODE, which throws away the
// only truth available.
func TestTheInferredTierWinsOnlyWhenNothingElseSpoke(t *testing.T) {
	t.Run("no record at all", func(t *testing.T) {
		got := Resolve(obs.Session{}, quiet(time.Minute), NOW)
		if got.Status != obs.StatusIdle || got.Confidence != obs.TierInferred {
			t.Fatalf("published %v/%v, want idle/inferred", got.Status, got.Confidence)
		}
		if got.Conflict != nil {
			t.Errorf("one tier speaking alone raised a conflict: %+v", *got.Conflict)
		}
	})

	t.Run("a record this build cannot decode", func(t *testing.T) {
		got := Resolve(obs.Session{StatusRaw: "compacting"}, quiet(time.Hour), NOW)
		if got.Status != obs.StatusUnknown {
			t.Fatalf("status %v — an undecodable record was replaced by a guess", got.Status)
		}
		if got.Confidence != obs.TierUnknown {
			t.Errorf("confidence %v, want unknown", got.Confidence)
		}
		if got.StatusRaw != "compacting" {
			t.Errorf("the raw word was lost: %q", got.StatusRaw)
		}
	})
}

// TestTheInferredTierNeverGuessesWaitingOrShell — it answers one question, and
// its vocabulary is poorer than the one it maps into on purpose.
//
// A WRONG IMPLEMENTATION maps a long silence onto "waiting", which is the most
// useful word and the one the evidence cannot support: a quiet screen is what
// idle, waiting-on-input and a permission prompt all look like.
func TestTheInferredTierNeverGuessesWaitingOrShell(t *testing.T) {
	for _, sc := range []Screen{
		{}, {Known: true}, {Known: true, Typed: true},
		quiet(0), quiet(time.Second), quiet(QuietIdle), quiet(time.Hour), quiet(-time.Second),
	} {
		got := sc.Status()
		if got == obs.StatusWaiting || got == obs.StatusShell {
			t.Fatalf("screen %+v inferred %v, which no terminal evidence can support", sc, got)
		}
	}
}

// TestTheScreenMaps reads the inferred tier's whole table.
func TestTheScreenMaps(t *testing.T) {
	for _, tt := range []struct {
		name   string
		screen Screen
		want   obs.Status
	}{
		{"nothing known", Screen{}, obs.StatusUnknown},
		{"typed into", Screen{Known: true, Typed: true}, obs.StatusUnknown},
		{"bytes arriving", quiet(0), obs.StatusBusy},
		{"just under the first rung", quiet(QuietIdle - time.Millisecond), obs.StatusBusy},
		{"on the first rung", quiet(QuietIdle), obs.StatusIdle},
		{"long silence", quiet(time.Hour), obs.StatusIdle},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.screen.Status(); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// ── the attention policy ────────────────────────────────────────────────────

// TestTheAttentionPolicy is the policy itself, read as a table, in the order
// the package doc states the rules. It is the single test that would have to
// be rewritten for the policy to change, which is the point: the rule lives in
// the doc, this asserts the doc, and everything else asserts a corner of it.
func TestTheAttentionPolicy(t *testing.T) {
	// The three spellings the want column uses, named so that a typo in one
	// row is a compile error rather than a silently unreachable expectation.
	const (
		yes   = "true"
		nope  = "false"
		dunno = "nil"
	)
	for _, tt := range []struct {
		name    string
		session obs.Session
		screen  Screen
		want    string
		reason  []string
	}{
		// 1 — a gate said no.
		{
			"rule1 dead process", obs.Session{Status: obs.StatusWaiting, ProcessAlive: obs.LivenessNo},
			Screen{}, nope, []string{"process has exited"},
		},
		{
			"rule1 closed pane", obs.Session{Status: obs.StatusWaiting, PaneAlive: obs.LivenessNo},
			Screen{}, nope, []string{"pane was closed"},
		},
		{
			"rule1 both",
			obs.Session{Status: obs.StatusWaiting, ProcessAlive: obs.LivenessNo, PaneAlive: obs.LivenessNo},
			Screen{}, nope, []string{"process has exited", "pane was closed"},
		},
		// 2 — waiting, with the two reasons kept apart.
		{
			"rule2 permission",
			obs.Session{
				Status: obs.StatusWaiting, Waiting: obs.WaitingReasonPermission,
				ProcessAlive: obs.LivenessYes, StatusSince: since(3 * time.Minute),
			},
			// The expectation is magmux's OWN sentence, deliberately not the
			// bare string Claude writes: NFR6's decode detector flags a
			// literal equal to one of Claude's values anywhere outside
			// schema.go, and it flagged this line when it was written that
			// way. The rule earning its keep in a test assertion is the rule
			// working.
			Screen{}, yes, []string{"holding a permission prompt open", "for 3m 0s"},
		},
		{
			"rule2 input",
			obs.Session{
				Status: obs.StatusWaiting, Waiting: obs.WaitingReasonInput,
				ProcessAlive: obs.LivenessYes,
			},
			Screen{}, yes, []string{"waiting for you to type"},
		},
		{
			"rule2 waiting for nothing this build knows",
			obs.Session{Status: obs.StatusWaiting, WaitingRaw: "thinking about it"},
			Screen{}, yes, []string{"thinking about it"},
		},
		{
			"rule2 waiting, reason undetermined",
			obs.Session{Status: obs.StatusWaiting}, Screen{}, yes, []string{"not determined"},
		},
		// 3 — an API-level error.
		{
			"rule3 one error",
			obs.Session{Status: obs.StatusIdle, Fault: obs.Fault{APIErrors: obs.Ptr(1), Detail: "quota exhausted"}},
			Screen{}, yes, []string{"an API-level error", "quota exhausted"},
		},
		{
			"rule3 several errors",
			obs.Session{Status: obs.StatusBusy, Fault: obs.Fault{APIErrors: obs.Ptr(4)}},
			Screen{}, yes, []string{"4 API-level errors"},
		},
		{
			"rule3 a determined zero is not an error",
			obs.Session{Status: obs.StatusIdle, Fault: obs.Fault{APIErrors: obs.Ptr(0)}},
			Screen{}, nope, []string{"nothing outstanding"},
		},
		{
			"rule3 an uncounted error is not an error",
			obs.Session{Status: obs.StatusIdle}, Screen{}, nope, []string{"nothing outstanding"},
		},
		// 4 — the tiers conflict.
		{
			"rule4 conflict", obs.Session{Status: obs.StatusBusy}, quiet(5 * time.Minute),
			dunno, []string{"record says it is working", "5m 0s", "cannot be told"},
		},
		// 5 — busy.
		{
			"rule5 busy", obs.Session{Status: obs.StatusBusy, StatusSince: since(90 * time.Second)},
			quiet(time.Second), nope, []string{"it is working", "for 1m 30s"},
		},
		// 6 — idle or shell, authoritative.
		{
			"rule6 idle", obs.Session{Status: obs.StatusIdle, StatusSince: since(72 * time.Hour)},
			quiet(72 * time.Hour), nope, []string{"nothing outstanding", "for 72h 0m"},
		},
		{
			"rule6 shell", obs.Session{Status: obs.StatusShell}, quiet(time.Hour), nope,
			[]string{"handed its terminal to a shell"},
		},
		// 7 — the shell says it is at a prompt. The generic tier's one
		// determined answer; the rows are in shell_test.go, which owns the
		// evidence, and this is the table entry that keeps the policy's ORDER
		// readable in one place.
		{
			"rule7 at a prompt", obs.Session{}, atPrompt(5 * time.Minute), nope,
			[]string{"shell is at a prompt"},
		},
		// 8 — idle on the inferred tier alone.
		{
			"rule8 inferred idle", obs.Session{}, quiet(5 * time.Minute), dunno,
			[]string{"quiet for 5m 0s", "cannot be told"},
		},
		{
			"rule8 does not apply to inferred busy", obs.Session{}, quiet(0), nope,
			[]string{"it is working"},
		},
		// 9 — a command is running and the pane is silent.
		{
			"rule9 silent command", obs.Session{}, runningFor(5 * time.Minute), dunno,
			[]string{"a command is running", "cannot be told"},
		},
		// 10 — nothing known.
		{
			"rule10 undecodable word", obs.Session{StatusRaw: "compacting"}, Screen{}, dunno,
			[]string{`"compacting"`},
		},
		{"rule10 nothing at all", obs.Session{}, Screen{}, dunno, []string{"nothing is known"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.session, tt.screen, NOW)
			if n := needed(got.Attention); n != tt.want {
				t.Fatalf("attention %s, want %s (reason %q)", n, tt.want, got.Attention.Reason)
			}
			for _, want := range tt.reason {
				if !strings.Contains(got.Attention.Reason, want) {
					t.Errorf("reason %q does not contain %q", got.Attention.Reason, want)
				}
			}
		})
	}
}

// TestAnUndeterminedGateNeverRejects.
//
// A WRONG IMPLEMENTATION treats "nobody could tell" as "no" and answers "this
// session does not need anyone" about a session nothing has been able to look
// at — the exact failure the tri-state exists to prevent, and the reason both
// gates are three-valued rather than bool.
func TestAnUndeterminedGateNeverRejects(t *testing.T) {
	s := obs.Session{
		Status: obs.StatusWaiting, Waiting: obs.WaitingReasonPermission,
		ProcessAlive: obs.LivenessUnknown, PaneAlive: obs.LivenessUnknown,
	}
	got := Resolve(s, Screen{}, NOW)
	if got.Attention.Needed == nil || !*got.Attention.Needed {
		t.Fatalf("attention %s for a waiting session whose gates are undetermined, want true "+
			"(reason %q)", needed(got.Attention), got.Attention.Reason)
	}
}

// TestALiveGateNeverRejects — the other half: LivenessYes must not be read as
// a rejection by an implementation that tests for "not LivenessNo" backwards.
func TestALiveGateNeverRejects(t *testing.T) {
	s := obs.Session{
		Status: obs.StatusWaiting, Waiting: obs.WaitingReasonInput,
		ProcessAlive: obs.LivenessYes, PaneAlive: obs.LivenessYes,
	}
	if got := Resolve(s, Screen{}, NOW); got.Attention.Needed == nil || !*got.Attention.Needed {
		t.Fatalf("attention %s for a live waiting session, want true", needed(got.Attention))
	}
}

// TestEveryVerdictCarriesAReason, and an undetermined one most of all.
//
// A WRONG IMPLEMENTATION leaves Reason empty on the nil path, which makes "we
// cannot tell, and here is why" indistinguishable from "this field was never
// filled in" — and the second is the thing the whole model is written against.
func TestEveryVerdictCarriesAReason(t *testing.T) {
	for _, tt := range []struct {
		name    string
		session obs.Session
		screen  Screen
	}{
		{"gate", obs.Session{ProcessAlive: obs.LivenessNo}, Screen{}},
		{"waiting", obs.Session{Status: obs.StatusWaiting}, Screen{}},
		{"error", obs.Session{Fault: obs.Fault{APIErrors: obs.Ptr(2)}}, Screen{}},
		{"conflict", obs.Session{Status: obs.StatusBusy}, quiet(time.Hour)},
		{"busy", obs.Session{Status: obs.StatusBusy}, Screen{}},
		{"idle", obs.Session{Status: obs.StatusIdle}, Screen{}},
		{"shell", obs.Session{Status: obs.StatusShell}, Screen{}},
		{"inferred idle", obs.Session{}, quiet(time.Hour)},
		{"raw word", obs.Session{StatusRaw: "compacting"}, Screen{}},
		{"nothing", obs.Session{}, Screen{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.session, tt.screen, NOW); got.Attention.Reason == "" {
				t.Fatalf("a %s verdict (%s) carries no reason", tt.name, needed(got.Attention))
			}
		})
	}
}

// TestNilIsReachableAndSoAreTheOtherTwo.
//
// A WRONG IMPLEMENTATION collapses nil into false — which is exactly what a
// *bool exists to prevent, and which every other test here would have to be
// wrong for as well. This one states the property on its own so the failure
// names it.
func TestNilIsReachableAndSoAreTheOtherTwo(t *testing.T) {
	seen := map[string]string{}
	for _, tt := range []struct {
		name    string
		session obs.Session
		screen  Screen
	}{
		{"waiting", obs.Session{Status: obs.StatusWaiting}, Screen{}},
		{"idle", obs.Session{Status: obs.StatusIdle}, Screen{}},
		{"conflict", obs.Session{Status: obs.StatusBusy}, quiet(time.Hour)},
		{"unexamined", obs.Session{}, Screen{}},
	} {
		seen[needed(Resolve(tt.session, tt.screen, NOW).Attention)] = tt.name
	}
	for _, want := range []string{"true", "false", "nil"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("no input produced attention %s; the three states are not all reachable "+
				"(reached: %v)", want, seen)
		}
	}
}

// ── invariants ──────────────────────────────────────────────────────────────

// TestEveryThresholdTheVerdictTurnsOnIsARungOnTheLadder.
//
// A WRONG IMPLEMENTATION picks a threshold that is not a rung — 45 seconds,
// say — and then the verdict CHANGES at a moment no alarm is armed for. The
// session's answer would be stale until something unrelated caused a cycle,
// and the bug would look like "the table is sometimes right".
func TestEveryThresholdTheVerdictTurnsOnIsARungOnTheLadder(t *testing.T) {
	ladder := termev.DefaultLadder()
	on := ladder[0]
	if QuietIdle != on {
		t.Errorf("QuietIdle is %s but the ladder's first rung is %s", QuietIdle, on)
	}
	found := false
	for _, rung := range ladder {
		if rung == QuietConflict {
			found = true
		}
	}
	if !found {
		t.Errorf("QuietConflict is %s, which is not a rung on %v", QuietConflict, ladder)
	}
}

// TestResolveDoesNotMutateItsArgument. [Resolve] takes and returns a value, and
// the actor clones the result afterwards — but a stage that wrote THROUGH a
// pointer it was handed would corrupt a fact set that has already been
// published.
func TestResolveDoesNotMutateItsArgument(t *testing.T) {
	in := obs.Session{
		Status: obs.StatusBusy, Attention: obs.Attention{Needed: obs.Ptr(true), Reason: "upstream"},
		Conflict: &obs.Conflict{Tier: obs.TierInferred, Status: obs.StatusIdle, Detail: "upstream"},
	}
	Resolve(in, quiet(time.Hour), NOW)
	if in.Attention.Needed == nil || !*in.Attention.Needed || in.Attention.Reason != "upstream" {
		t.Errorf("Resolve wrote through the attention it was handed: %+v", in.Attention)
	}
	if in.Conflict == nil || in.Conflict.Detail != "upstream" {
		t.Errorf("Resolve wrote through the conflict it was handed: %+v", in.Conflict)
	}
}

// TestAStaleConflictIsCleared — the stage is called again and again for one
// session, and a conflict that was true a minute ago must not survive the
// observation that disproves it.
func TestAStaleConflictIsCleared(t *testing.T) {
	stale := obs.Session{
		Status:   obs.StatusBusy,
		Conflict: &obs.Conflict{Tier: obs.TierInferred, Status: obs.StatusIdle, Detail: "an hour ago"},
	}
	if got := Resolve(stale, quiet(time.Second), NOW); got.Conflict != nil {
		t.Fatalf("a conflict survived an observation that disproved it: %+v", *got.Conflict)
	}
}

// TestResolveIsDeterministic. The same evidence must produce the same verdict,
// because a document is meant to be diffed.
func TestResolveIsDeterministic(t *testing.T) {
	s := obs.Session{Status: obs.StatusBusy, StatusSince: since(time.Minute)}
	first := Resolve(s, quiet(10*time.Minute), NOW)
	for i := 0; i < 8; i++ {
		got := Resolve(s, quiet(10*time.Minute), NOW)
		if needed(got.Attention) != needed(first.Attention) ||
			got.Attention.Reason != first.Attention.Reason ||
			got.Status != first.Status || got.Confidence != first.Confidence {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
}

// TestHumanDurationMatchesTheStatusBarSpelling. It is a second copy of
// main.go's formatDuration — this package cannot import package main — so the
// one thing worth pinning is that the two SPELL a duration the same way.
func TestHumanDurationMatchesTheStatusBarSpelling(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{1500 * time.Millisecond, "1.5s"},
		{90 * time.Second, "1m 30s"},
		{72 * time.Hour, "72h 0m"},
		{-2 * time.Second, "-2.0s"},
	} {
		if got := humanDuration(tt.d); got != tt.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
