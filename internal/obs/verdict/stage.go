package verdict

import (
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/termev"
)

// Config is how a [Stage] is built. A plain value, copied on the way in.
type Config struct {
	// Activity is the inferred tier's only input. nil means there is no
	// terminal evidence available in this process, which is the honest and
	// normal case for anything running outside a magmux — `magmux agents` is
	// exactly that — and leaves every session resolved on the authoritative
	// tier alone, with no conflict possible.
	Activity obs.PaneActivity

	// Shell is the generic tier's DIRECT input: who owns the pane's terminal,
	// and what its shell's own OSC 133 marks last said. nil leaves the tier
	// with the staleness cascade alone, which is what it had before this port
	// existed and is the honest answer for anything running outside a magmux.
	//
	// It is a separate port from Activity and not an extension of it because
	// a call here SAMPLES: it advances the per-pane latch that makes
	// [obs.ShellState.AtPrompt] mean anything. See that field.
	Shell obs.PaneShell

	// Now is the instant an observation is taken at. nil takes time.Now.
	//
	// It is a func and not a [obs.Clock] on purpose, and the two are not
	// interchangeable: a Clock CREATES TIMERS and its fake answers one fixed
	// instant forever, which is exactly wrong for a stage whose whole subject
	// is elapsed time. See registry.Config.Now, which documents the trap.
	Now func() time.Time
}

// New builds ONE session's verdict stage.
//
// It satisfies pipeline.Middleware and pipeline.Waker structurally, without
// naming either: this package cannot import pipeline, because pipeline imports
// this one to put the stage at the end of every chain. Go's interfaces are
// structural, so the dependency only has to run one way.
//
// One instance per session, never shared. It remembers its session's pane and
// its session's cascade, and a shared instance would answer every session with
// the first session's silence.
func New(cfg Config) *Stage {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	// The ladder is the package's own and is deliberately not configurable:
	// QuietIdle and QuietConflict are both RUNGS on it, so the moments this
	// stage's answer can change are exactly the moments it asks to be woken
	// at. A caller-supplied ladder could break that silently, and
	// TestEveryThresholdTheVerdictTurnsOnIsARungOnTheLadder is what keeps it
	// true for this one.
	cascade, err := termev.NewCascade(termev.DefaultLadder()...)
	if err != nil {
		// Unreachable: DefaultLadder is a constant table with its own
		// constructor test. A nil cascade degrades to "no inferred tier"
		// rather than taking a terminal multiplexer down over an instrument.
		cascade = nil
	}
	return &Stage{act: cfg.Activity, shell: cfg.Shell, now: now, cascade: cascade}
}

// Stage is one session's inferred tier and one session's resolution.
//
// # It holds the cascade, and nothing outside its actor's goroutine ever sees
// it
//
// Phase 1 put a goroutine and a timer around a [termev.Cascade] in
// termev.Driver, which said in its own documentation that it was temporary and
// that the per-session actor would take the cascade over. This is that. The
// cascade is a field of a stage the actor built, is touched only by the
// actor's goroutine, and is reachable from nowhere else — there is no accessor
// and no pointer handed out. The waiting the Driver did in a select is now the
// actor's single one-shot timer, armed from [Stage.WakeIn].
type Stage struct {
	act     obs.PaneActivity
	shell   obs.PaneShell
	now     func() time.Time
	cascade *termev.Cascade

	// firstSeen is when this stage first looked at a pane that had neither
	// produced nor received a byte.
	//
	// termev.Driver armed its cascade AT BIRTH, because "a pane that produces
	// not one byte from the moment it is created is the most interesting
	// staleness there is". The birth instant does not reach this layer — the
	// activity port carries the two instants the brief names and no third —
	// so the cascade is armed at FIRST SIGHT instead. First sight is never
	// earlier than birth, so the silence measured this way is never longer
	// than the real one: the ladder UNDER-reports rather than over-reports,
	// which is the direction an inference is allowed to be wrong in.
	firstSeen time.Time

	// wake is how long until this stage's own answer changes, recomputed on
	// every observation. See [Stage.WakeIn].
	wake     time.Duration
	haveWake bool
}

// Name is how this stage appears in diagnostics.
func (st *Stage) Name() string { return "verdict" }

// Observe gathers the inferred tier and resolves the session. It is the last
// stage of every chain, because it is the only one that needs every other
// stage's facts: the pane gate's pane id, the registry's status, the Claude
// Code middleware's error count.
func (st *Stage) Observe(s obs.Session) obs.Session {
	now := st.now()
	return Resolve(s, st.look(s, now), now)
}

// WakeIn reports how long until this stage's answer changes with NO new
// observation, which is the whole reason the actor owns a timer.
//
// A staleness threshold crossing is the one thing in this layer that happens
// because time passed rather than because something changed on disk. Without
// it, a pane that goes silent while its record stays put would never be
// re-examined and the conflict this package exists to publish would never
// appear — the registry write that would wake the pipeline is precisely the
// thing a wedged session is not doing.
//
// ok is false when nothing will change on its own: the cascade is cancelled by
// input, or its ladder is exhausted, or there is no inferred tier at all. The
// actor then holds no timer for this session, which is what makes an idle
// machine cost nothing.
//
// A non-positive duration is reported as "nothing to wait for" rather than as
// "wake immediately". It is unreachable — [Stage.Observe] consumes every rung
// already due, so the next one is strictly in the future — and it is guarded
// anyway, because a zero-length wake would be a busy loop, which is the one
// thing this layer forbids outright.
func (st *Stage) WakeIn() (time.Duration, bool) { return st.wake, st.haveWake }

// look asks the activity port about this session's pane and turns the answer
// into a [Screen], re-arming or cancelling the cascade on the way.
func (st *Stage) look(s obs.Session, now time.Time) Screen {
	st.wake, st.haveWake = 0, false

	if st.act == nil || st.cascade == nil || s.PaneID == nil {
		return Screen{}
	}
	lastOutput, lastInput, ok := st.act.Activity(*s.PaneID)
	if !ok {
		return Screen{}
	}

	// The terminal facts, sampled once per observation and on no other
	// schedule. This call is what advances the pane's job-control latch, so it
	// happens on EVERY observation of a live pane — including the ones that
	// return early below, because a latch that only advanced on the paths a
	// verdict happened to need would prove itself late or never.
	//
	// A pane with no terminal to ask about — the control panel has no PTY —
	// answers false and leaves the zero value, which reads as "nothing was
	// established" everywhere downstream.
	var shell obs.ShellState
	if st.shell != nil {
		if sh, ok := st.shell.Shell(*s.PaneID); ok {
			shell = sh
		}
	}

	// INPUT CANCELS. The pane was typed into more recently than the child
	// printed, so its silence belongs to the human waiting for an answer and
	// not to the tool. The cascade holds no deadline afterwards, which is why
	// no timer is armed for this session until output resumes.
	if lastInput.After(lastOutput) {
		st.cascade.Input(lastInput)
		return Screen{Known: true, Typed: true, Shell: shell}
	}

	since := lastOutput
	if since.IsZero() {
		// Neither instant is set: this pane has done nothing at all. Armed at
		// first sight; see the field's own comment for why that is not birth.
		if st.firstSeen.IsZero() {
			st.firstSeen = now
		}
		since = st.firstSeen
	}

	// Refusal is the NORMAL case and is not an error: a quiet pane reports the
	// same instant on every observation, and the cascade refuses anything not
	// strictly after the quiet period it is already measuring. Honouring it
	// would restart a silence that has already crossed rungs.
	st.cascade.Activity(since)

	// Drain the rungs that are already due, so that the deadline below is the
	// NEXT crossing rather than one in the past. The events are not used: this
	// stage measures the silence directly, to the instant, rather than
	// reporting which rung it last passed.
	for {
		if _, ok := st.cascade.Advance(now); !ok {
			break
		}
	}
	if deadline, armed := st.cascade.Deadline(); armed {
		if d := deadline.Sub(now); d > 0 {
			st.wake, st.haveWake = d, true
		}
	}

	return Screen{Known: true, Quiet: now.Sub(st.cascade.Since()), Shell: shell}
}
