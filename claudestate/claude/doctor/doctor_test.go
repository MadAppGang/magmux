package doctor

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

var now = time.Date(2026, 9, 18, 2, 40, 11, 204e6, time.UTC)

func corpusRoot(scenario string) ports.Root {
	tree := func(name string) ports.Tree {
		return ports.Tree{Path: "/testdata/" + name, FS: os.DirFS(filepath.Join("..", "..", "testdata", "roots", scenario, name))}
	}
	return ports.Root{Sessions: tree("sessions"), Jobs: tree("jobs"), Projects: tree("projects")}
}

func byCheck(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.Check] = r
	}
	return m
}

func TestThereAreTwentyChecksInFR11sOrder(t *testing.T) {
	want := []string{
		"registry-keys", "registry-status-values", "registry-waiting-for", "registry-tmux-shape",
		"registry-mtime-tracks-status", "registry-version-spread",
		"jobs-keys", "jobs-state-values", "jobs-dir-mtime-tracks-record", "jobs-updatedat-not-earlier-than-timeline",
		"jobs-timeline-shape", "jobs-linkscanpath", "jobs-signal-keys",
		"transcript-retention-floor", "transcript-deep-population", "transcript-untimestamped-types",
		"transcript-agent-color-shape", "transcript-api-error-key", "subagent-meta-files", "agents-cli-shape",
	}
	got := Checks()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", got)
	}
	rs := Run(Env{Root: corpusRoot("tt-corpus"), Now: now})
	if len(rs) != len(want) {
		t.Fatalf("%d results", len(rs))
	}
	for i, r := range rs {
		if r.Check != want[i] || r.Verdict == VerdictUnknown || r.Detail == "" {
			t.Errorf("result %d: %+v", i, r)
		}
	}
}

// With nothing to read, every check SKIPS: "nobody could look" is never a pass.
func TestNothingToReadIsASkipNeverAPass(t *testing.T) {
	for _, r := range Run(Env{Now: now}) {
		if r.Verdict != VerdictSkip {
			t.Errorf("%s: %v (%s)", r.Check, r.Verdict, r.Detail)
		}
	}
}

// Over tt's corpus the doctor reports what tt's own corpus holds: an unknown
// status and state, a waiting record with no reason, the job keys the scrub
// removed, and records that name no transcript. Nothing here depends on a file's
// mtime, which a git checkout sets.
func TestTheDoctorReadsTTsCorpus(t *testing.T) {
	m := byCheck(Run(Env{Root: corpusRoot("tt-corpus"), Now: now, Retention: 30 * 24 * time.Hour}))
	want := map[string]Verdict{
		CheckRegistryStatusValues:    VerdictWarn, // reticulating
		CheckRegistryWaitingFor:      VerdictWarn, // waiting-unknown-reason
		CheckRegistryTmuxShape:       VerdictOK,
		CheckRegistryKeys:            VerdictFail, // no-tmux-field and the background process carry no tmux
		CheckJobsKeys:                VerdictFail, // the never-read keys are not in this corpus at all
		CheckJobsStateValues:         VerdictFail, // reticulating
		CheckJobsLinkScanPath:        VerdictWarn,
		CheckJobsSignalKeys:          VerdictOK,
		CheckJobsTimelineShape:       VerdictWarn, // tt's timeline carries no text key: redacted
		CheckSubagentMetaFiles:       VerdictOK,
		CheckAgentsCLIShape:          VerdictSkip,
		CheckJobsUpdatedAtNotEarlier: VerdictOK,
	}
	for id, v := range want {
		if m[id].Verdict != v {
			t.Errorf("%s: %v (%s), want %v", id, m[id].Verdict, m[id].Detail, v)
		}
	}
	if !strings.Contains(m[CheckJobsKeys].Detail, claude.KeyIntent) {
		t.Errorf("jobs-keys names the missing keys: %s", m[CheckJobsKeys].Detail)
	}
}

// The retention check reads Env.Retention, else settings.json, else 30 days;
// and with no Claude root and no override it is UNKNOWN, never a silent 30.
func TestTheRetentionFloorAndItsSkip(t *testing.T) {
	projects := fstest.MapFS{
		"p/a.jsonl": {Data: []byte("{}\n"), ModTime: now.Add(-31 * 24 * time.Hour)},
		"p/b.jsonl": {Data: []byte("{}\n"), ModTime: now.Add(-time.Hour)},
	}
	root := ports.Root{Projects: ports.Tree{Path: "/p", FS: projects}}
	get := func(env Env) Result { return byCheck(Run(env))[CheckTranscriptRetentionFloor] }

	if r := get(Env{Root: root, Now: now}); r.Verdict != VerdictSkip || !strings.Contains(r.Detail, "unknown") {
		t.Fatalf("no settings and no override: %+v", r)
	}
	if r := get(Env{Root: root, Now: now, Retention: 30 * 24 * time.Hour}); r.Verdict != VerdictOK {
		t.Fatalf("a 31-day floor against 30 days: %+v", r)
	}
	if r := get(Env{Root: root, Now: now, Retention: 60 * 24 * time.Hour}); r.Verdict != VerdictWarn {
		t.Fatalf("a 31-day floor against 60 days: %+v", r)
	}
	settings := fstest.MapFS{claude.SettingsFile: {Data: []byte(`{"` + claude.KeyCleanupPeriodDays + `": 90}`)}}
	if r := get(Env{Root: root, Settings: settings, Now: now}); r.Verdict != VerdictWarn || !strings.Contains(r.Evidence[0], "90.00") {
		t.Fatalf("settings.json's 90 days: %+v", r)
	}
	if r := get(Env{Root: root, Settings: fstest.MapFS{}, Now: now}); r.Verdict != VerdictOK || !strings.Contains(r.Evidence[0], "default") {
		t.Fatalf("an absent key is the 30-day default: %+v", r)
	}
}

func TestRegistryMtimeTracksStatus(t *testing.T) {
	rec := func(ms int64) []byte {
		return []byte(`{"` + claude.KeyPID + `":1,"` + claude.KeySessionID + `":"s","` + claude.KeyStatusUpdatedAt + `":` + strconv.FormatInt(ms, 10) + `}`)
	}
	at := now.Add(-time.Hour)
	tracking := fstest.MapFS{"1.json": {Data: rec(at.UnixMilli()), ModTime: at.Add(time.Second)}}
	gone := fstest.MapFS{"1.json": {Data: rec(at.UnixMilli()), ModTime: at.Add(time.Hour)}}
	get := func(fsys fstest.MapFS) Verdict {
		return byCheck(Run(Env{Root: ports.Root{Sessions: ports.Tree{Path: "/s", FS: fsys}}, Now: now}))[CheckRegistryMtimeTracks].Verdict
	}
	if get(tracking) != VerdictOK || get(gone) != VerdictFail {
		t.Fatalf("tracking %v, gone %v", get(tracking), get(gone))
	}
}

func TestTheAgentsCheckDecodesTheGivenListing(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "agents", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	get := func(raw []byte) Result {
		return byCheck(Run(Env{Agents: &ports.Listing{Raw: raw, At: now}, Now: now}))[CheckAgentsCLIShape]
	}
	if r := get(read("real-39.json")); r.Verdict != VerdictOK || !strings.Contains(r.Evidence[0], "36 interactive, 3 background") {
		t.Fatalf("%+v", r)
	}
	if r := get(read("malformed.json")); r.Verdict != VerdictWarn {
		t.Fatalf("malformed: %+v", r)
	}
	if r := get(read("empty.json")); r.Verdict != VerdictWarn {
		t.Fatalf("empty: %+v", r)
	}
}

// The doctor reads only through the fs.FS values in Env: no non-test file
// imports os.
func TestTheDoctorCallsNoOSFunction(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		n++
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "os" || p == "os/exec" || p == "syscall" {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
	if n < 5 {
		t.Fatalf("scanned %d files", n)
	}
}
