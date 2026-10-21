// Package notify decides when a session is worth interrupting a human for,
// and what to say. It does not deliver anything.
//
// requirements.md R4.4 is one sentence — "macOS notifications, filtered per
// R6" — and the sentence after it is the whole design: "30 sessions firing
// unfiltered would be unusable". Everything here is filtering.
//
// # Two things this is not, and both were named in the brief
//
// It is NOT A POLL. `delay = "2m"` means "tell me when a session has been
// waiting two minutes", and the obvious implementation checks every session on
// a timer. [Stage] implements claudestate's optional stage.Waker instead: when a
// session qualifies but has not waited long enough yet, it reports how long is
// left, its actor arms ONE one-shot alarm for exactly that, and the deadline
// produces one cycle. A session that has already been announced, or that will
// never qualify, arms nothing at all, so an idle machine holds no timers.
//
// It is NOT A REPEAT. A session that sits `waiting` for an hour is one
// notification, not thirty. What remembers that is [Stage.announced], and it
// is per-session state living in a per-session stage — the same rule as
// every other stage in the chain, and the reason this is a stage rather than a
// sink. A sink sees the whole fleet and would have to keep a map keyed by
// session id, which is a second lifetime to get wrong beside the actor table
// that already exists.
//
// # The episode, which is what "once" is counted per
//
// Not once per session and not once per process: once per EPISODE. An episode
// is one spell of one trigger, identified by the instant the session entered
// the status it is in — the EFFECTIVE model.Session.StatusSince, a record's own
// instant (the registry's statusUpdatedAt, or a job's updatedAt when the job
// decides), which is an ANSWER rather than a lower bound, because Claude Code
// writes the record on a transition and not on a heartbeat (D7). So a session that
// goes waiting, is answered, works, and goes waiting again is two episodes and
// two notifications, and a session nobody answers is one.
//
// # Where the decision stops and delivery begins
//
// Everything above happens here, on the actor's goroutine, with no I/O at all.
// What comes out is a [Note] carrying an act.Decision that says allowed or
// suppressed AND WHY, in the words of the notify clause that decided. The
// Note is a stage.Effect: the claudestate pipeline hands it, unwrapped, to
// magmux's OnEffects on the coordinator goroutine, which writes the audit
// record and calls the [Port], in that order, because a notification is an
// outward-facing act and requirements.md R5.A's trail is not optional for one
// — see mux's obsAnnouncer, where the consequence of an unwritable trail is
// stated.
//
// That split is the same one Gate.Do makes between deciding and acting, for
// the same reason: the thing that decides must not be the thing that does, and
// the record goes between them.
package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/claudestate/join"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// Port is how a notification reaches a human. One implementation lives in
// magmux (`osascript`, on macOS); a test supplies its own.
//
// # It runs on the COORDINATOR's goroutine and MUST NOT block indefinitely
//
// The same contract act.Actuator carries, with the same reason and no context
// deadline of its own to hide behind: while this runs, the pipeline is not
// servicing events, so a Send that never returns wedges every session's next
// cycle. The ctx carries cancellation and an implementation that waits on
// anything MUST select on it — and must ALSO bound itself, because the
// pipeline creates no timers and deliberately does not start one here.
//
// An error is reported into the audit trail as the outcome of the attempt.
// It is not retried: a notification that was late is worth less than one that
// was not sent, and a retry loop against a broken notification centre is a
// storm waiting for the day it comes back.
type Port interface {
	Send(ctx context.Context, n Note) error
}

// Note is one notification, decided but not yet delivered.
//
// It carries a CLONE of the session rather than a pointer into the chain's
// fact set. model.Session is mostly pointers and freeze point 1 replaces the
// whole fact set on the next observation; a note that shared those pointers
// would be describing whichever observation happened to be current when the
// coordinator got round to it, which is not the observation it was decided
// from.
type Note struct {
	// Session is what the note is about, frozen at the instant it was decided.
	Session model.Session

	// On is which of requirements.md R6's `notify.on` triggers raised it.
	On act.NotifyOn

	// For is how long the session had been in its current status when the note
	// was decided, or zero when nothing could measure that. It is the figure
	// `notify.delay` was compared against.
	For time.Duration

	// At is when the note was decided.
	At time.Time

	// Decision is allowed-or-not and, in Rule, the notify clause that decided.
	// It is the same type an action's audit record carries, so one trail holds
	// both kinds of line and a reader needs one schema.
	//
	// Stance is deliberately left unset, and marshals as null. A notification
	// is governed by R6's `[notify]` block and not by an `[actions]` stance;
	// filling in a stance would suggest one of the four tiers applies to it,
	// and none does.
	Decision act.Decision
}

// Title is the notification's headline: which session, in the words a person
// uses for it.
func (n Note) Title() string {
	name := n.Session.Name
	if name == "" {
		name = shortID(n.Session.Key.SessionID)
	}
	if name == "" {
		return fmt.Sprintf("Claude session %d", n.Session.PID)
	}
	return name
}

// Body is the sentence underneath it.
//
// It says WHAT and FOR HOW LONG, because the only question a person has when
// they see one of these is whether to get up now. The waiting reason is named
// when it is known — "input needed" and "permission prompt" are different
// errands — and is left out rather than guessed at when it is not.
func (n Note) Body() string {
	switch n.On {
	case act.NotifyOnError:
		count := 0
		if n.Session.Fault.APIErrors != nil {
			count = *n.Session.Fault.APIErrors
		}
		word := "errors"
		if count == 1 {
			word = "error"
		}
		return fmt.Sprintf("%d API %s", count, word)
	case act.NotifyOnWaiting:
		// Built from the enum's own word rather than from a sentence per
		// case, which is NFR6's rule turned into a habit: the two errands are
		// spelled "input" and "permission" on the wire, in `magmux agents`, and
		// here, so a person reading a notification and then the document sees
		// one vocabulary. A table of prose here would be a second one, and
		// phase 6 already caught a test writing Claude's own spelling of it.
		// Only the two errands magmux's wire has a word for: a dialog, or a
		// blocked background job, is "waiting for you", exactly as before the
		// library, when neither had a reason the wire could name.
		what := "waiting for you"
		switch n.Session.Waiting {
		case model.WaitingInput, model.WaitingPermission:
			what = "waiting for " + n.Session.Waiting.String()
		}
		if n.For > 0 {
			return fmt.Sprintf("%s for %s", what, roundDuration(n.For))
		}
		return what
	}
	return n.Decision.Rule
}

// shortID is the first segment of a UUID-shaped session id — enough to tell
// two sessions apart in a notification, where the whole thing would not fit.
func shortID(id string) string {
	if i := len(id); i > 8 {
		return id[:8]
	}
	return id
}

// roundDuration renders a duration the way a person would say it. A
// notification that says "waiting for 2m1.483s" is a notification written by a
// computer for a computer.
func roundDuration(d time.Duration) time.Duration {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute)
	case d >= time.Minute:
		return d.Round(time.Second)
	default:
		return d.Round(time.Second)
	}
}

// Config is how a [Stage] is built. A plain value, copied on the way in.
type Config struct {
	// Policy is the whole posture. Only [act.Policy.Notify] and
	// [act.Policy.Source] are read — the first to decide, the second to name
	// the authority in the audit record — and the whole value is taken rather
	// than those two fields so that a caller passes what it already has and
	// cannot pass a notify block from one policy with a source from another.
	//
	// THE ZERO VALUE IS SILENCE. act.Notify's zero value is Enabled false with
	// no triggers, so a stage built from a struct literal that says nothing
	// says nothing, which is the same safe-by-default rule the rest of the
	// layer follows.
	Policy act.Policy

	// Now is the clock the delay is measured against. nil takes time.Now.
	//
	// It is a func and not a ports.Clock because this stage never waits: it
	// reports how long is left and the actor owns the alarm.
	Now func() time.Time
}

// Stage is one session's notifier: a claudestate stage.Stage, a stage.Waker,
// and a stage.Effecting whose effects are the [Note]s magmux delivers.
//
// One instance per session, never shared. It remembers which episode it has
// announced, and a shared instance would answer one session's question with
// another session's memory — silently, and wrong in exactly the case the
// feature exists for.
type Stage struct {
	policy act.Policy
	now    func() time.Time

	// announced is the episode this stage has already produced a note for.
	// The zero value is "nothing yet".
	announced episode

	// pending is what the coordinator has not collected. It is a slice rather
	// than one note because a session can in principle cross two thresholds
	// between collections; in practice it holds zero or one.
	pending []Note

	// wake is how long until this stage's answer changes on its own, set by
	// the last Observe. It is the delay a session has not yet served.
	wake    time.Duration
	hasWake bool
}

// episode identifies one spell of one trigger. Two notes with the same episode
// are the same notification, however many cycles apart they were decided.
type episode struct {
	on act.NotifyOn
	// since is when the session entered the status it is in. The zero instant
	// is a real value here and means "the record did not say", which is why
	// `on` carries the "is there an episode at all" answer instead.
	since time.Time
}

func (e episode) live() bool { return e.on != act.NotifyOnUnknown }

// Factory builds one notifier per session actor. The zero policy inside cfg
// is silence, and that is the switch the USER holds: a process with no way to
// reach a human still decides, and the port's absence is recorded where the
// note would be delivered.
func Factory(cfg Config) stage.Factory {
	return func(model.Key) stage.Stage { return New(cfg) }
}

// New builds one session's notifier.
func New(cfg Config) *Stage {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Stage{policy: cfg.Policy, now: now}
}

// Name identifies this stage in diagnostics.
func (s *Stage) Name() string { return "notify" }

// Observe decides, and changes nothing about the session.
//
// It is a pure reader of the fact set: the notification is not a FACT about
// the session and does not belong on the wire. architecture-v4 §9 settles the
// document's keys and a `notified` field would be magmux reporting on its own
// behaviour in a document that is otherwise entirely about the session — the
// same line the control panel's wire-tap rule draws, in the other surface.
func (s *Stage) Observe(in model.Session) model.Session {
	s.wake, s.hasWake = 0, false

	on := s.trigger(in)
	if on == act.NotifyOnUnknown {
		// The session is not in a notifying state. The latch clears, so the
		// NEXT spell announces: this is what makes a session that goes
		// waiting, is answered, and goes waiting again into two episodes.
		s.announced = episode{}
		return in
	}

	now := s.now()
	ep := episode{on: on}
	if in.StatusSince != nil {
		ep.since = *in.StatusSince
	}
	if s.announced == ep {
		return in // already said, once, for this episode
	}

	var waited time.Duration
	measured := false
	if d := join.AgesAt(in, now).InStatus; d != nil {
		waited, measured = *d, true
	}
	delay := s.policy.Notify.Delay
	if delay > 0 {
		if !measured {
			// `delay` is a claim about elapsed time and nothing here can
			// measure any. Saying nothing is the honest answer; announcing
			// would be asserting the session has waited long enough on no
			// evidence at all, which is NFR4's wrong-answer-from-a-real-field.
			// No alarm either: nothing will change until the record does.
			return in
		}
		if waited < delay {
			// The one thing in this layer that is waiting for TIME rather
			// than for an event. One shot, for exactly the remainder.
			s.wake, s.hasWake = delay-waited, true
			return in
		}
	}
	if !measured {
		waited = 0
	}

	s.announced = ep
	s.pending = append(s.pending, Note{
		Session:  in.Clone(),
		On:       on,
		For:      waited,
		At:       now,
		Decision: s.decide(on, waited, now),
	})
	return in
}

// trigger is which of requirements.md R6's `notify.on` values this session is
// currently an instance of, or NotifyOnUnknown.
//
// # Why it reads the STATUS and not the attention verdict
//
// The attention verdict is magmux's own answer to "which session needs me" and
// is the better signal — but `notify.on` is a list the USER writes, in words
// the requirement gives them, and those words name Claude's states. A
// notification raised by the verdict would fire for reasons nobody can express
// in the config file, and could not be turned off by editing it. That is also
// why this stage sits before the verdict in the chain rather than after it:
// the verdict is documented as last and always, and this needs nothing it
// produces.
//
// Waiting beats error when both are true, because it is the one a human can do
// something about in the next thirty seconds.
//
// # A session either gate REJECTED never notifies
//
// requirements.md R1.3: Claude Code never deletes a record, and about HALF the
// records on the machine this was built for are stale. A record that says
// `waiting` and whose process exited three days ago is a session nobody can
// help, and a notification about it is precisely the noise R4.4 exists to
// prevent — worse than useless, because it trains a person to ignore the
// banner that matters.
//
// UNDETERMINED never rejects, which is the whole point of the tri-state: "we
// could not tell whether the process is alive" is not "it is dead", and a
// session dropped because nobody looked is the failure the model is built
// against. Only a DETERMINED no is a rejection.
//
// This was found by running the real binary against a staged registry of dead
// pids and watching it decide to notify about both of them.
//
// # The process gate applies only to a session no job claims
//
// For a session with a job record, ProcessAlive describes the process that
// SERVED the job, and the job's own record has already been weighed against it
// by the effective status (claudestate's twinStatus): a blocked job whose
// serving process exited is still blocked, and still needs a human. That is
// the scope verdict's Rule 1 has, for the same reason (§3.4's consumers
// table), and a notifier that dismissed what the attention verdict raises
// would contradict the panel beside it.
//
// # Waiting is WaitingApplies, so a blocked job announces
//
// The effective status is one vocabulary, and "needs a human" is
// Status.IsWaiting(): waiting, or a background job blocked (D6, D14). Reading
// only StatusWaiting would announce every interactive session and not one
// blocked job.
func (s *Stage) trigger(in model.Session) act.NotifyOn {
	if (in.Job == nil && in.ProcessAlive == model.LivenessNo) || in.PaneAlive == model.LivenessNo {
		return act.NotifyOnUnknown
	}
	if in.WaitingApplies() && s.policy.Notify.Fires(act.NotifyOnWaiting) {
		return act.NotifyOnWaiting
	}
	if in.Fault.APIErrors != nil && *in.Fault.APIErrors > 0 && s.policy.Notify.Fires(act.NotifyOnError) {
		return act.NotifyOnError
	}
	return act.NotifyOnUnknown
}

// decide resolves the quiet windows and writes the authority down.
//
// # A notification suppressed by `quiet` is SUPPRESSED, not deferred
//
// The alternative — hold it and deliver at 08:00 — is the failure R4.4 names
// in its own sentence, arriving all at once: a night's worth of sessions
// firing the moment quiet hours end is exactly "30 sessions firing
// unfiltered", moved to breakfast. The episode is marked announced either way,
// so a session that went waiting at 23:00 and is still waiting at 09:00
// produces one suppressed record and no notification at all. It is still in
// the document, in `magmux agents`, and in the panel; `quiet` means the machine
// does not interrupt, not that it stops knowing.
func (s *Stage) decide(on act.NotifyOn, waited time.Duration, now time.Time) act.Decision {
	src := s.policy.Source
	if src == "" {
		src = "an unnamed policy"
	}
	for _, q := range s.policy.Notify.Quiet {
		if !q.Contains(now) {
			continue
		}
		return act.Decision{
			Rule: fmt.Sprintf("%s: notify.quiet %s-%s covers %s, so nothing is announced",
				src, q.From, q.To, now.Format("15:04")),
		}
	}
	rule := fmt.Sprintf("%s: notify.on contains %q", src, on)
	if d := s.policy.Notify.Delay; d > 0 {
		rule += fmt.Sprintf(", and it has been %s (notify.delay = %s)", roundDuration(waited), d)
	}
	return act.Decision{Rule: rule, Allowed: true}
}

// Effects hands over the notes decided since the last call and forgets them
// (stage.Effecting).
//
// Draining is what makes the hand-off exactly-once: the actor takes them and
// is then the only thing that holds them, so a note cannot be delivered twice
// by a cycle that ran while the first delivery was in flight. Each is the
// [Note] VALUE, never a wrapper, so the host's type assertion in OnEffects is
// the hand-off (claudestate's stage.Effect contract).
func (s *Stage) Effects() []stage.Effect {
	if len(s.pending) == 0 {
		return nil
	}
	out := make([]stage.Effect, len(s.pending))
	for i, n := range s.pending {
		out[i] = n
	}
	s.pending = nil
	return out
}

// WakeIn reports how long until this stage's answer changes on its own: the
// remainder of `notify.delay` for a session that qualifies and has not waited
// long enough yet.
//
// false for everything else, which is almost every session almost all the
// time. A stage that answered a constant duration would turn its actor into a
// poll loop, which is the one thing this layer forbids outright.
func (s *Stage) WakeIn() (time.Duration, bool) { return s.wake, s.hasWake }

// EffectName names the effect: a Note is a stage.Effect, and this is the only
// effect magmux's chain produces.
func (n Note) EffectName() string { return EffectName }

// EffectName is [Note.EffectName]'s answer, for a host to compare against.
const EffectName = "notify"
