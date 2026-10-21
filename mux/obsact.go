package mux

// magmux's half of the observation layer's WRITE path, on the claudestate
// pipeline: the action gate and its audit trail, the notification announcer,
// and the one bridge from an action to the navigate stage on a session's
// actor.
//
// # Single owner, by construction
//
// act.Gate and act.Writer are single-owner values with no lock: the writer
// holds a sequence counter and its doc says one goroutine owns it. The library
// runs host code in exactly two places — Pipeline.Exec's fn and
// Config.OnEffects — and BOTH run on the pipeline's coordinator goroutine
// (claudestate architecture §3.2, ADR-X2). Everything in obsActions is touched
// only from those two, so the gate and the writer keep exactly one owner, as
// they had when the pipeline was magmux's own. The library knows nothing of
// act.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/navigate"
	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/obsview"
)

// obsActionStats is what the write path has done. The counters left
// pipeline.Stats when the pipeline moved into the library, and live here, on
// the coordinator, read through Exec so they too have one owner.
type obsActionStats struct {
	Actions, ActionsRefused int
	// Notifications were delivered to the port; Suppressed were decided and
	// refused by the policy (quiet hours); Unrecorded could not be written to
	// the trail and so were NOT sent.
	Notifications, NotificationsSuppressed, NotificationsUnrecorded int
}

// obsActions is the gate, the trail and the notification port, owned by the
// coordinator goroutine (see the file comment).
type obsActions struct {
	gate  *act.Gate
	audit act.Auditor
	port  notify.Port
	stats obsActionStats
	// batchBudget bounds ONE OnEffects call, all of its notes together; 0
	// takes notifyBatchBudget.
	batchBudget time.Duration
}

// newObsActions builds the write path. now is the clock the gate renders a
// `before` state's time in status against — the pipeline's own epoch clock,
// as it was when the gate lived in the pipeline; nil takes time.Now.
func newObsActions(policy act.Policy, audit act.Auditor, port notify.Port, now func() time.Time) *obsActions {
	return &obsActions{
		gate:  act.NewGate(act.GateConfig{Policy: policy, Audit: audit, Now: now}),
		audit: audit,
		port:  port,
	}
}

// errNotObserving is every action's answer when no pipeline is running.
var errNotObserving = errors.New("session observation is not running")

// do runs one action through the gate, on the coordinator, against the session
// whose key resolve picks from the last published document.
//
// resolve sees the document and returns the ONE key the request is about, or
// an error that refuses the request before the gate — an ambiguous target is
// one (D12): acting on the wrong session of two is the failure this layer
// exists to avoid. A key the document does not hold is NOT such an error: the
// gate is asked anyway, with no actuator, so the refusal is recorded like
// every other.
func (a *obsActions) do(ctx context.Context, p *pipeline.Pipeline, req act.Request,
	resolve func(model.Fleet) (model.Key, error)) (act.Report, error) {
	if p == nil {
		return act.Report{}, errNotObserving
	}
	var (
		rep act.Report
		err error
	)
	execErr := p.Exec(ctx, func(ctx context.Context, x pipeline.Ops) error {
		doc, _ := x.Latest()
		key, rerr := resolve(doc)
		if rerr != nil {
			return rerr
		}
		s, ok := x.Session(key)
		var target act.Actuator
		if ok {
			target = &actuatorOver{ctx: ctx, ops: x, key: key}
		}
		a.stats.Actions++
		rep, err = a.gate.Do(ctx, req, s, target)
		if err != nil {
			a.stats.ActionsRefused++
		}
		return nil
	})
	if execErr != nil {
		return act.Report{}, execErr
	}
	return rep, err
}

// statsOf reads the counters on the coordinator.
func (a *obsActions) statsOf(ctx context.Context, p *pipeline.Pipeline) (obsActionStats, error) {
	var st obsActionStats
	err := p.Exec(ctx, func(context.Context, pipeline.Ops) error {
		st = a.stats
		return nil
	})
	return st, err
}

// keyByID resolves a session id to the one session that carries it.
//
// Two sessions on one id — two interactive processes, a job and its resume —
// are refused, naming every pid and every job short (D12): "the session
// 6c5b2f09" names nobody when two processes answer to it. None is not an
// error here; see do.
func keyByID(id string) func(model.Fleet) (model.Key, error) {
	return func(doc model.Fleet) (model.Key, error) {
		var hits []model.Session
		for _, s := range doc.Sessions {
			if s.Key.SessionID == id {
				hits = append(hits, s)
			}
		}
		switch len(hits) {
		case 0:
			return model.Key{SessionID: id}, nil
		case 1:
			return hits[0].Key, nil
		}
		return model.Key{}, ambiguityError("session id", id, hits)
	}
}

// exactKey is the resolver for a key named on an earlier read: that key and
// nothing else, never a re-resolution by id or by name.
func exactKey(k model.Key) func(model.Fleet) (model.Key, error) {
	return func(model.Fleet) (model.Key, error) { return k, nil }
}

// ambiguityError names every candidate: its id, its pid, and its job short
// when it has one, so a person can say which one they meant.
func ambiguityError(kind, target string, hits []model.Session) error {
	var names []string
	for _, s := range hits {
		names = append(names, sessionName(s))
	}
	return fmt.Errorf("%s %q matches %d sessions — %s; name one exactly",
		kind, target, len(hits), strings.Join(names, ", "))
}

// sessionName is how a refusal names one session.
func sessionName(s model.Session) string {
	n := fmt.Sprintf("%s (pid %d", s.Key.SessionID, s.PID)
	if s.Key.Job != "" {
		n += ", job " + s.Key.Job
	}
	return n + ")"
}

// actuatorOver is act.Actuator for ONE session, reached through the
// pipeline's Ops.Call: it runs on that session's actor goroutine, finds the
// stage named navigate.Name, and asks it. The host never sees the chain
// itself (claudestate architecture §3.2).
type actuatorOver struct {
	ctx context.Context
	ops pipeline.Ops
	key model.Key
}

// errNoNavigator is a chain built without the navigate stage: the honest
// answer is "nothing here can do that", which Capabilities says by declaring
// nothing.
var errNoNavigator = errors.New("this session's chain has no navigate stage")

func (x *actuatorOver) navigator(fn func(*navigate.Stage) error) error {
	return x.ops.Call(x.ctx, x.key, func(_ model.Session, find func(string) (stage.Stage, bool)) error {
		st, ok := find(navigate.Name)
		if !ok {
			return errNoNavigator
		}
		nav, ok := st.(*navigate.Stage)
		if !ok {
			return fmt.Errorf("the stage named %q is a %T, not magmux's navigator", navigate.Name, st)
		}
		return fn(nav)
	})
}

// Capabilities is the navigate stage's, as of its last observation. An actor
// that retired between the read and the action declares nothing — "cannot",
// recorded as such — rather than an error the gate has no slot for.
func (x *actuatorOver) Capabilities() []act.Capability {
	var caps []act.Capability
	if err := x.navigator(func(nav *navigate.Stage) error {
		caps = nav.Capabilities()
		return nil
	}); err != nil {
		return nil
	}
	return caps
}

// Do performs the action on the session's actor.
func (x *actuatorOver) Do(ctx context.Context, a act.Action) (act.Result, error) {
	var (
		res  act.Result
		derr error
	)
	if err := x.navigator(func(nav *navigate.Stage) error {
		res, derr = nav.Do(ctx, a)
		return nil
	}); err != nil {
		return act.Result{}, err
	}
	return res, derr
}

// ── notifications ───────────────────────────────────────────────────────────

// onEffects is magmux's announcer: pipeline.Config.OnEffects. It runs on the
// coordinator after the cycle's document was published, once per cycle that
// produced effects, and the problems it returns ride out on the NEXT delivery
// to every subscriber (C13) — the attention panel counts them and a streaming
// `magmux agents` prints them on stderr.
//
// Each effect is the notify stage's own Note value, so the type assertion
// below IS the hand-off (stage.Effect's contract). Anything else is a
// programming error in magmux and is returned as a problem, never ignored.
//
// The whole batch shares one deadline, notifyBatchBudget: see there.
func (a *obsActions) onEffects(ctx context.Context, b pipeline.EffectBatch) []model.Problem {
	ctx, cancel := context.WithTimeout(ctx, a.budget())
	defer cancel()
	var problems []model.Problem
	for _, item := range b.Items {
		n, ok := item.Effect.(notify.Note)
		if !ok {
			problems = append(problems, hostProblem(item.Key, fmt.Sprintf(
				"an effect of type %T (%q) reached magmux's announcer, which delivers only notifications",
				item.Effect, effectName(item.Effect))))
			continue
		}
		if p := a.announce(ctx, n, item.Published); p != nil {
			problems = append(problems, *p)
		}
	}
	return problems
}

func effectName(e stage.Effect) string {
	if e == nil {
		return ""
	}
	return e.EffectName()
}

// hostProblem is a problem the host raised about one session.
func hostProblem(k model.Key, detail string) model.Problem {
	key := k
	return model.Problem{Scope: model.ScopeHost, Kind: model.ProblemHost, Key: &key, Detail: detail}
}

// announce records ONE notification and, if it was allowed and can be
// recorded, delivers it. It returns the problem a sink must see, or nil.
//
// # The order is the gate's order, for the gate's reason
//
// Decide (already done, in the stage), RECORD, act, record. requirements.md
// R5.A asks the trail to say what was done, to which session, why and by which
// rule, and a notification answers all four — it is an outward-facing act, and
// the one magmux performs without anybody asking it to. So the attempt is
// written BEFORE the port is called, and if it cannot be written the
// notification DOES NOT HAPPEN.
//
// # Suppressing an unrecordable notification, and why the latch stays set
//
// That choice has a real cost — an unwritable state directory silences
// notifications — and it is the same trade act.ErrUnaudited already makes for
// actions, taken the same way. The stage has already marked the episode
// announced, so the session gets no second attempt while it stays in that
// state. That is deliberate: an unwritable trail is a standing condition, and
// un-latching would turn one broken directory into a retry on every cycle — a
// notification storm caused by the failure to record a notification. The
// failure rides out to every sink as a ScopeHost problem instead.
//
// # A notify line and an action line are told apart by their verb
//
// `tier` is null, because a notification belongs to no action tier: it changes
// nothing on any session and is aimed at the HUMAN. `requestedBy` is null,
// because nothing asked — the policy did. act.VerbNotify's doc states the one
// in-process collision in full, and TestAReservedVerbWithNoSurfaceWearsANotificationsShape
// pins it.
func (a *obsActions) announce(ctx context.Context, n notify.Note, published model.Session) *model.Problem {
	target := act.Target{SessionID: published.Key.SessionID, PID: published.PID, Name: published.Name}
	if id, ok := obsview.PaneID(published.Pane); ok {
		target.PaneID = &id
	}
	before := act.StateOf(published, n.At)
	base := act.Record{
		Verb:   act.VerbNotify,
		Target: target,
		Policy: n.Decision,
		Before: &before,
	}
	key := published.Key

	if a.audit == nil {
		// No trail at all. The same answer as a failed write and for the same
		// reason, said once here so the two paths cannot drift.
		return a.unrecorded(key, fmt.Errorf("there is no audit trail, and a notification that "+
			"cannot be recorded is not sent"))
	}

	if !n.Decision.Allowed {
		a.stats.NotificationsSuppressed++
		rec := base
		rec.Event = act.EventRefused
		rec.Outcome = &act.Outcome{Refused: true, Error: n.Decision.Rule}
		if _, err := a.audit.Append(rec); err != nil {
			p := hostProblem(key, fmt.Sprintf("recording a suppressed notification for session %s: %v",
				key.SessionID, err))
			return &p
		}
		return nil
	}

	attempt := base
	attempt.Event = act.EventAttempt
	written, err := a.audit.Append(attempt)
	if err != nil {
		return a.unrecorded(key, err)
	}

	if a.port == nil {
		// Decided, recorded, and nothing here can deliver it — the honest
		// state of a magmux built without a port. An OUTCOME rather than a
		// silence: an attempt with no outcome beside it means "we do not know
		// what happened", and we know exactly what happened.
		return a.finish(written, base, false, "", "nothing in this process can deliver a notification")
	}
	if ctx.Err() != nil {
		// The batch's budget went on earlier notes. Recorded as not sent, and
		// said out loud: a note silently dropped would be indistinguishable
		// from one nobody decided to send.
		why := fmt.Sprintf("not sent: this cycle's notifications spent their %s budget on earlier ones", a.budget())
		if p := a.finish(written, base, false, "", why); p != nil {
			return p
		}
		p := hostProblem(key, fmt.Sprintf("a notification for session %s was %s", key.SessionID, why))
		return &p
	}
	a.stats.Notifications++
	if sendErr := a.port.Send(ctx, n); sendErr != nil {
		return a.finish(written, base, false, "", sendErr.Error())
	}
	return a.finish(written, base, true, fmt.Sprintf("notified: %s — %s", n.Title(), n.Body()), "")
}

// finish writes the outcome line that refers back to the attempt.
func (a *obsActions) finish(attempt act.Record, base act.Record, done bool, detail, errText string) *model.Problem {
	rec := base
	rec.Event = act.EventOutcome
	rec.Ref = attempt.Seq
	// Omitted on an outcome line: the attempt beside it already carries it,
	// exactly as Gate.Do does it.
	rec.Before = nil
	rec.Outcome = &act.Outcome{Done: done, Detail: detail, Error: errText}
	if _, err := a.audit.Append(rec); err != nil {
		p := model.Problem{Scope: model.ScopeHost, Kind: model.ProblemHost,
			Detail: fmt.Sprintf("recording the outcome of a notification for session %s: %v", base.Target.SessionID, err)}
		return &p
	}
	return nil
}

// unrecorded is the path where the trail could not take the attempt, so
// nothing was sent.
func (a *obsActions) unrecorded(key model.Key, err error) *model.Problem {
	a.stats.NotificationsUnrecorded++
	p := hostProblem(key, fmt.Sprintf("a notification for session %s was NOT sent because it could not be "+
		"recorded: %v", key.SessionID, err))
	return &p
}

// notifyBatchBudget bounds one OnEffects call: every note of one cycle,
// together (review r1 #5). Each delivery is bounded by its port
// (notifyTimeout), but OnEffects runs on the coordinator, which serves every
// session's next cycle, every Exec and every Stats, and a burst of N notes
// cost N bounds back to back — at startup, or after a re-key, every waiting
// session's actor starts unlatched and fires at once. One port bound is the
// most the coordinator now waits on notifications in one cycle. A note the
// budget cuts off is still recorded, attempt then outcome "not sent", and
// rides out as a ScopeHost problem.
const notifyBatchBudget = notifyTimeout

func (a *obsActions) budget() time.Duration {
	if a.batchBudget > 0 {
		return a.batchBudget
	}
	return notifyBatchBudget
}
