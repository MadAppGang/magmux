package obs

import (
	"encoding/json"
	"testing"
)

// The zero value of every enum in this package is "unknown". A wrong
// implementation here is the tempting one: making Idle the zero value, so that
// a Session nobody has looked at yet reports a session that is doing nothing.
// That reads correctly in every test that sets the field and is wrong in every
// path that forgets to.
func TestEnumZeroValueIsUnknown(t *testing.T) {
	var s Status
	if s != StatusUnknown {
		t.Errorf("zero Status = %v, want StatusUnknown", s)
	}
	var w WaitingReason
	if w != WaitingReasonUnknown {
		t.Errorf("zero WaitingReason = %v, want WaitingReasonUnknown", w)
	}
	var l Liveness
	if l != LivenessUnknown {
		t.Errorf("zero Liveness = %v, want LivenessUnknown", l)
	}
}

// Unknown marshals to null, and a DETERMINED negative does not. An
// implementation that marshalled WaitingReasonNone as null — or that
// marshalled every enum as its ordinal — would make "we did not determine what
// this session is waiting for" and "it is waiting for nothing" the same bytes,
// which is the one distinction the type exists to carry.
func TestEnumsMarshalUnknownAsNullAndNothingElseAsNull(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"status unknown", StatusUnknown, `null`},
		{"status determined", StatusBusy, `"busy"`},
		{"waiting unknown", WaitingReasonUnknown, `null`},
		{"waiting determined negative", WaitingReasonNone, `"none"`},
		{"waiting determined", WaitingReasonPermission, `"permission"`},
		{"liveness unknown", LivenessUnknown, `null`},
		{"liveness yes", LivenessYes, `"yes"`},
		{"liveness no", LivenessNo, `"no"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.val)
			if err != nil {
				t.Fatalf("Marshal(%v): %v", c.val, err)
			}
			if string(b) != c.want {
				t.Errorf("Marshal(%v) = %s, want %s", c.val, b, c.want)
			}
		})
	}
}

// The settled wire vocabulary, written out value by value, because three
// independent consumers key on these exact bytes and a table this small is
// cheaper to read than the three name slices it pins.
//
// status mirrors Claude's four words deliberately — with one harness in scope,
// a neutral vocabulary would buy no insulation and cost every reader a second
// set of words. waitingOn does NOT mirror: Claude's spellings carry spaces,
// which are poor values for jq and for shell pipelines, so they are normalised
// here and the mapping lives in adapters/schema.go.
func TestTheWireVocabularyIsTheSettledOne(t *testing.T) {
	cases := []struct {
		val  any
		want string
	}{
		{StatusIdle, `"idle"`},
		{StatusBusy, `"busy"`},
		{StatusShell, `"shell"`},
		{StatusWaiting, `"waiting"`},
		{StatusUnknown, `null`},

		{WaitingReasonInput, `"input"`},
		{WaitingReasonPermission, `"permission"`},
		{WaitingReasonNone, `"none"`},
		{WaitingReasonUnknown, `null`},

		{LivenessYes, `"yes"`},
		{LivenessNo, `"no"`},
		{LivenessUnknown, `null`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.val)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", c.val, err)
		}
		if string(b) != c.want {
			t.Errorf("Marshal(%v) = %s, want %s", c.val, b, c.want)
		}
	}
}

// The display spelling of an undetermined value is not a wire value. String
// reports "unknown" so that a printed value reads as something, but null is
// the ONE encoding of "no opinion" and a document spelling it any other way is
// not one of ours.
//
// The wrong implementation accepts both, which costs every consumer a second
// case to test for and lets two spellings of the same state into documents
// that are meant to be diffed.
func TestTheDisplaySpellingOfUnknownIsNotAWireValue(t *testing.T) {
	if StatusUnknown.String() != "unknown" {
		t.Fatalf("StatusUnknown.String() = %q, want a display spelling", StatusUnknown.String())
	}
	var s Status
	if err := json.Unmarshal([]byte(`"unknown"`), &s); err == nil {
		t.Error("the display spelling was accepted as a wire value")
	}
	var w WaitingReason
	if err := json.Unmarshal([]byte(`"unknown"`), &w); err == nil {
		t.Error("the display spelling was accepted as a wire value")
	}
	var l Liveness
	if err := json.Unmarshal([]byte(`"unknown"`), &l); err == nil {
		t.Error("the display spelling was accepted as a wire value")
	}
	// The determined negative next to it IS a wire value, and stays one.
	if err := json.Unmarshal([]byte(`"none"`), &w); err != nil {
		t.Errorf("Unmarshal(\"none\"): %v", err)
	}
	if w != WaitingReasonNone {
		t.Errorf("waiting for nothing decoded to %v", w)
	}
}

// Round trip. null must come back as Unknown rather than as an error, because
// a document that honestly says "no opinion" is a document we wrote ourselves.
func TestEnumsRoundTripThroughJSON(t *testing.T) {
	statuses := []Status{StatusUnknown, StatusIdle, StatusBusy, StatusShell, StatusWaiting}
	for _, want := range statuses {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", want, err)
		}
		var got Status
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Errorf("round trip of %v gave %v", want, got)
		}
	}

	reasons := []WaitingReason{
		WaitingReasonUnknown, WaitingReasonNone,
		WaitingReasonInput, WaitingReasonPermission,
	}
	for _, want := range reasons {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", want, err)
		}
		var got WaitingReason
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Errorf("round trip of %v gave %v", want, got)
		}
	}

	for _, want := range []Liveness{LivenessUnknown, LivenessYes, LivenessNo} {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", want, err)
		}
		var got Liveness
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if got != want {
			t.Errorf("round trip of %v gave %v", want, got)
		}
	}
}

// A name this package did not write is an error, NOT a silent Unknown. The
// leniency that a tool's undocumented format earns stops at the adapter
// boundary: inside our own document an unrecognised name means something
// magmux wrote is wrong, and swallowing it turns a defect into a blank column.
func TestUnrecognisedNameInOurOwnJSONIsAnError(t *testing.T) {
	var s Status
	if err := json.Unmarshal([]byte(`"StatusCompacting"`), &s); err == nil {
		t.Fatal("Unmarshal of an unrecognised name succeeded, want an error")
	}
	if s != StatusUnknown {
		t.Errorf("after a failed Unmarshal, Status = %v, want it left at StatusUnknown", s)
	}
	var w WaitingReason
	if err := json.Unmarshal([]byte(`"WaitingReasonNetwork"`), &w); err == nil {
		t.Fatal("Unmarshal of an unrecognised name succeeded, want an error")
	}
	var l Liveness
	if err := json.Unmarshal([]byte(`"LivenessMaybe"`), &l); err == nil {
		t.Fatal("Unmarshal of an unrecognised name succeeded, want an error")
	}
}

// String names the wire word, so a value printed by a failing test and the
// same value on the wire read identically — one vocabulary, which is the whole
// argument for mirroring Claude's spelling rather than translating it. A value
// outside the enum prints its ordinal rather than claiming to be unknown: a
// corrupt value must look corrupt.
func TestStringNamesTheWireWord(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{StatusUnknown.String(), "unknown"},
		{StatusShell.String(), "shell"},
		{WaitingReasonInput.String(), "input"},
		{LivenessNo.String(), "no"},
		{Status(99).String(), "Status(99)"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
}

// LivenessOf converts a determined bool. There is deliberately no conversion
// the other way, so this test also pins the absence: if someone adds
// Liveness.Bool(), LivenessUnknown has to become true or false, and whichever
// they pick is a wrong answer for half the callers.
func TestLivenessOfConvertsOnlyDeterminedAnswers(t *testing.T) {
	if got := LivenessOf(true); got != LivenessYes {
		t.Errorf("LivenessOf(true) = %v, want LivenessYes", got)
	}
	if got := LivenessOf(false); got != LivenessNo {
		t.Errorf("LivenessOf(false) = %v, want LivenessNo", got)
	}
}
