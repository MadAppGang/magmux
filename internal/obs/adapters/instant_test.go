package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The corpus's own reference instant, as milliseconds. PROVENANCE.md names it
// as `NOW = 1789545200` in SECONDS, which is the spelling that caused the bug
// this file exists to guard; it is written out here in the unit the records
// actually use.
const nowMillis = int64(1789545200_000)

// WRONG: reading Claude's timestamps as epoch SECONDS, which is what this
// package's own documentation said until 2026-09-18.
//
// It is worth being precise about how badly this fails, because the failure is
// silent in both directions. A real 13-digit value read as seconds is the year
// 58600. A 10-digit value read as milliseconds is January 1970. Neither errors,
// neither panics, both marshal to a perfectly well-formed RFC 3339 string, and
// a `statusSince` of "+58600-05-21T..." is the sort of thing a reader skims
// past once and never questions again.
//
// The measurement that settled it is in the doc comment on KeyStatusUpdatedAt:
// every one of the 34 live records carries 13 digits, and the one live record
// that had not been rewritten since the corpus was captured holds exactly the
// corpus's value times a thousand, 36ms before its own mtime.
func TestInstantsAreMilliseconds(t *testing.T) {
	got, ok := DecodeInstant(1789536692864)
	if !ok {
		t.Fatalf("DecodeInstant refused a 13-digit value, which is what every live record carries")
	}
	want := time.Date(2026, 9, 16, 5, 31, 32, 864_000_000, time.UTC)
	if !got.Equal(want) {
		t.Errorf("decoded %s, want %s — the value is epoch milliseconds", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
	if _, offset := got.Zone(); offset != 0 {
		t.Errorf("decoded instant is not UTC (offset %ds); two machines would build different bytes for one document", offset)
	}
}

// WRONG: accepting a seconds-shaped value — or worse, "helpfully" multiplying
// it by a thousand.
//
// Converting is the tempting fix and it is the dangerous one. A decoder that
// silently rescales a 10-digit value makes a corpus with the wrong unit test
// green forever, which is exactly how this survived phase 0 and phase 1: every
// record in internal/obs/testdata carried seconds, every test agreed with
// every record, and nothing anywhere compared either against the live registry.
//
// Refusing is also the honest answer for the case that is NOT a unit error: a
// value this package cannot make sense of yields "no opinion", the raw number
// stays visible to whoever is looking, and nobody is handed a date that was
// invented for them.
func TestASecondsShapedInstantIsRefusedNotConverted(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    int64
	}{
		{"the corpus's reference instant as seconds", 1789545200},
		{"a live record's value divided by a thousand", 1789536692},
		{"zero", 0},
		{"negative", -1},
		{"microseconds", 1789536692864000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DecodeInstant(tc.v)
			if ok {
				t.Fatalf("DecodeInstant(%d) answered %s. A 10-digit value is SECONDS: "+
					"these keys are epoch milliseconds, measured against the live registry "+
					"on 2026-09-18. Do not rescale it here — a converting decoder makes a "+
					"corpus with the wrong unit green forever.", tc.v, got.Format(time.RFC3339))
			}
			if !got.IsZero() {
				t.Errorf("a refused value returned %s, want the zero instant", got)
			}
		})
	}
}

// WRONG: a corpus in one unit and a live registry in another, which is a bug
// that cannot be found from inside the corpus.
//
// This reads the test data — never ~/.claude — and asserts that every record
// carries a value this package's own decoder accepts. It is the cheap half of
// the guard: the expensive half was noticing at all.
func TestEveryRecordInTheCorpusCarriesMillisecondInstants(t *testing.T) {
	dir := filepath.Join("..", "testdata", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	var checked int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var rec map[string]any
		if json.Unmarshal(b, &rec) != nil {
			// 60004.json is a torn read and 60005.json is empty. Both are
			// deliberate, both are the registry source's problem rather than
			// this one's, and skipping them here is not hiding anything: the
			// count below proves the walk still saw the rest.
			continue
		}
		for _, key := range []string{KeyStatusUpdatedAt, KeyUpdatedAt} {
			raw, present := rec[key]
			if !present {
				continue
			}
			n, isNumber := raw.(float64)
			if !isNumber {
				t.Errorf("%s: %s is %T, want a number", e.Name(), key, raw)
				continue
			}
			checked++
			if _, ok := DecodeInstant(int64(n)); !ok {
				t.Errorf("%s: %s is %.0f, which this package refuses. If it has ten digits "+
					"it is SECONDS, and the corpus has drifted from the live registry again — "+
					"see PROVENANCE.md, Corrections.", e.Name(), key, n)
			}
		}
	}
	// A walk that checked nothing would pass silently, which is the one way a
	// data-inspection test can lie.
	if checked < 30 {
		t.Fatalf("checked only %d instants under %s; the walk is not reaching the records", checked, dir)
	}
}

// WRONG: a reader that believes its own reference instant is in seconds. The
// corpus's NOW is documented in seconds and every record is in milliseconds,
// so the two have to be reconciled ONCE, out loud, rather than in each test.
func TestTheCorpusReferenceInstantIsAMillisecondValue(t *testing.T) {
	if _, ok := DecodeInstant(nowMillis); !ok {
		t.Fatalf("the reference instant %d is not a plausible millisecond value", nowMillis)
	}
	if _, ok := DecodeInstant(nowMillis / 1000); ok {
		t.Fatal("the reference instant in SECONDS was accepted, which is the trap this guards")
	}
}
