package audit

// G5: no mutex or atomic (NFR3, C3), over every file, tests included.
//
// The allowlist is EMPTY. The one test in today's tree that used sync tests
// host behaviour and moves to magmux at M2. A future entry is a cited line in
// the diff that adds it.

import (
	"go/ast"
	"strconv"
	"testing"
)

var syncBanned = map[string]bool{
	"Mutex": true, "RWMutex": true, "Cond": true, "Locker": true,
	"Once": true, "OnceFunc": true, "OnceValue": true, "OnceValues": true,
}

func syncFindings(files []file) []finding {
	var out []finding
	for _, f := range files {
		for _, imp := range f.AST.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "sync/atomic" {
				out = append(out, finding{File: f.Rel, Line: lineOf(f, imp.Pos()), Rule: "G5 no-sync",
					Detail: "imports sync/atomic; ownership by one goroutine plus channels is the model"})
			}
		}
		names := importNames(f, "sync")
		walkWithFuncs(f, func(fn string, n ast.Node) {
			pkg, name, sel, ok := qualified(n)
			if ok && names[pkg] && syncBanned[name] {
				out = append(out, finding{File: f.Rel, Line: lineOf(f, sel.Pos()), Func: fn, Rule: "G5 no-sync",
					Detail: "sync." + name + ": no lock in the library"})
			}
		})
	}
	return out
}

func TestNoMutexOrAtomicAnywhereInTheModule(t *testing.T) {
	files := loadModule(t)
	requireScanned(t, files, "ports/clockfake.go", "ports/ports_test.go")
	if got := syncFindings(files); len(got) != 0 {
		t.Fatalf("a lock or atomic in the library:\n%s", format(got))
	}
}

func TestTheSyncAuditActuallyDetectsLocks(t *testing.T) {
	const planted = `package p

import (
	"sync"
	"sync/atomic"
)

var mu sync.Mutex
var n atomic.Int64
var wg sync.WaitGroup
`
	got := syncFindings([]file{snippet(t, "pipeline/zz_test.go", planted)})
	if len(got) != 2 {
		t.Fatalf("want 2 findings (sync.Mutex, sync/atomic), got:\n%s", format(got))
	}
}
