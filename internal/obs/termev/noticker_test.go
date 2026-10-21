package termev

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// obsRoot is internal/, two directories up from this package.
const obsRoot = "../.."

// TestNoTickerAnywhereInTheObservationLayer proves the no-polling decision by
// reading the source, because it cannot be proven by behaviour.
//
// A WRONG IMPLEMENTATION built on a ticker passes every other test in this
// package. Feed a ticker's channel the same instants the fake clock feeds a
// one-shot timer and the cascade emits exactly the same events — the state
// machine cannot tell where a wake-up came from, and that is by design. What
// differs is what happens when NOTHING is going on: a ticker wakes once per
// period per pane forever, so a magmux with twenty idle panes burns CPU
// around the clock, and the dirty-flag rendering the whole terminal is built
// on stops being the thing that decides whether magmux costs anything. There
// is no assertion over an idle process that catches that reliably on a shared
// CI machine. There is an assertion over the source, and this is it.
//
// It scans the AST rather than grepping, so that this file's own prose about
// tickers, and any future comment explaining why there are none, cannot fail
// it — only a real reference to the time package's ticker API can.
func TestNoTickerAnywhereInTheObservationLayer(t *testing.T) {
	banned := map[string]string{
		"Tick":      "time.Tick leaks a ticker that can never be stopped",
		"NewTicker": "time.NewTicker is a poll loop by another name",
		"Ticker":    "a time.Ticker in the observation layer is a poll loop",
	}

	root, err := filepath.Abs(obsRoot)
	if err != nil {
		t.Fatalf("resolving %s: %v", obsRoot, err)
	}
	if _, err := os.Stat(filepath.Join(root, "obs")); err != nil {
		t.Fatalf("%s does not look like internal/: %v", root, err)
	}

	var scanned int
	walkErr := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if e.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		// Only a file that imports "time" can name time.Ticker at all, and a
		// dot-import or a rename would defeat a naive selector check — so both
		// are resolved here rather than assumed away.
		names := timeImportNames(file)
		if len(names) == 0 {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || !names[ident.Name] {
				return true
			}
			if why, bad := banned[sel.Sel.Name]; bad {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d: %s.%s — %s. The observation layer uses one-shot timers only.",
					rel, fset.Position(sel.Pos()).Line, ident.Name, sel.Sel.Name, why)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", root, walkErr)
	}
	if scanned == 0 {
		// A walk that found nothing would pass silently and prove nothing at
		// all, which is the one way a source-inspection test can lie.
		t.Fatalf("scanned no Go files under %s", root)
	}
	t.Logf("scanned %d Go files under internal/", scanned)
}

// timeImportNames returns every identifier under which this file refers to the
// time package. Usually just "time"; a renamed import is included, and a dot
// import is reported as the empty name, which no selector can match — that is
// handled by refusing to resolve it, so a dot-imported time package would show
// up as a compile-level oddity rather than as a silent pass.
func timeImportNames(f *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range f.Imports {
		if imp.Path == nil || imp.Path.Value != `"time"` {
			continue
		}
		if imp.Name == nil {
			out["time"] = true
			continue
		}
		out[imp.Name.Name] = true
	}
	return out
}

// TestTheTickerDetectorCatchesTheShapesItMustCatch proves the test above is
// not vacuous.
//
// A WRONG DETECTOR — one that grepped for the literal "NewTicker", or one that
// checked only the selector name without resolving the package — would pass
// the real scan today and keep passing after somebody wrote `import clock
// "time"` and called `clock.NewTicker`. This runs the same AST logic against
// source that deliberately contains each shape.
func TestTheTickerDetectorCatchesTheShapesItMustCatch(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "plain time.NewTicker",
			src:  "package p\nimport \"time\"\nfunc f() { _ = time.NewTicker(time.Second) }\n",
			want: true,
		},
		{
			name: "time.Tick",
			src:  "package p\nimport \"time\"\nfunc f() { _ = time.Tick(time.Second) }\n",
			want: true,
		},
		{
			name: "a renamed import",
			src:  "package p\nimport clock \"time\"\nfunc f() { _ = clock.NewTicker(clock.Second) }\n",
			want: true,
		},
		{
			name: "a ticker in a struct field",
			src:  "package p\nimport \"time\"\ntype s struct { t *time.Ticker }\n",
			want: true,
		},
		{
			name: "the one-shot timer this package actually uses",
			src:  "package p\nimport \"time\"\nfunc f() { _ = time.NewTimer(time.Second) }\n",
			want: false,
		},
		{
			name: "a comment and a string mentioning a ticker",
			src:  "package p\n// not a time.NewTicker\nconst s = \"time.NewTicker\"\n",
			want: false,
		},
		{
			name: "a local variable that happens to be called time",
			src:  "package p\nfunc f() { var time struct{ NewTicker int }; _ = time.NewTicker }\n",
			want: false,
		},
	}

	banned := map[string]bool{"Tick": true, "NewTicker": true, "Ticker": true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parsing the case source: %v", err)
			}
			names := timeImportNames(file)
			found := false
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || !names[ident.Name] {
					return true
				}
				if banned[sel.Sel.Name] {
					found = true
				}
				return true
			})
			if found != tc.want {
				t.Fatalf("detector reported %v, want %v", found, tc.want)
			}
		})
	}
}
