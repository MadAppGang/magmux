// Package pipeline is the runtime: one coordinator goroutine that owns the
// sources, the watcher, the actor table and the hub; one actor per session key
// owning that session's chain; and three freeze points (an actor's clone at
// chain exit, the document's clone at collect, and the hub's copy per
// subscriber). Channels only: there is no mutex, no atomic and no ticker.
//
// A cycle runs on a coalesced watch event, a Refresh, a RefreshSources
// completion, an actor's wake, or a host-problem follow-up, in the order §3.1
// numbers. Exec and OnEffects run host code ON the coordinator, between cycles,
// which is what keeps a host's single-owner state (magmux's audit writer)
// single-owner.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/claudestate/join"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/claudestate/verdict"
	"github.com/MadAppGang/magmux/claudestate/watch"
)

// Config is how a Pipeline is built. A plain value, copied on the way in.
type Config struct {
	Root     ports.Root // required: at least one non-zero tree (else ErrNoRoot)
	Oracle   ports.ProcessOracle
	Locator  ports.PaneLocator // may also implement ports.CycleLocator
	Activity ports.PaneActivity
	Shell    ports.PaneShell
	Agents   ports.AgentsListing // nil = source off
	Verdict  verdict.Rules
	Chain    []stage.Factory // after claudecode, before verdict
	// OnEffects runs on the coordinator goroutine after publish and release,
	// once per cycle that produced effects. It must bound itself. The problems
	// it returns ride out on the next Delivery (C13).
	OnEffects func(ctx context.Context, b EffectBatch) []model.Problem
	Watch     bool         // false => no watcher, no descriptors, every Grant Unwatched
	Budget    watch.Budget // zero => watch.RuntimeBudget()
	// Now is the document epoch and the agents source's gate clock; nil =>
	// Clock.Now.
	Now        func() time.Time
	Clock      ports.Clock   // required
	RetryDelay time.Duration // the torn-read retry; 0 => 20 ms
	// TailBytes and TimingTailBytes: 0 => claude.FactsTailBytes,
	// claude.TimingTailBytes.
	TailBytes, TimingTailBytes int
	SubscriberBuffer           int // default per-subscriber buffer; 0 => 4
}

// The errors.
var (
	ErrStopped     = errors.New("pipeline: stopped")
	ErrReentrant   = errors.New("pipeline: called from inside Exec or OnEffects with its context")
	ErrNoSuchKey   = errors.New("pipeline: no actor for that key")
	ErrSourceOff   = errors.New("pipeline: the agents source is off")
	ErrNoRoot      = errors.New("pipeline: no Claude tree was given")
	ErrUnwatchable = errors.New("pipeline: Watch was asked of a tree that cannot be watched (Dir is empty)")
	ErrBadBuffer   = errors.New("pipeline: a subscriber buffer cannot be negative")
	errNoClock     = errors.New("pipeline: Config.Clock is required")

	// ErrOutcomeUnknown is Exec's and Ops.Call's answer when the caller's
	// context ended AFTER the work was accepted and before its result came
	// back. The work may have run, may be running and may yet complete: it is
	// neither a success nor a refusal, and a host that records outcomes (an
	// audit trail) must record it as unknown, never as "not done". It always
	// wraps the context's own error, so errors.Is(err, context.Canceled) and
	// errors.Is(err, context.DeadlineExceeded) still answer which.
	ErrOutcomeUnknown = errors.New("pipeline: accepted, and the caller stopped waiting before the outcome was known; it may have run")
)

// outcomeUnknown is ErrOutcomeUnknown wrapping ctx's error.
func outcomeUnknown(ctx context.Context) error {
	return fmt.Errorf("%w: %w", ErrOutcomeUnknown, ctx.Err())
}

// DefaultSubscriberBuffer is a subscription's buffer when nobody says.
const DefaultSubscriberBuffer = 4

// Delivery is one published document, as one subscriber receives it.
type Delivery struct {
	Doc      model.Fleet
	Problems []model.Problem
	Changes  []join.Change
	// Dropped is how many documents this subscriber lost since its last
	// delivery, and Resync says this delivery replaced them.
	Dropped int
	Resync  bool
}

// EffectBatch is one cycle's effects, each paired with its session AS
// PUBLISHED. Effect is the stage's own value, never a wrapper.
type EffectBatch struct {
	Epoch time.Time
	Items []EffectItem
}

// EffectItem is one effect.
type EffectItem struct {
	Key       model.Key
	Effect    stage.Effect
	Published model.Session
}

// RefreshOutcome is what one RefreshSources did for the agents source. A
// refusal by the rate limit or the backoff is NOT an error.
type RefreshOutcome struct {
	Ran     bool
	Skipped string // "" | "min-interval" | "backoff"
	At      time.Time
	Entries int
	Err     error
}

// Stats is what the pipeline has done.
type Stats struct {
	Cycles, Published, ActorsStarted, ActorsStopped, ObservationsDropped int
	Subscribers, HubDropped, Resyncs                                     int
	WatchEvents, WatchDropped                                            int
	// TranscriptGlobs counts the by-id globs run: D5's cost, V3's number.
	TranscriptGlobs int
	// Watching is false with Watch off, on demand (D15), and once the
	// watcher has stopped: in all three every cycle is an unwatched pull.
	Watching bool
	Budget   watch.Budget
	LastArm  watch.ArmReport
}

// Ops is the coordinator's state, seen from inside Exec.
type Ops interface {
	Latest() (model.Fleet, bool)
	Session(k model.Key) (model.Session, bool)
	// Call runs fn on k's actor goroutine. find looks a stage up by Name().
	// fn must return promptly: the actor serves nothing else while it runs.
	//
	// ctx follows Exec's acceptance rule, one level down. Before the actor
	// has fn, an ended ctx is ctx.Err() and fn never runs; once it has fn, an
	// answer already given wins, and a ctx that ends first is
	// ErrOutcomeUnknown wrapping ctx.Err() — fn may be completing (a focus
	// that moves the keyboard) and must not be reported as not done. An actor
	// that is gone is ErrNoSuchKey.
	Call(ctx context.Context, k model.Key, fn func(s model.Session, find func(name string) (stage.Stage, bool)) error) error
}

// Subscription is one sink's channel.
type Subscription struct {
	name string
	c    chan Delivery
}

// C is closed on Unsubscribe and on Stop.
func (s *Subscription) C() <-chan Delivery { return s.c }

// Name is the name given at Subscribe.
func (s *Subscription) Name() string { return s.name }

// Pipeline is the coordinator, seen from outside.
type Pipeline struct {
	in       chan command
	refresh  chan struct{} // 1 slot: a pending refresh coalesces
	wake     chan struct{} // 1 slot: an actor's alarm came due
	selfWake chan struct{} // 1 slot: deliver host problems (C13)
	done     chan struct{}
}

// reentry tags the context fn and OnEffects receive.
type reentry struct{}

func (p *Pipeline) reentrant(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	q, _ := ctx.Value(reentry{}).(*Pipeline)
	return q == p
}

// Start builds the pipeline and starts the coordinator, and the watcher when
// Watch is set. It publishes nothing until the first Refresh or event (C9).
func Start(cfg Config) (*Pipeline, error) {
	if cfg.Clock == nil {
		return nil, errNoClock
	}
	r := cfg.Root
	if r.Sessions.IsZero() && r.Jobs.IsZero() && r.Projects.IsZero() {
		return nil, ErrNoRoot
	}
	var w *watch.Watcher
	if cfg.Watch {
		for _, t := range []ports.Tree{r.Sessions, r.Jobs, r.Projects} {
			if !t.IsZero() && t.Dir == "" {
				return nil, ErrUnwatchable
			}
		}
		var never []string
		if r.Projects.Dir != "" {
			never = append(never, r.Projects.Dir)
		}
		var err error
		w, err = watch.Start(watch.Config{Clock: cfg.Clock, Budget: cfg.Budget, Never: never})
		if err != nil {
			return nil, err
		}
	}
	if cfg.Now == nil {
		cfg.Now = cfg.Clock.Now
	}
	if cfg.SubscriberBuffer <= 0 {
		cfg.SubscriberBuffer = DefaultSubscriberBuffer
	}
	p := &Pipeline{
		in:       make(chan command),
		refresh:  make(chan struct{}, 1),
		wake:     make(chan struct{}, 1),
		selfWake: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	c := newCoordinator(p, cfg, w)
	go c.loop()
	return p, nil
}

type commandKind int

const (
	cmdSubscribe commandKind = iota
	cmdUnsubscribe
	cmdExec
	cmdStats
	cmdRefreshSources
	cmdStop
)

type command struct {
	kind   commandKind
	ctx    context.Context
	name   string
	buffer int
	sub    *Subscription
	fn     func(context.Context, Ops) error
	ids    []model.SourceID
	reply  chan reply
}

type reply struct {
	sub     *Subscription
	err     error
	stats   Stats
	outcome RefreshOutcome
}

// ask sends one command and waits for its reply, honouring ctx and Stop.
//
// ACCEPTANCE is the line. Until the coordinator takes the command, a context
// that ends is a plain refusal: nothing ran. Once it has taken it, a result
// already sent always wins, and a caller whose context ends first gets, for an
// Exec, ErrOutcomeUnknown wrapping ctx.Err() — fn has run or is running with
// that ctx, so ctx.Err() alone would tell the caller "not done" about
// something that may have been done (review r1 #6b), and waiting for fn would
// hand the caller's goroutine to host code that ignores its context (review
// r2 #2). The other commands are bookkeeping with no effect to misreport, and
// keep the plain error.
func (p *Pipeline) ask(ctx context.Context, c command) (reply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.reentrant(ctx) {
		return reply{}, ErrReentrant
	}
	select {
	case <-p.done:
		return reply{}, ErrStopped
	default:
	}
	if err := ctx.Err(); err != nil {
		return reply{}, err // never offered: a ready send would win the select at random
	}
	c.ctx = ctx
	c.reply = make(chan reply, 1)
	select {
	case p.in <- c:
	case <-p.done:
		return reply{}, ErrStopped
	case <-ctx.Done():
		return reply{}, ctx.Err()
	}
	// Accepted. The coordinator answers a command it took before it can
	// stop, so a reply that is ready is the answer, whatever else is.
	select {
	case r := <-c.reply:
		return r, nil
	case <-p.done:
		select {
		case r := <-c.reply:
			return r, nil
		default:
		}
		return reply{}, ErrStopped
	case <-ctx.Done():
		select {
		case r := <-c.reply:
			return r, nil
		default:
		}
		if c.kind == cmdExec {
			return reply{}, outcomeUnknown(ctx)
		}
		return reply{}, ctx.Err()
	}
}

// Subscribe registers a sink. buffer 0 takes Config.SubscriberBuffer.
func (p *Pipeline) Subscribe(ctx context.Context, name string, buffer int) (*Subscription, error) {
	if buffer < 0 {
		return nil, ErrBadBuffer
	}
	r, err := p.ask(ctx, command{kind: cmdSubscribe, name: name, buffer: buffer})
	if err != nil {
		return nil, err
	}
	return r.sub, nil
}

// Unsubscribe removes a sink and closes its channel. Idempotent.
func (p *Pipeline) Unsubscribe(ctx context.Context, s *Subscription) error {
	if s == nil {
		return nil
	}
	_, err := p.ask(ctx, command{kind: cmdUnsubscribe, sub: s})
	return err
}

// Refresh asks for a cycle. It never blocks, and may be called from a
// callback. false once stopped.
func (p *Pipeline) Refresh() bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.refresh <- struct{}{}:
	default: // a refresh is already pending
	}
	return true
}

// RefreshSources refreshes the agents source, the only source with its own
// refresh; any other id is satisfied by the next cycle. ctx bounds only this
// caller's wait: the List runs under the pipeline's own context, so a caller
// giving up neither cancels it nor fails the source for anyone else.
func (p *Pipeline) RefreshSources(ctx context.Context, ids []model.SourceID) (RefreshOutcome, error) {
	r, err := p.ask(ctx, command{kind: cmdRefreshSources, ids: ids})
	if err != nil {
		return RefreshOutcome{}, err
	}
	return r.outcome, r.err
}

// Exec runs fn on the coordinator, between cycles. fn receives ctx and must
// return promptly, honouring it: it runs on the goroutine that services every
// session's next cycle.
//
// What Exec returns depends on whether the coordinator had ACCEPTED fn when
// ctx ended:
//   - not yet: ctx.Err(), plainly. fn never runs.
//   - accepted, and fn answered first: fn's own answer, whatever ctx did.
//   - accepted, and ctx ended first: ErrOutcomeUnknown wrapping ctx.Err(),
//     at once. fn has run, is running or will finish; Exec does not wait for
//     it, so a fn that ignores ctx cannot hold the caller. A host recording
//     outcomes records this one as unknown — never as "not done" — and reads
//     its own record (the audit trail) for what happened.
//
// Exec never blocks for ever: it returns on fn's answer, ctx ending, or Stop.
func (p *Pipeline) Exec(ctx context.Context, fn func(context.Context, Ops) error) error {
	r, err := p.ask(ctx, command{kind: cmdExec, fn: fn})
	if err != nil {
		return err
	}
	return r.err
}

// Stats reports what the pipeline has done.
func (p *Pipeline) Stats(ctx context.Context) (Stats, error) {
	r, err := p.ask(ctx, command{kind: cmdStats})
	return r.stats, err
}

// Stop shuts the pipeline down and waits for every goroutine it owns. It is
// idempotent. It MUST NOT be called from fn or OnEffects: that is the one
// misuse a context cannot detect (C15).
func (p *Pipeline) Stop() {
	select {
	case p.in <- command{kind: cmdStop}:
	case <-p.done:
	}
	<-p.done
}
