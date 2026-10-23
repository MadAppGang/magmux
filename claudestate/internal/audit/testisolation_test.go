package audit

// G8: no test reaches ~/.claude, runs Claude, or probes a foreign pid
// (NFR9, §9).
//
// It scans _test.go files under the DEFAULT build tags (a plain `go test` on
// darwin or linux) for CALLS that reach the real tree or the real tool. A
// ".claude" substring in a string literal is allowed: vectors, refusal tests
// and t.Setenv("HOME", "/home/someone") all need one, and none of them reads
// anything.
//
// This module has no claude_e2e exception. The owner's AQ8 exception covers
// exactly one file in the ROOT module (mux/claude_e2e_test.go) and is held by
// the host twin; here the set of files carrying that tag must be empty.

import (
	"go/ast"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// isolationAllowlist: test file -> citation. A stale entry fails.
var isolationAllowlist = map[string]string{
	"proclive/proclive_test.go": "architecture §6 G8: the production kill(pid, 0) oracle's own test probes " +
		"this test process's pid and a child it spawned and reaped, and no other pid",
}

const proclivePath = modulePath + "/proclive"

// isTempDirShaped reports an argument that can only name a test's own
// temporary directory or a literal path: a string literal, a call to
// <x>.TempDir(), or filepath.Join of such arguments.
func isTempDirShaped(e ast.Expr) bool {
	if _, ok := stringLit(e); ok {
		return true
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name == "TempDir" && len(call.Args) == 0 {
		return true
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "filepath" && sel.Sel.Name == "Join" {
		for _, a := range call.Args {
			if !isTempDirShaped(a) {
				return false
			}
		}
		return len(call.Args) > 0
	}
	return false
}

// isClaudeProgram reports a literal program name that runs Claude Code.
func isClaudeProgram(e ast.Expr) bool {
	s, ok := stringLit(e)
	return ok && filepath.Base(s) == "claude"
}

func isolationFindings(files []file) []finding {
	var out []finding
	for _, f := range tests(files) {
		if !inDefaultBuild(f) || isolationAllowlist[f.Rel] != "" {
			continue
		}
		report := func(fn string, n ast.Node, detail string) {
			out = append(out, finding{File: f.Rel, Line: lineOf(f, n.Pos()), Func: fn, Rule: "G8 test-isolation", Detail: detail})
		}
		for _, imp := range f.AST.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == proclivePath {
				report("", imp, "imports proclive, the production kill(pid,0) oracle; inject a fake ProcessOracle")
			}
		}
		osNames := importNames(f, "os")
		userNames := importNames(f, "os/user")
		execNames := importNames(f, "os/exec")
		portsNames := importNames(f, modulePath+"/ports")
		syscallNames := importNames(f, "syscall")
		unixNames := importNames(f, "golang.org/x/sys/unix")
		inPorts := f.Dir == "ports"

		walkWithFuncs(f, func(fn string, n ast.Node) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return
			}
			var pkg, name string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fun.X.(*ast.Ident); ok {
					pkg, name = id.Name, fun.Sel.Name
				}
			case *ast.Ident:
				if inPorts {
					pkg, name = "ports", fun.Name
				}
			}
			isPorts := portsNames[pkg] || (inPorts && pkg == "ports")
			switch {
			case osNames[pkg] && name == "UserHomeDir":
				report(fn, call, "os.UserHomeDir reaches the real home directory")
			case userNames[pkg] && name == "Current":
				report(fn, call, "user.Current reaches the real home directory")
			case osNames[pkg] && (name == "Getenv" || name == "LookupEnv"):
				if len(call.Args) == 1 {
					s, lit := stringLit(call.Args[0])
					if !lit {
						report(fn, call, "os."+name+" of a non-literal name cannot be shown to avoid HOME or CLAUDE_CONFIG_DIR")
					} else if s == "HOME" || s == "CLAUDE_CONFIG_DIR" {
						report(fn, call, "os."+name+"("+strconv.Quote(s)+") reaches the real Claude root")
					}
				}
			case isPorts && name == "RootFromEnv":
				report(fn, call, "ports.RootFromEnv is the real Claude root; hosts only")
			case isPorts && name == "RootAt":
				if len(call.Args) != 1 || !isTempDirShaped(call.Args[0]) {
					report(fn, call, "ports.RootAt with an argument that is neither a literal nor t.TempDir()")
				}
			case execNames[pkg] && name == "LookPath":
				if len(call.Args) == 1 && isClaudeProgram(call.Args[0]) {
					report(fn, call, `exec.LookPath("claude") looks for the real tool`)
				}
			case execNames[pkg] && name == "Command":
				if len(call.Args) >= 1 && isClaudeProgram(call.Args[0]) {
					report(fn, call, "exec.Command runs Claude Code")
				}
			case execNames[pkg] && name == "CommandContext":
				if len(call.Args) >= 2 && isClaudeProgram(call.Args[1]) {
					report(fn, call, "exec.CommandContext runs Claude Code")
				}
			case (syscallNames[pkg] || unixNames[pkg]) && name == "Kill":
				report(fn, call, pkg+".Kill probes or signals a process")
			}
		})
	}
	return out
}

// e2eTagged lists files whose build constraint names the claude_e2e tag.
func e2eTagged(files []file) []string {
	var out []string
	for _, f := range files {
		if f.Build != nil && strings.Contains(f.Build.String(), "claude_e2e") {
			out = append(out, f.Rel)
		}
	}
	return out
}

func TestNoTestReachesTheRealClaudeRootOrAForeignPID(t *testing.T) {
	files := loadModule(t)
	requireScanned(t, tests(files), "ports/ports_test.go", "internal/audit/testisolation_test.go")
	if got := isolationFindings(files); len(got) != 0 {
		t.Fatalf("a test reaches the real Claude root, runs Claude, or probes a pid:\n%s", format(got))
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f.Rel] = true
	}
	for rel := range isolationAllowlist {
		if !have[rel] {
			t.Errorf("isolationAllowlist names %s, which does not exist: a stale exemption", rel)
		}
	}
	if tagged := e2eTagged(files); len(tagged) != 0 {
		t.Errorf("files carrying the claude_e2e build tag: %v; this module has no AQ8 exception", tagged)
	}
}

func TestTheIsolationAuditActuallyDetectsEachShape(t *testing.T) {
	cases := []struct {
		name, src string
	}{
		{"UserHomeDir", `os.UserHomeDir()`},
		{"user.Current", `user.Current()`},
		{"Getenv HOME", `os.Getenv("HOME")`},
		{"LookupEnv CLAUDE_CONFIG_DIR", `os.LookupEnv("CLAUDE_CONFIG_DIR")`},
		{"Getenv non-literal", `os.Getenv(name)`},
		{"RootFromEnv", `ports.RootFromEnv()`},
		{"RootAt non-literal", `ports.RootAt(dir)`},
		{"LookPath claude", `exec.LookPath("claude")`},
		{"Command claude", `exec.Command("claude", "--print")`},
		{"Command abs claude", `exec.Command("/usr/local/bin/claude")`},
		{"CommandContext claude", `exec.CommandContext(ctx, "claude")`},
		{"syscall.Kill", `syscall.Kill(pid, 0)`},
		{"unix.Kill", `unix.Kill(pid, 0)`},
	}
	for _, c := range cases {
		src := `package p

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/MadAppGang/magmux/claudestate/ports"
)

var (
	_ = context.Background
	_ = os.Getpid
	_ = exec.Command
	_ = user.Current
	_ = syscall.Getpid
	_ = unix.Getpid
	_ = ports.RootAt
)

func TestX(t *testing.T) {
	var name, dir string
	var pid int
	var ctx context.Context
	_, _, _, _ = name, dir, pid, ctx
	` + c.src + `
}
`
		got := isolationFindings([]file{snippet(t, "watch/zz_test.go", src)})
		if len(got) != 1 {
			t.Errorf("%s: %d findings, want 1:\n%s", c.name, len(got), format(got))
		}
	}

	// The proclive import alone is a finding.
	imp := snippet(t, "join/zz_test.go", "package join\nimport _ \""+proclivePath+"\"\n")
	if got := isolationFindings([]file{imp}); len(got) != 1 {
		t.Errorf("a proclive import: %d findings, want 1:\n%s", len(got), format(got))
	}

	// Inside package ports the calls are unqualified.
	bare := snippet(t, "ports/zz_test.go", "package ports\nimport \"testing\"\nfunc TestX(t *testing.T) { RootFromEnv(); RootAt(\"x\"); RootAt(t.TempDir()); RootAt(filepath.Join(t.TempDir(), \"a\")) }\n")
	if got := isolationFindings([]file{bare}); len(got) != 1 {
		t.Errorf("bare ports calls: %d findings, want 1 (RootFromEnv):\n%s", len(got), format(got))
	}
}

func TestTheIsolationAuditAllowsWhatReadsNothing(t *testing.T) {
	const fine = `package p

import (
	"os"
	"os/exec"
	"testing"
)

func TestX(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	_ = "/home/someone/.claude/sessions"
	_ = os.Getenv("MAGMUX_AGENTS")
	_ = exec.Command("go", "list")
}
`
	if got := isolationFindings([]file{snippet(t, "claude/zz_test.go", fine)}); len(got) != 0 {
		t.Fatalf("calls that reach nothing were reported:\n%s", format(got))
	}
	// A non-test file is not this guard's business (G2 covers writes there).
	if got := isolationFindings([]file{snippet(t, "ports/zz.go", "package ports\nimport \"os\"\nvar _, _ = os.UserHomeDir()\n")}); len(got) != 0 {
		t.Fatalf("a non-test file was scanned:\n%s", format(got))
	}
	// A file outside the default build is not compiled by `go test`.
	tagged := snippet(t, "claude/zz_test.go", "//go:build manual\n\npackage p\nimport \"os\"\nvar _, _ = os.UserHomeDir()\n")
	if got := isolationFindings([]file{tagged}); len(got) != 0 {
		t.Fatalf("a file under a custom build tag was scanned:\n%s", format(got))
	}
	linuxOnly := snippet(t, "watch/zz_linux_test.go", "//go:build linux\n\npackage p\nimport \"os\"\nvar _, _ = os.UserHomeDir()\n")
	if got := isolationFindings([]file{linuxOnly}); len(got) != 1 {
		t.Fatalf("a linux-only test is in CI's default build and must be scanned on every host: %d findings", len(got))
	}
}

func TestAClaudeE2ETaggedFileIsAFinding(t *testing.T) {
	planted := snippet(t, "claude/zz_e2e_test.go", "//go:build claude_e2e\n\npackage claude\n")
	if got := e2eTagged([]file{planted}); len(got) != 1 {
		t.Fatalf("a claude_e2e file was not seen: %v", got)
	}
}
