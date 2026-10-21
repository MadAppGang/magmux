package audit

// G2: no writes (NFR1). Library code creates, writes, renames or removes
// nothing anywhere (C1). A port of tt's write audit (tt:audit.go Writes,
// tt:audit_test.go:57), keyed on FILE rather than package, and walking _probe
// and _diff.

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// writeCalls are the os-spelled calls that can modify the filesystem.
// os.OpenFile and the syscall openers are decided by their flags instead.
var writeCalls = []string{
	"Create", "CreateTemp", "WriteFile", "Remove", "RemoveAll", "Rename",
	"Mkdir", "MkdirAll", "MkdirTemp", "Chmod", "Chown", "Lchown", "Chtimes",
	"Symlink", "Link", "Truncate",
}

// syscallWriteCalls are the POSIX spellings, for the unix and syscall
// qualifiers. Kept apart from writeCalls so each list says which surface it
// polices.
var syscallWriteCalls = []string{
	"Unlink", "Unlinkat", "Rmdir", "Mkdir", "Mkdirat", "Mknod", "Mknodat",
	"Write", "Writev", "Pwrite", "Pwritev", "Ftruncate", "Truncate",
	"Fchmod", "Fchmodat", "Chmod", "Fchown", "Fchownat", "Chown", "Lchown",
	"Rename", "Renameat", "Renameatx_np", "Renamexat_np",
	"Symlink", "Symlinkat", "Link", "Linkat",
	"Setxattr", "Fsetxattr", "Removexattr", "Fremovexattr",
	"Utimes", "UtimesNano", "Futimes", "Futimesat",
	"Creat", "Mkfifo", "Mkfifoat", "Clonefile", "Clonefileat", "Fclonefileat",
	"Exchangedata", "Undelete", "Copyfile", "Fcopyfile",
}

// openWithFlags: qualified opener -> index of its flag argument.
var openWithFlags = map[string]int{
	"os.OpenFile":  1,
	"unix.Open":    1,
	"unix.Openat":  2,
	"syscall.Open": 1,
}

// pureReadFlags is the only flag vocabulary a read-only open may use. Each
// either is O_RDONLY or only NARROWS what an open may reach.
var pureReadFlags = map[string]bool{
	"O_RDONLY": true, "O_EVTONLY": true, "O_CLOEXEC": true,
	"O_NONBLOCK": true, "O_DIRECTORY": true, "O_NOFOLLOW": true,
}

// rootOpeners hand back an *os.Root, whose methods write. Only a cited writing
// file may hold one.
var rootOpeners = []string{"OpenRoot", "OpenInRoot"}

var fsPackages = map[string]bool{"os": true, "unix": true, "syscall": true, "ioutil": true}

// bindCalls create a file: a unix socket is an inode at a path.
var bindCalls = []string{"Listen", "ListenUnix", "ListenUnixgram", "ListenPacket"}

// writingFile is a cited exception: the one file that may manage a sandbox.
// Inside it only the named path calls are allowed, every other write goes
// through *os.Root methods, and RemoveAll may only remove what MkdirTemp made.
type writingFile struct {
	Rel      string
	Allowed  []string // os.* path calls permitted in this file
	Citation string
}

// writingFiles is the ONE cited entry (architecture §6 G2, C1): the _diff
// sandbox's lifecycle. An entry for a file that does not exist fails
// TestNoCodeWeShipWritesToTheFilesystem, so it cannot go stale.
var writingFiles = []writingFile{
	{
		Rel:     "_diff/sandbox.go",
		Allowed: []string{"MkdirTemp", "OpenRoot", "RemoveAll"},
		Citation: "architecture §6 G2 and §4.7 C1: the _diff sandbox's lifecycle; every other write in it " +
			"goes through an *os.Root opened on its MkdirTemp directory or on testdata/",
	},
}

func writeFindings(files []file, allow []writingFile) []finding {
	exempt := map[string]writingFile{}
	for _, w := range allow {
		if strings.TrimSpace(w.Citation) != "" {
			exempt[w.Rel] = w
		}
	}
	var out []finding
	for _, f := range files {
		if w, ok := exempt[f.Rel]; ok {
			out = append(out, containedWrites(f, w)...)
			continue
		}
		approved := approvedReadOpens(f)
		walkWithFuncs(f, func(fn string, n ast.Node) {
			pkg, name, sel, ok := qualified(n)
			if !ok {
				return
			}
			report := func(detail string) {
				out = append(out, finding{File: f.Rel, Line: lineOf(f, sel.Pos()), Func: fn, Rule: "G2 no-writes", Detail: detail})
			}
			if pkg == "net" {
				for _, b := range bindCalls {
					if name == b {
						report(fmt.Sprintf("net.%s binds a socket, which creates a file", name))
					}
				}
				return
			}
			if !fsPackages[pkg] {
				return
			}
			if _, isOpener := openWithFlags[pkg+"."+name]; isOpener {
				if !approved[sel.Pos()] {
					report(fmt.Sprintf("%s.%s without a provably read-only flag set (allowed: %s)",
						pkg, name, strings.Join(sortedKeys(pureReadFlags), "|")))
				}
				return
			}
			if pkg == "os" {
				for _, r := range rootOpeners {
					if name == r {
						report(fmt.Sprintf("os.%s returns an *os.Root, whose methods write; only a cited writing file may hold one", name))
						return
					}
				}
			}
			lists := [][]string{writeCalls}
			if pkg == "unix" || pkg == "syscall" {
				lists = append(lists, syscallWriteCalls)
			}
			for _, list := range lists {
				for _, banned := range list {
					if name == banned {
						report(fmt.Sprintf("%s.%s writes to the filesystem", pkg, name))
						return
					}
				}
			}
		})
	}
	return out
}

// containedWrites holds a writing file to its citation: only its allowed
// os.* path calls, RemoveAll only of a MkdirTemp result, and nothing from
// unix or syscall that writes.
func containedWrites(f file, w writingFile) []finding {
	allowed := map[string]bool{}
	for _, a := range w.Allowed {
		allowed[a] = true
	}
	var out []finding
	for _, decl := range f.AST.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		fnName := ""
		temps := map[string]bool{} // identifiers assigned from os.MkdirTemp in this function
		if ok {
			fnName = fn.Name.Name
			ast.Inspect(fn, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				if pkg, name, _, ok := qualified(call.Fun); ok && pkg == "os" && name == "MkdirTemp" {
					if id, ok := as.Lhs[0].(*ast.Ident); ok {
						temps[id.Name] = true
					}
				}
				return true
			})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			report := func(pos token.Pos, detail string) {
				out = append(out, finding{File: f.Rel, Line: lineOf(f, pos), Func: fnName, Rule: "G2 contained-write", Detail: detail})
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if pkg, name, sel, ok := qualified(call.Fun); ok && pkg == "os" && name == "RemoveAll" && allowed["RemoveAll"] {
					arg := ""
					if len(call.Args) == 1 {
						if id, ok := call.Args[0].(*ast.Ident); ok {
							arg = id.Name
						}
					}
					if !temps[arg] {
						report(sel.Pos(), "os.RemoveAll of something other than this function's os.MkdirTemp result")
					}
				}
				return true
			}
			pkg, name, sel, ok := qualified(n)
			if !ok || !fsPackages[pkg] {
				return true
			}
			if pkg == "os" && allowed[name] {
				return true
			}
			if _, isOpener := openWithFlags[pkg+"."+name]; isOpener {
				report(sel.Pos(), fmt.Sprintf("%s.%s in the writing file: write through the *os.Root instead", pkg, name))
				return true
			}
			lists := [][]string{writeCalls}
			if pkg == "unix" || pkg == "syscall" {
				lists = append(lists, syscallWriteCalls)
			}
			for _, list := range lists {
				for _, banned := range list {
					if name == banned {
						report(sel.Pos(), fmt.Sprintf("%s.%s takes a path; the writing file writes only through *os.Root methods", pkg, name))
						return true
					}
				}
			}
			return true
		})
	}
	return out
}

// approvedReadOpens finds opener selectors CALLED with a pure-read flag set.
func approvedReadOpens(f file) map[token.Pos]bool {
	approved := map[token.Pos]bool{}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		pkg, name, sel, ok := qualified(call.Fun)
		if !ok {
			return true
		}
		idx, isOpener := openWithFlags[pkg+"."+name]
		if !isOpener || len(call.Args) <= idx {
			return true
		}
		if isPureReadFlagExpr(call.Args[idx]) {
			approved[sel.Pos()] = true
		}
		return true
	})
	return approved
}

func isPureReadFlagExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		return v.Op == token.OR && isPureReadFlagExpr(v.X) && isPureReadFlagExpr(v.Y)
	case *ast.ParenExpr:
		return isPureReadFlagExpr(v.X)
	case *ast.SelectorExpr:
		return pureReadFlags[v.Sel.Name]
	case *ast.Ident:
		return pureReadFlags[v.Name]
	case *ast.BasicLit:
		return v.Kind == token.INT && v.Value == "0"
	default:
		return false
	}
}

func TestNoCodeWeShipWritesToTheFilesystem(t *testing.T) {
	files := nonTest(loadModule(t))
	requireScanned(t, files, "_probe/probe.go", "_diff/sandbox.go", "ports/root.go")
	if got := writeFindings(files, writingFiles); len(got) != 0 {
		t.Fatalf("code in this module writes to the filesystem:\n%s", format(got))
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f.Rel] = true
	}
	for _, w := range writingFiles {
		if !have[w.Rel] {
			t.Errorf("writingFiles names %s, which does not exist: a stale exemption", w.Rel)
		}
		if strings.TrimSpace(w.Citation) == "" {
			t.Errorf("writingFiles entry %s has no citation", w.Rel)
		}
	}
}

func TestWriteAuditActuallyDetectsWrites(t *testing.T) {
	const violating = `package thing

import "os"

var alias = os.WriteFile

func write(p string) {
	os.MkdirAll(p, 0o755)
	os.Remove(p)
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o644)
	_ = f
	r, _ := os.OpenRoot(p)
	_ = r
}
`
	got := writeFindings([]file{snippet(t, "claude/bad.go", violating)}, writingFiles)
	text := format(got)
	for _, w := range []string{"WriteFile", "MkdirAll", "Remove", "OpenFile", "OpenRoot"} {
		if !strings.Contains(text, w) {
			t.Errorf("findings do not mention %s:\n%s", w, text)
		}
	}
	if len(got) != 5 {
		t.Errorf("got %d findings, want 5:\n%s", len(got), text)
	}
}

func TestWriteAuditDetectsThePosixSpellingsToo(t *testing.T) {
	const violating = `package thing

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func write(p string, fd int) {
	unix.Unlink(p)
	unix.Renameat(fd, p, fd, p)
	syscall.Rmdir(p)
	syscall.Write(fd, nil)
	unix.Open(p, unix.O_RDWR, 0)
	syscall.Open(p, syscall.O_CREAT|syscall.O_WRONLY, 0o600)
	net.Listen("unix", p)
}
`
	got := writeFindings([]file{snippet(t, "watch/bad.go", violating)}, writingFiles)
	text := format(got)
	for _, w := range []string{"unix.Unlink", "unix.Renameat", "syscall.Rmdir", "syscall.Write", "unix.Open", "syscall.Open", "net.Listen"} {
		if !strings.Contains(text, w) {
			t.Errorf("findings do not mention %s:\n%s", w, text)
		}
	}
}

func TestWriteAuditPermitsProvablyReadOnlyOpens(t *testing.T) {
	const reading = `package thing

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func read(p string) {
	f, _ := os.Open(p)
	_ = f
	g, _ := os.OpenFile(p, os.O_RDONLY, 0)
	_ = g
	syscall.Open(p, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	unix.Open(p, unix.O_RDONLY|unix.O_EVTONLY|unix.O_CLOEXEC, 0)
	unix.Open(p, 0, 0)
	os.ReadFile(p)
	os.Stat(p)
}
`
	if got := writeFindings([]file{snippet(t, "watch/read.go", reading)}, writingFiles); len(got) != 0 {
		t.Fatalf("read-only opens were reported:\n%s", format(got))
	}
}

// _probe has NO exemption: -truth-json prints to stdout. A planted write in it
// must be a finding, which also proves the loader's "_" handling matters.
func TestAPlantedWriteInTheProbeIsAFinding(t *testing.T) {
	const planted = `package main

import "os"

func save(b []byte) error { return os.WriteFile("/tmp/truth.json", b, 0o644) }
`
	got := writeFindings([]file{snippet(t, "_probe/zz_planted.go", planted)}, writingFiles)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "os.WriteFile") {
		t.Fatalf("a planted os.WriteFile in _probe produced:\n%s", format(got))
	}
}

// The writing-file rule, from both sides, against the shape L6's sandbox will
// have: MkdirTemp, OpenRoot and a RemoveAll of the MkdirTemp result are
// allowed; every other write must go through the *os.Root.
func TestTheWritingFileIsHeldToOSRoot(t *testing.T) {
	sandbox := writingFile{
		Rel:      "_diff/sandbox.go",
		Allowed:  []string{"MkdirTemp", "OpenRoot", "RemoveAll"},
		Citation: "architecture §6 G2: the _diff sandbox's lifecycle",
	}
	const good = `package main

import "os"

func run() error {
	dir, err := os.MkdirTemp("", "diff-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	return r.WriteFile("x", nil, 0o600)
}
`
	if got := writeFindings([]file{snippet(t, sandbox.Rel, good)}, []writingFile{sandbox}); len(got) != 0 {
		t.Fatalf("the sandbox's allowed shape was reported:\n%s", format(got))
	}

	const bad = `package main

import "os"

func run(other string) error {
	dir, _ := os.MkdirTemp("", "diff-")
	_ = dir
	os.RemoveAll(other)
	return os.WriteFile(other, nil, 0o600)
}
`
	got := writeFindings([]file{snippet(t, sandbox.Rel, bad)}, []writingFile{sandbox})
	text := format(got)
	if !strings.Contains(text, "RemoveAll of something other") || !strings.Contains(text, "os.WriteFile") {
		t.Fatalf("the writing file's path writes were not both reported:\n%s", text)
	}

	// An uncited entry grants nothing.
	uncited := sandbox
	uncited.Citation = ""
	if got := writeFindings([]file{snippet(t, sandbox.Rel, good)}, []writingFile{uncited}); len(got) == 0 {
		t.Fatal("an entry with no citation exempted its file")
	}
}
