package act

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/obsview"
)

// ── test doubles ────────────────────────────────────────────────────────────

// fakeActuator declares whatever it is told to declare and RECORDS EVERY CALL.
// The recording is the point: a tier that is refused must leave this empty,
// and "the actuator was never reached" is the only way to tell a refusal from
// a silent success.
type fakeActuator struct {
	caps   []Capability
	calls  []Action
	result Result
	err    error
}

func (f *fakeActuator) Capabilities() []Capability { return f.caps }

func (f *fakeActuator) Do(_ context.Context, a Action) (Result, error) {
	f.calls = append(f.calls, a)
	return f.result, f.err
}

// everyVerb is an actuator that declares and implements ALL FOUR tiers,
// including the three that are deferred.
//
// It exists because "hard-disabled" has to mean more than "nothing calls it".
// A tier that is refused only because no middleware implements it is one
// middleware away from being enabled by accident, and the test below proves
// the refusal survives a middleware that is eager to do the work.
func everyVerb() *fakeActuator {
	return &fakeActuator{
		caps: []Capability{
			{Verb: VerbFocus, Tier: TierNavigate, Describe: "focus a pane"},
			{Verb: "approve", Tier: TierApprove, Describe: "answer a permission prompt"},
			{Verb: "send_input", Tier: TierSend, Describe: "type into the prompt"},
			{Verb: "restart", Tier: TierLifecycle, Describe: "restart the session"},
		},
		result: Result{Done: true, Detail: "the fake did it"},
	}
}

// fakeAuditor keeps the records in memory. It stamps like the real writer so
// that a test can assert on sequence numbers without a filesystem.
type fakeAuditor struct {
	records []Record
	seq     int
	err     error
}

func (f *fakeAuditor) Append(r Record) (Record, error) {
	if f.err != nil {
		return r, f.err
	}
	f.seq++
	r.Seq = f.seq
	r.PID = 7777
	r.At = auditNow
	f.records = append(f.records, r)
	return r, nil
}

func (f *fakeAuditor) events() []Event {
	out := make([]Event, 0, len(f.records))
	for _, r := range f.records {
		out = append(out, r.Event)
	}
	return out
}

// permissive is a policy that says yes to everything. It is what a user who
// had read the schema and wanted no friction would write, and three quarters
// of it must have no effect whatever.
func permissive() Policy {
	return Policy{
		Source: "a policy that says always to everything",
		Actions: Actions{
			Navigate:  StanceAlways,
			Approve:   StanceAlways,
			Send:      StanceAlways,
			Lifecycle: StanceAlways,
		},
	}
}

func testSession() model.Session {
	return model.Session{Key: model.Key{SessionID: "id-a", PID: 4242}, PID: 4242, Name: "circl", Pane: "2"}
}

func request(verb string, tier Tier) Request {
	return Request{
		Action: Action{Verb: verb, Tier: tier, Session: "id-a"},
		By:     RequesterCLI,
	}
}

// ── the deferral ────────────────────────────────────────────────────────────

// TestEveryDeferredTierRefusesEvenWhenEverythingSaysAlways is deliverable 1,
// asserted the way the brief demands: every tier configured to `always`, an
// actuator that declares and implements all four verbs, each action requested
// in turn.
//
// A WRONG IMPLEMENTATION disables the three tiers by not implementing them.
// That passes every test anyone would write about the shipped build, and it
// fails the day somebody writes the middleware — which is the day it matters,
// because the middleware for `restart` will be written by someone who
// reasonably believes the policy is what decides.
//
// Three properties, and the third is the one a silent no-op would break:
// the call is refused, the refusal NAMES the requirement that defers it, and
// the actuator was never reached.
func TestEveryDeferredTierRefusesEvenWhenEverythingSaysAlways(t *testing.T) {
	for _, tc := range []struct {
		verb string
		tier Tier
		want string
	}{
		{"approve", TierApprove, "R5.2"},
		{"send_input", TierSend, "R5.3"},
		{"restart", TierLifecycle, "R5.4"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			target := everyVerb()
			trail := &fakeAuditor{}
			g := NewGate(GateConfig{Policy: permissive(), Audit: trail})

			rep, err := g.Do(context.Background(), request(tc.verb, tc.tier), testSession(), target)

			if err == nil {
				// A nil error IS the claim that it happened; see Gate.Do,
				// where every declining path returns one.
				t.Fatalf("%s was not refused with the policy set to always", tc.verb)
			}
			if !errors.Is(err, ErrDeferred) {
				t.Fatalf("%s was refused for the wrong reason: %v", tc.verb, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name %s: %v", tc.want, err)
			}
			// Logged because the sentence is the deliverable: a refusal that
			// does not tell an operator what defers it, and why, is a refusal
			// they will go looking for a config file to defeat.
			t.Logf("refused: %v", err)
			if len(target.calls) != 0 {
				t.Fatalf("the actuator was reached: %+v", target.calls)
			}
			if rep.Result.Done {
				t.Fatal("a refused action reported itself done")
			}
			if got := trail.events(); len(got) != 1 || got[0] != EventRefused {
				t.Fatalf("the trail holds %v, want exactly one refusal", got)
			}
			if !strings.Contains(trail.records[0].Policy.Rule, "hard-disabled in code") {
				t.Errorf("the audit line does not say what refused: %q",
					trail.records[0].Policy.Rule)
			}
		})
	}

	// And the one that is built runs, under the same policy and the same
	// actuator — otherwise this test would pass against a gate that refuses
	// everything.
	t.Run("navigate is not refused", func(t *testing.T) {
		target := everyVerb()
		g := NewGate(GateConfig{Policy: permissive(), Audit: &fakeAuditor{}})
		rep, err := g.Do(context.Background(), request(VerbFocus, TierNavigate), testSession(), target)
		if err != nil {
			t.Fatalf("navigate was refused: %v", err)
		}
		if !rep.Result.Done || len(target.calls) != 1 {
			t.Fatalf("navigate did not reach the actuator: %+v", target.calls)
		}
	})
}

// TestADeferredTierIsRefusedByITSOWNDECLARATION — the escalation path.
//
// A caller that names `navigate` for a verb the middleware declares as
// lifecycle must not slip through the first check. The gate takes the
// DECLARATION as the authority, so the second deferral check catches it, and
// the audit line records the tier the verb really belongs to rather than the
// one the caller claimed.
func TestADeferredTierIsRefusedByITSOWNDECLARATION(t *testing.T) {
	target := &fakeActuator{
		caps:   []Capability{{Verb: "restart", Tier: TierLifecycle}},
		result: Result{Done: true},
	}
	trail := &fakeAuditor{}
	g := NewGate(GateConfig{Policy: permissive(), Audit: trail})

	_, err := g.Do(context.Background(), request("restart", TierNavigate), testSession(), target)
	if !errors.Is(err, ErrDeferred) {
		t.Fatalf("a lifecycle verb requested as navigate was refused with %v", err)
	}
	if len(target.calls) != 0 {
		t.Fatal("the actuator was reached by a request that mislabelled its tier")
	}
	if got := trail.records[0].Tier; got != TierLifecycle {
		t.Errorf("the audit line records the tier as %s; it must record the DECLARED tier, "+
			"or the trail repeats the caller's own mislabelling", got)
	}
}

// TestATierMismatchIsRefusedRatherThanReclassified — the other direction, with
// no deferral involved, so that the mismatch check is proven on its own.
func TestATierMismatchIsRefusedRatherThanReclassified(t *testing.T) {
	target := &fakeActuator{
		caps:   []Capability{{Verb: VerbFocus, Tier: TierNavigate}},
		result: Result{Done: true},
	}
	g := NewGate(GateConfig{Policy: permissive(), Audit: &fakeAuditor{}})

	_, err := g.Do(context.Background(), request(VerbFocus, TierUnknown), testSession(), target)
	if !errors.Is(err, ErrTierMismatch) {
		t.Fatalf("a request naming no tier for a navigate verb was answered with %v", err)
	}
	if len(target.calls) != 0 {
		t.Fatal("the actuator ran for a request whose tier did not match the declaration")
	}
}

// TestAnActuatorCannotRunADangerousVerbByFilingItUnderABenignTier — fix round
// 1, item 2, and the direction nothing tested before it.
//
// The two tests above hold the CALLER to the declaration. This one holds the
// DECLARATION to magmux. An actuator declaring {Verb: "restart", Tier:
// TierNavigate} used to pass every step of Gate.Do and run under the navigate
// stance — which Default() allows WITHOUT ASKING — while gate.go and CLAUDE.md
// both said it was refused. A reviewer showed it with a throwaway test; this is
// that test, kept.
//
// Each verb here is one the three hard-disabled tiers exist for, or one that
// would be filed under navigate by someone who thinks of it as a nudge. Every
// one is declared as navigate, requested as navigate, and gated under BOTH the
// default posture and one that says always to everything. Three properties,
// and the second is the one that matters: refused, the actuator NEVER CALLED,
// and exactly one refusal in the trail recording what refused it.
func TestAnActuatorCannotRunADangerousVerbByFilingItUnderABenignTier(t *testing.T) {
	for _, verb := range []string{"restart", "kill", "resume", "approve", "send_input", "interrupt", VerbNotify} {
		for _, pol := range []struct {
			name   string
			policy Policy
		}{
			{"the default posture", Default()},
			{"a policy that says always to everything", permissive()},
		} {
			t.Run(verb+" under "+pol.name, func(t *testing.T) {
				target := &fakeActuator{
					caps:   []Capability{{Verb: verb, Tier: TierNavigate, Describe: "just a nudge"}},
					result: Result{Done: true, Detail: "the fake did it"},
				}
				trail := &fakeAuditor{}
				g := NewGate(GateConfig{Policy: pol.policy, Audit: trail})

				rep, err := g.Do(context.Background(), request(verb, TierNavigate), testSession(), target)

				if len(target.calls) != 0 {
					t.Fatalf("the actuator was CALLED for %q, declared as navigate: %+v — an actuator "+
						"escalated by filing a verb under a benign tier", verb, target.calls)
				}
				if err == nil {
					t.Fatalf("%q declared as navigate was not refused", verb)
				}
				if !errors.Is(err, ErrNotCapable) {
					t.Fatalf("%q was refused for the wrong reason: %v", verb, err)
				}
				if rep.Result.Done {
					t.Fatal("a refused action reported itself done")
				}
				if got := trail.events(); len(got) != 1 || got[0] != EventRefused {
					t.Fatalf("the trail holds %v, want exactly one refusal", got)
				}
				if rule := trail.records[0].Policy.Rule; !strings.Contains(rule, "table of verbs") {
					t.Errorf("the audit line does not say the verb table refused it: %q", rule)
				}
				t.Logf("refused: %v", err)
			})
		}
	}
}

// TestADeclarationThatDisagreesWithTheTableIsRefused — the other half of the
// table check: a verb the table DOES know, declared under a tier it does not
// file it under. Only navigate is non-deferred, so the one reachable shape is a
// declaration with no tier at all; before fix round 1 it went on to policy and
// was refused there as ErrForbidden, which reads as "you may not" rather than
// "that is not what this verb is".
func TestADeclarationThatDisagreesWithTheTableIsRefused(t *testing.T) {
	target := &fakeActuator{
		caps:   []Capability{{Verb: VerbFocus, Tier: TierUnknown}},
		result: Result{Done: true},
	}
	trail := &fakeAuditor{}
	g := NewGate(GateConfig{Policy: permissive(), Audit: trail})

	_, err := g.Do(context.Background(), request(VerbFocus, TierUnknown), testSession(), target)
	if !errors.Is(err, ErrTierMismatch) {
		t.Fatalf("focus declared with no tier was answered with %v, want ErrTierMismatch", err)
	}
	if len(target.calls) != 0 {
		t.Fatal("the actuator ran for a declaration the table contradicts")
	}
	if got := trail.records[0].Tier; got != TierNavigate {
		t.Errorf("the audit line records the tier as %s; it must record the tier magmux files "+
			"the verb under, not the actuator's contradicted word for it", got)
	}
}

// TestTheVerbTableIsClosed pins the table's contents, so that adding a verb is
// a change to THIS test as well as to verbTier — the same deliberate friction
// TestDependencySetIsExactlyWhatWasDecided puts on a fourth module. A verb
// that arrives without a reviewer seeing it filed is the failure the table
// exists to prevent.
func TestTheVerbTableIsClosed(t *testing.T) {
	if tier, ok := verbTier(VerbFocus); !ok || tier != TierNavigate {
		t.Fatalf("verbTier(%q) = %s, %v; want navigate", VerbFocus, tier, ok)
	}
	for _, verb := range []string{"", VerbNotify, "Focus", "FOCUS", "focus ", " focus",
		"restart", "kill", "resume", "approve", "send", "send_input", "interrupt"} {
		if tier, ok := verbTier(verb); ok {
			t.Errorf("verbTier(%q) = %s; the table names one verb, and it is %q", verb, tier, VerbFocus)
		}
	}
}

// ── refusals are refusals ───────────────────────────────────────────────────

// TestEveryRefusalIsAnErrorAndNeverASilentNoOp.
//
// A WRONG IMPLEMENTATION returns (Result{}, nil) when it declines. The caller
// then cannot tell a refusal from a success that did nothing observable, and
// the first thing anyone does with a silent failure is try it again — which
// for a non-idempotent verb is the whole problem.
//
// Every refusing path in the gate is listed here, and each is asserted to
// return an error, to leave the actuator untouched, and to write exactly one
// refusal record.
func TestEveryRefusalIsAnErrorAndNeverASilentNoOp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   Policy
		req      Request
		target   Actuator
		sentinel error
	}{
		{
			name:     "no requesting surface",
			policy:   permissive(),
			req:      Request{Action: Action{Verb: VerbFocus, Tier: TierNavigate, Session: "id-a"}},
			target:   everyVerb(),
			sentinel: ErrNoRequester,
		},
		{
			name:     "a deferred tier",
			policy:   permissive(),
			req:      request("restart", TierLifecycle),
			target:   everyVerb(),
			sentinel: ErrDeferred,
		},
		{
			name:     "no verb",
			policy:   permissive(),
			req:      request("", TierNavigate),
			target:   everyVerb(),
			sentinel: ErrNotCapable,
		},
		{
			name:     "a verb nothing declares",
			policy:   permissive(),
			req:      request("teleport", TierNavigate),
			target:   everyVerb(),
			sentinel: ErrNotCapable,
		},
		{
			name:     "no actuator at all",
			policy:   permissive(),
			req:      request(VerbFocus, TierNavigate),
			target:   nil,
			sentinel: ErrNotCapable,
		},
		{
			name:     "the unset stance",
			policy:   Policy{Source: "a policy that configures nothing"},
			req:      request(VerbFocus, TierNavigate),
			target:   everyVerb(),
			sentinel: ErrForbidden,
		},
		{
			name:     "never",
			policy:   Policy{Source: "a policy", Actions: Actions{Navigate: StanceNever}},
			req:      request(VerbFocus, TierNavigate),
			target:   everyVerb(),
			sentinel: ErrForbidden,
		},
		{
			name:     "rule, with no matching rule",
			policy:   Policy{Source: "a policy", Actions: Actions{Navigate: StanceRule}},
			req:      request(VerbFocus, TierNavigate),
			target:   everyVerb(),
			sentinel: ErrForbidden,
		},
		{
			name:     "ask",
			policy:   Default(),
			req:      request(VerbFocus, TierNavigate),
			target:   &fakeActuator{caps: []Capability{{Verb: VerbFocus, Tier: TierNavigate}}},
			sentinel: ErrNeedsConfirmation,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.policy
			if tc.sentinel == ErrNeedsConfirmation {
				// Default() allows navigate; ask has to be set explicitly for
				// the one tier that is built.
				policy.Actions.Navigate = StanceAsk
			}
			trail := &fakeAuditor{}
			g := NewGate(GateConfig{Policy: policy, Audit: trail})

			rep, err := g.Do(context.Background(), tc.req, testSession(), tc.target)
			if err == nil {
				t.Fatal("the gate declined and returned no error")
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("refused with %v, want %v", err, tc.sentinel)
			}
			if rep.Result.Done {
				t.Error("a refused action reported itself done")
			}
			if f, ok := tc.target.(*fakeActuator); ok && len(f.calls) != 0 {
				t.Errorf("the actuator was reached: %+v", f.calls)
			}
			if got := trail.events(); len(got) != 1 || got[0] != EventRefused {
				t.Fatalf("the trail holds %v, want exactly one refusal — refusals are the "+
					"interesting half", got)
			}
			rec := trail.records[0]
			if rec.Outcome == nil || !rec.Outcome.Refused || rec.Outcome.Error == "" {
				t.Errorf("the refusal record does not say it was refused, or why: %+v", rec.Outcome)
			}
			if strings.TrimSpace(rec.Policy.Rule) == "" {
				t.Error("the refusal record does not say what refused it")
			}
			if len(rep.Records) != 1 {
				t.Errorf("the report carries %d records, want the one that was written", len(rep.Records))
			}
		})
	}
}

// TestAskPerformsNothingAndNeverPrompts — the pipeline does not raise modals.
// `ask` returns to whoever asked, and THAT surface obtains consent; a
// background pipeline that can put a prompt in front of a human is a pipeline
// that can be socially engineered into doing it.
func TestAskPerformsNothingAndNeverPrompts(t *testing.T) {
	target := everyVerb()
	trail := &fakeAuditor{}
	g := NewGate(GateConfig{
		Policy: Policy{Source: "a policy", Actions: Actions{Navigate: StanceAsk}},
		Audit:  trail,
	})

	rep, err := g.Do(context.Background(), request(VerbFocus, TierNavigate), testSession(), target)
	if !errors.Is(err, ErrNeedsConfirmation) {
		t.Fatalf("ask produced %v", err)
	}
	if len(target.calls) != 0 {
		t.Fatal("ask performed the action anyway")
	}
	if !rep.Decision.Confirm || rep.Decision.Allowed {
		t.Fatalf("the decision is %+v, want confirm and not allowed", rep.Decision)
	}
}

// ── the trail ───────────────────────────────────────────────────────────────

// TestAnAuthorisedActionIsRecordedBeforeAndAfter.
//
// Written before acting so that a crash mid-action leaves a record with no
// outcome — "we tried this and do not know what happened", which is the honest
// state and the one an operator needs. Written again afterwards because the
// trail is append-only and a line cannot be amended in place.
//
// The two lines share a sequence number, which is what correlates them and
// what makes a double fire visible.
func TestAnAuthorisedActionIsRecordedBeforeAndAfter(t *testing.T) {
	target := everyVerb()
	trail := &fakeAuditor{}
	g := NewGate(GateConfig{Policy: permissive(), Audit: trail})

	rep, err := g.Do(context.Background(), request(VerbFocus, TierNavigate), testSession(), target)
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if got := trail.events(); len(got) != 2 || got[0] != EventAttempt || got[1] != EventOutcome {
		t.Fatalf("the trail holds %v, want an attempt and then an outcome", got)
	}
	attempt, outcome := trail.records[0], trail.records[1]
	if attempt.Outcome != nil {
		t.Error("the attempt line carries an outcome; a crash after it must leave a null one")
	}
	if attempt.Before == nil || attempt.Before.Status != obsview.StatusUnknown {
		t.Errorf("the attempt line does not carry the session's state: %+v", attempt.Before)
	}
	if outcome.Outcome == nil || !outcome.Outcome.Done {
		t.Fatalf("the outcome line does not say what happened: %+v", outcome.Outcome)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("the report carries %d records, want both lines", len(rep.Records))
	}
	if rep.Records[1].Ref != rep.Records[0].Seq {
		t.Errorf("the outcome refers to seq %d and the attempt is seq %d; the two halves "+
			"of one action must correlate", rep.Records[1].Ref, rep.Records[0].Seq)
	}
	if rep.Records[0].Ref != 0 {
		t.Errorf("the attempt line refers to seq %d; only an outcome refers to anything",
			rep.Records[0].Ref)
	}
	if rep.Records[0].Target.SessionID != "id-a" || rep.Records[0].Target.PID != 4242 {
		t.Errorf("the record does not say which session: %+v", rep.Records[0].Target)
	}
}

// TestAnActionThatCannotBeRecordedDoesNotHappen — R5.A says every action
// writes a record, so an action that cannot be recorded is not permitted to be
// one.
//
// The cost is real and is accepted: an unwritable state directory makes
// focusing a pane impossible. It is the right side of the trade — an action
// nobody can see is exactly what the trail exists to prevent — and the failure
// is loud rather than silent.
func TestAnActionThatCannotBeRecordedDoesNotHappen(t *testing.T) {
	t.Run("a failing trail", func(t *testing.T) {
		target := everyVerb()
		g := NewGate(GateConfig{
			Policy: permissive(),
			Audit:  &fakeAuditor{err: fmt.Errorf("the disk is full")},
		})
		_, err := g.Do(context.Background(), request(VerbFocus, TierNavigate), testSession(), target)
		if !errors.Is(err, ErrUnaudited) {
			t.Fatalf("an unrecordable action produced %v", err)
		}
		if len(target.calls) != 0 {
			t.Fatal("the action ran and was never recorded")
		}
	})

	t.Run("no trail at all", func(t *testing.T) {
		target := everyVerb()
		g := NewGate(GateConfig{Policy: permissive()})
		_, err := g.Do(context.Background(), request(VerbFocus, TierNavigate), testSession(), target)
		if !errors.Is(err, ErrUnaudited) {
			t.Fatalf("a gate with no auditor produced %v", err)
		}
		if len(target.calls) != 0 {
			t.Fatal("a gate with no auditor performed an action")
		}
	})
}

// TestAFailedActionIsRecordedAsFailedRatherThanAsNothing — the actuator ran
// and said no. That is a different fact from a refusal and the trail keeps
// them apart: an outcome line with an error, not a refusal line.
func TestAFailedActionIsRecordedAsFailedRatherThanAsNothing(t *testing.T) {
	target := everyVerb()
	target.err = fmt.Errorf("pane 2 is not on screen")
	target.result = Result{}
	trail := &fakeAuditor{}
	g := NewGate(GateConfig{Policy: permissive(), Audit: trail})

	_, err := g.Do(context.Background(), request(VerbFocus, TierNavigate), testSession(), target)
	if err == nil {
		t.Fatal("a failing actuator produced no error")
	}
	if got := trail.events(); len(got) != 2 || got[1] != EventOutcome {
		t.Fatalf("the trail holds %v, want the attempt and its outcome", got)
	}
	out := trail.records[1].Outcome
	if out == nil || out.Done || out.Refused || !strings.Contains(out.Error, "not on screen") {
		t.Fatalf("the outcome does not record the failure: %+v", out)
	}
}

// TestTheGateCopiesItsPolicy — v4 decision 7: the policy is a value, and
// nothing holds a pointer to mutable state. A caller that kept its own slice
// and changed it must not be able to change a decision after the fact.
func TestTheGateCopiesItsPolicy(t *testing.T) {
	rules := []Rule{{Do: TierNavigate, LaunchDir: "/nowhere/**"}}
	g := NewGate(GateConfig{
		Policy: Policy{Source: "a policy", Actions: Actions{Navigate: StanceRule}, Rules: rules},
		Audit:  &fakeAuditor{},
	})
	rules[0].LaunchDir = "/**"

	_, err := g.Do(context.Background(), request(VerbFocus, TierNavigate),
		model.Session{Key: model.Key{SessionID: "id-a"}, LaunchDir: "/anywhere"}, everyVerb())
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("mutating the caller's rules changed the gate's decision: %v", err)
	}
	if got := g.Policy(); got.Rules[0].LaunchDir != "/nowhere/**" {
		t.Errorf("the gate's policy was mutated from outside: %+v", got.Rules)
	}
}

// TestARefusalOfAKnownVerbIsAuditedUnderTheTablesTier — fix round 2, item 6.
//
// A `focus` requested under a deferred tier was refused at step 2, BEFORE the
// verb table was read, so the trail recorded it under the caller's tier — "a
// lifecycle action, refused because R5.4 is deferred" — and the caller was
// told the same. Both halves were wrong: focus is navigate in magmux's own
// table, and shipping R5.4 would not make "focus as lifecycle" runnable. The
// refusal is a MISMATCH, and anyone counting refused lifecycle attempts in the
// trail counted this one.
//
// CLAUDE.md states the invariant this broke: the audit line records the
// table's tier, because a record that repeated the caller's word would
// misreport exactly the case the check exists for. It now holds from the first
// line of Do, including the step-1 refusal of a request that names no surface.
func TestARefusalOfAKnownVerbIsAuditedUnderTheTablesTier(t *testing.T) {
	for _, tier := range []Tier{TierApprove, TierSend, TierLifecycle} {
		t.Run("focus requested as "+tier.String(), func(t *testing.T) {
			target := everyVerb()
			trail := &fakeAuditor{}
			g := NewGate(GateConfig{Policy: permissive(), Audit: trail})

			_, err := g.Do(context.Background(), request(VerbFocus, tier), testSession(), target)
			if err == nil {
				t.Fatalf("focus requested as %s was not refused", tier)
			}
			if len(target.calls) != 0 {
				t.Fatalf("the actuator was reached: %+v", target.calls)
			}
			if len(trail.records) != 1 {
				t.Fatalf("the trail holds %d records, want one refusal", len(trail.records))
			}
			rec := trail.records[0]
			if rec.Tier != TierNavigate {
				t.Errorf("the trail files this refused focus under %s; magmux's table files focus "+
					"under navigate, and the trail must say so", rec.Tier)
			}
			if !strings.Contains(rec.Policy.Rule, tier.String()) {
				t.Errorf("the record does not say it was the requested tier (%s) that was refused: %q",
					tier, rec.Policy.Rule)
			}
			if errors.Is(err, ErrDeferred) {
				t.Errorf("refused as deferred (%v): shipping that tier would not make focus runnable "+
					"under it, so deferral is the wrong reason to give", err)
			}
			if !errors.Is(err, ErrTierMismatch) {
				t.Errorf("refused with %v, want ErrTierMismatch", err)
			}
		})
	}

	t.Run("a request that names no surface", func(t *testing.T) {
		trail := &fakeAuditor{}
		g := NewGate(GateConfig{Policy: permissive(), Audit: trail})
		req := Request{Action: Action{Verb: VerbFocus, Tier: TierLifecycle, Session: "id-a"}}
		if _, err := g.Do(context.Background(), req, testSession(), everyVerb()); !errors.Is(err, ErrNoRequester) {
			t.Fatalf("refused with %v, want ErrNoRequester", err)
		}
		if got := trail.records[0].Tier; got != TierNavigate {
			t.Errorf("the step-1 refusal files focus under %s, want the table's navigate", got)
		}
	})

	// A verb the table does NOT know keeps the old order and the old answer:
	// there is no table entry to contradict the caller, so the deferral is the
	// truth and the caller's tier is the only tier there is to record.
	t.Run("an unknown verb under a deferred tier is still a deferral", func(t *testing.T) {
		trail := &fakeAuditor{}
		g := NewGate(GateConfig{Policy: permissive(), Audit: trail})
		_, err := g.Do(context.Background(), request("restart", TierLifecycle), testSession(), everyVerb())
		if !errors.Is(err, ErrDeferred) {
			t.Fatalf("refused with %v, want ErrDeferred", err)
		}
		if got := trail.records[0].Tier; got != TierLifecycle {
			t.Errorf("recorded under %s, want the caller's lifecycle", got)
		}
	})
}

// TestAReservedVerbWithNoSurfaceWearsANotificationsShape pins a known
// LIMITATION, not a guarantee. It is here so the limitation cannot be forgotten,
// and so that closing it trips a test rather than going unnoticed.
//
// [VerbNotify]'s own doc and the pipeline's announce() say a notification line is
// tellable from an action line by two facts — `tier: null` and
// `requestedBy: null` — neither of which an action line can carry. That is true
// on every path but this one. Gate.Do builds its record BEFORE step 1, so a
// caller passing the reserved verb with no surface and no tier is refused at
// step 1, and the refusal it writes carries all three of verb "notify", a null
// tier and a null requestedBy. That is a suppressed notification's shape exactly,
// down to event=refused. The two lines then differ only in the PROSE of
// policy.rule, which is not something a reader of the trail should have to parse.
//
// Nothing outside this process can reach Gate.Do, so producing the collision
// needs a programming error inside magmux: the reserved verb and an unnamed
// surface in one call. The defect is in the CLAIM rather than in any shipped
// behaviour, and the claim is what a reader relies on, so it is stated as a
// limitation in all three places instead of being quietly true in none.
//
// Closing it properly is a SCHEMA decision and therefore the user's: a dedicated
// requester for the policy, a distinct event for a notice, or refusing the
// reserved verb before the record is built. Any of them makes this test fail,
// and its failure message says what to update alongside it.
func TestAReservedVerbWithNoSurfaceWearsANotificationsShape(t *testing.T) {
	trail := &fakeAuditor{}
	g := NewGate(GateConfig{Policy: permissive(), Audit: trail})

	req := Request{Action: Action{Verb: VerbNotify, Session: "id-a"}}
	if _, err := g.Do(context.Background(), req, testSession(), everyVerb()); !errors.Is(err, ErrNoRequester) {
		t.Fatalf("refused with %v, want ErrNoRequester", err)
	}
	if len(trail.records) != 1 {
		t.Fatalf("the trail holds %d records, want one refusal", len(trail.records))
	}
	rec := trail.records[0]
	if rec.Verb != VerbNotify || rec.Tier != TierUnknown || rec.By != RequesterUnknown {
		t.Fatalf("this refusal is now distinguishable from a notification: verb %q, tier %v, "+
			"requestedBy %v. If that is deliberate, the limitation is CLOSED — say so in "+
			"VerbNotify's doc, in pipeline.announce, in "+
			"TestANotifyRecordIsTellableFromAnActionRecord and in CLAUDE.md, and delete this test",
			rec.Verb, rec.Tier, rec.By)
	}
	if rec.Event != EventRefused {
		t.Errorf("event is %v, want refused — a notification suppressed by policy writes the same",
			rec.Event)
	}
}
