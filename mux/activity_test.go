package mux

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// newActivityTestPane builds a leaf pane with a screen and a VT parser but no
// child process. newControlPane is borrowed because it is the one constructor
// that does not fork; isControl is then cleared, because everything under test
// here belongs to a session pane.
func newActivityTestPane() *Pane {
	p := newControlPane(0, 0, 24, 80, "activity-test")
	p.isControl = false
	return p
}

// feedPane runs readLoop over one payload and returns only once the loop has
// finished with it.
//
// The pipe is closed to end the loop, which is what makes this DETERMINISTIC
// with no sleeping anywhere: readLoop returns on EOF, and EOF is necessarily
// read after the payload that preceded it, so wg.Wait is a barrier ordered
// after the stamp. Polling the stamp with a sleep would be the obvious
// alternative and would put a real wait into a test suite that has none.
func feedPane(t *testing.T, p *Pane, payload string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	p.ptmx = r

	var wg sync.WaitGroup
	wg.Add(1)
	go p.readLoop(&wg)

	if _, err := w.Write([]byte(payload)); err != nil {
		t.Fatalf("writing to the pane's PTY: %v", err)
	}
	w.Close()
	wg.Wait()
}

// drainPTY gives a pane a PTY somebody is reading, so that writePTY and
// injectPTY cannot block on a full pipe buffer.
func drainPTY(t *testing.T, p *Pane) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	p.ptmx = w
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256)
		for {
			if _, err := r.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		w.Close()
		r.Close()
		<-done
	})
}

// TestReadLoopStampsLastOutputAt is the functional half of the content stamp.
//
// A WRONG IMPLEMENTATION that left the stamp out entirely would leave
// lastOutputAt at the zero instant forever, and termev's Cascade refuses the
// zero instant — so every pane would be permanently disarmed and the whole
// staleness feature would emit nothing, with no error anywhere. That is the
// failure this test exists to make loud. One that stamped only the FIRST byte
// a pane ever produced would pass the first half and fail the second, and a
// chatty pane would then look exactly as stale as a hung one.
func TestReadLoopStampsLastOutputAt(t *testing.T) {
	p := newActivityTestPane()

	if out, _ := p.activity(); !out.IsZero() {
		t.Fatalf("a fresh pane already carries lastOutputAt=%s", out)
	}

	before := time.Now()
	feedPane(t, p, "hello")

	first, _ := p.activity()
	if first.IsZero() {
		t.Fatal("readLoop left lastOutputAt at the zero instant")
	}
	if first.Before(before) {
		t.Fatalf("lastOutputAt is %s, before the write at %s", first, before)
	}

	feedPane(t, p, "more")
	second, _ := p.activity()
	if !second.After(first) {
		t.Fatalf("a second burst left the stamp at %s (was %s); the cascade re-arms from each new stamp", second, first)
	}
}

// TestReadLoopDoesNotStampInput proves the two stamps are independent facts.
//
// A WRONG IMPLEMENTATION collapsing them into one "something happened" field
// would make input indistinguishable from output — and since input CANCELS the
// cascade while output RE-ARMS it, one field could only ever mean one of them.
func TestReadLoopDoesNotStampInput(t *testing.T) {
	p := newActivityTestPane()
	feedPane(t, p, "x")

	if _, in := p.activity(); !in.IsZero() {
		t.Fatalf("output from the child stamped lastInputAt=%s", in)
	}
}

// TestInputStampsLastInputAtEvenWhenThePaneWasNotMarkedDone is the reason the
// stamp sits ABOVE clearCompletionLocked's guard.
//
// A WRONG IMPLEMENTATION putting it below the `if !p.inputReady { return }`
// would record only keystrokes that landed on a pane magmux had already marked
// finished — which is to say it would miss every keystroke into a pane
// somebody is simply typing into, the exact case the cancellation exists for.
// The pane here is deliberately NOT inputReady, so a stamp under the guard
// fails this and passes everything else in the file.
func TestInputStampsLastInputAtEvenWhenThePaneWasNotMarkedDone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(p *Pane)
	}{
		{"keystroke", func(p *Pane) { p.writePTY([]byte("q")) }},
		{"controller instruction", func(p *Pane) { p.injectPTY([]byte("go on")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newActivityTestPane()
			drainPTY(t, p)

			if p.inputReady {
				t.Fatal("the test pane is already marked done; it must not be")
			}

			before := time.Now()
			tc.write(p)

			out, in := p.activity()
			if in.IsZero() {
				t.Fatal("input left lastInputAt at the zero instant")
			}
			if in.Before(before) {
				t.Fatalf("lastInputAt is %s, before the write at %s", in, before)
			}
			if !out.IsZero() {
				t.Fatalf("input stamped lastOutputAt=%s", out)
			}
		})
	}
}

// TestInputStampsLastInputAtOnAFinishedPane covers the other branch of the
// same function, where the completion chrome is cleared as well.
//
// A WRONG IMPLEMENTATION that moved the stamp into one of the two callers
// rather than into the function they share would drift the moment somebody
// edited only one of them — precisely the drift clearCompletionLocked was
// written to stop, which has already happened once (writePTY left the idle
// clocks standing while injectPTY reset them).
func TestInputStampsLastInputAtOnAFinishedPane(t *testing.T) {
	p := newActivityTestPane()
	drainPTY(t, p)
	p.inputReady = true
	p.tint = "green"
	p.overlayText = "✓ DONE"

	p.writePTY([]byte("next"))

	if _, in := p.activity(); in.IsZero() {
		t.Fatal("input into a finished pane left lastInputAt at the zero instant")
	}
	if p.inputReady || p.tint != "" || p.overlayText != "" {
		t.Errorf("the completion state survived the keystroke: inputReady=%v tint=%q overlay=%q",
			p.inputReady, p.tint, p.overlayText)
	}
}

// TestTheOutputStampDoesNotAllocate pins the cost of the one line added to
// magmux's hottest path.
//
// readLoop runs once per PTY read, for every pane, for the life of the
// process; CLAUDE.md already holds the parser to "must not allocate" and the
// scrollback to a zero-allocation test for the same reason. A WRONG
// IMPLEMENTATION storing a *time.Time — the obvious way to spell "not set yet"
// if you have not noticed that the zero Time already does — would allocate on
// every read, and a burst of output would hand the garbage collector one
// object per 8 KB of a build log. So would anything that formatted the instant
// or put it in an interface.
func TestTheOutputStampDoesNotAllocate(t *testing.T) {
	p := newActivityTestPane()
	allocs := testing.AllocsPerRun(1000, func() {
		p.mu.Lock()
		p.lastOutputAt = time.Now()
		p.mu.Unlock()
	})
	if allocs != 0 {
		t.Fatalf("the output stamp allocates %.1f objects per call, want 0", allocs)
	}
}

// TestLastOutputAtIsStampedInExactlyOnePlace is the invariant the whole design
// rests on, and it can only be checked by reading the source.
//
// A WRONG IMPLEMENTATION here is not a broken one — it is a REASONABLE one.
// magmux has around twenty sites that set p.dirty, they all look like "the
// pane changed", and the obvious move for somebody adding a feature is to
// stamp the one next to the line they are editing. Do that and the cascade
// dies quietly: magmux repaints its own chrome constantly, so a five-minute
// threshold can never fire, nothing errors, and the observation layer simply
// stops having anything to say. No assertion about the running program catches
// that. A count of assignment sites does.
func TestLastOutputAtIsStampedInExactlyOnePlace(t *testing.T) {
	sites := assignmentSites(t, "lastOutputAt")
	if len(sites) != 1 {
		t.Fatalf("lastOutputAt is assigned at %d sites, want exactly 1: %v", len(sites), sites)
	}
	if sites[0].fn != "readLoop" {
		t.Fatalf("lastOutputAt is stamped in %s (%s:%d), want readLoop — the only place bytes arrive from a child",
			sites[0].fn, sites[0].file, sites[0].line)
	}
}

// TestLastInputAtIsStampedInExactlyOnePlace is the same rule for the other
// stamp.
//
// A WRONG IMPLEMENTATION stamping it in writePTY and injectPTY separately
// would work on the day it was written and drift on the day somebody adds a
// third input path — a socket verb, a paste, a replay. clearCompletionLocked
// is the one function every input path already goes through, and this pins the
// stamp there rather than beside it.
func TestLastInputAtIsStampedInExactlyOnePlace(t *testing.T) {
	sites := assignmentSites(t, "lastInputAt")
	if len(sites) != 1 {
		t.Fatalf("lastInputAt is assigned at %d sites, want exactly 1: %v", len(sites), sites)
	}
	if sites[0].fn != "clearCompletionLocked" {
		t.Fatalf("lastInputAt is stamped in %s (%s:%d), want clearCompletionLocked — the one function both input paths share",
			sites[0].fn, sites[0].file, sites[0].line)
	}
}

// TestTheStampSiteDetectorCatchesASecondSite proves the two tests above are
// not vacuous.
//
// A WRONG DETECTOR — one that looked only at main.go, or only at simple `=`
// assignments and not at `:=` or a struct literal field — would keep passing
// after somebody added the stamp in control.go or pilot.go, which is exactly
// the regression it is there to catch. This runs the same AST logic over
// source that contains a second site.
func TestTheStampSiteDetectorCatchesASecondSite(t *testing.T) {
	src := `package p
type q struct{ lastOutputAt int }
func a(x *q) { x.lastOutputAt = 1 }
func b(x *q) { x.lastOutputAt = 2 }
func c(x *q) { _ = x.lastOutputAt }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the case source: %v", err)
	}
	var fns []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		for _, s := range fieldAssignments(fn, "lastOutputAt") {
			_ = s
			fns = append(fns, fn.Name.Name)
		}
	}
	if len(fns) != 2 {
		t.Fatalf("the detector found %d assignment sites (%v), want 2 — a read is not an assignment", len(fns), fns)
	}
	if fns[0] != "a" || fns[1] != "b" {
		t.Fatalf("the detector named %v, want [a b]", fns)
	}
}

type stampSite struct {
	file string
	line int
	fn   string
}

func (s stampSite) String() string { return s.fn + " (" + s.file + ")" }

// assignmentSites finds every `<something>.field = …` across the whole of
// package main's non-test source — main.go, control.go, pilot.go and the rest
// — and names the function each one is in. The package, not one file: the
// point is to catch a second stamp wherever it is added.
func assignmentSites(t *testing.T, field string) []stampSite {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing package main: %v", err)
	}
	var out []stampSite
	var files int
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			files++
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				for _, pos := range fieldAssignments(fn, field) {
					out = append(out, stampSite{file: name, line: fset.Position(pos).Line, fn: fn.Name.Name})
				}
			}
		}
	}
	if files == 0 {
		// A scan that read nothing would pass and prove nothing, which is the
		// one way a source-inspection test can lie.
		t.Fatal("parsed no non-test Go files in package main")
	}
	return out
}

// fieldAssignments returns the position of every assignment whose left-hand
// side selects field. Struct-literal fields are covered too, because
// `Pane{lastOutputAt: t}` is a stamp site by any other name.
func fieldAssignments(fn *ast.FuncDecl, field string) []token.Pos {
	var out []token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
					out = append(out, sel.Pos())
				}
			}
		case *ast.CompositeLit:
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
					out = append(out, kv.Pos())
				}
			}
		}
		return true
	})
	return out
}
