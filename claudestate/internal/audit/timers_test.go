package audit

// G4: the timer allowlist. In NON-TEST files, time.After, AfterFunc,
// NewTimer, Sleep and Tick appear only in ports/clock.go (RealClock).
//
// ports/clockfake.go is NOT on the list: FakeClock arms no real timer. Test
// files may use time.After and Sleep as bounded waits, because fsnotify is
// real; G3 still bans a ticker there.

import (
	"go/ast"
	"testing"
)

var timerCalls = map[string]bool{"After": true, "AfterFunc": true, "NewTimer": true, "Sleep": true, "Tick": true}

// timerAllowlist: file -> citation.
var timerAllowlist = map[string]string{
	"ports/clock.go": "architecture §4.3 / G4: RealClock is the one production timer",
}

func timerFindings(files []file) []finding {
	var out []finding
	for _, f := range nonTest(files) {
		if timerAllowlist[f.Rel] != "" {
			continue
		}
		names := importNames(f, "time")
		walkWithFuncs(f, func(fn string, n ast.Node) {
			pkg, name, sel, ok := qualified(n)
			if !ok || !names[pkg] || !timerCalls[name] {
				return
			}
			out = append(out, finding{File: f.Rel, Line: lineOf(f, sel.Pos()), Func: fn, Rule: "G4 timer-allowlist",
				Detail: "time." + name + " outside ports/clock.go: take a ports.Clock"})
		})
	}
	return out
}

func TestNoRealTimerOutsideThePortsClock(t *testing.T) {
	files := loadModule(t)
	requireScanned(t, files, "ports/clock.go", "ports/clockfake.go")
	if got := timerFindings(files); len(got) != 0 {
		t.Fatalf("a real timer outside ports/clock.go:\n%s", format(got))
	}
	for rel := range timerAllowlist {
		requireScanned(t, nonTest(files), rel)
	}
}

func TestTheTimerAuditActuallyDetectsBannedTimers(t *testing.T) {
	inWatch := snippet(t, "watch/zz.go", "package watch\nimport \"time\"\nfunc f() { <-time.After(time.Second) }\n")
	inFake := snippet(t, "ports/clockfake.go", "package ports\nimport \"time\"\nfunc f() { time.Sleep(1) }\n")
	if got := timerFindings([]file{inWatch, inFake}); len(got) != 2 {
		t.Fatalf("want 2 findings (watch/ and ports/clockfake.go), got:\n%s", format(got))
	}
	inTest := snippet(t, "watch/zz_test.go", "package watch\nimport \"time\"\nfunc f() { <-time.After(time.Second) }\n")
	if got := timerFindings([]file{inTest}); len(got) != 0 {
		t.Fatalf("a bounded wait in a test file was reported:\n%s", format(got))
	}
	inClock := snippet(t, "ports/clock.go", "package ports\nimport \"time\"\nfunc f() { _ = time.NewTimer(1) }\n")
	if got := timerFindings([]file{inClock}); len(got) != 0 {
		t.Fatalf("the allowlisted file was reported:\n%s", format(got))
	}
}
