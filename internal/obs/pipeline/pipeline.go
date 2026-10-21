package pipeline

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
	"github.com/MadAppGang/magmux/internal/obs/fswatch"
	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/internal/obs/registry"
)

// Config is how a [Pipeline] is built. It is a plain value, copied on the way
// in: nothing mutates it afterwards, so nothing needs atomic access and "who
// may change this while a cycle is running" never arises.
type Config struct {
	// Root is the registry directory. Required.
	Root string

	// Oracle answers the process-liveness gate. nil yields
	// [obs.LivenessUnknown] for every session — honest, and what a test that
	// forgot to inject one should get rather than a probe that signals
	// whatever pids happen to exist on the machine running it.
	Oracle registry.ProcessOracle

	// Locator answers the pane gate. nil means "there is no pane table in
	// this process", which is true of anything running outside a magmux —
	// `magmux agents` is exactly that — and leaves both pane facts undetermined
	// rather than asserting a pane is absent.
	//
	// A non-nil locator adds the pane-gate middleware to the FRONT of every
	// session's chain automatically. It is not left to the caller to
	// remember: the gate is the second half of the inclusion rule, and a
	// caller that forgot it would produce a document that looks complete and
	// silently reports every session as pane-less.
	Locator obs.PaneLocator

	// Activity is the INFERRED tier's input: when each pane last produced
	// output, and when something was last pushed into it. nil means there is
	// no terminal evidence in this process — true of anything running outside
	// a magmux — so every session is resolved on the authoritative tier alone
	// and no conflict can arise.
	//
	// Like Locator, it is not left to the caller to remember to add a stage
	// for. The verdict stage is appended to every chain whatever this is set
	// to; a nil here only decides whether that stage has an inferred tier to
	// weigh.
	Activity obs.PaneActivity

	// Shell is the GENERIC tier's direct input: who owns each pane's terminal
	// and what its shell's own OSC 133 marks last said. nil leaves the
	// inferred tier with the staleness cascade alone — the honest answer for
	// anything running outside a magmux, and exactly the behaviour this
	// package had before the port existed.
	//
	// Like Locator and Activity it adds no stage of its own: the verdict stage
	// is appended to every chain either way, and this only decides whether
	// that stage has direct terminal evidence to weigh against silence.
	Shell obs.PaneShell

	// Focuser is the WRITE port: what moves keyboard focus. nil means nothing
	// in this process can — true of anything running outside a magmux — and
	// every session then declares no navigate capability at all, which is
	// "cannot" rather than "will not".
	//
	// Like Locator and Activity it is not left to the caller to add a stage
	// for; see navigateStage.
	Focuser obs.PaneFocuser

	// Policy is the action policy. THE ZERO VALUE REFUSES EVERYTHING,
	// navigate included, and that is deliberate: a pipeline built from a
	// struct literal that says nothing about policy must not be able to act.
	// A caller that wants requirements.md R6.1's posture passes act.Default(),
	// and one that has a configuration passes what act.Load returned.
	Policy act.Policy

	// Audit is where every action and every refusal is recorded. nil refuses
	// every action with act.ErrUnaudited — R5.A says every action writes a
	// record, so an action that cannot be recorded does not happen. The same
	// rule covers notifications; see Notify.
	Audit act.Auditor

	// Notify is how a decided notification reaches a human. nil means nothing
	// in this process can deliver one — true of any platform without a
	// notification centre, and of `magmux agents`, which prints instead.
	//
	// What it does NOT control is whether notifications are DECIDED: that is
	// act.Policy.Notify, whose zero value is silence, and it is the switch the
	// user holds. A decided notification with no port is still recorded, with
	// an outcome saying nothing could deliver it, because an attempt with no
	// outcome beside it means "we do not know what happened" and we know
	// exactly what happened.
	//
	// It is called ON THIS PIPELINE'S COORDINATOR GOROUTINE and must bound
	// itself. Nothing here starts a timer for it — there are no timers in this
	// package at all — so the deadline belongs to the implementation, exactly
	// as act.Actuator's does. See notify.Port.
	Notify notify.Port

	// Chain is the rest of the middleware, in order, after the pane gate and
	// before the navigator and the verdict. Each factory is called once per
	// session.
	Chain []Factory

	// Watch starts a filesystem watch on Root, so that a record changing
	// produces a cycle. Without it the only cycles are the ones
	// [Pipeline.Refresh] asks for, which is what `--once` and most tests
	// want. There is no ticker either way.
	Watch bool

	// Now answers what the instant is, for the document's epoch. nil takes
	// time.Now. It is separate from Clock on purpose; see registry.Config.Now,
	// which documents the trap in full.
	Now func() time.Time

	// Clock is where this path's one-shot timers come from: the registry's
	// torn-read retry, fswatch's coalescing window, and each actor's staleness
	// alarm. nil takes [obs.RealClock].
	//
	// There are only ever one-shot timers behind it. An actor holds at most
	// one alarm and only while some stage of its chain has an answer that will
	// change on its own; an actor whose session is settled holds none.
	Clock obs.Clock

	// RetryDelay is how long a torn read waits before its single retry.
	// Non-positive takes the registry's own default.
	RetryDelay time.Duration
}

// Stats is what the pipeline has done. Read through a command with a reply,
// like every other read of goroutine-owned state here.
type Stats struct {
	// Cycles is how many times the registry has been read.
	Cycles int
	// Published is how many documents were handed to the hub. It is lower
	// than Cycles only when the hub has stopped.
	Published int
	// Actors is how many session actors are running right now, and Started /
	// Stopped are the lifetime totals. Started minus Stopped equal to Actors
	// is the invariant a goroutine leak breaks.
	Actors        int
	ActorsStarted int
	ActorsStopped int
	// ObservationsDropped is how many times an actor could not accept an
	// observation because its backlog was full. Counted by the sender, which
	// is the only goroutine that knows it tried.
	ObservationsDropped int
	// DuplicateSessions is how many sessions in a single document shared an
	// identity with an earlier one. Those are passed through unmodified
	// rather than being routed to an actor that belongs to a different
	// session; a non-zero count means the registry holds two records claiming
	// to be the same session.
	DuplicateSessions int
	// WatchDropped is how many change notifications fswatch discarded
	// because this pipeline was not reading. It does not affect correctness —
	// a cycle re-reads the whole directory rather than applying an event — but
	// a consumer counting changes would be short.
	WatchDropped int
	// WatchErrors is how many errors the watcher has reported. Each one is
	// also delivered to every sink as a [registry.Problem], because the way
	// this subsystem fails is SILENCE.
	WatchErrors int
	// Wakes is how many cycles were asked for by a staleness deadline coming
	// due rather than by a change on disk. It is the number to check when the
	// no-polling claim is doubted: it is bounded by the length of the
	// staleness ladder per quiet period per session, and on a machine where
	// nothing is happening it stops climbing once every ladder is exhausted.
	// A Wakes count that grows without bound is a poll loop, whoever wrote it.
	Wakes int
	// Actions is how many actions were REQUESTED, and ActionsRefused how many
	// of those the gate refused. The two are counted separately rather than
	// as a performed-count, because a refusal is the interesting half: a run
	// where every action was refused and one where none was requested look
	// identical in any count that only tracks what happened.
	Actions        int
	ActionsRefused int
	// Notifications is how many notifications reached a port, and
	// NotificationsSuppressed how many were decided and then silenced by
	// requirements.md R6's `notify.quiet`. They are counted apart for the same
	// reason Actions and ActionsRefused are: a night where every notification
	// was suppressed and one where none was raised look identical in any count
	// that only tracks what was sent, and the first one is a config working
	// while the second is a config doing nothing.
	Notifications           int
	NotificationsSuppressed int
	// NotificationsUnrecorded is how many were decided and then NOT sent
	// because the audit trail could not take them. It is the number that
	// explains a silent night on a machine whose state directory is
	// unwritable; each one also rides out to every sink as a problem.
	NotificationsUnrecorded int
	// Subscribers is the hub's own accounting, one entry per sink.
	Subscribers []SubscriberStats
}

// Pipeline is the coordinator: it owns the registry source, the watcher, the
// table of per-session actors and the hub, and it is the only goroutine that
// touches any of them.
//
// A cycle is: read the directory, hand each session to its actor, ask each
// actor for the fact set its chain produced, and publish one document. It
// happens when a record changes or when somebody asks. Never on a clock.
type Pipeline struct {
	in chan pipeMessage
	// wake carries "a staleness deadline came due" from an actor's goroutine
	// to the coordinator's. It is a channel of its own rather than a
	// pipeMessage so that the send can be NON-BLOCKING without a full
	// backlog ever refusing a Stop or a Stats: an actor that blocked here
	// while the coordinator was waiting on that same actor's reply would
	// deadlock the pair. One buffered slot is all it needs, because a pending
	// wake already means a cycle is coming and a second one would ask for the
	// same thing.
	wake chan struct{}
	done chan struct{}
	hub  *Hub
}

type pipeMessageKind int

const (
	pipeRefresh pipeMessageKind = iota
	pipeStats
	pipeAct
	pipeStop
)

type pipeMessage struct {
	kind  pipeMessageKind
	reply chan Stats
	// The three below belong to pipeAct only. The request and the caller's
	// context travel with the message; the answer comes back on actReply,
	// which is a channel of its own because a Report is not a Stats and
	// overloading one reply channel with two meanings is how a select picks
	// the wrong one.
	req      act.Request
	ctx      context.Context
	actReply chan actOutcome
}

// actOutcome is one action's answer on its way back to the caller.
type actOutcome struct {
	report act.Report
	err    error
}

// Start builds the pipeline and starts its goroutines: the coordinator, the
// hub, and the watcher if [Config.Watch] is set. Actors start as sessions
// appear.
//
// It publishes nothing on its own. Subscribe first, then call
// [Pipeline.Refresh] — a document broadcast before anybody could subscribe is
// a document nobody receives, and making Start publish would make `--once`
// race its own sink.
func Start(cfg Config) (*Pipeline, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("pipeline: a registry root is required")
	}
	src, err := registry.New(registry.Config{
		Root:       cfg.Root,
		Oracle:     cfg.Oracle,
		Now:        cfg.Now,
		Clock:      cfg.Clock,
		RetryDelay: cfg.RetryDelay,
	})
	if err != nil {
		return nil, err
	}

	var watcher *fswatch.Watcher
	if cfg.Watch {
		watcher, err = fswatch.Start(fswatch.Config{
			Dir:    cfg.Root,
			Suffix: adapters.RecordSuffix,
			Clock:  cfg.Clock,
		})
		if err != nil {
			return nil, err
		}
	}

	clock := cfg.Clock
	if clock == nil {
		clock = obs.RealClock()
	}

	// The chain is built here and in one place, so the two stages a caller
	// must not be able to forget sit at the two ends of it.
	chain := make([]Factory, 0, len(cfg.Chain)+2)
	if cfg.Locator != nil {
		// Prepended, not appended: everything downstream may want to know
		// which pane it is looking at, and nothing upstream of the source can
		// tell it.
		chain = append(chain, PaneGate(cfg.Locator))
	}
	chain = append(chain, cfg.Chain...)
	// The write half, before the verdict and after everything that produces
	// the facts it acts on. See navigateStage.
	chain = append(chain, navigateStage(cfg))
	// The notifier, on the same terms: appended for the caller so it cannot be
	// forgotten, and silent unless act.Policy.Notify says otherwise. Before
	// the verdict because the verdict is last and always, and because
	// requirements.md R6's `notify.on` names Claude's states rather than
	// magmux's verdict. See notifyStage.
	chain = append(chain, notifyStage(cfg))
	// APPENDED LAST, AND ALWAYS. The verdict is the only stage that needs
	// every other stage's facts — the gate's pane, the registry's status, the
	// tool middleware's error count — so it cannot run anywhere but the end.
	// It is added unconditionally rather than left to the caller for the same
	// reason the pane gate is prepended for them: a document without an
	// attention verdict looks complete and answers the one question this layer
	// exists to answer with silence.
	chain = append(chain, verdictStage(cfg))

	p := &Pipeline{
		in:   make(chan pipeMessage, 4),
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
		hub:  StartHub(),
	}
	co := &coordinator{
		pipe:    p,
		root:    cfg.Root,
		src:     src,
		watcher: watcher,
		chain:   chain,
		clock:   clock,
		actors:  map[string]*Actor{},
		// Always built, never nil. A pipeline whose config said nothing about
		// policy or audit still has a gate — one that refuses everything —
		// because the alternative is a nil check at the call site, and a nil
		// check is a branch somebody can get the wrong way round. The refusal
		// comes with a reason either way.
		gate: act.NewGate(act.GateConfig{
			Policy: cfg.Policy,
			Audit:  cfg.Audit,
			Now:    cfg.Now,
		}),
		audit:      cfg.Audit,
		notifyPort: cfg.Notify,
	}
	go co.loop()
	return p, nil
}

// Subscribe registers a sink. See [Hub.Subscribe].
func (p *Pipeline) Subscribe(name string, buffer int) (*Subscription, error) {
	return p.hub.Subscribe(name, buffer)
}

// Unsubscribe removes a sink. See [Hub.Unsubscribe].
func (p *Pipeline) Unsubscribe(sub *Subscription) { p.hub.Unsubscribe(sub) }

// Refresh asks for a cycle now. It does not wait for one: the result arrives
// on every subscription, which is the only place a document is ever delivered.
//
// It reports whether the request was accepted; false means the pipeline has
// stopped.
func (p *Pipeline) Refresh() bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.in <- pipeMessage{kind: pipeRefresh}:
		return true
	case <-p.done:
		return false
	}
}

// Do performs ONE action against ONE session, and is the only way into the
// write half from outside this package.
//
// # It is a command with a reply channel, and it runs on the coordinator
//
// The coordinator owns the actor table, the policy and the audit trail, so the
// whole gated sequence — decide, record, act, record — happens on its
// goroutine and needs no lock to be serialised with the cycles around it. The
// caller blocks here until the action is finished, because an action has an
// outcome and a caller that cannot see it cannot tell a refusal from a
// success.
//
// The cost, stated rather than discovered: while an action runs, the
// coordinator is not servicing events, so a stage that blocks holds up the
// next cycle. It is bounded today because the only live verb focuses a pane
// in this process; act.Actuator's contract — may do I/O, may not block
// indefinitely — is what keeps it bounded when a slower verb arrives, and the
// context passed here is the caller's lever on one that misbehaves. The
// coordinator already does filesystem I/O on this same goroutine once per
// cycle, so this is the existing shape rather than a new one.
//
// # What comes back
//
// A report carrying the result, the policy decision and every audit line
// written for this request, and an error that is non-nil for EVERY outcome
// that is not "it happened". A refusal is an error: a caller that asked for
// something and got a nil error and an empty result cannot tell that from
// success. Use errors.Is against the act package's sentinels to tell the
// classes apart — act.ErrDeferred, act.ErrNotCapable, act.ErrForbidden,
// act.ErrNeedsConfirmation.
//
// A session this pipeline has never seen is refused as act.ErrNotCapable:
// there is no actor, so nothing declares anything for it. That refusal is
// recorded like any other.
func (p *Pipeline) Do(ctx context.Context, req act.Request) (act.Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reply := make(chan actOutcome, 1)
	select {
	case <-p.done:
		return act.Report{}, fmt.Errorf("pipeline: the observation pipeline has stopped")
	default:
	}
	select {
	case p.in <- pipeMessage{kind: pipeAct, req: req, ctx: ctx, actReply: reply}:
	case <-p.done:
		return act.Report{}, fmt.Errorf("pipeline: the observation pipeline has stopped")
	case <-ctx.Done():
		return act.Report{}, ctx.Err()
	}
	select {
	case out := <-reply:
		return out.report, out.err
	case <-p.done:
		return act.Report{}, fmt.Errorf("pipeline: the observation pipeline stopped before the action was decided")
	}
}

// Stats reports what the pipeline and its hub have done.
func (p *Pipeline) Stats() Stats {
	reply := make(chan Stats, 1)
	select {
	case <-p.done:
		return Stats{}
	default:
	}
	select {
	case p.in <- pipeMessage{kind: pipeStats, reply: reply}:
	case <-p.done:
		return Stats{}
	}
	select {
	case s := <-reply:
		return s
	case <-p.done:
		return Stats{}
	}
}

// Stop shuts everything down in order — watcher, then every actor, then the
// hub — and returns only once every goroutine it owns has finished and every
// subscription has been closed.
//
// That ordering is the shutdown barrier. The watcher goes first so no new
// cycle can start; the actors next, so nothing is mid-chain; the hub last, so
// the close a sink is waiting for arrives after the final document rather than
// beside it. A sink that ranges over its subscription is therefore finished
// with the last document it will ever get by the time this returns, which is
// what stops a sink being mid-write when the process exits.
//
// It is idempotent and may be called from any goroutine.
func (p *Pipeline) Stop() {
	select {
	case p.in <- pipeMessage{kind: pipeStop}:
	case <-p.done:
	}
	<-p.done
}

// coordinator is the pipeline's goroutine-owned state. It is a separate type
// from Pipeline so that the split is structural rather than a comment: the
// fields here are reachable only from loop, and the fields on Pipeline are the
// channels a caller uses.
type coordinator struct {
	pipe    *Pipeline
	root    string
	src     *registry.Source
	watcher *fswatch.Watcher
	chain   []Factory
	// clock is handed to every actor, for the one-shot staleness alarm. It is
	// never read here: this goroutine does not wait on a clock at all, and a
	// cycle happens because a record changed, because an alarm came due, or
	// because somebody asked.
	clock  obs.Clock
	actors map[string]*Actor
	// gate is the policy and the audit trail. It is owned by this goroutine
	// and by nothing else, which is what lets it hold a sequence counter
	// without a lock.
	gate *act.Gate
	// audit is the SAME trail the gate holds, reached directly for the one
	// kind of line that is not an action: a notification. It is one Auditor
	// and not two, deliberately — act.Writer stamps (pid, seq) and a second
	// writer in this process would restart the counter, so two lines would
	// claim to be seq 1 of the same pid. See act.Writer on single ownership.
	audit act.Auditor
	// notifyPort delivers a decided notification. nil means nothing here can.
	notifyPort notify.Port
	// pending carries problems that did not come from a read — watcher
	// errors — onto the next document, so that everything a sink needs
	// arrives through one channel.
	pending []registry.Problem
	stats   Stats
}

func (c *coordinator) loop() {
	defer close(c.pipe.done)
	defer c.shutdown()

	var (
		events <-chan fswatch.Event
		errs   <-chan error
	)
	if c.watcher != nil {
		events = c.watcher.Events()
		errs = c.watcher.Errors()
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// A closed channel would otherwise be ready forever and spin
				// this loop. A nil channel blocks in a select, which is the
				// behaviour wanted: the watcher is gone, the commands are not.
				events = nil
				continue
			}
			c.stats.WatchDropped += ev.Dropped
			// One cycle per burst. The watcher has already coalesced a
			// window; this drains whatever else is waiting so that four
			// records changing at once is one document and not four.
			for drained := true; drained; {
				select {
				case more, ok := <-events:
					if !ok {
						events, drained = nil, false
						continue
					}
					c.stats.WatchDropped += more.Dropped
				default:
					drained = false
				}
			}
			c.cycle()

		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			c.stats.WatchErrors++
			c.pending = append(c.pending, registry.Problem{
				Path:   c.root,
				Kind:   registry.ProblemUnreadable,
				Detail: fmt.Sprintf("watching for changes: %v", err),
			})
			// Published immediately rather than held until something else
			// happens: a watcher that has gone quiet with an error nobody was
			// told about is indistinguishable from a quiet machine, and the
			// whole point of the error channel is that it is not silent.
			c.cycle()

		case <-c.pipe.wake:
			// An actor's staleness alarm came due. This is the only cycle in
			// the whole design that nothing on disk asked for, and it is what
			// makes a pane going SILENT observable at all: silence produces no
			// event, so the record write that would otherwise wake this loop
			// is exactly the thing a wedged session is not doing.
			c.stats.Wakes++
			c.cycle()

		case m := <-c.pipe.in:
			switch m.kind {
			case pipeRefresh:
				c.cycle()
			case pipeStats:
				s := c.stats
				s.Actors = len(c.actors)
				s.Subscribers = c.pipe.hub.Stats()
				m.reply <- s
			case pipeAct:
				rep, err := c.do(m.ctx, m.req)
				m.actReply <- actOutcome{report: rep, err: err}
			case pipeStop:
				return
			}
		}
	}
}

// cycle is one read, one route, one publish.
func (c *coordinator) cycle() {
	fleet, problems := c.src.Read()
	c.stats.Cycles++

	// Route every session to its own actor, starting one for a session seen
	// for the first time. This loop does not wait for any chain to run: the
	// point of an actor per session is that the fifth session's filesystem
	// work overlaps the first's.
	live := make(map[string]bool, len(fleet.Sessions))
	for _, s := range fleet.Sessions {
		key := sessionKey(s)
		if live[key] {
			continue
		}
		live[key] = true
		a := c.actors[key]
		if a == nil {
			var err error
			a, err = StartActorWith(key, c.chain, ActorConfig{Clock: c.clock, Wake: c.wakeFunc()})
			if err != nil {
				// The only error is an empty id, and sessionKey never returns
				// one. Recorded as a problem rather than dropped, because a
				// session absent for a reason nobody stated is the failure
				// this whole layer is written against.
				problems = append(problems, registry.Problem{
					Path:   key,
					Kind:   registry.ProblemUnreadable,
					Detail: fmt.Sprintf("no actor for this session: %v", err),
				})
				continue
			}
			c.actors[key] = a
			c.stats.ActorsStarted++
		}
		if !a.Observe(s) {
			c.stats.ObservationsDropped++
		}
	}

	// Retire the actors of sessions that are gone. This is the only place an
	// actor is stopped outside shutdown, and it is where a goroutine leak
	// would live: a session that vanishes and leaves its goroutine behind
	// costs one per session forever on a machine that starts and finishes
	// agents all day.
	for key, a := range c.actors {
		if live[key] {
			continue
		}
		a.Stop()
		delete(c.actors, key)
		c.stats.ActorsStopped++
	}

	// Collect what each chain made of its session. Every observation above is
	// already in flight, so this is where the actors' work is joined —
	// and because events and commands share one channel per actor, the answer
	// is guaranteed to be about the observation just sent.
	out := make([]obs.Session, 0, len(fleet.Sessions))
	// Notifications decided during this cycle, collected with the fact sets
	// and delivered AFTER the document is published: publishing is what every
	// sink is waiting for, and it must not queue behind a fork/exec.
	//
	// Each note is paired with the PUBLISHED fact set of its session rather
	// than carrying only its own. The notify stage sits before the verdict —
	// the verdict is last and always — so the session it decided from has no
	// attention verdict and no confidence tier yet, and an audit record built
	// from it would say `attentionNeeded: null` about a session the document
	// published the same instant as needing a human. The pairing is what makes
	// a notify record argue against a document somebody could actually have
	// been looking at, which is the argument coordinator.do already makes for
	// an action.
	var notes []pendingNote
	taken := make(map[string]bool, len(fleet.Sessions))
	for _, s := range fleet.Sessions {
		key := sessionKey(s)
		if taken[key] {
			// Two records claiming one identity. The second is published
			// exactly as it was read, because asking the first one's actor
			// would answer with the first one's facts and silently replace a
			// session with a copy of its namesake.
			c.stats.DuplicateSessions++
			out = append(out, s)
			continue
		}
		taken[key] = true
		a := c.actors[key]
		if a == nil {
			out = append(out, s)
			continue
		}
		frozen, pending, ok := a.collect()
		published := frozen
		if !ok || frozen.ID == "" && frozen.PID == 0 {
			// A stopped actor, or one whose observation was dropped before it
			// ever ran a chain. The raw session is still a session.
			published = s
		}
		for _, n := range pending {
			notes = append(notes, pendingNote{note: n, published: published})
		}
		out = append(out, published)
	}

	doc := obs.Fleet{Epoch: fleet.Epoch, Sessions: out}
	doc.Sort()

	if len(c.pending) > 0 {
		problems = append(problems, c.pending...)
		c.pending = nil
	}
	if c.pipe.hub.Publish(doc, problems) {
		c.stats.Published++
	}

	// After the publish, deliberately. A notification is a fork/exec and the
	// document is what every sink is waiting for; delivering first would put
	// the panel a whole notification behind the registry for no reason. Any
	// problem raised in here rides out on the NEXT document, which is the same
	// treatment a watcher error gets and for the same reason.
	for _, n := range notes {
		c.announce(n.note, n.published)
	}
}

// do runs one action through the gate, on the coordinator's goroutine.
//
// It resolves two things and decides nothing: WHICH session's fact set the
// policy is evaluated against, and WHAT can act on it. The gate does the rest,
// in the order its own documentation numbers, and every path through it —
// refusal included — writes an audit record.
//
// A session with no actor yields a nil actuator and a zero fact set. That is
// the honest description of a session this pipeline has never observed: there
// is nothing that declares a capability for it, so the gate refuses with
// act.ErrNotCapable and records the refusal against the id that was asked
// for.
func (c *coordinator) do(ctx context.Context, req act.Request) (act.Report, error) {
	c.stats.Actions++

	var (
		before obs.Session
		target act.Actuator
	)
	if a := c.actors[req.Action.Session]; a != nil {
		// The fact set the gate decides against is the frozen one from the
		// last completed chain run — the same value every sink was given, so
		// a decision can be argued about against a document somebody actually
		// saw.
		if s, ok := a.Snapshot(); ok {
			before = s
		}
		target = actorActuator{a: a}
	}

	rep, err := c.gate.Do(ctx, req, before, target)
	if err != nil {
		c.stats.ActionsRefused++
	}
	return rep, err
}

// actorActuator is one actor seen as an act.Actuator: both halves are commands
// with reply channels, so the gate reaches a session's chain the only way
// anything reaches it.
//
// It exists so that the gate can be written against an interface and tested
// without a pipeline, and so that the actor's own capabilities and perform
// commands can stay unexported — which is what makes "the only way to an
// actuator is through the gate" a property of the package rather than a
// convention.
type actorActuator struct{ a *Actor }

// Capabilities asks the chain what it can do. A stopped actor declares
// nothing, which the gate reports as act.ErrNotCapable — true, and the same
// answer a session with no write half gets.
func (x actorActuator) Capabilities() []act.Capability {
	caps, _ := x.a.capabilities()
	return caps
}

// Do runs the action on the actor's goroutine.
func (x actorActuator) Do(ctx context.Context, a act.Action) (act.Result, error) {
	return x.a.perform(ctx, a)
}

// wakeFunc is what an actor calls when its one-shot alarm comes due.
//
// It runs on the ACTOR's goroutine and never blocks, which is the whole point:
// the coordinator routinely waits on an actor's reply, and an actor that
// blocked here while the coordinator waited on it would deadlock the pair. The
// send is therefore dropped when a wake is already pending, which costs
// nothing — a pending wake already means a cycle is coming, and a second one
// would ask for the same cycle.
//
// The channel is captured rather than reached through c, so nothing on an
// actor's goroutine touches coordinator-owned state.
func (c *coordinator) wakeFunc() func() {
	wake := c.pipe.wake
	return func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// shutdown runs on the coordinator's goroutine, on the way out. The order is
// the barrier; see [Pipeline.Stop].
func (c *coordinator) shutdown() {
	if c.watcher != nil {
		c.watcher.Stop()
	}
	for key, a := range c.actors {
		a.Stop()
		delete(c.actors, key)
		c.stats.ActorsStopped++
	}
	c.pipe.hub.Stop()
}

// sessionKey is an actor's identity.
//
// The tool's own session id first, because it is the only identifier that
// survives a restart of anything magmux owns, and an actor keyed by it follows
// its session across one. A record with no id falls back to its pid, prefixed
// so the two namespaces cannot collide: a session whose id happens to be "412"
// is not the session running as pid 412.
func sessionKey(s obs.Session) string {
	if s.ID != "" {
		return s.ID
	}
	return "pid:" + strconv.Itoa(s.PID)
}
