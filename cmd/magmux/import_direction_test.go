package main

import (
	"os/exec"
	"slices"
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
