package claude

import (
	"errors"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

func TestAJobRecordDecodesItsFieldsByName(t *testing.T) {
	j, ps, err := ttJob(t, "blocked-with-needs")
	if err != nil || len(ps) != 0 {
		t.Fatalf("%v %v", err, ps)
	}
	if j.SessionID != "97030e53-3ae6-4b0e-9f5a-1c5d4b2a8e01" || j.Short != "blocked-with-needs" || j.DaemonShort != j.Dir {
		t.Fatalf("%+v", j)
	}
	if j.State != model.StatusBlocked || j.StateRaw != ValueStateBlocked || j.Tempo == nil || *j.Tempo != ValueTempoBlocked {
		t.Fatalf("state %v %q tempo %v", j.State, j.StateRaw, j.Tempo)
	}
	if j.Needs == nil || j.Detail == nil || j.Tokens == nil || *j.Tokens != 47104 {
		t.Fatalf("needs %v detail %v tokens %v", j.Needs, j.Detail, j.Tokens)
	}
	if j.LinkScanPath != "/testdata/projects/-Users-user-dotfiles/97030e53-3ae6-4b0e-9f5a-1c5d4b2a8e01.jsonl" {
		t.Fatalf("the scrubbed prefix: %q", j.LinkScanPath)
	}
	want := time.Date(2026, 8, 25, 3, 15, 47, 319e6, time.UTC)
	if j.UpdatedAt == nil || !j.UpdatedAt.Equal(want) || j.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updatedAt %v", j.UpdatedAt)
	}
	if j.CreatedAt == nil || j.ReapedMidWorkAt != nil || j.Children != nil {
		t.Fatalf("createdAt %v reaped %v children %v", j.CreatedAt, j.ReapedMidWorkAt, j.Children)
	}
}

// The record's own short wins over the directory, and the disagreement is
// reported (review 01-H2.1).
func TestADaemonShortThatIsNotTheDirectoryWinsAndIsReported(t *testing.T) {
	j, ps, _ := ttJob(t, "shortid-mismatch")
	if j.Short != "deadbeef" || j.Dir != "shortid-mismatch" || !hasProblem(ps, model.ProblemShortIDMismatch, KeyDaemonShort) {
		t.Fatalf("%+v %v", j, ps)
	}
	j, _, _ = DecodeJob("abc", []byte(`{"sessionId":"s","state":"working"}`))
	if j.Short != "abc" || j.DaemonShort != "" {
		t.Fatalf("no daemonShort: %+v", j)
	}
}

// D11 extended to jobs: an id-less record is KEPT with a Problem (tt drops it).
func TestAJobWithNoSessionIDIsKeptWithAProblem(t *testing.T) {
	j, ps, err := ttJob(t, "no-session-id")
	if err != nil || j.SessionID != "" || j.Short != "no-session-id" || !hasProblem(ps, model.ProblemMissingField, KeySessionID) {
		t.Fatalf("%+v %v %v", j, ps, err)
	}
}

func TestATornJobRecordIsErrTorn(t *testing.T) {
	if _, _, err := ttJob(t, "torn"); !errors.Is(err, ErrTorn) {
		t.Fatalf("%v", err)
	}
}

// An unrecognised state is Unknown with the raw word kept, and the record is
// still used: refusing it would delete agents the day the vendor adds a state.
func TestAnUnknownJobStateIsUnknownWithTheRawKept(t *testing.T) {
	j, _, err := ttJob(t, "unknown-state")
	if err != nil || j.State != model.StatusUnknown || j.StateRaw != "reticulating" {
		t.Fatalf("%+v %v", j, err)
	}
}

// Children are ARTIFACTS: an object becomes its href, else kind:id, and never a
// bare id that a reader would take for a session id.
func TestChildrenAreArtifactsNotSessionIDs(t *testing.T) {
	j, _, _ := ttJob(t, "blocked-no-needs-tempo-idle")
	if len(j.Children) != 1 || j.Children[0] != "https://example.invalid/pull/158" {
		t.Fatalf("%v", j.Children)
	}
	j, _, _ = DecodeJob("d", []byte(`{"sessionId":"s","children":[{"kind":"pr","id":"9"},"plain",{"id":"7"},{}]}`))
	if len(j.Children) != 3 || j.Children[0] != "pr:9" || j.Children[1] != "plain" || j.Children[2] != "7" {
		t.Fatalf("%v", j.Children)
	}
}

func TestReapedMidWorkIsDecoded(t *testing.T) {
	j, _, _ := ttJob(t, "reaped-mid-work")
	if j.ReapedMidWorkAt == nil || j.State != model.StatusWorking {
		t.Fatalf("%+v", j)
	}
}

// Depth 1 only: a deeper state.json is a decoy, never a record.
func TestOnlyADepthOneStateFileIsAJobRecord(t *testing.T) {
	for rel, want := range map[string]bool{
		"abc/state.json":                true,
		"state.json":                    false,
		"abc/tmp/state.json":            false,
		"be9663df/tmp/a/b/c/state.json": false,
		"abc/timeline.jsonl":            false,
		"../state.json":                 false,
		"abc/state.json.bak":            false,
	} {
		if got := IsJobStatePath(rel); got != want {
			t.Errorf("IsJobStatePath(%q) = %v", rel, got)
		}
	}
}

// The job record's instants are RFC3339; an implausible one is refused with a
// Problem naming the key (C4).
func TestJobInstantsAreRFC3339AndAnImplausibleOneIsReported(t *testing.T) {
	j, ps, _ := DecodeJob("d", []byte(`{"sessionId":"s","createdAt":"1970-01-02T00:00:00Z","updatedAt":"not a time","reapedMidWorkAt":"0001-01-01T00:00:00Z"}`))
	if j.CreatedAt != nil || j.UpdatedAt != nil || j.ReapedMidWorkAt != nil {
		t.Fatalf("%+v", j)
	}
	if !hasProblem(ps, model.ProblemImplausibleInstant, KeyCreatedAt) || !hasProblem(ps, model.ProblemUndecoded, KeyUpdatedAt) {
		t.Fatalf("%v", ps)
	}
	if hasProblem(ps, model.ProblemImplausibleInstant, KeyReapedMidWorkAt) {
		t.Fatalf("an instant before the epoch is no instant, not an implausible one: %v", ps)
	}
}

// Five keys are never decoded at all; Job has no field to carry them.
func TestTheNeverReadKeysHaveNoFieldToArriveIn(t *testing.T) {
	j, _, err := DecodeJob("d", []byte(`{"sessionId":"s","intent":"SECRET","output":"SECRET","suggestedReply":"SECRET"}`))
	if err != nil {
		t.Fatal(err)
	}
	if j.Needs != nil || j.Detail != nil || j.Name != "" || j.CWD != "" {
		t.Fatalf("a never-read key leaked into a field: %+v", j)
	}
}

// A job record with bytes after its object is torn, the same rule as the
// registry's (decodeObject is the one reader of both): an in-place rewrite
// that shrank the record leaves the old tail behind it.
func TestAJobRecordWithATrailingTailIsErrTorn(t *testing.T) {
	for _, b := range []string{`{"sessionId":"s","state":"working"} }`, `{"sessionId":"s","state":"working"}]`} {
		if _, _, err := DecodeJob("d", []byte(b)); !errors.Is(err, ErrTorn) {
			t.Errorf("%s: err %v, want ErrTorn", b, err)
		}
	}
}
