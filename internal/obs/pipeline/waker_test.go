package pipeline

import (
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// wakerStage is a middleware that asks to be woken after a fixed delay, so the
// actor's alarm can be tested without a cascade, a pane or a clock reading.
//
// deadlines is drained one per observation: a stage can therefore ask for one
// thing on the first run and something else on the second, which is how a real
// stage behaves as its silence grows.
type wakerStage struct {
	name      string
	deadlines []time.Duration
	seen      int
}

func (w *wakerStage) Name() string { return w.name }

func (w *wakerStage) Observe(s obs.Session) obs.Session {
	w.seen++
	return s
}

func (w *wakerStage) WakeIn() (time.Duration, bool) {
	i := w.seen - 1
	if i < 0 || i >= len(w.deadlines) {
		return 0, false
	}
	d := w.deadlines[i]
	return d, d > 0
}

// plainStage implements Middleware and NOT Waker, which is what every stage
// written before this phase looks like.
type plainStage struct{ name string }

func (p plainStage) Name() string                      { return p.name }
func (p plainStage) Observe(s obs.Session) obs.Session { return s }

// wakeCounter records wakes over a channel rather than a counter, because the
// actor calls Wake on ITS goroutine and this layer has no mutexes. A buffered
// channel with a non-blocking send is the same shape the pipeline's own wake
// uses, for the same reason.
type wakeCounter chan struct{}

func (w wakeCounter) fn() func() {
	return func() {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

func (w wakeCounter) await(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-w:
	case <-time.After(within):
		t.Fatal("the alarm came due and the actor never asked for a cycle")
	}
}

func (w wakeCounter) none(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-w:
		t.Fatal("a cycle was asked for when nothing should have woken")
	case <-time.After(within):
	}
}

// TestAWakerStageArmsOneAlarmAndTheActorAsksForACycle is the whole mechanism:
// a stage says when its answer changes, the actor waits exactly that long, and
// waking means asking for a cycle rather than running the chain again.
//
// A WRONG IMPLEMENTATION re-runs the chain on the actor's own goroutine and
// publishes nothing, so the staleness is recorded where nobody can see it — the
// document a sink receives only ever comes from a cycle.
func TestAWakerStageArmsOneAlarmAndTheActorAsksForACycle(t *testing.T) {
	clock := obs.NewFakeClock(time.Unix(1789545200, 0))
	wake := make(wakeCounter, 4)
	stage := &wakerStage{name: "waker", deadlines: []time.Duration{5 * time.Second}}

	a, err := StartActorWith("s1", []Factory{factoryFor(stage)},
		ActorConfig{Clock: clock, Wake: wake.fn()})
	if err != nil {
		t.Fatalf("StartActorWith: %v", err)
	}
	defer a.Stop()

	a.Observe(obs.Session{ID: "s1"})

	ft, err := clock.NextTimer(2 * time.Second)
	if err != nil {
		t.Fatalf("no alarm was armed: %v", err)
	}
	if ft.Delay() != 5*time.Second {
		t.Fatalf("the alarm was armed for %s, want the 5s the stage asked for", ft.Delay())
	}
	wake.none(t, 50*time.Millisecond)

	ft.Fire(time.Unix(1789545205, 0))
	wake.await(t, 2*time.Second)

	st, ok := a.Stats()
	if !ok || st.Woken != 1 {
		t.Errorf("stats report %d wakes, want 1", st.Woken)
	}
	// One-shot. The alarm is not re-armed by its own firing — the cycle it
	// asked for is what observes again and decides whether to wait further.
	if err := clock.NoTimerWithin(100 * time.Millisecond); err != nil {
		t.Errorf("%v — an alarm re-armed itself, which is a ticker", err)
	}
}

// TestAnActorWithNothingToWaitForHoldsNoAlarm — the claim that an idle machine
// costs nothing, stated where it can fail.
func TestAnActorWithNothingToWaitForHoldsNoAlarm(t *testing.T) {
	for _, tt := range []struct {
		name  string
		chain []Factory
	}{
		{"no waker in the chain", []Factory{factoryFor(plainStage{name: "plain"})}},
		{"a waker with nothing to wait for", []Factory{factoryFor(&wakerStage{name: "w"})}},
		{"an empty chain", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clock := obs.NewFakeClock(time.Unix(1789545200, 0))
			wake := make(wakeCounter, 4)
			a, err := StartActorWith("s1", tt.chain, ActorConfig{Clock: clock, Wake: wake.fn()})
			if err != nil {
				t.Fatalf("StartActorWith: %v", err)
			}
			defer a.Stop()

			a.Observe(obs.Session{ID: "s1"})
			if _, ok := a.Snapshot(); !ok {
				t.Fatal("the actor stopped")
			}
			if err := clock.NoTimerWithin(100 * time.Millisecond); err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

// TestANonPositiveDeadlineIsRefused.
//
// A WRONG IMPLEMENTATION clamps it to zero and arms an alarm that is already
// due: it fires at once, asks for a cycle, and that cycle arms it again. A poll
// loop with a one-shot timer's name on it, spinning as fast as the machine
// allows.
func TestANonPositiveDeadlineIsRefused(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			clock := obs.NewFakeClock(time.Unix(1789545200, 0))
			stage := &wakerStage{name: "w", deadlines: []time.Duration{d}}
			// The stage's own guard is bypassed on purpose: this asserts the
			// ACTOR refuses it, so the two guards are independent.
			stage.deadlines = []time.Duration{d}
			a, err := StartActorWith("s1", []Factory{factoryFor(stage)}, ActorConfig{Clock: clock})
			if err != nil {
				t.Fatalf("StartActorWith: %v", err)
			}
			defer a.Stop()

			a.Observe(obs.Session{ID: "s1"})
			a.Snapshot()
			if err := clock.NoTimerWithin(100 * time.Millisecond); err != nil {
				t.Fatalf("%v — an alarm for %s would spin", err, d)
			}
		})
	}
}

// TestTheEarliestDeadlineWinsAndTheOldAlarmIsReleased.
//
// A WRONG IMPLEMENTATION arms one alarm per Waker, so a chain with two of them
// wakes twice for one deadline; or it re-arms without stopping the old alarm,
// and the released one is still holding a runtime timer.
func TestTheEarliestDeadlineWinsAndTheOldAlarmIsReleased(t *testing.T) {
	clock := obs.NewFakeClock(time.Unix(1789545200, 0))
	slow := &wakerStage{name: "slow", deadlines: []time.Duration{time.Hour, time.Hour}}
	fast := &wakerStage{name: "fast", deadlines: []time.Duration{30 * time.Second, time.Minute}}

	a, err := StartActorWith("s1", []Factory{factoryFor(slow), factoryFor(fast)},
		ActorConfig{Clock: clock})
	if err != nil {
		t.Fatalf("StartActorWith: %v", err)
	}
	defer a.Stop()

	a.Observe(obs.Session{ID: "s1"})
	first, err := clock.NextTimer(2 * time.Second)
	if err != nil {
		t.Fatalf("no alarm: %v", err)
	}
	if first.Delay() != 30*time.Second {
		t.Fatalf("armed for %s, want the earliest of 30s and 1h", first.Delay())
	}
	if err := clock.NoTimerWithin(50 * time.Millisecond); err != nil {
		t.Fatalf("%v — one alarm per Waker instead of one per actor", err)
	}

	// A second observation re-arms, and must release the first.
	a.Observe(obs.Session{ID: "s1"})
	second, err := clock.NextTimer(2 * time.Second)
	if err != nil {
		t.Fatalf("no alarm on the second observation: %v", err)
	}
	if second.Delay() != time.Minute {
		t.Errorf("re-armed for %s, want 1m", second.Delay())
	}
	if !first.Stopped() {
		t.Error("the replaced alarm was never stopped; a re-arm leaked its predecessor")
	}
}

// TestStoppingAnActorReleasesItsAlarm — one per session, on a machine that
// starts and finishes agents all day.
func TestStoppingAnActorReleasesItsAlarm(t *testing.T) {
	clock := obs.NewFakeClock(time.Unix(1789545200, 0))
	stage := &wakerStage{name: "w", deadlines: []time.Duration{time.Hour}}
	a, err := StartActorWith("s1", []Factory{factoryFor(stage)}, ActorConfig{Clock: clock})
	if err != nil {
		t.Fatalf("StartActorWith: %v", err)
	}
	a.Observe(obs.Session{ID: "s1"})
	ft, err := clock.NextTimer(2 * time.Second)
	if err != nil {
		t.Fatalf("no alarm: %v", err)
	}

	a.Stop()
	if !ft.Stopped() {
		t.Fatal("a stopped actor left its alarm running")
	}
}

// TestAnActorWithNoClockNeverWaits — the zero [ActorConfig], which is what
// [StartActor] gives and therefore what every caller written before this phase
// gets.
func TestAnActorWithNoClockNeverWaits(t *testing.T) {
	stage := &wakerStage{name: "w", deadlines: []time.Duration{time.Second, time.Second}}
	a, err := StartActor("s1", []Factory{factoryFor(stage)})
	if err != nil {
		t.Fatalf("StartActor: %v", err)
	}
	defer a.Stop()

	a.Observe(obs.Session{ID: "s1"})
	if _, ok := a.Snapshot(); !ok {
		t.Fatal("the actor stopped")
	}
	if st, _ := a.Stats(); st.Woken != 0 {
		t.Errorf("an actor with no clock woke %d times", st.Woken)
	}
}

// TestAWakeIsNeverBlocking is the deadlock this design would otherwise have.
//
// The coordinator asks an actor for a snapshot and WAITS on the reply. An actor
// that blocked inside Wake — on a full channel nobody is draining — would never
// answer, and the two would hold each other forever. The pipeline's wake
// channel is one slot deep and its send has a default, so this asserts that a
// wake into a channel nobody reads still lets the actor answer.
func TestAWakeIsNeverBlocking(t *testing.T) {
	clock := obs.NewFakeClock(time.Unix(1789545200, 0))
	blocked := make(chan struct{}) // unbuffered, never read
	wake := func() {
		select {
		case blocked <- struct{}{}:
		default:
		}
	}
	stage := &wakerStage{name: "w", deadlines: []time.Duration{time.Second, time.Second, time.Second}}
	a, err := StartActorWith("s1", []Factory{factoryFor(stage)},
		ActorConfig{Clock: clock, Wake: wake})
	if err != nil {
		t.Fatalf("StartActorWith: %v", err)
	}
	defer a.Stop()

	a.Observe(obs.Session{ID: "s1"})
	ft, err := clock.NextTimer(2 * time.Second)
	if err != nil {
		t.Fatalf("no alarm: %v", err)
	}
	ft.Fire(time.Unix(1789545201, 0))

	done := make(chan bool, 1)
	go func() {
		_, ok := a.Snapshot()
		done <- ok
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the actor stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the actor never answered after waking; Wake blocked on its caller")
	}
}

// ── the pipeline end of it ──────────────────────────────────────────────────

// pipeActivity is a pane table for the pipeline tests: every pane it knows has
// been silent since a fixed instant.
type pipeActivity struct {
	out map[int]time.Time
}

func (p pipeActivity) Activity(pane int) (time.Time, time.Time, bool) {
	at, ok := p.out[pane]
	return at, time.Time{}, ok
}

// TestEveryPublishedSessionCarriesAVerdict.
//
// A WRONG IMPLEMENTATION leaves the verdict stage to the caller's Chain, and a
// caller that forgets it ships a document that looks complete and answers the
// one question this layer exists to answer with silence. It is appended here
// for the same reason the pane gate is prepended here.
func TestEveryPublishedSessionCarriesAVerdict(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	writeRecord(t, dir, 4343, "id-b", "session-b", "idle")

	d := firstDoc(t, startPipeline(t, Config{Root: dir}))
	if len(d.Doc.Sessions) != 2 {
		t.Fatalf("the document has %d sessions, want 2", len(d.Doc.Sessions))
	}
	for _, s := range d.Doc.Sessions {
		if s.Confidence != obs.TierAuthoritative {
			t.Errorf("session %d: confidence %v, want authoritative", s.PID, s.Confidence)
		}
		if s.Attention.Needed == nil {
			t.Errorf("session %d: attention undetermined (%q) for a record that decoded cleanly",
				s.PID, s.Attention.Reason)
		}
		if s.Attention.Reason == "" {
			t.Errorf("session %d: a verdict with no reason", s.PID)
		}
	}
	waiting := sessionByPID(t, d.Doc, 4242)
	if waiting.Attention.Needed == nil || !*waiting.Attention.Needed {
		t.Error("a waiting session was not flagged for attention")
	}
	idle := sessionByPID(t, d.Doc, 4343)
	if idle.Attention.Needed == nil || *idle.Attention.Needed {
		t.Error("an idle session was flagged for attention")
	}
}

// TestAStalenessDeadlineDrivesACycleWithNothingOnDiskChanging is the end of
// the chain the whole deliverable is: a pane goes silent, nothing is written
// anywhere, and a new document arrives anyway.
//
// A WRONG IMPLEMENTATION publishes only on a filesystem event, so the one case
// the conflict field exists for — a record that stopped being written while its
// pane went quiet — can never produce a document, because the write that would
// wake the pipeline is precisely what is not happening.
func TestAStalenessDeadlineDrivesACycleWithNothingOnDiskChanging(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "busy")

	clock := obs.NewFakeClock(epoch)
	p := startPipeline(t, Config{
		Root:     dir,
		Locator:  staticLocator{pane: map[int]int{4242: 1}, alive: map[int]obs.Liveness{1: obs.LivenessYes}},
		Activity: pipeActivity{out: map[int]time.Time{1: epoch.Add(-2 * time.Second)}},
		Clock:    clock,
		Now:      func() time.Time { return epoch },
	})

	sub, err := p.Subscribe("test", 4)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	first := mustReceive(t, sub, 10*time.Second)
	if s := sessionByPID(t, first.Doc, 4242); s.Conflict != nil {
		t.Fatalf("a pane silent for two seconds contradicted its busy record: %+v", *s.Conflict)
	}

	ft, err := clock.NextTimer(5 * time.Second)
	if err != nil {
		t.Fatalf("no staleness alarm was armed: %v", err)
	}
	ft.Fire(epoch.Add(3 * time.Second))

	second := mustReceive(t, sub, 10*time.Second)
	if second.Doc.Epoch.IsZero() {
		t.Fatal("the woken cycle published nothing")
	}
	st := p.Stats()
	if st.Wakes != 1 {
		t.Errorf("stats report %d wakes, want 1", st.Wakes)
	}
	if st.Cycles < 2 || st.Published < 2 {
		t.Errorf("stats report %d cycles and %d publishes; the alarm did not drive one",
			st.Cycles, st.Published)
	}
}

// TestInsideAMagmuxTheTwoTiersMeet — the pipeline with both an authoritative
// record and terminal evidence, which is what MAGMUX_AGENTS=1 builds and what
// `magmux agents` deliberately cannot.
func TestInsideAMagmuxTheTwoTiersMeet(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "wedged", "busy")

	d := firstDoc(t, startPipeline(t, Config{
		Root:     dir,
		Locator:  staticLocator{pane: map[int]int{4242: 1}, alive: map[int]obs.Liveness{1: obs.LivenessYes}},
		Activity: pipeActivity{out: map[int]time.Time{1: epoch.Add(-30 * time.Minute)}},
		Now:      func() time.Time { return epoch },
	}))

	s := sessionByPID(t, d.Doc, 4242)
	if s.Status != obs.StatusBusy || s.Confidence != obs.TierAuthoritative {
		t.Fatalf("published %v/%v, want busy/authoritative", s.Status, s.Confidence)
	}
	if s.Conflict == nil {
		t.Fatal("half an hour of silence against a busy record published no conflict")
	}
	if s.Conflict.Tier != obs.TierInferred || s.Conflict.Status != obs.StatusIdle {
		t.Errorf("conflict is %+v, want the inferred tier reporting idle", *s.Conflict)
	}
	if s.Attention.Needed != nil {
		t.Fatalf("attention %v for a conflicted session, want undetermined", *s.Attention.Needed)
	}
}

// TestWithNoActivityPortThereIsNoInferredTierAndNoAlarm — `magmux agents` from
// outside a magmux, where the pane questions are honestly unanswerable.
func TestWithNoActivityPortThereIsNoInferredTierAndNoAlarm(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "a", "busy")

	clock := obs.NewFakeClock(epoch)
	d := firstDoc(t, startPipeline(t, Config{Root: dir, Clock: clock}))

	s := sessionByPID(t, d.Doc, 4242)
	if s.Conflict != nil {
		t.Errorf("a process with no pane table produced a conflict: %+v", *s.Conflict)
	}
	if s.Confidence != obs.TierAuthoritative {
		t.Errorf("confidence %v, want authoritative", s.Confidence)
	}
	if err := clock.NoTimerWithin(200 * time.Millisecond); err != nil {
		t.Errorf("%v — there is nothing to be silent, so nothing to wait for", err)
	}
}
