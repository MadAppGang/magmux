package audit

// G10: the dependency pin. A dependency added without a decision has already
// defeated the gate, so adding one must be an edit to THIS file.
//
// Two states, and the module is in exactly one of them:
//
//   - pinNothing (L1–L3): no package imports fsnotify yet, so `go mod tidy`
//     would remove it, and go.mod requires NOTHING.
//   - pinFsnotify (from L4, the commit that adds watch/): direct =
//     {fsnotify v1.9.0}; indirect ⊆ {golang.org/x/sys}.
//
// In both: no replace, and no non-stdlib import except fsnotify, which only
// watch/ may import. It does not test for a go.work file: a developer's
// uncommitted go.work is the documented workflow, and "not committed" is a git
// fact CI checks.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type pinState int

const (
	pinNothing pinState = iota
	pinFsnotify
)

// currentPin is the state this commit is in.
const currentPin = pinNothing

const (
	fsnotifyModule  = "github.com/fsnotify/fsnotify"
	fsnotifyVersion = "v1.9.0"
)

type requirement struct {
	Path, Version string
	Indirect      bool
}

// parseGoMod reads the require and replace directives of a go.mod. It is
// deliberately small: the file is ours and its shape is gofmt's.
func parseGoMod(src string) (reqs []requirement, replaces []string) {
	block := ""
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if block != "" {
			if line == ")" {
				block = ""
				continue
			}
			switch block {
			case "require":
				reqs = append(reqs, parseRequire(line))
			case "replace":
				replaces = append(replaces, line)
			}
			continue
		}
		verb, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch {
		case (verb == "require" || verb == "replace") && rest == "(":
			block = verb
		case verb == "require":
			reqs = append(reqs, parseRequire(rest))
		case verb == "replace":
			replaces = append(replaces, rest)
		}
	}
	return reqs, replaces
}

func parseRequire(spec string) requirement {
	body, comment, _ := strings.Cut(spec, "//")
	f := strings.Fields(body)
	r := requirement{Indirect: strings.TrimSpace(comment) == "indirect"}
	if len(f) > 0 {
		r.Path = f[0]
	}
	if len(f) > 1 {
		r.Version = f[1]
	}
	return r
}

func goModProblems(src string, state pinState) []string {
	reqs, replaces := parseGoMod(src)
	var out []string
	for _, r := range replaces {
		out = append(out, "a replace directive: "+r)
	}
	sawFsnotify := false
	for _, r := range reqs {
		switch {
		case state == pinNothing:
			out = append(out, fmt.Sprintf("requires %s %s, but no package imports a module yet (L1–L3 require nothing)", r.Path, r.Version))
		case r.Path == fsnotifyModule:
			sawFsnotify = true
			if r.Version != fsnotifyVersion {
				out = append(out, fmt.Sprintf("fsnotify %s, want %s (the version G11 and G12 were measured against)", r.Version, fsnotifyVersion))
			}
			if r.Indirect {
				out = append(out, "fsnotify is indirect, but watch/ imports it")
			}
		case r.Path == "golang.org/x/sys" && r.Indirect:
			// fsnotify's own requirement, as tidy writes it.
		default:
			out = append(out, fmt.Sprintf("requires %s %s, which is not a decided dependency", r.Path, r.Version))
		}
	}
	if state == pinFsnotify && !sawFsnotify {
		out = append(out, "does not require fsnotify, which watch/ is built on")
	}
	return out
}

// importProblems: no non-stdlib import outside the module, except fsnotify
// in watch/ (and only from pinFsnotify on).
func importProblems(files []file, state pinState) []finding {
	var out []finding
	for _, f := range files {
		for _, imp := range f.AST.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(p, "/")
			if !strings.Contains(first, ".") || p == modulePath || strings.HasPrefix(p, modulePath+"/") {
				continue // standard library, or this module
			}
			if p == fsnotifyModule && state == pinFsnotify && (f.Dir == "watch" || strings.HasPrefix(f.Dir, "watch/")) {
				continue
			}
			out = append(out, finding{File: f.Rel, Line: lineOf(f, imp.Pos()), Rule: "G10 dependency-pin",
				Detail: "imports " + p + ", which is not a decided dependency of this file's package"})
		}
	}
	return out
}

func TestTheDependencySetIsExactlyWhatWasDecided(t *testing.T) {
	root := moduleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\ngo 1.26\n") {
		t.Errorf("go.mod's go line is not `go 1.26`: a floor, not a patch release, so tt's tidy never raises its own")
	}
	for _, p := range goModProblems(string(b), currentPin) {
		t.Errorf("go.mod %s", p)
	}
	if got := importProblems(loadModule(t), currentPin); len(got) != 0 {
		t.Errorf("an undecided import:\n%s", format(got))
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); err == nil && currentPin == pinNothing {
		t.Error("go.sum exists, but the module requires nothing")
	}
}

func TestThePinGuardActuallyDetectsDrift(t *testing.T) {
	const nothing = "module " + modulePath + "\n\ngo 1.26\n"
	const end = "module " + modulePath + "\n\ngo 1.26\n\nrequire github.com/fsnotify/fsnotify v1.9.0\n\nrequire golang.org/x/sys v0.13.0 // indirect\n"
	cases := []struct {
		name  string
		src   string
		state pinState
		bad   bool
	}{
		{"L1: nothing", nothing, pinNothing, false},
		{"L1: fsnotify too early", end, pinNothing, true},
		{"L4: the end state", end, pinFsnotify, false},
		{"L4: fsnotify missing", nothing, pinFsnotify, true},
		{"L4: a fourth module", end + "require example.com/x v1.0.0\n", pinFsnotify, true},
		{"L4: x/sys direct", "module m\n\nrequire (\n\tgithub.com/fsnotify/fsnotify v1.9.0\n\tgolang.org/x/sys v0.13.0\n)\n", pinFsnotify, true},
		{"L4: fsnotify drifted", "module m\n\nrequire github.com/fsnotify/fsnotify v1.10.1\n", pinFsnotify, true},
		{"L4: fsnotify indirect", "module m\n\nrequire github.com/fsnotify/fsnotify v1.9.0 // indirect\n", pinFsnotify, true},
		{"a replace", end + "\nreplace github.com/fsnotify/fsnotify => ../fsnotify\n", pinFsnotify, true},
		{"a replace block", nothing + "\nreplace (\n\tx => ../x\n)\n", pinNothing, true},
	}
	for _, c := range cases {
		got := goModProblems(c.src, c.state)
		if (len(got) > 0) != c.bad {
			t.Errorf("%s: problems %v, want bad=%v", c.name, got, c.bad)
		}
	}

	imp := "package p\nimport \"github.com/fsnotify/fsnotify\"\nvar _ = fsnotify.Create\n"
	if got := importProblems([]file{snippet(t, "watch/zz.go", imp)}, pinNothing); len(got) != 1 {
		t.Errorf("fsnotify before L4: %d findings, want 1", len(got))
	}
	if got := importProblems([]file{snippet(t, "watch/zz.go", imp)}, pinFsnotify); len(got) != 0 {
		t.Errorf("fsnotify in watch/ from L4: %d findings, want 0", len(got))
	}
	if got := importProblems([]file{snippet(t, "pipeline/zz.go", imp)}, pinFsnotify); len(got) != 1 {
		t.Errorf("fsnotify outside watch/: %d findings, want 1", len(got))
	}
	root := "package p\nimport \"github.com/MadAppGang/magmux/mux\"\n"
	if got := importProblems([]file{snippet(t, "join/zz.go", root)}, pinFsnotify); len(got) != 1 {
		t.Errorf("a magmux import: %d findings, want 1", len(got))
	}
}
