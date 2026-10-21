package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// The write path, end to end through the coordinator and an actor: the gate,
// the audit trail, the optional interface and the one live verb.
//
// Nothing here reads ~/.claude, probes a pid or writes outside a temporary
// directory. The registry is staged, the focuser is a fake, and the audit
// trail is a file under t.TempDir() — a default that resolved the real state
// directory would have `go test .` appending to the developer's own trail, and
// that is why pipeline.Config takes an Auditor rather than a path.

// countingFocuser is magmux's pane table, reduced to what a navigate needs.
type countingFocuser struct {
	focused int
	// calls is appended to on an ACTOR's goroutine and read by the test after
	// a command reply, which is a happens-before edge — the same rule the
	// tagger middleware in actor_test.go follows.
	calls []int
}

func (f *countingFocuser) Focus(paneID int) (int, error) {
	f.calls = append(f.calls, paneID)
	previous := f.focused
	f.focused = paneID
	return previous, nil
}

// paneOne answers the pane gate: every session is in pane 1, which is alive.
// It is the smallest locator that makes a navigate possible.
type paneOne struct{}

func (paneOne) PaneFor(pid int) (int, obs.Liveness) { return 1, obs.LivenessYes }
func (paneOne) PaneAlive(paneID int) obs.Liveness   { return obs.LivenessYes }

// trailIn builds a real audit writer under a temporary directory, so that the
// on-disk shape is exercised rather than a fake's idea of it.
func trailIn(t *testing.T) (*act.Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "magmux", act.AuditFileName)
	w, err := act.NewWriter(act.WriterConfig{
		Path: path,
		Now:  func() time.Time { return epoch },
		PID:  7777,
	})
	if err != nil {
		t.Fatalf("act.NewWriter: %v", err)
	}
	return w, path
}

func trailLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

func focusRequest(sessionID string) act.Request {
	return act.Request{
		Action: act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate, Session: sessionID},
		By:     act.RequesterCLI,
	}
}

// TestANavigateReachesTheChainThroughTheGateAndIsRecorded is the whole write
// path in one test: a request arrives, the coordinator resolves the session's
// actor, the gate decides and records, the actor's own goroutine runs the
// optional write half, and the trail on disk says what happened.
func TestANavigateReachesTheChainThroughTheGateAndIsRecorded(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")

	focuser := &countingFocuser{focused: 0}
	trail, path := trailIn(t)
	p := startPipeline(t, Config{
		Root:    dir,
		Locator: paneOne{},
		Focuser: focuser,
		Policy:  act.Default(),
		Audit:   trail,
	})
	// One cycle first, so the session has an actor and a fact set. A request
	// for a session nobody has observed is refused, and that is its own test.
	firstDoc(t, p)

	rep, err := p.Do(context.Background(), focusRequest("id-a"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !rep.Result.Done {
		t.Fatalf("the action reported itself not done: %+v", rep.Result)
	}
	if len(focuser.calls) != 1 || focuser.calls[0] != 1 {
		t.Fatalf("the focuser was called %v, want exactly pane 1", focuser.calls)
	}
	if rep.Decision.Stance != act.StanceAlways {
		t.Errorf("the decision was %+v; R6.1 gives navigate `always`", rep.Decision)
	}

	lines := trailLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("the trail holds %d lines, want an attempt and an outcome:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], `"event":"attempt"`) || !strings.Contains(lines[0], `"outcome":null`) {
		t.Errorf("the first line is not an attempt with a null outcome:\n%s", lines[0])
	}
	if !strings.Contains(lines[1], `"event":"outcome"`) || !strings.Contains(lines[1], `"done":true`) {
		t.Errorf("the second line does not record what happened:\n%s", lines[1])
	}
	for _, want := range []string{`"id":"id-a"`, `"pid":4242`, `"verb":"focus"`,
		`"tier":"navigate"`, `"requestedBy":"cli"`, `"rule":"the built-in default posture`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the attempt line does not carry %s:\n%s", want, lines[0])
		}
	}

	st := p.Stats()
	if st.Actions != 1 || st.ActionsRefused != 0 {
		t.Errorf("stats say %d actions and %d refused", st.Actions, st.ActionsRefused)
	}
}

// TestThePipelineRefusesEveryDeferredTier — the same proof as the act
// package's, made again through the real path, because the thing that could go
// wrong here is different: a pipeline that resolved a verb by asking the actor
// FIRST and the gate second would run a restart and then record that it had
// refused it.
func TestThePipelineRefusesEveryDeferredTier(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "busy")

	focuser := &countingFocuser{}
	trail, path := trailIn(t)
	p := startPipeline(t, Config{
		Root:    dir,
		Locator: paneOne{},
		Focuser: focuser,
		// Every tier set to always, which is the posture a user who wanted no
		// friction would write. Three quarters of it must have no effect.
		Policy: act.Policy{
			Source: "a policy that says always to everything",
			Actions: act.Actions{
				Navigate:  act.StanceAlways,
				Approve:   act.StanceAlways,
				Send:      act.StanceAlways,
				Lifecycle: act.StanceAlways,
			},
		},
		Audit: trail,
	})
	firstDoc(t, p)

	for _, tc := range []struct {
		verb string
		tier act.Tier
		want string
	}{
		{"approve", act.TierApprove, "R5.2"},
		{"send_input", act.TierSend, "R5.3"},
		{"restart", act.TierLifecycle, "R5.4"},
	} {
		req := act.Request{
			Action: act.Action{Verb: tc.verb, Tier: tc.tier, Session: "id-a"},
			By:     act.RequesterMCP,
		}
		_, err := p.Do(context.Background(), req)
		if !errors.Is(err, act.ErrDeferred) {
			t.Fatalf("%s was answered with %v, want a deferral", tc.verb, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("the refusal does not name %s: %v", tc.want, err)
		}
	}
	if len(focuser.calls) != 0 {
		t.Fatalf("a deferred tier reached the pane table: %v", focuser.calls)
	}

	lines := trailLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("three refusals produced %d lines", len(lines))
	}
	for _, line := range lines {
		if !strings.Contains(line, `"event":"refused"`) {
			t.Errorf("a deferred tier produced a line that is not a refusal:\n%s", line)
		}
		if !strings.Contains(line, "hard-disabled in code") {
			t.Errorf("the refusal does not say what refused it:\n%s", line)
		}
	}
	if st := p.Stats(); st.ActionsRefused != 3 {
		t.Errorf("stats say %d refused, want 3", st.ActionsRefused)
	}
}

// TestASessionWithNoPaneDeclaresNothing — `magmux agents` running outside a
// magmux, and every session on a machine magmux did not start. The refusal is
// "cannot", not "will not": nothing declares the verb, because there is no
// pane to focus.
func TestASessionWithNoPaneDeclaresNothing(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "busy")

	trail, path := trailIn(t)
	// No Locator and no Focuser: the honest shape of a process that is not a
	// magmux.
	p := startPipeline(t, Config{Root: dir, Policy: act.Default(), Audit: trail})
	firstDoc(t, p)

	_, err := p.Do(context.Background(), focusRequest("id-a"))
	if !errors.Is(err, act.ErrNotCapable) {
		t.Fatalf("a session with no pane was answered with %v, want `cannot`", err)
	}
	if errors.Is(err, act.ErrForbidden) {
		t.Fatal("a session with no pane was refused as a policy matter; `cannot` and " +
			"`will not` are different answers and a user will go looking for a config file")
	}
	if lines := trailLines(t, path); len(lines) != 1 || !strings.Contains(lines[0], `"event":"refused"`) {
		t.Errorf("the refusal was not recorded: %v", lines)
	}
}

// TestAnActionForASessionNobodyHasObservedIsRefusedAndRecorded — a typo, a
// session that has exited, or a document a client is holding from ten minutes
// ago. There is no actor, so nothing declares anything, and the refusal names
// the session that was asked for rather than silently doing nothing.
func TestAnActionForASessionNobodyHasObservedIsRefusedAndRecorded(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "busy")

	trail, path := trailIn(t)
	p := startPipeline(t, Config{
		Root: dir, Locator: paneOne{}, Focuser: &countingFocuser{},
		Policy: act.Default(), Audit: trail,
	})
	firstDoc(t, p)

	_, err := p.Do(context.Background(), focusRequest("id-that-never-was"))
	if !errors.Is(err, act.ErrNotCapable) {
		t.Fatalf("an unknown session was answered with %v", err)
	}
	lines := trailLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("the trail holds %d lines", len(lines))
	}
	if !strings.Contains(lines[0], `"id":"id-that-never-was"`) {
		t.Errorf("the refusal does not say what was asked for:\n%s", lines[0])
	}
}

// TestAPipelineThatWasNeverGivenAPolicyCannotAct is the zero-value rule at the
// level a caller actually builds: a Config literal that says nothing about
// policy or audit.
//
// A WRONG IMPLEMENTATION defaults to act.Default() here "for convenience".
// Every test in this repository builds its structs as literals, so that
// convenience would make every one of them permissive, and the permissiveness
// would be invisible in all of them.
// Two cases, and the FIRST one is the load-bearing half. An earlier version of
// this test built a pipeline with neither a policy nor an audit trail and
// accepted either refusal — and it passed with a deliberately permissive zero
// policy, because the missing trail refused first and the policy was never
// consulted. That is the brief's warning made real: a permissive zero value is
// invisible in exactly the tests that would otherwise catch it. So the policy
// case is given a working trail, leaving the policy as the only thing that can
// refuse.
func TestAPipelineThatWasNeverGivenAPolicyCannotAct(t *testing.T) {
	t.Run("no policy, but a working trail", func(t *testing.T) {
		dir := newRegistry(t)
		writeRecord(t, dir, 4242, "id-a", "session-a", "busy")

		focuser := &countingFocuser{}
		trail, _ := trailIn(t)
		p := startPipeline(t, Config{Root: dir, Locator: paneOne{}, Focuser: focuser, Audit: trail})
		firstDoc(t, p)

		_, err := p.Do(context.Background(), focusRequest("id-a"))
		if !errors.Is(err, act.ErrForbidden) {
			t.Fatalf("a pipeline whose Config says nothing about policy answered with %v, "+
				"want a refusal from the policy", err)
		}
		if len(focuser.calls) != 0 {
			t.Fatalf("the pane table was reached: %v", focuser.calls)
		}
	})

	// The mirror: a policy that ALLOWS, and no trail to record it in. The
	// policy has to allow, or the refusal would come from step 5 and this
	// would prove nothing about step 6 — which is the same trap the case
	// above fell into, in the other direction.
	t.Run("an allowing policy, and no trail", func(t *testing.T) {
		dir := newRegistry(t)
		writeRecord(t, dir, 4242, "id-a", "session-a", "busy")

		focuser := &countingFocuser{}
		p := startPipeline(t, Config{
			Root: dir, Locator: paneOne{}, Focuser: focuser, Policy: act.Default(),
		})
		firstDoc(t, p)

		_, err := p.Do(context.Background(), focusRequest("id-a"))
		if !errors.Is(err, act.ErrUnaudited) {
			t.Fatalf("a pipeline with no audit trail answered with %v", err)
		}
		if len(focuser.calls) != 0 {
			t.Fatalf("the pane table was reached: %v", focuser.calls)
		}
	})
}

// TestAnActionIsOrderedWithTheObservationsAroundIt — events and commands share
// one channel per actor, so the fact set an action is decided against is the
// one the session had when the gate looked, and an action cannot overtake a
// chain run that is in flight.
//
// The blocker holds the chain; the action queues behind it; releasing the
// chain lets both through in order.
func TestAnActionIsOrderedWithTheObservationsAroundIt(t *testing.T) {
	block := &blocker{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	a := mustStartActor(t, "s1", factoryFor(block), factoryFor(&navStub{pane: 1}))

	if !a.Observe(sessionWithID("s1", 4242)) {
		t.Fatal("the actor refused an observation")
	}
	<-block.entered

	done := make(chan error, 1)
	go func() {
		_, err := a.perform(context.Background(), act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the action overtook a chain run that was still in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(block.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the action failed once the chain was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the action never completed")
	}
}

// navStub is an actuating middleware with no dependencies: it declares focus
// and records that it ran. It exists so that ordering and routing can be
// tested without a pane table.
type navStub struct {
	pane  int
	calls int
}

func (n *navStub) Name() string                      { return "nav-stub" }
func (n *navStub) Observe(s obs.Session) obs.Session { return s }

func (n *navStub) Capabilities() []act.Capability {
	return []act.Capability{{Verb: act.VerbFocus, Tier: act.TierNavigate, Reversible: true, Idempotent: true}}
}

func (n *navStub) Do(context.Context, act.Action) (act.Result, error) {
	n.calls++
	return act.Result{Done: true, Detail: "the stub did it"}, nil
}

// TestOnlyTheStagesThatOptInAreAskedToAct — the optional interface, asserted
// the way it will actually be used: a chain of ordinary read-only middleware
// with one actuating stage in it.
func TestOnlyTheStagesThatOptInAreAskedToAct(t *testing.T) {
	reader := &tagger{name: "reader"}
	writer := &navStub{pane: 1}
	a := mustStartActor(t, "s1", factoryFor(reader), factoryFor(writer))

	caps, ok := a.capabilities()
	if !ok {
		t.Fatal("the actor has stopped")
	}
	if len(caps) != 1 || caps[0].Verb != act.VerbFocus {
		t.Fatalf("the chain declares %+v; only the actuating stage should", caps)
	}

	if _, err := a.perform(context.Background(), act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate}); err != nil {
		t.Fatalf("perform: %v", err)
	}
	if writer.calls != 1 {
		t.Fatalf("the actuating stage ran %d times", writer.calls)
	}
	if reader.runs != 0 {
		t.Fatal("a read-only stage was run by an action")
	}

	stats, _ := a.Stats()
	if stats.Acted != 1 {
		t.Errorf("the actor counted %d actions", stats.Acted)
	}
}

// TestAnActorWithNoWriteHalfDeclaresNothing — every chain outside magmux, and
// every chain built before this phase. The answer is an empty declaration and
// a refusal, never a panic and never a silent success.
func TestAnActorWithNoWriteHalfDeclaresNothing(t *testing.T) {
	a := mustStartActor(t, "s1", factoryFor(&tagger{name: "reader"}))

	caps, ok := a.capabilities()
	if !ok || len(caps) != 0 {
		t.Fatalf("a read-only chain declared %+v", caps)
	}
	_, err := a.perform(context.Background(), act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate})
	if !errors.Is(err, act.ErrNotCapable) {
		t.Fatalf("a read-only chain answered an action with %v", err)
	}
}
