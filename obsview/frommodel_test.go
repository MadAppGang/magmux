package obsview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// TestFromModelMapsTheOneVocabularyOntoMagmuxsFourWords is §4.9's status
// table: no word is added to the wire's enum (A4), a job's working is busy,
// its blocked is waiting, and done, failed and unknown are null with the raw
// word beside them.
func TestFromModelMapsTheOneVocabularyOntoMagmuxsFourWords(t *testing.T) {
	for _, tc := range []struct {
		in   model.Status
		want Status
	}{
		{model.StatusUnknown, StatusUnknown},
		{model.StatusIdle, StatusIdle},
		{model.StatusBusy, StatusBusy},
		{model.StatusShell, StatusShell},
		{model.StatusWaiting, StatusWaiting},
		{model.StatusWorking, StatusBusy},
		{model.StatusBlocked, StatusWaiting},
		{model.StatusDone, StatusUnknown},
		{model.StatusFailed, StatusUnknown},
	} {
		if got := FromModelSession(model.Session{Status: tc.in, StatusRaw: "raw"}); got.Status != tc.want || got.StatusRaw != "raw" {
			t.Errorf("%s maps to %s (raw %q), want %s with the raw word kept", tc.in, got.Status, got.StatusRaw, tc.want)
		}
	}
}

// TestAFinishedJobIsNullAndAuthoritative is the pair §4.9 names: the job said
// it, in a word magmux's enum does not have.
func TestAFinishedJobIsNullAndAuthoritative(t *testing.T) {
	w := FromModelSession(model.Session{
		Key: model.Key{SessionID: "id-a", Job: "j1"}, Status: model.StatusDone, StatusRaw: "done",
		Confidence: model.ConfidenceAuthoritative, Job: &model.JobFacts{Short: "j1"},
	})
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status":null`, `"statusRaw":"done"`, `"confidence":"authoritative"`, `"job":"j1"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("a finished job's row lacks %s: %s", want, b)
		}
	}
}

// TestTheWaitingReasonIsOnlyWhereItApplies is D14 on magmux's wire: a reason
// is published only when the effective status is waiting or blocked, input
// and permission have words, and a dialog is null with its raw word.
func TestTheWaitingReasonIsOnlyWhereItApplies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      model.Session
		want    WaitingReason
		wantRaw string
	}{
		{"waiting for input", model.Session{Status: model.StatusWaiting, Waiting: model.WaitingInput, WaitingRaw: "input needed"}, WaitingReasonInput, "input needed"},
		{"waiting for permission", model.Session{Status: model.StatusWaiting, Waiting: model.WaitingPermission, WaitingRaw: "permission prompt"}, WaitingReasonPermission, "permission prompt"},
		{"a dialog", model.Session{Status: model.StatusWaiting, Waiting: model.WaitingDialog, WaitingRaw: "dialog open"}, WaitingReasonUnknown, "dialog open"},
		{"a blocked job", model.Session{Status: model.StatusBlocked}, WaitingReasonUnknown, ""},
		{"a busy session with a stale reason", model.Session{Status: model.StatusBusy, Waiting: model.WaitingInput, WaitingRaw: "input needed"}, WaitingReasonUnknown, ""},
	} {
		w := FromModelSession(tc.in)
		if w.Waiting != tc.want || w.WaitingRaw != tc.wantRaw {
			t.Errorf("%s: waitingOn %s raw %q, want %s raw %q", tc.name, w.Waiting, w.WaitingRaw, tc.want, tc.wantRaw)
		}
	}
}

// TestAnAmbiguousTranscriptPublishesNoTranscriptFacts is D5 on the wire: no
// corpus session sits on an ambiguous id, so the goldens never show it, and
// this is where it is pinned. Nothing was opened, so subagentCount, color and
// fault.apiErrors are null.
func TestAnAmbiguousTranscriptPublishesNoTranscriptFacts(t *testing.T) {
	w := FromModelSession(model.Session{
		Key:        model.Key{SessionID: "id-a", PID: 7},
		Transcript: &model.TranscriptFacts{Location: model.Location{Matches: 2}, SubagentEvidence: model.EvidenceAmbiguous},
	})
	if w.SubagentCount != nil || w.Color != nil || w.Fault.APIErrors != nil {
		t.Errorf("an ambiguous transcript published facts: subagents %v, color %v, apiErrors %v", w.SubagentCount, w.Color, w.Fault.APIErrors)
	}
}

// TestTheTranscriptFactsAndTheFaultReachTheWire: the located transcript's
// counts and colour, and the fault's text with its window (D10).
func TestTheTranscriptFactsAndTheFaultReachTheWire(t *testing.T) {
	sub, errs, colour := 2, 1, "teal"
	w := FromModelSession(model.Session{
		Transcript: &model.TranscriptFacts{Subagents: &sub, Color: &colour},
		Fault: model.Fault{APIErrors: &errs, LastMessage: "API Error: Connection error.",
			Window: model.Window{TailBytes: 200 << 10, FileSize: 3 << 20, Partial: true}},
	})
	if w.SubagentCount == nil || *w.SubagentCount != 2 || w.Color == nil || *w.Color != "teal" {
		t.Errorf("transcript facts lost: %+v", w)
	}
	if w.Fault.APIErrors == nil || *w.Fault.APIErrors != 1 {
		t.Errorf("apiErrors %v", w.Fault.APIErrors)
	}
	want := "API Error: Connection error. (counted in the last 200 KB of a 3.0 MB transcript; errors before that are not counted)"
	if w.Fault.Detail != want {
		t.Errorf("fault.detail %q, want %q", w.Fault.Detail, want)
	}
	zero := 0
	if d := faultDetail(model.Fault{APIErrors: &zero, Window: model.Window{TailBytes: 200 << 10, FileSize: 3 << 20, Partial: true}}); !strings.HasPrefix(d, "no API-level error in the last 200 KB") {
		t.Errorf("a zero count over a partial window says %q; it must say how much was examined", d)
	}
	if d := faultDetail(model.Fault{APIErrors: &zero, Window: model.Window{TailBytes: 200 << 10, FileSize: 10}}); d != "" {
		t.Errorf("a zero count over a whole transcript says %q, want nothing", d)
	}
}

// TestThePaneRefIsMagmuxsDecimalID is ADR-X1: magmux formats its int and
// parses it back, and nothing else is an address.
func TestThePaneRefIsMagmuxsDecimalID(t *testing.T) {
	for _, id := range []int{0, 3, 1024} {
		got, ok := PaneID(PaneRef(id))
		if !ok || got != id {
			t.Errorf("pane %d round-trips to %d, %v", id, got, ok)
		}
	}
	for _, ref := range []model.PaneRef{"", "-1", "02", "%107", "3a", " 3"} {
		if _, ok := PaneID(ref); ok {
			t.Errorf("%q parsed as a pane id", ref)
		}
	}
	if w := FromModelSession(model.Session{Pane: "3"}); w.PaneID == nil || *w.PaneID != 3 {
		t.Errorf("paneId %v, want 3", w.PaneID)
	}
	if w := FromModelSession(model.Session{}); w.PaneID != nil {
		t.Errorf("no pane mapped to paneId %d", *w.PaneID)
	}
}

// TestTheIdentityAndFactsAreTheSessions: id, pid, job, name, launch directory,
// the previous names in order with their detail, liveness, attention.
func TestTheIdentityAndFactsAreTheSessions(t *testing.T) {
	until := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	yes := true
	w := FromModelSession(model.Session{
		Key: model.Key{SessionID: "id-a", Job: "j1"}, PID: 0, Name: "now",
		PreviousNames: []model.PreviousName{{Name: "old", Until: &until, SessionID: "id-z"}, {Name: "older"}},
		LaunchDir:     "/home/someone/dev", CWD: "/elsewhere",
		ProcessAlive: model.LivenessNo, PaneAlive: model.LivenessYes,
		Attention: model.Attention{Needed: &yes, Reason: "because"},
	})
	if w.ID != "id-a" || w.Job != "j1" || w.PID != 0 || w.Name != "now" || w.LaunchDir != "/home/someone/dev" {
		t.Errorf("identity lost: %+v", w)
	}
	if strings.Join(w.PreviousNames, ",") != "old,older" || len(w.PreviousNameDetail) != 2 ||
		w.PreviousNameDetail[0].Session != "id-z" || !w.PreviousNameDetail[0].Until.Equal(until) {
		t.Errorf("previous names lost: %v %+v", w.PreviousNames, w.PreviousNameDetail)
	}
	if w.ProcessAlive != LivenessNo || w.PaneAlive != LivenessYes || w.Attention.Needed == nil || !*w.Attention.Needed {
		t.Errorf("liveness or attention lost: %+v", w)
	}
	b, _ := json.Marshal(w)
	if strings.Contains(string(b), "/elsewhere") {
		t.Errorf("a record's live cwd reached the wire: %s", b)
	}
	if b, _ := json.Marshal(FromModelSession(model.Session{Key: model.Key{SessionID: "id-b", PID: 4}})); strings.Contains(string(b), `"job"`) {
		t.Errorf("a row with no job carries the job member: %s", b)
	}
}
