package claude_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

func bbHasProblem(ps []model.Problem, kind model.ProblemKind, field string) bool {
	for _, p := range ps {
		if p.Scope == model.ScopeRecord && p.Kind == kind && p.Field == field {
			return true
		}
	}
	return false
}

func bbInstant(t *testing.T, got *time.Time, want time.Time) {
	t.Helper()
	if got == nil || !got.Equal(want) {
		t.Fatalf("instant = %v; want %v", got, want)
	}
}

func TestBBRecordNameClassification(t *testing.T) {
	for _, tt := range []struct {
		name string
		want bool
	}{
		{"123.json", true}, {"synthetic.json", true}, {"123.deadbeef.key", false},
		{"123.json.tmp", false}, {"state.jsonl", false}, {"", false},
	} {
		if got := claude.IsRecordName(tt.name); got != tt.want {
			t.Errorf("IsRecordName(%q) = %v; want %v", tt.name, got, tt.want)
		}
	}
	for _, tt := range []struct {
		name string
		pid  int
		ok   bool
	}{
		{"123.json", 123, true}, {"synthetic.json", 0, false},
		{"123.deadbeef.key", 0, false}, {"123.jsonl", 0, false}, {"", 0, false},
	} {
		if got, ok := claude.PIDFromName(tt.name); got != tt.pid || ok != tt.ok {
			t.Errorf("PIDFromName(%q) = (%d, %v); want (%d, %v)", tt.name, got, ok, tt.pid, tt.ok)
		}
	}
}

func TestBBDecodeRecordPreservesUnionFieldsAndRawText(t *testing.T) {
	// Numeric formerNames.until is an inferred input shape, recorded in TEST-PLAN.
	// "permission prompt" is Claude's spelling of the permission wait (contract §4.10 A).
	b := []byte(`{"sessionId":"session-synthetic","pid":123,"status":"waiting","waitingFor":"permission prompt","statusUpdatedAt":1700000000000,"updatedAt":1700000001000,"startedAt":1699999999000,"name":"name\u001b]52;c;raw\u0007","nameSource":"user","formerNames":["first",{"name":"second","until":1700000000000,"sessionId":"old-session"}],"cwd":"/testdata/live","tmux":"not-a-tmux-reference","version":"9.9.9","messagingSocketPath":"/testdata/socket","kind":"bg","jobId":"job-a","peerProtocol":3,"peerFeatures":["one","two"]}`)
	got, ps, err := claude.DecodeRecord("123.json", b)
	if err != nil || len(ps) != 0 {
		t.Fatalf("DecodeRecord = problems %v, error %v", ps, err)
	}
	if got.File != "123.json" || got.SessionID != "session-synthetic" || got.PID != 123 || got.NamePID != 123 {
		t.Errorf("identity = %+v", got)
	}
	if got.Status != model.StatusWaiting || got.StatusRaw != "waiting" || got.Waiting != model.WaitingPermission || got.WaitingRaw != "permission prompt" {
		t.Errorf("status = %+v", got)
	}
	bbInstant(t, got.StatusUpdatedAt, time.UnixMilli(1700000000000))
	bbInstant(t, got.UpdatedAt, time.UnixMilli(1700000001000))
	bbInstant(t, got.StartedAt, time.UnixMilli(1699999999000))
	if got.Name != "name\x1b]52;c;raw\a" || got.NameSource != "user" || got.CWD != "/testdata/live" {
		t.Errorf("raw name/cwd = %+v", got)
	}
	if len(got.FormerNames) != 2 || got.FormerNames[0].Name != "first" || got.FormerNames[1].Name != "second" || got.FormerNames[1].SessionID != "old-session" {
		t.Fatalf("former names = %+v", got.FormerNames)
	}
	bbInstant(t, got.FormerNames[1].Until, time.UnixMilli(1700000000000))
	if got.Tmux == nil || got.Tmux.Raw != "not-a-tmux-reference" || got.Tmux.Session != "" || got.Tmux.Window != "" || got.Tmux.Pane != "" {
		t.Errorf("unparseable tmux = %+v", got.Tmux)
	}
	if got.Version != "9.9.9" || got.MessagingSocketPath == nil || *got.MessagingSocketPath != "/testdata/socket" || got.Kind != model.KindBackground || got.KindRaw != "bg" || got.JobID != "job-a" {
		t.Errorf("union fields = %+v", got)
	}
	if got.PeerProtocol == nil || *got.PeerProtocol != 3 || !reflect.DeepEqual(got.PeerFeatures, []string{"one", "two"}) {
		t.Errorf("peer fields = %+v", got)
	}
}

func TestBBRegistryVocabularyUnknownsAndWaitingDialog(t *testing.T) {
	for _, tt := range []struct {
		word string
		want model.Status
	}{
		{"idle", model.StatusIdle}, {"busy", model.StatusBusy}, {"shell", model.StatusShell},
		{"waiting", model.StatusWaiting}, {"dialog open", model.StatusUnknown}, {"future-status", model.StatusUnknown},
	} {
		got, _, err := claude.DecodeRecord("123.json", []byte(`{"sessionId":"s","pid":123,"status":"`+tt.word+`"}`))
		if err != nil || got.Status != tt.want || got.StatusRaw != tt.word {
			t.Errorf("status %q = (%v, %q, %v); want %v with raw preserved", tt.word, got.Status, got.StatusRaw, err, tt.want)
		}
	}
	// Contract §4.10 A: Claude's three waitingFor words, and the bare enum names
	// "input" / "permission" are NOT Claude's spellings, so they are Unknown with the
	// raw word kept (C4), like any other unrecognised word.
	for _, tt := range []struct {
		word string
		want model.WaitingReason
	}{
		{"input needed", model.WaitingInput}, {"permission prompt", model.WaitingPermission},
		{"dialog open", model.WaitingDialog},
		{"input", model.WaitingUnknown}, {"permission", model.WaitingUnknown},
		{"future-wait", model.WaitingUnknown},
	} {
		got, _, err := claude.DecodeRecord("123.json", []byte(`{"sessionId":"s","pid":123,"status":"waiting","waitingFor":"`+tt.word+`"}`))
		if err != nil || got.Waiting != tt.want || got.WaitingRaw != tt.word {
			t.Errorf("waitingFor %q = (%v, %q, %v)", tt.word, got.Waiting, got.WaitingRaw, err)
		}
	}
	got, _, err := claude.DecodeRecord("123.json", []byte(`{"sessionId":"s","pid":123,"kind":"future-kind"}`))
	if err != nil || got.Kind != model.KindUnknown || got.KindRaw != "future-kind" {
		t.Errorf("unknown kind = (%v, %q, %v)", got.Kind, got.KindRaw, err)
	}
	for _, tt := range []struct {
		raw  string
		want bool
	}{
		{"permission", true}, {"permission prompt", true}, {"input", false}, {"dialog open", false}, {"", false},
	} {
		if got := claude.LooksLikePermission(tt.raw); got != tt.want {
			t.Errorf("LooksLikePermission(%q) = %v; want %v", tt.raw, got, tt.want)
		}
	}
}

func TestBBDecodeRecordIncompleteIdentityAndPIDMismatch(t *testing.T) {
	for _, tt := range []struct {
		name, body, missing string
		pid                 int
		fromName            bool
	}{
		{"123.json", `{"sessionId":"s"}`, "pid", 123, true},
		{"unknown.json", `{"sessionId":"s"}`, "pid", 0, false},
		{"123.json", `{"pid":123}`, "sessionId", 123, false},
	} {
		got, ps, err := claude.DecodeRecord(tt.name, []byte(tt.body))
		if err != nil || got.PID != tt.pid || got.PIDFromName != tt.fromName || !bbHasProblem(ps, model.ProblemMissingField, tt.missing) {
			t.Errorf("incomplete record %s = %+v, %v, %v", tt.body, got, ps, err)
		}
	}
	got, ps, err := claude.DecodeRecord("123.json", []byte(`{"sessionId":"s","pid":456}`))
	if err != nil || got.PID != 456 || got.NamePID != 123 || got.PIDFromName {
		t.Fatalf("body pid must win: %+v, %v, %v", got, ps, err)
	}
	found := false
	for _, p := range ps {
		found = found || p.Kind == model.ProblemPIDMismatch
	}
	if !found {
		t.Errorf("pid mismatch has no problem: %v", ps)
	}
}

func TestBBDecodeRecordWrongFieldTypesAreProblemsNotTorn(t *testing.T) {
	got, ps, err := claude.DecodeRecord("unknown.json", []byte(`{"sessionId":17,"pid":"123","status":true,"cwd":[],"statusUpdatedAt":{}}`))
	if err != nil {
		t.Fatalf("object with bad fields returned error: %v", err)
	}
	if got.SessionID != "" || got.PID != 0 || got.Status != model.StatusUnknown || got.StatusRaw != "" || got.CWD != "" || got.StatusUpdatedAt != nil {
		t.Errorf("bad fields were coerced: %+v", got)
	}
	for _, field := range []string{"sessionId", "pid", "status", "cwd", "statusUpdatedAt"} {
		if !bbHasProblem(ps, model.ProblemUndecoded, field) {
			t.Errorf("missing ProblemUndecoded for %q: %v", field, ps)
		}
	}
}

func TestBBRecordAndJobNonObjectsAreTorn(t *testing.T) {
	for _, body := range []string{"", "{", "[]", "null", `"text"`, "true", "42"} {
		if _, _, err := claude.DecodeRecord("123.json", []byte(body)); !errors.Is(err, claude.ErrTorn) {
			t.Errorf("DecodeRecord(%q) error = %v; want ErrTorn", body, err)
		}
		if _, _, err := claude.DecodeJob("job-a", []byte(body)); !errors.Is(err, claude.ErrTorn) {
			t.Errorf("DecodeJob(%q) error = %v; want ErrTorn", body, err)
		}
	}
}

func TestBBJobStatePathDepthAndDecodeFields(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		{"job-a/state.json", true}, {"state.json", false}, {"job-a/deeper/state.json", false},
		{"job-a/timeline.jsonl", false}, {"job-a/state.json.tmp", false}, {"", false},
	} {
		if got := claude.IsJobStatePath(tt.path); got != tt.want {
			t.Errorf("IsJobStatePath(%q) = %v; want %v", tt.path, got, tt.want)
		}
	}
	got, ps, err := claude.DecodeJob("job-a", []byte(`{"sessionId":"s","daemonShort":"job-a","state":"working","tempo":"blocked","detail":"detail\u001b","needs":"approval","tokens":123456,"cwd":"/testdata/job","name":"job name","nameSource":"auto","cliVersion":"9.9.9","children":["child-a","child-b"],"createdAt":"2023-11-14T22:13:20Z","updatedAt":"2023-11-14T22:13:21Z","reapedMidWorkAt":"2023-11-14T22:13:22.000Z","linkScanPath":"/testdata/projects/p/s.jsonl","resumeSessionId":"resume-s"}`))
	if err != nil || len(ps) != 0 {
		t.Fatalf("DecodeJob = %v, %v", ps, err)
	}
	if got.Dir != "job-a" || got.Short != "job-a" || got.DaemonShort != "job-a" || got.SessionID != "s" || got.State != model.StatusWorking || got.StateRaw != "working" {
		t.Errorf("job identity/state = %+v", got)
	}
	if got.Tempo == nil || *got.Tempo != "blocked" || got.Detail == nil || *got.Detail != "detail\x1b" || got.Needs == nil || *got.Needs != "approval" || got.Tokens == nil || *got.Tokens != 123456 {
		t.Errorf("job optional fields = %+v", got)
	}
	if got.CWD != "/testdata/job" || got.Name != "job name" || got.NameSource != "auto" || got.CLIVersion != "9.9.9" || got.LinkScanPath != "/testdata/projects/p/s.jsonl" || got.ResumeSessionID != "resume-s" || !reflect.DeepEqual(got.Children, []string{"child-a", "child-b"}) {
		t.Errorf("job fields = %+v", got)
	}
	bbInstant(t, got.CreatedAt, time.UnixMilli(1700000000000))
	bbInstant(t, got.UpdatedAt, time.UnixMilli(1700000001000))
	bbInstant(t, got.ReapedMidWorkAt, time.UnixMilli(1700000002000))
}

func TestBBJobUnknownMissingAndShortMismatch(t *testing.T) {
	got, ps, err := claude.DecodeJob("job-a", []byte(`{"daemonShort":"other-short","state":"future-state","tempo":"future-tempo"}`))
	if err != nil || got.Short != "other-short" || got.SessionID != "" || got.State != model.StatusUnknown || got.StateRaw != "future-state" || got.Tempo == nil || *got.Tempo != "future-tempo" {
		t.Fatalf("unknown/incomplete job = %+v, %v, %v", got, ps, err)
	}
	if !bbHasProblem(ps, model.ProblemMissingField, "sessionId") {
		t.Errorf("missing sessionId has no problem: %v", ps)
	}
	found := false
	for _, p := range ps {
		found = found || p.Kind == model.ProblemShortIDMismatch
	}
	if !found {
		t.Errorf("short mismatch has no problem: %v", ps)
	}
	got, _, err = claude.DecodeJob("job-a", []byte(`{"sessionId":"s","state":"done"}`))
	if err != nil || got.Short != "job-a" || got.DaemonShort != "" || got.State != model.StatusDone {
		t.Errorf("directory short fallback = %+v, %v", got, err)
	}
}

func TestBBInstantBoundariesAndNoRescaling(t *testing.T) {
	for _, tt := range []struct {
		ms      int64
		present bool
		bad     bool
	}{
		{-1, false, false}, {0, false, false}, {1, false, true}, {999999999999, false, true},
		{1000000000000, true, false}, {1700000000000, true, false},
		{100000000000000, true, false}, {100000000000001, false, true},
		{1700000000, false, true}, {1700000000000000, false, true},
	} {
		got, err := claude.DecodeInstantMillis(tt.ms)
		if tt.bad {
			if !errors.Is(err, claude.ErrImplausibleInstant) || got != nil {
				t.Errorf("milliseconds %d = %v, %v; want nil, ErrImplausibleInstant", tt.ms, got, err)
			}
			continue
		}
		if err != nil || (got != nil) != tt.present {
			t.Errorf("milliseconds %d = %v, %v; present=%v", tt.ms, got, err, tt.present)
		} else if got != nil {
			bbInstant(t, got, time.UnixMilli(tt.ms))
		}
	}
	if got, err := claude.DecodeInstantString(""); got != nil || err != nil {
		t.Errorf("empty RFC3339 = %v, %v", got, err)
	}
	got, err := claude.DecodeInstantString("2023-11-14T22:13:20.123+02:00")
	want, parseErr := time.Parse(time.RFC3339Nano, "2023-11-14T22:13:20.123+02:00")
	if parseErr != nil || err != nil {
		t.Fatalf("RFC3339 errors = %v, %v", parseErr, err)
	}
	bbInstant(t, got, want)
	if got, err := claude.DecodeInstantString("not-an-instant"); err == nil || got != nil {
		t.Errorf("malformed instant = %v, %v; want nil, error", got, err)
	}
}

func TestBBImplausibleRecordAndJobInstantsAreFieldProblems(t *testing.T) {
	r, ps, err := claude.DecodeRecord("123.json", []byte(`{"sessionId":"s","pid":123,"statusUpdatedAt":1700000000,"updatedAt":1700000000000000,"startedAt":1}`))
	if err != nil || r.StatusUpdatedAt != nil || r.UpdatedAt != nil || r.StartedAt != nil {
		t.Fatalf("record instants rescaled: %+v, %v", r, err)
	}
	for _, field := range []string{"statusUpdatedAt", "updatedAt", "startedAt"} {
		if !bbHasProblem(ps, model.ProblemImplausibleInstant, field) {
			t.Errorf("missing instant problem for %s: %v", field, ps)
		}
	}
	// Contract §4.10 B: job instants are RFC3339 strings, and C4's window
	// (1e12..1e14 ms) applies to the value whatever its spelling. 1970-01-01T00:00:01Z
	// is 1e3 ms; 5138-11-16T09:46:40.001Z is 1e14+1 ms. reapedMidWorkAt is the
	// plausible control: it decodes and raises nothing.
	j, ps, err := claude.DecodeJob("job-a", []byte(`{"sessionId":"s","createdAt":"1970-01-01T00:00:01Z","updatedAt":"5138-11-16T09:46:40.001Z","reapedMidWorkAt":"2023-11-14T22:13:20Z"}`))
	if err != nil || j.CreatedAt != nil || j.UpdatedAt != nil {
		t.Fatalf("job instants rescaled: %+v, %v", j, err)
	}
	for _, field := range []string{"createdAt", "updatedAt"} {
		if !bbHasProblem(ps, model.ProblemImplausibleInstant, field) {
			t.Errorf("missing instant problem for %s: %v", field, ps)
		}
	}
	bbInstant(t, j.ReapedMidWorkAt, time.UnixMilli(1700000000000))
	for _, p := range ps {
		if p.Field == "reapedMidWorkAt" {
			t.Errorf("plausible control instant raised a problem: %v", p)
		}
	}
}

func TestBBInstantOfTheOtherJSONTypeIsUndecodedNeverCoerced(t *testing.T) {
	// Contract §4.10 B: a value of the other JSON type is an undecoded-field Problem,
	// never a coerced instant. Job instants are strings, so a plausible millisecond
	// NUMBER is refused; registry instants are milliseconds, so a plausible RFC3339
	// STRING is refused.
	j, ps, err := claude.DecodeJob("job-a", []byte(`{"sessionId":"s","createdAt":1700000000000,"updatedAt":1700000001000,"reapedMidWorkAt":1700000002000}`))
	if err != nil {
		t.Fatalf("job with numeric instants returned error: %v", err)
	}
	if j.CreatedAt != nil || j.UpdatedAt != nil || j.ReapedMidWorkAt != nil {
		t.Errorf("numeric job instants were coerced: %+v", j)
	}
	for _, field := range []string{"createdAt", "updatedAt", "reapedMidWorkAt"} {
		if !bbHasProblem(ps, model.ProblemUndecoded, field) {
			t.Errorf("numeric job %s has no undecoded-field problem: %v", field, ps)
		}
	}
	r, ps, err := claude.DecodeRecord("123.json", []byte(`{"sessionId":"s","pid":123,"statusUpdatedAt":"2023-11-14T22:13:20Z","updatedAt":"2023-11-14T22:13:21Z","startedAt":"2023-11-14T22:13:19Z"}`))
	if err != nil {
		t.Fatalf("record with string instants returned error: %v", err)
	}
	if r.StatusUpdatedAt != nil || r.UpdatedAt != nil || r.StartedAt != nil {
		t.Errorf("string registry instants were coerced: %+v", r)
	}
	for _, field := range []string{"statusUpdatedAt", "updatedAt", "startedAt"} {
		if !bbHasProblem(ps, model.ProblemUndecoded, field) {
			t.Errorf("string registry %s has no undecoded-field problem: %v", field, ps)
		}
	}
}

func TestBBDecodeAgentsEmptyMalformedAndUnion(t *testing.T) {
	at := time.UnixMilli(1700000000000)
	empty, ps, err := claude.DecodeAgents([]byte("[]"), at)
	if err != nil || len(ps) != 0 || len(empty.Entries) != 0 || !empty.At.Equal(at) {
		t.Fatalf("empty listing = %+v, %v, %v", empty, ps, err)
	}
	for _, raw := range []string{"", "[", "{}", `{"agents":[]}`, "not-json"} {
		if _, _, err := claude.DecodeAgents([]byte(raw), at); err == nil {
			t.Errorf("malformed listing %q accepted", raw)
		}
	}
	got, ps, err := claude.DecodeAgents([]byte(`[{"sessionId":"same","pid":123,"status":"waiting","waitingFor":"dialog open","name":"interactive","cwd":"/testdata/interactive","startedAt":1700000000000},{"sessionId":"same","id":"job-a","kind":"background","state":"blocked","name":"background","cwd":"/testdata/job"}]`), at)
	if err != nil || len(ps) != 0 || len(got.Entries) != 2 {
		t.Fatalf("agents union = %+v, %v, %v", got, ps, err)
	}
	// The agents listing spells the kind "background" (testdata corpus, 137 entries);
	// "bg" is the registry record's spelling, not the listing's.
	i, b := got.Entries[0], got.Entries[1]
	if i.PID == nil || *i.PID != 123 || i.Status != model.StatusWaiting || i.StatusRaw != "waiting" || i.Waiting != model.WaitingDialog || i.WaitingRaw != "dialog open" || !claude.IsInteractiveEntry(i) {
		t.Errorf("interactive entry = %+v", i)
	}
	bbInstant(t, i.StartedAt, at)
	if b.PID != nil || b.ID != "job-a" || b.State != model.StatusBlocked || b.StateRaw != "blocked" || b.Kind != model.KindBackground || b.KindRaw != "background" || claude.IsInteractiveEntry(b) {
		t.Errorf("background entry = %+v", b)
	}
}

func TestBBDecodeAgentsUnknownAndImplausibleInstant(t *testing.T) {
	got, ps, err := claude.DecodeAgents([]byte(`[{"sessionId":"s","pid":123,"status":"future-status","waitingFor":"future-wait","startedAt":1700000000}]`), time.UnixMilli(1700000000000))
	if err != nil || len(got.Entries) != 1 {
		t.Fatalf("unknown values dropped listing: %+v, %v, %v", got, ps, err)
	}
	e := got.Entries[0]
	if e.Status != model.StatusUnknown || e.StatusRaw != "future-status" || e.Waiting != model.WaitingUnknown || e.WaitingRaw != "future-wait" || e.StartedAt != nil {
		t.Errorf("unknown/raw/instant = %+v", e)
	}
	if !bbHasProblem(ps, model.ProblemImplausibleInstant, "startedAt") {
		t.Errorf("agents invalid instant has no field problem: %v", ps)
	}
}

func TestBBProjectDirectoryEncodingUTF16AndPunctuation(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"/testdata/a_b.c d", "-testdata-a-b-c-d"},
		{"/testdata/emoji-😀", "-testdata-emoji---"},
		{"/ABC/xyz09", "-ABC-xyz09"}, {"", ""},
	} {
		if got := claude.EncodeProjectDir(tt.input); got != tt.want {
			t.Errorf("EncodeProjectDir(%q) = %q; want %q", tt.input, got, tt.want)
		}
	}
	// Hash bytes, suffix width and separator have no supplied vector. Check only
	// shortening, determinism and an early preserved prefix, not a guessed hash.
	long := "/" + strings.Repeat("a", 1024)
	got := claude.EncodeProjectDir(long)
	if got != claude.EncodeProjectDir(long) || len(got) >= len(long) || !strings.HasPrefix(got, "-"+strings.Repeat("a", 100)) {
		t.Errorf("long encoding is not shortened/deterministic/prefix-preserving: %q", got)
	}
}
