package termev

import (
	"fmt"
	"time"
)

// DefaultLadder returns the settled staleness ladder: 5s, 10s, 30s, 1m, 5m,
// 30m, 1h.
//
// It is a FUNCTION returning a fresh slice rather than an exported variable,
// because a package-level slice is mutable by anyone who imports it and a
// caller that sorted or appended to it in place would change the ladder for
// every pane in the process.
//
// The rungs are not evenly spaced and should not be. The first three are a
// human noticing that something went quiet; the last three are a machine
// noticing that something has been quiet for a working session's worth of
// time. Nothing here decides what a threshold MEANS — the cascade names the
// rung it crossed and a consumer decides whether five seconds of silence from
// a compiler is interesting.
func DefaultLadder() []time.Duration {
	return []time.Duration{
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		time.Minute,
		5 * time.Minute,
		30 * time.Minute,
		time.Hour,
	}
}

// Event is one threshold crossing.
type Event struct {
	// Threshold is the rung that was crossed — the ladder entry itself, so a
	// consumer can switch on 5*time.Minute rather than on an index.
	Threshold time.Duration
	// Index is that rung's position in the ladder, 0-based. Useful for
	// severity: a higher index is a longer silence, with no table lookup.
	Index int
	// Since is the last-activity instant this quiet period was armed from. Two
	// events sharing a Since belong to the same silence.
	Since time.Time
	// At is when the crossing was OBSERVED, which is not the same as when the
	// threshold elapsed: a timer that fires late, or a process descheduled
	// under load, crosses several rungs at one look. At - Since is therefore
	// the real quiet time and is always at least Threshold.
	At time.Time
}

// Quiet is how long the pane had been silent when the crossing was observed.
func (e Event) Quiet() time.Duration { return e.At.Sub(e.Since) }

// String renders an event the way a log line wants it.
func (e Event) String() string {
	return fmt.Sprintf("quiet %s (threshold %s, rung %d)", e.Quiet(), e.Threshold, e.Index)
}

// Cascade is the staleness state machine: a ladder of thresholds, a
// last-activity instant, and the position on the ladder.
//
// It is a PURE state machine. It holds no timer, starts no goroutine, has no
// lock and never asks what time it is — every instant it acts on is handed to
// it. That is what makes it exhaustively testable without a clock at all, and
// it is why the only thing its owner has to add is the WAITING: the verdict
// stage holds one of these per session, and that stage's actor arms the single
// one-shot timer it asks for.
//
// A Cascade is single-owner. The goroutine that created one is the only
// goroutine that may call any method on it.
type Cascade struct {
	ladder []time.Duration
	// since is the instant this quiet period began — the last accepted
	// activity. The ZERO value means DISARMED, which is the state input leaves
	// it in: no rung can fire until output resumes.
	since time.Time
	// next is the index of the rung that will fire next. Reaching len(ladder)
	// means the ladder is exhausted and this quiet period has nothing more to
	// say. It is what makes each rung fire AT MOST ONCE per quiet period: it
	// only ever moves forward, and only a new quiet period resets it.
	next int
	// inputAt is the last instant input was seen, kept for diagnostics only.
	// Nothing in the state machine reads it.
	inputAt time.Time
}

// NewCascade builds a cascade over the given ladder.
//
// The ladder must be non-empty, strictly increasing and entirely positive. A
// ladder that repeats or goes backwards would make "the rung that fires next"
// ambiguous, and a non-positive rung would fire before the output that armed
// it. All three are programmer errors in a constant table, so they are
// reported at construction rather than tolerated at run time.
func NewCascade(ladder ...time.Duration) (*Cascade, error) {
	if len(ladder) == 0 {
		return nil, fmt.Errorf("termev: cascade needs at least one threshold")
	}
	for i, d := range ladder {
		if d <= 0 {
			return nil, fmt.Errorf("termev: threshold %d is %s, must be positive", i, d)
		}
		if i > 0 && d <= ladder[i-1] {
			return nil, fmt.Errorf("termev: threshold %d (%s) does not exceed threshold %d (%s); the ladder must strictly increase", i, d, i-1, ladder[i-1])
		}
	}
	c := &Cascade{ladder: make([]time.Duration, len(ladder)), next: len(ladder)}
	copy(c.ladder, ladder)
	return c, nil
}

// Ladder returns a copy of the thresholds.
func (c *Cascade) Ladder() []time.Duration {
	out := make([]time.Duration, len(c.ladder))
	copy(out, c.ladder)
	return out
}

// Armed reports whether a quiet period is running and the ladder still has a
// rung to fire. A cascade that has crossed its last rung is not armed: there
// is nothing further to say about this silence.
func (c *Cascade) Armed() bool { return !c.since.IsZero() && c.next < len(c.ladder) }

// Since is the instant the current quiet period began, zero if disarmed.
func (c *Cascade) Since() time.Time { return c.since }

// LastInput is the last instant [Cascade.Input] was given, for diagnostics.
func (c *Cascade) LastInput() time.Time { return c.inputAt }

// Activity re-arms the cascade from at: the pane produced output then, so the
// silence starts over and every rung becomes eligible again.
//
// Two instants are refused, both silently, because both are reports about a
// quiet period that is already accounted for:
//
//   - the ZERO time, which is what a pane that has never produced a byte
//     carries. Arming from it would put every rung in the past and fire the
//     whole ladder at once against the epoch;
//   - any instant not strictly after the current Since. A quiet period is
//     IDENTIFIED by its Since, so accepting an equal or earlier one would
//     restart a period that had already fired rungs, and "at most once per
//     quiet period" would no longer hold. An out-of-order stamp is an ordering
//     defect upstream; honouring it would convert that into a duplicate event
//     nobody can explain.
//
// A DISARMED cascade accepts any non-zero instant, which is how output
// restarts a cascade that input had cancelled.
//
// It reports whether the instant was accepted. A caller holding a timer needs
// that answer: re-arming a timer from a REFUSED instant would recompute the
// delay against a stale now and push the next rung out past its real deadline.
func (c *Cascade) Activity(at time.Time) bool {
	if at.IsZero() {
		return false
	}
	if !c.since.IsZero() && !at.After(c.since) {
		return false
	}
	c.since = at
	c.next = 0
	return true
}

// Input cancels the cascade: someone typed into this pane, or a controller
// pushed an instruction into it, so its silence is no longer the pane's to
// explain. No rung fires again until output resumes and [Cascade.Activity]
// re-arms it.
//
// Cancel, not re-arm. The alternative reading — treat input as activity and
// restart the clock — would report a pane as stale when what it is really
// doing is waiting for a program to answer something a human just typed. That
// is a different fact and it belongs to a different observer. The ladder
// measures the CHILD's silence.
func (c *Cascade) Input(at time.Time) {
	c.inputAt = at
	c.since = time.Time{}
	c.next = len(c.ladder)
}

// Deadline is when the next rung comes due, and whether there is one.
func (c *Cascade) Deadline() (time.Time, bool) {
	if !c.Armed() {
		return time.Time{}, false
	}
	return c.since.Add(c.ladder[c.next]), true
}

// Advance reports the next rung that is due as of now, if any.
//
// It yields ONE event per call and moves the ladder on by one, so a caller
// that woke late loops until it returns false: several rungs can come due at
// one look and each is reported separately, each naming its own threshold,
// while sharing the At of the single observation that found them.
func (c *Cascade) Advance(now time.Time) (Event, bool) {
	deadline, ok := c.Deadline()
	if !ok || now.Before(deadline) {
		return Event{}, false
	}
	ev := Event{
		Threshold: c.ladder[c.next],
		Index:     c.next,
		Since:     c.since,
		At:        now,
	}
	c.next++
	return ev, true
}
