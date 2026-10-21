package act

import (
	"context"
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// Gate is the one way an action reaches an [Actuator].
//
// It holds the policy, the audit trail and nothing else, and it is a
// single-owner value with no lock: in magmux the pipeline's coordinator owns
// it, and that is also the only goroutine that reaches an actor, so the
// serialisation an audit counter needs is already there.
//
// # Why the gate and not the actuator evaluates policy
//
// A middleware cannot approve its own action. Everything an actuator is told
// has already been checked, and the tier the check is made against is
// magmux's OWN filing of the verb (verbTier), which the actuator's declaration
// and the caller's request must both agree with — so an actuator cannot
// escalate by filing a verb under a gentler tier, and a caller cannot escalate
// by naming one. See [Gate.Do], where the order is numbered.
type Gate struct {
	policy Policy
	audit  Auditor
	now    func() time.Time
}

// GateConfig is how a [Gate] is built. A plain value, copied on the way in.
type GateConfig struct {
	// Policy is the posture. The ZERO VALUE REFUSES EVERYTHING, including
	// navigate; a caller that wants requirements.md R6.1's posture passes
	// [Default], and one that has a configuration passes what [Load]
	// returned. There is deliberately no "if empty, use Default" here: a
	// default applied silently is how a struct literal in a test becomes
	// permissive without anybody writing that down.
	Policy Policy

	// Audit is where every action and every refusal is recorded. nil REFUSES
	// every action with [ErrUnaudited] rather than acting silently — R5.A
	// says every action writes a record, so an action that cannot be recorded
	// is not permitted to be one.
	Audit Auditor

	// Now is the clock the `before` state's durations are rendered from. nil
	// takes time.Now. The audit trail stamps its own instants; see
	// [Writer.Append].
	Now func() time.Time
}

// NewGate builds the gate. It creates nothing and opens nothing.
func NewGate(cfg GateConfig) *Gate {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Gate{policy: cfg.Policy.Clone(), audit: cfg.Audit, now: now}
}

// Policy returns a COPY of the posture in force, for a surface that wants to
// show it. Mutating the result changes nothing.
func (g *Gate) Policy() Policy { return g.policy.Clone() }

// Report is everything one request produced: what happened, what the policy
// decided, and every audit line that was written for it.
//
// Records is returned because a surface has to be able to SHOW the trail it
// just wrote — an audit nobody can see is one nobody trusts — and because a
// test can then assert on the record rather than on the file.
type Report struct {
	Result   Result
	Decision Decision
	Records  []Record
}

// Do is the whole of the write path: decide, record, act, record.
//
// # The order, and why each step is where it is
//
//  1. The surface must be named. An action with no [Requester] cannot be
//     audited honestly, and the zero value of a struct literal is exactly
//     that.
//  2. The REQUESTED tier, before any policy is read. First, when magmux's own
//     table knows the verb, the request is held to it: a caller naming any
//     other tier is refused with [ErrTierMismatch], whatever tier it named.
//     Then the requested tier must not be deferred, because a deferral is not
//     a policy decision and reporting it as one would suggest a config could
//     change it. This is what refuses a lifecycle request on a magmux whose
//     config says `lifecycle = "always"`. The table comes FIRST since fix
//     round 2. Before, `focus` requested as lifecycle was refused as DEFERRED
//     before the table was read, so the trail filed a focus under lifecycle —
//     misreporting exactly the case the table exists for — and the caller was
//     told "R5.4 is deferred", which implies shipping R5.4 would let it run.
//     It would not: focus is a navigate action. For a verb the table does NOT
//     know there is no entry to contradict the caller, so the deferral is
//     still the answer, and still given here.
//  3. The verb must be DECLARED for this session. Not declared is
//     [ErrNotCapable] — "cannot", not "will not" — and a nil target is the
//     same answer: there is nothing here that could do it.
//  4. The DECLARED tier must not be deferred either. Then the declaration is
//     held against magmux's own closed table of verbs (verbTier): a verb the
//     table does not contain is [ErrNotCapable], and a declaration filing it
//     under a different tier than the table does is [ErrTierMismatch]. The
//     TABLE is the authority, not the actuator — an actuator that declares
//     `restart` as navigate is refused here rather than run, because
//     `restart` is not a verb this build knows. (The caller's tier was held to
//     the same table at step 2, so by here all three agree.) Until fix round 1
//     this step checked only the caller against the declaration, and this
//     comment claimed the mislabelled `restart` case anyway. It ran. The
//     direction that had never been tested is
//     TestAnActuatorCannotRunADangerousVerbByFilingItUnderABenignTier.
//  5. Policy decides, against the table's tier. `ask` returns
//     [ErrNeedsConfirmation] and performs nothing — the surface that asked
//     obtains consent, because nothing in this layer may prompt.
//  6. The attempt is recorded BEFORE the action runs. If it cannot be
//     recorded the action does NOT happen ([ErrUnaudited]). That has a real
//     cost — an unwritable state directory makes focusing a pane impossible —
//     and it is the right side of the trade: an action nobody can see is
//     exactly what the trail exists to prevent, and the failure is loud and
//     easy to fix.
//  7. The action runs.
//  8. The outcome is recorded, referring back to the attempt's Seq; see
//     [Record.Ref] for why it is a reference rather than a shared number.
//
// # Every refusal is a refusal
//
// There is no path out of here that declines to act and returns a nil error.
// A caller that asked for a restart and got silence cannot tell that from a
// restart that happened.
//
// # Every path writes a record
//
// Refusals included — they are the interesting half of a trail. The only
// exception is a context that was already cancelled when Do was called, which
// is a caller that went away rather than an action anybody decided about.
func (g *Gate) Do(ctx context.Context, req Request, before obs.Session, target Actuator) (Report, error) {
	a := req.Action
	rep := Report{}

	// The tier magmux's own table files the verb under, resolved before
	// anything else: every record this request writes — every refusal
	// included, from step 1 on — carries the TABLE's tier when the table knows
	// the verb. A record that repeated the caller's word, or an actuator's,
	// would misreport exactly the case the table exists for. For a verb the
	// table does not know, the caller's tier is the only tier there is, until
	// a declaration supplies one at step 3.
	filed, known := verbTier(a.Verb)
	rec := Record{
		Verb:   a.Verb,
		Tier:   a.Tier,
		By:     req.By,
		Target: targetOf(before, a.Session),
	}
	if known {
		rec.Tier = filed
	}
	state := StateOf(before, g.now())
	rec.Before = &state

	// 1. The surface.
	if req.By == RequesterUnknown {
		return g.refuse(rep, rec, Decision{Rule: "refused in code: an action must name the " +
			"surface it came from, so that the trail can say who asked"},
			refusalf(ErrNoRequester, "the request names no surface"))
	}

	// 2. The requested tier, before any policy is consulted: held to the table
	// when the table knows the verb, then checked for deferral.
	if known && a.Tier != filed {
		return g.refuse(rep, rec, Decision{Rule: fmt.Sprintf("refused in code: the request named "+
			"%s, and %q is filed under the %s tier in magmux's own table", tierWords(a.Tier),
			a.Verb, filed)},
			refusalf(ErrTierMismatch, "%q is a %s action, not %s", a.Verb, filed, tierWords(a.Tier)))
	}
	if reason, deferred := a.Tier.Deferred(); deferred {
		return g.refuse(rep, rec, Decision{Rule: "hard-disabled in code: " + reason},
			refusalf(ErrDeferred, "%s cannot run: %s", verbText(a), reason))
	}

	// 3. The declaration.
	if a.Verb == "" {
		return g.refuse(rep, rec, Decision{Rule: "refused in code: an action must name a verb"},
			refusalf(ErrNotCapable, "the request names no verb"))
	}
	capab, found := capabilityFor(target, a.Verb)
	if !found {
		return g.refuse(rep, rec, Decision{Rule: fmt.Sprintf("refused in code: no capability "+
			"named %q is declared for this session", a.Verb)},
			refusalf(ErrNotCapable, "nothing declares %q for session %s, so nothing can perform it",
				a.Verb, a.Session))
	}
	// A verb the table does not know is recorded under its DECLARATION from
	// here on, rather than the caller's word; one the table knows already
	// carries the table's tier, and keeps it.
	if !known {
		rec.Tier = capab.Tier
	}

	// 4. The declared tier, then the declaration against the table. The
	// caller was held to the table at step 2.
	if reason, deferred := capab.Tier.Deferred(); deferred {
		return g.refuse(rep, rec, Decision{Rule: "hard-disabled in code: " + reason},
			refusalf(ErrDeferred, "%s cannot run: %s", verbText(a), reason))
	}
	if !known {
		return g.refuse(rep, rec, Decision{Rule: fmt.Sprintf("refused in code: %q is not in "+
			"magmux's table of verbs, so no declaration can make it runnable — it was declared "+
			"as %s", a.Verb, capab.Tier)},
			refusalf(ErrNotCapable, "%s cannot run: %q is not a verb this build of magmux knows, "+
				"whatever tier an actuator files it under", verbText(a), a.Verb))
	}
	if capab.Tier != filed {
		return g.refuse(rep, rec, Decision{Rule: fmt.Sprintf("refused in code: %q is filed "+
			"under the %s tier in magmux's own table, and the actuator declared it %s",
			a.Verb, filed, capab.Tier)},
			refusalf(ErrTierMismatch, "%q is a %s action; an actuator declaring it %s is refused "+
				"rather than believed", a.Verb, filed, capab.Tier))
	}

	// 5. Policy.
	dec := g.policy.Decide(filed, before)
	rep.Decision = dec
	switch {
	case dec.Confirm:
		return g.refuse(rep, rec, dec,
			refusalf(ErrNeedsConfirmation, "%s needs confirmation (%s)", verbText(a), dec.Rule))
	case !dec.Allowed:
		return g.refuse(rep, rec, dec,
			refusalf(ErrForbidden, "%s is not allowed (%s)", verbText(a), dec.Rule))
	}

	// A caller that has already gone is not an action to record.
	if err := ctx.Err(); err != nil {
		return rep, err
	}

	// 6. The attempt, before anything happens.
	rec.Event = EventAttempt
	rec.Policy = dec
	attempt, err := g.append(rec)
	if err != nil {
		return rep, refusalf(ErrUnaudited, "%s was not attempted: %v", verbText(a), err)
	}
	rep.Records = append(rep.Records, attempt)

	// 7. The action, under the table's tier — which the declaration and the
	// request have both just been shown to agree with.
	performed := a
	performed.Tier = filed
	res, doErr := target.Do(ctx, performed)
	rep.Result = res

	// 8. The outcome, under the attempt's own sequence number.
	out := Outcome{Done: res.Done, Detail: res.Detail}
	if doErr != nil {
		out.Error = doErr.Error()
	}
	outRec := attempt
	outRec.Event = EventOutcome
	// Ref points back at the attempt. The writer stamps a fresh Seq, because
	// the counter identifies the LINE and the writer is the only thing
	// entitled to set it; see [Record.Ref].
	outRec.Ref = attempt.Seq
	// The state is on the attempt line; repeating it would double the bytes
	// of the trail to say nothing new.
	outRec.Before = nil
	outRec.Outcome = &out
	written, auditErr := g.append(outRec)
	if auditErr == nil {
		rep.Records = append(rep.Records, written)
	}

	switch {
	case doErr != nil && auditErr != nil:
		return rep, fmt.Errorf("%w (and the outcome could not be recorded: %v)", doErr, auditErr)
	case doErr != nil:
		return rep, doErr
	case auditErr != nil:
		// The action HAPPENED. Reporting it as an error is the conservative
		// reading and it is deliberate: a trail with attempts and no outcomes
		// is a broken trail and has to be visible. Report.Result still says
		// what was done, so a caller that needs to know can tell.
		return rep, fmt.Errorf("%s was performed but its outcome could not be recorded: %w",
			verbText(a), auditErr)
	}
	return rep, nil
}

// refuse records a refusal and returns it. Every early return above goes
// through it, so a refusal cannot come to exist without a line in the trail.
//
// An audit failure does not hide the refusal: the action was not going to
// happen either way, and the sentinel stays matchable with errors.Is so a
// caller branching on the class still works.
func (g *Gate) refuse(rep Report, rec Record, dec Decision, refusal error) (Report, error) {
	rep.Decision = dec
	rec.Event = EventRefused
	rec.Policy = dec
	rec.Outcome = &Outcome{Refused: true, Error: refusal.Error()}
	written, err := g.append(rec)
	if err != nil {
		return rep, fmt.Errorf("%w (and the refusal could not be recorded: %v)", refusal, err)
	}
	rep.Records = append(rep.Records, written)
	return rep, refusal
}

// append writes one record through the auditor, and refuses when there is no
// auditor at all.
func (g *Gate) append(r Record) (Record, error) {
	if g.audit == nil {
		return r, fmt.Errorf("there is no audit trail to record it in")
	}
	return g.audit.Append(r)
}

// capabilityFor finds the declaration for a verb. A nil target declares
// nothing, which is the honest answer for a session whose chain has no write
// half at all.
func capabilityFor(target Actuator, verb string) (Capability, bool) {
	if target == nil {
		return Capability{}, false
	}
	for _, c := range target.Capabilities() {
		if c.Verb == verb {
			return c, true
		}
	}
	return Capability{}, false
}

// targetOf records which session an action was aimed at. The session id comes
// from the ACTION rather than from the fact set, because an action aimed at a
// session nobody could observe must still say what it was aimed at.
func targetOf(s obs.Session, sessionID string) Target {
	t := Target{SessionID: sessionID, PID: s.PID, Name: s.Name}
	if t.SessionID == "" {
		t.SessionID = s.ID
	}
	if s.PaneID != nil {
		v := *s.PaneID
		t.PaneID = &v
	}
	return t
}

// tierWords names a requested tier in a sentence: "the lifecycle tier", or
// "no tier" for the zero value, whose display name is not a tier anybody
// asked for.
func tierWords(t Tier) string {
	if t == TierUnknown {
		return "no tier"
	}
	return "the " + t.String() + " tier"
}

// verbText is how an action is named in a sentence a human reads.
func verbText(a Action) string {
	if a.Session == "" {
		return fmt.Sprintf("%q", a.Verb)
	}
	return fmt.Sprintf("%q on session %s", a.Verb, a.Session)
}
