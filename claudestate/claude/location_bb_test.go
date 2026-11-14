package claude_test

import (
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

// A location or meta-name count must work without any file content open.
type bbMetadataFS struct {
	data   fstest.MapFS
	opens  []string
	badDir string
}

func (f *bbMetadataFS) Open(name string) (fs.File, error) {
	f.opens = append(f.opens, name)
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}
func (f *bbMetadataFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(f.data, name) }
func (f *bbMetadataFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.badDir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrPermission}
	}
	return fs.ReadDir(f.data, name)
}

func TestBBProjectsRelIsLexicalAndConfined(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	for _, tt := range []struct {
		name, declared, want string
		ok                   bool
	}{
		{"inside", filepath.Join(root, "project-a", "s.jsonl"), "project-a/s.jsonl", true},
		{"cleaned inside", filepath.Join(root, "project-a", "..", "project-b", "s.jsonl"), "project-b/s.jsonl", true},
		{"empty", "", "", false}, {"relative", "project-a/s.jsonl", "", false},
		{"root itself", root, "", false}, {"parent", filepath.Dir(root), "", false},
		{"sibling prefix", root + "-other/s.jsonl", "", false},
		{"escape", filepath.Join(root, "..", "outside", "s.jsonl"), "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := claude.ProjectsRel(root, tt.declared)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ProjectsRel = (%q, %v); want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestBBLocateTranscriptUsesMetadataAndNonrecursiveIDLookup(t *testing.T) {
	f := &bbMetadataFS{data: fstest.MapFS{
		"project-a/s.jsonl":                  {Data: []byte("must not be read")},
		"project-a/s/subagents/agent.jsonl":  {},
		"project-b/deeper/hidden.jsonl":      {},
		"project-b/s/subagents/hidden.jsonl": {},
	}}
	got, err := claude.LocateTranscript(f, claude.LocateRequest{SessionID: "s"})
	if err != nil || !reflect.DeepEqual(got, model.Location{Path: "project-a/s.jsonl", Matches: 1}) {
		t.Fatalf("unique location = %+v, %v", got, err)
	}
	for _, id := range []string{"hidden", "missing", "../s", "*", "s[ab]", "s/other"} {
		got, err := claude.LocateTranscript(f, claude.LocateRequest{SessionID: id})
		if err != nil || got.Matches != 0 || got.Path != "" {
			t.Errorf("id %q = %+v, %v; want no matches", id, got, err)
		}
	}
	if len(f.opens) != 0 {
		t.Errorf("location opened content: %v", f.opens)
	}
}

func TestBBLocateTranscriptAmbiguityIsReevaluated(t *testing.T) {
	f := &bbMetadataFS{data: fstest.MapFS{"project-a/s.jsonl": {}}}
	got, err := claude.LocateTranscript(f, claude.LocateRequest{SessionID: "s"})
	if err != nil || got.Path != "project-a/s.jsonl" || got.Matches != 1 {
		t.Fatalf("initial location = %+v, %v", got, err)
	}
	// Keep the old match: a previously unique hit must not be reused blindly.
	f.data["project-b/s.jsonl"] = &fstest.MapFile{}
	got, err = claude.LocateTranscript(f, claude.LocateRequest{SessionID: "s"})
	if err != nil || got.Path != "" || got.Matches != 2 || got.Declared {
		t.Errorf("second match = %+v, %v; want ambiguous", got, err)
	}
	if len(f.opens) != 0 {
		t.Errorf("ambiguous lookup opened content: %v", f.opens)
	}
}

func TestBBDeclaredTranscriptNeverFallsBack(t *testing.T) {
	f := &bbMetadataFS{data: fstest.MapFS{"project-a/s.jsonl": {}, "project-b/s.jsonl": {}}}
	got, err := claude.LocateTranscript(f, claude.LocateRequest{SessionID: "s", DeclaredPath: "project-b/s.jsonl"})
	if err != nil || !reflect.DeepEqual(got, model.Location{Path: "project-b/s.jsonl", Matches: 1, Declared: true}) {
		t.Fatalf("declared path = %+v, %v", got, err)
	}
	got, err = claude.LocateTranscript(f, claude.LocateRequest{SessionID: "s", DeclaredPath: "missing/s.jsonl"})
	if err != nil || !reflect.DeepEqual(got, model.Location{Declared: true}) {
		t.Errorf("missing declared path fell back: %+v, %v", got, err)
	}
	got, err = claude.LocateTranscript(f, claude.LocateRequest{SessionID: "s", DeclaredID: true})
	if err != nil || got.Matches != 2 || got.Path != "" || !got.Declared {
		t.Errorf("declared id ambiguity = %+v, %v", got, err)
	}
	if len(f.opens) != 0 {
		t.Errorf("declared lookup opened content: %v", f.opens)
	}
	for _, tt := range []struct {
		path, id string
		bad      bool
	}{
		{"project-a/s.jsonl", "s", false}, {"project-a/other.jsonl", "s", true},
		{"", "s", false}, {"project-a/s.jsonl", "", false},
	} {
		if err := claude.CheckDeclared(tt.path, tt.id); (err != nil) != tt.bad {
			t.Errorf("CheckDeclared(%q, %q) = %v; bad=%v", tt.path, tt.id, err, tt.bad)
		}
	}
}

// The contract names fs.Glob, which ignores directory-read errors, and says only
// that err != nil requires a filesystem error other than not-exist (a necessary
// condition, not a promise to report every one). So an unreadable projects root is
// NOT asserted to surface as an error. What is asserted: nothing is opened, no
// match is invented, and any error that is returned is not a not-exist error.
func TestBBLocateTranscriptOnAnUnreadableRootOpensNothingAndInventsNoMatch(t *testing.T) {
	f := &bbMetadataFS{data: fstest.MapFS{"project-a/s.jsonl": {Data: []byte("must not be read")}}, badDir: "."}
	got, err := claude.LocateTranscript(f, claude.LocateRequest{SessionID: "s"})
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lookup error = %v; not-exist must never be an error", err)
	}
	if got.Matches != 0 || got.Path != "" || got.Declared {
		t.Errorf("unreadable root = %+v, %v; want no match", got, err)
	}
	if len(f.opens) != 0 {
		t.Errorf("lookup on an unreadable root opened content: %v", f.opens)
	}
}

func bbInt(v int) *int { return &v }

func TestBBSubagentEvidenceDistinguishesAbsenceLocationAndAmbiguity(t *testing.T) {
	loc := model.Location{Path: "project-a/s.jsonl", Matches: 1}
	for _, tt := range []struct {
		name     string
		loc      model.Location
		data     fstest.MapFS
		badDir   string
		want     *int
		evidence model.Evidence
	}{
		{"located without directory", loc, fstest.MapFS{"project-a/s.jsonl": {}}, "", bbInt(0), model.EvidenceAbsent},
		{"not located", model.Location{}, fstest.MapFS{}, "", nil, model.EvidenceNoTranscript},
		{"ambiguous", model.Location{Matches: 2}, fstest.MapFS{}, "", nil, model.EvidenceAmbiguous},
		{"unreadable", loc, fstest.MapFS{}, "project-a/s/subagents", nil, model.EvidenceUnreadable},
		{"count names only", loc, fstest.MapFS{
			"project-a/s.jsonl":                        {},
			"project-a/s/subagents/a.meta.json":        {Data: []byte("not JSON and must not be read")},
			"project-a/s/subagents/b.meta.json":        {Data: []byte("{}")},
			"project-a/s/subagents/agent.jsonl":        {},
			"project-a/s/subagents/a.meta.json.tmp":    {},
			"project-a/s/subagents/deeper/c.meta.json": {},
		}, "", bbInt(2), model.EvidenceCounted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &bbMetadataFS{data: tt.data, badDir: tt.badDir}
			got, evidence := claude.CountSubagents(f, tt.loc)
			if !reflect.DeepEqual(got, tt.want) || evidence != tt.evidence {
				t.Errorf("CountSubagents = (%v, %v); want (%v, %v)", got, evidence, tt.want, tt.evidence)
			}
			if len(f.opens) != 0 {
				t.Errorf("meta contents were opened: %v", f.opens)
			}
		})
	}
	if got := claude.SubagentsDir(loc); got != "project-a/s/subagents" {
		t.Errorf("SubagentsDir = %q", got)
	}
	for _, loc := range []model.Location{{}, {Matches: 2}} {
		if got := claude.SubagentsDir(loc); got != "" {
			t.Errorf("unlocated/ambiguous SubagentsDir = %q", got)
		}
	}
}
