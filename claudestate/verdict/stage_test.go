package verdict

import (
	"strconv"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/internal/termev"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

// fakeActivity is a stand-in for magmux's pane table.
//
// It holds no counters and no memo, deliberately: [ports.PaneActivity] is shared
// by every session's stage, and a fake that counted its calls would need a lock
// this layer is not allowed to have. Its maps are written before the stage sees
// it and read afterwards, on one goroutine.
type fakeActivity struct {
	out map[int]time.Time
	in  map[int]time.Time
	// known lists the panes that exist. A pane absent from it answers false,
	// which is a different thing from a pane with two zero instants.
	known map[int]bool
}

func (f fakeActivity) Activity(ref model.PaneRef) (time.Time, time.Time, bool) {
	pane := paneNumber(ref)
	if !f.known[pane] {
		return time.Time{}, time.Time{}, false
	}
	return f.out[pane], f.in[pane], true
}

// stageAt builds a stage whose clock reads whatever now points at, so a test
// can move time by assigning to it.
func stageAt(act ports.PaneActivity, now *time.Time) *Stage {
	return New(Config{Activity: act, Now: func() time.Time { return *now }})
}

func paneSession(pane int) model.Session {
	return model.Session{Key: model.Key{SessionID: "s"}, PID: 4242, Pane: model.PaneRef(strconv.Itoa(pane))}
}

// TestTheStageMeasuresTheSilenceItIsToldAbout is the port doing its job: two
// instants in, a screen out.
func TestTheStageMeasuresTheSilenceItIsToldAbout(t *testing.T) {
	now := NOW
	act := fakeActivity{
		known: map[int]bool{3: true},
		out:   map[int]time.Time{3: NOW.Add(-7 * time.Minute)},
	}
	st := stageAt(act, &now)

	got := st.Observe(paneSession(3))
	if got.Status != model.StatusIdle || got.Confidence != model.ConfidenceInferred {
		t.Fatalf("published %v/%v, want idle/inferred for a pane silent seven minutes",
			got.Status, got.Confidence)
	}
	if got.Attention.Needed != nil {
		t.Errorf("attention %s for an inferred-only idle, want nil", needed(got.Attention))
	}
}

// TestASessionWithNoPaneHasNoInferredTier.
//
// A WRONG IMPLEMENTATION treats "no pane" as "a pane that has been silent
// forever" and reports every session magmux did not start as idle — which,
// outside magmux, is every session there is.
func TestASessionWithNoPaneHasNoInferredTier(t *testing.T) {
	now := NOW
	act := fakeActivity{known: map[int]bool{3: true}, out: map[int]time.Time{3: NOW.Add(-time.Hour)}}

	for _, tt := range []struct {
		name string
		st   *Stage
		s    model.Session
	}{
		{"no pane id", stageAt(act, &now), model.Session{Key: model.Key{SessionID: "s"}, PID: 1}},
		{"a pane the table does not have", stageAt(act, &now), paneSession(99)},
		{"no activity port at all", New(Config{Now: func() time.Time { return now }}), paneSession(3)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.st.Observe(tt.s)
			if got.Status != model.StatusUnknown {
				t.Errorf("status %v, want unknown — there was no terminal evidence", got.Status)
			}
			if got.Confidence != model.ConfidenceUnknown {
				t.Errorf("confidence %v, want unknown", got.Confidence)
			}
			if _, armed := tt.st.WakeIn(); armed {
				t.Error("an alarm was asked for on a session with no pane to be silent")
			}
		})
	}
}

// TestOutputReArmsAndInputCancels — the cascade's two signals, reaching it
// through the port rather than through a Driver's two methods.
//
// A WRONG IMPLEMENTATION measures the silence from whichever instant is later,
// so a pane a human typed into looks like a pane that produced output.
func TestOutputReArmsAndInputCancels(t *testing.T) {
	now := NOW
	act := fakeActivity{known: map[int]bool{1: true}, out: map[int]time.Time{}, in: map[int]time.Time{}}
	st := stageAt(act, &now)

	// Silent for ten minutes: inferred idle, and an alarm for the next rung.
	act.out[1] = NOW.Add(-10 * time.Minute)
	if got := st.Observe(paneSession(1)); got.Status != model.StatusIdle {
		t.Fatalf("status %v after ten minutes of silence, want idle", got.Status)
	}
	if _, armed := st.WakeIn(); !armed {
		t.Error("no alarm was asked for while rungs remained")
	}

	// Output arrives. The silence starts over: the pane is producing again.
	act.out[1] = NOW.Add(-time.Second)
	if got := st.Observe(paneSession(1)); got.Status != model.StatusBusy {
		t.Fatalf("status %v one second after output, want busy", got.Status)
	}

	// Somebody types. The cascade is CANCELLED, not re-armed: the silence
	// belongs to the human now.
	act.in[1] = NOW
	got := st.Observe(paneSession(1))
	if got.Status != model.StatusUnknown {
		t.Fatalf("status %v after input, want unknown — the inferred tier has no opinion "+
			"about a pane waiting on a human", got.Status)
	}
	if d, armed := st.WakeIn(); armed {
		t.Errorf("an alarm for %s was asked for after input cancelled the cascade", d)
	}

	// Output resumes, later than the input. The cascade re-arms.
	now = NOW.Add(time.Hour)
	act.out[1] = NOW.Add(time.Minute)
	if got := st.Observe(paneSession(1)); got.Status != model.StatusIdle {
		t.Fatalf("status %v once output resumed and went quiet again, want idle", got.Status)
	}
	if _, armed := st.WakeIn(); !armed {
		t.Error("no alarm was asked for after the cascade re-armed")
	}
}

// TestAPaneThatHasPrintedNothingIsArmedAtFirstSight.
//
// termev.Driver armed at BIRTH, because a child that hung before printing a
// byte is the most interesting staleness there is. The birth instant does not
// reach this layer, so the cascade is armed at first sight instead.
//
// A WRONG IMPLEMENTATION leaves such a pane undetermined forever — the cascade
// refuses the zero instant, so a stage that passed it straight through would
// never arm and a hung child would never be noticed.
func TestAPaneThatHasPrintedNothingIsArmedAtFirstSight(t *testing.T) {
	now := NOW
	act := fakeActivity{known: map[int]bool{0: true}}
	st := stageAt(act, &now)

	// First sight: nothing has happened, and nothing is claimed yet.
	if got := st.Observe(paneSession(0)); got.Status != model.StatusBusy {
		t.Fatalf("status %v at first sight, want busy — the silence has not been measured yet",
			got.Status)
	}
	if _, armed := st.WakeIn(); !armed {
		t.Fatal("no alarm was asked for on a pane that has printed nothing; a child that hung " +
			"before its first byte would never be noticed")
	}

	// Still nothing, ten minutes later.
	now = NOW.Add(10 * time.Minute)
	got := st.Observe(paneSession(0))
	if got.Status != model.StatusIdle {
		t.Fatalf("status %v ten minutes after first sight, want idle", got.Status)
	}
	if got.Attention.Needed != nil {
		t.Errorf("attention %s, want nil for an inferred-only verdict", needed(got.Attention))
	}
}

// TestFirstSightIsNotRestampedOnEveryLook.
//
// A WRONG IMPLEMENTATION re-stamps "first sight" on every observation, so a
// pane that has never printed is permanently zero seconds quiet and no rung
// ever fires. The cascade's own refusal of a non-advancing instant hides this
// unless the instant itself is what moves.
func TestFirstSightIsNotRestampedOnEveryLook(t *testing.T) {
	now := NOW
	st := stageAt(fakeActivity{known: map[int]bool{0: true}}, &now)

	for i := 0; i < 5; i++ {
		now = NOW.Add(time.Duration(i) * time.Second)
		st.Observe(paneSession(0))
	}
	now = NOW.Add(time.Hour)
	if got := st.Observe(paneSession(0)); got.Status != model.StatusIdle {
		t.Fatalf("status %v an hour after first sight, want idle — the first-sight instant "+
			"was restamped", got.Status)
	}
}

// TestTheAlarmAsksForTheNextRungAndNoSooner is the arithmetic the actor arms
// its one-shot timer with.
//
// A WRONG IMPLEMENTATION asks for a rung that has already passed — because it
// did not drain the crossed ones — and the alarm then fires immediately, asks
// for a cycle, and is armed again by that cycle. That is a poll loop wearing a
// one-shot timer's clothes, and it is the failure this layer is written
// against.
func TestTheAlarmAsksForTheNextRungAndNoSooner(t *testing.T) {
	ladder := termev.DefaultLadder()
	now := NOW
	act := fakeActivity{known: map[int]bool{2: true}, out: map[int]time.Time{}}
	st := stageAt(act, &now)

	for _, tt := range []struct {
		quiet time.Duration
		want  time.Duration
	}{
		{0, ladder[0]},
		{ladder[0] - time.Second, time.Second},
		{ladder[0], ladder[1] - ladder[0]},
		{ladder[2] + time.Second, ladder[3] - ladder[2] - time.Second},
	} {
		t.Run(tt.quiet.String(), func(t *testing.T) {
			act.out[2] = now.Add(-tt.quiet)
			st = stageAt(act, &now)
			st.Observe(paneSession(2))
			d, armed := st.WakeIn()
			if !armed {
				t.Fatalf("no alarm after %s of silence, want one in %s", tt.quiet, tt.want)
			}
			if d != tt.want {
				t.Fatalf("alarm asked for %s, want %s", d, tt.want)
			}
			if d <= 0 {
				t.Fatal("an alarm that is already due would spin the pipeline")
			}
		})
	}
}

// TestAnExhaustedLadderAsksForNothing — an idle machine must cost nothing, and
// this is where that stops being a claim.
func TestAnExhaustedLadderAsksForNothing(t *testing.T) {
	ladder := termev.DefaultLadder()
	now := NOW
	act := fakeActivity{
		known: map[int]bool{5: true},
		out:   map[int]time.Time{5: NOW.Add(-2 * ladder[len(ladder)-1])},
	}
	st := stageAt(act, &now)

	st.Observe(paneSession(5))
	if d, armed := st.WakeIn(); armed {
		t.Fatalf("an alarm for %s was asked for after every rung had been crossed", d)
	}
	// And the verdict is still right: the ladder running out does not make a
	// session less silent.
	if got := st.Observe(paneSession(5)); got.Status != model.StatusIdle {
		t.Errorf("status %v with an exhausted ladder, want idle", got.Status)
	}
}

// TestTheStageResolvesAgainstTheAuthoritativeTier — the stage is where the two
// tiers actually meet, so the end-to-end case is worth stating here too and not
// only against the pure function.
func TestTheStageResolvesAgainstTheAuthoritativeTier(t *testing.T) {
	now := NOW
	act := fakeActivity{
		known: map[int]bool{7: true},
		out:   map[int]time.Time{7: NOW.Add(-20 * time.Minute)},
	}
	st := stageAt(act, &now)

	s := paneSession(7)
	s.Status = model.StatusBusy
	s.StatusSince = since(25 * time.Minute)

	got := st.Observe(s)
	if got.Status != model.StatusBusy || got.Confidence != model.ConfidenceAuthoritative {
		t.Fatalf("published %v/%v, want busy/authoritative", got.Status, got.Confidence)
	}
	if got.Conflict == nil {
		t.Fatal("twenty minutes of silence against a busy record raised no conflict")
	}
	if got.Attention.Needed != nil {
		t.Fatalf("attention %s, want nil for a conflict", needed(got.Attention))
	}
}

// TestTheStageNamesItself. Free text, but a stage with no name is invisible in
// a diagnostic.
func TestTheStageNamesItself(t *testing.T) {
	if got := New(Config{}).Name(); got == "" {
		t.Fatal("the stage has no name")
	}
}

// TestTheStageDefaultsItsClock — a nil Now must not mean the zero instant,
// which would make every pane infinitely quiet.
func TestTheStageDefaultsItsClock(t *testing.T) {
	act := fakeActivity{known: map[int]bool{1: true}, out: map[int]time.Time{1: time.Now()}}
	got := New(Config{Activity: act}).Observe(paneSession(1))
	if got.Status != model.StatusBusy {
		t.Fatalf("status %v for a pane that produced output just now, want busy — a nil Now "+
			"fell back to the zero instant", got.Status)
	}
}

func paneNumber(ref model.PaneRef) int {
	n, err := strconv.Atoi(string(ref))
	if err != nil {
		return -1
	}
	return n
}

func ptr[T any](v T) *T { return &v }
