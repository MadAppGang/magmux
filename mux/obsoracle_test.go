package mux

// The liveness oracle is INJECTED, and this file is the proof that no test can
// reach the real one.
//
// `obsOracle` sends kill(pid, 0) — which delivers no signal, but which does ask
// the kernel about a pid that belongs to somebody else's process on whatever
// machine is running the suite. testdata/PROVENANCE.md rule 2 forbids that
// outright, and the reason it gives is not "signals are dangerous" but that a
// test's answer must not depend on what else happens to be running.

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// fakeOracle is the liveness answer a test supplies instead of the kernel's.
//
// A plain map read: one oracle serves every session's actor and they run on
// their own goroutines, so an oracle that counted its calls would need a lock.
// Anything absent from the map is undetermined, which is the honest answer for
// a pid nobody asked about.
type fakeOracle map[int]obs.Liveness

func (f fakeOracle) Alive(pid int) obs.Liveness { return f[pid] }

// TestTheInjectedOracleDecidesLiveness exercises the seam rather than merely
// asserting it exists, and takes attention rule 1 with it.
//
// A WRONG IMPLEMENTATION accepts the oracle and ignores it — which every other
// test in this file would still pass, because they are about what is NOT
// called.
func TestTheInjectedOracleDecidesLiveness(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	const pid = 4245
	for _, tt := range []struct {
		name   string
		oracle fakeOracle
		want   obs.Liveness
		// needed is the attention verdict that liveness produces. A dead
		// process is rule 1: there is nobody home to attend to.
		needed *bool
		reason string
	}{
		{"alive", fakeOracle{pid: obs.LivenessYes}, obs.LivenessYes, obs.Ptr(false), "it is working"},
		{"dead", fakeOracle{pid: obs.LivenessNo}, obs.LivenessNo, obs.Ptr(false), "its process has exited"},
		{"nobody could tell", fakeOracle{}, obs.LivenessUnknown, obs.Ptr(false), "it is working"},
		{"no oracle at all", nil, obs.LivenessUnknown, obs.Ptr(false), "it is working"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestMux(t, ctrlPanes(1)...)
			cfg := obsConfig{Root: newSilentPaneRegistry(t, pid, "busy")}
			if tt.oracle != nil {
				cfg.Oracle = tt.oracle
			}
			if err := m.startObservationWith(cfg); err != nil {
				t.Fatalf("startObservationWith: %v", err)
			}
			defer m.stopObservation()

			sub, err := m.obs.Subscribe("test", 0)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			m.obs.Refresh()
			select {
			case d, ok := <-sub.C():
				if !ok || len(d.Doc.Sessions) != 1 {
					t.Fatalf("the document is %+v (ok=%v)", d.Doc.Sessions, ok)
				}
				s := d.Doc.Sessions[0]
				if s.ProcessAlive != tt.want {
					t.Errorf("ProcessAlive is %v, want %v — the injected oracle was not consulted",
						s.ProcessAlive, tt.want)
				}
				if s.Attention.Needed == nil || *s.Attention.Needed != *tt.needed {
					t.Errorf("attention is %v, want %v (reason %q)",
						s.Attention.Needed, *tt.needed, s.Attention.Reason)
				}
				if !strings.Contains(s.Attention.Reason, tt.reason) {
					t.Errorf("reason %q does not contain %q", s.Attention.Reason, tt.reason)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("no document arrived")
			}
		})
	}
}

// oracleBanned is what a test file may not name: each is a route into the
// observation layer's signal probe that needs no other help.
//
// It is deliberately SHORT. An earlier draft banned the bare identifier "Kill"
// and immediately flagged two correct tests — one in obspane_test.go and one in
// sockdir_test.go — that fork their own `sleep` and kill it in cleanup. That is
// a test disposing of a process it owns, which is not what rule 2 is about; see
// the scope note on TestNoTestCanReachTheSignalOracle.
var oracleBanned = map[string]string{
	"obsOracle": "the observation layer's signal oracle itself",
	"RunAgents": "`magmux agents`'s entry point, which installs that oracle",
}

// oracleBannedSelectors is the same ban for a package-qualified call: a test
// that hand-rolled the probe rather than naming obsOracle. Resolved through the
// file's imports, so a renamed import cannot slip past, which is the lesson
// termev's ticker detector already learned.
var oracleBannedSelectors = map[string]map[string]string{
	"syscall": {"Kill": "syscall.Kill IS the probe, however it is spelled"},
}

// oracleProductionFiles is where naming obsOracle is CORRECT: the file that
// defines it, and the two program entry points that install it.
//
// Listed rather than counted, so that a third site is a deliberate edit to this
// table with a reviewer attached — and so this test cannot pass vacuously if
// the type is renamed and the ban above stops matching anything.
var oracleProductionFiles = map[string]bool{
	"mux/obscmd.go": true, // defines it, and installs it in RunAgents
	"mux/cli.go":    true, // installs it in Main, for startObservationIfEnabled
}

// oracleKillAllowed is the test files that may call syscall.Kill, each with the
// reason it does not breach rule 2: the pid it signals is one the test itself
// started. Keyed by the path from the module root, and held to the set of
// files that actually still do it, so an entry cannot outlive its reason.
//
// mux/claude_e2e_test.go is deliberately NOT here: it is compiled only under
// the claude_e2e build tag, which the scan below excludes, and the owner's
// exception for it is that tag (TestTheClaudeE2ETagIsOnOneFileOnly).
var oracleKillAllowed = map[string]string{
	"transport/firebase/emulator_e2e_test.go": "SIGTERMs the process GROUP of the emulator child it " +
		"started itself (-cmd.Process.Pid), on its own way out",
}

// TestNoTestCanReachTheSignalOracle proves by reading the source that nothing
// under _test.go can cause kill(pid, 0) to run.
//
// # Why this is a source scan and not an assertion over behaviour
//
// There is nothing to observe. signal 0 delivers nothing, leaves no trace and
// changes no state, so a test that probed a live pid would pass, would look
// exactly like a test that did not, and would only be wrong on the day the pid
// it named belonged to something whose liveness differed from the day before.
// That is the failure mode the rule exists to prevent, and a source scan is the
// only thing that can see it.
//
// # What it covers
//
// Every .go file the root module's default build compiles, in every package —
// mux, internal/, hub, mcp, transport and the rest (see walkModuleGoFiles). The
// oracle reaches the pipeline through registry.ProcessOracle, whose nil is
// documented as "undetermined" rather than "probe" — so a test that injects
// nothing is safe by construction, and the only way to reach the probe is to
// NAME something on the banned list.
//
// # What it claims, exactly
//
// That no test can reach obsOracle, and therefore that no observation-layer
// test result depends on what else happens to be running. It does NOT claim
// that no test anywhere asks the kernel about a pid, which would be false and
// should be: sockdir.go's own pidIsGone has tests that fork a `sleep`, ask
// about THAT pid and kill it afterwards. A test that owns the process it asks
// about satisfies rule 2's actual reason, and rewriting those would be
// obedience to the letter against the point.
//
// # What it does NOT cover, stated rather than implied
//
//  1. A test that EXECS the magmux binary with `agents` as its first argument, or
//     with MAGMUX_AGENTS switched on in the child's environment. A source scan
//     cannot see an argv or an env block assembled at run time. Nothing does
//     this today — the environment half is checked separately below, because
//     that is the one a stray line could turn on by accident.
//  2. The developer's own exported MAGMUX_AGENTS. Several tests spawn the real
//     binary and it inherits the environment, so `MAGMUX_AGENTS=1 go test .` would
//     have those children read the live registry and probe its pids. That is a
//     pre-existing property of the opt-in switch — it is why the switch exists
//     at all, see obsEnvVar — and it is out of this test's reach.
func TestNoTestCanReachTheSignalOracle(t *testing.T) {
	var scannedTests, scannedProd, named int
	killSeen := map[string]bool{}

	walkModuleGoFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		isTest := strings.HasSuffix(rel, "_test.go")
		if isTest {
			scannedTests++
		} else {
			scannedProd++
		}

		// Resolved per file so a renamed import is caught: `import sc
		// "syscall"` then sc.Kill would otherwise sail past a selector check
		// that assumed the package's own name.
		aliases := importAliases(file)

		ast.Inspect(file, func(n ast.Node) bool {
			// A package-qualified call first, because the selector's X is
			// itself an Ident and would otherwise be judged on its own.
			if sel, ok := n.(*ast.SelectorExpr); ok && isTest {
				if x, ok := sel.X.(*ast.Ident); ok {
					if pkg, ok := aliases[x.Name]; ok {
						if why, bad := oracleBannedSelectors[pkg][sel.Sel.Name]; bad {
							if _, allowed := oracleKillAllowed[rel]; allowed {
								killSeen[rel] = true
							} else {
								t.Errorf("%s: a test calls %s.%s — %s. Inject a fakeOracle, or nil, "+
									"which registry answers with LivenessUnknown rather than a probe.",
									fset.Position(sel.Pos()), x.Name, sel.Sel.Name, why)
							}
						}
					}
				}
			}
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "obsOracle" && !isTest {
				named++
				if !oracleProductionFiles[rel] {
					t.Errorf("%s: names obsOracle. Only %v may install the signal oracle; "+
						"add this file to oracleProductionFiles deliberately if that is wrong.",
						fset.Position(id.Pos()), keysOf(oracleProductionFiles))
				}
			}
			if !isTest {
				return true
			}
			if why, bad := oracleBanned[id.Name]; bad {
				t.Errorf("%s: a test names %q — %s. No test may reach the observation layer's "+
					"pid probe (testdata/PROVENANCE.md rule 2); inject a fakeOracle, or nil, "+
					"which registry answers with LivenessUnknown rather than a probe.",
					fset.Position(id.Pos()), id.Name, why)
			}
			return true
		})
	})

	// The allowlist is held to what is actually there: an entry whose file no
	// longer signals anything is a permission nobody needs, waiting to be used.
	for rel, why := range oracleKillAllowed {
		if !killSeen[rel] {
			t.Errorf("oracleKillAllowed lists %s (%s), but it no longer calls syscall.Kill; "+
				"remove the entry", rel, why)
		}
	}

	// Three non-vacuity checks. A scan that found nothing, or a ban that
	// matches nothing because the type was renamed, would pass silently and
	// prove the opposite of what it claims.
	if scannedTests == 0 || scannedProd == 0 {
		t.Fatalf("scanned %d test files and %d production files", scannedTests, scannedProd)
	}
	if named == 0 {
		t.Fatal("no production file names obsOracle at all — either the oracle was renamed and " +
			"this test is now checking for a word nothing uses, or nothing installs it")
	}
	t.Logf("scanned %d test files and %d production files; obsOracle named %d times, all in %v",
		scannedTests, scannedProd, named, keysOf(oracleProductionFiles))
}

// TestNoTestSwitchesTheObservationLayerOnForASpawnedBinary covers the half of
// gap 1 above that a source scan CAN see.
//
// Several tests spawn the real magmux binary, which honours MAGMUX_AGENTS from its
// environment and would then read the live registry and probe every pid in it.
// No test sets it to an on-value today, and this fails if one starts.
//
// It looks for Setenv CALLS rather than for the variable's name on a line. The
// line-based draft flagged `t.Fatal("the pipeline started with MAGMUX_AGENTS
// unset")` — a failure message, which obsEnabled quite correctly calls "on"
// because anything that is not an obvious off-word is on. Matching the call is
// the difference between checking what a test DOES and checking what it says.
func TestNoTestSwitchesTheObservationLayerOnForASpawnedBinary(t *testing.T) {
	var checked, found int

	walkModuleGoFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		if !strings.HasSuffix(rel, "_test.go") {
			return
		}
		checked++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Setenv" {
				return true
			}
			// t.Setenv and os.Setenv both, and anything else spelled Setenv:
			// what matters is the variable, not the receiver.
			if !namesObsEnvVar(call.Args[0]) {
				return true
			}
			found++
			value, ok := stringValue(call.Args[1])
			if !ok {
				t.Errorf("%s: sets %s to a value this test cannot read. Set it to a literal, "+
					"or this check cannot tell whether a spawned magmux would read the live "+
					"registry.", fset.Position(call.Pos()), obsEnvVar)
				return true
			}
			// obsEnabled is production's own answer, not a second list: a
			// value magmux would act on is a value this must refuse.
			if obsEnabled(value) {
				t.Errorf("%s: sets %s to %q, which is ON. A spawned magmux would read the live "+
					"registry and send kill(pid, 0) to every pid in it.",
					fset.Position(call.Pos()), obsEnvVar, value)
			}
			return true
		})
	})
	if checked == 0 {
		t.Fatal("no test files were parsed")
	}
	// Non-vacuity: there IS a test that sets the switch, and if it ever stops
	// existing this check is watching for something nobody does.
	if found == 0 {
		t.Fatalf("no test sets %s at all — this check is looking for a call nobody makes, "+
			"and would pass whatever happened", obsEnvVar)
	}
	t.Logf("parsed %d test files; %d set %s, none to an on-value", checked, found, obsEnvVar)
}

// namesObsEnvVar reports whether e is the switch's name, written either as the
// obsEnvVar constant or as the literal a test could reasonably inline.
func namesObsEnvVar(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "obsEnvVar"
	case *ast.BasicLit:
		s, ok := stringValue(v)
		return ok && s == obsEnvVar
	}
	return false
}

// stringValue unquotes a string literal. ok is false for anything computed,
// which the caller reports rather than ignores.
func stringValue(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// importAliases maps every identifier a file refers to a package by, back to
// that package's path. A dot import yields no usable name and is therefore
// absent, which makes it unresolvable rather than silently allowed.
func importAliases(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		if imp.Name != nil {
			if imp.Name.Name == "." || imp.Name.Name == "_" {
				continue
			}
			out[imp.Name.Name] = path
			continue
		}
		out[name] = path
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// moduleRoot is the root module's directory, seen from this package's own
// (go test runs each package with cwd = its directory, and this one is mux/).
const moduleRoot = ".."

// walkModuleGoFiles parses every .go file the root module's DEFAULT build
// compiles, and hands each to visit with its path from the module root.
//
// # Why the whole module, and not this package
//
// The guards below were written when everything was package main in one
// directory, and "." was the repository. It is not any more: obsOracle lives in
// mux, and a test in hub, client, mcp, transport or internal/obs could reach
// the probe just as well. The walk therefore starts at the module root and
// covers every package `go test ./...` builds.
//
// # What it skips, and why each is not a hole
//
//   - directories the go tool itself ignores for ./... — testdata, and names
//     beginning with "." or "_" — plus node_modules;
//   - NESTED MODULES (a directory with its own go.mod, today demo/rc/driver):
//     `go test ./...` here never builds them, so a test there cannot run under
//     this suite at all;
//   - files whose build constraint excludes them from the default build. This
//     is what keeps mux/claude_e2e_test.go out, and it is deliberate rather
//     than an allowlist entry: that file runs only under `-tags claude_e2e`,
//     which no CI job passes, and the owner's exception to rule 2 for it is
//     exactly that tag — see TestTheClaudeE2ETagIsOnOneFileOnly, which stops
//     the exception from growing.
func walkModuleGoFiles(t *testing.T, visit func(rel string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	var parsed int
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != moduleRoot {
				if name == "testdata" || name == "node_modules" ||
					strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					return fs.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return fs.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if !inDefaultBuild(file) {
			return nil
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		parsed++
		visit(filepath.ToSlash(rel), fset, file)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if parsed == 0 {
		t.Fatal("walked the module and parsed no Go file; the walk is blind")
	}
}

// buildConstraint returns the file's //go:build expression, or nil for none.
func buildConstraint(file *ast.File) constraint.Expr {
	for _, cg := range file.Comments {
		if cg.Pos() >= file.Package {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				if expr, err := constraint.Parse(c.Text); err == nil {
					return expr
				}
			}
		}
	}
	return nil
}

// inDefaultBuild evaluates a file's build constraint the way a plain
// `go test ./...` on this machine would: GOOS, GOARCH, unix, cgo and the
// release tags are set, and no custom tag is.
func inDefaultBuild(file *ast.File) bool {
	expr := buildConstraint(file)
	if expr == nil {
		return true
	}
	return expr.Eval(func(tag string) bool {
		switch {
		case tag == runtime.GOOS, tag == runtime.GOARCH, tag == "cgo":
			return true
		case tag == "unix":
			return runtime.GOOS != "windows" && runtime.GOOS != "plan9" && runtime.GOOS != "js" &&
				runtime.GOOS != "wasip1"
		case strings.HasPrefix(tag, "go1."):
			return true
		}
		return false
	})
}

// TestTheClaudeE2ETagIsOnOneFileOnly holds the owner's one exception to the
// rule that no test reads the real Claude config or runs `claude`: the
// quarantined TestClaudeE2E, compiled only under `-tags claude_e2e`. The
// exception is the TAG, so the set of files carrying it must stay exactly that
// one file — a second file under the same tag would inherit an exception
// nobody granted it.
func TestTheClaudeE2ETagIsOnOneFileOnly(t *testing.T) {
	want := map[string]bool{"mux/claude_e2e_test.go": true}
	got := map[string]bool{}
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != moduleRoot && (name == "node_modules" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.PackageClauseOnly)
		if err != nil {
			return err
		}
		if constraintNames(buildConstraint(file), "claude_e2e") {
			rel, _ := filepath.Rel(moduleRoot, path)
			got[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("files carrying the claude_e2e build tag: %v, want exactly %v", keysOf(got), keysOf(want))
	}
}

// constraintNames reports whether expr mentions tag anywhere.
func constraintNames(expr constraint.Expr, tag string) bool {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		return e.Tag == tag
	case *constraint.NotExpr:
		return constraintNames(e.X, tag)
	case *constraint.AndExpr:
		return constraintNames(e.X, tag) || constraintNames(e.Y, tag)
	case *constraint.OrExpr:
		return constraintNames(e.X, tag) || constraintNames(e.Y, tag)
	}
	return false
}
