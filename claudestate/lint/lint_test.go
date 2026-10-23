package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// Every snippet is built from claude's constants rather than words typed here,
// so a rename on Claude's side moves this test with it.

func write(t *testing.T, dir, name, src string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func shapes(fs []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		out[f.Shape] = f.Literal
	}
	return out
}

// The H1 self-test's shape (§6): a temp .go with m["sessionId"] and a
// json:"waitingFor" struct tag gives exactly two findings.
func TestAMapIndexAndAStructTagAreTwoFindings(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "planted.go", fmt.Sprintf("package p\n\ntype rec struct {\n\tW string `json:%q`\n}\n\nfunc id(m map[string]any) any { return m[%q] }\n",
		claude.KeyWaitingFor, claude.KeySessionID))
	got, err := ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d findings, want 2: %v", len(got), got)
	}
	s := shapes(got)
	if s[ShapeIndex] != claude.KeySessionID || s[ShapeStructTag] != claude.KeyWaitingFor {
		t.Fatalf("%v", got)
	}
	if got[0].Pos.Line == 0 || !strings.HasSuffix(got[0].Pos.Filename, "planted.go") {
		t.Fatalf("a finding names no position: %+v", got[0])
	}
}

func TestEveryDecodeShapeIsCaughtAndNothingElse(t *testing.T) {
	cases := []struct {
		name, src string
		want      string // a shape, or "" for no finding
	}{
		{"compare", fmt.Sprintf("package p\nfunc f(raw string) bool { return raw == %q }\n", claude.ValueWaitingPermission), ShapeCompare},
		{"not-equal, parenthesised", fmt.Sprintf("package p\nfunc f(raw string) bool { return (%q) != raw }\n", claude.ValueStatusIdle), ShapeCompare},
		{"case", fmt.Sprintf("package p\nfunc f(raw string) int {\nswitch raw {\ncase %q:\nreturn 1\n}\nreturn 0\n}\n", claude.ValueStateBlocked), ShapeCase},
		{"index", fmt.Sprintf("package p\nfunc f(m map[string]int) int { return m[%q] }\n", claude.KeyStatusUpdatedAt), ShapeIndex},
		{"struct tag with options", fmt.Sprintf("package p\ntype r struct {\nP int `json:\"%s,omitempty\"`\n}\n", claude.KeyPID), ShapeStructTag},
		{"a homograph in a name table is the host's own word", fmt.Sprintf("package p\nvar names = []string{%q, %q}\n", claude.ValueStatusIdle, claude.ValueStatusBusy), ""},
		{"a vocabulary word passed to a function", fmt.Sprintf("package p\nfunc g(string) {}\nfunc f() { g(%q) }\n", claude.KeyStatus), ""},
		{"a string the vocabulary does not hold", "package p\nfunc f(raw string) bool { return raw == \"no such claude word\" }\n", ""},
	}
	for _, c := range cases {
		dir := t.TempDir()
		write(t, dir, "x.go", c.src)
		got, err := ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s: a false positive: %v", c.name, got)
		case c.want != "" && (len(got) != 1 || got[0].Shape != c.want):
			t.Errorf("%s: want one %s finding, got %v", c.name, c.want, got)
		}
	}
}

func TestTheWalkRecursesSkipsTestdataAndHonoursSkip(t *testing.T) {
	dir := t.TempDir()
	bad := fmt.Sprintf("package p\nfunc f(m map[string]int) int { return m[%q] }\n", claude.KeySessionID)
	write(t, dir, "a/b/deep.go", bad)
	write(t, dir, "testdata/ignored.go", bad)
	write(t, dir, ".hidden/ignored.go", bad)
	write(t, dir, "notgo.txt", bad)
	skipped := write(t, dir, "a/skip_me.go", bad)
	write(t, dir, "excluded/x.go", bad)
	got, err := ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), func(p string) bool {
		return p == skipped || filepath.Base(p) == "excluded"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].Pos.Filename, filepath.Join("a", "b", "deep.go")) {
		t.Fatalf("%v", got)
	}
}

func TestTheVocabularyIsTheDecodersOwn(t *testing.T) {
	lits := Literals(claude.Vocabulary())
	for _, k := range []string{claude.KeySessionID, claude.KeyWaitingFor, claude.KeyStatusUpdatedAt, claude.ValueWaitingInput,
		claude.ValueStateBlocked, claude.ValueKindBG, claude.KeyAgentColor} {
		if !lits[k] {
			t.Errorf("the vocabulary lacks %q", k)
		}
	}
	if _, err := ClaudeDecodeSites(nil, claude.Vocab{}, nil); err == nil {
		t.Fatal("an empty vocabulary must refuse, not report a clean tree")
	}
}

func TestAnUnparseableFileIsAnErrorNotACleanResult(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "broken.go", "package p\nfunc {\n")
	if _, err := ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), nil); err == nil {
		t.Fatal("a file that does not parse was passed as clean")
	}
}
