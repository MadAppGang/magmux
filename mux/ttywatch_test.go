package mux

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/pty"
)

// fakeTTY is a scripted terminal: read() returns whatever mode is current and
// counts the calls. A current VALUE rather than a queue, so the hot goroutine
// reading it concurrently with a test cannot consume a step the test meant for
// itself.
type fakeTTY struct {
	mu    sync.Mutex
	mode  pty.Mode
	err   error
	reads int
}

var (
	cookedMode = pty.Mode{Canonical: true, Echo: true}
	rawMode    = pty.Mode{}
)

func newFakeTTY(m pty.Mode) *fakeTTY { return &fakeTTY{mode: m} }

func (f *fakeTTY) read() (pty.Mode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.mode, f.err
}

func (f *fakeTTY) set(m pty.Mode) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

func (f *fakeTTY) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeTTY) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// waitFor is lock_test.go's.

func TestTTYWatchFirstRawAndReset(t *testing.T) {
	f := newFakeTTY(cookedMode)
	w := newTTYWatch(f.read, time.Now())
	defer w.close()

	// A wrapper's cooked startup before Claude owns the terminal is not a reset.
	st := w.sampleNow()
	if st.Raw || !st.FirstRawAt.IsZero() || st.SawReset {
		t.Fatalf("cooked start: %+v", st)
	}
	if st.ChangedAt.IsZero() {
		t.Fatalf("the first sample must count as a change: %+v", st)
	}

	f.set(rawMode)
	before := time.Now()
	st = w.sampleNow()
	after := time.Now()
	if !st.Raw || st.SawReset {
		t.Fatalf("first raw: %+v", st)
	}
	if st.FirstRawAt.Before(before) || st.FirstRawAt.After(after) {
		t.Fatalf("FirstRawAt %v outside [%v, %v]", st.FirstRawAt, before, after)
	}
	firstRaw := st.FirstRawAt

	// Raw again: nothing moves.
	st = w.sampleNow()
	if !st.Raw || st.SawReset || !st.FirstRawAt.Equal(firstRaw) {
		t.Fatalf("raw again: %+v", st)
	}

	// The boot reset: cooked AFTER the first raw.
	f.set(cookedMode)
	before = time.Now()
	st = w.sampleNow()
	if st.Raw || !st.SawReset {
		t.Fatalf("reset not seen: %+v", st)
	}
	if st.ChangedAt.Before(before) {
		t.Fatalf("ChangedAt %v did not move to the reset (>= %v)", st.ChangedAt, before)
	}

	// Raw after the reset: FirstRawAt is the FIRST one, and SawReset stays.
	f.set(rawMode)
	st = w.sampleNow()
	if !st.Raw || !st.SawReset || !st.FirstRawAt.Equal(firstRaw) {
		t.Fatalf("raw after reset: %+v", st)
	}
	if got := w.state(); got.Raw != st.Raw || !got.ChangedAt.Equal(st.ChangedAt) || got.SawReset != st.SawReset {
		t.Fatalf("state() %+v disagrees with the sample it stored %+v", got, st)
	}

	// A read error is never raw, and is reported.
	boom := errors.New("closed master")
	f.fail(boom)
	st = w.sampleNow()
	if st.Raw || !errors.Is(st.Err, boom) {
		t.Fatalf("read error: %+v", st)
	}
}

func TestTTYWatchHeatsOnChange(t *testing.T) {
	f := newFakeTTY(cookedMode)
	w := newTTYWatch(f.read, time.Now())
	defer w.close()

	// The first sample is not a transition, so it does not heat.
	w.sampleCold(time.Now())
	if w.isRunning() {
		t.Fatalf("the first sample started the hot sampler")
	}

	f.set(rawMode)
	now := time.Now()
	w.sampleCold(now)
	if !w.isRunning() {
		t.Fatalf("an observed change did not start the hot sampler")
	}
	if hot := w.hotUntilValue(); hot.Before(now.Add(ttyHotAfterChange)) {
		t.Fatalf("hotUntil %v, want >= %v", hot, now.Add(ttyHotAfterChange))
	}

	// While hot, the goroutine samples on its own.
	n := f.count()
	if !waitFor(2*time.Second, func() bool { return f.count() >= n+3 }) {
		t.Fatalf("hot sampler is not sampling: %d reads", f.count())
	}

	// A change seen by the hot goroutine itself extends the window too.
	before := w.hotUntilValue()
	time.Sleep(10 * time.Millisecond)
	f.set(cookedMode)
	if !waitFor(2*time.Second, func() bool { return w.hotUntilValue().After(before) }) {
		t.Fatalf("a change seen while hot did not extend hotUntil past %v", before)
	}
	if w.startCount() != 1 {
		t.Fatalf("a change while running started a second goroutine (%d starts)", w.startCount())
	}
}

func TestTTYWatchGoesCold(t *testing.T) {
	f := newFakeTTY(rawMode)
	w := newTTYWatch(f.read, time.Now())
	defer w.close()

	w.heat(time.Now().Add(100 * time.Millisecond))
	if !w.isRunning() {
		t.Fatalf("heat did not start the sampler")
	}
	if !waitFor(3*time.Second, func() bool { return !w.isRunning() }) {
		t.Fatalf("hot sampler did not exit after hotUntil")
	}
	n := f.count()
	time.Sleep(5 * ttyHotInterval)
	if f.count() != n {
		t.Fatalf("a cold watch kept reading: %d -> %d", n, f.count())
	}
	if st := w.state(); !st.Raw {
		t.Fatalf("history lost after going cold: %+v", st)
	}
}

func TestTTYWatchColdCostsNoGoroutine(t *testing.T) {
	f := newFakeTTY(rawMode)
	w := newTTYWatch(f.read, time.Now())
	defer w.close()

	if w.isRunning() || w.startCount() != 0 {
		t.Fatalf("construction started a goroutine")
	}
	for i := 0; i < 50; i++ {
		w.sampleCold(time.Now())
	}
	if w.startCount() != 0 {
		t.Fatalf("steady cold sampling started %d goroutines", w.startCount())
	}
	if f.count() != 50 {
		t.Fatalf("sampleCold made %d reads for 50 calls, want one each", f.count())
	}
	// A heat in the past is not a heat.
	w.heat(time.Now().Add(-time.Second))
	if w.startCount() != 0 {
		t.Fatalf("an expired heat started a goroutine")
	}
}

func TestTTYWatchStopEndsGoroutine(t *testing.T) {
	f := newFakeTTY(rawMode)
	w := newTTYWatch(f.read, time.Now())

	w.heat(time.Now().Add(time.Minute))
	if !waitFor(time.Second, func() bool { return f.count() > 0 }) {
		t.Fatalf("hot sampler never read")
	}
	w.close()
	w.close() // idempotent: a second close must not panic on the channel
	if !waitFor(time.Second, func() bool { return !w.isRunning() }) {
		t.Fatalf("close did not end the hot sampler")
	}
	n := f.count()
	time.Sleep(5 * ttyHotInterval)
	if f.count() != n {
		t.Fatalf("reads continued after close: %d -> %d", n, f.count())
	}
}

func TestTTYWatchReadErrorEndsGoroutine(t *testing.T) {
	f := newFakeTTY(rawMode)
	f.fail(errors.New("use of closed file"))
	w := newTTYWatch(f.read, time.Now())
	defer w.close()

	w.heat(time.Now().Add(time.Minute))
	if !waitFor(time.Second, func() bool { return !w.isRunning() }) {
		t.Fatalf("a read error did not end the hot sampler")
	}
	if st := w.state(); st.Raw || st.Err == nil {
		t.Fatalf("error not recorded: %+v", st)
	}
}

// TestHeatAfterCloseStartsNothing (gemini L9): a heat racing ClosePane can
// never start a goroutine that outlives Stop(). Run under -race.
func TestHeatAfterCloseStartsNothing(t *testing.T) {
	f := newFakeTTY(rawMode)

	w := newTTYWatch(f.read, time.Now())
	w.close()
	w.heat(time.Now().Add(time.Minute))
	w.sampleCold(time.Now())
	if w.startCount() != 0 || w.isRunning() {
		t.Fatalf("heat after close started a goroutine")
	}

	for i := 0; i < 200; i++ {
		w := newTTYWatch(f.read, time.Now())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); w.close() }()
		go func() { defer wg.Done(); w.heat(time.Now().Add(time.Minute)) }()
		wg.Wait()
		if !waitFor(time.Second, func() bool { return !w.isRunning() }) {
			t.Fatalf("iteration %d: a goroutine outlived close", i)
		}
		starts := w.startCount()
		w.heat(time.Now().Add(time.Minute))
		if w.startCount() != starts || w.isRunning() {
			t.Fatalf("iteration %d: heat after close started a goroutine", i)
		}
	}
}

// TestSampleNowIsTheAuthority (gpt H1): a raw sample cached 20 ms ago must not
// authorise a write while a reset that began 10 ms ago is in progress.
func TestSampleNowIsTheAuthority(t *testing.T) {
	f := newFakeTTY(rawMode)
	w := newTTYWatch(f.read, time.Now())
	defer w.close()

	cachedAt := time.Now().Add(-20 * time.Millisecond)
	w.sampleCold(cachedAt)
	if st := w.state(); !st.Raw || !st.SampledAt.Equal(cachedAt) {
		t.Fatalf("seed: %+v", st)
	}

	f.set(cookedMode) // the reset, already under way
	reads := f.count()
	st := w.sampleNow()
	if f.count() != reads+1 {
		t.Fatalf("sampleNow made %d reads, want exactly 1", f.count()-reads)
	}
	if st.Raw {
		t.Fatalf("sampleNow reported raw from the cache: %+v", st)
	}
	if !st.ChangedAt.After(cachedAt) || !st.SampledAt.After(cachedAt) {
		t.Fatalf("sampleNow did not record the change: %+v", st)
	}
	if !st.SawReset {
		t.Fatalf("a cooked sample after a raw one is a reset: %+v", st)
	}
	if got := w.state(); got.Raw {
		t.Fatalf("history still says raw after the authority read cooked: %+v", got)
	}
}

// TestSampleNowReportsItsOwnReadEvenWhenStale: if a concurrent sampler stored a
// NEWER sample first, history keeps the newer one, but sampleNow still answers
// with what IT read — the authority is the read, not the cache.
func TestSampleNowReportsItsOwnReadEvenWhenStale(t *testing.T) {
	f := newFakeTTY(rawMode)
	w := newTTYWatch(f.read, time.Now())
	defer w.close()

	future := time.Now().Add(time.Hour)
	w.sampleCold(future) // a newer sample than anything sampleNow can take

	f.set(cookedMode)
	st := w.sampleNow()
	if st.Raw {
		t.Fatalf("sampleNow reported the cached raw instead of its own cooked read: %+v", st)
	}
	if got := w.state(); !got.Raw || !got.SampledAt.Equal(future) {
		t.Fatalf("an older sample overwrote newer history: %+v", got)
	}
}
