// Package navigate is the observation layer's one live actuator: it focuses
// the magmux pane a session is running in, and it does nothing else.
//
// It is a host stage in the claudestate pipeline's chain — a
// [github.com/MadAppGang/magmux/claudestate/stage.Stage] built per session by
// [Factory] — with an
// [github.com/MadAppGang/magmux/internal/obs/act.Actuator] implementation
// beside its Observe. That is the shape the whole write path was built for:
// adding R5.3's `send` later is a package like this one, satisfying the same
// interface, plus its verb filed in act's closed verb table — and no change to
// the actor, the gate's logic, the audit trail or any other stage. The table
// entry is not optional: a verb it does not contain is refused whatever its
// actuator declares.
//
// # It reads the fact set rather than the pane table
//
// The pane a session is in is resolved once per cycle by the library's pane
// gate and arrives here as the session's model.PaneRef, which is magmux's pane
// id in its decimal spelling (ADR-X1). This stage therefore does no
// process-tree walk of its own and cannot disagree with the document about
// which pane it is about — a navigate that focused a pane the table said was a
// different session's would be a wrong answer that looks exactly like a right
// one.
//
// # Nothing here decides whether it MAY act
//
// Policy is the gate's, and this package never consults it. A stage that
// approved its own action would be a second policy nobody configured; see the
// act package doc.
package navigate

import (
	"context"
	"fmt"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/obsview"
)

// Name is the stage's name in the chain, which is how a host finds it on an
// actor (pipeline.Ops.Call's find).
const Name = "navigate"

// PaneFocuser moves keyboard focus to a magmux pane and reports which pane
// held it before (-1 when none did, or when that cannot be seen).
//
// It is the observation layer's one WRITE port, and it lives here, beside the
// one stage that calls it, because it is a host port: the claudestate library
// has no write half at all.
//
// It is called from an actor goroutine, so an implementation must not block
// indefinitely: one that waits on a socket bounds the wait.
type PaneFocuser interface {
	Focus(paneID int) (previous int, err error)
}

// Config is how a [Stage] is built. A plain value, copied on the way in.
type Config struct {
	// Focuser is what actually moves focus. nil means nothing in this process
	// can — which is the honest and normal case for anything running outside
	// a magmux — and the stage then declares NO capability rather than
	// declaring one and refusing it. "Cannot" and "will not" are different
	// answers.
	Focuser PaneFocuser
}

// Factory builds one stage per session actor.
//
// A nil focuser does not remove the stage; it removes the stage's ability to
// act, which it reports by declaring no capability. That is the honest shape
// for a process with no pane table, and it is the difference between "nothing
// here can do that" and "you may not", which act.ErrNotCapable and
// act.ErrForbidden keep apart all the way out to the audit trail.
func Factory(cfg Config) stage.Factory {
	return func(model.Key) stage.Stage { return New(cfg) }
}

// Stage is one session's navigator.
//
// One instance per session, never shared: it remembers its session's pane, and
// a shared instance would focus the first session's pane for every session
// that asked.
type Stage struct {
	focuser PaneFocuser

	// The per-session memory, written by Observe on the actor's goroutine and
	// read by Capabilities and Do on the same one (pipeline.Ops.Call runs
	// there). No lock, because there is no second goroutine.
	paneID    int
	hasPane   bool
	paneAlive model.Liveness
}

// New builds one session's stage.
func New(cfg Config) *Stage { return &Stage{focuser: cfg.Focuser} }

// Name identifies the stage in the chain.
func (s *Stage) Name() string { return Name }

// Observe adds NOTHING to the fact set and changes nothing in it. It remembers
// the pane the gate resolved, which is the whole of what Do needs.
func (s *Stage) Observe(sess model.Session) model.Session {
	s.paneID, s.hasPane = obsview.PaneID(sess.Pane)
	s.paneAlive = sess.PaneAlive
	return sess
}

// Capabilities declares `focus` when this session can be navigated to, and
// nothing at all when it cannot.
//
// Three reasons a session cannot be, and each is an ABSENCE rather than a
// refusal, because a capability that is offered and then refused reads as
// policy:
//
//   - there is no focuser in this process, so nothing can move focus at all;
//   - no pane was resolved for this session, which is every session on a
//     machine that magmux did not start, and every session at all when the
//     observing process is not a magmux;
//   - the pane was determined to be GONE. An UNDETERMINED pane is still
//     declared — "nobody could tell" is not "no", and the focuser gets to
//     give the real answer rather than having one assumed for it.
func (s *Stage) Capabilities() []act.Capability {
	if s.focuser == nil || !s.hasPane || s.paneAlive == model.LivenessNo {
		return nil
	}
	return []act.Capability{{
		Verb: act.VerbFocus,
		Tier: act.TierNavigate,
		// Reversible: focus can be moved back, and the audit record says
		// where it was. Idempotent: focusing the focused pane is a no-op that
		// still succeeds, so a retry after a timeout cannot do anything
		// twice.
		Reversible: true,
		Idempotent: true,
		Describe:   fmt.Sprintf("move keyboard focus to magmux pane %d", s.paneID),
	}}
}

// Do performs the focus.
//
// It re-checks what [Stage.Capabilities] declared rather than trusting that
// the gate called them in order. Both run on this actor's goroutine and
// nothing can interleave between them today, which makes the re-check
// unreachable — and it is here anyway, because the day something can
// interleave, the alternative is focusing a pane nobody resolved inside the
// one function in this layer that changes something.
func (s *Stage) Do(ctx context.Context, a act.Action) (act.Result, error) {
	if err := ctx.Err(); err != nil {
		return act.Result{}, err
	}
	if a.Verb != act.VerbFocus {
		return act.Result{}, fmt.Errorf("navigate: %q is not a verb this stage performs", a.Verb)
	}
	if s.focuser == nil {
		return act.Result{}, fmt.Errorf("navigate: nothing in this process can move focus")
	}
	if !s.hasPane {
		return act.Result{}, fmt.Errorf("navigate: no magmux pane was resolved for this session")
	}
	if s.paneAlive == model.LivenessNo {
		return act.Result{}, fmt.Errorf("navigate: magmux pane %d is gone", s.paneID)
	}

	pane := s.paneID
	previous, err := s.focuser.Focus(pane)
	if err != nil {
		// Reported as a failed action rather than as a refusal: the gate
		// allowed it and it was attempted, and the audit outcome says so.
		return act.Result{}, fmt.Errorf("navigate: focusing magmux pane %d: %w", pane, err)
	}
	if previous == pane {
		return act.Result{
			Done:   true,
			Detail: fmt.Sprintf("focus was already on magmux pane %d", pane),
		}, nil
	}
	return act.Result{
		Done: true,
		Detail: fmt.Sprintf("focus moved from magmux pane %s to pane %d",
			paneText(previous), pane),
	}, nil
}

// paneText names the pane focus came from. -1 is the documented "nothing held
// focus" answer and is spelled out rather than printed as an id nobody has.
func paneText(id int) string {
	if id < 0 {
		return "(none)"
	}
	return fmt.Sprintf("%d", id)
}
