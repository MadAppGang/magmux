package audit

// G3: no ticker (NFR2), over the WHOLE module, test files included. From
// S:internal/obs/termev/noticker_test.go.
//
// A wrong implementation built on a ticker passes every behavioural test: feed
// its channel the instants a fake clock would and the state machine cannot
// tell where a wake-up came from. What differs is what happens when NOTHING is
// going on, and no assertion over an idle process catches that reliably on a
// shared CI machine. An assertion over the source does.

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

var tickerBanned = map[string]string{
	"Tick":      "time.Tick leaks a ticker that can never be stopped",
	"NewTicker": "time.NewTicker is a poll loop by another name",
	"Ticker":    "a time.Ticker is a poll loop",
}

func tickerFindings(files []file) []finding {
	var out []finding
	for _, f := range files {
		names := importNames(f, "time")
		if len(names) == 0 {
			continue
		}
		walkWithFuncs(f, func(fn string, n ast.Node) {
			pkg, name, sel, ok := qualified(n)
			if !ok || !names[pkg] {
				return
			}
			if why, bad := tickerBanned[name]; bad {
				out = append(out, finding{File: f.Rel, Line: lineOf(f, sel.Pos()), Func: fn, Rule: "G3 no-ticker",
					Detail: fmt.Sprintf("%s.%s: %s; every timer here is one-shot", pkg, name, why)})
			}
		})
		if names["."] {
			out = append(out, finding{File: f.Rel, Line: 1, Rule: "G3 no-ticker",
				Detail: `a dot-import of "time" cannot be audited`})
		}
	}
	return out
}

func TestNoTickerAnywhereInTheModule(t *testing.T) {
	files := loadModule(t)
	requireScanned(t, files, "_probe/probe.go", "ports/clock.go", "ports/ports_test.go")
	if got := tickerFindings(files); len(got) != 0 {
		t.Fatalf("a ticker in the library:\n%s", format(got))
	}
	t.Logf("scanned %d Go files, tests included", len(files))
}

func TestTheTickerDetectorCatchesTheShapesItMustCatch(t *testing.T) {
	cases := []struct {
		name, src string
		want      int
	}{
		{"time.NewTicker", "package p\nimport \"time\"\nfunc f() { _ = time.NewTicker(time.Second) }\n", 1},
		{"time.Tick", "package p\nimport \"time\"\nfunc f() { _ = time.Tick(time.Second) }\n", 1},
		{"a renamed import", "package p\nimport clock \"time\"\nfunc f() { _ = clock.NewTicker(clock.Second) }\n", 1},
		{"a Ticker type", "package p\nimport \"time\"\nvar t *time.Ticker\n", 1},
		{"in a test file", "package p\nimport (\"testing\"; \"time\")\nfunc TestX(t *testing.T) { _ = time.NewTicker(1) }\n", 1},
		{"prose only", "package p\n// time.NewTicker is banned here\nvar s = \"time.NewTicker\"\n", 0},
		{"a one-shot timer", "package p\nimport \"time\"\nfunc f() { _ = time.NewTimer(time.Second) }\n", 0},
	}
	for _, c := range cases {
		rel := "watch/x.go"
		if strings.Contains(c.name, "test") {
			rel = "watch/x_test.go"
		}
		got := tickerFindings([]file{snippet(t, rel, c.src)})
		if len(got) != c.want {
			t.Errorf("%s: %d findings, want %d:\n%s", c.name, len(got), c.want, format(got))
		}
	}
}
