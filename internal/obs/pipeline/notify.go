package pipeline

import (
	"context"
	"fmt"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/internal/obs/registry"
)

// Announcing is the OPTIONAL half of [Middleware] that produces NOTIFICATIONS
// — the third optional interface beside [Waker] and [Actuating], found the
// same way and for the same reason.
//
// It exists because a notification is neither a fact nor an action. A fact
// goes on the wire and is published to every sink; an action is requested from
// outside and goes through the gate. This is a stage saying "a human should be
// told about this session, and here is the clause that says so" — decided per
// session, on the actor's goroutine, and carried out to the coordinator
// because that is the goroutine that owns the audit trail and the port.
type Announcing interface {
	Middleware

	// Pending hands over what this stage has decided since the last call, and
	// forgets it. Draining is what makes the hand-off exactly-once.
	//
	// It is called on the actor's goroutine, must not block, and must not do
	// I/O: the caller is the coordinator, blocked on the reply.
	Pending() []notify.Note
}

// notifyStage is the factory for the stage that decides notifications.
//
// # Appended by this package rather than remembered by a caller
//
// The same rule as the pane gate, the navigator and the verdict. It sits
// BEFORE the verdict, which is documented as last and always, and it needs
// nothing the verdict produces — see notify.Stage.trigger, where reading the
// registry's status rather than magmux's own attention verdict is a decision
// with a reason and not an accident of ordering.
//
// # It is added whether or not there is a port
//
// A nil [Config.Notify] does not remove the stage; the zero act.Notify inside
// [Config.Policy] is what silences it, and that is the switch the USER holds.
// The port's absence is checked where the note would be delivered, so a
// process with no way to reach a human still decides — and a test can watch it
// decide without a notification centre.
func notifyStage(cfg Config) Factory {
	return func(sessionID string) Middleware {
		return notify.New(notify.Config{Policy: cfg.Policy, Now: cfg.Now})
	}
}

// pendingNote is one decided notification and the fact set its session was
// PUBLISHED with on the same cycle. See coordinator.cycle for why the two
// travel together rather than the note carrying only its own copy.
type pendingNote struct {
	note      notify.Note
	published obs.Session
}

// announcersIn picks out the stages that can raise a notification, once, on
// the goroutine that starts the actor.
func announcersIn(chain []Middleware) []Announcing {
	var out []Announcing
	for _, mw := range chain {
		if a, ok := mw.(Announcing); ok {
			out = append(out, a)
		}
	}
	return out
}

// announce records ONE notification and, if it was allowed and can be
// recorded, delivers it. It runs on the coordinator's goroutine.
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
// actions, taken the same way. The extra wrinkle here is that the stage has
// already marked the episode announced, so the session gets no second attempt
// while it stays in that state. That is deliberate: an unwritable trail is a
// standing condition rather than a transient one, and un-latching would turn
// one broken directory into a retry on every cycle — a notification storm
// caused by the failure to record a notification. The failure rides out to
// every sink as a [registry.Problem] instead, which is where a silence that
// nobody was told about becomes visible.
//
// # A notify line and an action line are told apart by their verb
//
// `tier` is null, because a notification belongs to no action tier: it changes
// nothing on any session and is aimed at the HUMAN. `requestedBy` is null,
// because nothing asked — the policy did.
//
// On every path reachable from outside this process, neither can appear on an
// ACTION line: Gate.Do refuses an unnamed requester at step 1 and an unknown
// tier matches no capability. TestANotifyRecordIsTellableFromAnActionRecord
// pins that. ONE in-process path collides, and act.VerbNotify's doc states it in
// full: Gate.Do called with the reserved verb, no surface and no tier writes a
// refusal wearing this exact shape. It needs a programming error in magmux to
// reach, the two lines then differ only in the prose of `policy.rule`, and
// TestAReservedVerbWithNoSurfaceWearsANotificationsShape pins the limitation so
// that closing it trips a test.
func (c *coordinator) announce(n notify.Note, published obs.Session) {
	target := act.Target{
		SessionID: published.ID,
		PID:       published.PID,
		Name:      published.Name,
		PaneID:    published.PaneID,
	}
	before := act.StateOf(published, n.At)
	base := act.Record{
		Verb:   act.VerbNotify,
		Target: target,
		Policy: n.Decision,
		Before: &before,
	}

	if c.audit == nil {
		// No trail at all. The same answer as a failed write and for the same
		// reason, said once here so the two paths cannot drift.
		c.notifyUnrecorded(n, fmt.Errorf("there is no audit trail, and a notification that "+
			"cannot be recorded is not sent"))
		return
	}

	if !n.Decision.Allowed {
		c.stats.NotificationsSuppressed++
		rec := base
		rec.Event = act.EventRefused
		rec.Outcome = &act.Outcome{Refused: true, Error: n.Decision.Rule}
		if _, err := c.audit.Append(rec); err != nil {
			c.problem(fmt.Errorf("recording a suppressed notification for session %s: %w",
				n.Session.ID, err))
		}
		return
	}

	attempt := base
	attempt.Event = act.EventAttempt
	written, err := c.audit.Append(attempt)
	if err != nil {
		c.notifyUnrecorded(n, err)
		return
	}

	if c.notifyPort == nil {
		// Decided, recorded, and nothing here can deliver it — which is the
		// honest state of a magmux built without a port, and of every one on a
		// platform with no notification centre. It is an OUTCOME rather than a
		// silence: an attempt with no outcome beside it means "we do not know
		// what happened", and we know exactly what happened.
		c.finishNotify(written, base, false, "", "nothing in this process can deliver a notification")
		return
	}
	c.stats.Notifications++
	sendErr := c.notifyPort.Send(context.Background(), n)
	if sendErr != nil {
		c.finishNotify(written, base, false, "", sendErr.Error())
		return
	}
	c.finishNotify(written, base, true, fmt.Sprintf("notified: %s — %s", n.Title(), n.Body()), "")
}

// finishNotify writes the outcome line that refers back to the attempt.
func (c *coordinator) finishNotify(attempt act.Record, base act.Record, done bool, detail, errText string) {
	rec := base
	rec.Event = act.EventOutcome
	rec.Ref = attempt.Seq
	// Omitted on an outcome line: the attempt beside it already carries it,
	// exactly as Gate.Do does it.
	rec.Before = nil
	rec.Outcome = &act.Outcome{Done: done, Detail: detail, Error: errText}
	if _, err := c.audit.Append(rec); err != nil {
		c.problem(fmt.Errorf("recording the outcome of a notification for session %s: %w",
			base.Target.SessionID, err))
	}
}

// notifyUnrecorded is the path where the trail could not take the attempt, so
// nothing was sent.
func (c *coordinator) notifyUnrecorded(n notify.Note, err error) {
	c.stats.NotificationsUnrecorded++
	c.problem(fmt.Errorf("a notification for session %s was NOT sent because it could not be "+
		"recorded: %w", n.Session.ID, err))
}

// problem queues something that did not come from a read onto the next
// document, which is the one channel everything a sink needs arrives through.
func (c *coordinator) problem(err error) {
	c.pending = append(c.pending, registry.Problem{
		Path:   c.root,
		Kind:   registry.ProblemUnreadable,
		Detail: err.Error(),
	})
}
