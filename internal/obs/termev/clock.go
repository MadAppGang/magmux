package termev

import (
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// The clock port MOVED to internal/obs, and what is left here is four ALIASES.
//
// It began here because termev was the only thing in the layer that had to
// wait. fswatch has to wait too — its coalescing window is a one-shot timer
// armed from the same seam — and two sources holding two copies of one test
// seam is how the copies drift: one grows a Reset, the other a mutable now,
// and a test written against either stops saying anything about the other.
//
// A Go type alias is the SAME type, not a conversion and not a wrapper, so
// every line already written against termev.Clock, termev.Timer,
// termev.FakeClock and termev.NewFakeClock still compiles and still means
// exactly what it did. That is the evidence the move changed no behaviour:
// phase 1's cascade and diff tests are byte-identical to what they were, and
// they pass.
//
// The port's own documentation lives with the port, at [obs.Clock]. The one
// thing worth repeating here is the rule this package was built on and which
// the move does not touch: a Clock CREATES TIMERS, and does not answer "what
// time is it". Every instant termev acts on is carried by the event that
// reports it.
//
// obs.Clock.Now now has NO caller anywhere in the layer. Its one caller was
// StartDriver, arming a cascade at birth, and phase 5 deleted that; the
// verdict stage arms at first sight instead, from the instant its own
// observation carries. The method stays on the port because a clock without
// "what time is it" is a surprising thing to hand the next person, and because
// obs.FakeClock.Now — fixed, never advanceable — is the shape that stops a
// test reading a moving instant out of a seam that creates timers. Anything
// needing an instant that MOVES injects its own func() time.Time; see
// registry.Config.Now and verdict.Config.Now, which both document the trap.

// Clock is [obs.Clock].
type Clock = obs.Clock

// Timer is [obs.Timer].
type Timer = obs.Timer

// FakeClock is [obs.FakeClock].
type FakeClock = obs.FakeClock

// FakeTimer is [obs.FakeTimer].
type FakeTimer = obs.FakeTimer

// RealClock returns [obs.RealClock].
func RealClock() Clock { return obs.RealClock() }

// NewFakeClock returns [obs.NewFakeClock].
func NewFakeClock(base time.Time) *FakeClock { return obs.NewFakeClock(base) }
