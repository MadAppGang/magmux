package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/notify"
)

// Actor is one session's goroutine. It owns that session's fact set and that
// session's middleware chain, and no other goroutine touches either.
//
// # Reads are events, writes are commands
//
// New observations arrive on [Actor.Observe] and are consumed; they carry no
// reply and the caller does not wait. Anything that reads actor-owned state —
// [Actor.Snapshot], [Actor.Stats] — sends a command carrying the channel it
// wants the answer on and blocks on that channel. That asymmetry is the shape
// phase 6's actuator needs, and it is established now because retrofitting a
// command path onto an event-only actor means rewriting every call site.
//
// # One channel, so ordering is FIFO
//
// Events and commands share a single channel deliberately. Two channels and a
// select would make "observe this, now tell me what you make of it" pick at
// random between the two, and the coordinator does exactly that on every
// cycle: it observes into every actor and then asks each one for its frozen
// fact set. With one channel the answer is guaranteed to be about the event
// just sent.
//
// # Freeze point 1
//
// When the chain finishes, the result is CLONED and the clone becomes the
// actor's state. The chain's own copy is then unreachable from the actor, so a
// middleware holding a pointer into it — and [obs.Session] is mostly pointers
// — cannot write into a fact set that has already been handed out.
type Actor struct {
	id   string
	in   chan message
	done chan struct{}
}

// ActorStats is what one actor has done. Read through a command like any other
// actor-owned state; see the package doc.
type ActorStats struct {
	// Observed is how many events the chain has been run for.
	Observed int
	// Stages is the length of this session's chain, which is a fact about the
	// actor and not about the factories it was built from: a factory that
	// returns nil contributes no stage.
	Stages int
	// Woken is how many times a [Waker] stage's one-shot alarm came due and
	// this actor asked the pipeline for a cycle. It is counted because it is
	// the only work this layer does that nothing on disk asked for, so it is
	// the number to look at if the "no polling" claim is ever doubted: on a
	// quiet machine it climbs to the length of the staleness ladder per
	// session and then stops.
	Woken int
	// Acted is how many actions this actor's chain has performed. It counts
	// what REACHED an actuator, so it is already past the gate: a refused
	// action never arrives here and is counted by the pipeline instead. The
	// two numbers differing is the whole point of having both.
	Acted int
}

// ActorConfig is the optional half of starting an actor: the two things an
// actor needs in order to WAIT, neither of which it needs in order to observe.
//
// A separate struct and a separate constructor, rather than two more
// parameters on [StartActor], because [StartActor] is what every caller that
// does not care about waiting still wants — the same shape, and the same
// reason, as magmux's own startObservation beside startObservationWith. The
// zero value is an actor that never arms a timer, which is exactly what an
// actor with no [Waker] in its chain does anyway.
type ActorConfig struct {
	// Clock creates the one-shot alarm. nil means this actor never waits:
	// [Waker] stages in its chain are not asked, and no timer exists.
	Clock obs.Clock

	// Wake is called ON THE ACTOR'S GOROUTINE when an alarm comes due.
	//
	// It MUST NOT BLOCK, and that is a correctness requirement rather than a
	// performance one. The coordinator asks each actor for a snapshot and
	// waits on the reply; an actor blocked inside Wake on a channel the
	// coordinator is not reading would deadlock the pair. The pipeline's own
	// implementation is a non-blocking send onto a buffered wake channel, and
	// a dropped wake costs nothing because the cycle it would have asked for
	// is already pending.
	Wake func()
}

type messageKind int

const (
	msgObserve messageKind = iota
	msgSnapshot
	msgCollect
	msgStats
	msgCapabilities
	msgPerform
	msgStop
)

type message struct {
	kind    messageKind
	session obs.Session
	// action and ctx belong to msgPerform only. The context travels WITH the
	// message rather than being held by the actor: it is the caller's
	// cancellation, the caller is blocked on the reply, and an actor that
	// kept one would be holding a value whose owner has gone.
	action act.Action
	ctx    context.Context
	reply  chan actorReply
}

type actorReply struct {
	session obs.Session
	notes   []notify.Note
	stats   ActorStats
	caps    []act.Capability
	result  act.Result
	err     error
}

// actorBacklog decouples the coordinator from an actor that is mid-chain. The
// coordinator observes into every actor before asking any of them for a
// result, so the whole point is that the Nth observe does not wait on the
// first actor's filesystem work.
const actorBacklog = 8

// StartActor builds one session's chain from factories and starts the
// goroutine that owns it. The caller owns the result and must [Actor.Stop] it.
//
// The chain is built HERE, on the calling goroutine, from factories rather
// than from built middleware: there is deliberately no way to hand an actor a
// middleware instance, because that is the only way two sessions could end up
// sharing one.
func StartActor(id string, factories []Factory) (*Actor, error) {
	return StartActorWith(id, factories, ActorConfig{})
}

// StartActorWith is [StartActor] plus the ability to WAIT: a clock to arm a
// one-shot alarm with, and a callback to run when it comes due. See
// [ActorConfig].
//
// The two are separate because waiting is separate: an actor whose chain
// contains no [Waker] behaves identically either way, and every caller that
// only observes should not have to name a clock it will never use.
func StartActorWith(id string, factories []Factory, cfg ActorConfig) (*Actor, error) {
	if id == "" {
		// An actor with no identity cannot be routed to, and the coordinator
		// keys its table by this. Caught here rather than becoming a second
		// actor for every unidentifiable session.
		return nil, fmt.Errorf("pipeline: an actor needs a session id")
	}
	a := &Actor{
		id:   id,
		in:   make(chan message, actorBacklog),
		done: make(chan struct{}),
	}
	go a.loop(buildChain(id, factories), cfg)
	return a, nil
}

// ID is the session this actor is for.
func (a *Actor) ID() string { return a.id }

// Observe hands the actor a new observation of its session. It does not wait
// for the chain to run.
//
// It reports whether the observation was accepted. False means either that the
// actor has stopped or that its backlog is full, and the caller counts that:
// the drop belongs to the sender's books, because the sender is the one
// goroutine that knows how many it tried to send. A dropped observation is not
// a lost session — the next cycle reads the whole registry again — but a
// snapshot taken right after one is a moment old, and that is worth counting.
func (a *Actor) Observe(s obs.Session) bool {
	// done first and on its own: a single select over both cases picks at
	// random when both are ready, so a stopped actor would accept about half
	// of everything sent to it — and the return value exists precisely so a
	// caller can stop reporting to an actor that has gone. A signal that
	// genuinely races the stop is still a race, and honestly so: either answer
	// is true of some instant.
	select {
	case <-a.done:
		return false
	default:
	}
	select {
	case a.in <- message{kind: msgObserve, session: s}:
		return true
	default:
		return false
	}
}

// Snapshot returns the session's frozen fact set — the output of the last
// completed chain run, deep-copied at chain exit. false means the actor has
// stopped.
//
// A session that has been observed zero times answers with the zero
// [obs.Session], which is "undetermined" in every field by construction.
//
// The value shares pointers with the actor's own state, and that is safe
// because the actor never writes THROUGH a pointer: the next chain run
// replaces the whole fact set with a new clone and leaves this one standing.
// A caller that wants to hold it past a publish clones it — which is what the
// hub does, once, for the whole document.
func (a *Actor) Snapshot() (obs.Session, bool) {
	r, ok := a.ask(msgSnapshot)
	return r.session, ok
}

// collect is [Actor.Snapshot] plus the notifications this session's chain has
// decided since the last collection, which it DRAINS.
//
// It is the coordinator's per-cycle read and it is unexported for the same
// reason perform is: draining is destructive, so a second caller would take
// notifications the coordinator was going to deliver and drop them on the
// floor. Snapshot stays non-draining beside it, because the action path reads
// a fact set without consuming anything.
//
// One command rather than two, so the fact set and the notes come from the
// same instant. Two round trips would leave a window in which an observation
// landed between them, and the note would then describe a session the
// published document does not.
func (a *Actor) collect() (obs.Session, []notify.Note, bool) {
	r, ok := a.ask(msgCollect)
	return r.session, r.notes, ok
}

// Stats returns what this actor has done. false means it has stopped.
func (a *Actor) Stats() (ActorStats, bool) {
	r, ok := a.ask(msgStats)
	return r.stats, ok
}

// capabilities asks this session's chain what it can DO — the write half's
// read, and the first half of every action.
//
// # It is unexported, and that is the security boundary
//
// So is [Actor.perform]. Together they mean an actor's actuators are
// unreachable from outside this package, so the only way to an actuator is
// [Pipeline.Do], which is the only thing that holds the gate. A caller that
// could reach an actor directly could act without policy and without an audit
// record, and "please go through the gate" would be a convention rather than a
// property. Exporting either of these is how that stops being true.
//
// false means the actor has stopped, which a caller reports as "nothing can do
// this for this session" — the same answer as a chain with no write half, and
// the honest one.
func (a *Actor) capabilities() ([]act.Capability, bool) {
	r, ok := a.ask(msgCapabilities)
	return r.caps, ok
}

// perform runs ONE action through this session's chain. Unexported for the
// reason on [Actor.capabilities].
//
// It is a command with a reply channel — the write shape this package's doc
// has described since phase 3 — and the caller blocks on the reply, which is
// what makes an action and the observations around it strictly ordered: they
// share one channel, so an action cannot overtake the observation before it or
// be overtaken by the one after.
func (a *Actor) perform(ctx context.Context, action act.Action) (act.Result, error) {
	reply := make(chan actorReply, 1)
	r, ok := a.askMsg(message{kind: msgPerform, action: action, ctx: ctx, reply: reply})
	if !ok {
		return act.Result{}, fmt.Errorf("pipeline: session %s is no longer being observed", a.id)
	}
	return r.result, r.err
}

// Stop shuts the actor down and waits for its goroutine to finish. It is
// idempotent and may be called from a different goroutine than the one that
// started it.
//
// Stopping is a SIGNAL rather than a closed channel: closing a channel twice
// panics, and making that safe needs a sync.Once, which is a mutex, which this
// layer does not have. A signal that races another signal merely queues, and
// one that arrives after the loop has gone is answered by done.
func (a *Actor) Stop() {
	select {
	case a.in <- message{kind: msgStop}:
	case <-a.done:
	}
	<-a.done
}

// ask sends a command and waits for its reply.
//
// The reply channel is buffered so that the actor's send cannot block even if
// this call is abandoned — which it can be, on the done path below.
func (a *Actor) ask(kind messageKind) (actorReply, bool) {
	return a.askMsg(message{kind: kind, reply: make(chan actorReply, 1)})
}

// askMsg is ask for a command that carries more than its kind. The reply
// channel is the caller's, so that a command with a payload is built in one
// place rather than half here and half at the call site.
func (a *Actor) askMsg(m message) (actorReply, bool) {
	reply := m.reply
	select {
	case <-a.done:
		return actorReply{}, false
	default:
	}
	select {
	case a.in <- m:
	case <-a.done:
		return actorReply{}, false
	}
	select {
	case r := <-reply:
		return r, true
	case <-a.done:
		// The actor stopped before answering. That is only reachable when a
		// stop was queued ahead of this command; a command already taken off
		// the channel is always answered, because the reply send cannot block.
		return actorReply{}, false
	}
}

func (a *Actor) loop(chain []Middleware, cfg ActorConfig) {
	defer close(a.done)

	var (
		frozen obs.Session
		stats  = ActorStats{Stages: len(chain)}
		timer  obs.Timer
		timerC <-chan time.Time
	)
	// Resolved once, on this goroutine, before the first message: a chain
	// never changes after an actor is built, so the type assertion is not
	// worth repeating per observation.
	wakers := wakersIn(chain)
	// The write half, found the same way and for the same reason. A chain
	// with none — which is every chain in a process that cannot act — leaves
	// this actor with no capabilities at all, and every action aimed at it is
	// refused as "nothing can do this", which is true.
	actuators := actuatorsIn(chain)
	// And the third optional half: the stages that can raise a notification.
	announcers := announcersIn(chain)

	// arm replaces the live alarm with one for the EARLIEST deadline any stage
	// asked for, or with none at all. The old alarm is stopped BEFORE the new
	// one is created, which is the ordering a test relies on to prove that a
	// re-arm released its predecessor: obs.FakeTimer.Stopped is only safe to
	// read once the replacement has been observed.
	arm := func() {
		if timer != nil {
			timer.Stop()
			timer, timerC = nil, nil
		}
		if cfg.Clock == nil || len(wakers) == 0 {
			return
		}
		var (
			next  time.Duration
			armed bool
		)
		for _, w := range wakers {
			d, ok := w.WakeIn()
			if !ok || d <= 0 {
				// Non-positive is refused rather than clamped to zero. A
				// zero-delay alarm fires immediately, asks for a cycle, and is
				// re-armed by that cycle — which is a poll loop with extra
				// steps, and this layer has none.
				continue
			}
			if !armed || d < next {
				next, armed = d, true
			}
		}
		if !armed {
			return
		}
		timer = cfg.Clock.NewTimer(next)
		timerC = timer.C()
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case m := <-a.in:
			switch m.kind {
			case msgObserve:
				s := m.session
				for _, mw := range chain {
					s = mw.Observe(s)
				}
				// FREEZE POINT 1. The clone is what makes the freeze real
				// rather than conventional: every pointer in the chain's copy
				// is replaced by a fresh one, so a stage that kept a reference
				// to something it set is writing into an object nobody will
				// ever read again.
				frozen = s.Clone()
				stats.Observed++
				// Re-armed from the chain that just ran, and only from there.
				// A stage's answer to "when do I change" is about the
				// observation it just made, so asking at any other moment
				// would be asking about a fact set that no longer exists.
				arm()

			case msgSnapshot:
				m.reply <- actorReply{session: frozen}

			case msgCollect:
				// The notes are drained here, on the actor's goroutine, so
				// "decided once" and "handed over once" are the same event.
				// The stage is the only other thing that touches them and it
				// runs on this goroutine too.
				var notes []notify.Note
				for _, an := range announcers {
					notes = append(notes, an.Pending()...)
				}
				m.reply <- actorReply{session: frozen, notes: notes}

			case msgStats:
				m.reply <- actorReply{stats: stats}

			case msgCapabilities:
				m.reply <- actorReply{caps: declaredBy(actuators)}

			case msgPerform:
				// On the actor's goroutine, in the same queue as the
				// observations, so the fact set the gate decided against is
				// the one this session had when it decided. The stage may do
				// I/O here and may not block indefinitely; see act.Actuator,
				// where the reason is the whole of this loop.
				res, err := performOn(actuators, m.ctx, m.action)
				if err == nil {
					stats.Acted++
				}
				m.reply <- actorReply{result: res, err: err}

			case msgStop:
				return
			}

		case <-timerC:
			// One-shot: the alarm has delivered and is finished with. It is
			// NOT re-armed here — the cycle this asks for will observe again,
			// and that observation is what decides whether there is anything
			// left to wait for. Re-arming here as well would give this session
			// two alarms for one deadline.
			timer, timerC = nil, nil
			stats.Woken++
			if cfg.Wake != nil {
				// On the actor's goroutine, and it must not block; see
				// ActorConfig.Wake, where the deadlock it would otherwise
				// cause is spelled out.
				cfg.Wake()
			}
		}
	}
}

// wakersIn picks out the stages that can say when their own answer changes.
// A chain with none — which is every chain outside magmux, where there is no
// pane to be silent — leaves the actor with nothing to wait for and no timer.
func wakersIn(chain []Middleware) []Waker {
	var out []Waker
	for _, mw := range chain {
		if w, ok := mw.(Waker); ok {
			out = append(out, w)
		}
	}
	return out
}

// actuatorsIn picks out the stages with a write half. The type assertion is
// the whole of the "optional interface" mechanism — the same one magmux uses
// for InputNotifier beside ToolController — and a stage that only enriches
// simply does not satisfy it and is not asked to pretend.
func actuatorsIn(chain []Middleware) []Actuating {
	var out []Actuating
	for _, mw := range chain {
		if x, ok := mw.(Actuating); ok {
			out = append(out, x)
		}
	}
	return out
}

// declaredBy collects what every write-capable stage says it can do to this
// session, in chain order.
func declaredBy(actuators []Actuating) []act.Capability {
	var caps []act.Capability
	for _, x := range actuators {
		caps = append(caps, x.Capabilities()...)
	}
	return caps
}

// performOn routes an action to the stage that declares its verb.
//
// FIRST DECLARATION WINS, and the duplicate is not an error: two stages
// declaring one verb is a composition mistake rather than a runtime condition,
// and refusing at the moment of use would turn it into an outage for an action
// that has already been authorised and recorded as attempted. Chain order is
// the tie-break because chain order is the one thing the caller controls.
//
// A verb nothing declares is refused here as well as at the gate. The gate's
// check is the one that matters — it happens before anything is recorded as
// attempted — and this one exists because "the gate checked" is an assumption
// this function would otherwise be making about a caller it cannot see.
func performOn(actuators []Actuating, ctx context.Context, a act.Action) (act.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, x := range actuators {
		for _, c := range x.Capabilities() {
			if c.Verb != a.Verb {
				continue
			}
			return x.Do(ctx, a)
		}
	}
	return act.Result{}, fmt.Errorf("%q on session %s: %w", a.Verb, a.Session, act.ErrNotCapable)
}
