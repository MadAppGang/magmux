package ports

import "time"

// FakeClock is the test Clock. It lives in a normal file because every
// package in the module needs the same fake, and a second fake clock is the
// shape that drifts first. It ARMS NO REAL TIMER (guard G4).
//
// # Why it needs no lock
//
// Now is fixed at base forever, so it needs no synchronisation. Every other
// hand-off between the goroutine that creates and stops timers (an actor, a
// watcher loop) and the test's goroutine is a channel:
//
//   - NewTimer publishes the timer on Timers() and records a "created" entry
//     on the private ledger channel;
//   - Stop closes the timer's stopped channel and records a "stopped" entry;
//   - Fire runs on the TEST goroutine, and so do Created and Outstanding,
//     which drain the ledger before they count.
//
// So every counter is written and read by one goroutine, the test's, and the
// only cross-goroutine facts are channel operations. The advance of time is
// not a field the test writes; it is the instant the test puts INTO the timer
// it fires, and code under test learns the time by being woken, exactly as in
// production.
//
// A caller that needs an instant which moves between reads must inject its
// own func() time.Time; this clock will show it one instant forever.
type FakeClock struct {
	base   time.Time
	timers chan *FakeTimer
	ledger chan ledgerEntry

	// Test goroutine only.
	created, fired, stopped int
}

const (
	fakeTimerBacklog = 256  // Timers(): a full buffer blocks the creator
	fakeLedgerSize   = 4096 // the private ledger: a full buffer blocks the creator
)

type ledgerEntry struct {
	timer   *FakeTimer
	stopped bool // false = created
}

// NewFakeClock returns a clock whose Now is fixed at base forever.
func NewFakeClock(base time.Time) *FakeClock {
	return &FakeClock{
		base:   base,
		timers: make(chan *FakeTimer, fakeTimerBacklog),
		ledger: make(chan ledgerEntry, fakeLedgerSize),
	}
}

// Now returns the fixed base instant.
func (c *FakeClock) Now() time.Time { return c.base }

// NewTimer records an alarm and publishes it on Timers. It never fires on its
// own: the test decides when, and with what instant.
func (c *FakeClock) NewTimer(d time.Duration) Timer {
	ft := &FakeTimer{
		clock:   c,
		delay:   d,
		ch:      make(chan time.Time, 1),
		stopped: make(chan struct{}),
	}
	c.ledger <- ledgerEntry{timer: ft}
	c.timers <- ft
	return ft
}

// Timers yields every timer, in creation order.
func (c *FakeClock) Timers() <-chan *FakeTimer { return c.timers }

// Created reports how many timers have been created. Test goroutine only.
func (c *FakeClock) Created() int {
	c.drain()
	return c.created
}

// Outstanding reports created − fired − stopped: the timers still live. A
// timer stopped after it fired counts as fired, once. Test goroutine only.
func (c *FakeClock) Outstanding() int {
	c.drain()
	return c.created - c.fired - c.stopped
}

func (c *FakeClock) drain() {
	for {
		select {
		case e := <-c.ledger:
			switch {
			case !e.stopped:
				c.created++
			case !e.timer.fired:
				c.stopped++
			}
		default:
			return
		}
	}
}

// FakeTimer is one alarm created by a FakeClock. The test fires it.
type FakeTimer struct {
	clock   *FakeClock
	delay   time.Duration
	ch      chan time.Time
	stopped chan struct{}

	fired bool // test goroutine only
}

// C implements Timer.
func (t *FakeTimer) C() <-chan time.Time { return t.ch }

// Stop implements Timer. Only the owning goroutine calls it, so the select
// guards only against that goroutine stopping the same alarm twice.
func (t *FakeTimer) Stop() {
	select {
	case <-t.stopped:
	default:
		close(t.stopped)
		t.clock.ledger <- ledgerEntry{timer: t, stopped: true}
	}
}

// Delay is the duration this alarm was armed for.
func (t *FakeTimer) Delay() time.Duration { return t.delay }

// Fire delivers at on the alarm's channel, as if it had gone off then. It is a
// no-op on a stopped alarm, and firing twice counts once. Test goroutine only.
func (t *FakeTimer) Fire(at time.Time) {
	if t.fired {
		return
	}
	select {
	case <-t.stopped:
		return
	default:
	}
	t.fired = true
	t.clock.fired++
	t.ch <- at
}

// Stopped is closed by Stop.
func (t *FakeTimer) Stopped() <-chan struct{} { return t.stopped }
