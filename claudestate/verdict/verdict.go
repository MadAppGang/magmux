package verdict

import (
	"fmt"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
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
	Shell model.ShellState
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
//     makes the second one mean anything, and [model.ShellState.AtPrompt]
//     documents the measurement behind it.
//
//  4. Otherwise undetermined, which is what a shell with no integration and
//     no proven job control leaves — the commonest pane on any machine.
func (sc Screen) running() model.Liveness {
	switch {
	case sc.Shell.Mark == model.ShellMarkRunning:
		return model.LivenessYes
	case sc.Shell.AtPrompt == model.LivenessNo:
		return model.LivenessYes
	case sc.Shell.Mark == model.ShellMarkPrompt, sc.Shell.AtPrompt == model.LivenessYes:
		return model.LivenessNo
	default:
		return model.LivenessUnknown
	}
}

// Status maps the screen onto the status vocabulary, conservatively.
//
// The mapping is deliberately poorer than the vocabulary it maps into: a quiet
// screen is what an idle session, a session holding a permission prompt and a
// session waiting on input all look like, so this never returns
// [model.StatusWaiting] or [model.StatusShell]. A tier that guessed between them
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
func (sc Screen) Status() model.Status {
	switch {
	case !sc.Known, sc.Typed:
		return model.StatusUnknown
	case sc.running() == model.LivenessNo:
		return model.StatusIdle
	case sc.Quiet < QuietIdle:
		return model.StatusBusy
	case sc.running() == model.LivenessYes:
		return model.StatusUnknown
	default:
		return model.StatusIdle
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
func (sc Screen) contradicts(auth model.Status) bool {
	return auth.IsWorking() && sc.Quiet >= QuietConflict && sc.Status() == model.StatusIdle
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
func Resolve(s model.Session, screen Screen, now time.Time, rules Rules) model.Session {
	s.Conflict = nil

	switch {
	case s.Status != model.StatusUnknown:
		// The harness spoke and we understood it. Authoritative wins.
		s.Confidence = model.ConfidenceAuthoritative
		if screen.contradicts(s.Status) {
			s.Conflict = &model.Conflict{
				Confidence: model.ConfidenceInferred,
				Status:     model.StatusIdle,
				Detail:     "the pane has produced no output for " + humanDuration(screen.Quiet) + screen.promptSuffix(),
			}
		}

	case s.StatusRaw != "":
		// The harness spoke and we could not decode it. The inferred tier does
		// NOT take over: replacing a word we failed to understand with a guess
		// throws away the only truth available, and StatusRaw is documented as
		// the sole source of truth in exactly this case.
		s.Confidence = model.ConfidenceUnknown

	case screen.Status() != model.StatusUnknown:
		// The harness said nothing at all, so the inferred tier wins by
		// default. There is no loser and therefore no conflict: one tier
		// speaking alone is not a disagreement. This is the ONE write to the
		// effective status after the join (C19), and it marks itself:
		// StatusFrom says no record decided it, and StatusRaw, StatusSince,
		// Waiting and WaitingRaw stay exactly as rule E gave them.
		s.Status = screen.Status()
		s.StatusFrom = model.SourceUnknown
		s.Confidence = model.ConfidenceInferred

	default:
		s.Confidence = model.ConfidenceUnknown
	}

	s.Attention = decide(s, screen, now, rules)
	return s
}

// Rules are the attention policy's switches (D9). The zero value is magmux's
// ladder; tt sets IgnoreAPIErrors. Stated as a negative so a struct literal
// keeps today's behaviour.
type Rules struct {
	// IgnoreAPIErrors skips rule 3: tt's attention is status alone.
	IgnoreAPIErrors bool
}

// The ladder's "cannot decide" reasons and Rule 1's dismissals, as a closed
// set of prefixes, so the differential's D9 predicates are a membership test
// and not a string match against prose.
const (
	reasonConflict      = "its own record says it is working, but "
	reasonInferredQuiet = "the pane has been quiet for "
	reasonRunningQuiet  = "a command is running in the pane and has printed nothing for "
	reasonUnrecognised  = "the tool reported a status this build does not recognise"
	reasonNothingKnown  = "nothing is known about this session yet"

	reasonGoneBoth    = "its process has exited and its pane was closed"
	reasonGoneProcess = "its process has exited"
	reasonGonePane    = "its pane was closed"
)

var undeterminedPrefixes = []string{
	reasonConflict, reasonInferredQuiet, reasonRunningQuiet, reasonUnrecognised, reasonNothingKnown,
}

// IsUndetermined reports whether an Attention.Reason is one of the ladder's
// "cannot decide" words (the D9a predicate).
func IsUndetermined(reason string) bool {
	for _, p := range undeterminedPrefixes {
		if strings.HasPrefix(reason, p) {
			return true
		}
	}
	return false
}

// IsLivenessDismissal reports whether a Reason is Rule 1's (the D9b predicate).
func IsLivenessDismissal(reason string) bool {
	switch reason {
	case reasonGoneBoth, reasonGoneProcess, reasonGonePane:
		return true
	}
	return false
}

// decide is the attention policy: ordered rules, first match wins. The whole
// of it is written out in the package doc, in the same order, with the
// reasoning for each. Every rule reads the EFFECTIVE status (§3.4).
func decide(s model.Session, screen Screen, now time.Time, rules Rules) model.Attention {
	// RULE 1 — a liveness gate said no. Only when no job record claims the
	// session: for a background session ProcessAlive describes the SERVING
	// process, which the twin rule has already weighed, and a dead twin over
	// a blocked job is still blocked (review 01-1.4).
	//
	// LivenessNo only. LivenessUnknown never matches: "nobody could tell" is
	// not "no".
	if s.Job == nil {
		gone := ""
		switch {
		case s.ProcessAlive == model.LivenessNo && s.PaneAlive == model.LivenessNo:
			gone = reasonGoneBoth
		case s.ProcessAlive == model.LivenessNo:
			gone = reasonGoneProcess
		case s.PaneAlive == model.LivenessNo:
			gone = reasonGonePane
		}
		if gone != "" {
			return no(gone)
		}
	}

	// RULE 2 — it is waiting, or its job is blocked. The point of the feature.
	if s.WaitingApplies() {
		return yes(waitingReason(s) + inStatusSuffix(s, now))
	}

	// RULE 3 — an API-level error was counted. A nil count is "nobody looked"
	// and a determined 0 is "none", and neither is an error.
	if n := s.Fault.APIErrors; !rules.IgnoreAPIErrors && n != nil && *n > 0 {
		reason := "it hit an API-level error"
		if *n > 1 {
			reason = fmt.Sprintf("it hit %d API-level errors", *n)
		}
		if s.Fault.LastMessage != "" {
			reason += ": " + s.Fault.LastMessage
		}
		return yes(reason)
	}

	// RULE 4 — the two tiers conflict. Undetermined: false would hide a
	// wedged session and true would cry wolf over every long tool call.
	if s.Conflict != nil {
		return undetermined(reasonConflict + s.Conflict.Detail +
			"; which of the two is right cannot be told from here")
	}

	// RULE 5 — it is working.
	if s.Status.IsWorking() {
		return no("it is working" + inStatusSuffix(s, now))
	}

	// RULE 6 — nothing outstanding, on the authoritative tier. A real "no":
	// we looked, at the best evidence there is. A failed job needs READING,
	// not answering, as tt's IsAttention has it.
	if s.Confidence == model.ConfidenceAuthoritative {
		switch s.Status {
		case model.StatusIdle:
			return no("its own record says it has nothing outstanding" + inStatusSuffix(s, now))
		case model.StatusShell:
			return no("it has handed its terminal to a shell" + inStatusSuffix(s, now))
		case model.StatusDone:
			return no("its job has finished" + inStatusSuffix(s, now))
		case model.StatusFailed:
			return no("its job failed; it needs reading, not answering" + inStatusSuffix(s, now))
		}
	}

	// RULE 7 — the shell is at a prompt, said outright. The generic tier's one
	// DETERMINED answer: a shell that has drawn its prompt cannot be holding a
	// permission prompt open.
	if s.Confidence == model.ConfidenceInferred && s.Status == model.StatusIdle &&
		screen.running() == model.LivenessNo {
		return no(screen.promptReason())
	}

	// RULE 8 — idle on the inferred tier alone. A quiet screen is what an idle
	// session, a permission prompt and a session waiting on input all look
	// like, so "it does not need anyone" is a claim this evidence cannot carry.
	if s.Confidence == model.ConfidenceInferred && s.Status == model.StatusIdle {
		return undetermined(fmt.Sprintf(reasonInferredQuiet+"%s, and the tool's own record says nothing, so whether "+
			"it is waiting for you cannot be told", humanDuration(screen.Quiet)))
	}

	// RULE 9 — a command is running in the pane and has printed nothing.
	if screen.running() == model.LivenessYes && screen.Known && !screen.Typed &&
		screen.Quiet >= QuietIdle && s.Status == model.StatusUnknown && s.StatusRaw == "" {
		return undetermined(fmt.Sprintf(reasonRunningQuiet+"%s, so whether it is "+
			"working or waiting for you cannot be told", humanDuration(screen.Quiet)))
	}

	// RULE 10 — nothing is known. The raw word is quoted when there is one, so
	// that a status from a newer Claude than this build is DISCOVERABLE.
	if s.StatusRaw != "" {
		return undetermined(fmt.Sprintf(reasonUnrecognised+" (%q)", s.StatusRaw))
	}
	return undetermined(reasonNothingKnown)
}

// promptReason is rule 7's sentence. The exit status is REPORTED and does not
// change the verdict: a non-zero exit would otherwise be sticky forever, the
// mark being the last thing that happened in the pane.
func (sc Screen) promptReason() string {
	base := "its shell is at a prompt with nothing running"
	switch {
	case sc.Shell.Exit == nil:
		return base
	case *sc.Shell.Exit == 0:
		return base + ", and its last command succeeded"
	default:
		return fmt.Sprintf("%s, but its last command exited %d", base, *sc.Shell.Exit)
	}
}

// promptSuffix is the same fact in the half-sentence a conflict detail wants.
func (sc Screen) promptSuffix() string {
	if sc.running() != model.LivenessNo {
		return ""
	}
	if sc.Shell.Exit != nil && *sc.Shell.Exit != 0 {
		return fmt.Sprintf(", and its shell is back at a prompt after a command that exited %d", *sc.Shell.Exit)
	}
	return ", and its shell is back at a prompt"
}

// waitingReason names what a waiting session is blocked on. A blocked job
// names its own needs when it states them.
func waitingReason(s model.Session) string {
	if s.Status == model.StatusBlocked {
		if s.Job != nil && s.Job.Needs != nil && *s.Job.Needs != "" {
			return "its job is blocked: " + *s.Job.Needs
		}
		if s.Waiting == model.WaitingUnknown {
			return "its job is blocked"
		}
	}
	switch s.Waiting {
	case model.WaitingPermission:
		return "it is holding a permission prompt open"
	case model.WaitingInput:
		return "it is waiting for you to type something"
	case model.WaitingDialog:
		return "it is holding a dialog open"
	}
	if s.WaitingRaw != "" {
		return fmt.Sprintf("it is waiting, for something this build does not recognise (%q)", s.WaitingRaw)
	}
	return "it is waiting, and what for was not determined"
}

// inStatusSuffix is ", for 3m 21s", or nothing when the instant was never
// determined. It affects only the text: no rule branches on how long.
func inStatusSuffix(s model.Session, now time.Time) string {
	if s.StatusSince == nil {
		return ""
	}
	return ", for " + humanDuration(now.Sub(*s.StatusSince))
}

func yes(reason string) model.Attention {
	t := true
	return model.Attention{Needed: &t, Reason: reason}
}

func no(reason string) model.Attention {
	f := false
	return model.Attention{Needed: &f, Reason: reason}
}

// undetermined is the third state, and it always carries a reason: a nil
// Needed with an empty Reason would be indistinguishable from a session
// nothing has looked at yet.
func undetermined(reason string) model.Attention {
	return model.Attention{Reason: reason}
}

// humanDuration spells a duration the way magmux's status bar does: a reason
// line reading "for 44h 21m" beside a table column reading "44h 21m" is one
// fact said once.
func humanDuration(d time.Duration) string {
	if d < 0 {
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
