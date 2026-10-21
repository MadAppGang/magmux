package audit

// G13: import direction, the §2.3 DAG, over NON-TEST packages via `go list`.
// Test files, external _test packages included, may import any library
// package except _probe.

import (
	"bytes"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// allowedImports: package (relative to the module) -> the module packages it
// may import. A package missing from this table is a finding, so a new
// package has to be placed in the DAG deliberately.
var allowedImports = map[string][]string{
	".":                   nil,
	"model":               nil,
	"ports":               {"model"},
	"stage":               {"model"},
	"claude":              {"model"},
	"claude/doctor":       {"claude", "model", "ports"},
	"join":                {"claude", "model"},
	"watch":               {"model", "ports"},
	"internal/source":     {"claude", "model", "ports"},
	"internal/termev":     {"model", "ports"},
	"verdict":             {"internal/termev", "stage", "model", "ports"},
	"internal/claudecode": {"claude", "internal/source", "stage", "model", "ports"},
	"pipeline":            {"model", "ports", "stage", "claude", "join", "watch", "internal/source", "verdict", "internal/claudecode"},
	"proclive":            {"model", "ports"},
	"lint":                {"claude"},
	"internal/difftest":   {"model", "join", "verdict", "claude"},
	"internal/audit":      nil,
	"_probe":              nil, // stdlib ONLY
}

// anythingInModule may import any module package except _probe.
var anythingInModule = map[string]bool{"_diff": true}

// listing is `go list`'s answer: package -> its direct imports (full paths).
type listing map[string][]string

func rel(p string) (string, bool) {
	switch {
	case p == modulePath:
		return ".", true
	case strings.HasPrefix(p, modulePath+"/"):
		return strings.TrimPrefix(p, modulePath+"/"), true
	}
	return "", false
}

func importDirectionProblems(l listing) []string {
	var out []string
	for _, pkg := range sortedKeys(l) {
		from, ok := rel(pkg)
		if !ok {
			out = append(out, "go list returned a package outside the module: "+pkg)
			continue
		}
		allowed, known := allowedImports[from]
		if !known && !anythingInModule[from] {
			out = append(out, from+" is not in the §2.3 table; place it in the DAG")
			continue
		}
		set := map[string]bool{}
		for _, a := range allowed {
			set[a] = true
		}
		for _, imp := range l[pkg] {
			to, inModule := rel(imp)
			if !inModule {
				first, _, _ := strings.Cut(imp, "/")
				if from == "_probe" && strings.Contains(first, ".") {
					out = append(out, "_probe imports "+imp+": it is stdlib ONLY")
				}
				continue // the stdlib, or an external module G10 pins
			}
			switch {
			case to == "_probe":
				out = append(out, from+" imports _probe: nothing in the module may")
			case anythingInModule[from]:
			case !set[to]:
				out = append(out, from+" imports "+to+", which §2.3 does not allow")
			}
		}
	}
	return out
}

func goList(t *testing.T) listing {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`, "./...", "./_probe")
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	l := listing{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 {
			l[f[0]] = f[1:]
		}
	}
	return l
}

func TestImportDirection(t *testing.T) {
	l := goList(t)
	if p := importDirectionProblems(l); len(p) != 0 {
		t.Fatalf("the import DAG is violated:\n  %s", strings.Join(p, "\n  "))
	}

	// The positive control that EXISTS at this commit: ports must be seen
	// importing model. Without it a listing that returned nothing would pass.
	// L5 switches it to pipeline importing watch.
	if !contains(l[modulePath+"/ports"], modulePath+"/model") {
		t.Fatalf("go list does not show ports importing model; the guard is not seeing the graph: %v", l[modulePath+"/ports"])
	}
	if _, ok := l[modulePath+"/_probe"]; !ok {
		t.Fatal("go list did not return _probe; its stdlib-only rule is not being checked")
	}
	var pkgs []string
	for p := range l {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	t.Logf("checked %d packages: %v", len(pkgs), pkgs)
}

func TestTheImportGuardActuallyDetectsViolations(t *testing.T) {
	m := func(s string) string { return modulePath + "/" + s }
	cases := []struct {
		name string
		l    listing
	}{
		{"model imports ports", listing{m("model"): {m("ports")}}},
		{"claude imports ports", listing{m("claude"): {m("model"), m("ports")}}},
		{"join imports pipeline", listing{m("join"): {m("pipeline")}}},
		{"a package imports _probe", listing{m("_diff"): {m("_probe")}}},
		{"_probe imports the module", listing{m("_probe"): {m("model")}}},
		{"_probe imports a module dependency", listing{m("_probe"): {"github.com/fsnotify/fsnotify"}}},
		{"an unplaced package", listing{m("newthing"): nil}},
	}
	for _, c := range cases {
		if p := importDirectionProblems(c.l); len(p) == 0 {
			t.Errorf("%s: no problem reported", c.name)
		}
	}
	ok := listing{m("ports"): {"context", m("model")}, m("_diff"): {m("pipeline"), m("model")}, m("_probe"): {"os"}}
	if p := importDirectionProblems(ok); len(p) != 0 {
		t.Errorf("a legal graph was reported: %v", p)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
