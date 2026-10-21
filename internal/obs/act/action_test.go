package act

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEveryDeferredTierNamesTheRequirementThatDefersIt.
//
// A WRONG IMPLEMENTATION returns a bare "not allowed". The refusal reaches a
// person — on a terminal, in an audit line, through an MCP client — and "not
// allowed" leaves them unable to tell whether to edit a config, wait for a
// release, or do the thing by hand. Each reason therefore names its
// requirement, and lifecycle says out loud that it is irreversible, because
// that is the fact that decides whether anyone should be trying to enable it.
func TestEveryDeferredTierNamesTheRequirementThatDefersIt(t *testing.T) {
	for _, tc := range []struct {
		tier Tier
		want string
	}{
		{TierApprove, "R5.2"},
		{TierSend, "R5.3"},
		{TierLifecycle, "R5.4"},
	} {
		reason, deferred := tc.tier.Deferred()
		if !deferred {
			t.Fatalf("%s is not deferred, and this build cannot perform it", tc.tier)
		}
		if !strings.Contains(reason, tc.want) {
			t.Errorf("%s defers with %q, which does not name %s", tc.tier, reason, tc.want)
		}
	}
	if reason, _ := TierLifecycle.Deferred(); !strings.Contains(reason, "IRREVERSIBLE") {
		t.Errorf("the lifecycle deferral does not say it is irreversible: %q", reason)
	}

	// The two that are not deferred, for opposite reasons: navigate is BUILT,
	// and the zero tier is not a tier at all. The second matters because a
	// zero-value Action must be refused by the capability check and the
	// policy, not by a deferral that would mislabel it.
	for _, tier := range []Tier{TierNavigate, TierUnknown} {
		if reason, deferred := tier.Deferred(); deferred {
			t.Errorf("%s reports itself deferred (%q)", tier, reason)
		}
	}
}

// TestTheEnumShapeMatchesTheCoreVocabulary pins this package's copy of
// obs.enumSpec to the rules the original states, because a copy is only safe
// while it still behaves like the thing it copied.
//
// Three rules, and the third is the one a naive implementation breaks: the
// zero value's display name is NOT accepted back, so there is exactly one
// encoding of "nothing was determined" on the wire.
func TestTheEnumShapeMatchesTheCoreVocabulary(t *testing.T) {
	t.Run("the zero value marshals to null", func(t *testing.T) {
		for _, v := range []any{TierUnknown, StanceUnknown, RequesterUnknown, EventUnknown, NotifyOnUnknown} {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshalling %v: %v", v, err)
			}
			if string(b) != "null" {
				t.Errorf("%T zero marshals to %s, want null", v, b)
			}
		}
	})

	t.Run("a determined value round-trips", func(t *testing.T) {
		b, err := json.Marshal(TierLifecycle)
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if string(b) != `"lifecycle"` {
			t.Fatalf("TierLifecycle marshals to %s", b)
		}
		var back Tier
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("unmarshalling: %v", err)
		}
		if back != TierLifecycle {
			t.Fatalf("round trip produced %v", back)
		}
	})

	t.Run("null decodes to the zero value and the display name does not", func(t *testing.T) {
		var tier Tier
		if err := json.Unmarshal([]byte("null"), &tier); err != nil || tier != TierUnknown {
			t.Fatalf("null decoded to %v, err %v", tier, err)
		}
		if err := json.Unmarshal([]byte(`"unknown"`), &tier); err == nil {
			t.Error(`"unknown" was accepted as a tier; there must be exactly one encoding of ` +
				`"nothing was determined", and it is null`)
		}
		if err := json.Unmarshal([]byte(`"restart"`), &tier); err == nil {
			t.Error(`an unrecognised tier name was accepted; this package's own records are ` +
				`its own contract and a name that does not decode is a defect, not a format change`)
		}
	})
}

// TestTheFourTiersAreTheFourTheRequirementNames — a fifth tier would be a
// class of action nobody wrote a stance for, and the policy would refuse it
// with "unset" rather than with a deferral. That is safe and it is also
// confusing, so the set is pinned.
func TestTheFourTiersAreTheFourTheRequirementNames(t *testing.T) {
	want := []string{"navigate", "approve", "send", "lifecycle"}
	for i, name := range want {
		if got := Tier(i + 1).String(); got != name {
			t.Errorf("tier %d is %q, want %q", i+1, got, name)
		}
	}
	if got := Tier(len(want) + 1).String(); !strings.HasPrefix(got, "Tier(") {
		t.Errorf("a fifth tier exists and spells itself %q", got)
	}
}
