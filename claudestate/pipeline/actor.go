package pipeline

import (
	"context"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
)

// actor is one session key's goroutine. It owns that session's chain and its
// frozen fact set, and no other goroutine touches either. Every message goes
// down ONE channel, so "prepare, grant, observe, now tell me" is FIFO: the
// answer is always about the observation just sent.
type actor struct {
	key  model.Key
	in   chan actorMsg
	done chan struct{}
}

type actorMsgKind int

const (
	msgPrepare actorMsgKind = iota
	msgObserve
	msgCollect
	msgCall
	msgStop
)

type actorMsg struct {
	kind    actorMsgKind
	session model.Session
	loc     model.Location
	grant   stage.Grant
	locator ports.PaneLocator
	call    func(model.Session, func(string) (stage.Stage, bool)) error
	reply   chan actorReply
}

type actorReply struct {
	targets  []model.WatchTarget
	session  model.Session
	effects  []stage.Effect
	problems []model.Problem
	err      error
}

// problemReporter is the library-internal seam through which a stage hands
// the coordinator what it could not do (claudecode's NotGranted, an
// implausible instant). Host stages have no such channel: their problems go
// through OnEffects.
type problemReporter interface{ Problems() []model.Problem }

// cycleLocated is the pane gate's seam: the cycle's immutable locator travels
// with every observation (§3.1 step 8).
type cycleLocated interface{ useLocator(ports.PaneLocator) }

// actorBacklog lets the coordinator send prepare/observe/collect to every
// actor before waiting on any of them.
const actorBacklog = 8

func startActor(key model.Key, factories []stage.Factory, clock ports.Clock, wake func()) *actor {
	a := &actor{key: key, in: make(chan actorMsg, actorBacklog), done: make(chan struct{})}
	chain := make([]stage.Stage, 0, len(factories))
	for _, f := range factories {
		if f == nil {
			continue
		}
		if st := f(key); st != nil {
			chain = append(chain, st)
		}
	}
	go a.loop(chain, clock, wake)
	return a
}

func (a *actor) send(m actorMsg) bool {
	select {
	case a.in <- m:
		return true
	case <-a.done:
		return false
	}
}

func (a *actor) await(r chan actorReply) (actorReply, bool) {
	select {
	case v := <-r:
		return v, true
	case <-a.done:
		return actorReply{}, false
	}
}

func (a *actor) stop() {
	select {
	case a.in <- actorMsg{kind: msgStop}:
	case <-a.done:
	}
	<-a.done
}

func (a *actor) loop(chain []stage.Stage, clock ports.Clock, wake func()) {
	defer close(a.done)
	var (
		frozen   model.Session
		effects  []stage.Effect
		problems []model.Problem
		timer    ports.Timer
		timerC   <-chan time.Time
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer, timerC = nil, nil
		}
	}
	defer stopTimer()

	// arm replaces the alarm with ONE for the earliest Waker answer, or none.
	arm := func() {
		stopTimer()
		var next time.Duration
		armed := false
		for _, st := range chain {
			w, ok := st.(stage.Waker)
			if !ok {
				continue
			}
			d, ok := w.WakeIn()
			if !ok || d <= 0 {
				continue // a zero-delay alarm would be a poll loop
			}
			if !armed || d < next {
				next, armed = d, true
			}
		}
		if armed && clock != nil {
			timer = clock.NewTimer(next)
			timerC = timer.C()
		}
	}

	find := func(name string) (stage.Stage, bool) {
		for _, st := range chain {
			if st.Name() == name {
				return st, true
			}
		}
		return nil, false
	}

	for {
		select {
		case m := <-a.in:
			switch m.kind {
			case msgPrepare:
				var targets []model.WatchTarget
				for _, st := range chain {
					if t, ok := st.(stage.Targeting); ok {
						targets = append(targets, t.Targets(m.session, m.loc)...)
					}
				}
				m.reply <- actorReply{targets: targets}

			case msgObserve:
				for _, st := range chain {
					if t, ok := st.(stage.Targeting); ok {
						t.Granted(m.grant)
					}
					if c, ok := st.(cycleLocated); ok {
						c.useLocator(m.locator)
					}
				}
				s := m.session
				for _, st := range chain {
					out := st.Observe(s)
					if out.Key != s.Key {
						// C21: a stage cannot change a session's identity.
						k := s.Key
						problems = append(problems, model.Problem{Scope: model.ScopeHost, Kind: model.ProblemHost,
							Key: &k, Detail: "stage " + st.Name() + " returned the key " + out.Key.String() + "; its output was discarded"})
					} else {
						s = out
					}
					if e, ok := st.(stage.Effecting); ok {
						effects = append(effects, e.Effects()...)
					}
					if r, ok := st.(problemReporter); ok {
						problems = append(problems, r.Problems()...)
					}
				}
				frozen = s.Clone() // freeze point 1
				arm()

			case msgCollect:
				m.reply <- actorReply{session: frozen, effects: effects, problems: problems}
				effects, problems = nil, nil

			case msgCall:
				m.reply <- actorReply{err: m.call(frozen, find)}

			case msgStop:
				return
			}

		case <-timerC:
			// One-shot: the cycle this asks for decides whether to re-arm.
			timer, timerC = nil, nil
			if wake != nil {
				wake() // non-blocking, or the coordinator and this actor deadlock
			}
		}
	}
}

// paneGate is the library's pane stage, prepended whenever a Locator is set.
// It asks the CYCLE's locator, every cycle, and keeps nothing across cycles:
// a failing CycleLocator makes every answer Unknown, never the previous table.
type paneGate struct{ loc ports.PaneLocator }

func paneGateFactory() stage.Factory {
	return func(model.Key) stage.Stage { return &paneGate{} }
}

func (g *paneGate) Name() string                     { return "pane" }
func (g *paneGate) useLocator(loc ports.PaneLocator) { g.loc = loc }

func (g *paneGate) Observe(s model.Session) model.Session {
	if g.loc == nil {
		return s
	}
	ref, found := g.loc.PaneFor(s)
	if found != model.LivenessYes {
		return s
	}
	s.Pane = ref
	s.PaneAlive = g.loc.PaneAlive(ref)
	return s
}

// callOn runs fn on k's actor and waits, honouring ctx.
func callOn(ctx context.Context, a *actor, fn func(model.Session, func(string) (stage.Stage, bool)) error) error {
	r := make(chan actorReply, 1)
	select {
	case a.in <- actorMsg{kind: msgCall, call: fn, reply: r}:
	case <-a.done:
		return ErrNoSuchKey
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case v := <-r:
		return v.err
	case <-a.done:
		return ErrNoSuchKey
	case <-ctx.Done():
		return ctx.Err()
	}
}
