package act

import (
	"context"
	"errors"
	"fmt"
)

// Tier is which of requirements.md R6's four `[actions]` classes an action
// belongs to. It is the unit POLICY is expressed in: a stance is configured
// per tier, never per verb, so a verb that is added later is governed by a
// line the user has already written.
//
// # There are two Tiers in this layer and they are not related
//
// obsview.Tier (the wire's spelling of claudestate's model.Confidence) is a CONFIDENCE tier — inferred or authoritative — and says where a
// FACT came from. This one is an ACTION tier and says how dangerous a CHANGE
// is. They share a name because both requirements.md and the architecture
// amendment call them that, and renaming one here would put this package's
// vocabulary out of step with the documents it implements. Nothing converts
// between them and nothing should.
//
// # Three of the four cannot execute
//
// [TierNavigate] is built. The other three are hard-disabled; see
// [Tier.Deferred] and the package doc.
type Tier uint8

const (
	// TierUnknown is the zero value: no tier. It is what a struct literal
	// that names no tier has, it matches no capability, and every stance
	// lookup for it refuses. See the package doc on why the zero value of
	// everything here is the safe answer.
	TierUnknown Tier = iota

	// TierNavigate is R5.1: move the VIEW. Focus a pane, and nothing beyond
	// that — no signal, no keystroke, no file. It is the only tier that can
	// execute in this build, and the only one R6.1's default posture allows
	// without asking.
	TierNavigate

	// TierApprove is R5.2: answer a permission prompt on a live agent's
	// behalf. Deferred.
	TierApprove

	// TierSend is R5.3: put text into a live agent's prompt. Deferred.
	TierSend

	// TierLifecycle is R5.4: restart, kill or resume a session. Deferred, and
	// the one of the three that is IRREVERSIBLE.
	TierLifecycle
)

var tierEnum = enumSpec{"Tier", []string{
	"unknown",
	"navigate",
	"approve",
	"send",
	"lifecycle",
}}

// String returns the wire word.
func (t Tier) String() string { return tierEnum.name(int(t)) }

// MarshalJSON writes null for [TierUnknown] and the wire word otherwise.
func (t Tier) MarshalJSON() ([]byte, error) { return tierEnum.marshal(int(t)) }

// UnmarshalJSON reads null as [TierUnknown]. An unrecognised name is an error.
func (t *Tier) UnmarshalJSON(b []byte) error {
	i, err := tierEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*t = Tier(i)
	return nil
}

// Deferred reports whether this tier is hard-disabled in this build, and why.
//
// # This function IS the hard disable
//
// It is consulted by [Gate.Do] before any policy is read and again after the
// verb's own declaration is resolved, so a tier cannot be enabled by a config
// file, by a caller passing a different tier, by a rule matching, or by a
// middleware declaring a capability for it. "Disabled because nothing calls
// it" is not disabled; this is a refusal with a reason, delivered to whoever
// asked.
//
// The reason NAMES the requirement it defers and what it would act on, because
// a refusal that says only "not allowed" tells an operator nothing about
// whether to wait for a release, edit a config, or do it by hand.
//
// # Removing a line here is how a tier ships
//
// Deliberately: the diff that enables R5.3 is a deleted case in this switch
// plus an actuator that implements it, and it is one file and one reviewer
// rather than a search for every place a tier is mentioned.
func (t Tier) Deferred() (reason string, deferred bool) {
	switch t {
	case TierApprove:
		return "R5.2 (answering permission prompts) is deferred: approving on a live agent's " +
			"behalf needs an allowlist the user has not written", true
	case TierSend:
		return "R5.3 (sending input) is deferred: injecting text into a live agent's prompt " +
			"needs an allowlist the user has not written", true
	case TierLifecycle:
		return "R5.4 (restart, kill, resume) is deferred: it acts on live agents and it is " +
			"IRREVERSIBLE, and two recorded incidents destroyed 16 sessions each", true
	}
	return "", false
}

// Requester is the SURFACE an action arrived through. The set is closed
// because sinks are barred from actuation — nothing that merely receives a
// document can act — so every action in the trail came through a surface a
// human invoked, and the audit can say which.
type Requester uint8

const (
	// RequesterUnknown is the zero value, and it REFUSES. An action whose
	// surface cannot be named cannot be audited honestly, and R5.A asks the
	// trail to say who asked.
	RequesterUnknown Requester = iota
	// RequesterCLI is a person at a terminal running magmux.
	RequesterCLI
	// RequesterSocket is magmux's own unix socket.
	RequesterSocket
	// RequesterMCP is an MCP client, which reaches magmux through the socket
	// as well; the extra name is worth keeping because the two have different
	// operators behind them.
	RequesterMCP
)

var requesterEnum = enumSpec{"Requester", []string{
	"unknown",
	"cli",
	"socket",
	"mcp",
}}

// String returns the wire word.
func (r Requester) String() string { return requesterEnum.name(int(r)) }

// MarshalJSON writes null for [RequesterUnknown] and the wire word otherwise.
func (r Requester) MarshalJSON() ([]byte, error) { return requesterEnum.marshal(int(r)) }

// UnmarshalJSON reads null as [RequesterUnknown]. An unrecognised name is an
// error.
func (r *Requester) UnmarshalJSON(b []byte) error {
	i, err := requesterEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*r = Requester(i)
	return nil
}

// VerbFocus is the only verb that can execute in this build: move keyboard
// focus to the pane a session is running in.
const VerbFocus = "focus"

// verbTier is magmux's OWN classification of every verb that may reach an
// actuator, and it is CLOSED: a verb absent from this switch cannot run,
// whatever any actuator declares for it.
//
// # Why the declaration alone was not enough
//
// [Gate.Do] used to take a verb's tier from the target's own
// [Actuator.Capabilities] and check nothing else about it. That refused a
// CALLER who named a gentler tier, and it did not refuse an ACTUATOR that did:
// a middleware declaring {Verb: "restart", Tier: TierNavigate} passed every
// step and ran under the navigate stance, which [Default] allows without
// asking. The gate's comment and CLAUDE.md both said that case was refused.
// A reviewer confirmed it was not with a throwaway test, and
// TestAnActuatorCannotRunADangerousVerbByFilingItUnderABenignTier is that test
// kept.
//
// So the tier of a verb is not the actuator's to state. It is this table's,
// and the gate refuses a declaration that disagrees with it or names a verb it
// does not contain.
//
// # Adding a verb is an edit here, and that is the point
//
// The same shape as [Tier.Deferred]: one switch, one file, one reviewer. The
// verb that sends Ctrl-C or clears a prompt — the one somebody will think of
// as "just a nudge" — cannot be filed under navigate by the person writing its
// actuator; it has to be filed here, in a diff a reviewer of this package
// sees, beside the three tiers that are hard-disabled and the reason they are.
//
// There is deliberately no entry for a deferred tier's verbs. Nothing
// implements them, and the name R5.4's restart will have is a decision for
// whoever builds it — a vocabulary invented ahead of its design is the one
// this layer has always called guaranteed to be wrong. An undeclared
// name is refused here as unknown, which is the same answer those tiers get
// from [Tier.Deferred] by every other route.
//
// # What this does NOT guarantee
//
// It ties a verb's NAME to a tier. It cannot tell whether an actuator's
// [Actuator.Do] does what the name says: an actuator that sends a keystroke
// under the verb "focus" passes this check. Code review of the actuator is the
// defence against that, and a table is not a substitute for it.
//
// [VerbNotify] is not here, on purpose. A notification never passes through
// the gate, so an ACTION calling itself "notify" is refused as unknown.
func verbTier(verb string) (Tier, bool) {
	switch verb {
	case VerbFocus:
		return TierNavigate, true
	}
	return TierUnknown, false
}

// VerbNotify is the one thing in the audit trail that is not an [Action].
//
// A notification is outward-facing — magmux interrupting a HUMAN — so
// requirements.md R5.A's four questions all have answers for it and it is
// recorded on the same terms as an action. It is not one, and the record says
// so in two places rather than one: `tier` is null, because it belongs to none
// of R6's four action classes and changes nothing on any session, and
// `requestedBy` is null, because nothing asked for it.
//
// # The one shape that collides, stated rather than glossed
//
// On every path a caller can reach from outside this process, those two nulls
// are unreachable on an ACTION line: [Gate.Do] refuses an unnamed requester at
// step 1 and [TierUnknown] matches no capability. There is one path inside the
// process where they are reachable together. Gate.Do builds its record BEFORE
// step 1, so a caller passing THIS verb with no surface and no tier is refused
// at step 1 and the refusal carries verb "notify", a null tier and a null
// requestedBy — a suppressed notification's shape exactly, down to
// event=refused. The two then differ only in the prose of `policy.rule`.
//
// It needs a programming error in magmux to produce: the reserved verb and an
// unnamed surface in one call. Nothing outside the process can reach Gate.Do.
// So the defect is in the CLAIM, not in behaviour anyone can trigger, and this
// is the claim corrected rather than repeated.
//
// TestAReservedVerbWithNoSurfaceWearsANotificationsShape pins it, and fails the
// moment a structural discriminator lands — a dedicated requester for the
// policy, a distinct event for a notice, or refusing this verb before the record
// is built. Each is a change to the trail's schema, so it is a decision rather
// than a fix to slip in.
//
// It never passes through [Gate.Do]. There is nothing for a stance to decide:
// R6 governs notifications through its `[notify]` block, and [Notify] is where
// that lives.
const VerbNotify = "notify"

// Action is ONE thing to do to ONE session.
//
// Verb is a string and Tier is an enum, on purpose. A tier is magmux's own
// classification and the thing a user writes policy about; a verb names an
// operation, and new ones arrive with new middleware. What a verb may NOT do is
// arrive with its own classification: the gate looks the verb up in the
// target's declaration AND in magmux's own closed table (verbTier), and a verb
// the table does not contain, or a declaration that files it under a different
// tier, is refused. Policy is never switched on a verb — only on the tier the
// table gives it.
type Action struct {
	// Verb names the operation. It must match a [Capability] the target
	// declares for this session; an undeclared verb is refused with
	// [ErrNotCapable] rather than attempted.
	Verb string

	// Tier is what the CALLER believes the verb's tier is. It is checked
	// against the declaration rather than trusted: see [Gate.Do] step 4.
	Tier Tier

	// Session is the tool's own session id, which is the key an actor is
	// routed by.
	Session string
}

// Request is an [Action] plus who asked for it. The two are separate types
// because the action is what happens and the requester is a fact about the
// audit trail, and folding them together would invite a middleware to read the
// surface and behave differently for one of them.
type Request struct {
	Action Action
	By     Requester
}

// Capability is one thing a middleware declares it can do TO THE SESSION IT
// BELONGS TO.
//
// # Declaration is not permission
//
// Declaring is how a capability becomes visible to the user, to the config and
// to the audit trail; the policy still decides whether it may run. An actuator
// that could act without declaring would be an ungoverned back door, which is
// why the gate resolves the verb through this list and refuses anything absent
// from it.
//
// # It is per session, and "cannot" is not "will not"
//
// A stage is built per session (see claudestate's stage.Factory), so this list is
// already about one session and needs no target parameter. A capability that
// cannot be performed FOR THIS SESSION is not declared for it — a session with
// no pane cannot be navigated to, and saying so by omission is different from
// declaring it and refusing, which would read as policy rather than as
// absence.
type Capability struct {
	// Verb is the operation's name, matched against [Action.Verb].
	Verb string

	// Tier is what the actuator says the operation's classification is. It is
	// CHECKED, not trusted: [Gate.Do] refuses a declaration that disagrees
	// with magmux's own table of verbs (see verbTier), so a middleware that
	// declares a lower tier than its verb deserves is refused rather than run
	// — TestAnActuatorCannotRunADangerousVerbByFilingItUnderABenignTier. See
	// also TestADeferredTierIsRefusedByITSOWNDECLARATION, where a `restart`
	// requested as a navigate is refused by its own declaration, and
	// TestATierMismatchIsRefusedRatherThanReclassified for the case with no
	// deferral in it.
	Tier Tier

	// Reversible says whether the effect can be undone. A caller needs it
	// before acting, not after.
	Reversible bool

	// Idempotent says whether repeating the operation is safe. A blind retry
	// of a non-idempotent restart starts two sessions.
	Idempotent bool

	// Describe is shown in the audit trail and in any confirmation, in
	// magmux's own words. Free text; never switched on.
	Describe string
}

// Result is what an [Actuator] did.
//
// There is deliberately no Undo field, and the v3 amendment's `Undo *Action`
// is not implemented. An undo for the one live verb would have to address a
// PANE — "put focus back where it was" — while an [Action] addresses a
// SESSION, and inventing a second addressing mode for one deferred feature is
// how a vocabulary goes wrong. What was displaced is recorded in Detail and
// therefore in the audit trail, which is what an operator needs in order to
// put it back by hand.
type Result struct {
	// Done is whether the operation happened. False with a nil error is not a
	// legal combination out of [Gate.Do]; a refusal is always an error.
	Done bool

	// Detail is what happened, in magmux's own words, and is the audit
	// trail's account of the effect. Free text.
	Detail string
}

// Actuator is the OPTIONAL write half of a middleware, discovered by type
// assertion in exactly the way magmux's own InputNotifier is discovered beside
// ToolController: a middleware that only enriches does not implement it and
// must not be forced to pretend.
//
// magmux reaches it through pipeline.Ops.Call, which runs on the session's
// actor and finds the stage by name (mux's actuatorOver): the library knows
// nothing of this interface, and needs to know nothing.
//
// # Both methods run on the ACTOR's goroutine
//
// The same contract Middleware.Observe carries, for the same reason. An
// implementation MAY do I/O — that is what the actor exists for, so that a
// write never happens under magmux's locks or in its render loop — and MUST
// NOT block indefinitely. One actor processes one message at a time, so a
// [Actuator.Do] that never returns wedges that session's actor and everything
// waiting on a reply from it, which today includes the coordinator and
// therefore every other session's next cycle. Bound the wait and report the
// absence of an answer as a failed [Result] with an error.
//
// # It is never reached without authorisation
//
// [Gate.Do] resolves the capability, evaluates the stance and writes the audit
// record before calling Do. An implementation therefore does NOT re-check
// policy, and must not: a middleware that approved its own action would be a
// second policy nobody configured.
type Actuator interface {
	// Capabilities declares what can be done to this middleware's session, as
	// of now. It is called on the actor's goroutine, must not block, and may
	// return nothing at all — which is the honest answer for a session whose
	// pane could not be resolved.
	Capabilities() []Capability

	// Do performs ONE action. It is reached only after the gate has
	// authorised it.
	//
	// ctx carries the caller's cancellation. An implementation that waits on
	// anything must select on it, because the caller is blocked on this
	// return.
	Do(ctx context.Context, a Action) (Result, error)
}

// The refusal sentinels. Each is wrapped with the specific reason, so a caller
// can branch on the class with errors.Is and still show a human the sentence.
var (
	// ErrDeferred is a tier that is hard-disabled in this build. It is
	// returned whatever the policy says.
	ErrDeferred = errors.New("this action tier is deferred and cannot execute")

	// ErrNotCapable is a verb the target does not declare for this session,
	// or one magmux's own table of verbs does not contain. It means "cannot",
	// not "will not": nothing was refused on policy grounds, there is simply
	// nothing in this build that may do it.
	ErrNotCapable = errors.New("nothing can perform this action for this session")

	// ErrTierMismatch is a tier that disagrees with magmux's own filing of
	// the verb: an actuator declaring it under a different tier, or a caller
	// naming a different one. The table wins and the call is refused rather
	// than silently reclassified.
	ErrTierMismatch = errors.New("the requested tier is not the tier this verb is declared under")

	// ErrForbidden is policy: the stance for this tier does not allow it, or
	// allows it only by a rule and no rule matched.
	ErrForbidden = errors.New("policy does not allow this action")

	// ErrNeedsConfirmation is the "ask" stance. The action was NOT performed.
	// The surface that asked must obtain consent and ask again; nothing in
	// this layer ever prompts, because a background pipeline that can raise a
	// prompt can be socially engineered into raising one.
	ErrNeedsConfirmation = errors.New("this action needs confirmation from whoever asked for it")

	// ErrUnaudited is an audit record that could not be written. The action
	// did NOT happen: R5.A says every action is recorded, and an action that
	// cannot be recorded is not permitted to be one. See [Gate.Do] step 6,
	// where the cost of that choice is stated.
	ErrUnaudited = errors.New("the action was not attempted because it could not be audited")

	// ErrNoRequester is an action whose surface was not named. The zero
	// [Requester] refuses for the same reason the zero [Stance] does.
	ErrNoRequester = errors.New("the action names no requesting surface, so it cannot be audited")
)

// refusal is a sentence for a human that classifies as a sentinel for a
// program.
//
// It is a type rather than fmt.Errorf("%s: %w", msg, sentinel) because that
// shape prints BOTH — "nothing can \"focus\" this session: nothing can perform
// this action for this session" — and the sentinel is the vaguer half of a
// sentence whose specific half is already there. Unwrap keeps errors.Is
// working, so a caller still branches on the class; only the duplication goes.
type refusal struct {
	msg      string
	sentinel error
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return r.sentinel }

// refusalf builds one. Every refusal in this package goes through it, so a
// caller can rely on both halves existing.
func refusalf(sentinel error, format string, args ...any) error {
	return &refusal{msg: fmt.Sprintf(format, args...), sentinel: sentinel}
}
