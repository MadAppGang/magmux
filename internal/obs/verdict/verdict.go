package verdict

import (
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// QuietIdle is how long a pane must have produced nothing before the inferred
// tier will call it idle at all.
//
// It is the FIRST RUNG of the staleness ladder, not a new number: a threshold
// invented here would be a second table of quiet times beside termev's, and
// the two would drift the first time either was tuned.
const QuietIdle = 5 * time.Second

// QuietConflict is how long a pane must have produced nothing before a record
// calling it busy is contradicted.
//
// A CHOICE, not a measurement, and the package doc says why at length: a
// working session goes quiet for short stretches legitimately, so a conflict
// raised at the first rung would fire constantly and mean nothing. This is the
// first rung past the half-minute a slow tool call plausibly takes, and it is
// a rung on the existing ladder.
const QuietConflict = time.Minute

// Screen is what the INFERRED tier has to say about one pane: whether anything
// is known at all, whether the last thing to happen was somebody typing, how
// long the child has been silent, and what the terminal itself says about the
// shell in there.
//
// It is a value with no methods that reach outside it, so the resolution below
// can be tested against every combination without a cascade, a clock, a pane
// or a syscall — and so that a reader can see the whole of the inferred tier's
// evidence in one struct. The tier is narrow because its evidence is narrow.
//
// The first three fields are the staleness half and were the whole of it until
// the generic tier was built out; [Screen.Shell] is the direct half. Silence
// is a PROXY for "nothing is happening" and the terminal facts are DIRECT
// evidence about it, which is why [Screen.running] resolves them first and why
// the conflict rule below withdraws when they speak.
type Screen struct {
	// Known is false when there is no terminal evidence at all — no pane, or
	// a pane that has neither produced nor received a byte. Every other field
	// is meaningless then.
	Known bool

	// Typed is true when the last thing to happen to the pane was INPUT: a
	// human keystroke or a controller's injected instruction, more recent than
	// any output. The silence is then the human's, not the child's, and the
	// inferred tier has no opinion about the tool.
	Typed bool

	// Quiet is how long the child has produced nothing, as of the instant the
	// observation was taken. Meaningless when Typed is true.
	Quiet time.Duration

	// Shell is what the terminal says: the latched tcgetpgrp answer and the
	// shell's own OSC 133 marks. Its zero value is "nothing was asked or
	// nothing answered", which is the common case — most shells emit no marks
	// and most panes never prove job control — and it maps to exactly the
	// behaviour this package had before the port existed.
	Shell obs.ShellState
}

// running reconciles the two direct signals into one answer: is there a
// command executing in this pane, or is the shell sitting at a prompt?
//
// The order is the whole of the reconciliation and each step is a claim:
//
//  1. OSC 133 saying RUNNING wins outright. It is the shell stating what it
//     itself is doing, and it is right in the one case tcgetpgrp is wrong in:
//     a BUILTIN — `read`, a `while` loop, a long `source` — executes inside
//     the shell's own process group, so the foreground group never moves and
//     the kernel fact says "at a prompt" while the shell is plainly busy.
//
//  2. A foreground group that is NOT the session leader is running, whatever
//     the marks say. It is unambiguous — something else holds the terminal —
//     and it also covers the reverse gap: a command started without a 133;C,
//     which happens the moment anything is launched by something other than
//     the shell's own prompt loop.
//
//  3. Either signal saying "at a prompt" is accepted. 133;A / 133;B / 133;D
//     are the shell volunteering that it has nothing outstanding, and a
//     LATCHED foreground equality is the kernel agreeing; the latch is what
//     makes the second one mean anything, and [obs.ShellState.AtPrompt]
//     documents the measurement behind it.
//
//  4. Otherwise undetermined, which is what a shell with no integration and
//     no proven job control leaves — the commonest pane on any machine.
func (sc Screen) running() obs.Liveness {
	switch {
	case sc.Shell.Mark == obs.ShellMarkRunning:
		return obs.LivenessYes
	case sc.Shell.AtPrompt == obs.LivenessNo:
		return obs.LivenessYes
	case sc.Shell.Mark == obs.ShellMarkPrompt, sc.Shell.AtPrompt == obs.LivenessYes:
		return obs.LivenessNo
	default:
		return obs.LivenessUnknown
	}
}

// Status maps the screen onto the status vocabulary, conservatively.
//
// The mapping is deliberately poorer than the vocabulary it maps into: a quiet
// screen is what an idle session, a session holding a permission prompt and a
// session waiting on input all look like, so this never returns
// [obs.StatusWaiting] or [obs.StatusShell]. A tier that guessed between them
// would be inventing a fact and labelling it observed.
//
// Two of the four arms are the direct evidence and they are read FIRST, because
// silence is a proxy for what they state outright:
//
//	at a prompt                    -> idle. Nothing is outstanding, said by the
//	                                  shell or by the kernel. No waiting on the
//	                                  ladder is needed to establish it.
//	bytes arriving in the last 5s  -> busy, as before.
//	silent, with a command RUNNING -> UNKNOWN, and this is the arm that used to
//	                                  say idle. A foreground command that has
//	                                  printed nothing is either working or
//	                                  waiting for a human, and the tier cannot
//	                                  tell which — but it now knows the one
//	                                  thing it can rule out, which is that the
//	                                  session finished. Reporting idle there is
//	                                  the wrong "idle" this whole tier is built
//	                                  to avoid.
//	silent, nothing else known     -> idle, exactly as before.
func (sc Screen) Status() obs.Status {
	switch {
	case !sc.Known, sc.Typed:
		return obs.StatusUnknown
	case sc.running() == obs.LivenessNo:
		return obs.StatusIdle
	case sc.Quiet < QuietIdle:
		return obs.StatusBusy
	case sc.running() == obs.LivenessYes:
		return obs.StatusUnknown
	default:
		return obs.StatusIdle
	}
}

// contradicts reports whether this screen contradicts the authoritative status
// — the one direction in which the two tiers are allowed to disagree.
//
// Only "the record says busy and the pane has been silent for a full minute".
// The mirror case is not a conflict; see the package doc, where the asymmetry
// is argued rather than assumed.
//
// # The minute did not move, and direct evidence WITHDRAWS the conflict
//
// The threshold is the same rung it always was. What changed is that silence
// is no longer the only thing the tier can see, and the rule now requires the
// tier to have actually CONCLUDED idle rather than merely to have counted a
// minute: a pane whose shell says a command is running is a pane with direct
// evidence that the record's "busy" is right, and raising a conflict against
// it would be publishing a disagreement that no longer exists. That is the
// whole of the change — direct evidence can only ever take a conflict away
// here, never add one.
//
// The other direction was considered and rejected. Direct at-prompt evidence
// against a busy record is a stronger contradiction than a minute of silence,
// and it is tempting to raise it immediately. It does not earn its risk: a
// shell back at its prompt while a harness record says busy means the tool's
// process is gone, which the process liveness gate reports directly and which
// attention rule 1 acts on before any of this is read. Shortening the
// threshold there would buy a faster answer to a question already answered,
// at the cost of putting a latch's correctness in front of it.
func (sc Screen) contradicts(auth obs.Status) bool {
	return auth == obs.StatusBusy && sc.Quiet >= QuietConflict && sc.Status() == obs.StatusIdle
}

// Resolve applies the two-tier rule and the attention policy to one session,
// and is the whole of this package's behaviour. Everything else here either
// gathers the screen for it or wraps it in a pipeline stage.
//
// It is a pure function over values: same session, same screen, same instant,
// same answer, every time. That is what lets the policy be tested as a table
// rather than through a pipeline, and it is why the rules can be read in one
// place.
//
// now is the instant the observation was taken, and is used only for duration
// TEXT — how long a session has been in its status, and how long a pane has
// been quiet. No rule branches on it.
func Resolve(s obs.Session, screen Screen, now time.Time) obs.Session {
	s.Conflict = nil

	switch {
	case s.Status != obs.StatusUnknown:
		// The harness spoke and we understood it. Authoritative wins.
		s.Confidence = obs.TierAuthoritative
		if screen.contradicts(s.Status) {
			s.Conflict = &obs.Conflict{
				Tier:   obs.TierInferred,
				Status: obs.StatusIdle,
				Detail: "the pane has produced no output for " + humanDuration(screen.Quiet) + screen.promptSuffix(),
			}
		}

	case s.StatusRaw != "":
		// The harness spoke and we could not decode it. The inferred tier does
		// NOT take over: replacing a word we failed to understand with a guess
		// throws away the only truth available, and StatusRaw is documented as
		// the sole source of truth in exactly this case.
		s.Confidence = obs.TierUnknown

	case screen.Status() != obs.StatusUnknown:
		// The harness said nothing at all, so the inferred tier wins by
		// default. There is no loser and therefore no conflict: one tier
		// speaking alone is not a disagreement.
		s.Status = screen.Status()
		s.Confidence = obs.TierInferred

	default:
		s.Confidence = obs.TierUnknown
	}

	s.Attention = decide(s, screen, now)
	return s
}

// decide is the attention policy: ordered rules, first match wins. The whole
// of it is written out in the package doc, in the same order, with the
// reasoning for each. Keep the two in step — a rule that exists only here is a
// rule nobody can argue with.
func decide(s obs.Session, screen Screen, now time.Time) obs.Attention {
	// RULE 1 — a liveness gate said no.
	//
	// LivenessNo only. LivenessUnknown never matches: "nobody could tell" is
	// not "no", and a session dismissed because nobody looked is the exact
	// failure the tri-state exists to prevent.
	gone := ""
	switch {
	case s.ProcessAlive == obs.LivenessNo && s.PaneAlive == obs.LivenessNo:
		gone = "its process has exited and its pane was closed"
	case s.ProcessAlive == obs.LivenessNo:
		gone = "its process has exited"
	case s.PaneAlive == obs.LivenessNo:
		gone = "its pane was closed"
	}
	if gone != "" {
		return no(gone)
	}

	// RULE 2 — it is waiting. The point of the feature.
	if s.Status == obs.StatusWaiting {
		return yes(waitingReason(s) + inStatusSuffix(s, now))
	}

	// RULE 3 — an API-level error was counted. A nil count is "nobody looked"
	// and a determined 0 is "none", and neither is an error.
	if n := s.Fault.APIErrors; n != nil && *n > 0 {
		reason := "it hit an API-level error"
		if *n > 1 {
			reason = fmt.Sprintf("it hit %d API-level errors", *n)
		}
		if s.Fault.Detail != "" {
			reason += ": " + s.Fault.Detail
		}
		return yes(reason)
	}

	// RULE 4 — the two tiers conflict. Undetermined, and this is the case the
	// three-state field exists for: false would hide a wedged session and true
	// would cry wolf over every long tool call.
	if s.Conflict != nil {
		return undetermined(fmt.Sprintf(
			"its own record says it is working, but %s; which of the two is right cannot be told from here",
			s.Conflict.Detail))
	}

	// RULE 5 — it is working.
	if s.Status == obs.StatusBusy {
		return no("it is working" + inStatusSuffix(s, now))
	}

	// RULE 6 — idle or in a shell, on the authoritative tier. A real "no": we
	// looked, at the best evidence there is.
	if s.Confidence == obs.TierAuthoritative {
		switch s.Status {
		case obs.StatusIdle:
			return no("its own record says it has nothing outstanding" + inStatusSuffix(s, now))
		case obs.StatusShell:
			return no("it has handed its terminal to a shell" + inStatusSuffix(s, now))
		}
	}

	// RULE 7 — the shell is at a prompt, said outright. The generic tier's one
	// DETERMINED answer, and the only rule in this policy that direct terminal
	// evidence can reach on its own.
	//
	// It sits above rule 8 because it defeats rule 8's argument rather than
	// sharing it: a quiet screen could be a permission prompt, but a shell
	// that has drawn its prompt has nothing outstanding and cannot be holding
	// one. The last command's status rides in the reason and NOT in the
	// verdict; the exit-status paragraph below says why.
	if s.Confidence == obs.TierInferred && s.Status == obs.StatusIdle &&
		screen.running() == obs.LivenessNo {
		return no(screen.promptReason())
	}

	// RULE 8 — idle on the inferred tier alone. A quiet screen is what an idle
	// session, a permission prompt and a session waiting on input all look
	// like, so "it does not need anyone" is a claim this evidence cannot carry.
	if s.Confidence == obs.TierInferred && s.Status == obs.StatusIdle {
		return undetermined(fmt.Sprintf(
			"the pane has been quiet for %s, and the tool's own record says nothing, so whether "+
				"it is waiting for you cannot be told", humanDuration(screen.Quiet)))
	}

	// RULE 9 — a command is running in the pane and has printed nothing. Not
	// "nothing is known": one thing is known, and it is the one that would
	// otherwise be got wrong. Rule 8's silence would have called this idle.
	//
	// StatusRaw is excluded so that rule 10 still quotes a word this build
	// does not recognise. A harness that spoke is more interesting than a
	// terminal fact about the shell underneath it, even when we failed to
	// understand what it said.
	if screen.running() == obs.LivenessYes && screen.Known && !screen.Typed &&
		screen.Quiet >= QuietIdle && s.Status == obs.StatusUnknown && s.StatusRaw == "" {
		return undetermined(fmt.Sprintf(
			"a command is running in the pane and has printed nothing for %s, so whether it is "+
				"working or waiting for you cannot be told", humanDuration(screen.Quiet)))
	}

	// RULE 10 — nothing is known. The raw word is quoted when there is one, so
	// that a status from a newer Claude than this build is DISCOVERABLE rather
	// than merely absent.
	if s.StatusRaw != "" {
		return undetermined(fmt.Sprintf(
			"the tool reported a status this build does not recognise (%q)", s.StatusRaw))
	}
	return undetermined("nothing is known about this session yet")
}

// promptReason is rule 7's sentence: the shell is at a prompt, and how the
// last command it ran ended.
//
// # The exit status is REPORTED and does not change the verdict
//
// A non-zero exit is the only generic signal in this layer that says a command
// FAILED rather than merely ended, and the obvious use of it — attention
// needed, a build broke — was considered and rejected for one reason that is
// not conservatism. It would be STICKY FOREVER. The mark is the last thing
// that happened in that pane, so a `grep` that matched nothing at 10am would
// have the pane asking for a human until something else is run in it, and the
// list of things needing attention would fill up with facts about the past. A
// verdict that cannot be cleared by the user acting on it is not a verdict.
//
// So it goes in the reason, where a human scanning the fleet sees "exited 1"
// beside a pane and can decide. Nothing switches on the text.
func (sc Screen) promptReason() string {
	base := "its shell is at a prompt with nothing running"
	switch {
	case sc.Shell.Exit == nil:
		// No status was reported. NOT "it succeeded": a bare 133;D is legal
		// and common, and most shells send no marks at all.
		return base
	case *sc.Shell.Exit == 0:
		return base + ", and its last command succeeded"
	default:
		return fmt.Sprintf("%s, but its last command exited %d", base, *sc.Shell.Exit)
	}
}

// promptSuffix is the same fact in the half-sentence a conflict detail wants.
// Empty unless the shell said so outright — a conflict detail is read by
// someone deciding which of two tiers to believe, and "the pane is quiet AND
// its shell is back at a prompt" is a much stronger thing to hand them than
// the silence alone.
func (sc Screen) promptSuffix() string {
	if sc.running() != obs.LivenessNo {
		return ""
	}
	if sc.Shell.Exit != nil && *sc.Shell.Exit != 0 {
		return fmt.Sprintf(", and its shell is back at a prompt after a command that exited %d", *sc.Shell.Exit)
	}
	return ", and its shell is back at a prompt"
}

// waitingReason names what a waiting session is blocked on.
//
// The two specific reasons get their own sentences rather than one sentence
// with a noun swapped in, because they are different situations: a permission
// prompt is a question that has already been asked and is holding the tool
// still, and waiting on input is a tool with nothing left to do. R2 keeps the
// two values distinct all the way to the wire, and collapsing them here — at
// the last step, in the one field a human actually reads — would undo that.
func waitingReason(s obs.Session) string {
	switch s.Waiting {
	case obs.WaitingReasonPermission:
		return "it is holding a permission prompt open"
	case obs.WaitingReasonInput:
		return "it is waiting for you to type something"
	case obs.WaitingReasonNone:
		return "it is waiting, on nothing it could name"
	}
	if s.WaitingRaw != "" {
		return fmt.Sprintf("it is waiting, for something this build does not recognise (%q)", s.WaitingRaw)
	}
	return "it is waiting, and what for was not determined"
}

// inStatusSuffix is ", for 3m 21s", or nothing when the instant was never
// determined.
//
// This is `blockedFor` — now minus statusUpdatedAt — and it is an ANSWER
// rather than a lower bound: the record is written when something CHANGES, so
// an idle session's instant is old and exact. Measured, after the opposite was
// believed for a while; see findings/statusupdatedat-is-not-restamped.md.
//
// It affects only the text. No attention rule branches on how long a session
// has been waiting, because how long to sit on a notification before
// interrupting a human is R6's `delay`, which is a policy about people.
func inStatusSuffix(s obs.Session, now time.Time) string {
	d, ok := s.InStatusFor(now)
	if !ok {
		return ""
	}
	return ", for " + humanDuration(d)
}

func yes(reason string) obs.Attention {
	return obs.Attention{Needed: obs.Ptr(true), Reason: reason}
}

func no(reason string) obs.Attention {
	return obs.Attention{Needed: obs.Ptr(false), Reason: reason}
}

// undetermined is the third state, and it always carries a reason.
//
// A nil Needed with an empty Reason would be indistinguishable from a session
// nothing has looked at yet, which is the one thing this package must never
// produce: it is the difference between "we cannot tell, and here is why" and
// "this field was never filled in".
func undetermined(reason string) obs.Attention {
	return obs.Attention{Reason: reason}
}

// humanDuration spells a duration the way magmux's own status bar does.
//
// It is a second copy of main.go's formatDuration and it has to be: this
// package cannot import package main — Go forbids importing main, which is the
// whole reason the observation layer talks to magmux through ports. The
// duplication is bounded to this one function, and the SPELLING is the thing
// being duplicated deliberately: a reason line reading "for 44h 21m" beside a
// table column reading "44h 21m" is one fact said once.
func humanDuration(d time.Duration) string {
	if d < 0 {
		// The reader's clock is behind the instant it was handed. Said rather
		// than clamped away, because a negative duration is a fact worth
		// seeing — most often a record written by a machine whose clock is
		// ahead of this one.
		return "-" + humanDuration(-d)
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
