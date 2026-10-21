// Package act is the observation layer's WRITE half: the vocabulary for an
// action, the policy that decides whether it may run, the gate that enforces
// that decision, and the audit trail that records it either way.
//
// Everything else under internal/obs observes. This package is the first thing
// in the layer that changes anything, and almost all of it is machinery for
// NOT changing things.
//
// # Scope: one tier of four is live, and the other three cannot execute
//
// requirements.md R5 names four action tiers. Exactly one of them is built:
//
//   - [TierNavigate] (R5.1) — focus a pane. It touches the VIEW and nothing
//     else: no process is signalled, no file is written, no text reaches any
//     child, and the worst outcome of getting it wrong is that a human is
//     looking at the wrong pane.
//   - [TierApprove] (R5.2), [TierSend] (R5.3) and [TierLifecycle] (R5.4) are
//     named in the schema and HARD-DISABLED in code. They cannot execute: not
//     when a config asks for them, not when a caller requests them, not when a
//     rule matches, and not when a middleware declares a capability for them.
//     See [Tier.Deferred], which is the single place that refusal is written,
//     and TestEveryDeferredTierRefusesEvenWhenEverythingSaysAlways, which
//     proves it against a middleware that declares and implements all four.
//
// The reason is two recorded incidents rather than caution. A `tmux run-shell`
// whose client had gone away segfaulted the tmux server and killed all 16
// sessions on it (2026-08-16), and a test server that read the user's own tmux
// config autoloaded a session-restore plugin and destroyed 16 sessions, 44
// windows and 71 panes (2026-08-17). Those three tiers act on live agents —
// thirty of them on the machine this was built for — and R5.4 is
// irreversible. requirements.md §1 records the decision in one sentence:
// "acting on 30+ live agents unattended is not a judgment call to make on his
// behalf."
//
// Adding a tier later is a new type satisfying [Actuator], a deleted line in
// [Tier.Deferred], and its verbs filed in verbTier, magmux's closed table of
// verbs. It is not a change to the gate, the audit trail, the actor or any
// middleware, and that is the whole point of building the path before the
// verbs that will use it.
//
// # A refusal is a refusal, never a silent no-op
//
// Every path out of [Gate.Do] that does not act returns an error naming what
// refused and why. A caller that asked for a restart and got a nil error and
// an empty result cannot tell that from a restart that happened, and the first
// thing anyone does with a silent failure is do it again. The same rule covers
// "ask": [ErrNeedsConfirmation] is returned TO THE CALLER, and the surface
// that asked — a CLI, an MCP client — is what obtains consent. Nothing in this
// layer ever raises a prompt of its own; a background pipeline that can put a
// modal in front of a human is a pipeline that can be socially engineered.
//
// # The gate is evaluated by the caller of the actuator, never by the actuator
//
// A middleware cannot approve its own action. [Gate.Do] resolves the
// capability, resolves the stance, writes the audit record and only then calls
// [Actuator.Do]; a middleware that is reached has already been authorised.
// Policy is evaluated against the tier magmux itself files the verb under
// (verbTier, a closed switch), and both the actuator's declaration and the
// caller's request must agree with it — so an actuator cannot escalate by
// filing a dangerous verb under a benign tier, and a caller cannot escalate by
// naming one. See [Gate.Do]'s ordering, where each of those checks is numbered
// and has its own test.
//
// # The audit trail does not live under ~/.claude, and cannot be made to
//
// NFR1 is absolute: magmux never writes under ~/.claude, including lockfiles
// and temporary files, because thirty live Claude Code processes own that tree
// and none of them expects a foreign writer. The audit log therefore lives at
// ${XDG_STATE_HOME:-~/.local/state}/magmux/sessions-audit.jsonl, and
// [NewWriter] REFUSES a path with a .claude element in it rather than trusting
// the default to be the only path anyone ever passes. See [Writer].
//
// # The policy is a struct, and the FILE arrived in phase 7 as JSON
//
// requirements.md R6 sketches the config in TOML, and TOML is the better
// format for it — a file whose whole purpose is recording rules a human wrote
// and will want to annotate, and JSON has no comments. The file is JSON
// anyway, on REVERSIBILITY grounds and not on merit: a TOML library is a
// FOURTH module against a set pinned at three by
// TestDependencySetIsExactlyWhatWasDecided, that pin exists so a fourth is a
// deliberate decision in a diff that says so, and that decision is the user's
// and has not been made. JSON-now-TOML-later is one file against an unchanged
// schema; a dependency added without the decision has already defeated the
// gate. [FileLoader] states the whole of it where the format is.
//
// The split phase 6 established is what made that cheap, and it stands: a
// schema is the SHAPE of the data, not its serialisation. [Policy] carries all
// four tiers and the notify block as a plain Go value, [Loader] is the one
// seam a format has to fill, and nothing that DECIDES anything reads a file.
//
// R6.3 — "an unparseable or absent config yields the default posture, never a
// more-permissive one" — is therefore a property of the LOADER rather than of
// a parser: see [Load], which returns [Default] for every failure and for no
// loader at all, and returns the error beside it so that a degradation is
// never silent. [FileLoader] adds the two concrete cases underneath it — an
// absent file is [Default] and NO error, because that is the normal state of
// an unconfigured machine, and an unparseable one is [Default] WITH an error,
// because the user wrote something that is not in force.
//
// # The zero value is the SAFE posture, and that is the one thing to check
//
// Every unit test in this repository builds its structs as literals, so a
// permissive zero value would be invisible in exactly the tests that would
// otherwise catch it: every existing test would keep passing while
// `act.Policy{}` quietly authorised a lifecycle action. So [StanceUnknown] is
// index 0 and refuses, [TierUnknown] is index 0 and matches no capability,
// [RequesterUnknown] is index 0 and refuses, and
// TestTheZeroPolicyRefusesEverythingIncludingNavigate asserts the whole of it
// directly rather than as a side effect of something else.
//
// # Ownership
//
// No goroutines, no mutexes, no atomics — the same rule as the rest of the
// layer. [Gate] and [Writer] are single-owner values: in magmux they are owned
// by the pipeline's coordinator goroutine, which is also the only goroutine
// that reaches an actor. A second owner would need synchronisation, so there
// is not one, and the doc comment on each type says so where somebody would
// otherwise have to guess.
package act
