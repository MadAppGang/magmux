package mux

// Two source guards over magmux's side of the observation layer.
//
//   - H1: no Claude literal outside the library. Claude Code's formats are
//     decoded in claudestate/claude and nowhere else; magmux reaches them
//     through its decoders (claude.DecodeLine, DecodeRecord, ParseTurns,
//     EncodeProjectDir, ...). This replaced the schema guard that lived in
//     internal/obs/adapters, in the commit that deleted that package, so there
//     was no commit with neither.
//   - H5: no ticker in magmux's observation code. The library has its own
//     (G3); this one covers what magmux added beside it.
//
// Both read the source, because neither property can be shown by behaviour: a
// second decoder answers correctly until Claude renames a key, and a ticker
// produces the same events as a one-shot timer until the machine is idle.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/lint"
)

// claudeLiteralScope is every magmux directory H1 reads, from this package's
// directory: the terminal core, the act/navigate/notify packages, the wire,
// and the two processes that speak to magmux over its socket.
var claudeLiteralScope = []string{".", "../internal/obs", "../obsview", "../mcp", "../client"}

// claudeLiterals splits the library's vocabulary the way a HOST must read it.
//
// lint.ClaudeDecodeSites flags every vocabulary literal in decode position,
// and the vocabulary is the decoders' whole table — which includes "type",
// "id", "status", "pid", "text" and twenty more plain English words that are
// also the keys of magmux's OWN protocols (the socket schema, MCP's JSON-RPC,
// the audit trail, the wire). Run raw over magmux it reports ninety sites,
// every one of them magmux decoding its own JSON. A guard that needs ninety
// exceptions guards nothing, so the split is made here, by the shape of the
// literal, and said out loud:
//
//   - claudeOnly: a literal that is not a plain lower-case word —
//     "sessionId", "waitingFor", "input needed", "tool_use_id", "ai-title".
//     Nobody else spells these, so ANY decode position is a second decoder.
//   - values: the plain lower-case words that are Claude's VALUES — the
//     registry's status words, the job states and tempos, the kinds. Comparing
//     against one or switching on one is decoding a status, so the compare and
//     case shapes are flagged; an index or a tag of one is not.
//
// What this does NOT catch, stated so nobody mistakes it for a guarantee: a
// plain lower-case Claude KEY ("status", "cwd", "name") used to index a
// record. Those words are magmux's own keys too, everywhere, and telling the
// two apart needs types, not source. The library's G1 holds that line inside
// claudestate; here it is the code review's.
func claudeLiterals(t *testing.T) (claudeOnly, values map[string]bool) {
	t.Helper()
	v := claude.Vocabulary()
	claudeOnly = map[string]bool{}
	for lit := range lint.Literals(v) {
		if !plainWord(lit) {
			claudeOnly[lit] = true
		}
	}
	values = map[string]bool{}
	for _, list := range [][]string{v.RegistryStatus, v.WaitingFor, v.JobState, v.Tempo, v.Kind} {
		for _, w := range list {
			if plainWord(w) {
				values[w] = true
			}
		}
	}
	if len(claudeOnly) < 20 || len(values) < 8 {
		t.Fatalf("the vocabulary split found %d Claude-only literals and %d values; the library's "+
			"Vocabulary() is not what this guard was written against", len(claudeOnly), len(values))
	}
	return claudeOnly, values
}

// plainWord is a literal made of lower-case ASCII letters only.
func plainWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// claudeSites is H1's detector over parsed files: lint.Sites with the host's
// split.
func claudeSites(fset *token.FileSet, f *ast.File, claudeOnly, values map[string]bool) []lint.Finding {
	var out []lint.Finding
	for _, s := range lint.Sites(fset, f, claudeOnly) {
		out = append(out, s)
	}
	for _, s := range lint.Sites(fset, f, values) {
		if s.Shape == lint.ShapeCompare || s.Shape == lint.ShapeCase {
			out = append(out, s)
		}
	}
	return out
}

// magmuxOwnWords are the sites the detector flags that are magmux's OWN
// vocabulary rather than a reading of Claude's — homographs — keyed by the
// file (from the module root) and the literal. Each carries its reason, and an
// entry that no longer matches anything is an error, so the list cannot
// outlive what it excuses.
var magmuxOwnWords = map[string]string{
	"mux/grid.go:sessionId": "the `sessionId` member of magmux's own results/snapshot pane entry — magmux's socket " +
		"schema, spelled the same as Claude's key",
	"mux/grid.go:startedAt": "the `startedAt` member of the same pane entry: magmux's socket schema",
	"mux/control.go:working": "a control-panel route's state and the controller state word, magmux's own " +
		"controller vocabulary (ctrlRoute.state, CtrlWorking)",
	"mux/control.go:failed":            "the panel's step-outcome word, magmux's own",
	"mux/control.go:idle":              "a control-panel route's state, magmux's own controller vocabulary",
	"mux/controller_plugin.go:working": "a plugin controller's reported state, in magmux's controller vocabulary",
	"mux/socket.go:working":            "the status word of magmux's own `agent` hook verb, which a hook script sends",
	"mux/socket.go:idle":               "the status word of magmux's own `agent` hook verb, which a hook script sends",
	"mux/obssock.go:failed": "a pane's `state` in magmux's own connect-time aggregate (buildPaneResultsLocked), " +
		"read back to tell a reaped pane",
	"client/client.go:failed": "a pane's `state` in magmux's own socket results",
}

// TestNoClaudeLiteralOutsideTheLibrary is H1.
//
// Test files are not read: they build Claude records as INPUT (a registry
// file, a transcript line), and spelling the format to construct a fixture of
// it is not decoding it.
func TestNoClaudeLiteralOutsideTheLibrary(t *testing.T) {
	claudeOnly, values := claudeLiterals(t)
	used := map[string]bool{}
	scanned := 0
	for _, dir := range claudeLiteralScope {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			scanned++
			rel := moduleRel(t, p)
			for _, s := range claudeSites(fset, f, claudeOnly, values) {
				key := rel + ":" + s.Literal
				if _, own := magmuxOwnWords[key]; own {
					used[key] = true
					continue
				}
				t.Errorf("%s: %s decodes %q, which is Claude Code's. Claude's formats are decoded in "+
					"claudestate/claude and nowhere else; call its decoder, or — if this is magmux's OWN "+
					"word — add %q to magmuxOwnWords with the reason.", s.Pos, s.Shape, s.Literal, key)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if scanned < 50 {
		t.Errorf("scanned only %d Go files; the walk is not reaching magmux", scanned)
	}
	var stale []string
	for key, why := range magmuxOwnWords {
		if !used[key] {
			stale = append(stale, key+" ("+why+")")
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("magmuxOwnWords lists %s, and no such site exists any more; remove the entry", s)
	}
}

// moduleRel is p from the module root, slash-separated.
func moduleRel(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

// TestTheClaudeLiteralGuardCatchesAPlantedDecode is H1's self-test, against a
// FILE on disk walked by the same library walker the guard is built on: a
// guard that passes on a clean tree passes whether it works or is a stub.
//
// The plant is §6's: a map index of "sessionId" and a `json:"waitingFor"`
// tag, two findings. Then the value rule: a compare against a registry status
// word is a decode, and indexing a record by the plain word "status" is not
// one this guard can tell from magmux's own JSON — said here so the limit is
// pinned rather than discovered.
func TestTheClaudeLiteralGuardCatchesAPlantedDecode(t *testing.T) {
	claudeOnly, values := claudeLiterals(t)
	dir := t.TempDir()
	src := "package p\n\n" +
		"type rec struct {\n\tW string `json:\"waitingFor\"`\n}\n\n" +
		"func f(m map[string]any) any { return m[\"sessionId\"] }\n"
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := lint.ClaudeDecodeSites([]string{dir}, claude.Vocabulary(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var mine []lint.Finding
	for _, f := range found {
		if claudeOnly[f.Literal] {
			mine = append(mine, f)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("the planted decoder produced %d findings (%v), want 2: the index and the tag", len(mine), mine)
	}

	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"comparing against a registry status word", "package p\nfunc f(raw string) bool { return raw == \"busy\" }\n", 1},
		{"switching on a job state", "package p\nfunc f(raw string) int {\nswitch raw {\ncase \"blocked\":\nreturn 1\n}\nreturn 0\n}\n", 1},
		{"a Claude-only value in a compare", "package p\nfunc f(raw string) bool { return raw == \"input needed\" }\n", 1},
		{"indexing by a plain key: magmux's own JSON, out of reach", "package p\nfunc f(m map[string]any) any { return m[\"status\"] }\n", 0},
		{"a status word as output", "package p\nconst s = \"busy\"\n", 0},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", tc.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := len(claudeSites(fset, f, claudeOnly, values)); got != tc.want {
			t.Errorf("%s: %d findings, want %d", tc.name, got, tc.want)
		}
	}
}

// ── H5 ──────────────────────────────────────────────────────────────────────

// obsTickerScope is magmux's observation code: the act, navigate and notify
// packages (tests included), and mux's obs*.go files.
func obsTickerFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir("../internal/obs", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := filepath.Glob("obs*.go")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, mine...)
}

var tickerBanned = map[string]string{
	"Tick":      "time.Tick leaks a ticker that can never be stopped",
	"NewTicker": "time.NewTicker is a poll loop by another name",
	"Ticker":    "a time.Ticker in the observation layer is a poll loop",
}

// tickerSites returns every reference to the time package's ticker API.
func tickerSites(fset *token.FileSet, f *ast.File) []string {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if imp.Path == nil || imp.Path.Value != `"time"` {
			continue
		}
		if imp.Name == nil {
			names["time"] = true
		} else {
			names[imp.Name.Name] = true
		}
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && names[id.Name] {
			if why, bad := tickerBanned[sel.Sel.Name]; bad {
				out = append(out, fset.Position(sel.Pos()).String()+": "+id.Name+"."+sel.Sel.Name+" — "+why)
			}
		}
		return true
	})
	return out
}

// TestNoTickerAnywhereInTheObservationLayer is H5: the no-polling decision,
// proven by reading the source because it cannot be proven by behaviour. A
// ticker's channel and a one-shot timer feed the same instants; what differs
// is what happens when NOTHING is going on, and there is no assertion over an
// idle process that catches that reliably on a shared CI machine.
func TestNoTickerAnywhereInTheObservationLayer(t *testing.T) {
	files := obsTickerFiles(t)
	if len(files) < 20 {
		t.Fatalf("found only %d files; the scope is not reaching the observation code", len(files))
	}
	for _, p := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range tickerSites(fset, f) {
			t.Errorf("%s. The observation layer uses one-shot timers only.", s)
		}
	}
	t.Logf("scanned %d files", len(files))
}

// TestTheTickerDetectorCatchesTheShapesItMustCatch proves H5 is not vacuous.
func TestTheTickerDetectorCatchesTheShapesItMustCatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"plain time.NewTicker", "package p\nimport \"time\"\nfunc f() { _ = time.NewTicker(time.Second) }\n", true},
		{"time.Tick", "package p\nimport \"time\"\nfunc f() { _ = time.Tick(time.Second) }\n", true},
		{"a renamed import", "package p\nimport clock \"time\"\nfunc f() { _ = clock.NewTicker(clock.Second) }\n", true},
		{"a ticker in a struct field", "package p\nimport \"time\"\ntype s struct { t *time.Ticker }\n", true},
		{"the one-shot timer", "package p\nimport \"time\"\nfunc f() { _ = time.NewTimer(time.Second) }\n", false},
		{"a comment and a string", "package p\n// not a time.NewTicker\nconst s = \"time.NewTicker\"\n", false},
		{"a local called time", "package p\nfunc f() { var time struct{ NewTicker int }; _ = time.NewTicker }\n", false},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", tc.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := len(tickerSites(fset, f)) > 0; got != tc.want {
			t.Errorf("%s: detector reported %v, want %v", tc.name, got, tc.want)
		}
	}
}
