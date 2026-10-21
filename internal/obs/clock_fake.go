package obs

import (
	"fmt"
	"time"
)

// FakeClock is the test [Clock]. It lives in a normal file rather than a
// _test.go one because every package in this layer needs the same fake and
// they are all different packages — termev's driver tests, fswatch's
// coalescing tests, and phase 3's actor tests. An export_test.go would have
// made a second copy inevitable, and a second fake clock is the shape that
// drifts first.
//
// # Why it needs no lock, which is the whole design
//
// A fake clock usually carries a mutable "now" that the test advances while
// the code under test reads it. That is shared mutable state across two
// goroutines and it needs a mutex, which this layer is not allowed to have.
//
// This one has no mutable state at all. Every field is written once, before
// the value is handed to anyone, and every cross-goroutine hand-off is a
// channel:
//
//   - base is immutable, so Now needs no synchronisation;
//   - a timer created on the driver's goroutine is published on timers, and a
//     test that RECEIVES it is ordered after its construction, so reading
//     Delay is safe;
//   - Stop closes a channel on the driver's goroutine and a test observes the
//     close, which is the one ordering guarantee a close gives for free;
//   - Fire sends on a buffered channel from the test's goroutine.
//
// The advance is therefore not a field the test writes; it is the instant the
// test puts INTO the timer it fires. Which is also more honest: the driver
// learns the time by being woken, exactly as it does in production.
type FakeClock struct {
	base   time.Time
	timers chan *FakeTimer
}

// fakeTimerBacklog bounds the published-timer channel. A cascade re-arms once
// per signal, so a test that drives many signals without draining Timers would
// eventually block the driver — which shows up as the test's own deadline,
// with the backlog as the reason, rather than as a hang with no explanation.
const fakeTimerBacklog = 256

// NewFakeClock returns a clock whose Now is fixed at base forever.
//
// Fixed, not frozen-and-advanceable. Nothing in the layer calls Now at all
// any more — see [Clock], where the one caller it used to have is accounted
// for — and a fake that could be advanced would invite a test to start. Tests
// express the passage of time by choosing the instant they fire a timer with,
// and code under test learns the time by being woken, exactly as it does in
// production.
//
// A caller that reads Now REPEATEDLY and expects it to move is using the wrong
// port and will see one instant forever — which is why anything needing a
// moving instant injects its own func() time.Time instead. See
// registry.Config.Now.
func NewFakeClock(base time.Time) *FakeClock {
	return &FakeClock{base: base, timers: make(chan *FakeTimer, fakeTimerBacklog)}
}

// Now returns the fixed base instant.
func (c *FakeClock) Now() time.Time { return c.base }

// NewTimer records an alarm and publishes it on [FakeClock.Timers]. It never
// fires on its own — the test decides when, and with what instant.
func (c *FakeClock) NewTimer(d time.Duration) Timer {
	ft := &FakeTimer{
		delay:   d,
		ch:      make(chan time.Time, 1),
		stopped: make(chan struct{}),
	}
	c.timers <- ft
	return ft
}

// Timers yields every alarm created, in creation order.
func (c *FakeClock) Timers() <-chan *FakeTimer { return c.timers }

// NextTimer returns the next alarm created, waiting up to timeout for one.
//
// The timeout is a FAILURE deadline, not a wait for a threshold: on a working
// implementation the alarm is already in the channel by the time the signal
// that caused it has been processed. It exists so a broken implementation
// reports "no timer was armed" instead of hanging until the whole test binary
// is killed.
func (c *FakeClock) NextTimer(timeout time.Duration) (*FakeTimer, error) {
	select {
	case ft := <-c.timers:
		return ft, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("obs: no timer was armed within %s", timeout)
	}
}

// NoTimerWithin reports whether the clock stays quiet for d — that no alarm at
// all is armed. It is how a test proves that input CANCELLED the cascade,
// which is an absence and can only be shown by waiting a little.
func (c *FakeClock) NoTimerWithin(d time.Duration) error {
	select {
	case ft := <-c.timers:
		return fmt.Errorf("obs: an alarm was armed for %s, expected none", ft.delay)
	case <-time.After(d):
		return nil
	}
}

// FakeTimer is one alarm created by a [FakeClock]. The test fires it.
type FakeTimer struct {
	delay   time.Duration
	ch      chan time.Time
	stopped chan struct{}
}

// C implements [Timer].
func (t *FakeTimer) C() <-chan time.Time { return t.ch }

// Stop implements [Timer].
//
// Only the goroutine that owns the timer calls this — the driver's loop, which
// stops an alarm before replacing it and on the way out. There is therefore no
// concurrent closer, and the select guards only against the same goroutine
// stopping the same alarm twice.
func (t *FakeTimer) Stop() {
	select {
	case <-t.stopped:
	default:
		close(t.stopped)
	}
}

// Delay is the duration this alarm was armed for.
func (t *FakeTimer) Delay() time.Duration { return t.delay }

// Fire delivers at on the alarm's channel, as if it had gone off then.
func (t *FakeTimer) Fire(at time.Time) { t.ch <- at }

// Stopped reports whether the alarm has been released. Safe to call once the
// test has observed something that is ordered after the stop — the arrival of
// the replacement alarm, for instance, because the driver stops the old one
// before it creates the new one.
func (t *FakeTimer) Stopped() bool {
	select {
	case <-t.stopped:
		return true
	default:
		return false
	}
}
