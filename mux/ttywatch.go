package mux

import (
	"sync"
	"time"

	"github.com/MadAppGang/magmux/pty"
)

// ── ttyWatch: the child's termios, as history and as authority ───────────────
//
// A ttyWatch samples the termios of one pane's PTY (pty.ReadMode on the
// master, which reports the child's settings) and does two jobs that are kept
// apart on purpose:
//
//  1. HISTORY. A reset that happened while nobody was asking must still be
//     known about. The sampler keeps ChangedAt (the last fingerprint change),
//     FirstRawAt (the first non-canonical sample: the moment the tool, not a
//     wrapper, owns the terminal) and SawReset (a canonical sample AFTER
//     FirstRawAt: the boot reset). It runs HOT — one goroutine, one read every
//     ttyHotInterval — while now < hotUntil, and is otherwise COLD: Poll calls
//     sampleCold once per tick and there is no goroutine at all.
//
//  2. AUTHORITY. Every decision that can lead to a write calls sampleNow and
//     decides on the state it RETURNS. state() is history and is never the
//     authority for a write: a raw sample cached 20 ms ago says nothing about a
//     reset that began 10 ms ago.
//
// Lock rule: w.mu is a LEAF. It is never held across read() (an ioctl in
// production), and no other lock is ever taken while it is held. read() is
// therefore called with no ttyWatch lock, and its result is stored under mu
// afterwards.
//
// The goroutine's exits are: hot expiry, a read error (the master closed), and
// close(). close() is final: heat is a no-op afterwards, so a heat racing
// ClosePane can never start a goroutine that outlives Stop().
//
// A pane with no controller that needs readiness has no ttyWatch, so this
// costs nothing for every other pane.

const (
	ttyHotInterval    = 20 * time.Millisecond // hot sampling period
	ttyHotStartup     = 20 * time.Second      // hot window from the pane's start; covers every boot reset measured
	ttyHotAfterChange = 5 * time.Second       // every observed change extends the window by this
	ttyDemandHold     = 2 * time.Second       // every readiness demand extends it by this
)

type ttyWatch struct {
	read      func() (pty.Mode, error) // pty.ReadMode(ptmx) in production; a fake in tests
	startedAt time.Time                // the pane's start; the controller heats to startedAt+ttyHotStartup

	mu         sync.Mutex // LEAF: never held across read(), never while taking another lock
	last       pty.Mode
	have       bool
	err        error
	sampledAt  time.Time // newest stored sample
	changedAt  time.Time // last fingerprint change (the first sample counts as one)
	firstRawAt time.Time // first sample with !Canonical; zero until then
	sawReset   bool      // a Canonical sample observed AFTER firstRawAt (the boot reset)
	hotUntil   time.Time
	running    bool
	closed     bool // set once by close(); heat() is a no-op afterwards
	starts     int  // goroutines ever started; observability for tests
	stop       chan struct{}
}

// ttyState is one consistent view of a ttyWatch.
type ttyState struct {
	Raw        bool // have && !last.Canonical && err == nil
	ChangedAt  time.Time
	FirstRawAt time.Time
	SawReset   bool
	SampledAt  time.Time
	Err        error
}

// newTTYWatch builds a cold watch. It starts nothing: the caller heats it
// (the Claude controller's Start does heat(startedAt.Add(ttyHotStartup))).
func newTTYWatch(read func() (pty.Mode, error), startedAt time.Time) *ttyWatch {
	return &ttyWatch{
		read:      read,
		startedAt: startedAt,
		stop:      make(chan struct{}),
	}
}

// heat extends the hot window to until, and starts the sampling goroutine if
// none is running. A no-op once closed, and an until already in the past
// starts nothing.
func (w *ttyWatch) heat(until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.heatLocked(until, time.Now())
}

// heatLocked is heat's body; w.mu must be held. It spawns a goroutine, which
// is not blocking I/O, so it may run under the leaf lock.
func (w *ttyWatch) heatLocked(until, now time.Time) {
	if w.closed {
		return
	}
	if until.After(w.hotUntil) {
		w.hotUntil = until
	}
	if w.running || !now.Before(w.hotUntil) {
		return
	}
	w.running = true
	w.starts++
	go w.run()
}

// run is the hot sampler. It exits on hot expiry, on a read error and on
// close(), and always leaves running == false behind it.
func (w *ttyWatch) run() {
	t := time.NewTicker(ttyHotInterval)
	defer t.Stop()
	for {
		m, err := w.read()
		now := time.Now()

		w.mu.Lock()
		w.storeLocked(m, err, now)
		if err != nil || w.closed || !now.Before(w.hotUntil) {
			w.running = false
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()

		select {
		case <-w.stop:
			w.mu.Lock()
			w.running = false
			w.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

// sampleCold takes one sample iff the hot sampler is not running (it is
// already sampling) and the watch is not closed. Poll calls it, with treeMu
// released, so a cold pane still gets one reading per render tick.
func (w *ttyWatch) sampleCold(now time.Time) {
	w.mu.Lock()
	skip := w.running || w.closed
	w.mu.Unlock()
	if skip {
		return
	}
	m, err := w.read()

	w.mu.Lock()
	w.storeLocked(m, err, now)
	w.mu.Unlock()
}

// sampleNow is the AUTHORITY: one read now, stored into history, and the state
// returned is decided by THIS read. If a concurrent sampler has meanwhile
// stored a newer sample, history keeps the newer one, but Raw and Err in the
// result are still this read's — the caller asked what the terminal is now,
// and this is the freshest read it owns.
func (w *ttyWatch) sampleNow() ttyState {
	m, err := w.read()
	now := time.Now()

	w.mu.Lock()
	defer w.mu.Unlock()
	w.storeLocked(m, err, now)
	st := w.stateLocked()
	st.Raw = err == nil && !m.Canonical
	st.Err = err
	return st
}

// state is history only; never the authority for a write.
func (w *ttyWatch) state() ttyState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stateLocked()
}

// close ends the watch: closed is set, stop is closed exactly once, and the
// goroutine (if any) exits. Idempotent.
func (w *ttyWatch) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.stop)
}

func (w *ttyWatch) stateLocked() ttyState {
	return ttyState{
		Raw:        w.have && !w.last.Canonical && w.err == nil,
		ChangedAt:  w.changedAt,
		FirstRawAt: w.firstRawAt,
		SawReset:   w.sawReset,
		SampledAt:  w.sampledAt,
		Err:        w.err,
	}
}

// storeLocked files one sample into history; w.mu must be held. A sample older
// than the newest stored one is dropped: two samplers can finish their reads
// in either order, and history must not step backwards. A fingerprint change
// after the first sample heats the watch by ttyHotAfterChange. The first
// sample counts as a change for ChangedAt but is not a transition, so it does
// not heat (Start's ttyHotStartup already covers the boot).
func (w *ttyWatch) storeLocked(m pty.Mode, err error, at time.Time) {
	if at.Before(w.sampledAt) {
		return
	}
	w.sampledAt = at
	w.err = err
	if err != nil {
		return
	}
	changed := !w.have || !w.last.Same(m)
	transition := w.have && changed
	if changed {
		w.changedAt = at
	}
	if !m.Canonical && w.firstRawAt.IsZero() {
		w.firstRawAt = at
	}
	if m.Canonical && !w.firstRawAt.IsZero() {
		w.sawReset = true
	}
	w.last = m
	w.have = true
	if transition {
		w.heatLocked(at.Add(ttyHotAfterChange), time.Now())
	}
}

// isRunning, hotUntilValue and startCount are observability for tests.
func (w *ttyWatch) isRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *ttyWatch) hotUntilValue() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hotUntil
}

func (w *ttyWatch) startCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.starts
}
