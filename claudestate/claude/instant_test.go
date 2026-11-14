package claude

import (
	"errors"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// D4: one instant decoder. Milliseconds only inside 1e12..1e14; <= 0 is nil;
// anything else is refused, never rescaled. Re-expressed from magmux's
// TestInstantsAreMilliseconds and TestASecondsShapedInstantIsRefusedNotConverted.
func TestDecodeInstantMillisAcceptsOnlyMilliseconds(t *testing.T) {
	cases := []struct {
		ms      int64
		wantNil bool
		wantErr bool
	}{
		{1789536692864, false, false},
		{1_000_000_000_000, false, false},
		{100_000_000_000_000, false, false},
		{0, true, false},
		{-5, true, false},
		{1789536692, true, true},          // seconds: 1970 if believed
		{1789536692864000, true, true},    // microseconds
		{999_999_999_999, true, true},     // one below the floor
		{100_000_000_000_001, true, true}, // one above the ceiling
	}
	for _, c := range cases {
		got, err := DecodeInstantMillis(c.ms)
		if (got == nil) != c.wantNil || errors.Is(err, ErrImplausibleInstant) != c.wantErr {
			t.Errorf("DecodeInstantMillis(%d) = %v, %v", c.ms, got, err)
		}
	}
	got, _ := DecodeInstantMillis(1789536692864)
	if got.Location() != time.UTC || got.UnixMilli() != 1789536692864 {
		t.Fatalf("%v", got)
	}
}

func TestDecodeInstantStringIsRFC3339AtMillisecondsUTC(t *testing.T) {
	got, err := DecodeInstantString("2026-08-25T05:15:47.319789+02:00")
	if err != nil || !got.Equal(time.Date(2026, 8, 25, 3, 15, 47, 319e6, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("%v %v", got, err)
	}
	for s, want := range map[string]bool{"": false, "1970-01-01T00:00:00Z": false, "0001-01-01T00:00:00Z": false} {
		if got, err := DecodeInstantString(s); got != nil || err != nil || want {
			t.Errorf("%q: %v %v", s, got, err)
		}
	}
	if _, err := DecodeInstantString("1999-01-01T00:00:00Z"); !errors.Is(err, ErrImplausibleInstant) {
		t.Errorf("before 2001: %v", err)
	}
	if _, err := DecodeInstantString("yesterday"); err == nil || errors.Is(err, ErrImplausibleInstant) {
		t.Errorf("unparseable: %v", err)
	}
}

// The corpus carries millisecond instants everywhere (magmux's
// TestEveryRecordInTheCorpusCarriesMillisecondInstants): a corpus in the wrong
// unit would produce problems here, not silently wrong dates.
func TestEveryCorpusRecordInstantIsPlausible(t *testing.T) {
	for _, root := range []string{"magmux-corpus", "tt-corpus"} {
		entries, err := readDirNames(rootDir(root, "sessions"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range entries {
			if !IsRecordName(name) {
				continue
			}
			_, ps, err := DecodeRecord(name, readCorpus(t, "roots", root, "sessions", name))
			if err != nil {
				continue // the torn and empty records
			}
			if hasProblem(ps, model.ProblemImplausibleInstant, "") {
				t.Errorf("%s/%s: %v", root, name, ps)
			}
		}
	}
}

func TestRetentionIsTTsRule(t *testing.T) {
	for in, want := range map[string]time.Duration{
		`{"cleanupPeriodDays": 14}`:   14 * 24 * time.Hour,
		`{"cleanupPeriodDays": "14"}`: 0,
		`{"cleanupPeriodDays": 0}`:    0,
		`{"cleanupPeriodDays": -3}`:   0,
		`{"cleanupPeriodDays": 1.5}`:  0,
		`{}`:                          0,
		`not json`:                    0,
	} {
		got, ok := DecodeRetention([]byte(in))
		if got != want || ok != (want > 0) {
			t.Errorf("DecodeRetention(%s) = %v, %v", in, got, ok)
		}
	}
	if DefaultTranscriptRetention != 30*24*time.Hour {
		t.Fatal("tt's default is 30 days")
	}
}

func TestTimelineLinesDecodeForTheDoctor(t *testing.T) {
	b := readCorpus(t, "roots", "tt-corpus", "jobs", "blocked-with-needs", JobTimelineFile)
	var n int
	for _, line := range splitLines(b) {
		l, ok := DecodeTimelineLine(line)
		if !ok || l.At == nil || len(l.Keys) != 3 {
			t.Fatalf("%s: %+v %v", line, l, ok)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("%d lines", n)
	}
	l, ok := DecodeTimelineLine([]byte(`{"at":"1970-01-02T00:00:00Z"}`))
	if !ok || l.At != nil || !l.BadInstant {
		t.Fatalf("%+v", l)
	}
	if _, ok := DecodeTimelineLine([]byte(`x`)); ok {
		t.Fatal("not an object")
	}
}
