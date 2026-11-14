package claude_test

import (
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

// These cases cover specified single-speaker/fallback behavior, not a full twin
// oracle: the referenced section 3.4 decision and speaker tables were not supplied.
func TestBBEffectiveRegistryOnlyKeepsItsOwnWordAndInstant(t *testing.T) {
	since := time.UnixMilli(1700000000000)
	for _, tt := range []struct {
		status model.Status
		raw    string
	}{
		{model.StatusIdle, "idle"}, {model.StatusBusy, "busy"}, {model.StatusShell, "shell"},
		{model.StatusWaiting, "waiting"}, {model.StatusUnknown, "future-status"},
	} {
		r := claude.Record{Status: tt.status, StatusRaw: tt.raw, StatusUpdatedAt: &since}
		if tt.status == model.StatusWaiting {
			r.Waiting, r.WaitingRaw = model.WaitingDialog, "dialog open"
		}
		got := claude.Effective(claude.EffectiveInput{Registry: &r, ProcAlive: model.LivenessYes})
		if got.Status != tt.status || got.Raw != tt.raw || got.From != model.SourceRegistry || got.Conflict {
			t.Errorf("registry-only %q = %+v", tt.raw, got)
		}
		bbInstant(t, got.Since, since)
		if got.Waiting != r.Waiting || got.WaitingRaw != r.WaitingRaw {
			t.Errorf("waiting fields for %q = %+v", tt.raw, got)
		}
	}
}

func TestBBEffectiveJobOnlyMapsTempoAndTerminalStates(t *testing.T) {
	since := time.UnixMilli(1700000000000)
	blocked := "blocked"
	for _, tt := range []struct {
		name  string
		job   claude.Job
		state model.Status
	}{
		{"working", claude.Job{State: model.StatusWorking, StateRaw: "working"}, model.StatusWorking},
		{"tempo blocked", claude.Job{State: model.StatusWorking, StateRaw: "working", Tempo: &blocked}, model.StatusBlocked},
		{"blocked", claude.Job{State: model.StatusBlocked, StateRaw: "blocked"}, model.StatusBlocked},
		{"done", claude.Job{State: model.StatusDone, StateRaw: "done"}, model.StatusDone},
		{"failed", claude.Job{State: model.StatusFailed, StateRaw: "failed"}, model.StatusFailed},
		{"unknown", claude.Job{State: model.StatusUnknown, StateRaw: "future-state"}, model.StatusUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.job.UpdatedAt = &since
			got := claude.Effective(claude.EffectiveInput{Job: &tt.job})
			if got.Status != tt.state || got.From != model.SourceJobs || got.Conflict {
				t.Errorf("job-only effective = %+v; want status %v from jobs", got, tt.state)
			}
			bbInstant(t, got.Since, since)
			if tt.job.Tempo == nil && got.Raw != tt.job.StateRaw {
				t.Errorf("job word = %q; want %q", got.Raw, tt.job.StateRaw)
			}
		})
	}
}

func TestBBEffectiveAgentsOnlyAndRegistryPrecedence(t *testing.T) {
	// Contract §4.10 A: "permission prompt" is Claude's word for the permission wait.
	a := claude.AgentsEntry{Status: model.StatusWaiting, StatusRaw: "waiting", Waiting: model.WaitingPermission, WaitingRaw: "permission prompt"}
	got := claude.Effective(claude.EffectiveInput{Agents: &a})
	if got.Status != model.StatusWaiting || got.Raw != "waiting" || got.From != model.SourceAgents || got.Waiting != model.WaitingPermission || got.WaitingRaw != "permission prompt" {
		t.Errorf("agents-only = %+v", got)
	}
	// The other side of §4.10 A: a bare "permission" is not Claude's word, so rule E
	// (which maps the raw words, §4.5) gives WaitingUnknown and keeps the raw word.
	bare := claude.AgentsEntry{Status: model.StatusWaiting, StatusRaw: "waiting", WaitingRaw: "permission"}
	got = claude.Effective(claude.EffectiveInput{Agents: &bare})
	if got.Status != model.StatusWaiting || got.From != model.SourceAgents || got.Waiting != model.WaitingUnknown || got.WaitingRaw != "permission" {
		t.Errorf("agents-only bare permission = %+v; want WaitingUnknown with raw kept", got)
	}
	r := claude.Record{Status: model.StatusBusy, StatusRaw: "busy"}
	got = claude.Effective(claude.EffectiveInput{Registry: &r, Agents: &a})
	if got.Status != model.StatusBusy || got.Raw != "busy" || got.From != model.SourceRegistry {
		t.Errorf("agents overwrote registry speaker: %+v", got)
	}
	j := claude.Job{State: model.StatusDone, StateRaw: "done"}
	got = claude.Effective(claude.EffectiveInput{Job: &j, Agents: &a})
	if got.Status != model.StatusDone || got.From != model.SourceJobs {
		t.Errorf("agents overwrote job speaker: %+v", got)
	}
	got = claude.Effective(claude.EffectiveInput{})
	if got.Status != model.StatusUnknown || got.Raw != "" || got.Since != nil || got.From != model.SourceUnknown || got.Waiting != model.WaitingUnknown || got.WaitingRaw != "" || got.Conflict {
		t.Errorf("no speakers invented facts: %+v", got)
	}
}
