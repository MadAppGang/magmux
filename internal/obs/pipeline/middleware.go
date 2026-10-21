package pipeline

import (
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// Middleware is one stage of ONE session's chain: it is handed the fact set as
// it stands and returns it with whatever it knows added.
//
// # One instance per session, never shared
//
// Unanimous across all four plans in the architecture round, and it is why
// this package takes [Factory] values rather than built middleware: there is
// no API here through which two sessions could be given the same instance.
// A middleware is expected to REMEMBER things about its session — the pane it
// resolved, the transcript it found, the offset it read to — and a shared
// instance would answer one session's question with another session's memory.
// The failure is silent and it is wrong in exactly the cases the feature
// exists for.
//
// # It is called on the actor's goroutine, and may do I/O
//
// The actor exists so that a middleware can read a file or walk a process tree
// without any of it happening under magmux's locks or in its render loop. What
// a middleware may NOT do is block indefinitely: its actor processes one
// message at a time, so a stage that never returns wedges that session's actor
// and everything waiting on a reply from it. Bound the wait, and report the
// absence of an answer as an undetermined value — the model has one for every
// field.
//
// # Values, not pointers
//
// Observe takes and returns an [obs.Session] by value so that a stage cannot
// mutate the fact set of a previous stage after the fact, and so the actor can
// freeze the chain's output without asking anyone's permission. Assign a new
// pointer to set an optional field; never write through one that arrived.
//
// # The write half
//
// [Actuating] is it, and it arrived exactly as this comment promised it would:
// an OPTIONAL interface that embeds this one and that an actor type-asserts,
// which is the pattern magmux already uses for InputNotifier beside
// ToolController, reached through the command-with-reply path on [Actor].
type Middleware interface {
	// Name is how this stage is identified in diagnostics. Free text, never
	// switched on.
	Name() string

	// Observe transforms the fact set. A stage that has nothing to say
	// returns what it was given, unchanged.
	Observe(obs.Session) obs.Session
}

// Actuating is the OPTIONAL WRITE half of [Middleware], type-asserted by the
// actor — the same mechanism as [Waker], for the other direction.
//
// A middleware that only enriches does not implement it and must not be forced
// to pretend. A middleware that does implement it can act on the tool it
// understands, which is what a middleware understanding a tool is FOR:
// internal/obs/navigate focuses the pane a session runs in, and R5.3's `send`
// would be a second one, satisfying this same interface, changing nothing
// here.
//
// # Reads are events; writes are COMMANDS WITH REPLY CHANNELS
//
// [Middleware.Observe] is driven by an event the caller does not wait for.
// Everything here is driven by a command whose sender is blocked on the
// answer, because a write has an outcome and "it may or may not have happened"
// is not one. Both arrive on the actor's single channel, so an action cannot
// overtake the observation in front of it.
//
// # What a write-side stage may and may not do
//
// The same contract [Middleware.Observe] carries, with the same reason and a
// sharper edge:
//
//   - it runs ON THE ACTOR'S GOROUTINE, so nothing it does happens under
//     magmux's locks or in its render loop;
//   - it MAY do I/O — that is the whole point of an actor owning it;
//   - it MUST NOT block indefinitely. One actor processes one message at a
//     time, so a stage that never returns wedges that session's actor and
//     everything waiting on a reply from it, which today includes the
//     coordinator and therefore every other session's next cycle. Bound the
//     wait, select on the context, and report the absence of an answer as a
//     failed result with an error.
//
// # It is never reached without authorisation
//
// [Pipeline.Do] is the only way in, and it holds the gate: policy is resolved
// and the audit record is written before [act.Actuator.Do] is called. An
// implementation therefore does not re-check policy, and must not — a
// middleware that approved its own action would be a second policy nobody
// configured. The actor's own capabilities and perform commands are
// unexported so that this is a property rather than a convention.
type Actuating interface {
	Middleware
	act.Actuator
}

// Waker is the OPTIONAL half of [Middleware], type-asserted by the actor —
// the pattern the package doc above promised the write half would use, arriving
// first for the read half.
//
// It exists for the one kind of fact in this layer that changes because TIME
// PASSED rather than because something changed on disk: a staleness threshold.
// Every other fact arrives with an event. A pane that goes silent produces no
// event at all — silence is the absence of one — so a stage measuring silence
// has to be able to say when its own answer will next be different, and
// something has to be awake to ask again.
//
// The actor asks every stage that implements this after each chain run, takes
// the EARLIEST answer, and arms ONE one-shot timer. When it fires, the actor
// asks the pipeline for a cycle; it does not run the chain itself, so every
// published fact set still comes from a cycle and there is exactly one path
// into a document. A stage with nothing to wait for arms nothing, and an actor
// whose stages all have nothing to wait for holds no timer at all, which is
// what keeps an idle machine free.
//
// It is NOT a ticker and must not be used as one. A stage that answered a
// constant duration forever would turn its actor into a poll loop, which is the
// one thing this layer forbids outright — see
// termev.TestNoTickerAnywhereInTheObservationLayer, which enforces the letter
// of it, and this comment, which is the spirit.
type Waker interface {
	Middleware

	// WakeIn reports how long until this stage's answer changes on its own,
	// measured from the moment the observation that produced it finished.
	//
	// ok is false when nothing will change until something is observed. A
	// non-positive duration is treated as false for the same reason a stage
	// should not return one: it would arm an alarm that is already due, and
	// the cycle it caused would arm it again.
	WakeIn() (time.Duration, bool)
}

// Factory builds a FRESH middleware for one session. It is called once per
// actor, on the goroutine that starts the actor, before that actor's goroutine
// exists.
//
// A factory that returns the same instance twice breaks the rule above and
// nothing can detect it from here; that is the one thing a factory must not
// do.
type Factory func(sessionID string) Middleware

// buildChain instantiates one chain for one session. Nil factories are
// skipped rather than refused: a caller composing a chain from optional parts
// — a pane gate that only exists inside magmux, a tool middleware that only
// exists for tools it understands — should not have to filter the slice.
func buildChain(id string, factories []Factory) []Middleware {
	chain := make([]Middleware, 0, len(factories))
	for _, f := range factories {
		if f == nil {
			continue
		}
		if mw := f(id); mw != nil {
			chain = append(chain, mw)
		}
	}
	return chain
}
