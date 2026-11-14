package claude

import (
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// Re-expressed from tt:join_twins_test.go and tt:status_test.go, case for case.

var twinNow = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time { t := twinNow.Add(-d); return &t }

func twinJob(state, tempo string) *Job {
	j := &Job{Dir: "857ef435", Short: "857ef435", DaemonShort: "857ef435", SessionID: "857ef435-aa77-46f9-bb28-ca9c5374be9d",
		StateRaw: state, State: MapJobState(state), UpdatedAt: ago(33 * time.Hour)}
	if tempo != "" {
		j.Tempo = &tempo
	}
	return j
}

func twinReg(status, waiting string) *Record {
	r := &Record{File: "65863.json", PID: 65863, SessionID: "857ef435-aa77-46f9-bb28-ca9c5374be9d",
		StatusRaw: status, WaitingRaw: waiting, StatusUpdatedAt: ago(30 * time.Hour), Kind: model.KindBackground, JobID: "857ef435"}
	r.Status, r.Waiting = MapRegistryStatus(status, waiting)
	return r
}

func TestMapJobRecordReadsTempoAsWellAsState(t *testing.T) {
	cases := []struct {
		state, tempo string
		want         model.Status
	}{
		{ValueStateWorking, ValueTempoBlocked, model.StatusBlocked},
		{ValueStateWorking, ValueTempoActive, model.StatusWorking},
		{ValueStateWorking, ValueTempoIdle, model.StatusWorking},
		{ValueStateWorking, "", model.StatusWorking},
		{ValueStateBlocked, ValueTempoIdle, model.StatusBlocked},
		{ValueStateDone, ValueTempoBlocked, model.StatusDone},
		{ValueStateFailed, ValueTempoBlocked, model.StatusFailed},
		{"reticulating", ValueTempoBlocked, model.StatusUnknown},
	}
	for _, c := range cases {
		if got := MapJobRecord(c.state, c.tempo); got != c.want {
			t.Errorf("MapJobRecord(%q, %q) = %v, want %v", c.state, c.tempo, got, c.want)
		}
	}
}

func TestTwinStatusFollowsTheVendorsRuleForTheMergedPair(t *testing.T) {
	cases := []struct {
		name       string
		job        *Job
		reg        *Record
		proc       model.Liveness
		want       model.Status
		wantFrom   model.SourceID
		wantReason model.WaitingReason
	}{
		{"a busy process is working whatever the job last said",
			twinJob("blocked", "blocked"), twinReg("busy", ""), model.LivenessYes, model.StatusWorking, model.SourceRegistry, model.WaitingUnknown},
		{"stopped mid-turn on a question",
			twinJob("working", "blocked"), twinReg("waiting", "input needed"), model.LivenessYes, model.StatusBlocked, model.SourceJobs, model.WaitingInput},
		{"a turn that ended on an ask",
			twinJob("blocked", "blocked"), twinReg("idle", ""), model.LivenessYes, model.StatusBlocked, model.SourceJobs, model.WaitingUnknown},
		{"the process waits while the job still says working",
			twinJob("working", "active"), twinReg("waiting", "permission prompt"), model.LivenessYes, model.StatusBlocked, model.SourceRegistry, model.WaitingPermission},
		{"neither asks for the user",
			twinJob("working", "idle"), twinReg("idle", ""), model.LivenessYes, model.StatusWorking, model.SourceJobs, model.WaitingUnknown},
		{"done while the process is alive",
			twinJob("done", "idle"), twinReg("idle", ""), model.LivenessYes, model.StatusIdle, model.SourceRegistry, model.WaitingUnknown},
		{"done while the process waits",
			twinJob("done", "idle"), twinReg("waiting", "input needed"), model.LivenessYes, model.StatusBlocked, model.SourceRegistry, model.WaitingInput},
		{"done with no word on the process",
			twinJob("done", "idle"), twinReg("idle", ""), model.LivenessUnknown, model.StatusDone, model.SourceJobs, model.WaitingUnknown},
		{"a settled state the tempo still calls active is not settled",
			twinJob("failed", "active"), twinReg("idle", ""), model.LivenessYes, model.StatusWorking, model.SourceJobs, model.WaitingUnknown},
		{"a dead process is not believed",
			twinJob("working", "blocked"), twinReg("busy", ""), model.LivenessNo, model.StatusBlocked, model.SourceJobs, model.WaitingUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Effective(EffectiveInput{Registry: c.reg, Job: c.job, ProcAlive: c.proc})
			if v.Status != c.want || v.From != c.wantFrom || v.Waiting != c.wantReason {
				t.Errorf("Effective = (%v, %v, %v), want (%v, %v, %v)", v.Status, v.From, v.Waiting, c.want, c.wantFrom, c.wantReason)
			}
			// The six fields describe ONE decision: the raw word and the
			// instant are the deciding record's.
			switch v.From {
			case model.SourceJobs:
				if v.Raw != c.job.StateRaw || v.Since != c.job.UpdatedAt {
					t.Errorf("job decided, raw %q since %v", v.Raw, v.Since)
				}
			case model.SourceRegistry:
				if v.Raw != c.reg.StatusRaw || v.Since != c.reg.StatusUpdatedAt {
					t.Errorf("registry decided, raw %q since %v", v.Raw, v.Since)
				}
			}
			if !v.Status.IsWaiting() && (v.Waiting != model.WaitingUnknown || v.WaitingRaw != "") {
				t.Errorf("a reason on a non-waiting status: %v %q", v.Waiting, v.WaitingRaw)
			}
		})
	}
}

// The two records alone would disagree: the merged answer stands, flagged.
func TestTwinsDisagreeIsTheConflictFlag(t *testing.T) {
	cases := []struct {
		job  *Job
		reg  *Record
		want bool
	}{
		{twinJob("working", "blocked"), twinReg("waiting", "input needed"), false},
		{twinJob("working", "active"), twinReg("idle", ""), true},
		{twinJob("blocked", "blocked"), twinReg("idle", ""), true},
		{twinJob("working", "active"), twinReg("busy", ""), false},
	}
	for i, c := range cases {
		if got := Effective(EffectiveInput{Registry: c.reg, Job: c.job, ProcAlive: model.LivenessYes}).Conflict; got != c.want {
			t.Errorf("case %d: conflict %v", i, got)
		}
	}
}

// E2: a job alone. C19's named case: working + tempo blocked is BLOCKED and
// therefore needs a human (review 03-C3).
func TestAJobAloneUsesStateAndTempo(t *testing.T) {
	v := Effective(EffectiveInput{Job: twinJob("working", "blocked")})
	if v.Status != model.StatusBlocked || !v.Status.IsWaiting() || v.From != model.SourceJobs || v.Raw != ValueStateWorking {
		t.Fatalf("%+v", v)
	}
	if v.Waiting != model.WaitingUnknown || v.WaitingRaw != "" || v.Conflict {
		t.Fatalf("a job alone has no waiting reason and no conflict: %+v", v)
	}
}

// E3: a registry record alone, which is every S-origin row: the effective
// fields ARE the registry fields.
func TestARegistryRecordAloneIsItsOwnWord(t *testing.T) {
	for _, f := range []string{"98198.json", "1724.json", "60003.json", "60006.json"} {
		r, _, _ := mmRecord(t, f)
		v := Effective(EffectiveInput{Registry: &r})
		if v.Status != r.Status || v.Raw != r.StatusRaw || v.Since != r.StatusUpdatedAt || v.From != model.SourceRegistry {
			t.Errorf("%s: %+v against %+v", f, v, r)
		}
		if r.Status == model.StatusWaiting && (v.Waiting != r.Waiting || v.WaitingRaw != r.WaitingRaw) {
			t.Errorf("%s: waiting %v %q", f, v.Waiting, v.WaitingRaw)
		}
	}
	// The registry's raw waitingFor on an idle record is not a waiting reason.
	r := twinReg("idle", "input needed")
	if v := Effective(EffectiveInput{Registry: r}); v.WaitingRaw != "" {
		t.Errorf("idle with a waitingFor: %+v", v)
	}
}

// E4 and E0.
func TestAnAgentsEntryAloneAndNoSpeakerAtAll(t *testing.T) {
	pid := 7
	e := &AgentsEntry{SessionID: "s", PID: &pid, StatusRaw: ValueStatusWaiting, WaitingRaw: ValueWaitingPermission, KindRaw: ValueKindInteractive}
	v := Effective(EffectiveInput{Agents: e})
	if v.Status != model.StatusWaiting || v.Waiting != model.WaitingPermission || v.Since != nil || v.From != model.SourceAgents {
		t.Fatalf("%+v", v)
	}
	if v := Effective(EffectiveInput{Agents: e, Registry: twinReg("idle", "")}); v.From != model.SourceRegistry {
		t.Fatalf("the agents entry is consulted only with no record: %+v", v)
	}
	if v := Effective(EffectiveInput{}); v != (EffectiveStatus{}) {
		t.Fatalf("E0: %+v", v)
	}
}

func TestMapAgentsStatusPicksTheVocabularyByField(t *testing.T) {
	if s, _ := MapAgentsStatus("", ValueStateBlocked, ""); s != model.StatusBlocked {
		t.Error("a state speaks the job vocabulary")
	}
	if s, w := MapAgentsStatus(ValueStatusWaiting, "", ValueWaitingInput); s != model.StatusWaiting || w != model.WaitingInput {
		t.Error("a status speaks the registry vocabulary")
	}
	if s, _ := MapAgentsStatus("", "", ""); s != model.StatusUnknown {
		t.Error("neither is unknown")
	}
}
