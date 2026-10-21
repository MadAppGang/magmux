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
)

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
	Watching        bool
	Budget          watch.Budget
	LastArm         watch.ArmReport
}

// Ops is the coordinator's state, seen from inside Exec.
type Ops interface {
	Latest() (model.Fleet, bool)
	Session(k model.Key) (model.Session, bool)
	// Call runs fn on k's actor goroutine. find looks a stage up by Name().
	// fn must return promptly: the actor serves nothing else while it runs.
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
// An Exec honours ctx only until the coordinator ACCEPTS it. From then on fn
// has run or is running, with ctx, and its answer is the answer: returning
// ctx.Err() instead would tell the caller "not done" about something that may
// have been done, and the host's audit trail would say otherwise (review r1
// #6b). fn is bounded by its own obligation to honour ctx (Exec's doc).
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
	c.ctx = ctx
	c.reply = make(chan reply, 1)
	select {
	case p.in <- c:
	case <-p.done:
		return reply{}, ErrStopped
	case <-ctx.Done():
		return reply{}, ctx.Err()
	}
	cancelled := ctx.Done()
	if c.kind == cmdExec {
		cancelled = nil // accepted: wait for fn's own answer
	}
	select {
	case r := <-c.reply:
		return r, nil
	case <-p.done:
		return reply{}, ErrStopped
	case <-cancelled:
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

// Exec runs fn on the coordinator, between cycles. ctx bounds the wait for the
// coordinator to accept fn; once accepted, Exec returns fn's own answer. fn
// receives ctx and must return promptly, honouring it: it runs on the
// goroutine that services every session's next cycle.
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
