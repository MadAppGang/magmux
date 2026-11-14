package claude

// G1: ONE DECODER (FR12, NFR6), widened from magmux's
// TestOnlySchemaFileDecodesClaudeStrings (S:internal/obs/adapters/schema_test.go)
// to the whole module.
//
// The inventory is every string constant in schema.go, read out of its AST, so
// it cannot drift from the decoders. It splits in two, and the split is
// DERIVED rather than listed:
//
//   - A word (lower-case letters only: "status", "idle", "kind") is also
//     ordinary English and the library's own vocabulary; model's enum words
//     are homographs of Claude's values. A word is banned only in DECODE
//     position: compared against, switched on, used as a map index, or named
//     by a struct tag.
//   - Anything else ("statusUpdatedAt", "input needed", "agent-color",
//     ".jsonl") could only have come from Claude. In a non-test file it is
//     banned in any position. In a test file it is banned in decode position:
//     a test builds Claude records as its INPUT on purpose, and that is not a
//     decoder.
//
// Two things are not walked: the one cited directory entry in decoderAllowlist,
// and files ending _bb_test.go (blackBoxSuffix, cited there), whose literal
// Claude words are a blind test's independent oracle.
//
// What it does not catch, so nobody mistakes it for a guarantee: a literal
// split in two, and a word passed to a function. Comparison, switch, index and
// tag are the shapes a second decoder actually gets written in.
//
// Two further rules hold claude/ itself pure (§2.3, M15): no non-test file
// imports os or os/exec, and none calls an Open or ReadFile. It decodes bytes
// and readers it is handed, and touches an fs.FS through fs.Stat, fs.ReadDir
// and fs.Glob (which reads directories) only.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// decoderAllowlist: directory prefix (module-relative) -> citation. ONE entry,
// by design (§6 G1). Neither directory exists before L6; the entry is checked
// for its citation, not for existence, because the packages it exempts land
// later in the same plan.
var decoderAllowlist = map[string]string{
	"internal/difftest+_diff": "architecture §6 G1: they decode tt's WIRE, not Claude's files; their one touch of " +
		"a Claude file, the _diff sandbox's linkScanPath prefix rewrite, is a byte-prefix substitution with no key literal (§8.2)",
}

func allowlisted(rel string) bool {
	for key := range decoderAllowlist {
		for _, dir := range strings.Split(key, "+") {
			if rel == dir || strings.HasPrefix(rel, dir+"/") {
				return true
			}
		}
	}
	return false
}

// blackBoxSuffix names the ONE file-level exemption beside the allowlist.
//
// A *_bb_test.go file is a blind black-box test (Phase 6): written by an
// external writer from the public contract alone, without seeing schema.go or
// any other implementation file. Its literal Claude words ("waiting",
// "permission prompt", "timestamp") are the INDEPENDENT ORACLE. Re-spelling
// them through schema.go's constants would make the test agree with the
// decoder by construction — a wrong constant would then be checked against
// itself and pass — which is exactly what a black-box test exists to catch.
// The exemption is by suffix, so it covers test files only (a non-test file
// can never end in _test.go) and nothing else in the walk changes.
const blackBoxSuffix = "_bb_test.go"

const blackBoxCitation = "Phase 6 black-box tests: blind, written from the contract only; their literal " +
	"Claude words are the independent oracle, and spelling them through schema.go would pass with a wrong constant"

func blackBoxExempt(rel string) bool { return strings.HasSuffix(rel, blackBoxSuffix) }

const schemaFile = "claude/schema.go"

// inventory reads every string constant out of schema.go.
func inventory(t *testing.T, root string) (claudeOnly, words map[string]bool) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, schemaFile), nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", schemaFile, err)
	}
	claudeOnly, words = map[string]bool{}, map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, v := range vs.Values {
			s, ok := stringLiteral(v)
			if !ok || s == "" {
				continue
			}
			if isWord(s) {
				words[s] = true
			} else {
				claudeOnly[s] = true
			}
		}
		return true
	})
	return claudeOnly, words
}

func isWord(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// site is one flagged position.
type site struct {
	pos token.Pos
	lit string
	how string
}

// decodeSites walks one file. anyPosition: a Claude-only literal is flagged
// wherever it appears (non-test files).
func decodeSites(file *ast.File, claudeOnly, words map[string]bool, anyPosition bool) []site {
	var found []site
	seen := map[token.Pos]bool{}
	add := func(pos token.Pos, lit, how string) {
		if !seen[pos] {
			seen[pos] = true
			found = append(found, site{pos, lit, how})
		}
	}
	decoded := func(e ast.Expr, how string) {
		if s, ok := stringLiteral(e); ok && (claudeOnly[s] || words[s]) {
			add(e.Pos(), s, how)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if v.Op == token.EQL || v.Op == token.NEQ {
				decoded(v.X, "compares a value against")
				decoded(v.Y, "compares a value against")
			}
		case *ast.CaseClause:
			for _, e := range v.List {
				decoded(e, "switches on")
			}
		case *ast.IndexExpr:
			decoded(v.Index, "indexes a map by")
		case *ast.Field:
			if v.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(v.Tag.Value)
			if err != nil {
				return true
			}
			name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
			if claudeOnly[name] || words[name] {
				add(v.Tag.Pos(), name, "carries a struct tag decoding")
			}
		case *ast.BasicLit:
			if !anyPosition {
				return true
			}
			if s, ok := stringLiteral(v); ok && claudeOnly[s] {
				add(v.Pos(), s, "names")
			}
		}
		return true
	})
	return found
}

// purityFindings is the claude/-only rule: no os, no exec, no Open/ReadFile.
func purityFindings(file *ast.File) []string {
	var out []string
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p == "os" || p == "os/exec" || p == "syscall" {
			out = append(out, "imports "+p)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "Open", "OpenFile", "ReadFile":
				out = append(out, "calls "+sel.Sel.Name+": claude opens nothing")
			}
		}
		return true
	})
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			if !bytes.HasPrefix(b, []byte("module github.com/MadAppGang/magmux/claudestate\n")) {
				t.Fatalf("%s is not the claudestate module", dir)
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

func TestOnlySchemaFileDecodesClaudeStrings(t *testing.T) {
	root := moduleRoot(t)
	claudeOnly, words := inventory(t, root)
	if len(claudeOnly) < 40 || len(words) < 40 {
		t.Fatalf("the inventory is %d Claude-only literals and %d words; schema.go is not being read", len(claudeOnly), len(words))
	}
	var scanned, schema, claudeFiles, blackBox int
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			if name == "testdata" || name == "vendor" || name == "_probe" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".go" {
			return nil
		}
		if rel == schemaFile {
			schema++
			return nil
		}
		if allowlisted(rel) {
			return nil
		}
		if blackBoxExempt(rel) {
			blackBox++
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		isTest := strings.HasSuffix(rel, "_test.go")
		for _, s := range decodeSites(file, claudeOnly, words, !isTest) {
			t.Errorf("%s: %s %q, which is Claude Code's. Name the constant in %s.", fset.Position(s.pos), s.how, s.lit, schemaFile)
		}
		if strings.HasPrefix(rel, "claude/") && !strings.Contains(rel[len("claude/"):], "/") && !isTest {
			claudeFiles++
			for _, f := range purityFindings(file) {
				t.Errorf("%s: %s", rel, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if schema != 1 {
		t.Errorf("found %d %s files, want exactly 1", schema, schemaFile)
	}
	if blackBox == 0 {
		t.Errorf("no %s file exists, so the black-box exemption is stale: delete it", blackBoxSuffix)
	}
	if scanned < 30 || claudeFiles < 8 {
		t.Errorf("scanned %d files (%d in claude/): the walk is not reaching the module", scanned, claudeFiles)
	}
}

func TestTheDecoderAllowlistIsOneCitedEntry(t *testing.T) {
	if len(decoderAllowlist) != 1 {
		t.Fatalf("the decoder allowlist has %d entries; §6 G1 allows exactly one", len(decoderAllowlist))
	}
	for k, why := range decoderAllowlist {
		if k != "internal/difftest+_diff" || !strings.Contains(why, "§6 G1") {
			t.Fatalf("the one entry must be internal/difftest+_diff with its citation: %q -> %q", k, why)
		}
	}
	if allowlisted("claude/doctor/doctor.go") || allowlisted("internal/difftestx/a.go") {
		t.Fatal("the allowlist matches more than its two directories")
	}
	// The black-box suffix is the one exemption outside the map, held to the
	// same terms: cited, and no wider than its name. Changing either is a
	// deliberate edit to this test.
	if blackBoxSuffix != "_bb_test.go" || !strings.Contains(blackBoxCitation, "Phase 6") ||
		!strings.Contains(blackBoxCitation, "independent oracle") {
		t.Fatalf("the black-box exemption must be _bb_test.go with its citation: %q -> %q", blackBoxSuffix, blackBoxCitation)
	}
	for rel, want := range map[string]bool{
		"claude/records_bb_test.go":         true,
		"pipeline/observations_bb_test.go":  true,
		"claude/records_test.go":            false, // an ordinary test
		"claude/records_bb.go":              false, // not a test file
		"claude/bb_test.go":                 false, // no _bb_ suffix
		"claude/records_bb_test.go.orig":    false,
		"claude/records_bb_test_helpers.go": false,
	} {
		if got := blackBoxExempt(rel); got != want {
			t.Errorf("blackBoxExempt(%q) = %v, want %v", rel, got, want)
		}
	}
}

// A planted decode in a file that is NOT a _bb_ test is still caught: the
// exemption is decided on the file name before parsing, so the detector itself
// is untouched, and an ordinary test decoding by Claude's key is a finding.
func TestTheBlackBoxExemptionLeavesOrdinaryTestsGuarded(t *testing.T) {
	claudeOnly, words := inventory(t, moduleRoot(t))
	src := fmt.Sprintf("package p\nfunc f(raw string) bool { return raw == %q }\n", ValueWaitingPermission)
	for _, rel := range []string{"claude/zz_test.go", "pipeline/zz_bbtest_test.go", "join/zz_bb.go"} {
		if blackBoxExempt(rel) {
			t.Fatalf("%s is exempt; only *%s is", rel, blackBoxSuffix)
		}
		file, err := parser.ParseFile(token.NewFileSet(), rel, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeSites(file, claudeOnly, words, !strings.HasSuffix(rel, "_test.go")); len(got) != 1 {
			t.Errorf("%s: %d findings for a planted comparison against %q, want 1", rel, len(got), ValueWaitingPermission)
		}
	}
}

// The detector is tested against source, which is the only way to know it
// catches anything: the walk above passes on a clean tree whether the detector
// works or is a stub. Every snippet is built from schema.go's constants.
func TestTheDecodeDetectorCatchesTheShapesItMustCatch(t *testing.T) {
	claudeOnly, words := inventory(t, moduleRoot(t))
	cases := []struct {
		name string
		src  string
		test bool // parse as a test file
		want bool
	}{
		{"a raw value compared against Claude's status", "package p\nfunc f(raw string) bool { return raw == %[1]q }\n", false, true},
		{"switching on Claude's waiting reason", "package p\nfunc f(raw string) int {\nswitch raw {\ncase %[2]q:\nreturn 1\n}\nreturn 0\n}\n", false, true},
		{"a struct tag naming Claude's key", "package p\ntype r struct {\nAt int64 `json:%[3]q`\n}\n", false, true},
		{"a Claude-only key as a bare literal", "package p\nvar keys = []string{%[4]q}\n", false, true},
		{"a map read by Claude's key", "package p\nfunc f(m map[string]any) any { return m[%[5]q] }\n", false, true},
		{"the planted status literal in claude/doctor", "package doctor\nfunc f(m map[string]any) any { return m[%[7]q] }\n", false, true},
		{"a word in a name table", "package p\nvar names = []string{%[1]q, %[6]q}\n", false, false},
		{"a comment mentioning Claude's value", "package p\n// Claude writes %[1]s here.\nvar x int\n", false, false},
		{"a word written out, not read in", "package p\nfunc name() string { return %[6]q }\n", false, false},
		{"a test building a record as input", "package p\nvar rec = map[string]any{%[3]q: 1}\n", true, false},
		{"a test decoding by a Claude-only key", "package p\nfunc f(m map[string]any) any { return m[%[3]q] }\n", true, true},
	}
	for _, c := range cases {
		src := fmt.Sprintf(c.src, ValueStatusWaiting, ValueWaitingPermission, KeyStatusUpdatedAt,
			KeyAgentColor, KeySessionID, ValueStatusIdle, KeyStatus)
		file, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, src)
		}
		if got := len(decodeSites(file, claudeOnly, words, !c.test)) > 0; got != c.want {
			t.Errorf("%s: violation = %v, want %v, in:\n%s", c.name, got, c.want, src)
		}
	}
}

func TestThePurityRuleCatchesAnOpen(t *testing.T) {
	planted := "package claude\nimport (\n\"io/fs\"\n\"os\"\n)\nfunc f(fsys fs.FS) { _, _ = fs.ReadFile(fsys, \"x\"); _, _ = fsys.Open(\"y\"); _ = os.Getpid }\n"
	file, err := parser.ParseFile(token.NewFileSet(), "zz.go", planted, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := purityFindings(file); len(got) != 3 {
		t.Fatalf("want 3 findings (os import, fs.ReadFile, Open), got %v", got)
	}
	clean := "package claude\nimport \"io/fs\"\nfunc f(fsys fs.FS) { _, _ = fs.ReadDir(fsys, \".\"); _, _ = fs.Stat(fsys, \"x\") }\n"
	file, _ = parser.ParseFile(token.NewFileSet(), "zz.go", clean, 0)
	if got := purityFindings(file); len(got) != 0 {
		t.Fatalf("metadata reads were reported: %v", got)
	}
}
