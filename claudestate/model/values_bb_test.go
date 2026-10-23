package model_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

func TestBBKeyStringAndCompare(t *testing.T) {
	tests := []struct {
		key  model.Key
		want string
	}{
		{model.Key{SessionID: "sid", PID: 42}, "sid/pid:42"},
		{model.Key{SessionID: "sid", Job: "short"}, "sid/job:short"},
		{model.Key{Orphan: "idless.json"}, "orphan:idless.json"},
		{model.Key{Orphan: "jobs/idless"}, "orphan:jobs/idless"},
	}
	for _, tt := range tests {
		if got := tt.key.String(); got != tt.want {
			t.Errorf("%+v.String() = %q, want %q", tt.key, got, tt.want)
		}
	}
	ordered := []model.Key{
		{},
		{Orphan: "a"},
		{Orphan: "b"},
		{Job: "a"},
		{PID: 1},
		{SessionID: "a"},
		{SessionID: "b"},
	}
	for i, a := range ordered {
		for j, b := range ordered {
			got := a.Compare(b)
			if (got < 0) != (i < j) || (got == 0) != (i == j) || (got > 0) != (i > j) {
				t.Errorf("Compare(%+v, %+v) = %d; want ordering of %d and %d", a, b, got, i, j)
			}
		}
	}
}

func TestBBModelEnumsKeepUnknownZeroAndDiagnosticStrings(t *testing.T) {
	tests := []struct {
		value fmt.Stringer
		want  string
	}{
		{model.KindUnknown, "unknown"}, {model.KindBackground, "background"},
		{model.StatusUnknown, "unknown"}, {model.StatusBlocked, "blocked"},
		{model.WaitingUnknown, "unknown"}, {model.WaitingDialog, "dialog"},
		{model.LivenessUnknown, "unknown"}, {model.LivenessYes, "yes"},
		{model.ConfidenceUnknown, "unknown"}, {model.ConfidenceAuthoritative, "authoritative"},
		{model.EvidenceUnknown, "unknown"}, {model.EvidenceAbsent, "absent"},
		{model.ShellMarkUnknown, "unknown"}, {model.ShellMarkPrompt, "prompt"},
		{model.SourceUnknown, "unknown"}, {model.SourceRegistry, "registry"},
		{model.SourceStateUnknown, "unknown"}, {model.SourceDegraded, "degraded"},
		{model.TargetUnknown, "unknown"}, {model.TargetTranscript, "transcript"},
		{model.ScopeUnknown, "unknown"}, {model.ScopeRecord, "record"},
		{model.ProblemUnknown, "unknown"},
		{model.Kind(255), "Kind(255)"},
		{model.Status(255), "Status(255)"},
		{model.WaitingReason(255), "WaitingReason(255)"},
		{model.Liveness(255), "Liveness(255)"},
		{model.Confidence(255), "Confidence(255)"},
		{model.Evidence(255), "Evidence(255)"},
		{model.ShellMark(255), "ShellMark(255)"},
		{model.SourceID(255), "SourceID(255)"},
		{model.SourceState(255), "SourceState(255)"},
		{model.TargetKind(255), "TargetKind(255)"},
		{model.Scope(255), "Scope(255)"},
		{model.ProblemKind(255), "ProblemKind(255)"},
	}
	for _, tt := range tests {
		if got := tt.value.String(); got != tt.want {
			t.Errorf("%T.String() = %q, want %q", tt.value, got, tt.want)
		}
	}
}

func TestBBStatusPredicatesReadEffectiveStatusOnly(t *testing.T) {
	for _, status := range []model.Status{
		model.StatusUnknown, model.StatusIdle, model.StatusBusy, model.StatusShell,
		model.StatusWaiting, model.StatusWorking, model.StatusBlocked, model.StatusDone, model.StatusFailed,
	} {
		t.Run(status.String(), func(t *testing.T) {
			waiting := status == model.StatusWaiting || status == model.StatusBlocked
			if status.IsWaiting() != waiting {
				t.Errorf("IsWaiting(%v) = %v, want %v", status, status.IsWaiting(), waiting)
			}
			if got, want := status.IsWorking(), status == model.StatusBusy || status == model.StatusWorking; got != want {
				t.Errorf("IsWorking(%v) = %v, want %v", status, got, want)
			}
			if got, want := status.IsTerminal(), status == model.StatusDone || status == model.StatusFailed; got != want {
				t.Errorf("IsTerminal(%v) = %v, want %v", status, got, want)
			}
			s := model.Session{Status: status, Waiting: model.WaitingPermission, Registry: &model.RegistryFacts{Status: model.StatusWaiting}}
			if s.WaitingApplies() != waiting {
				t.Errorf("WaitingApplies consulted a source's status instead of effective %v", status)
			}
		})
	}
}

func bbPtr[T any](v T) *T { return &v }

func bbRichSession() model.Session {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return model.Session{
		Key: model.Key{SessionID: "sid", Job: "short"}, PID: 42, Claimants: []int{42, 43},
		Name: "name", PreviousNames: []model.PreviousName{{Name: "old", Until: bbPtr(at), SessionID: "before"}},
		StartedAt: bbPtr(at), StatusSince: bbPtr(at),
		Registry:            &model.RegistryFacts{File: "42.json", StatusSince: bbPtr(at), UpdatedAt: bbPtr(at)},
		MessagingSocketPath: bbPtr("socket"), Tmux: &model.TmuxRef{Raw: "$1:@2:%3", Pane: "%3"},
		ListedBy: []model.SourceID{model.SourceRegistry, model.SourceJobs},
		Job: &model.JobFacts{
			Dir: "short", Tempo: bbPtr("blocked"), Needs: bbPtr("permission"), Detail: bbPtr("detail"),
			Tokens: bbPtr(int64(12)), Children: []string{"child"},
			CreatedAt: bbPtr(at), UpdatedAt: bbPtr(at), ReapedMidWorkAt: bbPtr(at),
		},
		Transcript: &model.TranscriptFacts{
			Location:  model.Location{Path: "project/sid.jsonl", Matches: 1},
			APIErrors: bbPtr(1), Color: bbPtr("blue"), Subagents: bbPtr(2),
			ContextTokens: bbPtr(int64(9)), Compactions: bbPtr(1),
			LastActivityAt: bbPtr(at), LatestTextAt: bbPtr(at), LatestText: "answer",
		},
		Attention: model.Attention{Needed: bbPtr(true), Reason: "reason"},
		Fault:     model.Fault{APIErrors: bbPtr(1), LastMessage: "raw"},
		Conflict:  &model.Conflict{Status: model.StatusBusy, Detail: "conflict"},
	}
}

func bbMutateSession(s *model.Session) {
	s.Key.SessionID = "changed"
	s.Claimants[0] = 99
	s.PreviousNames[0].Name = "changed"
	*s.PreviousNames[0].Until = s.PreviousNames[0].Until.Add(time.Hour)
	*s.StartedAt = s.StartedAt.Add(time.Hour)
	*s.StatusSince = s.StatusSince.Add(time.Hour)
	s.Registry.File = "changed"
	*s.Registry.StatusSince = s.Registry.StatusSince.Add(time.Hour)
	*s.Registry.UpdatedAt = s.Registry.UpdatedAt.Add(time.Hour)
	*s.MessagingSocketPath = "changed"
	s.Tmux.Pane = "%99"
	s.ListedBy[0] = model.SourceAgents
	s.Job.Dir = "changed"
	*s.Job.Tempo, *s.Job.Needs, *s.Job.Detail = "changed", "changed", "changed"
	*s.Job.Tokens = 99
	s.Job.Children[0] = "changed"
	*s.Job.CreatedAt = s.Job.CreatedAt.Add(time.Hour)
	*s.Job.UpdatedAt = s.Job.UpdatedAt.Add(time.Hour)
	*s.Job.ReapedMidWorkAt = s.Job.ReapedMidWorkAt.Add(time.Hour)
	s.Transcript.Location.Path = "changed"
	*s.Transcript.APIErrors, *s.Transcript.Subagents, *s.Transcript.Compactions = 99, 99, 99
	*s.Transcript.Color = "changed"
	*s.Transcript.ContextTokens = 99
	*s.Transcript.LastActivityAt = s.Transcript.LastActivityAt.Add(time.Hour)
	*s.Transcript.LatestTextAt = s.Transcript.LatestTextAt.Add(time.Hour)
	*s.Attention.Needed = false
	*s.Fault.APIErrors = 99
	s.Conflict.Detail = "changed"
}

func TestBBSessionCloneHasNoSharedPointersOrSlices(t *testing.T) {
	for _, mutateOriginal := range []bool{true, false} {
		t.Run(fmt.Sprintf("mutateOriginal=%v", mutateOriginal), func(t *testing.T) {
			original := bbRichSession()
			copy := original.Clone()
			if !reflect.DeepEqual(copy, original) {
				t.Fatal("Clone changed the value before mutation")
			}
			if mutateOriginal {
				bbMutateSession(&original)
				if !reflect.DeepEqual(copy, bbRichSession()) {
					t.Fatal("mutating original changed clone")
				}
			} else {
				bbMutateSession(&copy)
				if !reflect.DeepEqual(original, bbRichSession()) {
					t.Fatal("mutating clone changed original")
				}
			}
		})
	}
	if got := (model.Session{}).Clone(); !reflect.DeepEqual(got, model.Session{}) {
		t.Errorf("zero session clone = %+v", got)
	}
}

func TestBBFleetCloneAndSort(t *testing.T) {
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	original := model.Fleet{Epoch: at, Sessions: []model.Session{bbRichSession()}, Sources: []model.SourceReport{{ID: model.SourceRegistry, At: bbPtr(at), Detail: "source"}}}
	copy := original.Clone()
	bbMutateSession(&copy.Sessions[0])
	copy.Sources[0].Detail = "changed"
	*copy.Sources[0].At = at.Add(time.Hour)
	if !reflect.DeepEqual(original.Sessions[0], bbRichSession()) || original.Sources[0].Detail != "source" || !original.Sources[0].At.Equal(at) {
		t.Fatal("Fleet.Clone shared source or session storage")
	}
	keys := []model.Key{
		{SessionID: "b", PID: 2},
		{SessionID: "a", Job: "z"},
		{SessionID: "a", Job: "a"},
		{Orphan: "z.json"},
		{Orphan: "a.json"},
	}
	f := model.Fleet{Sessions: []model.Session{
		{PID: 9, Key: model.Key{SessionID: "0-first-by-key", PID: 9}},
		{PID: 2, Key: keys[0]}, {PID: 2, Key: keys[1]},
		{PID: 2, Key: keys[2]}, {Key: keys[3]}, {Key: keys[4]},
	}}
	f.Sort()
	want := []model.Key{keys[4], keys[3], keys[2], keys[1], keys[0], {SessionID: "0-first-by-key", PID: 9}}
	for i, s := range f.Sessions {
		if s.Key != want[i] {
			t.Errorf("sorted session %d = %+v, want %+v", i, s.Key, want[i])
		}
	}
}

func TestBBToolResultPairingUsesCallIdentity(t *testing.T) {
	turns := []model.Turn{
		{Role: model.TurnAssistant, Tools: []model.ToolCall{model.NewToolCall("Read", `{"path":"a"}`, "call-a")}},
		{Role: model.TurnAssistant, Tools: []model.ToolCall{model.NewToolCall("Read", `{"path":"b"}`, "call-b")}},
	}
	if got := model.AttachToolResult(turns, "call-b", "result-b"); got != 1 {
		t.Fatalf("AttachToolResult index = %d, want 1", got)
	}
	if turns[1].Tools[0].Result != "result-b" || turns[0].Tools[0].Result != "" {
		t.Fatalf("result attached to wrong same-named tool: %+v", turns)
	}
	if got := model.AttachToolResult(turns, "absent", "ignored"); got != -1 {
		t.Errorf("unknown call index = %d, want -1", got)
	}
	if got := model.AttachToolResult(nil, "call-a", "ignored"); got != -1 {
		t.Errorf("empty turns index = %d, want -1", got)
	}
	if turns[0].Tools[0].Result != "" || turns[1].Tools[0].Result != "result-b" {
		t.Errorf("unmatched result changed a paired call: %+v", turns)
	}
	if got := turns[0].Tools[0]; got.Name != "Read" || got.Input != `{"path":"a"}` {
		t.Errorf("NewToolCall lost public values: %+v", got)
	}
}

var _ error = model.Problem{}
