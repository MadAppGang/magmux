package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// The root module's dependency pin (architecture §6 G10, the host half).
//
// magmux's dependency rule used to be "none": two golang.org/x modules for the
// PTY ioctls and raw mode, and nothing else. It was lifted twice, each time for
// one module and in a diff that said so: fsnotify, for the directory watch the
// observation layer is built on (the measured reason is G11, which lives with
// the watcher in claudestate/watch), and claudestate itself, the library that
// observation layer now is.
//
// "Lifted for one module" is a claim that decays silently. This test is what
// keeps it true: adding a dependency now requires deciding to, in a diff that
// says so.
//
// It does not test for a go.work file. A developer's uncommitted go.work is
// the documented workflow (architecture §7.2); "not committed" is a git fact,
// checked by .gitignore and by CI's `git ls-files` step.

const (
	rootGoMod        = "../../go.mod"
	claudestateGoMod = "../../claudestate/go.mod"

	claudestateModule = modulePath + "/claudestate"
	fsnotifyModule    = "github.com/fsnotify/fsnotify"
)

// decidedDependencies is the whole of the root's requirement set, and why
// each one is here.
var decidedDependencies = map[string]string{
	"golang.org/x/sys":  "PTY ioctls",
	"golang.org/x/term": "raw mode",
	fsnotifyModule:      "the directory watch claudestate/watch is built on",
	claudestateModule:   "the Claude Code observation library",
}

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

// rootPinProblems holds the root go.mod to the decided set. libFsnotify is the
// fsnotify version claudestate requires: the root's must be the same, so the
// version G11 and G12 were measured against is the one magmux links.
// claudestate may be at any version that is not replaced: a pseudo-version
// before the first tag, a tag after it (the release gate tells them apart).
// fsnotify may be direct or indirect: direct while a root package still
// imports it, indirect once only the library does.
func rootPinProblems(src, libFsnotify string) []string {
	reqs, replaces := parseGoMod(src)
	var out []string
	for _, r := range replaces {
		out = append(out, "has a replace directive ("+r+"); the root requires claudestate by version, never by path (architecture §7)")
	}
	seen := map[string]bool{}
	for _, r := range reqs {
		why, ok := decidedDependencies[r.Path]
		if !ok {
			out = append(out, fmt.Sprintf("requires %s %s, which is not one of the modules magmux decided to depend on. "+
				"Adding a dependency is a decision; make it explicitly here, or drop the import.", r.Path, r.Version))
			continue
		}
		seen[r.Path] = true
		switch r.Path {
		case fsnotifyModule:
			if libFsnotify != "" && r.Version != libFsnotify {
				out = append(out, fmt.Sprintf("requires fsnotify %s, but claudestate requires %s: the library's G11 and G12 "+
					"were measured against its own version, so bump the two together", r.Version, libFsnotify))
			}
		case claudestateModule:
			if r.Version == "" {
				out = append(out, "requires claudestate with no version")
			}
		default:
			if r.Indirect {
				out = append(out, fmt.Sprintf("requires %s as indirect, but magmux imports it itself (%s)", r.Path, why))
			}
		}
	}
	for path, why := range decidedDependencies {
		if !seen[path] {
			out = append(out, fmt.Sprintf("no longer requires %s, which was here for %s. "+
				"If the need has genuinely gone, remove it from this test in the same diff.", path, why))
		}
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func requirementOf(src, path string) (requirement, bool) {
	reqs, _ := parseGoMod(src)
	for _, r := range reqs {
		if r.Path == path {
			return r, true
		}
	}
	return requirement{}, false
}

func TestDependencySetIsExactlyWhatWasDecided(t *testing.T) {
	lib, ok := requirementOf(readFile(t, claudestateGoMod), fsnotifyModule)
	if !ok {
		t.Fatalf("%s does not require fsnotify, so there is no version to hold the root to", claudestateGoMod)
	}
	for _, p := range rootPinProblems(readFile(t, rootGoMod), lib.Version) {
		t.Errorf("go.mod %s", p)
	}
}

// TestClaudestateIsRequiredAndResolvable is what keeps the requirement through
// `go mod tidy` before any production package imports the library (architecture
// §7.1, review HIGH-7): tidy keeps what the main module's tests need, and this
// test needs claudestate/model. Resolvable means it compiles against the
// pinned version, not the in-tree copy: the root builds with GOWORK=off.
func TestClaudestateIsRequiredAndResolvable(t *testing.T) {
	if model.KindUnknown.String() != "unknown" {
		t.Errorf("claudestate/model.KindUnknown prints %q, want \"unknown\"", model.KindUnknown.String())
	}
	src := readFile(t, rootGoMod)
	r, ok := requirementOf(src, claudestateModule)
	if !ok {
		t.Fatalf("go.mod does not require %s", claudestateModule)
	}
	if r.Indirect {
		t.Errorf("go.mod requires %s %s as indirect, but this module's own test imports it", claudestateModule, r.Version)
	}
	if _, replaces := parseGoMod(src); len(replaces) != 0 {
		t.Errorf("go.mod replaces a module (%v): claudestate is required by version, never by path", replaces)
	}
}

func TestThePinGuardActuallyDetectsDrift(t *testing.T) {
	const pseudo = claudestateModule + " v0.0.0-20261021095107-3f18ddddbfe6"
	good := "module " + modulePath + "\n\ngo 1.26.1\n\nrequire (\n" +
		"\t" + pseudo + "\n" +
		"\tgithub.com/fsnotify/fsnotify v1.9.0\n" +
		"\tgolang.org/x/sys v0.42.0\n" +
		"\tgolang.org/x/term v0.41.0\n)\n"
	indirectFsnotify := strings.Replace(good, "fsnotify v1.9.0\n", "fsnotify v1.9.0 // indirect\n", 1)
	tagged := strings.Replace(good, "v0.0.0-20261021095107-3f18ddddbfe6", "v0.1.0", 1)
	cases := []struct {
		name string
		src  string
		bad  bool
	}{
		{"the decided set, claudestate at a pseudo-version", good, false},
		{"fsnotify indirect once only the library imports it", indirectFsnotify, false},
		{"claudestate at a tag", tagged, false},
		{"a fifth module", good + "\nrequire example.com/x v1.0.0\n", true},
		{"claudestate missing", strings.Replace(good, "\t"+pseudo+"\n", "", 1), true},
		{"x/term missing", strings.Replace(good, "\tgolang.org/x/term v0.41.0\n", "", 1), true},
		{"x/sys indirect", strings.Replace(good, "x/sys v0.42.0\n", "x/sys v0.42.0 // indirect\n", 1), true},
		{"fsnotify drifted from the library's", strings.Replace(good, "fsnotify v1.9.0", "fsnotify v1.10.1", 1), true},
		{"a replace line", good + "\nreplace " + claudestateModule + " => ./claudestate\n", true},
		{"a replace block", good + "\nreplace (\n\t" + claudestateModule + " => ./claudestate\n)\n", true},
	}
	for _, c := range cases {
		got := rootPinProblems(c.src, "v1.9.0")
		if (len(got) > 0) != c.bad {
			t.Errorf("%s: problems %v, want bad=%v", c.name, got, c.bad)
		}
	}
}
