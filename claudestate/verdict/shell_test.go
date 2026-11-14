package verdict

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// The generic tier: what the terminal itself says, weighed against silence.
//
// Every test here is a table over VALUES. Nothing in this file opens a
// terminal, forks a process or asks the kernel anything: the two direct
// signals arrive as an [model.ShellState], which is the whole reason the port
// hands over a value rather than a file descriptor.

// atPrompt is a screen whose shell says, by whichever route, that it is at a
// prompt after d of silence.
func atPrompt(d time.Duration) Screen {
	return Screen{Known: true, Quiet: d, Shell: model.ShellState{AtPrompt: model.LivenessYes}}
}

// runningFor is a screen whose shell says a command is executing, d of silence
// in.
func runningFor(d time.Duration) Screen {
	return Screen{Known: true, Quiet: d, Shell: model.ShellState{Mark: model.ShellMarkRunning}}
}

// TestTheTwoDirectSignalsReconcile reads the whole of Screen.running, which is
// where the kernel's answer and the shell's own marks meet.
//
// A WRONG IMPLEMENTATION picks one source and ignores the other. Each covers
// the case the other is blind to: a shell BUILTIN never moves the foreground
// group, so tcgetpgrp reports "at a prompt" while the shell is plainly busy;
// and a command started by anything other than the shell's prompt loop emits
// no 133;C, so the marks say "at a prompt" while something else holds the
// terminal. Either source alone is wrong about a case that happens daily.
func TestTheTwoDirectSignalsReconcile(t *testing.T) {
	for _, tt := range []struct {
		name  string
		shell model.ShellState
		want  model.Liveness
	}{
		{"nothing said at all", model.ShellState{}, model.LivenessUnknown},
		{"marks running", model.ShellState{Mark: model.ShellMarkRunning}, model.LivenessYes},
		{"marks at a prompt", model.ShellState{Mark: model.ShellMarkPrompt}, model.LivenessNo},
		{"kernel: a foreign foreground group", model.ShellState{AtPrompt: model.LivenessNo}, model.LivenessYes},
		{"kernel: the leader, latch proven", model.ShellState{AtPrompt: model.LivenessYes}, model.LivenessNo},
		{
			"a builtin: marks say running, the kernel sees the leader",
			model.ShellState{Mark: model.ShellMarkRunning, AtPrompt: model.LivenessYes},
			model.LivenessYes,
		},
		{
			"a command nobody marked: marks stale at prompt, kernel sees a stranger",
			model.ShellState{Mark: model.ShellMarkPrompt, AtPrompt: model.LivenessNo},
			model.LivenessYes,
		},
		{
			"both agree nothing is running",
			model.ShellState{Mark: model.ShellMarkPrompt, AtPrompt: model.LivenessYes},
			model.LivenessNo,
		},
		{
			"marks at a prompt, latch unproven: the mark still counts",
			model.ShellState{Mark: model.ShellMarkPrompt, AtPrompt: model.LivenessUnknown},
			model.LivenessNo,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Screen{Known: true, Shell: tt.shell}.running()
			if got != tt.want {
				t.Fatalf("running %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDirectEvidenceOutranksSilence is the new half of the status map.
//
// A WRONG IMPLEMENTATION keeps reading the ladder first, so a pane with a
// command still in its foreground is reported IDLE after five seconds of
// silence — `sleep 600`, a linking step, a buffered test run — which is the
// wrong "idle" the whole generic tier exists to avoid.
func TestDirectEvidenceOutranksSilence(t *testing.T) {
	for _, tt := range []struct {
		name   string
		screen Screen
		want   model.Status
	}{
		{"at a prompt, no waiting needed", atPrompt(0), model.StatusIdle},
		{"at a prompt after an hour", atPrompt(time.Hour), model.StatusIdle},
		{"running, bytes arriving", runningFor(time.Second), model.StatusBusy},
		{"running and silent", runningFor(QuietIdle), model.StatusUnknown},
		{"running and silent for an hour", runningFor(time.Hour), model.StatusUnknown},
		{"typed into beats everything", Screen{
			Known: true, Typed: true, Shell: model.ShellState{AtPrompt: model.LivenessYes},
		}, model.StatusUnknown},
		{"no pane beats everything", Screen{
			Shell: model.ShellState{Mark: model.ShellMarkRunning},
		}, model.StatusUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.screen.Status(); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUndeterminedShellEvidenceChangesNothing is the property that makes this
// phase safe to ship: a shell that says nothing — no OSC 133, no proven job
// control, which is the commonest pane on any machine — resolves EXACTLY as it
// did before the port existed.
//
// A WRONG IMPLEMENTATION reads the zero [model.ShellState] as a negative
// somewhere: ShellMarkUnknown as "nothing is running", or LivenessUnknown as
// "not at a prompt". Either one changes the answer for every pane magmux has
// ever observed, and the table below is phase 5's own status table, unchanged.
func TestUndeterminedShellEvidenceChangesNothing(t *testing.T) {
	for _, tt := range []struct {
		name   string
		screen Screen
		want   model.Status
	}{
		{"nothing known", Screen{}, model.StatusUnknown},
		{"typed into", Screen{Known: true, Typed: true}, model.StatusUnknown},
		{"bytes arriving", quiet(0), model.StatusBusy},
		{"just under the first rung", quiet(QuietIdle - time.Millisecond), model.StatusBusy},
		{"on the first rung", quiet(QuietIdle), model.StatusIdle},
		{"long silence", quiet(time.Hour), model.StatusIdle},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.screen.Shell != (model.ShellState{}) {
				t.Fatalf("the screen under test carries shell evidence: %+v", tt.screen.Shell)
			}
			if got := tt.screen.Status(); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestARunningCommandWithdrawsTheConflict is the one change direct evidence
// makes to the conflict rule.
//
// A WRONG IMPLEMENTATION keeps the phase 5 rule — busy record, a minute of
// silence — and publishes a disagreement against a pane whose own shell says
// the command the record is talking about is still running. The two tiers
// AGREE there; silence was only ever a proxy for the thing the shell states
// outright.
func TestARunningCommandWithdrawsTheConflict(t *testing.T) {
	busy := model.Session{Status: model.StatusBusy}

	silent := Resolve(busy, quiet(5*time.Minute), NOW, Rules{})
	if silent.Conflict == nil {
		t.Fatal("a busy record against five minutes of unexplained silence: the conflict went missing")
	}

	still := Resolve(busy, runningFor(5*time.Minute), NOW, Rules{})
	if still.Conflict != nil {
		t.Fatalf("a busy record against a pane whose shell says a command is running is not a "+
			"disagreement; got conflict %+v", still.Conflict)
	}
	if n := needed(still.Attention); n != "false" {
		t.Errorf("attention %s, want false: the record says busy and nothing contradicts it", n)
	}
}

// TestAtPromptStrengthensTheConflictItDoesNotHastenIt.
//
// A WRONG IMPLEMENTATION shortens the minute when the shell says it is at a
// prompt. The threshold is deliberately unmoved — see Screen.contradicts —
// and what direct evidence buys is a better SENTENCE for the human reading the
// published loser.
func TestAtPromptStrengthensTheConflictItDoesNotHastenIt(t *testing.T) {
	busy := model.Session{Status: model.StatusBusy}

	early := Resolve(busy, atPrompt(QuietConflict-time.Second), NOW, Rules{})
	if early.Conflict != nil {
		t.Fatalf("direct evidence hastened the conflict past its threshold: %+v", early.Conflict)
	}

	late := Resolve(busy, atPrompt(QuietConflict), NOW, Rules{})
	if late.Conflict == nil {
		t.Fatal("a busy record against a shell back at its prompt for a full minute: no conflict raised")
	}
	if !strings.Contains(late.Conflict.Detail, "back at a prompt") {
		t.Errorf("conflict detail %q does not say what the shell said", late.Conflict.Detail)
	}
}

// TestAShellAtAPromptIsADeterminedNo is attention rule 7, the generic tier's
// one determined answer.
//
// A WRONG IMPLEMENTATION leaves it undetermined with rule 8's sentence. That
// rule's argument is that a quiet screen could be a permission prompt; a shell
// that has drawn its prompt cannot be holding one, so the evidence now
// supports a verdict where before it supported a shrug.
func TestAShellAtAPromptIsADeterminedNo(t *testing.T) {
	got := Resolve(model.Session{}, atPrompt(30*time.Second), NOW, Rules{})

	if got.Status != model.StatusIdle {
		t.Fatalf("status %v, want idle", got.Status)
	}
	if got.Confidence != model.ConfidenceInferred {
		t.Fatalf("confidence %v, want inferred", got.Confidence)
	}
	if n := needed(got.Attention); n != "false" {
		t.Fatalf("attention %s, want false (reason %q)", n, got.Attention.Reason)
	}
	if !strings.Contains(got.Attention.Reason, "at a prompt") {
		t.Errorf("reason %q does not say why", got.Attention.Reason)
	}
}

// TestTheExitStatusHasThreeStates is the tri-state doctrine applied to the one
// generic signal that says a command FAILED rather than merely ended.
//
// A WRONG IMPLEMENTATION collapses "no status was reported" into zero and
// reports every shell without integration as a machine where everything always
// works — or treats a non-zero status as a clean finish, which loses the only
// failure signal this tier has.
func TestTheExitStatusHasThreeStates(t *testing.T) {
	for _, tt := range []struct {
		name    string
		exit    *int
		want    string
		notWant string
	}{
		{"no status reported", nil, "at a prompt with nothing running", "exited"},
		{"a determined zero", ptr(0), "last command succeeded", "exited"},
		{"failed", ptr(1), "last command exited 1", "succeeded"},
		{"failed with a signal-ish status", ptr(130), "last command exited 130", "succeeded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sc := atPrompt(time.Minute)
			sc.Shell.Exit = tt.exit
			got := Resolve(model.Session{}, sc, NOW, Rules{})

			if n := needed(got.Attention); n != "false" {
				t.Fatalf("attention %s, want false: a failed command does not ask for a human, "+
					"it reports; see rule 7", n)
			}
			if !strings.Contains(got.Attention.Reason, tt.want) {
				t.Fatalf("reason %q does not contain %q", got.Attention.Reason, tt.want)
			}
			if tt.notWant != "" && strings.Contains(got.Attention.Reason, tt.notWant) {
				t.Fatalf("reason %q contains %q, which is a different outcome", got.Attention.Reason, tt.notWant)
			}
		})
	}
}

// TestASilentRunningCommandIsUndeterminedNotIdle is attention rule 9, and it
// is the rule this phase exists for.
//
// A WRONG IMPLEMENTATION lets rule 8 have it: the pane has been quiet for five
// minutes, so it is idle, so — on the inferred tier — undetermined with a
// sentence about silence. The verdict happens to match, and the STATUS does
// not: a document that says a session with a command still running is idle is
// wrong in the field every consumer reads first.
func TestASilentRunningCommandIsUndeterminedNotIdle(t *testing.T) {
	got := Resolve(model.Session{}, runningFor(5*time.Minute), NOW, Rules{})

	if got.Status == model.StatusIdle {
		t.Fatal("a pane with a command still in its foreground was published as idle")
	}
	if got.Status != model.StatusUnknown {
		t.Fatalf("status %v, want unknown", got.Status)
	}
	if n := needed(got.Attention); n != "nil" {
		t.Fatalf("attention %s, want nil (reason %q)", n, got.Attention.Reason)
	}
	for _, want := range []string{"a command is running", "5m 0s", "cannot be told"} {
		if !strings.Contains(got.Attention.Reason, want) {
			t.Errorf("reason %q does not contain %q", got.Attention.Reason, want)
		}
	}
}

// TestAWordThisBuildCannotDecodeOutranksTheShell.
//
// A WRONG IMPLEMENTATION lets rule 9 answer for a session whose harness spoke
// a status we failed to parse, and the raw word — documented as the only truth
// available in that case — never reaches the human. A new Claude release then
// looks like a shell fact instead of like a new Claude release.
func TestAWordThisBuildCannotDecodeOutranksTheShell(t *testing.T) {
	got := Resolve(model.Session{StatusRaw: "compacting"}, runningFor(5*time.Minute), NOW, Rules{})

	if !strings.Contains(got.Attention.Reason, `"compacting"`) {
		t.Fatalf("reason %q does not quote the word the harness wrote", got.Attention.Reason)
	}
	if got.Confidence != model.ConfidenceUnknown {
		t.Errorf("confidence %v, want unknown: the inferred tier does not take over an "+
			"undecodable harness word", got.Confidence)
	}
}

// ── the stage's end of the port ─────────────────────────────────────────────

// fakeShell stands in for magmux's pane table. It COUNTS, which the shared
// [ports.PaneShell] contract says needs synchronisation — and does not have any,
// because every test below drives exactly one stage from the test's own
// goroutine. The count is here for one assertion that cannot be made any other
// way: that the latch's owner is asked on every observation.
type fakeShell struct {
	state map[int]model.ShellState
	// missing lists panes that have no terminal to ask about — a closed pane,
	// or magmux's own control panel, which has no PTY and never will.
	missing map[int]bool
	calls   int
}

func (f *fakeShell) Shell(ref model.PaneRef) (model.ShellState, bool) {
	pane := paneNumber(ref)
	f.calls++
	if f.missing[pane] {
		return model.ShellState{}, false
	}
	return f.state[pane], true
}

// TestTheStageSamplesTheShellOnEveryObservation.
//
// A WRONG IMPLEMENTATION asks only on the paths where the answer is used —
// skipping the pane that was just typed into, or the one whose silence has not
// reached a rung. The port is what ADVANCES the job-control latch, so an
// implementation that samples only when it wants an answer proves job control
// late, or never, and reports undetermined for a pane it could have been sure
// about.
func TestTheStageSamplesTheShellOnEveryObservation(t *testing.T) {
	now := NOW
	act := fakeActivity{
		known: map[int]bool{3: true},
		out:   map[int]time.Time{3: NOW.Add(-time.Minute)},
		in:    map[int]time.Time{3: NOW.Add(-time.Second)}, // typed into: the early-return path
	}
	sh := &fakeShell{state: map[int]model.ShellState{3: {AtPrompt: model.LivenessYes}}}
	st := New(Config{Activity: act, Shell: sh, Now: func() time.Time { return now }})

	for i := 0; i < 3; i++ {
		st.Observe(paneSession(3))
		now = now.Add(time.Second)
	}
	if sh.calls != 3 {
		t.Fatalf("the shell port was asked %d times across 3 observations, want 3", sh.calls)
	}
}

// TestANilShellPortLeavesTheTierExactlyAsItWas.
//
// A WRONG IMPLEMENTATION dereferences it, or treats "no port" as a determined
// negative. Running outside a magmux — `magmux agents` — there is no terminal to
// ask about at all, and the tier must degrade to the staleness cascade it had
// before this port existed rather than to a guess.
func TestANilShellPortLeavesTheTierExactlyAsItWas(t *testing.T) {
	now := NOW
	act := fakeActivity{known: map[int]bool{1: true}, out: map[int]time.Time{1: NOW.Add(-time.Hour)}}
	st := New(Config{Activity: act, Now: func() time.Time { return now }})

	got := st.Observe(paneSession(1))
	if got.Status != model.StatusIdle || got.Confidence != model.ConfidenceInferred {
		t.Fatalf("status %v confidence %v, want idle/inferred — the cascade's own answer",
			got.Status, got.Confidence)
	}
}

// TestAPaneWithNoTerminalIsNotEvidence.
//
// A WRONG IMPLEMENTATION reads the zero [model.ShellState] that comes back
// beside a false and files it as "this shell says nothing is running" — which
// is what a pane with no PTY would then look like. false is "there is nothing
// to ask", and the two are different.
func TestAPaneWithNoTerminalIsNotEvidence(t *testing.T) {
	now := NOW
	act := fakeActivity{known: map[int]bool{9: true}, out: map[int]time.Time{9: NOW.Add(-time.Hour)}}
	sh := &fakeShell{
		missing: map[int]bool{9: true},
		// Deliberately staged with an answer it must not hand over.
		state: map[int]model.ShellState{9: {Mark: model.ShellMarkRunning}},
	}
	st := New(Config{Activity: act, Shell: sh, Now: func() time.Time { return now }})

	got := st.Observe(paneSession(9))
	if got.Status != model.StatusIdle {
		t.Fatalf("status %v, want the cascade's idle: a pane with no terminal supplies no "+
			"terminal evidence", got.Status)
	}
}

// TestTheInferredTierStillNeverGuessesWaitingOrShell re-runs phase 5's
// invariant across every combination of the new evidence.
//
// A WRONG IMPLEMENTATION maps "at a prompt" onto [model.StatusShell] — the word
// is right there in the vocabulary and it is the wrong word. StatusShell means
// a HARNESS handed its terminal to a shell, which is a fact about a session
// nobody has here; and a shell at a prompt is not a session waiting, because
// nothing has been asked of it.
func TestTheInferredTierStillNeverGuessesWaitingOrShell(t *testing.T) {
	marks := []model.ShellMark{model.ShellMarkUnknown, model.ShellMarkPrompt, model.ShellMarkRunning}
	prompts := []model.Liveness{model.LivenessUnknown, model.LivenessYes, model.LivenessNo}
	exits := []*int{nil, ptr(0), ptr(7)}
	quiets := []time.Duration{0, time.Second, QuietIdle, QuietConflict, time.Hour}

	for _, m := range marks {
		for _, p := range prompts {
			for _, e := range exits {
				for _, q := range quiets {
					for _, typed := range []bool{false, true} {
						sc := Screen{
							Known: true, Typed: typed, Quiet: q,
							Shell: model.ShellState{Mark: m, AtPrompt: p, Exit: e},
						}
						if got := sc.Status(); got == model.StatusWaiting || got == model.StatusShell {
							t.Fatalf("screen %+v inferred %v, which no terminal evidence can support", sc, got)
						}
					}
				}
			}
		}
	}
}
