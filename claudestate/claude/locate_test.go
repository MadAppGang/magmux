package claude

import (
	"io/fs"
	"os"
	"testing"
	"testing/fstest"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// tt's containment cases (tt:transcriptfs_test.go), plus the escape the corpus
// carries.
func TestProjectsRelIsTTsLexicalContainment(t *testing.T) {
	const root = "/testdata/projects"
	cases := []struct {
		in   string
		rel  string
		want bool
	}{
		{"/testdata/projects/-Users-user-dotfiles/a.jsonl", "-Users-user-dotfiles/a.jsonl", true},
		{"/testdata/projects/x/../y/a.jsonl", "y/a.jsonl", true},
		{"/testdata/projects/../../../etc/passwd", "", false},
		{"/testdata/projects", "", false},
		{"/testdata/projects-evil/x.jsonl", "", false},
		{"/testdata", "", false},
		{"relative/x.jsonl", "", false},
		{"", "", false},
		{"   ", "", false},
	}
	for _, c := range cases {
		rel, ok := ProjectsRel(root, c.in)
		if ok != c.want || rel != c.rel {
			t.Errorf("ProjectsRel(%q) = %q, %v; want %q, %v", c.in, rel, ok, c.rel, c.want)
		}
	}
	if _, ok := ProjectsRel("", "/a/b"); ok {
		t.Error("an empty projects path contains nothing")
	}
}

func TestLocateByIDFindsOneAmbiguousTwoAndMissNone(t *testing.T) {
	mm := &openCounter{FS: os.DirFS(rootDir("magmux-corpus", "projects"))}
	loc, err := LocateTranscript(mm, LocateRequest{SessionID: "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b"})
	if err != nil || loc != (model.Location{Path: "-Users-user-dev-magmux/a1478d55-6e02-4bb9-93cf-01d2e8f47a6b.jsonl", Matches: 1}) {
		t.Fatalf("%+v %v", loc, err)
	}
	loc, _ = LocateTranscript(mm, LocateRequest{SessionID: "6c5b2f09-3a81-4d76-99e4-0f8c1b3a7d52"})
	if loc != (model.Location{}) {
		t.Fatalf("1828 has no transcript anywhere: %+v", loc)
	}
	tt := &openCounter{FS: os.DirFS(rootDir("tt-corpus", "projects"))}
	loc, _ = LocateTranscript(tt, LocateRequest{SessionID: "d0b1e000-0000-4000-8000-000000000002"})
	if loc.Matches != 2 || loc.Path != "" {
		t.Fatalf("D5: one id in two directories is ambiguous: %+v", loc)
	}
	if len(mm.opens)+len(tt.opens) != 0 {
		t.Fatalf("locating opened files: %v %v", mm.opens, tt.opens)
	}
}

// Location is by session id, so a directory holding another session's
// transcript is never read (the wrong-directory trap, re-expressed).
func TestLocatingByIDNeverLandsInAnotherSessionsDirectory(t *testing.T) {
	fsys := os.DirFS(rootDir("agent-color", "projects"))
	loc, _ := LocateTranscript(fsys, LocateRequest{SessionID: "c0100003-0000-4000-8000-000000000003"})
	if loc.Path != "-Users-user-dev-colour/c0100003-0000-4000-8000-000000000003.jsonl" {
		t.Fatalf("%+v", loc)
	}
}

// An id tt would not glob is never globbed: empty, too long, or carrying any
// byte outside [0-9A-Za-z_-] (a metacharacter would match a neighbour's file).
func TestAnIDOutsideTTsAlphabetIsNeverGlobbed(t *testing.T) {
	fsys := fstest.MapFS{
		"p/abc.jsonl": {Data: []byte("{}\n")},
		"p/a*c.jsonl": {Data: []byte("{}\n")},
	}
	for _, id := range []string{"", "a*c", "a?c", "../p/abc", "a/b", "a c", string(make([]byte, 129))} {
		loc, err := LocateTranscript(fsys, LocateRequest{SessionID: id})
		if err != nil || loc != (model.Location{}) {
			t.Errorf("%q: %+v %v", id, loc, err)
		}
	}
	if loc, _ := LocateTranscript(fsys, LocateRequest{SessionID: "abc"}); loc.Matches != 1 {
		t.Fatalf("a valid id: %+v", loc)
	}
}

// The glob is non-recursive: a subagent transcript at depth 3 is unreachable,
// and a DIRECTORY named like a transcript is not one.
func TestTheGlobIsNonRecursiveAndFilesOnly(t *testing.T) {
	fsys := fstest.MapFS{
		"p/s1/subagents/s2.jsonl": {Data: []byte("{}\n")},
		"q/s3.jsonl/x":            {Data: []byte("{}\n")},
	}
	for _, id := range []string{"s2", "s3"} {
		if loc, _ := LocateTranscript(fsys, LocateRequest{SessionID: id}); loc.Matches != 0 {
			t.Errorf("%s: %+v", id, loc)
		}
	}
	if loc, err := LocateTranscript(os.DirFS(t.TempDir()+"/absent"), LocateRequest{SessionID: "x"}); err != nil || loc.Matches != 0 {
		t.Fatalf("no projects tree is a miss, not an error: %+v %v", loc, err)
	}
}

// A declared path is addressed directly and NEVER falls back to the glob.
func TestADeclaredPathNeverFallsBack(t *testing.T) {
	fsys := fstest.MapFS{"p/s.jsonl": {Data: []byte("{}\n")}, "q/s.jsonl": {Data: []byte("{}\n")}}
	loc, _ := LocateTranscript(fsys, LocateRequest{SessionID: "s", DeclaredPath: "p/s.jsonl"})
	if loc != (model.Location{Path: "p/s.jsonl", Matches: 1, Declared: true}) {
		t.Fatalf("%+v", loc)
	}
	loc, _ = LocateTranscript(fsys, LocateRequest{SessionID: "s", DeclaredPath: "r/s.jsonl"})
	if loc != (model.Location{Declared: true}) {
		t.Fatalf("an absent declared path is a miss, not a glob: %+v", loc)
	}
	loc, _ = LocateTranscript(fsys, LocateRequest{SessionID: "s", DeclaredID: true})
	if loc.Matches != 2 || !loc.Declared {
		t.Fatalf("a declared id is located by id: %+v", loc)
	}
	if loc, _ := LocateTranscript(fsys, LocateRequest{DeclaredPath: "../p/s.jsonl"}); loc.Matches != 0 {
		t.Fatalf("an escaping declared path: %+v", loc)
	}
}

func TestCheckDeclaredIsMainsBindingRule(t *testing.T) {
	if CheckDeclared("/x/y/abc.jsonl", "abc") != nil || CheckDeclared("", "abc") != nil || CheckDeclared("/x/a.jsonl", "") != nil {
		t.Fatal("a consistent or partial declaration was refused")
	}
	if CheckDeclared("/x/y/other.jsonl", "abc") == nil {
		t.Fatal("a mismatched declaration was accepted")
	}
}

// D1, C7: counted by NAME from the files, never from the transcript, and a
// missing directory is a determined zero only when the transcript was found.
func TestSubagentsAreCountedFromMetaFileNames(t *testing.T) {
	fsys := &openCounter{FS: os.DirFS(rootDir("magmux-corpus", "projects"))}
	cases := []struct {
		id   string
		n    int
		ev   model.Evidence
		null bool
	}{
		{"5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3", 23, model.EvidenceCounted, false},
		{"c874de20-33c1-480a-ba90-8ae3f2f332c4", 0, model.EvidenceCounted, false},
		{"a1478d55-6e02-4bb9-93cf-01d2e8f47a6b", 3, model.EvidenceCounted, false},
		{"3b90f6cc-1d47-4e20-8a3b-9f5e6c0d2a18", 0, model.EvidenceAbsent, false},
		{"6c5b2f09-3a81-4d76-99e4-0f8c1b3a7d52", 0, model.EvidenceNoTranscript, true},
	}
	for _, c := range cases {
		loc, _ := LocateTranscript(fsys, LocateRequest{SessionID: c.id})
		n, ev := CountSubagents(fsys, loc)
		if ev != c.ev || (n == nil) != c.null || (n != nil && *n != c.n) {
			t.Errorf("%s: %v %v, want %d %v", c.id, n, ev, c.n, c.ev)
		}
	}
	if n, ev := CountSubagents(fsys, model.Location{Matches: 2}); n != nil || ev != model.EvidenceAmbiguous {
		t.Errorf("ambiguous: %v %v", n, ev)
	}
	if len(fsys.opens) != 0 {
		t.Fatalf("a meta file was opened: %v", fsys.opens)
	}
	tt := os.DirFS(rootDir("tt-corpus", "projects"))
	loc, _ := LocateTranscript(tt, LocateRequest{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"})
	if n, ev := CountSubagents(tt, loc); n == nil || *n != 3 || ev != model.EvidenceCounted {
		t.Fatalf("tt's three meta files beside two agent transcripts: %v %v", n, ev)
	}
}

func TestAnUnreadableSubagentsDirectoryIsUndetermined(t *testing.T) {
	fsys := failingReadDir{fstest.MapFS{"p/s.jsonl": {Data: []byte("{}\n")}}}
	n, ev := CountSubagents(fsys, model.Location{Path: "p/s.jsonl", Matches: 1})
	if n != nil || ev != model.EvidenceUnreadable {
		t.Fatalf("%v %v", n, ev)
	}
	if got := SubagentsDir(model.Location{Path: "p/s.jsonl", Matches: 1}); got != "p/s/subagents" {
		t.Fatalf("under the SESSION ID, not beside it: %q", got)
	}
}

type failingReadDir struct{ fstest.MapFS }

func (failingReadDir) ReadDir(string) ([]fs.DirEntry, error) { return nil, fs.ErrPermission }
