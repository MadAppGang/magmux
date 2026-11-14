package pipeline

// G7: the watch protocol's properties, asserted against the SOURCE of
// coordinator.cycle (a rewrite of tt:watchloop_ast_test.go:118-320 against
// §3.1's landmark names). A behavioural test fails only when an interleaving
// happens to be exercised; this one fails on the EDIT that moves a call.
//
//	(a1) every armSpine / discoverJobDirs / armJobDirs statement precedes the
//	     first verifyIdentity, which precedes readSources;
//	(a2) readSources < locateTranscripts < prepareTargets < armTargets <
//	     the second verifyIdentity < grant < observeAll < publish;
//	(b)  the watcher's Release has exactly one call site, releaseOnce, and it
//	     runs after publish;
//	(c)  no Arm is reachable from any statement after publish;
//	(d)  deliverEffects is the last statement;
//	(e)  only pipeline calls the watcher's Arm, Verify or Release.
//
// The exemptions (directory metadata, degraded pulls) are not expressed in the
// AST; they are runtime clauses C17 and C11, each with its test.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const watchPkg = "github.com/MadAppGang/magmux/claudestate/watch"

// protocolFindings checks (a)–(d) over the package's non-test files.
func protocolFindings(files []*ast.File) []string {
	var out []string
	callees := map[string]map[string]bool{}
	var cycle *ast.FuncDecl
	releaseSites := map[string]int{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			names := calls(fn.Body)
			callees[fn.Name.Name] = names
			if fn.Name.Name == "cycle" && fn.Recv != nil {
				cycle = fn
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel := watcherCall(n); sel == "Release" {
					releaseSites[fn.Name.Name]++
				}
				return true
			})
		}
	}
	if cycle == nil {
		return []string{"coordinator.cycle is gone; the protocol moved and G7 must move with it"}
	}
	reaches := func(from, target string) bool {
		seen := map[string]bool{}
		var walk func(string) bool
		walk = func(n string) bool {
			if seen[n] {
				return false
			}
			seen[n] = true
			for c := range callees[n] {
				if c == target || walk(c) {
					return true
				}
			}
			return false
		}
		return from == target || walk(from)
	}
	// Index the landmarks by statement.
	at := map[string][]int{}
	for i, st := range cycle.Body.List {
		for name := range calls(st) {
			at[name] = append(at[name], i)
		}
	}
	first := func(name string) int {
		if len(at[name]) == 0 {
			out = append(out, "cycle never calls "+name)
			return -1
		}
		return at[name][0]
	}
	verify := at["verifyIdentity"]
	if len(verify) != 2 {
		out = append(out, fmt.Sprintf("cycle calls verifyIdentity %d times, want 2 (before the read, after the target wave)", len(verify)))
		return out
	}
	read := first("readSources")
	// (a1)
	for _, name := range []string{"armSpine", "discoverJobDirs", "armJobDirs"} {
		for _, i := range at[name] {
			if i >= verify[0] {
				out = append(out, fmt.Sprintf("(a1) %s (statement %d) does not precede the first verifyIdentity (%d)", name, i, verify[0]))
			}
		}
		first(name)
	}
	if !(verify[0] < read) {
		out = append(out, fmt.Sprintf("(a1) the first verifyIdentity (%d) does not precede readSources (%d)", verify[0], read))
	}
	// (a2)
	order := []struct {
		name string
		i    int
	}{
		{"readSources", read}, {"locateTranscripts", first("locateTranscripts")}, {"prepareTargets", first("prepareTargets")},
		{"armTargets", first("armTargets")}, {"verifyIdentity#2", verify[1]}, {"grant", first("grant")},
		{"observeAll", first("observeAll")}, {"publish", first("publish")},
	}
	for k := 1; k < len(order); k++ {
		if !(order[k-1].i < order[k].i) {
			out = append(out, fmt.Sprintf("(a2) %s (%d) does not precede %s (%d)", order[k-1].name, order[k-1].i, order[k].name, order[k].i))
		}
	}
	publish := first("publish")
	// (b)
	if len(releaseSites) != 1 || releaseSites["releaseOnce"] != 1 {
		out = append(out, fmt.Sprintf("(b) the watcher's Release is called from %v; want exactly one site, releaseOnce", releaseSites))
	}
	for _, i := range at["releaseOnce"] {
		if i <= publish {
			out = append(out, fmt.Sprintf("(b) releaseOnce (%d) runs at or before publish (%d)", i, publish))
		}
	}
	if len(at["releaseOnce"]) != 1 {
		out = append(out, fmt.Sprintf("(b) cycle calls releaseOnce %d times", len(at["releaseOnce"])))
	}
	// (c)
	for i := publish + 1; i < len(cycle.Body.List); i++ {
		for name := range calls(cycle.Body.List[i]) {
			if name == "Arm" || reaches(name, "Arm") {
				out = append(out, fmt.Sprintf("(c) statement %d (%s) reaches the watcher's Arm after publish", i, name))
			}
		}
	}
	if reaches("releaseOnce", "Arm") || reaches("deliverEffects", "Arm") {
		out = append(out, "(c) the post-publication phase reaches Arm")
	}
	// (d)
	if last := len(cycle.Body.List) - 1; len(at["deliverEffects"]) != 1 || at["deliverEffects"][0] != last {
		out = append(out, fmt.Sprintf("(d) deliverEffects is not the last statement of cycle (%v of %d)", at["deliverEffects"], last))
	}
	return out
}

// calls is the called names under n: a selector's final identifier, or an
// identifier.
func calls(n ast.Node) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			names[f.Sel.Name] = true
		case *ast.Ident:
			names[f.Name] = true
		}
		return true
	})
	return names
}

// watcherCall names an Arm/Verify/Release call on a value reached through a
// field called watcher, which is how this package holds its *watch.Watcher.
func watcherCall(n ast.Node) string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	switch sel.Sel.Name {
	case "Arm", "Verify", "Release":
	default:
		return ""
	}
	if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "watcher" {
		return sel.Sel.Name
	}
	return ""
}

// foreignWatchCallers is (e): a non-test file outside pipeline/ and watch/
// that imports the watch package and calls Arm, Verify or Release.
func foreignWatchCallers(rel string, f *ast.File) []string {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "pipeline" || dir == "watch" || strings.HasSuffix(rel, "_test.go") {
		return nil
	}
	imported := false
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == watchPkg {
			imported = true
		}
	}
	if !imported {
		return nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains([]string{"Arm", "Verify", "Release"}, sel.Sel.Name) {
				out = append(out, rel+" calls "+sel.Sel.Name+": only pipeline drives the watcher (e)")
			}
		}
		return true
	})
	return out
}

func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	paths, _ := filepath.Glob("*.go")
	var out []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	if len(out) < 3 {
		t.Fatalf("parsed %d files", len(out))
	}
	return out
}

func TestTheCycleKeepsTheWatchProtocol(t *testing.T) {
	if got := protocolFindings(parsePackage(t)); len(got) != 0 {
		t.Fatalf("the watch protocol is violated:\n  %s", strings.Join(got, "\n  "))
	}
	root := filepath.Join("..")
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || (p != root && fileExists(filepath.Join(p, "go.mod"))) {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".go" {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		n++
		for _, v := range foreignWatchCallers(rel, f) {
			t.Error(v)
		}
		return nil
	})
	if err != nil || n < 30 {
		t.Fatalf("walked %d files: %v", n, err)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Each property is shown to fail on a mutated copy of the real cycle.
func TestTheProtocolGuardCatchesEachMutation(t *testing.T) {
	type mutation struct {
		name string
		fn   func(files []*ast.File)
	}
	cycleOf := func(files []*ast.File) *ast.FuncDecl {
		for _, f := range files {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "cycle" && fn.Recv != nil {
					return fn
				}
			}
		}
		t.Fatal("no cycle")
		return nil
	}
	index := func(fn *ast.FuncDecl, name string) int {
		for i, st := range fn.Body.List {
			if calls(st)[name] {
				return i
			}
		}
		t.Fatalf("no %s", name)
		return -1
	}
	swap := func(fn *ast.FuncDecl, a, b string) {
		i, j := index(fn, a), index(fn, b)
		fn.Body.List[i], fn.Body.List[j] = fn.Body.List[j], fn.Body.List[i]
	}
	stmt := func(src string) ast.Stmt {
		e, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		return &ast.ExprStmt{X: e}
	}
	insertAfter := func(fn *ast.FuncDecl, name string, s ast.Stmt) {
		i := index(fn, name)
		fn.Body.List = append(fn.Body.List[:i+1], append([]ast.Stmt{s}, fn.Body.List[i+1:]...)...)
	}
	mutations := []mutation{
		{"(a1) the job-directory arm moved after the first verify", func(fs []*ast.File) { swap(cycleOf(fs), "armJobDirs", "verifyIdentity") }},
		{"(a1) the read moved before the first verify", func(fs []*ast.File) { swap(cycleOf(fs), "verifyIdentity", "readSources") }},
		{"(a2) the target wave moved after the grant", func(fs []*ast.File) { swap(cycleOf(fs), "armTargets", "grant") }},
		{"(a2) location after prepare", func(fs []*ast.File) { swap(cycleOf(fs), "locateTranscripts", "prepareTargets") }},
		{"(b) a second Release site", func(fs []*ast.File) {
			fn := cycleOf(fs)
			fn.Body.List = append([]ast.Stmt{stmt("c.watcher.Release(nil)")}, fn.Body.List...)
		}},
		{"(b) the release before the publish", func(fs []*ast.File) { swap(cycleOf(fs), "publish", "releaseOnce") }},
		{"(c) an arm wave after the publish", func(fs []*ast.File) { insertAfter(cycleOf(fs), "publish", stmt("c.armTargets(cs)")) }},
		{"(d) deliverEffects not last", func(fs []*ast.File) { swap(cycleOf(fs), "releaseOnce", "deliverEffects") }},
	}
	for _, m := range mutations {
		files := parsePackage(t)
		m.fn(files)
		if got := protocolFindings(files); len(got) == 0 {
			t.Errorf("%s: no finding", m.name)
		}
	}
	foreign, err := parser.ParseFile(token.NewFileSet(), "verdict/zz.go",
		"package verdict\nimport \""+watchPkg+"\"\nfunc f(w *watch.Watcher) { w.Arm(nil) }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := foreignWatchCallers("verdict/zz.go", foreign); len(got) != 1 {
		t.Errorf("(e) a caller outside pipeline: %v", got)
	}
}
