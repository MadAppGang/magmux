package pipeline

import (
	"testing"

	"github.com/MadAppGang/magmux/internal/obs"
)

// fakeLocator is a pane table without a magmux. It counts its calls, because
// the middleware's caching rule is stated in terms of which question is
// re-asked and which is not.
type fakeLocator struct {
	// pane answers PaneFor: pid -> pane id. A pid that is absent is a
	// determined miss unless unknownFor says otherwise.
	pane map[int]int
	// unknownFor makes PaneFor answer LivenessUnknown for a pid, which is what
	// an unreadable process tree looks like.
	unknownFor map[int]bool
	// alive answers PaneAlive.
	alive map[int]obs.Liveness

	forCalls   int
	aliveCalls int
}

func (f *fakeLocator) PaneFor(pid int) (int, obs.Liveness) {
	f.forCalls++
	if f.unknownFor[pid] {
		return 0, obs.LivenessUnknown
	}
	if id, ok := f.pane[pid]; ok {
		return id, obs.LivenessYes
	}
	return 0, obs.LivenessNo
}

func (f *fakeLocator) PaneAlive(paneID int) obs.Liveness {
	f.aliveCalls++
	if l, ok := f.alive[paneID]; ok {
		return l
	}
	return obs.LivenessUnknown
}

// staticLocator is the same answers with no counters, for the tests that run a
// whole pipeline: ONE locator is shared by every actor goroutine, so anything
// it mutates is a data race in the test's own fixture. That is a real property
// of the port and it is documented on obs.PaneLocator — the middleware in
// front of it is per-session, the locator behind it is not.
type staticLocator struct {
	pane  map[int]int
	alive map[int]obs.Liveness
}

func (s staticLocator) PaneFor(pid int) (int, obs.Liveness) {
	if id, ok := s.pane[pid]; ok {
		return id, obs.LivenessYes
	}
	return 0, obs.LivenessNo
}

func (s staticLocator) PaneAlive(paneID int) obs.Liveness {
	if l, ok := s.alive[paneID]; ok {
		return l
	}
	return obs.LivenessUnknown
}

// TestPaneGateFillsBothPaneFacts — the second liveness gate, which phase 2
// deliberately left undetermined.
func TestPaneGateFillsBothPaneFacts(t *testing.T) {
	loc := &fakeLocator{
		pane:  map[int]int{4242: 3},
		alive: map[int]obs.Liveness{3: obs.LivenessYes},
	}
	mw := PaneGate(loc)("s1")

	got := mw.Observe(obs.Session{ID: "s1", PID: 4242})
	if got.PaneID == nil || *got.PaneID != 3 {
		t.Fatalf("PaneID is %v, want 3", got.PaneID)
	}
	if got.PaneAlive != obs.LivenessYes {
		t.Fatalf("PaneAlive is %v, want yes", got.PaneAlive)
	}
}

// TestPaneGateRemembersTheMatchAndReasksTheLiveness pins the asymmetry, which
// is the whole reason this middleware has state.
//
// Ancestry is fixed at fork time, so the pane that owns a process owns it for
// as long as it lives and the walk — a sysctl per hop — need not be repeated.
// Whether that pane is still THERE is the half that changes, and caching it
// would report a closed pane as alive for the rest of the session's life.
func TestPaneGateRemembersTheMatchAndReasksTheLiveness(t *testing.T) {
	loc := &fakeLocator{
		pane:  map[int]int{4242: 3},
		alive: map[int]obs.Liveness{3: obs.LivenessYes},
	}
	mw := PaneGate(loc)("s1")

	for i := 0; i < 5; i++ {
		mw.Observe(obs.Session{ID: "s1", PID: 4242})
	}
	if loc.forCalls != 1 {
		t.Errorf("the ancestry walk ran %d times for 5 observations of one session", loc.forCalls)
	}
	if loc.aliveCalls != 5 {
		t.Errorf("the pane liveness was asked %d times for 5 observations", loc.aliveCalls)
	}

	// The pane is closed underneath the session. PaneID goes on naming it —
	// "this session ran in pane 3, and pane 3 is gone" is a different
	// situation from "this session was never one of ours" — and the gate says
	// so.
	loc.alive[3] = obs.LivenessNo
	got := mw.Observe(obs.Session{ID: "s1", PID: 4242})
	if got.PaneAlive != obs.LivenessNo {
		t.Fatalf("PaneAlive is %v after the pane was closed, want no", got.PaneAlive)
	}
	if got.PaneID == nil || *got.PaneID != 3 {
		t.Fatalf("PaneID is %v after the pane was closed; the rejecting gate must still name it", got.PaneID)
	}
}

// TestPaneGateLeavesTheFactsAloneOnAMiss — a session with no pane is still a
// session.
//
// Nothing is filtered here and nothing is asserted: a nil PaneID already means
// "not one of magmux's panes", which is not hypothetical — discovery sees
// sessions magmux did not start — and PaneAlive stays undetermined because it
// is a fact about a pane and there is no pane to have one. Writing
// LivenessNo would tell a consumer that a pane had VANISHED.
func TestPaneGateLeavesTheFactsAloneOnAMiss(t *testing.T) {
	loc := &fakeLocator{pane: map[int]int{}}
	mw := PaneGate(loc)("s1")

	got := mw.Observe(obs.Session{ID: "s1", PID: 777})
	if got.PaneID != nil {
		t.Errorf("PaneID is %v for a session with no pane, want nil", *got.PaneID)
	}
	if got.PaneAlive != obs.LivenessUnknown {
		t.Errorf("PaneAlive is %v for a session with no pane, want undetermined — "+
			"\"no\" would mean a pane had vanished", got.PaneAlive)
	}
	if loc.aliveCalls != 0 {
		t.Errorf("the liveness of a pane that does not exist was asked %d times", loc.aliveCalls)
	}
}

// TestPaneGateRetriesAfterAnUndeterminedWalk — a failure is never cached.
//
// An unreadable /proc entry, a restricted container, a transient sysctl
// failure: caching any of those would turn one bad moment into a permanently
// pane-less session, and nothing anywhere would say why.
func TestPaneGateRetriesAfterAnUndeterminedWalk(t *testing.T) {
	loc := &fakeLocator{
		pane:       map[int]int{4242: 3},
		unknownFor: map[int]bool{4242: true},
		alive:      map[int]obs.Liveness{3: obs.LivenessYes},
	}
	mw := PaneGate(loc)("s1")

	if got := mw.Observe(obs.Session{ID: "s1", PID: 4242}); got.PaneID != nil {
		t.Fatalf("an undetermined walk produced a pane id: %v", *got.PaneID)
	}

	// The tree becomes readable.
	delete(loc.unknownFor, 4242)
	got := mw.Observe(obs.Session{ID: "s1", PID: 4242})
	if got.PaneID == nil || *got.PaneID != 3 {
		t.Fatalf("the walk was not retried after an undetermined answer: PaneID %v", got.PaneID)
	}
	if loc.forCalls != 2 {
		t.Fatalf("the walk ran %d times, want 2 — an undetermined answer must not be cached", loc.forCalls)
	}
}

// TestPaneGateRetriesAfterAMissBecauseThePaneTableGrows — an agent's
// open_pane, or the panel being revealed, adds panes after a session was
// first seen.
func TestPaneGateRetriesAfterAMissBecauseThePaneTableGrows(t *testing.T) {
	loc := &fakeLocator{pane: map[int]int{}, alive: map[int]obs.Liveness{7: obs.LivenessYes}}
	mw := PaneGate(loc)("s1")

	mw.Observe(obs.Session{ID: "s1", PID: 4242})
	loc.pane[4242] = 7
	got := mw.Observe(obs.Session{ID: "s1", PID: 4242})
	if got.PaneID == nil || *got.PaneID != 7 {
		t.Fatalf("a miss was cached: PaneID is %v after the pane appeared", got.PaneID)
	}
}

// TestPaneGateInstancesDoNotShareAMemory is the per-session rule at the one
// place it has teeth: this middleware remembers a pane, and a shared instance
// would answer every session with the first one's.
func TestPaneGateInstancesDoNotShareAMemory(t *testing.T) {
	loc := &fakeLocator{
		pane:  map[int]int{100: 1, 200: 2},
		alive: map[int]obs.Liveness{1: obs.LivenessYes, 2: obs.LivenessYes},
	}
	factory := PaneGate(loc)
	a, b := factory("sessionA"), factory("sessionB")
	if a == b {
		t.Fatal("the factory returned the same instance twice")
	}

	ga := a.Observe(obs.Session{ID: "sessionA", PID: 100})
	gb := b.Observe(obs.Session{ID: "sessionB", PID: 200})
	if ga.PaneID == nil || *ga.PaneID != 1 || gb.PaneID == nil || *gb.PaneID != 2 {
		t.Fatalf("panes resolved as %v and %v, want 1 and 2", ga.PaneID, gb.PaneID)
	}
}

// TestNilLocatorChangesNothing — running outside a magmux, where there is no
// pane table to consult. `magmux agents` is exactly that case.
func TestNilLocatorChangesNothing(t *testing.T) {
	mw := PaneGate(nil)("s1")
	in := obs.Session{ID: "s1", PID: 4242, PaneAlive: obs.LivenessUnknown}
	got := mw.Observe(in)
	if got.PaneID != nil || got.PaneAlive != obs.LivenessUnknown {
		t.Fatalf("a nil locator produced pane facts: %+v", got)
	}
	if mw.Name() != "pane" {
		t.Errorf("the stage is named %q", mw.Name())
	}
}
