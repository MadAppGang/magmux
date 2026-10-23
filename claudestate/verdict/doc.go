// Package verdict answers the question the whole observation layer exists for:
// WHICH SESSION NEEDS ME, AND WHY.
//
// Everything upstream of here gathers evidence. The registry source says what
// the tool wrote down, the Claude Code middleware reads the tool's own
// transcript, the pane gate says whether magmux still owns the pane, and the
// staleness cascade says how long the screen has been silent. This package
// turns all of that into two published facts: [model.Session.Attention], which
// is the product, and [model.Session.Conflict], which is the honesty.
//
// It does three things, in this order, and the whole of each is written down
// below because a policy that can only be read as a chain of branches is a
// policy nobody can argue with.
//
//  1. It produces the INFERRED tier from the pane's traffic.
//  2. It RESOLVES the two tiers, publishing the winner and, when they
//     disagree, the loser.
//  3. It decides ATTENTION.
//
// # 1. The inferred tier
//
// Two ports. [ports.PaneActivity] answers when a pane last produced output and
// when something was last pushed into it; a [termev.Cascade] turns the first
// into a ladder of thresholds — 5s, 10s, 30s, 1m, 5m, 30m, 1h — and the second
// cancels it, because a pane somebody just typed into is not a pane whose
// silence is its own to explain. [ports.PaneShell] answers what the terminal
// itself says: who owns it, and what the shell's own OSC 133 marks reported.
//
// The difference between the two is the difference between a PROXY and DIRECT
// evidence, and it decides the order every rule below reads them in. Silence
// is a proxy: a pane that has printed nothing for five minutes has probably
// stopped, and "probably" is the whole of what the staleness half can say. The
// terminal facts state it outright — a shell at a prompt has nothing
// outstanding, and a foreground command that has not returned has not
// finished.
//
// Both halves can decline to answer, and on a real machine they usually do.
// Most shells emit no OSC 133 marks at all, and a pane that has never been
// seen to hand its terminal to a command has not proved that it can — see
// [model.ShellState.AtPrompt], which is latched for a measured reason and whose
// honest answer before that proof is "undetermined". A tier with no direct
// evidence behaves EXACTLY as it did before the port existed.
//
// The tier's status vocabulary is {busy, idle, unknown} and it is mapped
// conservatively. It never returns waiting or shell: a quiet screen is what an
// idle session, a session holding a permission prompt and a session waiting
// for a human to type all look like, and a tier that guessed between them
// would be inventing a fact and labelling it observed.
//
//	the last thing to happen was INPUT       -> unknown. Something was typed and
//	                                            the child has not answered. That is
//	                                            a fact about the human, not the tool.
//	the shell is AT A PROMPT                 -> idle. Direct, and needs no waiting.
//	quiet for less than 5s                   -> busy. Bytes are arriving.
//	quiet, with a command still RUNNING      -> unknown. Direct evidence that it has
//	                                            NOT finished, and none at all about
//	                                            whether it is working or waiting.
//	quiet for 5s or more, nothing else known -> idle. The screen has stopped.
//	no pane, or no instants at all           -> unknown.
//
// 5s is the FIRST RUNG of the existing ladder rather than a new number. It is
// a threshold chosen for a terminal full of agents, where a working Claude
// Code pane animates a spinner and a token counter several times a second, so
// five seconds of true silence from one really does mean it has stopped
// drawing.
//
// The row that CHANGED is the fourth. A silent pane with a command still in
// its foreground used to be reported idle on the strength of the silence
// alone, and that is the wrong "idle" the generic tier exists to avoid:
// `sleep 600`, a linking step, a test run that buffers its output and a
// program sitting at a prompt of its own all look identical to the ladder, and
// a user who is told one of them is idle acts on it.
//
// # 2. Resolution: authoritative wins, and the loser is published
//
//	| Tier          | Source                                           | Confidence    |
//	|---------------|--------------------------------------------------|---------------|
//	| Terminal      | the staleness cascade, tcgetpgrp and OSC 133      | inferred      |
//	| Harness       | Claude Code's own registry and transcript        | authoritative |
//
// Authoritative wins. That was never in doubt: the tool's own record of itself
// beats a guess made from the outside, every time.
//
// The part that matters is what happens to the LOSER. It is published, in
// [model.Session.Conflict], so that a consumer can see that the screen is lying
// rather than merely being overruled silently. That type's own documentation
// explains at length why a conflict is not an error; the rule for RAISING one
// is here:
//
//	A conflict is raised when, and only when, the record says BUSY, the pane
//	has produced no output for a full minute, and the inferred tier concluded
//	IDLE from that.
//
// Three halves of that rule are choices and all three are stated as choices.
//
// ONE MINUTE, not five seconds. A working session goes quiet for short
// stretches legitimately — a tool call, a compile, a network round trip — and a
// conflict raised at the first rung would fire on every session several times a
// minute and mean nothing. One minute is the first rung past the half-minute a
// slow tool call plausibly takes, and it is a rung on the existing ladder
// rather than a fresh number invented for this rule. It is not a measurement
// and it is not presented as one.
//
// ONLY IN THAT DIRECTION. The mirror case — the record says idle while bytes
// are still arriving — is NOT a conflict, and that asymmetry is deliberate.
// Claude Code writes "idle" the moment its turn ends and goes on painting for a
// few hundred milliseconds afterwards, so a symmetric rule would raise a
// conflict against every completed turn on the machine for the following five
// seconds. A conflict that fires on healthy behaviour is worse than no conflict
// at all, because it trains a reader to ignore the field that exists to be
// noticed.
//
// AND THE TIER MUST ACTUALLY DISAGREE. This is the clause the generic tier
// added, and it only ever takes conflicts AWAY. Silence was a proxy for "the
// session has stopped"; a pane whose shell says a command is still running is
// direct evidence that the record's "busy" is right, so there is no
// disagreement to publish and the minute of silence is explained. The reverse
// — letting direct at-prompt evidence raise a conflict FASTER than the minute
// — was considered and rejected at the rule itself, in Screen.contradicts:
// a shell back at its prompt while a record says busy means the tool's process
// is gone, which the liveness gate reports directly and attention rule 1 acts
// on first.
//
// When the record says nothing at all — no status and no raw string — the
// inferred tier wins by default and [model.Session.Confidence] says so. There is
// no loser then, and therefore no conflict: one tier speaking alone is not a
// disagreement.
//
// When the record says something magmux cannot DECODE — a status word from a
// newer Claude than this build knows — the inferred tier does NOT take over.
// The harness spoke; we failed to understand it. Replacing its word with our
// guess would throw away the only truth available, which is the raw string, and
// [model.Session.StatusRaw] is documented as the only source of truth in exactly
// that case.
//
// There is no third tier and there will not be one. See [model.Tier], which
// records the rejection and the reason.
//
// # 3. The attention policy, in full
//
// [model.Attention.Needed] is a *bool with three states, and nil is a real one:
//
//	true   a human should look at this session now.
//	false  determined: we looked, and it does not need anyone.
//	nil    UNDETERMINED. Not a failure state — the honest answer when the
//	       evidence supports neither verdict.
//
// The rules are ordered and the FIRST ONE THAT MATCHES WINS. Reason text is
// free text shown to a human; nothing switches on it.
//
//  1. A LIVENESS GATE SAID NO                     -> false
//     The process has exited, or the pane was closed, or both. There is
//     nobody home to attend to. This is ranked first on purpose: a dead
//     session that was waiting when it died is still dead, and ranking
//     "waiting" above it would keep a tombstone at the top of a list of
//     things to do. An UNDETERMINED gate never matches — "nobody could tell"
//     is not "no", and a session excluded because nobody looked is the exact
//     failure the tri-state exists to prevent.
//
//  2. THE SESSION IS WAITING                      -> true
//     R2.1: waiting is modelled first-class, and it is the point of the
//     feature. The reason distinguishes a permission prompt from waiting on
//     input, because those are different situations with different urgencies
//     and R2 insists the two values stay distinct — a design that collapses
//     them into one boolean throws the distinction away at the last step,
//     which is why the reason carries it even though the verdict cannot.
//     It does NOT wait for a delay to elapse. How long to sit on a
//     notification before interrupting a human is R6's `delay`, which is a
//     policy about people; this is a fact about a session, and a permission
//     prompt needs a human from its first second.
//
//  3. AN API-LEVEL ERROR WAS COUNTED              -> true
//     Acceptance criterion 5: surface an error without opening the pane.
//     A count of zero is a determined zero and does not match; a nil count is
//     "nobody looked" and does not match either. Only errors the tool did NOT
//     recover from are counted, upstream — see the Claude Code middleware,
//     where counting the recovered ones was measured to report 29 of 32
//     healthy sessions as broken. And only RECENT ones: the count covers the
//     last 200 KB of the transcript, so an error the session has since
//     written past stops matching this rule. That is deliberate — this rule
//     interrupts a human, and an error from twenty megabytes ago is not a
//     reason to — and the fault's own detail says how much was examined.
//
//  4. THE TWO TIERS CONFLICT                      -> nil
//     The record says it is working; the screen has said nothing for a
//     minute or more. We published the record's answer because authoritative
//     wins, and we do not know whether that session needs a human. This is
//     the case nil exists for: forcing it to false hides a wedged session,
//     and forcing it to true cries wolf over every long tool call. The
//     reason names both tiers so a human can decide in one glance.
//
//  5. THE SESSION IS BUSY                         -> false
//     It is working and nothing is outstanding.
//
//  6. THE SESSION IS IDLE OR IN A SHELL,
//     ON THE AUTHORITATIVE TIER                    -> false
//     The tool's own record says it has nothing outstanding. That is a real
//     "no": we looked, at the best evidence there is.
//
//  7. THE SHELL IS AT A PROMPT                     -> false
//     The generic tier's one DETERMINED answer, and the only rule direct
//     terminal evidence reaches on its own. It sits above rule 8 because it
//     defeats rule 8's argument rather than sharing it: a quiet screen could
//     be a permission prompt, but a shell that has drawn its prompt has
//     nothing outstanding and cannot be holding one.
//     THE LAST COMMAND'S EXIT STATUS RIDES IN THE REASON AND NOT IN THE
//     VERDICT. A non-zero exit is the only generic signal in this layer that
//     says a command failed rather than merely ended, and making it ask for a
//     human was rejected for one reason: it would be sticky forever. The mark
//     is the last thing that happened in that pane, so a `grep` that matched
//     nothing at 10am would have the pane asking for attention until
//     something else was run in it, and a verdict a user cannot clear by
//     acting on it is not a verdict. See Screen.promptReason.
//
//  8. THE SESSION IS IDLE ON THE INFERRED
//     TIER ALONE                                   -> nil
//     The screen has been quiet and there is no record to say why. A quiet
//     screen is what an idle session, a permission prompt and a session
//     waiting for input all look like, so "it does not need anyone" is a
//     claim the evidence does not support. This is the difference between a
//     verdict and a guess, and it is the rule most worth arguing with.
//
//  9. A COMMAND IS RUNNING AND THE PANE IS SILENT  -> nil
//     Not "nothing is known": one thing is known, and it is exactly the one
//     that would otherwise have been got wrong. Rule 8 would have called this
//     idle on the strength of the silence. What the evidence cannot say is
//     whether that command is working or waiting for a human — vim, a REPL
//     and a linking step are indistinguishable from outside — so the verdict
//     is undetermined and the reason says which of the two questions was
//     answered.
//
//  10. NOTHING IS KNOWN                            -> nil
//     No status from either tier — the record was unreadable, or it carried
//     a word this build does not recognise, in which case the reason quotes
//     the word so that a new Claude release is discoverable rather than
//     invisible. A word we could not decode outranks rule 9: a harness that
//     spoke is more interesting than a terminal fact about the shell
//     underneath it, even when we failed to understand what it said.
//
// Two facts that are NOT in the policy, and one that cannot be:
//
//   - COST and CONTEXT PERCENTAGE. requirements.md R2 lists them. They are not
//     obtainable by this layer and this package does not pretend otherwise:
//     Claude Code writes no statusline record anywhere, it pipes a payload to a
//     command the user configures, and an event on somebody else's stdin is not
//     a file. See findings/cost-and-context-are-out-of-reach.md.
//   - SUBAGENT COUNT does not affect attention. A session with eleven subagents
//     running is busy, which rule 5 already covers, and a count is not an
//     interruption.
//   - HOW LONG a session has been waiting does not affect the verdict, only the
//     reason text. See rule 2.
//
// # Concurrency
//
// No goroutines, no mutexes, no atomics, no timers. A [Stage] is ONE session's
// state and is touched only by that session's actor goroutine. The cascade it
// holds is a plain state machine that never asks what time it is, and the only
// thing this package needs a clock for it does not do itself: it reports how
// long until its own answer changes, through [Stage.WakeIn], and the actor arms
// the one-shot timer.
//
// The one thing shared across sessions is the [ports.PaneActivity] port, which is
// why that port is documented as needing to be safe for concurrent use.
package verdict
