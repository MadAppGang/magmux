package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Former names, as Claude Code writes them in two shapes.
//
// Fix round 2, item 1. On 2026-09-19 Claude Code 2.1.276 was found writing
// each former name as an OBJECT — {name, until, sessionId} — where every
// record in the 2026-09-16 test data wrote a bare string. The decoder expected
// strings and dropped every object, so a renamed session was published with
// previousNames null: no longer findable under the name a human last gave it,
// which is the one thing requirements.md R2 asks of the field.
//
// These tests assert through the PUBLISHED document — PreviousNames and the
// marshalled wire — rather than through a helper, so that the same file
// compiles against the code before the fix and can be shown failing there.

// formerNameNotes is every note ParseRecord wrote about former names.
func formerNameNotes(notes []string) []string {
	var out []string
	for _, n := range notes {
		if strings.Contains(n, "previous name") || strings.Contains(n, KeyFormerNames) {
			out = append(out, n)
		}
	}
	return out
}

// wireDetail is the published detail array, read off the marshalled session:
// one map per former name, keyed by our wire spelling.
func wireDetail(t *testing.T, s any) []map[string]any {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshalling the session: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("reading the marshalled session back: %v", err)
	}
	raw, ok := doc["previousNameDetail"]
	if !ok {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("previousNameDetail is not an array of objects: %s", raw)
	}
	return out
}

// testRecordBytes reads one registry record out of the test data.
func testRecordBytes(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdataSessions(), file))
	if err != nil {
		t.Fatalf("reading test data %s: %v", file, err)
	}
	return b
}

// TestFormerNamesWrittenAsObjectsDecodeToTheirNamesInOrder is the live
// record, byte for byte: 76136.json, copied on 2026-09-19 from a Claude Code
// 2.1.276 session that had been renamed once.
//
// Three things must survive. The NAME, or the session is not findable by it.
// The ORDER, or "what was it called before" has no answer. And `until` —
// Claude Code's own session listing renders this exact peer as "says it was
// agent-fleet-sessions until 5m ago", so the instant carries meaning, and a
// former name reduced to its bare string is the flattening R2 forbids one
// level down.
func TestFormerNamesWrittenAsObjectsDecodeToTheirNamesInOrder(t *testing.T) {
	t.Run("the live record", func(t *testing.T) {
		b := testRecordBytes(t, "76136.json")
		s, notes, err := ParseRecord(b)
		if err != nil {
			t.Fatalf("ParseRecord: %v", err)
		}
		if got := formerNameNotes(notes); len(got) != 0 {
			t.Errorf("a well-formed former name produced notes: %q", got)
		}
		if want := []string{"agent-fleet-sessions"}; !reflect.DeepEqual(s.PreviousNames, want) {
			t.Fatalf("previousNames is %q, want %q — the object shape is Claude Code's current "+
				"format, and dropping it leaves the session unfindable by its former name",
				s.PreviousNames, want)
		}
		detail := wireDetail(t, s)
		if len(detail) != 1 {
			t.Fatalf("previousNameDetail has %d entries, want 1: %v", len(detail), detail)
		}
		// 1789745531896 ms is 2026-09-18T15:32:11.896Z. Verbatim, milliseconds
		// included: the unit is the one KeyStatusUpdatedAt carries, and a
		// decoder that rounded it would pass a whole-second test.
		if got := detail[0]["until"]; got != "2026-09-18T15:32:11.896Z" {
			t.Errorf("until is %v, want 2026-09-18T15:32:11.896Z — the instant the name "+
				"stopped being current, which Claude Code itself displays", got)
		}
		if got := detail[0]["session"]; got != "d1f05570-4e47-4517-b4c5-9bda85d3bd6d" {
			t.Errorf("session is %v, want the id Claude recorded beside the name, verbatim", got)
		}
		if got := detail[0]["name"]; got != "agent-fleet-sessions" {
			t.Errorf("the detail's name is %v, want agent-fleet-sessions", got)
		}
	})

	// The live record has one former name, and one cannot show an order. Two
	// objects, oldest first, as Claude writes a list.
	t.Run("two objects keep their order", func(t *testing.T) {
		rec := `{"sessionId":"s","pid":7,"formerNames":[` +
			`{"name":"first","until":1789000000000,"sessionId":"s"},` +
			`{"name":"second","until":1789100000000,"sessionId":"s"}]}`
		s, notes, err := ParseRecord([]byte(rec))
		if err != nil {
			t.Fatalf("ParseRecord: %v", err)
		}
		if got := formerNameNotes(notes); len(got) != 0 {
			t.Errorf("notes: %q", got)
		}
		if want := []string{"first", "second"}; !reflect.DeepEqual(s.PreviousNames, want) {
			t.Fatalf("previousNames is %q, want %q in the order written", s.PreviousNames, want)
		}
		detail := wireDetail(t, s)
		if len(detail) != 2 || detail[0]["name"] != "first" || detail[1]["name"] != "second" {
			t.Fatalf("previousNameDetail is %v, want first then second", detail)
		}
		if detail[0]["until"] != "2026-09-10T00:26:40Z" || detail[1]["until"] != "2026-09-11T04:13:20Z" {
			t.Errorf("the untils are %v and %v, want each element's own instant",
				detail[0]["until"], detail[1]["until"])
		}
	})
}

// TestFormerNamesWrittenAsStringsDecodeExactlyAsBefore is the other shape.
//
// 45682.json holds two strings. Its PROVENANCE label says REAL SHAPE, and that
// claim is now UNCONFIRMED — no string-shaped record remained on the machine to
// check it against — so the shape is still accepted, and accepted exactly as it
// was: same names, same order, no note.
//
// The last assertion is the new part: a string element has no `until`, and its
// detail entry says so with a null rather than inventing one.
func TestFormerNamesWrittenAsStringsDecodeExactlyAsBefore(t *testing.T) {
	b := testRecordBytes(t, "45682.json")
	s, notes, err := ParseRecord(b)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if want := []string{"magmux-release", "release-prep"}; !reflect.DeepEqual(s.PreviousNames, want) {
		t.Fatalf("previousNames is %q, want %q", s.PreviousNames, want)
	}
	if got := formerNameNotes(notes); len(got) != 0 {
		t.Errorf("string-shaped former names produced notes: %q", got)
	}
	detail := wireDetail(t, s)
	if len(detail) != 2 {
		t.Fatalf("previousNameDetail has %d entries, want one per former name: %v", len(detail), detail)
	}
	for i, d := range detail {
		if d["name"] != s.PreviousNames[i] {
			t.Errorf("detail[%d] names %v, previousNames[%d] is %q — the two arrays must agree "+
				"index for index", i, d["name"], i, s.PreviousNames[i])
		}
		if until, present := d["until"]; !present || until != nil {
			t.Errorf("detail[%d].until is %v (present %v), want null: a bare string records no "+
				"instant, and null is how this document says undetermined", i, until, present)
		}
		if _, present := d["session"]; present {
			t.Errorf("detail[%d] carries a session id the record never wrote: %v", i, d["session"])
		}
	}
}

// TestOneBadFormerNameCostsOnlyItself is the lenient-adapter rule, per
// ELEMENT.
//
// A mixed array: a string, an object, something that is neither, and an
// object with no `until`. The bad one is skipped with ONE note naming its
// index, and every other name survives in order.
func TestOneBadFormerNameCostsOnlyItself(t *testing.T) {
	rec := `{"sessionId":"s","pid":7,"formerNames":[` +
		`"alpha",` +
		`{"name":"beta","until":1789745531896,"sessionId":"an-earlier-session"},` +
		`42,` +
		`{"name":"gamma"}]}`
	s, notes, err := ParseRecord([]byte(rec))
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if want := []string{"alpha", "beta", "gamma"}; !reflect.DeepEqual(s.PreviousNames, want) {
		t.Fatalf("previousNames is %q, want %q — one bad element must cost itself and nothing else",
			s.PreviousNames, want)
	}
	got := formerNameNotes(notes)
	if len(got) != 1 {
		t.Fatalf("got %d notes about former names, want exactly 1 (the bad element): %q", len(got), got)
	}
	if !strings.Contains(got[0], "previous name 2 ") {
		t.Errorf("the note %q does not name index 2, the element that was skipped", got[0])
	}

	detail := wireDetail(t, s)
	if len(detail) != 3 {
		t.Fatalf("previousNameDetail has %d entries, want 3: %v", len(detail), detail)
	}
	// A former name recorded under a different session id is what a resumed or
	// forked session would look like. It is carried verbatim, not dropped.
	if detail[1]["session"] != "an-earlier-session" {
		t.Errorf("beta's session is %v, want the id Claude wrote beside it", detail[1]["session"])
	}
	if detail[2]["until"] != nil {
		t.Errorf("gamma's until is %v, want null — its object carried none", detail[2]["until"])
	}
}

// TestEachFormerNameDegradesOnItsOwn walks the ways ONE element can be wrong,
// and what each costs.
//
// Two classes. An element that names nothing is SKIPPED with a note. An
// element that names something but carries a bad `until` or `sessionId` KEEPS
// its name, loses only the bad part, and is noted: the name is what R2 is
// about, and losing it over a timestamp would be the all-or-nothing failure
// one level down.
func TestEachFormerNameDegradesOnItsOwn(t *testing.T) {
	for _, tt := range []struct {
		name      string
		element   string
		wantNames []string
		wantNote  string // a substring of the one note, or "" for none
	}{
		{"a number", `7`, nil, "previous name 1 "},
		{"null", `null`, nil, "previous name 1 "},
		{"an array", `["x"]`, nil, "previous name 1 "},
		{"an object whose name is a number", `{"name":7}`, nil, "previous name 1 "},
		{"an object with no name", `{"until":1789745531896}`, nil, "previous name 1 "},
		{"an until in SECONDS keeps the name", `{"name":"kept","until":1789745531}`,
			[]string{"kept"}, "previous name 1 "},
		{"an until that is text keeps the name", `{"name":"kept","until":"yesterday"}`,
			[]string{"kept"}, "previous name 1 "},
		{"a session id that is not text keeps the name", `{"name":"kept","sessionId":7}`,
			[]string{"kept"}, "previous name 1 "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := `{"sessionId":"s","pid":7,"formerNames":["first",` + tt.element + `]}`
			s, notes, err := ParseRecord([]byte(rec))
			if err != nil {
				t.Fatalf("ParseRecord: %v", err)
			}
			want := append([]string{"first"}, tt.wantNames...)
			if !reflect.DeepEqual(s.PreviousNames, want) {
				t.Errorf("previousNames is %q, want %q", s.PreviousNames, want)
			}
			got := formerNameNotes(notes)
			if len(got) != 1 || !strings.Contains(got[0], tt.wantNote) {
				t.Errorf("notes are %q, want exactly one naming %q", got, tt.wantNote)
			}
			if len(tt.wantNames) == 1 {
				detail := wireDetail(t, s)
				if len(detail) != 2 || detail[1]["name"] != "kept" {
					t.Fatalf("previousNameDetail is %v, want the kept name second", detail)
				}
				if strings.Contains(tt.name, "until") && detail[1]["until"] != nil {
					t.Errorf("an unusable until was published as %v; it must be null", detail[1]["until"])
				}
				if _, present := detail[1]["session"]; strings.Contains(tt.name, "session") && present {
					t.Errorf("an unusable session id was published as %v", detail[1]["session"])
				}
			}
		})
	}

	// A formerNames that is not a list at all used to vanish without a word.
	// It is the same format change as an object element, one level up, and it
	// is named the same way.
	t.Run("a formerNames that is not a list", func(t *testing.T) {
		s, notes, err := ParseRecord([]byte(`{"sessionId":"s","pid":7,"formerNames":{"name":"x"}}`))
		if err != nil {
			t.Fatalf("ParseRecord: %v", err)
		}
		if s.PreviousNames != nil {
			t.Errorf("previousNames is %q, want none", s.PreviousNames)
		}
		if got := formerNameNotes(notes); len(got) != 1 {
			t.Errorf("notes are %q, want exactly one saying %s was not a list", got, KeyFormerNames)
		}
	})
	// And null is absence, which is ordinary and says nothing.
	t.Run("a null formerNames is absence", func(t *testing.T) {
		_, notes, err := ParseRecord([]byte(`{"sessionId":"s","pid":7,"formerNames":null}`))
		if err != nil {
			t.Fatalf("ParseRecord: %v", err)
		}
		if got := formerNameNotes(notes); len(got) != 0 {
			t.Errorf("a null formerNames produced notes: %q", got)
		}
	})
}

// TestAFormerNameNoteQuotesWhatItWasGiven — the notes reach a terminal on
// stderr (`magmux agents` prints every problem), and a former name is text that
// can be generated from conversation content. A note that echoed a bad element
// raw would carry its control bytes to the terminal; %q escapes them.
func TestAFormerNameNoteQuotesWhatItWasGiven(t *testing.T) {
	hostile, err := json.Marshal("\x1b]52;c;aGk=\x07")
	if err != nil {
		t.Fatalf("building the record: %v", err)
	}
	rec := `{"sessionId":"s","pid":7,"formerNames":[{"name":"kept","until":` + string(hostile) + `}]}`
	_, notes, err := ParseRecord([]byte(rec))
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	for _, n := range notes {
		for _, r := range n {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("a note carries control character %U raw: %q", r, n)
			}
		}
	}
}
