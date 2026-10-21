package navigate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// fakeFocuser stands in for magmux's pane table. It records every call,
// because "focus did not move" is the thing a wrong implementation would do
// silently.
type fakeFocuser struct {
	focused int
	calls   []int
	err     error
}

func (f *fakeFocuser) Focus(paneID int) (int, error) {
	f.calls = append(f.calls, paneID)
	if f.err != nil {
		return -1, f.err
	}
	previous := f.focused
	f.focused = paneID
	return previous, nil
}

func session(paneID *int, alive obs.Liveness) obs.Session {
	return obs.Session{ID: "id-a", PID: 4242, PaneID: paneID, PaneAlive: alive}
}

// TestNothingIsDeclaredWhenNothingCanBeDone — the three absences, each of
// which is a "cannot" and not a "will not".
//
// A WRONG IMPLEMENTATION declares `focus` unconditionally and refuses at
// call time. The refusal then reads as policy — the user goes looking for a
// config to change — when the truth is that this process has no pane table,
// or this session has no pane. act.ErrNotCapable and act.ErrForbidden are
// different answers all the way out to the audit trail, and this is where
// the distinction is made.
func TestNothingIsDeclaredWhenNothingCanBeDone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		focuser obs.PaneFocuser
		session obs.Session
	}{
		{"no focuser in this process", nil, session(obs.Ptr(2), obs.LivenessYes)},
		{"no pane for this session", &fakeFocuser{}, session(nil, obs.LivenessUnknown)},
		{"the pane is gone", &fakeFocuser{}, session(obs.Ptr(2), obs.LivenessNo)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Focuser: tc.focuser})
			s.Observe(tc.session)
			if caps := s.Capabilities(); len(caps) != 0 {
				t.Fatalf("declared %+v", caps)
			}
		})
	}

	t.Run("an undetermined pane is still declared", func(t *testing.T) {
		// "Nobody could tell" is not "no". Declaring it lets the focuser give
		// the real answer instead of having one assumed on its behalf, which
		// is the tri-state rule the whole layer is built on.
		s := New(Config{Focuser: &fakeFocuser{}})
		s.Observe(session(obs.Ptr(2), obs.LivenessUnknown))
		if caps := s.Capabilities(); len(caps) != 1 {
			t.Fatalf("declared %+v for a session whose pane is undetermined", caps)
		}
	})
}

// TestTheDeclarationSaysNavigateAndSaysItIsSafeToRetry — a caller needs
// Reversible and Idempotent BEFORE acting, not after. Focus is both, and
// saying so is what makes a retry after a timeout safe.
func TestTheDeclarationSaysNavigateAndSaysItIsSafeToRetry(t *testing.T) {
	s := New(Config{Focuser: &fakeFocuser{}})
	s.Observe(session(obs.Ptr(3), obs.LivenessYes))

	caps := s.Capabilities()
	if len(caps) != 1 {
		t.Fatalf("declared %d capabilities", len(caps))
	}
	c := caps[0]
	if c.Verb != act.VerbFocus || c.Tier != act.TierNavigate {
		t.Fatalf("declared %s under %s", c.Verb, c.Tier)
	}
	if !c.Reversible || !c.Idempotent {
		t.Error("focus declares itself irreversible or non-idempotent; it is neither")
	}
	if !strings.Contains(c.Describe, "3") {
		t.Errorf("the description does not name the pane: %q", c.Describe)
	}
}

// TestFocusMovesFocusAndSaysWhatWasDisplaced — the displaced pane is the whole
// of the undo information a trail can carry for this verb, so it has to reach
// the result.
func TestFocusMovesFocusAndSaysWhatWasDisplaced(t *testing.T) {
	f := &fakeFocuser{focused: 1}
	s := New(Config{Focuser: f})
	s.Observe(session(obs.Ptr(2), obs.LivenessYes))

	res, err := s.Do(context.Background(), act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !res.Done {
		t.Fatal("the action reported itself not done")
	}
	if f.focused != 2 || len(f.calls) != 1 {
		t.Fatalf("focus is on %d after calls %v", f.focused, f.calls)
	}
	if !strings.Contains(res.Detail, "pane 1") || !strings.Contains(res.Detail, "pane 2") {
		t.Errorf("the detail does not say where focus came from and went: %q", res.Detail)
	}
}

// TestFocusingTheFocusedPaneSucceeds — idempotent means a retry is safe, and
// "safe" has to include the answer being success rather than an error a caller
// would treat as a failure and retry again.
func TestFocusingTheFocusedPaneSucceeds(t *testing.T) {
	f := &fakeFocuser{focused: 2}
	s := New(Config{Focuser: f})
	s.Observe(session(obs.Ptr(2), obs.LivenessYes))

	res, err := s.Do(context.Background(), act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate})
	if err != nil || !res.Done {
		t.Fatalf("Do: %v, %+v", err, res)
	}
	if !strings.Contains(res.Detail, "already") {
		t.Errorf("the detail does not say focus was already there: %q", res.Detail)
	}
}

// TestAFailingFocuserIsAFailedActionAndNotARefusal — the gate allowed it and
// it was attempted. The error says which pane and wraps what the pane table
// said, because "a pane that was closed" and "a pane that is not on screen"
// have different remedies.
func TestAFailingFocuserIsAFailedActionAndNotARefusal(t *testing.T) {
	boom := errors.New("pane 2 is not on screen")
	f := &fakeFocuser{err: boom}
	s := New(Config{Focuser: f})
	s.Observe(session(obs.Ptr(2), obs.LivenessYes))

	_, err := s.Do(context.Background(), act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate})
	if err == nil {
		t.Fatal("a failing focuser produced no error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying reason was lost: %v", err)
	}
}

// TestDoRefusesAVerbThisStageDoesNotPerform — the gate routes by declaration
// and cannot send the wrong verb here today. The check exists for the day a
// second actuator is added and the routing is one line more complicated.
func TestDoRefusesAVerbThisStageDoesNotPerform(t *testing.T) {
	s := New(Config{Focuser: &fakeFocuser{}})
	s.Observe(session(obs.Ptr(2), obs.LivenessYes))

	if _, err := s.Do(context.Background(), act.Action{Verb: "restart", Tier: act.TierLifecycle}); err == nil {
		t.Fatal("the navigator performed a restart")
	}
}

// TestObserveChangesNothingInTheDocument — this stage is in every chain, in
// both directions of the process: inside magmux where it can act, and outside
// where it cannot. A stage that contributed a fact would change what `magmux
// obs` reports depending on whether actuation was configured, which is a read
// path that depends on the write path.
func TestObserveChangesNothingInTheDocument(t *testing.T) {
	in := obs.Session{
		ID: "id-a", PID: 4242, Name: "circl", PaneID: obs.Ptr(2),
		Status: obs.StatusBusy, Confidence: obs.TierAuthoritative,
	}
	out := New(Config{Focuser: &fakeFocuser{}}).Observe(in)
	if out.Status != in.Status || out.Confidence != in.Confidence || out.Name != in.Name {
		t.Fatalf("the navigator changed the fact set: %+v", out)
	}
	if out.PaneID != in.PaneID {
		t.Fatal("the navigator replaced the pane pointer")
	}
}

// TestACancelledContextStopsTheActionBeforeItHappens — the caller is blocked
// on this return and may have gone. Checking first means a cancelled request
// does not move a human's focus a second after they gave up on it.
func TestACancelledContextStopsTheActionBeforeItHappens(t *testing.T) {
	f := &fakeFocuser{}
	s := New(Config{Focuser: f})
	s.Observe(session(obs.Ptr(2), obs.LivenessYes))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Do(ctx, act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate}); err == nil {
		t.Fatal("a cancelled action ran")
	}
	if len(f.calls) != 0 {
		t.Fatalf("focus moved for a cancelled action: %v", f.calls)
	}
}
