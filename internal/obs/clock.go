package obs

import "time"

// Clock is the observation layer's only port onto waiting. It exists so that
// no test anywhere under internal/obs ever sleeps: a test that waits five real
// seconds to prove a five-second threshold is a test nobody runs, and a test
// that shortens the threshold to milliseconds proves the threshold it invented
// rather than the one magmux ships.
//
// # Why it lives here and not in one source
//
// It began in termev, which was the only thing that needed to wait. fswatch
// needs exactly the same seam for its coalescing window, and two sources with
// two copies of one test seam is how the two copies drift — one gains a Reset,
// the other a mutable now, and a test written against either stops saying
// anything about the other. So the port sits in the core, which neither source
// can avoid importing, and termev keeps type ALIASES so that nothing written
// against the old spelling had to move.
//
// An interface adds no goroutine, no mutex and no atomic, so the core is still
// the plain-values package its own documentation describes. What it now also
// carries is the one thing every source in the layer agrees about time.
//
// # The port is small on purpose: it CREATES TIMERS
//
// It is not "what time is it". Every instant this layer acts on is carried by
// the event that reports it — the moment output arrived, the moment a record
// changed — because the gap between an event happening and a goroutine hearing
// about it is exactly the quantity these thresholds measure. What a source
// genuinely cannot do for itself is WAIT, so waiting is the thing behind the
// interface.
//
// Now has no caller left in this layer, and that is worth saying rather than
// leaving to be discovered. Its one caller was termev.StartDriver, which armed
// a cascade at birth so that a pane producing not one byte still went stale;
// phase 5 moved the cascade under the per-session actor and deleted the
// driver, and the verdict stage arms at FIRST SIGHT instead, from the instant
// its own observation carries.
//
// It stays for two reasons. A clock port with no "what time is it" is a
// surprising thing to hand the next person who needs one. And [FakeClock.Now]
// is FIXED rather than advanceable — one instant forever — which is the
// property that stops a test quietly reading a moving clock out of a seam
// whose job is to create timers. A caller that needs an instant which moves
// between reads must inject its own func() time.Time; see registry.Config.Now
// and verdict.Config.Now, which document the trap in full.
type Clock interface {
	Now() time.Time
	// NewTimer returns a ONE-SHOT alarm that delivers once after d and never
	// again. A non-positive d fires as soon as the receiver is ready.
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot alarm.
//
// There is no Reset. Re-arming allocates a fresh timer and drops the old one,
// which costs an allocation a source takes at most once per burst and buys
// immunity from the Reset-versus-drain race that the standard library's own
// documentation spends three paragraphs on: a stopped timer whose value was
// already in flight will deliver it, and a Reset timer's channel can then hold
// a stale wake-up. A dropped timer's channel is simply never read again.
type Timer interface {
	// C is the delivery channel. It yields the instant the alarm fired.
	C() <-chan time.Time
	// Stop releases the alarm. It is safe to call on an alarm that already
	// fired, and the caller must not read C afterwards.
	Stop()
}

// RealClock is the production [Clock]: a thin wrapper over the time package.
func RealClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer {
	return realTimer{t: time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }

func (r realTimer) Stop() { r.t.Stop() }
