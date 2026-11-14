package claude

import (
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

var listingAt = time.Date(2026, 9, 18, 2, 40, 11, 204e6, time.UTC)

func decodeListing(t *testing.T, name string) (AgentsListing, []model.Problem, error) {
	t.Helper()
	return DecodeAgents(readCorpus(t, "agents", name), listingAt)
}

// The listing is a FLAT array of two populations with two key sets, and it is
// sorted by session id because every first-wins rule needs the order to be ours.
func TestTheLiveListingDecodesBothPopulations(t *testing.T) {
	l, ps, err := decodeListing(t, "real-39.json")
	if err != nil || len(ps) != 0 {
		t.Fatalf("%v %v", err, ps)
	}
	if len(l.Entries) != 39 || !l.At.Equal(listingAt) {
		t.Fatalf("%d entries at %v", len(l.Entries), l.At)
	}
	var interactive, background int
	for i, e := range l.Entries {
		if i > 0 && l.Entries[i-1].SessionID > e.SessionID {
			t.Fatal("not sorted by session id")
		}
		switch {
		case IsInteractiveEntry(e):
			interactive++
			if e.PID == nil || e.StatusRaw == "" || e.Status == model.StatusUnknown {
				t.Errorf("interactive entry %+v", e)
			}
		default:
			background++
			if e.PID != nil || e.ID == "" || e.State != model.StatusBlocked || e.Kind != model.KindBackground {
				t.Errorf("background entry %+v", e)
			}
		}
		if e.StartedAt == nil {
			t.Errorf("startedAt is epoch milliseconds on every entry: %+v", e)
		}
	}
	if interactive != 36 || background != 3 {
		t.Fatalf("%d interactive, %d background", interactive, background)
	}
}

func TestTheWaitingEntryCarriesItsReason(t *testing.T) {
	l, _, _ := decodeListing(t, "real-37-with-waitingfor.json")
	var found bool
	for _, e := range l.Entries {
		if e.Status == model.StatusWaiting {
			found = e.Waiting == model.WaitingInput && e.WaitingRaw == ValueWaitingInput
		}
	}
	if !found {
		t.Fatal("no waiting entry with an input-needed reason")
	}
}

// C12: "[]" is a valid empty listing; malformed output is an ERROR, never an
// empty success, which would be positive evidence of death for every job.
func TestEmptyIsNotMalformed(t *testing.T) {
	l, _, err := decodeListing(t, "empty.json")
	if err != nil || len(l.Entries) != 0 {
		t.Fatalf("empty: %+v %v", l, err)
	}
	for _, b := range []string{"claude: error: unknown command\n", `{"agents":[]}`, ``, `[{"sessionId":`} {
		if _, _, err := DecodeAgents([]byte(b), listingAt); err == nil {
			t.Errorf("%q decoded without an error", b)
		}
	}
	if _, _, err := decodeListing(t, "malformed.json"); err == nil {
		t.Fatal("malformed.json decoded")
	}
}

func TestAnEntryWithNoSessionIDIsSkippedNotFatal(t *testing.T) {
	l, _, err := DecodeAgents([]byte(`[{"pid":1,"kind":"interactive"},{"sessionId":"b","pid":2,"status":"idle"},{"sessionId":"a","id":"x","state":"done","kind":"background"}]`), listingAt)
	if err != nil || len(l.Entries) != 2 || l.Entries[0].SessionID != "a" {
		t.Fatalf("%+v %v", l, err)
	}
	if !IsInteractiveEntry(l.Entries[1]) || IsInteractiveEntry(l.Entries[0]) {
		t.Fatal("an unlabelled entry with a pid is interactive; a background one is not")
	}
	if !l.Lists("a") || l.Lists("") || l.Lists("zz") {
		t.Fatal("Lists")
	}
}

func TestAnImplausibleListingInstantIsReported(t *testing.T) {
	l, ps, err := DecodeAgents([]byte(`[{"sessionId":"a","pid":1,"status":"idle","startedAt":1789536674}]`), listingAt)
	if err != nil || l.Entries[0].StartedAt != nil || !hasProblem(ps, model.ProblemImplausibleInstant, KeyStartedAt) {
		t.Fatalf("%+v %v %v", l, ps, err)
	}
}

func TestTheUnregisteredInteractiveEntryKeepsItsPID(t *testing.T) {
	l, _, _ := decodeListing(t, "interactive-no-registry.json")
	if len(l.Entries) != 1 || l.Entries[0].PID == nil || *l.Entries[0].PID != 987654 || !IsInteractiveEntry(l.Entries[0]) {
		t.Fatalf("%+v", l.Entries)
	}
}
