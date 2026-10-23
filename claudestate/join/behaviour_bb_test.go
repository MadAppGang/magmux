package join_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/join"
	"github.com/MadAppGang/magmux/claudestate/model"
)

func bbPtr[T any](v T) *T { return &v }

func bbRecord(file, id string, pid int) claude.Record {
	return claude.Record{File: file, SessionID: id, PID: pid, Kind: model.KindInteractive, Status: model.StatusBusy, StatusRaw: "busy", CWD: "/synthetic/live"}
}

func bbByKey(t *testing.T, sessions []model.Session) map[model.Key]model.Session {
	t.Helper()
	out := make(map[model.Key]model.Session)
	for _, s := range sessions {
		if _, ok := out[s.Key]; ok {
			t.Fatalf("duplicate published key: %+v", s.Key)
		}
		out[s.Key] = s
	}
	return out
}

func bbHasProblem(problems []model.Problem, kind model.ProblemKind) bool {
	for _, p := range problems {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

func TestBBInteractiveProcessesOnOneIDStaySeparate(t *testing.T) {
	a, b := bbRecord("101.json", "sid", 101), bbRecord("102.json", "sid", 102)
	sessions, _ := join.Build(join.Inputs{Records: []claude.Record{a, b}, Alive: func(int) model.Liveness { return model.LivenessYes }})
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want both processes", len(sessions))
	}
	byKey := bbByKey(t, sessions)
	for _, r := range []claude.Record{a, b} {
		s, ok := byKey[model.Key{SessionID: "sid", PID: r.PID}]
		if !ok || s.PID != r.PID || s.Registry == nil || s.Registry.File != r.File {
			t.Errorf("process %d lost its own facts: %+v", r.PID, s)
		}
		if s.LaunchDir != "" {
			t.Errorf("library inferred LaunchDir from live cwd: %q", s.LaunchDir)
		}
	}
}

func TestBBBackgroundClaimantsMergeWithoutLosingPIDs(t *testing.T) {
	a, b := bbRecord("101.json", "sid", 101), bbRecord("102.json", "sid", 102)
	for _, r := range []*claude.Record{&a, &b} {
		r.Kind, r.KindRaw, r.JobID = model.KindBackground, "bg", "job-a"
	}
	job := claude.Job{Dir: "job-a", Short: "job-a", SessionID: "sid", State: model.StatusWorking, StateRaw: "working"}
	sessions, problems := join.Build(join.Inputs{Records: []claude.Record{b, a}, Jobs: []claude.Job{job}, Alive: func(int) model.Liveness { return model.LivenessYes }})
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want one job key: %+v", len(sessions), sessions)
	}
	s := sessions[0]
	if s.Key != (model.Key{SessionID: "sid", Job: "job-a"}) || s.Kind != model.KindBackground || s.Job == nil || !s.Job.Twin {
		t.Fatalf("not a merged background twin: %+v", s)
	}
	if !reflect.DeepEqual(s.Claimants, []int{101, 102}) {
		t.Errorf("Claimants = %v, want [101 102]", s.Claimants)
	}
	if s.PID != 101 && s.PID != 102 {
		t.Errorf("speaker pid %d is not a claimant", s.PID)
	}
	if !reflect.DeepEqual(s.ListedBy, []model.SourceID{model.SourceRegistry, model.SourceJobs}) {
		t.Errorf("ListedBy = %v, want ordered registry/jobs membership", s.ListedBy)
	}
	if !bbHasProblem(problems, model.ProblemDuplicateJobClaim) {
		t.Fatalf("missing duplicate job claim problem: %+v", problems)
	}
	for _, p := range problems {
		if p.Kind == model.ProblemDuplicateJobClaim && !reflect.DeepEqual(p.PIDs, []int{101, 102}) {
			t.Errorf("duplicate problem lost sorted claimants: %+v", p)
		}
	}
}

func TestBBKeysDependOnlyOnTheirOwnIdentity(t *testing.T) {
	interactive := bbRecord("101.json", "sid", 101)
	background := bbRecord("102.json", "sid", 102)
	background.Kind, background.KindRaw, background.JobID = model.KindBackground, "bg", "short"
	job := claude.Job{Dir: "directory", Short: "short", DaemonShort: "short", SessionID: "sid", State: model.StatusDone, StateRaw: "done"}
	tests := []struct {
		name string
		in   join.Inputs
		want []model.Key
	}{
		{"interactive-alone", join.Inputs{Records: []claude.Record{interactive}}, []model.Key{{SessionID: "sid", PID: 101}}},
		{"interactive-with-job", join.Inputs{Records: []claude.Record{interactive}, Jobs: []claude.Job{job}}, []model.Key{{SessionID: "sid", PID: 101}, {SessionID: "sid", Job: "short"}}},
		{"background-without-job", join.Inputs{Records: []claude.Record{background}}, []model.Key{{SessionID: "sid", Job: "short"}}},
		{"background-with-job", join.Inputs{Records: []claude.Record{background}, Jobs: []claude.Job{job}}, []model.Key{{SessionID: "sid", Job: "short"}}},
		{"cross-kind", join.Inputs{Records: []claude.Record{interactive, background}, Jobs: []claude.Job{job}}, []model.Key{{SessionID: "sid", PID: 101}, {SessionID: "sid", Job: "short"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions, _ := join.Build(tt.in)
			byKey := bbByKey(t, sessions)
			if len(byKey) != len(tt.want) {
				t.Fatalf("keys = %+v, want %+v", byKey, tt.want)
			}
			for _, key := range tt.want {
				if _, ok := byKey[key]; !ok {
					t.Errorf("missing stable key %+v", key)
				}
			}
		})
	}
}

func TestBBIDLessRecordsAndJobsNeverJoinThroughEmptyID(t *testing.T) {
	// SYNTHETIC: records are decoded so filename fallback and D11 admission are exercised together.
	var records []claude.Record
	for _, name := range []string{"one.json", "two.json"} {
		r, _, err := claude.DecodeRecord(name, []byte(`{"status":"waiting"}`))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	job, _, err := claude.DecodeJob("one", []byte(`{"state":"blocked"}`))
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := join.Build(join.Inputs{Records: records, Jobs: []claude.Job{job}})
	byKey := bbByKey(t, sessions)
	want := []model.Key{{Orphan: "one.json"}, {Orphan: "two.json"}, {Orphan: "jobs/one"}}
	if len(byKey) != len(want) {
		t.Fatalf("id-less values collapsed: %+v", sessions)
	}
	for _, key := range want {
		s, ok := byKey[key]
		if !ok || s.Key.Job != "" || s.PID != 0 {
			t.Errorf("missing or malformed orphan %+v: %+v", key, s)
		}
	}
	if s := byKey[model.Key{Orphan: "jobs/one"}]; s.Job == nil || s.Job.Short != "one" || s.Kind != model.KindBackground {
		t.Errorf("id-less job lost its non-identity short or kind: %+v", s)
	}
}

func TestBBDuplicateKeysMergeDeterministically(t *testing.T) {
	a, b := bbRecord("a.json", "sid", 101), bbRecord("b.json", "sid", 101)
	a.Name, b.Name = "first", "second"
	first, problems := join.Build(join.Inputs{Records: []claude.Record{a, b}})
	second, reversedProblems := join.Build(join.Inputs{Records: []claude.Record{b, a}})
	if len(first) != 1 || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(problems, reversedProblems) {
		t.Fatalf("input order changes duplicate merge: %+v / %+v; problems %+v / %+v", first, second, problems, reversedProblems)
	}
	if !bbHasProblem(problems, model.ProblemDuplicateKey) {
		t.Errorf("missing duplicate-key problem: %+v", problems)
	}
	if first[0].Registry == nil || (first[0].Registry.File != a.File && first[0].Registry.File != b.File) {
		t.Errorf("speaker was not one of the records: %+v", first[0])
	}
}

func TestBBAgentsAdmitInteractiveButNotBackgroundOnlyEntries(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// "input needed" is Claude's word for the input wait (contract §4.10 A).
	listing, _, err := claude.DecodeAgents([]byte(`[{"sessionId":"sid","pid":101,"status":"waiting","waitingFor":"input needed","name":"from agents","cwd":"/synthetic/agents"},{"sessionId":"sid","id":"short","kind":"bg","state":"working"}]`), at)
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := join.Build(join.Inputs{Agents: &listing})
	if len(sessions) != 1 {
		t.Fatalf("agents-only background entry was admitted or interactive lost: %+v", sessions)
	}
	s := sessions[0]
	if s.Key != (model.Key{SessionID: "sid", PID: 101}) || s.PID != 101 || s.Registry != nil || s.Job != nil {
		t.Errorf("wrong admitted identity: %+v", s)
	}
	if s.Status != model.StatusWaiting || s.Waiting != model.WaitingInput || s.WaitingRaw != "input needed" || s.StatusFrom != model.SourceAgents || s.CWD != "/synthetic/agents" || s.Name != "from agents" {
		t.Errorf("admitted entry lost its own effective facts: %+v", s)
	}
	if !reflect.DeepEqual(s.ListedBy, []model.SourceID{model.SourceAgents}) {
		t.Errorf("agents membership = %v", s.ListedBy)
	}
	r := bbRecord("101.json", "sid", 101)
	r.Name = "registry name"
	joined, _ := join.Build(join.Inputs{Records: []claude.Record{r}, Agents: &listing})
	if len(joined) != 1 || joined[0].Name != r.Name || joined[0].CWD != r.CWD || joined[0].Status != r.Status || joined[0].StatusFrom != model.SourceRegistry {
		t.Fatalf("corroboration overwrote the registry speaker: %+v", joined)
	}
	if !reflect.DeepEqual(joined[0].ListedBy, []model.SourceID{model.SourceRegistry, model.SourceAgents}) {
		t.Errorf("corroboration membership = %v", joined[0].ListedBy)
	}
}

func TestBBJoinUsesEffectiveForEveryStatusCombination(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, regStatus := range []model.Status{model.StatusUnknown, model.StatusIdle, model.StatusBusy, model.StatusShell, model.StatusWaiting} {
		for _, jobState := range []model.Status{model.StatusUnknown, model.StatusWorking, model.StatusBlocked, model.StatusDone, model.StatusFailed} {
			for _, live := range []model.Liveness{model.LivenessUnknown, model.LivenessNo, model.LivenessYes} {
				t.Run(regStatus.String()+"/"+jobState.String()+"/"+live.String(), func(t *testing.T) {
					r := bbRecord("101.json", "sid", 101)
					r.Kind, r.KindRaw, r.JobID = model.KindBackground, "bg", "short"
					r.Status, r.StatusRaw = regStatus, regStatus.String()
					r.StatusUpdatedAt, r.Waiting, r.WaitingRaw = bbPtr(at), model.WaitingPermission, "permission"
					job := claude.Job{Dir: "short", Short: "short", SessionID: "sid", State: jobState, StateRaw: jobState.String(), UpdatedAt: bbPtr(at.Add(time.Minute))}
					want := claude.Effective(claude.EffectiveInput{Registry: &r, Job: &job, ProcAlive: live})
					sessions, _ := join.Build(join.Inputs{Records: []claude.Record{r}, Jobs: []claude.Job{job}, Alive: func(int) model.Liveness { return live }})
					if len(sessions) != 1 {
						t.Fatalf("got %d sessions", len(sessions))
					}
					s := sessions[0]
					got := claude.EffectiveStatus{Status: s.Status, Raw: s.StatusRaw, Since: s.StatusSince, From: s.StatusFrom, Waiting: s.Waiting, WaitingRaw: s.WaitingRaw}
					wantConflict := want.Conflict
					want.Conflict = false // Conflict is a separate JobFacts fact, not one of the six effective fields.
					if !reflect.DeepEqual(got, want) {
						t.Errorf("join effective fields = %+v, claude.Effective = %+v", got, want)
					}
					if s.Registry == nil || s.Registry.Status != regStatus || s.Job == nil || s.Job.State != jobState || s.Job.Conflict != wantConflict {
						t.Errorf("effective result replaced source words: %+v", s)
					}
				})
			}
		}
	}
}

func TestBBJoinIsIndependentOfRecordJobAndListingOrder(t *testing.T) {
	a, b := bbRecord("101.json", "sid", 101), bbRecord("102.json", "sid", 102)
	for _, r := range []*claude.Record{&a, &b} {
		r.Kind, r.KindRaw, r.JobID = model.KindBackground, "bg", "short"
	}
	x := bbRecord("103.json", "interactive", 103)
	j := claude.Job{Dir: "short", Short: "short", SessionID: "sid", State: model.StatusWorking, StateRaw: "working"}
	k := claude.Job{Dir: "other", Short: "other", SessionID: "other", State: model.StatusDone, StateRaw: "done"}
	e := claude.AgentsEntry{SessionID: "interactive", PID: bbPtr(103), Kind: model.KindInteractive, Status: model.StatusBusy}
	f := claude.AgentsEntry{SessionID: "agents-only", PID: bbPtr(104), Kind: model.KindInteractive, Status: model.StatusIdle}
	alive := func(int) model.Liveness { return model.LivenessYes }
	first, firstProblems := join.Build(join.Inputs{Records: []claude.Record{a, b, x}, Jobs: []claude.Job{j, k}, Agents: &claude.AgentsListing{Entries: []claude.AgentsEntry{e, f}}, Alive: alive})
	second, secondProblems := join.Build(join.Inputs{Records: []claude.Record{x, b, a}, Jobs: []claude.Job{k, j}, Agents: &claude.AgentsListing{Entries: []claude.AgentsEntry{f, e}}, Alive: alive})
	left, right := model.Fleet{Sessions: first}, model.Fleet{Sessions: second}
	left.Sort()
	right.Sort()
	if !reflect.DeepEqual(left, right) || !reflect.DeepEqual(firstProblems, secondProblems) {
		t.Fatalf("input order changed join: %+v / %+v; problems %+v / %+v", left, right, firstProblems, secondProblems)
	}
}

func TestBBJobOnlyBlockedTempoHasOneEffectiveStatus(t *testing.T) {
	job, _, err := claude.DecodeJob("short", []byte(`{"sessionId":"sid","state":"working","tempo":"blocked"}`))
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := join.Build(join.Inputs{Jobs: []claude.Job{job}})
	if len(sessions) != 1 || sessions[0].Status != model.StatusBlocked || !sessions[0].WaitingApplies() || sessions[0].StatusFrom != model.SourceJobs || sessions[0].Job == nil || sessions[0].Job.State != model.StatusWorking {
		t.Fatalf("working + blocked tempo did not preserve distinct effective/source facts: %+v", sessions)
	}
}

func TestBBDiffIsKeyedAndReportsModelFieldNames(t *testing.T) {
	a := model.Session{Key: model.Key{SessionID: "sid", PID: 101}, PID: 101, Name: "before"}
	b := model.Session{Key: model.Key{SessionID: "sid", PID: 102}, PID: 102}
	prev := model.Fleet{Sessions: []model.Session{a, b}}
	if changes := join.Diff(prev, model.Fleet{Sessions: []model.Session{b, a}}); len(changes) != 0 {
		t.Fatalf("reordering changed the document: %+v", changes)
	}
	a.Name = "after"
	c := model.Session{Key: model.Key{SessionID: "sid", Job: "short"}}
	changes := join.Diff(prev, model.Fleet{Sessions: []model.Session{c, a}})
	if len(changes) != 3 {
		t.Fatalf("changes = %+v, want added/changed/removed", changes)
	}
	seen := make(map[model.Key]join.Change)
	for _, change := range changes {
		seen[change.Key] = change
	}
	if seen[b.Key].Type != join.ChangeRemoved || seen[c.Key].Type != join.ChangeAdded || seen[a.Key].Type != join.ChangeChanged {
		t.Errorf("wrong keyed changes: %+v", seen)
	}
	if !reflect.DeepEqual(seen[a.Key].Fields, []string{"Name"}) {
		t.Errorf("changed Fields = %v, want model name Name", seen[a.Key].Fields)
	}
}

func TestBBAgesAreComputedFromEffectiveInstantsNotStoredOrMtime(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	// Contract §4.10 E: RecordAge is derived from Job.UpdatedAt only, and is nil for
	// a session with no job. The registry's own instants are decoys: an hour-old
	// statusUpdatedAt must not feed InStatus, and a 7m updatedAt must not feed RecordAge.
	s := model.Session{
		Status: model.StatusBlocked, StatusSince: bbPtr(now.Add(-3 * time.Minute)),
		Registry: &model.RegistryFacts{UpdatedAt: bbPtr(now.Add(-7 * time.Minute)), StatusSince: bbPtr(now.Add(-time.Hour))},
		Job:      &model.JobFacts{UpdatedAt: bbPtr(now.Add(-5 * time.Minute))},
	}
	ages := join.AgesAt(s, now)
	if ages.InStatus == nil || *ages.InStatus != 3*time.Minute || ages.BlockedFor == nil || *ages.BlockedFor != 3*time.Minute || ages.RecordAge == nil || *ages.RecordAge != 5*time.Minute {
		t.Errorf("effective ages = %+v", ages)
	}
	noJob := s
	noJob.Job = nil
	if got := join.AgesAt(noJob, now); got.RecordAge != nil {
		t.Errorf("registry updatedAt alone produced a RecordAge: %+v", got)
	}
	s.Status = model.StatusBusy
	if got := join.AgesAt(s, now); got.BlockedFor != nil {
		t.Errorf("nonwaiting status has blocked age: %+v", got)
	}
	if got := join.AgesAt(model.Session{}, now); got.InStatus != nil || got.RecordAge != nil || got.BlockedFor != nil {
		t.Errorf("missing instants invented ages: %+v", got)
	}
}
