package navigate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
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

// session builds a session in the pane the library's gate resolved: its
// PaneRef, magmux's id in decimal (ADR-X1), or "" for no pane.
func session(pane model.PaneRef, alive model.Liveness) model.Session {
	return model.Session{Key: model.Key{SessionID: "id-a", PID: 4242}, PID: 4242, Pane: pane, PaneAlive: alive}
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
		focuser PaneFocuser
		session model.Session
	}{
		{"no focuser in this process", nil, session("2", model.LivenessYes)},
		{"no pane for this session", &fakeFocuser{}, session("", model.LivenessUnknown)},
		{"the pane is gone", &fakeFocuser{}, session("2", model.LivenessNo)},
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
		s.Observe(session("2", model.LivenessUnknown))
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
	s.Observe(session("3", model.LivenessYes))

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
	s.Observe(session("2", model.LivenessYes))

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
	s.Observe(session("2", model.LivenessYes))

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
	s.Observe(session("2", model.LivenessYes))

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
	s.Observe(session("2", model.LivenessYes))

	if _, err := s.Do(context.Background(), act.Action{Verb: "restart", Tier: act.TierLifecycle}); err == nil {
		t.Fatal("the navigator performed a restart")
	}
}

// TestObserveChangesNothingInTheDocument — this stage is in every chain, in
// both directions of the process: inside magmux where it can act, and outside
// where it cannot. A stage that contributed a fact would change what `magmux
// agents` reports depending on whether actuation was configured, which is a read
// path that depends on the write path.
func TestObserveChangesNothingInTheDocument(t *testing.T) {
	in := model.Session{
		Key: model.Key{SessionID: "id-a", PID: 4242}, PID: 4242, Name: "circl", Pane: "2",
		Status: model.StatusBusy, Confidence: model.ConfidenceAuthoritative,
	}
	out := New(Config{Focuser: &fakeFocuser{}}).Observe(in)
	if out.Status != in.Status || out.Confidence != in.Confidence || out.Name != in.Name {
		t.Fatalf("the navigator changed the fact set: %+v", out)
	}
	if out.Pane != in.Pane || out.Key != in.Key {
		t.Fatal("the navigator changed the pane or the key")
	}
}

// TestACancelledContextStopsTheActionBeforeItHappens — the caller is blocked
// on this return and may have gone. Checking first means a cancelled request
// does not move a human's focus a second after they gave up on it.
func TestACancelledContextStopsTheActionBeforeItHappens(t *testing.T) {
	f := &fakeFocuser{}
	s := New(Config{Focuser: f})
	s.Observe(session("2", model.LivenessYes))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Do(ctx, act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate}); err == nil {
		t.Fatal("a cancelled action ran")
	}
	if len(f.calls) != 0 {
		t.Fatalf("focus moved for a cancelled action: %v", f.calls)
	}
}

// TestAPaneRefThatIsNotAMagmuxIDDeclaresNothing — the handle is opaque to the
// library and magmux's own decimal spelling here (ADR-X1). Anything else is not
// an address in magmux's table, and is treated as no pane: "cannot", never a
// focus of some other pane.
func TestAPaneRefThatIsNotAMagmuxIDDeclaresNothing(t *testing.T) {
	for _, ref := range []model.PaneRef{"%107", "-1", "02", "two", " 2"} {
		s := New(Config{Focuser: &fakeFocuser{}})
		s.Observe(session(ref, model.LivenessYes))
		if caps := s.Capabilities(); len(caps) != 0 {
			t.Errorf("pane ref %q declared %+v", ref, caps)
		}
	}
}

// TestTheFactoryBuildsOneStagePerSession — a shared instance would focus the
// first session's pane for every session that asked.
func TestTheFactoryBuildsOneStagePerSession(t *testing.T) {
	f := Factory(Config{Focuser: &fakeFocuser{}})
	a, b := f(model.Key{SessionID: "a"}), f(model.Key{SessionID: "b"})
	if a == b {
		t.Fatal("the factory returned one instance for two sessions")
	}
	if a.Name() != Name {
		t.Errorf("the stage is named %q, want %q (hosts find it by name)", a.Name(), Name)
	}
}
