package main

import (
	"go/parser"
	"go/token"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath = "github.com/MadAppGang/magmux"
	muxPath    = modulePath + "/mux"
)

// guardedPatterns are the packages that must never depend on mux.
//
// magmux has no `internal` directory, so the compiler no longer refuses an
// import that points the wrong way, and only some wrong-way imports are
// cycles. `mcp` is the clearest case: mux never imports mcp, so an mcp -> mux
// import would compile. This test is the guard the compiler would otherwise
// have been.
//
// A pattern that matches no package yet is skipped, so the guard passes today
// and starts biting the moment the package is created.
var guardedPatterns = []string{
	modulePath + "/hub",
	modulePath + "/transport/...",
	modulePath + "/plugin",
	modulePath + "/auth",
	modulePath + "/protocol",
	modulePath + "/client",
	modulePath + "/mcp",
	modulePath + "/sockdir",
	modulePath + "/unixsock/...",
	modulePath + "/internal/obs/...",
	modulePath + "/obsview",
}

// goList runs `go list` and returns its non-empty stdout lines.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var lines []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// muxImporters returns every package in pkg's dependency graph (pkg
// included) that imports mux directly. It covers the production graph only,
// the one `go build` links.
func muxImporters(t *testing.T, pkg string) []string {
	t.Helper()
	var importers []string
	for _, line := range goList(t, "-deps", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", pkg) {
		fields := strings.Fields(line)
		if slices.Contains(fields[1:], muxPath) {
			importers = append(importers, fields[0])
		}
	}
	return importers
}

// TestImportDirection enforces the downward import direction:
// cmd/magmux -> {mux, mcp} -> {hub, plugin, transport/..., auth, leaves} ->
// protocol. None of the guarded packages may reach mux, directly or through
// any dependency.
func TestImportDirection(t *testing.T) {
	// Positive control: the shim imports mux, so the detection must see it.
	// Without this, a typo in muxPath would make every check below pass
	// vacuously.
	if got := muxImporters(t, modulePath+"/cmd/magmux"); !slices.Contains(got, modulePath+"/cmd/magmux") {
		t.Fatalf("positive control failed: cmd/magmux imports mux but the guard saw importers %v; the check is blind", got)
	}

	// -e keeps a missing package from failing the listing; its .Error is set,
	// so the template drops it. An unmatched `...` pattern prints nothing.
	existing := goList(t, append([]string{"-e", "-f", "{{if not .Error}}{{.ImportPath}}{{end}}"}, guardedPatterns...)...)
	t.Logf("guarded packages present: %v", existing)

	for _, pkg := range existing {
		if importers := muxImporters(t, pkg); len(importers) > 0 {
			t.Errorf("%s depends on %s (imported directly by %s); mux registers INTO the lower packages, never the reverse",
				pkg, muxPath, strings.Join(importers, ", "))
		}
	}
}

// libraryPipeline is the claudestate runtime. §2.3's host rules: only mux's
// obs*.go files import it, and obsview and internal/obs/... never import mux
// (guardedPatterns above), and obsview never imports internal/obs/act.
const libraryPipeline = modulePath + "/claudestate/pipeline"

// pipelineImporters lists every non-test file of the root module that imports
// claudestate/pipeline, as "<package>/<file>".
func pipelineImporters(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range goList(t, "-f", "{{.ImportPath}} {{.Dir}}{{range .GoFiles}} {{.}}{{end}}", modulePath+"/...") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pkg, dir := fields[0], fields[1]
		for _, f := range fields[2:] {
			if fileImports(t, dir+"/"+f, libraryPipeline) {
				out = append(out, strings.TrimPrefix(pkg, modulePath+"/")+"/"+f)
			}
		}
	}
	return out
}

func fileImports(t *testing.T, path, imp string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	for _, s := range f.Imports {
		if p, err := strconv.Unquote(s.Path.Value); err == nil && p == imp {
			return true
		}
	}
	return false
}

// pipelineImportAllowed is the rule as a predicate, so the self-test holds it.
func pipelineImportAllowed(file string) bool {
	rest, ok := strings.CutPrefix(file, "mux/")
	return ok && !strings.Contains(rest, "/") && strings.HasPrefix(rest, "obs")
}

func TestOnlyMuxObsFilesImportTheLibraryPipeline(t *testing.T) {
	importers := pipelineImporters(t)
	if len(importers) == 0 {
		t.Fatal("no file imports claudestate/pipeline; the scan is blind")
	}
	for _, f := range importers {
		if !pipelineImportAllowed(f) {
			t.Errorf("%s imports %s; only mux/obs*.go may (architecture §2.3)", f, libraryPipeline)
		}
	}
	t.Logf("importers: %v", importers)
	for _, tc := range []struct {
		file string
		ok   bool
	}{{"mux/obspane.go", true}, {"mux/grid.go", false}, {"mcp/obs.go", false}, {"obsview/table.go", false}} {
		if pipelineImportAllowed(tc.file) != tc.ok {
			t.Errorf("the rule says %s is %v, want %v", tc.file, !tc.ok, tc.ok)
		}
	}
	if got := goList(t, "-deps", "-f", "{{.ImportPath}}", modulePath+"/obsview"); slices.Contains(got, modulePath+"/internal/obs/act") {
		t.Error("obsview imports internal/obs/act; act may import obsview, never the reverse (§2.3)")
	}
}
