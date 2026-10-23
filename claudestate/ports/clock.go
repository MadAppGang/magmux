package ports

import "time"

// Clock is the library's only port onto waiting.
//
// It exists so no test sleeps through a threshold: a test that waits five real
// seconds to prove a five-second threshold is a test nobody runs, and one that
// shortens the threshold to milliseconds proves the threshold it invented.
//
// THIS FILE IS THE ONLY NON-TEST FILE IN THE MODULE THAT MAY ARM A REAL TIMER
// (guard G4). Everything else that waits takes a Clock.
type Clock interface {
	Now() time.Time
	// NewTimer returns a ONE-SHOT alarm that delivers once after d and never
	// again. A non-positive d fires as soon as the receiver is ready.
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot alarm, with no Reset.
//
// Re-arming allocates a fresh timer and drops the old one. That buys immunity
// from the Reset-versus-drain race the standard library spends three
// paragraphs on: a dropped timer's channel is simply never read again.
//
// A Timer is owned by the goroutine that created it, and only that goroutine
// calls Stop. Stop is idempotent.
type Timer interface {
	C() <-chan time.Time
	Stop()
}

// RealClock is the production Clock, a thin wrapper over package time.
func RealClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{t: time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }

func (r realTimer) Stop() { r.t.Stop() }
