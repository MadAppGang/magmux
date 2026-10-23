package join

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

// --- builders ---------------------------------------------------------------

func reg(file string, pid int, id, status string, opts ...func(*claude.Record)) claude.Record {
	r := claude.Record{File: file, PID: pid, SessionID: id, StatusRaw: status, Kind: model.KindInteractive}
	r.Status, r.Waiting = claude.MapRegistryStatus(status, "")
	for _, o := range opts {
		o(&r)
	}
	r.Status, r.Waiting = claude.MapRegistryStatus(r.StatusRaw, r.WaitingRaw)
	return r
}

func bg(job string) func(*claude.Record) {
	return func(r *claude.Record) { r.Kind, r.KindRaw, r.JobID = model.KindBackground, claude.ValueKindBG, job }
}

func waitingFor(raw string) func(*claude.Record) {
	return func(r *claude.Record) { r.WaitingRaw = raw }
}

func named(n string) func(*claude.Record) { return func(r *claude.Record) { r.Name = n } }

func job(dir, id, short, state, tempo string) claude.Job {
	j := claude.Job{Dir: dir, SessionID: id, DaemonShort: short, Short: short, StateRaw: state, State: claude.MapJobState(state)}
	if j.Short == "" {
		j.Short = dir
	}
	if tempo != "" {
		j.Tempo = &tempo
	}
	return j
}

func oracle(dead ...int) func(int) model.Liveness {
	return func(pid int) model.Liveness {
		if slices.Contains(dead, pid) {
			return model.LivenessNo
		}
		return model.LivenessYes
	}
}

func keys(ss []model.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Key.String())
	}
	return out
}

func only(t *testing.T, ss []model.Session) model.Session {
	t.Helper()
	if len(ss) != 1 {
		t.Fatalf("%d sessions, want 1: %v", len(ss), keys(ss))
	}
	return ss[0]
}

func problemOf(ps []model.Problem, kind model.ProblemKind) *model.Problem {
	for i := range ps {
		if ps[i].Kind == kind {
			return &ps[i]
		}
	}
	return nil
}

// --- rule 1: keys ---------------------------------------------------------

func TestKeysAreAFunctionOfOneRecordsOwnFields(t *testing.T) {
	cases := []struct {
		name string
		got  model.Key
		want model.Key
	}{
		{"K1", RegistryKey(reg("1.json", 1, "s", "idle", bg("j"))), model.Key{SessionID: "s", Job: "j"}},
		{"an id-less kind:bg record is K3 and cannot twin", RegistryKey(reg("1.json", 1, "", "idle", bg("j"))), model.Key{PID: 1}},
		{"a spare with no jobId is K3", RegistryKey(reg("1.json", 1, "s", "idle", bg(""))), model.Key{SessionID: "s", PID: 1}},
		{"K3", RegistryKey(reg("1.json", 1, "s", "idle")), model.Key{SessionID: "s", PID: 1}},
		{"K3 with an id and no pid", RegistryKey(reg("x.json", 0, "s", "idle")), model.Key{SessionID: "s"}},
		{"K4", RegistryKey(reg("x.json", 0, "", "idle")), model.Key{Orphan: "x.json"}},
		{"K2 by daemonShort", JobKey(job("dir", "s", "deadbeef", "working", "")), model.Key{SessionID: "s", Job: "deadbeef"}},
		{"K2 by directory", JobKey(job("dir", "s", "", "working", "")), model.Key{SessionID: "s", Job: "dir"}},
		{"an id-less job is its directory's orphan", JobKey(job("dir", "", "short", "working", "")), model.Key{Orphan: "jobs/dir"}},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, c.got, c.want)
		}
	}
	if (model.Key{Orphan: "jobs/x"}).String() == (model.Key{Orphan: "x"}).String() {
		t.Fatal("the two orphan spellings met")
	}
}

// §3.4's table, one cycle-sequence sub-test per row. Each step is one join
// over the records present then; the assertion is the set of keys, which is
// what an actor's identity is.
func TestKeyTransitions(t *testing.T) {
	const id, j = "s", "j1"
	twinReg := reg("100.json", 100, id, "idle", bg(j))
	twinJob := job(j, id, j, "working", "")
	type step struct {
		in   Inputs
		want []string
	}
	K := func(k model.Key) string { return k.String() }
	jobKey := K(model.Key{SessionID: id, Job: j})
	rows := []struct {
		name  string
		steps []step
	}{
		{"1 registry first, then the job record", []step{
			{Inputs{Records: []claude.Record{twinReg}}, []string{jobKey}},
			{Inputs{Records: []claude.Record{twinReg}, Jobs: []claude.Job{twinJob}}, []string{jobKey}},
		}},
		{"2 job first, then its process registers", []step{
			{Inputs{Jobs: []claude.Job{twinJob}}, []string{jobKey}},
			{Inputs{Records: []claude.Record{twinReg}, Jobs: []claude.Job{twinJob}}, []string{jobKey}},
		}},
		{"3 the twin's process exits and its file remains", []step{
			{Inputs{Records: []claude.Record{twinReg}, Jobs: []claude.Job{twinJob}, Alive: oracle()}, []string{jobKey}},
			{Inputs{Records: []claude.Record{twinReg}, Jobs: []claude.Job{twinJob}, Alive: oracle(100)}, []string{jobKey}},
		}},
		{"4 the twin's registry file is removed", []step{
			{Inputs{Records: []claude.Record{twinReg}, Jobs: []claude.Job{twinJob}}, []string{jobKey}},
			{Inputs{Jobs: []claude.Job{twinJob}}, []string{jobKey}},
		}},
		{"5 the job record is removed while its process lives", []step{
			{Inputs{Records: []claude.Record{twinReg}, Jobs: []claude.Job{twinJob}}, []string{jobKey}},
			{Inputs{Records: []claude.Record{twinReg}}, []string{jobKey}},
		}},
		{"6 a second claimant with a lower pid appears", []step{
			{Inputs{Records: []claude.Record{twinReg}, Jobs: []claude.Job{twinJob}}, []string{jobKey}},
			{Inputs{Records: []claude.Record{twinReg, reg("50.json", 50, id, "busy", bg(j))}, Jobs: []claude.Job{twinJob}}, []string{jobKey}},
		}},
		{"7 a claimant disappears", []step{
			{Inputs{Records: []claude.Record{twinReg, reg("50.json", 50, id, "busy", bg(j))}}, []string{jobKey}},
			{Inputs{Records: []claude.Record{twinReg}}, []string{jobKey}},
		}},
		{"8 an interactive resume of a job's id is its own K3", []step{
			{Inputs{Jobs: []claude.Job{twinJob}}, []string{jobKey}},
			{Inputs{Jobs: []claude.Job{twinJob}, Records: []claude.Record{reg("200.json", 200, id, "busy")}},
				[]string{jobKey, K(model.Key{SessionID: id, PID: 200})}},
		}},
		{"9 a finished job resumed as a new job", []step{
			{Inputs{Jobs: []claude.Job{job(j, id, j, "done", "")}}, []string{jobKey}},
			{Inputs{Jobs: []claude.Job{job(j, id, j, "done", ""), job("j2", id, "j2", "working", "")}},
				[]string{jobKey, K(model.Key{SessionID: id, Job: "j2"})}},
		}},
		{"10 an interactive restart or /clear is a new K3", []step{
			{Inputs{Records: []claude.Record{reg("300.json", 300, id, "idle")}}, []string{K(model.Key{SessionID: id, PID: 300})}},
			{Inputs{Records: []claude.Record{reg("301.json", 301, id, "idle")}}, []string{K(model.Key{SessionID: id, PID: 301})}},
			{Inputs{Records: []claude.Record{reg("301.json", 301, "t", "idle")}}, []string{K(model.Key{SessionID: "t", PID: 301})}},
		}},
		{"11 a torn read keeps the last good value (the source's job), so the key holds", []step{
			{Inputs{Records: []claude.Record{twinReg}}, []string{jobKey}},
			{Inputs{Records: []claude.Record{twinReg}}, []string{jobKey}},
		}},
		{"12 an agents-only entry, then its own registry record", []step{
			{Inputs{Agents: listing(entry(id, 400))}, []string{K(model.Key{SessionID: id, PID: 400})}},
			{Inputs{Agents: listing(entry(id, 400)), Records: []claude.Record{reg("400.json", 400, id, "idle")}},
				[]string{K(model.Key{SessionID: id, PID: 400})}},
		}},
		{"13 an agents-only entry, then a record for the id with another pid", []step{
			{Inputs{Agents: listing(entry(id, 400))}, []string{K(model.Key{SessionID: id, PID: 400})}},
			{Inputs{Agents: listing(entry(id, 400)), Records: []claude.Record{reg("401.json", 401, id, "idle")}},
				[]string{K(model.Key{SessionID: id, PID: 401})}},
		}},
		{"14 a spare acquires a jobId", []step{
			{Inputs{Records: []claude.Record{reg("500.json", 500, id, "idle", bg(""))}}, []string{K(model.Key{SessionID: id, PID: 500})}},
			{Inputs{Records: []claude.Record{reg("500.json", 500, id, "idle", bg(j))}}, []string{jobKey}},
		}},
		{"15 two id-less jobs sharing one daemonShort", []step{
			{Inputs{Jobs: []claude.Job{job("a", "", "short", "working", ""), job("b", "", "short", "working", "")}},
				[]string{"orphan:jobs/a", "orphan:jobs/b"}},
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for i, st := range row.steps {
				ss, _ := Build(st.in)
				if got := keys(ss); strings.Join(got, " ") != strings.Join(st.want, " ") {
					t.Fatalf("step %d: keys %v, want %v", i, got, st.want)
				}
			}
		})
	}
}

// Row 6 asserts the most: the key holds, the speaker switches to the live
// lower pid, stays with the old one when the new one is dead, and the claim is
// named with both pids.
func TestASecondClaimantChangesTheSpeakerNeverTheKey(t *testing.T) {
	a := reg("100.json", 100, "s", "idle", bg("j"))
	b := reg("50.json", 50, "s", "busy", bg("j"))
	ss, ps := Build(Inputs{Records: []claude.Record{a, b}, Jobs: []claude.Job{job("j", "s", "j", "working", "")}, Alive: oracle()})
	s := only(t, ss)
	if s.PID != 50 || s.Registry.File != "50.json" || s.Status != model.StatusWorking {
		t.Fatalf("both alive: speaker %d %s status %v", s.PID, s.Registry.File, s.Status)
	}
	p := problemOf(ps, model.ProblemDuplicateJobClaim)
	if p == nil || !slices.Equal(p.PIDs, []int{50, 100}) || p.Key == nil || *p.Key != s.Key {
		t.Fatalf("problem %+v", p)
	}
	if !slices.Equal(s.Claimants, []int{50, 100}) {
		t.Fatalf("claimants %v", s.Claimants)
	}
	ss, _ = Build(Inputs{Records: []claude.Record{a, b}, Jobs: []claude.Job{job("j", "s", "j", "working", "")}, Alive: oracle(50)})
	if s := only(t, ss); s.PID != 100 || s.ProcessAlive != model.LivenessYes {
		t.Fatalf("the lower pid dead: speaker %d", s.PID)
	}
}

// The K3 and K2 duplicate speakers follow tt's order BYTE-WISE: "100.json"
// sorts before "99.json", so 100.json speaks.
func TestDuplicateSpeakersFollowTTsOrder(t *testing.T) {
	ss, ps := Build(Inputs{Records: []claude.Record{
		reg("99.json", 200, "s", "busy", named("ninety-nine")), reg("100.json", 200, "s", "idle", named("one-hundred")),
	}})
	s := only(t, ss)
	if s.Registry.File != "100.json" || s.Name != "one-hundred" || s.Status != model.StatusIdle {
		t.Fatalf("speaker %s %q %v", s.Registry.File, s.Name, s.Status)
	}
	if p := problemOf(ps, model.ProblemDuplicateKey); p == nil || !strings.Contains(p.Detail, "100.json, 99.json") {
		t.Fatalf("%+v", p)
	}
	ss, ps = Build(Inputs{Jobs: []claude.Job{job("b", "s", "k", "working", ""), job("a", "s", "k", "blocked", "")}})
	if s := only(t, ss); s.Job.Dir != "a" || s.Status != model.StatusBlocked {
		t.Fatalf("job speaker %s %v", s.Job.Dir, s.Status)
	}
	if p := problemOf(ps, model.ProblemDuplicateKey); p == nil || !strings.Contains(p.Detail, "a, b") {
		t.Fatalf("%+v", p)
	}
}

// Rule 2's field precedence: the registry speaker's words when it has them,
// even DEAD; the job's otherwise; the pid is the registry speaker's.
func TestTheRegistrySpeakerSuppliesFactsEvenWhenDead(t *testing.T) {
	r := reg("70051.json", 70051, "s", "idle", bg("j"), named("from-registry"))
	r.CWD, r.Version = "/reg", "2.1.280"
	jb := job("j", "s", "j", "blocked", "blocked")
	jb.Name, jb.CWD, jb.CLIVersion = "from-job", "/job", "2.1.233"
	s := only(t, first(Build(Inputs{Records: []claude.Record{r}, Jobs: []claude.Job{jb}, Alive: oracle(70051)})))
	if s.Name != "from-registry" || s.CWD != "/reg" || s.Version != "2.1.280" || s.PID != 70051 || s.ProcessAlive != model.LivenessNo {
		t.Fatalf("%+v", s)
	}
	if s.Status != model.StatusBlocked || s.StatusFrom != model.SourceJobs || !s.WaitingApplies() {
		t.Fatalf("a dead twin over a blocked job is blocked: %v from %v", s.Status, s.StatusFrom)
	}
	if s.Job == nil || !s.Job.Twin || s.Job.Name != "from-job" || s.Kind != model.KindBackground {
		t.Fatalf("job facts %+v kind %v", s.Job, s.Kind)
	}
	if s.LaunchDir != "" {
		t.Fatal("the join never sets LaunchDir")
	}
}

// An admitted agents entry keeps its pid and liveness (review 3 Issue 4), and
// an entry is admitted only when neither its id nor its pid is on a record.
func TestAnAgentsOnlySessionKeepsItsPIDAndLiveness(t *testing.T) {
	s := only(t, first(Build(Inputs{Agents: listing(entry("lonely", 987654)), Alive: oracle()})))
	if s.PID != 987654 || s.ProcessAlive != model.LivenessYes || s.StatusFrom != model.SourceAgents ||
		!slices.Equal(s.ListedBy, []model.SourceID{model.SourceAgents}) || s.Kind != model.KindInteractive || s.Registry != nil {
		t.Fatalf("%+v", s)
	}
	ss, _ := Build(Inputs{Agents: listing(entry("other-id", 7)), Records: []claude.Record{reg("7.json", 7, "current", "idle")}})
	if got := keys(ss); len(got) != 1 || got[0] != "current/pid:7" {
		t.Fatalf("a listing older than a /clear must not resurrect the old id on a live pid: %v", got)
	}
	bgEntry := claude.AgentsEntry{SessionID: "bgonly", ID: "x", KindRaw: claude.ValueKindBackground, Kind: model.KindBackground}
	if ss, _ := Build(Inputs{Agents: &claude.AgentsListing{Entries: []claude.AgentsEntry{bgEntry}}}); len(ss) != 0 {
		t.Fatalf("a background entry never creates a session: %v", keys(ss))
	}
}

// ListedBy is MEMBERSHIP, in the order registry, jobs, agents, and an empty id
// is never listed.
func TestListedByMembership(t *testing.T) {
	l := &claude.AgentsListing{Entries: []claude.AgentsEntry{
		{SessionID: "s", ID: "j", KindRaw: claude.ValueKindBackground},
		{SessionID: "", KindRaw: claude.ValueKindInteractive},
	}}
	ss, _ := Build(Inputs{Records: []claude.Record{reg("1.json", 1, "s", "idle", bg("j")), reg("2.json", 2, "", "idle")},
		Jobs: []claude.Job{job("j", "s", "j", "working", "")}, Agents: l})
	for _, s := range ss {
		switch s.Key.SessionID {
		case "s":
			if !slices.Equal(s.ListedBy, []model.SourceID{model.SourceRegistry, model.SourceJobs, model.SourceAgents}) {
				t.Errorf("twin: %v", s.ListedBy)
			}
		case "":
			if !slices.Equal(s.ListedBy, []model.SourceID{model.SourceRegistry}) {
				t.Errorf("id-less: %v", s.ListedBy)
			}
		}
	}
}

// C19: the effective fields are exactly claude.Effective over the speakers.
func TestTheSixEffectiveFieldsAreEffectiveOverTheSpeakers(t *testing.T) {
	r := reg("1.json", 1, "s", "waiting", bg("j"), waitingFor(claude.ValueWaitingInput))
	jb := job("j", "s", "j", "working", "blocked")
	s := only(t, first(Build(Inputs{Records: []claude.Record{r}, Jobs: []claude.Job{jb}, Alive: oracle()})))
	want := claude.Effective(claude.EffectiveInput{Registry: &r, Job: &jb, ProcAlive: model.LivenessYes})
	if s.Status != want.Status || s.StatusRaw != want.Raw || s.StatusFrom != want.From || s.Waiting != want.Waiting ||
		s.WaitingRaw != want.WaitingRaw || !samePtrTime(s.StatusSince, want.Since) {
		t.Fatalf("session %+v against %+v", s, want)
	}
	if s.Status != model.StatusBlocked || s.Waiting != model.WaitingInput {
		t.Fatalf("%v %v", s.Status, s.Waiting)
	}
}

// D11: records missing their pid or id are KEPT.
func TestIncompleteRecordsArePublishedNotDropped(t *testing.T) {
	ss, _ := Build(Inputs{
		Records: []claude.Record{reg("x.json", 0, "", "idle"), reg("2196.json", 2196, "", "idle")},
		Jobs:    []claude.Job{job("nid", "", "short", "blocked", "")},
	})
	if got := strings.Join(keys(ss), " "); got != "orphan:jobs/nid orphan:x.json /pid:2196" {
		t.Fatalf("%s", got)
	}
	for _, s := range ss {
		if s.Key.Orphan == "jobs/nid" && (s.Kind != model.KindBackground || s.Key.Job != "" || s.Job.Short != "short" || s.PID != 0) {
			t.Fatalf("id-less job: %+v", s)
		}
	}
}

// D11 x FR15: a registry record with a pid and no sessionId, and a listing
// entry naming that pid with an id, are ONE process, so they are one session.
// The session takes the listing's id, keeps the record's facts and records the
// listing's corroboration; the record is still incomplete, so the decoder's
// D11 problem stands. A record with an id is unaffected.
func TestAnIDLessRecordTakesTheIDTheListingGivesItsPID(t *testing.T) {
	raw := []byte(`{"pid":2196,"cwd":"/rec/cwd","name":"from-record","version":"2.1.273","tmux":"t:@1.%2",` +
		`"messagingSocketPath":"/tmp/2196.sock","kind":"interactive","status":"busy","statusUpdatedAt":1789699206204}`)
	r, dps, err := claude.DecodeRecord("2196.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	if p := problemOf(dps, model.ProblemMissingField); p == nil || p.Field != claude.KeySessionID || p.Scope != model.ScopeRecord {
		t.Fatalf("the record is incomplete and the decoder says so: %v", dps)
	}
	e := entry("listed-id", 2196)
	e.Name, e.CWD = "from-listing", "/listing/cwd"
	other := entry("unrelated", 4242)
	ss, ps := Build(Inputs{Records: []claude.Record{r, reg("7.json", 7, "has-id", "idle")},
		Agents: listing(e, other, entry("other-id", 7)), Alive: oracle()})

	got := map[string]model.Session{}
	for _, s := range ss {
		got[s.Key.String()] = s
	}
	if k := strings.Join(keys(ss), " "); k != "has-id/pid:7 listed-id/pid:2196 unrelated/pid:4242" {
		t.Fatalf("one session per process: %s", k)
	}
	s := got["listed-id/pid:2196"]
	if s.PID != 2196 || s.Name != "from-record" || s.CWD != "/rec/cwd" || s.Tmux == nil || s.Tmux.Raw != "t:@1.%2" ||
		s.MessagingSocketPath == nil || *s.MessagingSocketPath != "/tmp/2196.sock" || s.Version != "2.1.273" || s.Status != model.StatusBusy ||
		s.StatusFrom != model.SourceRegistry || s.Registry == nil || s.Registry.File != "2196.json" ||
		!slices.Equal(s.Claimants, []int{2196}) || s.ProcessAlive != model.LivenessYes {
		t.Fatalf("the record's facts are kept: %+v", s)
	}
	if !slices.Equal(s.ListedBy, []model.SourceID{model.SourceRegistry, model.SourceAgents}) {
		t.Fatalf("the listing's corroboration is recorded: %v", s.ListedBy)
	}
	if len(ps) != 0 {
		t.Fatalf("a single claimant raises no join problem: %v", ps)
	}
	if r.SessionID != "" {
		t.Fatal("the record itself is not rewritten")
	}
	// A record WITH an id keeps it, even when the listing names its pid
	// under another (a listing older than a /clear).
	if s := got["has-id/pid:7"]; s.Registry == nil || slices.Contains(s.ListedBy, model.SourceAgents) {
		t.Fatalf("an id-bearing record is unaffected: %+v", s)
	}
	// Input order never matters.
	ss2, _ := Build(Inputs{Records: []claude.Record{reg("7.json", 7, "has-id", "idle"), r},
		Agents: listing(entry("other-id", 7), other, e), Alive: oracle()})
	if a, b := strings.Join(keys(ss), " "), strings.Join(keys(ss2), " "); a != b {
		t.Fatalf("%s against %s", a, b)
	}
}

// Two listing entries naming one id-less record's pid under two ids: the
// listing contradicts itself, so the join chooses neither. The id-less row is
// kept as it was, neither entry is admitted (the pid is on a record), and a
// Problem on that row names both ids.
func TestTwoListingClaimantsOnAnIDLessPIDAreNotJoined(t *testing.T) {
	ss, ps := Build(Inputs{Records: []claude.Record{reg("2196.json", 2196, "", "idle")},
		Agents: listing(entry("id-b", 2196), entry("id-a", 2196)), Alive: oracle()})
	s := only(t, ss)
	if s.Key != (model.Key{PID: 2196}) || s.Registry == nil || !slices.Equal(s.ListedBy, []model.SourceID{model.SourceRegistry}) {
		t.Fatalf("the id-less row is kept: %+v", s)
	}
	if len(ps) != 1 {
		t.Fatalf("one problem, got %v", ps)
	}
	p := ps[0]
	if p.Scope != model.ScopeRecord || p.Kind != model.ProblemDuplicateKey || p.Key == nil || *p.Key != s.Key ||
		!slices.Equal(p.PIDs, []int{2196}) || !strings.Contains(p.Detail, "id-a, id-b") {
		t.Fatalf("the refusal is said: %+v", p)
	}
	// The same id twice is one claim, not two.
	ss, ps = Build(Inputs{Records: []claude.Record{reg("2196.json", 2196, "", "idle")},
		Agents: listing(entry("id-a", 2196), entry("id-a", 2196)), Alive: oracle()})
	if s := only(t, ss); s.Key != (model.Key{SessionID: "id-a", PID: 2196}) || len(ps) != 0 {
		t.Fatalf("one id named twice joins: %v %v", keys(ss), ps)
	}
}

// The join is deterministic: input order never matters, and the output shares
// nothing with the input.
func TestBuildIsDeterministicAndSharesNothing(t *testing.T) {
	rs := []claude.Record{reg("3.json", 3, "c", "idle"), reg("1.json", 1, "a", "busy"), reg("2.json", 2, "b", "idle", bg("j"))}
	js := []claude.Job{job("j", "b", "j", "working", ""), job("k", "z", "k", "done", "")}
	want, _ := Build(Inputs{Records: rs, Jobs: js})
	slices.Reverse(rs)
	slices.Reverse(js)
	got, _ := Build(Inputs{Records: rs, Jobs: js})
	if fmt.Sprint(keys(got)) != fmt.Sprint(keys(want)) {
		t.Fatalf("%v != %v", keys(got), keys(want))
	}
	at := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	want0 := at
	rs[0].StartedAt = &at
	ss, _ := Build(Inputs{Records: rs[:1]})
	*rs[0].StartedAt = at.Add(time.Hour)
	if !ss[0].StartedAt.Equal(want0) {
		t.Fatal("a session shares a pointer with its record")
	}
}

// --- over the scenario roots ------------------------------------------------

func readRoot(t *testing.T, scenario string) Inputs {
	t.Helper()
	dir := filepath.Join("..", "testdata", "roots", scenario)
	var in Inputs
	if entries, err := os.ReadDir(filepath.Join(dir, "sessions")); err == nil {
		for _, e := range entries {
			if !claude.IsRecordName(e.Name()) {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(dir, "sessions", e.Name()))
			if r, _, err := claude.DecodeRecord(e.Name(), b); err == nil {
				in.Records = append(in.Records, r)
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "jobs")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, "jobs", path.Join(e.Name(), claude.JobStateFile)))
			if err != nil {
				continue
			}
			if j, _, err := claude.DecodeJob(e.Name(), b); err == nil {
				in.Jobs = append(in.Jobs, j)
			}
		}
	}
	return in
}

// The §8.1 scenarios, on the library side, with their dead pids from
// PROVENANCE.md.
func TestTheSyntheticScenarios(t *testing.T) {
	type check func(t *testing.T, ss []model.Session, ps []model.Problem)
	cases := map[string]struct {
		dead []int
		fn   check
	}{
		"working-tempo-blocked": {nil, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.Status != model.StatusBlocked || !s.WaitingApplies() || s.Kind != model.KindBackground {
				t.Errorf("%+v", s)
			}
		}},
		"twin-dead-blocked": {[]int{70001}, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.Status != model.StatusBlocked || s.PID != 70001 || s.ProcessAlive != model.LivenessNo {
				t.Errorf("%+v", s)
			}
		}},
		"twin-waiting-done": {[]int{70002}, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.Status != model.StatusDone || s.WaitingApplies() || s.Waiting != model.WaitingUnknown {
				t.Errorf("%+v", s)
			}
		}},
		"bg-two-claimants": {[]int{70011}, func(t *testing.T, ss []model.Session, ps []model.Problem) {
			s := only(t, ss)
			if s.PID != 70012 || !slices.Equal(s.Claimants, []int{70011, 70012}) || problemOf(ps, model.ProblemDuplicateJobClaim) == nil {
				t.Errorf("%+v %v", s, ps)
			}
		}},
		"lone-bg": {nil, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.Key.Job != "857ef435" || s.Job != nil || s.Kind != model.KindBackground || s.Registry == nil {
				t.Errorf("a lone K1: %+v", s)
			}
		}},
		"lone-bg-two-claimants": {[]int{70031}, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.PID != 70032 || s.Job != nil {
				t.Errorf("%+v", s)
			}
		}},
		"bg-spare-no-jobid": {nil, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.Kind != model.KindBackground || s.KindRaw != claude.ValueKindBG || s.Key.Job != "" || s.Key.PID != 70041 {
				t.Errorf("D6-kind: %+v", s)
			}
		}},
		"job-no-id-shared-short": {nil, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if got := strings.Join(keys(ss), " "); got != "orphan:jobs/nid-a orphan:jobs/nid-b" {
				t.Errorf("%s", got)
			}
		}},
		"k3-duplicate-both-mismatched": {nil, func(t *testing.T, ss []model.Session, ps []model.Problem) {
			if s := only(t, ss); s.Key.PID != 200 || s.Registry.File != "100.json" || problemOf(ps, model.ProblemDuplicateKey) == nil {
				t.Errorf("%+v", s)
			}
		}},
		"k3-duplicate-99-100": {nil, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.Registry.File != "100.json" {
				t.Errorf("byte-wise: %s", s.Registry.File)
			}
		}},
		"twin-dead-overlay": {[]int{70051}, func(t *testing.T, ss []model.Session, _ []model.Problem) {
			if s := only(t, ss); s.Name != "overlay-registry-name" || s.CWD != "/Users/user/dev/overlay-reg" || s.Version != "2.1.280" {
				t.Errorf("%+v", s)
			}
		}},
		"k2-duplicate-different-transcripts": {nil, func(t *testing.T, ss []model.Session, ps []model.Problem) {
			if s := only(t, ss); s.Job.Dir != "a" || s.Key.Job != "k2dup001" || problemOf(ps, model.ProblemDuplicateKey) == nil {
				t.Errorf("%+v", s)
			}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := readRoot(t, name)
			in.Alive = oracle(c.dead...)
			ss, ps := Build(in)
			c.fn(t, ss, ps)
		})
	}
}

// tt's corpus as one root. dup-a and dup-b carry one id but DIFFERENT shorts
// (their directory names), so they are two K2 keys: tt publishes one and the
// library two, which the differential cites D12a. One id as both kinds is two
// sessions, and the background process with no job record is a lone K1.
func TestTTsCorpusJoins(t *testing.T) {
	in := readRoot(t, "tt-corpus")
	ss, _ := Build(in)
	byKey := map[string]model.Session{}
	for _, s := range ss {
		byKey[s.Key.String()] = s
	}
	dupA, dupB := byKey["77777777-7777-4777-8777-777777777777/job:dup-a"], byKey["77777777-7777-4777-8777-777777777777/job:dup-b"]
	if dupA.Job == nil || dupB.Job == nil {
		t.Fatalf("dup-a and dup-b: %v", keys(ss))
	}
	cross := byKey["f350c44d-d22a-4c47-b3ef-00021713db7f/job:cross-kind"]
	interactive := byKey["f350c44d-d22a-4c47-b3ef-00021713db7f/pid:78761"]
	if cross.Job == nil || interactive.Registry == nil {
		t.Fatalf("one id as both kinds is two sessions, never merged: %v", keys(ss))
	}
	bgp := byKey["857ef435-aa77-46f9-bb28-ca9c5374be9d/job:857ef435"]
	if bgp.Registry == nil || bgp.Job != nil {
		t.Fatalf("the background process with no job record is a lone K1: %+v", bgp)
	}
	if _, ok := byKey["orphan:jobs/no-session-id"]; !ok {
		t.Fatalf("D11: the id-less job is published: %v", keys(ss))
	}
}

// --- Diff and AgesAt --------------------------------------------------------

func TestDiffIsPerKeyAndReportsFields(t *testing.T) {
	a := reg("1.json", 1, "s", "idle")
	b := reg("2.json", 2, "t", "idle")
	prev, _ := Build(Inputs{Records: []claude.Record{a, b}})
	a2 := reg("1.json", 1, "s", "busy")
	c := reg("3.json", 3, "u", "idle")
	next, _ := Build(Inputs{Records: []claude.Record{a2, c}})
	changes := Diff(model.Fleet{Sessions: prev}, model.Fleet{Sessions: next})
	if len(changes) != 3 {
		t.Fatalf("%+v", changes)
	}
	if changes[0].Type != ChangeChanged || changes[0].Key.SessionID != "s" || !slices.Contains(changes[0].Fields, "Status") ||
		!slices.Contains(changes[0].Fields, "StatusRaw") || slices.Contains(changes[0].Fields, "Name") {
		t.Errorf("%+v", changes[0])
	}
	if changes[1].Type != ChangeRemoved || changes[1].Key.SessionID != "t" || changes[1].Reason == "" {
		t.Errorf("%+v", changes[1])
	}
	if changes[2].Type != ChangeAdded || changes[2].Key.SessionID != "u" {
		t.Errorf("%+v", changes[2])
	}
	if got := Diff(model.Fleet{Sessions: next}, model.Fleet{Sessions: cloneAll(next)}); len(got) != 0 {
		t.Fatalf("a quiet machine produced changes: %+v", got)
	}
	// Two sessions on one id never shadow each other.
	x, y := reg("1.json", 1, "same", "idle"), reg("2.json", 2, "same", "idle")
	p, _ := Build(Inputs{Records: []claude.Record{x, y}})
	n, _ := Build(Inputs{Records: []claude.Record{x}})
	if got := Diff(model.Fleet{Sessions: p}, model.Fleet{Sessions: n}); len(got) != 1 || got[0].Key.PID != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestAgesAreDerivedNotPublished(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	since := now.Add(-90 * time.Second)
	upd := now.Add(-time.Hour)
	waiting := model.Session{Status: model.StatusWaiting, StatusSince: &since, Job: &model.JobFacts{UpdatedAt: &upd}}
	a := AgesAt(waiting, now)
	if a.InStatus == nil || *a.InStatus != 90*time.Second || a.BlockedFor == nil || *a.BlockedFor != 90*time.Second ||
		a.RecordAge == nil || *a.RecordAge != time.Hour {
		t.Fatalf("%+v", a)
	}
	busy := model.Session{Status: model.StatusBusy, StatusSince: &since}
	if a := AgesAt(busy, now); a.BlockedFor != nil || a.RecordAge != nil {
		t.Fatalf("BlockedFor is a view of InStatus only while waiting applies: %+v", a)
	}
	if a := AgesAt(model.Session{Status: model.StatusBlocked}, now); a.InStatus != nil || a.BlockedFor != nil {
		t.Fatalf("no instant, no age: %+v", a)
	}
}

// --- helpers -----------------------------------------------------------------

func listing(es ...claude.AgentsEntry) *claude.AgentsListing {
	return &claude.AgentsListing{Entries: es}
}

func entry(id string, pid int) claude.AgentsEntry {
	return claude.AgentsEntry{SessionID: id, PID: &pid, KindRaw: claude.ValueKindInteractive, Kind: model.KindInteractive,
		StatusRaw: claude.ValueStatusIdle, Status: model.StatusIdle}
}

func first(ss []model.Session, _ []model.Problem) []model.Session { return ss }

func cloneAll(ss []model.Session) []model.Session {
	out := make([]model.Session, len(ss))
	for i, s := range ss {
		out[i] = s.Clone()
	}
	return out
}

func samePtrTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// C10: with no oracle every liveness is Unknown, never No, and a session with
// no pid is Unknown whatever the oracle says.
func TestANilOracleLeavesLivenessUnknown(t *testing.T) {
	ss, _ := Build(Inputs{Records: []claude.Record{reg("1.json", 1, "s", "idle"), reg("x.json", 0, "", "idle")}})
	for _, s := range ss {
		if s.ProcessAlive != model.LivenessUnknown {
			t.Errorf("%s: %v", s.Key, s.ProcessAlive)
		}
	}
	ss, _ = Build(Inputs{Jobs: []claude.Job{job("j", "s", "j", "working", "")}, Alive: oracle(0)})
	if s := only(t, ss); s.PID != 0 || s.ProcessAlive != model.LivenessUnknown {
		t.Fatalf("a job-only session has no pid to ask about: %+v", s)
	}
}
