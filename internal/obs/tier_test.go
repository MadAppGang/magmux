package obs

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTierMarshalsLikeEveryOtherEnumHere — null for undetermined, the wire word
// otherwise.
//
// A WRONG IMPLEMENTATION emits "unknown" for the zero value, which puts a
// SECOND spelling of "undetermined" on the wire for every consumer to have to
// test for, beside the null every other optional field uses.
func TestTierMarshalsLikeEveryOtherEnumHere(t *testing.T) {
	for _, tt := range []struct {
		tier Tier
		want string
	}{
		{TierUnknown, "null"},
		{TierInferred, `"inferred"`},
		{TierAuthoritative, `"authoritative"`},
	} {
		t.Run(tt.tier.String(), func(t *testing.T) {
			b, err := json.Marshal(tt.tier)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Fatalf("got %s, want %s", b, tt.want)
			}
			var back Tier
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back != tt.tier {
				t.Fatalf("round trip gave %v, want %v", back, tt.tier)
			}
		})
	}
}

// TestTierRejectsAWordItDoesNotKnow. This package's own document is its own
// contract: a name that does not decode here is a defect in something magmux
// wrote, not a tool changing its format underneath us. The lenient path stops
// at the adapter boundary.
//
// The third tier is in the list on purpose. `stale_authoritative` was proposed
// and rejected — see [Tier] — and a decoder that quietly accepted it would let
// one back in through the wire.
func TestTierRejectsAWordItDoesNotKnow(t *testing.T) {
	for _, wire := range []string{
		`"unknown"`, `""`, `"stale_authoritative"`, `"INFERRED"`, `"guess"`, `0`, `1`, `true`, `{}`,
	} {
		t.Run(wire, func(t *testing.T) {
			var tier Tier
			if err := json.Unmarshal([]byte(wire), &tier); err == nil {
				t.Fatalf("accepted %s as tier %v", wire, tier)
			}
		})
	}
}

// TestThereAreExactlyTwoDeterminedTiers.
//
// A WRONG IMPLEMENTATION adds the third tier this design rejected. The
// rejection is recorded in [Tier]'s own documentation because a plan is not
// where somebody about to add one will look; this is where it is enforced.
func TestThereAreExactlyTwoDeterminedTiers(t *testing.T) {
	if got := len(tierEnum.names) - 1; got != 2 {
		t.Fatalf("%d determined tiers, want exactly 2 (inferred and authoritative): %v",
			got, tierEnum.names)
	}
}

// TestAConflictIsAbsentAndNotNullWhenThereIsNothingToSay.
//
// A field that is null asks a reader "which conflict was undetermined?". There
// is no such thing: either the two tiers disagreed and the loser is here, or
// they did not and there is nothing to describe. omitempty says that; null does
// not.
func TestAConflictIsAbsentAndNotNullWhenThereIsNothingToSay(t *testing.T) {
	b, err := json.Marshal(Session{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "conflict") {
		t.Fatalf("a session with no conflict still carries the key:\n%s", b)
	}
	// And the tier it was resolved on IS null, because that one is
	// undetermined rather than absent.
	if !strings.Contains(string(b), `"confidence":null`) {
		t.Fatalf("a session resolved by nobody does not report a null confidence:\n%s", b)
	}
}

// TestAConflictRoundTripsWithBothTiersNamed.
func TestAConflictRoundTripsWithBothTiersNamed(t *testing.T) {
	want := Session{
		Status:     StatusBusy,
		Confidence: TierAuthoritative,
		Conflict: &Conflict{
			Tier:   TierInferred,
			Status: StatusIdle,
			Detail: "the pane has produced no output for 5m 0s",
		},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, frag := range []string{
		`"confidence":"authoritative"`,
		`"conflict":{"tier":"inferred","status":"idle"`,
		`"detail":"the pane has produced no output for 5m 0s"`,
	} {
		if !strings.Contains(string(b), frag) {
			t.Errorf("the document does not contain %s:\n%s", frag, b)
		}
	}

	var got Session
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Conflict == nil || *got.Conflict != *want.Conflict || got.Confidence != want.Confidence {
		t.Fatalf("round trip gave %+v, want %+v", got.Conflict, want.Conflict)
	}
}

// TestCloneIsolatesTheConflict — [Session.Clone] is the publish boundary, and a
// pointer it shares is a route from a stage into a document already handed out.
func TestCloneIsolatesTheConflict(t *testing.T) {
	orig := Session{Conflict: &Conflict{Tier: TierInferred, Status: StatusIdle, Detail: "original"}}
	c := orig.Clone()
	if c.Conflict == nil {
		t.Fatal("the clone dropped the conflict")
	}
	c.Conflict.Detail = "changed"
	c.Conflict.Status = StatusBusy
	if orig.Conflict.Detail != "original" || orig.Conflict.Status != StatusIdle {
		t.Fatalf("the clone shares its conflict with the original: %+v", *orig.Conflict)
	}
	var empty Session
	if empty.Clone().Conflict != nil {
		t.Error("cloning a session with no conflict invented one")
	}
}
