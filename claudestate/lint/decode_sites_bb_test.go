package lint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/lint"
)

func bbWriteGo(t *testing.T, dir, name, source string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBBClaudeDecodeSitesFindsEveryPublicShape(t *testing.T) {
	dir := t.TempDir()
	path := bbWriteGo(t, dir, "planted.go", "package planted\n"+
		"type record struct { ID string `json:\"sessionId\"` }\n"+
		"func decode(m map[string]string, value string) {\n"+
		" _ = m[\"sessionId\"]\n"+
		" _ = value == \"dialog open\"\n"+
		" switch value { case \"dialog open\": }\n"+
		"}\n")
	findings, err := lint.ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		line int
		word string
	}{
		"struct-tag": {2, "sessionId"},
		"index":      {4, "sessionId"},
		"compare":    {5, "dialog open"},
		"case":       {6, "dialog open"},
	}
	seen := make(map[string]int)
	for _, finding := range findings {
		expect, ok := want[finding.Shape]
		if !ok {
			t.Errorf("unexpected finding shape: %+v", finding)
			continue
		}
		seen[finding.Shape]++
		if filepath.Clean(finding.Pos.Filename) != path || finding.Pos.Line != expect.line || finding.Pos.Column <= 0 || !strings.Contains(finding.Literal, expect.word) {
			t.Errorf("finding lacks source location/literal evidence: %+v, want %s:%d and %q", finding, path, expect.line, expect.word)
		}
	}
	for shape := range want {
		if seen[shape] != 1 {
			t.Errorf("%s occurrences = %d, want 1; findings %+v", shape, seen[shape], findings)
		}
	}
}

func TestBBClaudeDecodeSitesIgnoresOrdinaryProseAndUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	bbWriteGo(t, dir, "clean.go", `package clean
// sessionId is documentation, not a decode site.
var documentation = "sessionId"
func ordinary(m map[string]string, value string) {
 _ = m["syntheticNonClaudeKey"]
 _ = value == "syntheticNonClaudeValue"
 switch value { case "syntheticNonClaudeValue": }
}
`)
	findings, err := lint.ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("ordinary prose/unrelated schema reported as Claude decode: %+v", findings)
	}
}

func TestBBClaudeDecodeSitesHonoursSkipAndInjectedVocabulary(t *testing.T) {
	dir := t.TempDir()
	skipped := bbWriteGo(t, dir, "skip.go", "package planted\nvar _ = map[string]int{}[\"sessionId\"]\n")
	kept := bbWriteGo(t, dir, "keep.go", "package planted\nvar _ = map[string]int{}[\"syntheticSchemaKey\"]\n")
	v := claude.Vocabulary()
	v.Registry.Universal = append(v.Registry.Universal, "syntheticSchemaKey")
	findings, err := lint.ClaudeDecodeSites([]string{dir}, v, func(path string) bool { return filepath.Clean(path) == skipped })
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || filepath.Clean(findings[0].Pos.Filename) != kept || findings[0].Shape != "index" || !strings.Contains(findings[0].Literal, "syntheticSchemaKey") {
		t.Errorf("skip or decoder-derived vocabulary ignored: %+v", findings)
	}
}

func TestBBClaudeDecodeSitesReportsInputErrors(t *testing.T) {
	dir := t.TempDir()
	bbWriteGo(t, dir, "invalid.go", "package broken\nfunc (\n")
	if _, err := lint.ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), nil); err == nil {
		t.Error("invalid Go source did not return a parse error")
	}
	if _, err := lint.ClaudeDecodeSites([]string{filepath.Join(t.TempDir(), "missing")}, claude.Vocabulary(), nil); err == nil {
		t.Error("missing input directory did not return an error")
	}
}
