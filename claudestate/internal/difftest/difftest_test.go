package difftest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/verdict"
)

// Every predicate is tested from both sides: once where it must cite, and
// once with exactly one of its conjuncts false, where it must not.

var epoch = time.Date(2026, 9, 18, 2, 40, 11, 204e6, time.UTC)

func ptr[T any](v T) *T { return &v }

func libRow(s model.Session, problems ...model.Problem) Row {
	return FromModel(model.Fleet{Sessions: []model.Session{s}}, problems)[0]
}

// ttRow builds a tt row the way FromTT would, from a lib row's fields.
func ttRow(key RowKey, from Row, override map[string]Value) Row {
	f := map[string]Value{}
	for k, v := range from.Fields {
		f[k] = v
	}
	for k, v := range override {
		f[k] = v
	}
	return Row{Key: key, Fields: f}
}

func one(t *testing.T, d []Diff) Diff {
	t.Helper()
	if len(d) != 1 {
		t.Fatalf("want one difference, got %d: %v", len(d), formatAll(d))
	}
	return d[0]
}

func formatAll(d []Diff) []string {
	var out []string
	for _, x := range d {
		out = append(out, x.Format())
	}
	return out
}

func interactive(id string, pid int) model.Session {
	return model.Session{
		Key: model.Key{SessionID: id, PID: pid}, PID: pid, Kind: model.KindInteractive, KindRaw: claude.ValueKindInteractive,
		Status: model.StatusIdle, StatusRaw: claude.ValueStatusIdle, Registry: &model.RegistryFacts{File: fmt.Sprint(pid, ".json")},
		ListedBy: []model.SourceID{model.SourceRegistry}, ProcessAlive: model.LivenessYes,
	}
}

func lk(s model.Session) RowKey {
	return RowKey{SessionID: s.Key.SessionID, PID: s.Key.PID, Job: s.Key.Job, Orphan: s.Key.Orphan}
}

func TestEqualRowsHaveNoDifference(t *testing.T) {
	s := interactive("s", 10)
	l := libRow(s)
	if d := Compare([]Row{ttRow(lk(s), l, nil)}, []Row{l}, nil); len(d) != 0 {
		t.Fatalf("%v", formatAll(d))
	}
}

func TestAnUnexplainedFieldIsUncited(t *testing.T) {
	s := interactive("s", 10)
	l := libRow(s)
	d := one(t, Compare([]Row{ttRow(lk(s), l, map[string]Value{FieldName: {S: "other"}})}, []Row{l}, nil))
	if d.Cited != "" || d.Field != FieldName {
		t.Fatalf("%s", d.Format())
	}
}

func TestD1(t *testing.T) {
	s := interactive("s", 10)
	s.Transcript = &model.TranscriptFacts{Location: model.Location{Path: "p/s.jsonl", Matches: 1}, Subagents: ptr(0), SubagentEvidence: model.EvidenceAbsent}
	l := libRow(s)
	tt := ttRow(lk(s), l, map[string]Value{FieldSubagents: null})
	if d := one(t, Compare([]Row{tt}, []Row{l}, nil)); d.Cited != D1 {
		t.Fatalf("%s", d.Format())
	}
	s.Transcript.SubagentEvidence = model.EvidenceCounted // counted, not absent: no D1
	l = libRow(s)
	if d := one(t, Compare([]Row{ttRow(lk(s), l, map[string]Value{FieldSubagents: null})}, []Row{l}, nil)); d.Cited != "" {
		t.Fatalf("%s", d.Format())
	}
}

func TestD4(t *testing.T) {
	s := interactive("s", 10)
	l := libRow(s)
	tt := ttRow(lk(s), l, map[string]Value{FieldStatusSince: {S: "1"}})
	bad := model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemImplausibleInstant, Path: "10.json", Field: claude.KeyStatusUpdatedAt}
	if d := one(t, Compare([]Row{tt}, []Row{l}, []model.Problem{bad})); d.Cited != D4 {
		t.Fatalf("tied by the speaker's Path: %s", d.Format())
	}
	other := bad
	other.Field = claude.KeyStartedAt // a problem on another field explains nothing here
	if d := one(t, Compare([]Row{tt}, []Row{l}, []model.Problem{other})); d.Cited != "" {
		t.Fatalf("%s", d.Format())
	}
	elsewhere := bad
	elsewhere.Path = "11.json" // another record's problem
	if d := one(t, Compare([]Row{tt}, []Row{l}, []model.Problem{elsewhere})); d.Cited != "" {
		t.Fatalf("%s", d.Format())
	}
}

func bgSession(id, job string, pids ...int) model.Session {
	s := interactive(id, pids[len(pids)-1])
	s.Key = model.Key{SessionID: id, Job: job}
	s.Kind, s.KindRaw = model.KindBackground, claude.ValueKindBG
	s.Claimants = pids
	return s
}

func TestThePairingRuleAndD6Lone(t *testing.T) {
	s := bgSession("s", "j", 10)
	l := libRow(s)
	tt := ttRow(RowKey{SessionID: "s", PID: 10}, l, map[string]Value{FieldKind: {S: "interactive"}, FieldShortID: null})
	d := Compare([]Row{tt}, []Row{l}, nil)
	if len(d) != 3 {
		t.Fatalf("key, kind and shortId: %v", formatAll(d))
	}
	for _, x := range d {
		if x.Cited != D6Lone || x.OneSided {
			t.Errorf("%s", x.Format())
		}
	}

	// A second candidate on the tt side pairs NOTHING: both rows stay
	// one-sided and uncited.
	s2 := bgSession("s", "j", 10, 11)
	l2 := libRow(s2)
	tt10 := ttRow(RowKey{SessionID: "s", PID: 10}, l2, nil)
	tt11 := ttRow(RowKey{SessionID: "s", PID: 11}, l2, nil)
	d = Compare([]Row{tt10, tt11}, []Row{l2}, nil)
	if len(d) != 3 {
		t.Fatalf("%v", formatAll(d))
	}
	for _, x := range d {
		if !x.OneSided || x.Cited != "" {
			t.Errorf("an ambiguous pairing paired: %s", x.Format())
		}
	}

	// A tt pid that is not a claimant is not a candidate.
	d = Compare([]Row{ttRow(RowKey{SessionID: "s", PID: 99}, l, nil)}, []Row{l}, nil)
	for _, x := range d {
		if !x.OneSided {
			t.Errorf("a non-claimant paired: %s", x.Format())
		}
	}
}

func TestD6Kind(t *testing.T) {
	s := interactive("s", 10)
	s.Kind, s.KindRaw = model.KindBackground, claude.ValueKindBG
	l := libRow(s)
	tt := ttRow(lk(s), l, map[string]Value{FieldKind: {S: "interactive"}})
	if d := one(t, Compare([]Row{tt}, []Row{l}, nil)); d.Cited != D6Kind {
		t.Fatalf("%s", d.Format())
	}
	s.KindRaw = "something-else"
	l = libRow(s)
	if d := one(t, Compare([]Row{ttRow(lk(s), l, map[string]Value{FieldKind: {S: "interactive"}})}, []Row{l}, nil)); d.Cited != "" {
		t.Fatalf("%s", d.Format())
	}
}

// The two attention rows take their reasons from the ladder itself
// (verdict.Resolve under tt's rules), so the test cannot drift from the words
// verdict really writes.
func TestD9aAndD9b(t *testing.T) {
	rules := verdict.Rules{IgnoreAPIErrors: true}

	undecided := interactive("s", 10)
	undecided.Status, undecided.StatusRaw = model.StatusUnknown, "a word this build does not know"
	undecided = verdict.Resolve(undecided, verdict.Screen{}, epoch, rules)
	if undecided.Attention.Needed != nil || !verdict.IsUndetermined(undecided.Attention.Reason) {
		t.Fatalf("the setup: %+v", undecided.Attention)
	}
	l := libRow(undecided)
	tt := ttRow(lk(undecided), l, map[string]Value{FieldAttention: {S: "false"}})
	if d := one(t, Compare([]Row{tt}, []Row{l}, nil)); d.Cited != D9a {
		t.Fatalf("%s", d.Format())
	}
	tw := ttRow(lk(undecided), l, map[string]Value{FieldAttention: {S: "false"}, FieldStatus: {S: "waiting"}})
	for _, d := range Compare([]Row{tw}, []Row{l}, nil) {
		if d.Cited == D9a {
			t.Fatalf("tt waiting is never D9a: %s", d.Format())
		}
	}

	gone := interactive("s", 10)
	gone.Status, gone.StatusRaw = model.StatusWaiting, claude.ValueStatusWaiting
	gone.ProcessAlive = model.LivenessNo
	gone = verdict.Resolve(gone, verdict.Screen{}, epoch, rules)
	if gone.Attention.Needed == nil || *gone.Attention.Needed || !verdict.IsLivenessDismissal(gone.Attention.Reason) {
		t.Fatalf("the setup: %+v", gone.Attention)
	}
	l = libRow(gone)
	tt = ttRow(lk(gone), l, map[string]Value{FieldAttention: {S: "true"}})
	if d := one(t, Compare([]Row{tt}, []Row{l}, nil)); d.Cited != D9b {
		t.Fatalf("%s", d.Format())
	}
	gone.Job = &model.JobFacts{Dir: "j"} // Rule 1 does not apply to a job
	l = libRow(gone)
	tt = ttRow(lk(gone), l, map[string]Value{FieldAttention: {S: "true"}})
	for _, d := range Compare([]Row{tt}, []Row{l}, nil) {
		if d.Cited == D9b {
			t.Fatalf("a job's attention is never D9b: %s", d.Format())
		}
	}
}

func TestD11D12aD12b(t *testing.T) {
	// D11: a lib-only row with a missing-field problem.
	orphan := interactive("", 0)
	orphan.Key = model.Key{Orphan: "x.json"}
	orphan.PID = 0
	orphan.Registry.File = "x.json"
	missing := model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemMissingField, Path: "x.json", Field: claude.KeySessionID}
	if d := one(t, Compare(nil, []Row{libRow(orphan)}, []model.Problem{missing})); d.Cited != D11 || !d.OneSided {
		t.Fatalf("%s", d.Format())
	}
	if d := one(t, Compare(nil, []Row{libRow(orphan)}, nil)); d.Cited != "" {
		t.Fatalf("no problem, no D11: %s", d.Format())
	}

	// D12a: tt kept one of two interactive records on one id.
	a, b := interactive("dup", 1000), interactive("dup", 2000)
	la, lb := libRow(a), libRow(b)
	if d := one(t, Compare([]Row{ttRow(lk(a), la, nil)}, []Row{la, lb}, nil)); d.Cited != D12a || d.Key != lk(b) {
		t.Fatalf("%s", d.Format())
	}
	if d := one(t, Compare([]Row{ttRow(RowKey{SessionID: "other", PID: 1}, la, nil)}[:0], []Row{lb}, nil)); d.Cited != "" {
		t.Fatalf("one lib row on the id, none on tt: %s", d.Format())
	}

	// D12b: tt publishes the non-winning bg claimant as its own row.
	job := bgSession("s", "j", 10, 11)
	job.Job = &model.JobFacts{Dir: "j", Short: "j"}
	claim := model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemDuplicateJobClaim, PIDs: []int{10, 11}, Key: &job.Key}
	lj := libRow(job)
	ttJob := ttRow(lk(job), lj, nil)
	ttSpare := ttRow(RowKey{SessionID: "s", PID: 10}, lj, nil)
	d := Compare([]Row{ttJob, ttSpare}, []Row{lj}, []model.Problem{claim})
	if x := one(t, d); x.Cited != D12b || !x.OneSided {
		t.Fatalf("%s", x.Format())
	}
	if x := one(t, Compare([]Row{ttJob, ttSpare}, []Row{lj}, nil)); x.Cited != "" {
		t.Fatalf("no duplicate claim, no D12b: %s", x.Format())
	}
}

func TestD12c(t *testing.T) {
	job := bgSession("s", "j", 10, 11)
	job.Job = &model.JobFacts{Dir: "j", Short: "j"}
	claim := model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemDuplicateJobClaim, PIDs: []int{10, 11}, Key: &job.Key}
	lj := libRow(job)
	tt := ttRow(lk(job), lj, map[string]Value{FieldPID: {S: "10"}, FieldProcAlive: {S: "false"}})
	d := Compare([]Row{tt}, []Row{lj}, []model.Problem{claim})
	if len(d) != 2 || d[0].Cited != D12c || d[1].Cited != D12c {
		t.Fatalf("%v", formatAll(d))
	}
	// A job.* field is never speaker-derived.
	tt = ttRow(lk(job), lj, map[string]Value{FieldPID: {S: "10"}, FieldTokens: {S: "5"}})
	for _, x := range Compare([]Row{tt}, []Row{lj}, []model.Problem{claim}) {
		if x.Field == FieldTokens && x.Cited != "" {
			t.Fatalf("tokens cited: %s", x.Format())
		}
	}
	// tt's pid not a claimant: no D12c.
	tt = ttRow(lk(job), lj, map[string]Value{FieldPID: {S: "12"}})
	if x := one(t, Compare([]Row{tt}, []Row{lj}, []model.Problem{claim})); x.Cited != "" {
		t.Fatalf("%s", x.Format())
	}
}

func TestD12d(t *testing.T) {
	s := model.Session{Key: model.Key{SessionID: "s", Job: "k"}, Kind: model.KindBackground,
		Job: &model.JobFacts{Dir: "a", Short: "k"}, Transcript: &model.TranscriptFacts{LastActivityAt: ptr(epoch)}}
	dup := model.Problem{Scope: model.ScopeRecord, Kind: model.ProblemDuplicateKey, Key: &s.Key,
		Detail: "2 job directories describe one session: a, b; a speaks"}
	l := libRow(s)
	tt := ttRow(lk(s), l, map[string]Value{FieldLastActivityAt: {S: "1"}})
	if d := one(t, Compare([]Row{tt}, []Row{l}, []model.Problem{dup})); d.Cited != D12d {
		t.Fatalf("%s", d.Format())
	}
	reg := dup
	reg.Detail, reg.PIDs = "2 registry files describe one session: 1.json, 2.json; 1.json speaks", []int{1}
	if d := one(t, Compare([]Row{tt}, []Row{l}, []model.Problem{reg})); d.Cited != "" {
		t.Fatalf("a registry duplicate is not D12d: %s", d.Format())
	}
	other := ttRow(lk(s), l, map[string]Value{FieldRecordUpdatedAt: {S: "1"}})
	if d := one(t, Compare([]Row{other}, []Row{l}, []model.Problem{dup})); d.Cited != "" {
		t.Fatalf("D12d is lastActivityAt only: %s", d.Format())
	}
}

func TestInputOrderNeverMatters(t *testing.T) {
	a, b := interactive("a", 1), interactive("b", 2)
	la, lb := libRow(a), libRow(b)
	ta := ttRow(lk(a), la, map[string]Value{FieldName: {S: "x"}})
	tb := ttRow(lk(b), lb, map[string]Value{FieldCWD: {S: "y"}})
	d1 := Compare([]Row{ta, tb}, []Row{la, lb}, nil)
	d2 := Compare([]Row{tb, ta}, []Row{lb, la}, nil)
	if !slices.EqualFunc(d1, d2, func(x, y Diff) bool { return x.Format() == y.Format() }) {
		t.Fatalf("%v\n%v", formatAll(d1), formatAll(d2))
	}
}

func TestFromTT(t *testing.T) {
	doc := []byte(`{"observedAt":"2026-09-18T02:40:11.204Z","sessions":[
	 {"sessionId":"a","shortId":"j","pid":null,"kind":"background","status":"blocked","waitReason":"",
	  "statusSince":"2026-09-18T02:39:11.204Z","statusAgeMs":60000,"blockedForMs":60001,
	  "recordUpdatedAt":"2026-09-18T02:39:11.204Z","recordAgeMs":59000,
	  "transcript":{"path":"/testdata/projects/p/a.jsonl","subagents":null,"subagentsKnown":false,"colorFound":true,"agentColor":"","apiErrors":0,"compactions":0}},
	 {"sessionId":"b","shortId":null,"pid":42,"kind":"interactive","status":"waiting","waitReason":"permission-prompt",
	  "transcript":{"path":"/elsewhere/../etc/x","subagents":3,"subagentsKnown":true,"colorFound":false,"agentColor":""}}]}`)
	rows, at, err := FromTT(doc, "/testdata/projects")
	if err != nil {
		t.Fatal(err)
	}
	if !at.Equal(epoch) || len(rows) != 2 {
		t.Fatalf("%v %d", at, len(rows))
	}
	a, b := rows[0], rows[1]
	if a.Key != (RowKey{SessionID: "a", Job: "j"}) || b.Key != (RowKey{SessionID: "b", PID: 42}) {
		t.Fatalf("%v %v", a.Key, b.Key)
	}
	if a.field(FieldTranscriptPath) != (Value{S: "p/a.jsonl"}) || b.field(FieldTranscriptPath) != (Value{S: "outside:/etc/x"}) {
		t.Fatalf("paths: %v %v", a.field(FieldTranscriptPath), b.field(FieldTranscriptPath))
	}
	if a.field(FieldAgentColor) != (Value{S: "="}) || !b.field(FieldAgentColor).Null {
		t.Fatalf("an explicitly empty colour is a value; an unfound one is null")
	}
	if !a.field(FieldWaitReason).Null || b.field(FieldWaitReason) != (Value{S: "permission"}) {
		t.Fatalf("D14 spellings")
	}
	if _, ok := a.Fields[FieldStatusAgeMs]; ok {
		t.Fatal("a consistent age is dropped")
	}
	if _, ok := a.Fields[FieldBlockedForMs]; ok {
		t.Fatal("a 1 ms rounding is within the bound")
	}
	if v, ok := a.Fields[FieldRecordAgeMs]; !ok || !strings.Contains(v.S, "inconsistent") {
		t.Fatalf("an age its own instant does not explain stays visible: %v", a.Fields)
	}
	if _, _, err := FromTT([]byte("not json"), "/x"); !errors.Is(err, ErrNotATTDocument) {
		t.Fatal(err)
	}
}

func TestWaitingIsUnifiedPerD14(t *testing.T) {
	s := interactive("s", 1)
	s.Status = model.StatusBlocked
	if v := libWaiting(s); !v.Null {
		t.Fatalf("blocked with no decided reason is tt's \"\": %v", v)
	}
	s.Status = model.StatusWaiting
	if v := libWaiting(s); v != (Value{S: "unknown"}) {
		t.Fatalf("waiting with no decided reason is tt's \"unknown\": %v", v)
	}
	s.Waiting = model.WaitingInput
	if v := libWaiting(s); v != (Value{S: "input"}) {
		t.Fatal(v)
	}
	s.Status = model.StatusIdle
	if v := libWaiting(s); !v.Null {
		t.Fatal("not applicable is null")
	}
}

// §8.2 step 4's preflight, with the planted 99.json/100.json case: canonically
// 100.json speaks ("100.json" < "99.json"), and any ascending map of two pids
// of one digit count flips it.
func TestFilenameOrderKept(t *testing.T) {
	groups := [][]string{{"99.json", "100.json"}}
	if err := FilenameOrderKept(groups, map[int]int{99: 50001, 100: 50002, 200: 50003}); err == nil {
		t.Fatal("the planted 99/100 case passed the preflight")
	}
	if err := FilenameOrderKept([][]string{{"100.json", "150.json"}}, map[int]int{100: 50001, 150: 50002}); err != nil {
		t.Fatalf("one digit count keeps its order: %v", err)
	}
	if err := FilenameOrderKept([][]string{{"duplicate-a.json", "duplicate-b.json"}}, map[int]int{1000: 5}); err != nil {
		t.Fatalf("names that carry no pid are not renamed: %v", err)
	}
	if err := FilenameOrderKept([][]string{{"9.json", "10.json"}}, map[int]int{9: 9999, 10: 10000}); err != nil {
		t.Fatalf("a map that keeps the byte order passes: %v", err)
	}
}

func TestThePIDRewrite(t *testing.T) {
	m := map[int]int{98198: 7, 60011: 8, 60099: 9}
	if got := RenameByPID("98198.abc.key", m); got != "7.abc.key" {
		t.Fatal(got)
	}
	if got := RenameByPID("98198.json", m); got != "7.json" {
		t.Fatal(got)
	}
	if got := RenameByPID("live-idle.json", m); got != "live-idle.json" {
		t.Fatal(got)
	}
	body := []byte(`{"` + claude.KeyPID + `": 60099, "x": {"` + claude.KeyPID + `":98198}}`)
	got, err := RewritePIDs(body, m)
	if err != nil || !bytes.Equal(got, []byte(`{"`+claude.KeyPID+`": 9, "x": {"`+claude.KeyPID+`":7}}`)) {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := RewritePIDs([]byte(`{"`+claude.KeyPID+`":5}`), m); !errors.Is(err, ErrUnmappedPID) {
		t.Fatalf("an unmapped pid must refuse: %v", err)
	}
	names := map[string][]byte{"60011.json": []byte(`{"` + claude.KeyPID + `":60099}`), "98198.k.key": nil}
	if got := CanonicalPIDs(names, []byte(`[{"`+claude.KeyPID+`": 4}]`)); !slices.Equal(got, []int{4, 60011, 60099, 98198}) {
		t.Fatalf("%v", got)
	}
	if got := ReplacePrefix([]byte(`{"p":"/testdata/projects/a"}`), "/testdata/projects", "/sb/root/projects"); string(got) != `{"p":"/sb/root/projects/a"}` {
		t.Fatal(string(got))
	}
	if SafeForJSON(`/tmp/a"b`) || SafeForJSON("/tmp/a'b") || SafeForJSON("/tmp/a\nb") || !SafeForJSON("/var/folders/x/T/claudestate-diff-1") {
		t.Fatal("SafeForJSON")
	}
}

func TestCanonicalise(t *testing.T) {
	doc := []byte(`{"pid":7,"sessions":[{"pid":9,"path":"/sb/s/x/root/projects/p.jsonl","state":"/sb/s/x/state/tt","n":12}]}`)
	got, err := Canonicalise(doc, map[int]int{7: 98198, 9: 60099}, []Rewrite{{"/sb/s/x/root", "/testdata"}, {"/sb/s/x", "/sandbox"}}, "/sb")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"pid": 98198`, `"pid": 60099`, `"/testdata/projects/p.jsonl"`, `"/sandbox/state/tt"`, `"n": 12`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("%s missing from\n%s", want, got)
		}
	}
	if _, err := Canonicalise([]byte(`{"p":"/private/sb/s/x/root"}`), nil, []Rewrite{{"/sb/s/x/root", "/testdata"}}, "/sb"); err == nil {
		t.Fatal("a resolved sandbox path survived the rewrite and was not refused")
	}
}

func TestSummarise(t *testing.T) {
	r := Summarise([]Diff{{Cited: D1}, {Cited: D1}, {OneSided: true, Cited: D11}, {Field: "x"}})
	if r.PerRow[D1] != 2 || r.PerRow[D11] != 1 || r.OneSided != 1 || len(r.Uncited) != 1 || len(r.Cited) != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestEveryCorpusScenarioHasARoot(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Corpus() {
		if seen[s.Name] || s.Root == "" {
			t.Fatalf("%+v", s)
		}
		seen[s.Name] = true
		if got, ok := ScenarioNamed(s.Name); !ok || got.Name != s.Name {
			t.Fatal(s.Name)
		}
	}
	r := NewRecorded(Scenario{Name: "x", Root: "x", Dead: []int{2}}, []int{1, 2})
	back, err := DecodeRecorded(r.Encode())
	if err != nil || !slices.Equal(back.DeadPIDs(), []int{2}) || back.Liveness["1"] != Alive {
		t.Fatalf("%+v %v", back, err)
	}
	if _, err := DecodeRecorded([]byte(`{"liveness":{"1":"maybe"}}`)); err == nil {
		t.Fatal("a liveness word outside alive|dead must refuse")
	}
}
