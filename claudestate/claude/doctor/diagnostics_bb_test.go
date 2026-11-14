package doctor_test

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/claude/doctor"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

func bbCheck(t *testing.T, results []doctor.Result, name string) doctor.Result {
	t.Helper()
	var found []doctor.Result
	for _, result := range results {
		if result.Check == name {
			found = append(found, result)
		}
	}
	if len(found) != 1 {
		t.Fatalf("check %q occurs %d times: %+v", name, len(found), results)
	}
	return found[0]
}

func bbTree(fsys fs.FS, name string) ports.Tree {
	return ports.Tree{Path: "/synthetic/" + name, FS: fsys}
}

func bbNow() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) }

func TestBBDoctorReturnsExactlyTheTwentyContractChecksInOrder(t *testing.T) {
	want := []string{
		"registry-keys", "registry-status-values", "registry-waiting-for", "registry-tmux-shape", "registry-mtime-tracks-status", "registry-version-spread",
		"jobs-keys", "jobs-state-values", "jobs-dir-mtime-tracks-record", "jobs-updatedat-not-earlier-than-timeline", "jobs-timeline-shape", "jobs-linkscanpath", "jobs-signal-keys",
		"transcript-retention-floor", "transcript-deep-population", "transcript-untimestamped-types", "transcript-agent-color-shape", "transcript-api-error-key",
		"subagent-meta-files", "agents-cli-shape",
	}
	if got := doctor.Checks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Checks = %v, want FR11 order %v", got, want)
	}
	results := doctor.Run(doctor.Env{Now: bbNow()})
	if len(results) != len(want) {
		t.Fatalf("Run returned %d results, want %d", len(results), len(want))
	}
	for i, result := range results {
		if result.Check != want[i] {
			t.Errorf("result %d check = %q, want %q", i, result.Check, want[i])
		}
		switch result.Verdict {
		case doctor.VerdictOK, doctor.VerdictWarn, doctor.VerdictFail, doctor.VerdictSkip:
		default:
			t.Errorf("check %q returned a non-verdict: %v", result.Check, result.Verdict)
		}
	}
}

func TestBBDoctorUnknownRetentionIsAnExplicitSkipNotAPass(t *testing.T) {
	env := doctor.Env{Root: ports.Root{Projects: bbTree(fstest.MapFS{
		"project/sid.jsonl": {Data: []byte("{\"type\":\"user\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"content\":\"synthetic prompt\"}}\n"), ModTime: bbNow().Add(-24 * time.Hour)},
	}, "projects")}, Now: bbNow()}
	result := bbCheck(t, doctor.Run(env), "transcript-retention-floor")
	if result.Verdict != doctor.VerdictSkip {
		t.Fatalf("no settings/root retention = %+v, want Skip", result)
	}
	detail := strings.ToLower(result.Detail)
	if !strings.Contains(detail, "unknown") || !strings.Contains(detail, "root") {
		t.Errorf("skip did not explain missing root/unknown retention: %q", result.Detail)
	}
	for _, settings := range []fstest.MapFS{
		{"settings.json": {Data: []byte(`{}`)}},
		{"settings.json": {Data: []byte(`{"cleanupPeriodDays":30}`)}},
	} {
		env.Settings = settings
		if got := bbCheck(t, doctor.Run(env), "transcript-retention-floor"); got.Verdict == doctor.VerdictSkip {
			t.Errorf("settings supplies known/default retention, not unknown: %+v", got)
		}
	}
	settings := bbReads(fstest.MapFS{"settings.json": {Data: []byte(`{"cleanupPeriodDays":1}`)}})
	env.Settings, env.Retention = settings, 30*24*time.Hour
	if got := bbCheck(t, doctor.Run(env), "transcript-retention-floor"); got.Verdict == doctor.VerdictSkip {
		t.Errorf("explicit retention was ignored: %+v", got)
	}
	if settings.maximums()["settings.json"] != 0 {
		t.Error("explicit retention should override settings without reading it")
	}
}

func TestBBDoctorJobsKeysUsesTheMeasuredUniversalVocabulary(t *testing.T) {
	vocab := claude.Vocabulary()
	if len(vocab.Jobs.Universal) != 19 {
		t.Fatalf("measured jobs key union has %d keys, want 19", len(vocab.Jobs.Universal))
	}
	record := make(map[string]any)
	for _, key := range vocab.Jobs.Universal {
		record[key] = nil
	}
	// Contract §4.10 C: Universal is the always-present set, so a never-read key
	// measured as optional (suggestedReply) is in Optional, not Universal. Every
	// never-read key is still in the vocabulary doctor knows: Universal ∪ Optional.
	for _, key := range vocab.JobsNeverRead {
		_, inU := record[key]
		inO := false
		for _, o := range vocab.Jobs.Optional {
			inO = inO || o == key
		}
		if !inU && !inO {
			t.Errorf("deliberately unread key %q is in neither Jobs.Universal nor Jobs.Optional", key)
		}
	}
	if _, ok := record["suggestedReply"]; ok {
		t.Errorf("Jobs.Universal holds the optional key suggestedReply: %v", vocab.Jobs.Universal)
	}
	// Job instants are RFC3339 strings (§4.10 B); milliseconds would be a wrong-type
	// field and could colour the jobs-keys verdict for an unrelated reason.
	stamp := bbNow().Format(time.RFC3339)
	for key, value := range map[string]any{
		claude.KeySessionID: "sid", "daemonShort": "short", "state": "working",
		"detail": "detail", "needs": "permission", "tempo": "blocked", "tokens": 12,
		"cwd": "/synthetic/job", "name": "synthetic job", "nameSource": "test", "cliVersion": "0.0.0",
		"children": []string{"child"}, "createdAt": stamp, "updatedAt": stamp,
		"reapedMidWorkAt": stamp, "linkScanPath": "", "resumeSessionId": "sid",
	} {
		record[key] = value
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	env := doctor.Env{Root: ports.Root{Jobs: bbTree(fstest.MapFS{"short/state.json": {Data: data}}, "jobs")}, Now: bbNow()}
	if got := bbCheck(t, doctor.Run(env), "jobs-keys"); got.Verdict != doctor.VerdictOK {
		t.Fatalf("documented measured union is not accepted: %+v", got)
	}
	record["syntheticUnknownKey"] = true
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	env.Root.Jobs.FS = fstest.MapFS{"short/state.json": {Data: data}}
	got := bbCheck(t, doctor.Run(env), "jobs-keys")
	if got.Verdict != doctor.VerdictWarn && got.Verdict != doctor.VerdictFail {
		t.Errorf("unknown job key was not diagnosed: %+v", got)
	}
}

func TestBBDoctorMalformedAgentsAreNotReportedAsEmptySuccess(t *testing.T) {
	env := doctor.Env{Now: bbNow(), Agents: &ports.Listing{Raw: []byte(`not-json`), At: bbNow()}}
	got := bbCheck(t, doctor.Run(env), "agents-cli-shape")
	if got.Verdict != doctor.VerdictWarn && got.Verdict != doctor.VerdictFail {
		t.Errorf("malformed listing was not diagnosed: %+v", got)
	}
	env.Agents.Raw = []byte(`[]`)
	got = bbCheck(t, doctor.Run(env), "agents-cli-shape")
	if got.Verdict == doctor.VerdictFail {
		t.Errorf("valid empty listing was classified as malformed: %+v", got)
	}
}

// This read-only fs.FS exposes the total bytes actually requested from oversized records.
// All data is SYNTHETIC and resident in memory; doctor has no writable capability here.
type bbReadFS struct {
	base  fstest.MapFS
	reads chan bbRead
}

type bbRead struct {
	path string
	n    int
}

func bbReads(base fstest.MapFS) *bbReadFS {
	return &bbReadFS{base: base, reads: make(chan bbRead, 4096)}
}

func (f *bbReadFS) maximums() map[string]int {
	out := make(map[string]int)
	for {
		select {
		case read := <-f.reads:
			if read.n > out[read.path] {
				out[read.path] = read.n
			}
		default:
			return out
		}
	}
}

func (f *bbReadFS) Open(name string) (fs.File, error) {
	file, err := f.base.Open(name)
	if err != nil {
		return nil, err
	}
	if dir, ok := file.(fs.ReadDirFile); ok {
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if info.IsDir() {
			return dir, nil
		}
	}
	return &bbReadFile{File: file, owner: f, name: name}, nil
}

type bbReadFile struct {
	fs.File
	owner *bbReadFS
	name  string
	total int
}

func (f *bbReadFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	f.total += n
	return n, err
}

func (f *bbReadFile) Close() error {
	err := f.File.Close()
	select {
	case f.owner.reads <- bbRead{path: f.name, n: f.total}:
	default:
		panic("synthetic doctor read ledger exhausted")
	}
	return err
}

func TestBBDoctorDoesNotReadOversizedRecordsWhole(t *testing.T) {
	oversized := []byte(`{"sessionId":"sid","pid":101,"status":"idle","padding":"` + strings.Repeat("x", 2*claude.MaxRecordBytes) + `"}`)
	registry := bbReads(fstest.MapFS{"101.json": {Data: oversized}, "102.json": {Data: []byte(`{"sessionId":"small","pid":102,"status":"idle"}`)}})
	jobs := bbReads(fstest.MapFS{"short/state.json": {Data: oversized}})
	doctor.Run(doctor.Env{Root: ports.Root{Sessions: bbTree(registry, "sessions"), Jobs: bbTree(jobs, "jobs")}, Now: bbNow()})
	registryReads, jobReads := registry.maximums(), jobs.maximums()
	if registryReads["102.json"] == 0 {
		t.Fatal("positive control: doctor never read and closed the valid small record")
	}
	for label, reads := range map[string]map[string]int{"registry": registryReads, "jobs": jobReads} {
		for path, n := range reads {
			if n >= len(oversized) {
				t.Errorf("%s record %q read whole: %d bytes", label, path, n)
			}
		}
	}
}
