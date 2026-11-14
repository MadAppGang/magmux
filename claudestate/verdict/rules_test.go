package verdict

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// The ladder over the EFFECTIVE status (§3.4's consumers table, D9).

func TestALivenessGateDismissesOnlyASessionWithNoJob(t *testing.T) {
	dead := model.Session{Status: model.StatusBlocked, ProcessAlive: model.LivenessNo, Job: &model.JobFacts{}}
	got := Resolve(dead, Screen{}, NOW, Rules{})
	if got.Attention.Needed == nil || !*got.Attention.Needed {
		t.Fatalf("a dead twin over a blocked job is still blocked: %s %q", needed(got.Attention), got.Attention.Reason)
	}
	gone := model.Session{Status: model.StatusWaiting, ProcessAlive: model.LivenessNo}
	got = Resolve(gone, Screen{}, NOW, Rules{})
	if got.Attention.Needed == nil || *got.Attention.Needed || !IsLivenessDismissal(got.Attention.Reason) {
		t.Fatalf("an interactive session whose process exited: %s %q", needed(got.Attention), got.Attention.Reason)
	}
}

func TestABlockedJobNeedsAHumanAndSaysWhat(t *testing.T) {
	needs := "approve the migration"
	s := model.Session{Status: model.StatusBlocked, Job: &model.JobFacts{Needs: &needs}, StatusSince: since(time.Hour)}
	got := Resolve(s, Screen{}, NOW, Rules{})
	if got.Attention.Needed == nil || !*got.Attention.Needed || !strings.Contains(got.Attention.Reason, needs) ||
		!strings.Contains(got.Attention.Reason, "1h 0m") {
		t.Fatalf("%s %q", needed(got.Attention), got.Attention.Reason)
	}
}

func TestWorkingIsBusyAndDoneOrFailedNeedNobody(t *testing.T) {
	for _, st := range []model.Status{model.StatusWorking, model.StatusBusy, model.StatusDone, model.StatusFailed} {
		got := Resolve(model.Session{Status: st}, Screen{}, NOW, Rules{})
		if got.Confidence != model.ConfidenceAuthoritative || got.Attention.Needed == nil || *got.Attention.Needed {
			t.Errorf("%v: %v %s %q", st, got.Confidence, needed(got.Attention), got.Attention.Reason)
		}
	}
	if got := Resolve(model.Session{Status: model.StatusWorking}, quiet(5*time.Minute), NOW, Rules{}); got.Conflict == nil {
		t.Fatal("a working record and a silent pane conflict, as busy does")
	}
}

// C19: the inferred tier is the one writer after the join, only when rule E
// gave Unknown with an EMPTY raw word, and it marks itself.
func TestOnlyTheInferredTierWritesStatusAfterTheJoin(t *testing.T) {
	at := NOW.Add(-time.Minute)
	s := model.Session{StatusFrom: model.SourceRegistry, StatusSince: &at, Waiting: model.WaitingUnknown}
	got := Resolve(s, quiet(10*time.Minute), NOW, Rules{})
	if got.Status != model.StatusIdle || got.Confidence != model.ConfidenceInferred || got.StatusFrom != model.SourceUnknown ||
		got.StatusRaw != "" || got.StatusSince != s.StatusSince {
		t.Fatalf("%+v", got)
	}
	raw := model.Session{StatusRaw: "compacting", StatusFrom: model.SourceRegistry}
	if got := Resolve(raw, quiet(10*time.Minute), NOW, Rules{}); got.Status != model.StatusUnknown || got.StatusFrom != model.SourceRegistry {
		t.Fatalf("an unrecognised word is never replaced by a guess: %+v", got)
	}
	auth := model.Session{Status: model.StatusIdle, StatusFrom: model.SourceJobs}
	if got := Resolve(auth, quiet(10*time.Minute), NOW, Rules{}); got.Status != model.StatusIdle || got.StatusFrom != model.SourceJobs {
		t.Fatalf("an authoritative status is untouched: %+v", got)
	}
}

// D9: tt's rules ignore API errors; magmux's zero value does not.
func TestIgnoreAPIErrorsSkipsRuleThree(t *testing.T) {
	n := 2
	s := model.Session{Status: model.StatusIdle, Fault: model.Fault{APIErrors: &n, LastMessage: "overloaded"}}
	if got := Resolve(s, Screen{}, NOW, Rules{}); got.Attention.Needed == nil || !*got.Attention.Needed ||
		!strings.Contains(got.Attention.Reason, "overloaded") {
		t.Fatalf("magmux's ladder: %s %q", needed(got.Attention), got.Attention.Reason)
	}
	if got := Resolve(s, Screen{}, NOW, Rules{IgnoreAPIErrors: true}); got.Attention.Needed == nil || *got.Attention.Needed {
		t.Fatalf("tt's rules: %s %q", needed(got.Attention), got.Attention.Reason)
	}
}

// The two predicates are closed sets over what the ladder actually says.
func TestThePredicatesCoverEveryUndeterminedAndDismissalReason(t *testing.T) {
	cases := []model.Session{
		{Status: model.StatusBusy},
		{},
		{StatusRaw: "compacting"},
	}
	screens := []Screen{quiet(30 * time.Minute), quiet(10 * time.Minute), {}}
	for i, s := range cases {
		got := Resolve(s, screens[i], NOW, Rules{})
		if got.Attention.Needed != nil || !IsUndetermined(got.Attention.Reason) {
			t.Errorf("case %d: %s %q", i, needed(got.Attention), got.Attention.Reason)
		}
		if IsLivenessDismissal(got.Attention.Reason) {
			t.Errorf("case %d is not a dismissal", i)
		}
	}
	running := Screen{Known: true, Quiet: time.Minute, Shell: model.ShellState{Mark: model.ShellMarkRunning}}
	if got := Resolve(model.Session{}, running, NOW, Rules{}); !IsUndetermined(got.Attention.Reason) {
		t.Errorf("rule 9: %q", got.Attention.Reason)
	}
	for _, pa := range []model.Liveness{model.LivenessNo, model.LivenessUnknown} {
		got := Resolve(model.Session{Status: model.StatusIdle, ProcessAlive: model.LivenessNo, PaneAlive: pa}, Screen{}, NOW, Rules{})
		if !IsLivenessDismissal(got.Attention.Reason) || IsUndetermined(got.Attention.Reason) {
			t.Errorf("%v: %q", pa, got.Attention.Reason)
		}
	}
	if IsUndetermined("its own record says it has nothing outstanding") || IsLivenessDismissal("it is working") {
		t.Fatal("a determined answer matched")
	}
}
