package pipeline

import (
	"runtime"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// ── test middleware ─────────────────────────────────────────────────────────

// tagger appends its own name to the session's PreviousNames, which is the
// cheapest observable proof of "this stage ran, and it ran here in the order".
type tagger struct {
	name string
	// runs counts calls. It is written on the actor's goroutine and read by
	// the test only after a command reply, which is a happens-before edge.
	runs int
}

func (t *tagger) Name() string { return t.name }

func (t *tagger) Observe(s obs.Session) obs.Session {
	t.runs++
	s.PreviousNames = append(append([]string{}, s.PreviousNames...), t.name)
	return s
}

// blocker holds the chain until the test lets it go. It is how "the actor is
// busy" is expressed without a sleep.
type blocker struct {
	release chan struct{}
	entered chan struct{}
}

func (b *blocker) Name() string { return "blocker" }

func (b *blocker) Observe(s obs.Session) obs.Session {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return s
}

func factoryFor(mw Middleware) Factory {
	return func(string) Middleware { return mw }
}

func sessionWithID(id string, pid int) obs.Session {
	return obs.Session{ID: id, PID: pid}
}

// mustStartActor starts an actor and stops it at the end of the test.
func mustStartActor(t *testing.T, id string, factories ...Factory) *Actor {
	t.Helper()
	a, err := StartActor(id, factories)
	if err != nil {
		t.Fatalf("StartActor: %v", err)
	}
	t.Cleanup(a.Stop)
	return a
}

// ── tests ───────────────────────────────────────────────────────────────────

// TestActorRunsItsChainInOrder pins the one thing a chain is for.
//
// A WRONG IMPLEMENTATION that ranged over a map of middleware, or that
// prepended instead of appended, would pass every other test here: each stage
// still runs, the fact set still comes out the far end. What breaks is a chain
// where a later stage depends on an earlier one — the pane gate resolving the
// pane that a tool middleware then uses to read a transcript — and it breaks
// intermittently, which is the worst available failure.
func TestActorRunsItsChainInOrder(t *testing.T) {
	first, second, third := &tagger{name: "first"}, &tagger{name: "second"}, &tagger{name: "third"}
	a := mustStartActor(t, "s1", factoryFor(first), factoryFor(second), factoryFor(third))

	if !a.Observe(sessionWithID("s1", 10)) {
		t.Fatal("the actor refused an observation")
	}
	got, ok := a.Snapshot()
	if !ok {
		t.Fatal("Snapshot after Observe reported the actor stopped")
	}
	want := []string{"first", "second", "third"}
	if len(got.PreviousNames) != len(want) {
		t.Fatalf("chain produced %v, want %v", got.PreviousNames, want)
	}
	for i := range want {
		if got.PreviousNames[i] != want[i] {
			t.Fatalf("chain produced %v, want %v", got.PreviousNames, want)
		}
	}
}

// TestSnapshotSeesTheObservationJustSent pins the FIFO ordering that events
// and commands share one channel for.
//
// A WRONG IMPLEMENTATION with two channels and a select passes this most of
// the time: the select only picks the command first when both are ready at
// once, which is exactly what happens under load and never in a quiet test.
// This runs the round trip enough times that a random choice would have to be
// lucky a hundred times running.
func TestSnapshotSeesTheObservationJustSent(t *testing.T) {
	a := mustStartActor(t, "s1", factoryFor(&tagger{name: "tag"}))
	for i := 1; i <= 100; i++ {
		s := sessionWithID("s1", i)
		if !a.Observe(s) {
			t.Fatalf("observation %d refused", i)
		}
		got, ok := a.Snapshot()
		if !ok {
			t.Fatalf("snapshot %d reported the actor stopped", i)
		}
		if got.PID != i {
			t.Fatalf("snapshot after observation %d reported pid %d — the command overtook the event",
				i, got.PID)
		}
	}
}

// mutator is freeze point 1's adversary: a middleware that keeps a pointer to
// something it set and writes through it on a LATER pass.
type mutator struct {
	kept *int
}

func (m *mutator) Name() string { return "mutator" }

func (m *mutator) Observe(s obs.Session) obs.Session {
	if m.kept != nil {
		// Writing into the fact set of the PREVIOUS run, through a pointer the
		// chain still holds. This is exactly what a real middleware does by
		// accident when it caches "the value I computed last time".
		*m.kept = 9999
	}
	v := int(s.PID)
	m.kept = &v
	s.PaneID = &v
	return s
}

// TestFreezePointOneMakesThePublishedFactSetUnreachable is freeze point 1.
//
// A WRONG IMPLEMENTATION — storing the chain's own output instead of a clone
// of it — passes every other test in this file. It fails only when a
// middleware keeps a pointer, which [obs.Session] invites: PaneID,
// SubagentCount, Color, Attention.Needed and Fault.APIErrors are all pointers,
// and a stage that remembers the one it made will write through it. The
// symptom in production is a document that was already published changing
// under a reader, which no consumer can defend against and none would suspect.
func TestFreezePointOneMakesThePublishedFactSetUnreachable(t *testing.T) {
	a := mustStartActor(t, "s1", factoryFor(&mutator{}))

	a.Observe(sessionWithID("s1", 42))
	first, ok := a.Snapshot()
	if !ok {
		t.Fatal("first snapshot reported the actor stopped")
	}
	if first.PaneID == nil || *first.PaneID != 42 {
		t.Fatalf("the chain did not set PaneID: %v", first.PaneID)
	}

	// The second pass writes through the pointer it kept from the first.
	a.Observe(sessionWithID("s1", 43))
	if _, ok := a.Snapshot(); !ok {
		t.Fatal("second snapshot reported the actor stopped")
	}

	if *first.PaneID != 42 {
		t.Fatalf("the already-published fact set changed under its reader: PaneID is now %d. "+
			"The chain's copy is still reachable from the actor — freeze point 1 is not frozen",
			*first.PaneID)
	}
}

// TestEachActorBuildsItsOwnChain pins "no middleware instance is ever shared
// between sessions", which is unanimous across all four architecture plans.
//
// A WRONG IMPLEMENTATION takes built middleware instead of factories, and a
// caller then quite reasonably builds the chain once and hands it to every
// actor. Every stage still runs and every test above still passes; what
// happens instead is that the pane gate answers session B with session A's
// pane, which is a wrong answer that looks exactly like a right one.
func TestEachActorBuildsItsOwnChain(t *testing.T) {
	var built []Middleware
	factory := func(id string) Middleware {
		mw := &tagger{name: "per-" + id}
		built = append(built, mw)
		return mw
	}

	a := mustStartActor(t, "sessionA", factory)
	b := mustStartActor(t, "sessionB", factory)

	if len(built) != 2 {
		t.Fatalf("the factory was called %d times for 2 actors", len(built))
	}
	if built[0] == built[1] {
		t.Fatal("both actors were given the same middleware instance")
	}
	if built[0].Name() != "per-sessionA" || built[1].Name() != "per-sessionB" {
		t.Fatalf("a factory was not told which session it was building for: %q, %q",
			built[0].Name(), built[1].Name())
	}

	a.Observe(sessionWithID("sessionA", 1))
	b.Observe(sessionWithID("sessionB", 2))
	sa, _ := a.Snapshot()
	sb, _ := b.Snapshot()
	if sa.PreviousNames[0] != "per-sessionA" || sb.PreviousNames[0] != "per-sessionB" {
		t.Fatalf("a chain ran the other session's middleware: %v / %v", sa.PreviousNames, sb.PreviousNames)
	}
}

// TestStatsReportTheChainAndTheRuns proves the read-through-a-command path
// carries more than the fact set.
func TestStatsReportTheChainAndTheRuns(t *testing.T) {
	a := mustStartActor(t, "s1", factoryFor(&tagger{name: "one"}), nil, factoryFor(&tagger{name: "two"}))

	st, ok := a.Stats()
	if !ok {
		t.Fatal("Stats reported the actor stopped")
	}
	if st.Stages != 2 {
		t.Fatalf("Stages is %d, want 2 — a nil factory must be skipped, not counted", st.Stages)
	}
	if st.Observed != 0 {
		t.Fatalf("Observed is %d before any observation", st.Observed)
	}

	a.Observe(sessionWithID("s1", 1))
	a.Observe(sessionWithID("s1", 2))
	if st, _ = a.Stats(); st.Observed != 2 {
		t.Fatalf("Observed is %d after two observations", st.Observed)
	}
}

// TestObserveDoesNotBlockOnABusyActor pins the event half of the contract: a
// caller that reports an observation never waits for a chain to run.
//
// The coordinator observes into every actor and only then collects from any of
// them, so a blocking Observe would serialise every session behind the slowest
// one — and the slowest one is the one reading a 600 MB transcript.
func TestObserveDoesNotBlockOnABusyActor(t *testing.T) {
	b := &blocker{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	a := mustStartActor(t, "s1", factoryFor(b))
	defer close(b.release)

	a.Observe(sessionWithID("s1", 1))
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the chain never started; the fixture is wrong, not the actor")
	}

	// The actor is now wedged inside the chain. Filling the backlog must
	// return, not block, and must report the refusals.
	done := make(chan int, 1)
	go func() {
		refused := 0
		for i := 0; i < actorBacklog*4; i++ {
			if !a.Observe(sessionWithID("s1", i)) {
				refused++
			}
		}
		done <- refused
	}()

	select {
	case refused := <-done:
		if refused == 0 {
			t.Fatalf("%d observations into a backlog of %d and none was refused — "+
				"either the backlog is unbounded or the drop is not reported",
				actorBacklog*4, actorBacklog)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Observe blocked on a busy actor")
	}
}

// TestStopIsIdempotentAndLeaksNoGoroutine is the failure this design is most
// likely to have: one goroutine per session, on a machine that starts and
// finishes agents all day.
func TestStopIsIdempotentAndLeaksNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()

	a, err := StartActor("s1", []Factory{factoryFor(&tagger{name: "tag"})})
	if err != nil {
		t.Fatalf("StartActor: %v", err)
	}
	a.Observe(sessionWithID("s1", 1))
	if _, ok := a.Snapshot(); !ok {
		t.Fatal("the actor stopped before it was told to")
	}

	// From a different goroutine than the one that started it, twice, plus a
	// third from here. None of them may panic and none may hang.
	stopped := make(chan struct{})
	go func() {
		a.Stop()
		a.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	a.Stop()

	if after := settledGoroutines(before, 2*time.Second); after > before {
		t.Errorf("goroutines went from %d to %d across an actor's whole life", before, after)
	}
}

// TestCommandsAfterStopReportStoppedRatherThanHanging pins the other half of
// Stop: a caller that asks a dead actor a question gets an answer.
func TestCommandsAfterStopReportStoppedRatherThanHanging(t *testing.T) {
	a, err := StartActor("s1", nil)
	if err != nil {
		t.Fatalf("StartActor: %v", err)
	}
	a.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := a.Snapshot(); ok {
			t.Error("Snapshot on a stopped actor reported success")
		}
		if _, ok := a.Stats(); ok {
			t.Error("Stats on a stopped actor reported success")
		}
		if a.Observe(sessionWithID("s1", 1)) {
			t.Error("Observe on a stopped actor reported success")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a command to a stopped actor never returned")
	}
}

// TestAnActorNeedsAnIdentity — the coordinator keys its table by it, and two
// unidentifiable sessions would otherwise share one actor.
func TestAnActorNeedsAnIdentity(t *testing.T) {
	if _, err := StartActor("", nil); err == nil {
		t.Fatal("an actor with no session id was accepted")
	}
}

// settledGoroutines waits for the count to come back down to want, and returns
// what it settled at. Goroutines exit asynchronously — the one being waited on
// has closed its done channel before its stack is gone — so an immediate
// comparison is a flake generator.
func settledGoroutines(want int, within time.Duration) int {
	deadline := time.Now().Add(within)
	n := runtime.NumGoroutine()
	for n > want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}
