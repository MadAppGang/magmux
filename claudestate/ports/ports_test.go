package ports

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

var base = time.Unix(1789545200, 0)

func TestFakeClockNowIsFixed(t *testing.T) {
	c := NewFakeClock(base)
	if !c.Now().Equal(base) || !c.Now().Equal(c.Now()) {
		t.Fatalf("Now moved: %v", c.Now())
	}
}

// WRONG: a ledger that counts a timer stopped after it fired as both fired and
// stopped, which makes Outstanding go negative and hides a real leak elsewhere.
func TestFakeClockLedgerCountsEachTimerOnce(t *testing.T) {
	c := NewFakeClock(base)
	a := c.NewTimer(time.Second)
	b := c.NewTimer(2 * time.Second)
	d := c.NewTimer(3 * time.Second)
	if got := c.Created(); got != 3 {
		t.Fatalf("Created = %d, want 3", got)
	}
	if got := c.Outstanding(); got != 3 {
		t.Fatalf("Outstanding = %d, want 3", got)
	}

	fa := <-c.Timers()
	fb := <-c.Timers()
	fd := <-c.Timers()
	if fa.Delay() != time.Second || fb.Delay() != 2*time.Second || fd.Delay() != 3*time.Second {
		t.Fatalf("Timers out of creation order: %v %v %v", fa.Delay(), fb.Delay(), fd.Delay())
	}

	fa.Fire(base.Add(time.Second))
	fa.Fire(base.Add(time.Second)) // twice counts once
	if at := <-a.C(); !at.Equal(base.Add(time.Second)) {
		t.Fatalf("fired at %v", at)
	}
	a.Stop() // stopped after it fired: still counts as fired, once
	a.Stop() // idempotent
	if got := c.Outstanding(); got != 2 {
		t.Fatalf("after one fire (and a later Stop): Outstanding = %d, want 2", got)
	}

	b.Stop()
	select {
	case <-fb.Stopped():
	default:
		t.Fatal("Stopped() is not closed by Stop")
	}
	fb.Fire(base.Add(2 * time.Second)) // no-op after Stop
	select {
	case at := <-b.C():
		t.Fatalf("a stopped timer delivered %v", at)
	default:
	}
	if got := c.Outstanding(); got != 1 {
		t.Fatalf("after a stop: Outstanding = %d, want 1", got)
	}

	fd.Fire(base.Add(3 * time.Second))
	<-d.C()
	if got := c.Outstanding(); got != 0 {
		t.Fatalf("Outstanding = %d, want 0", got)
	}
	if got := c.Created(); got != 3 {
		t.Fatalf("Created = %d, want 3", got)
	}
}

// The clock's whole design is its goroutine rule: timers are created and
// stopped on one goroutine, fired and counted on the test's. Run under -race.
func TestFakeClockIsRaceFreeAcrossTheOwnerAndTheTest(t *testing.T) {
	c := NewFakeClock(base)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			tm := c.NewTimer(time.Millisecond)
			if i%2 == 0 {
				tm.Stop()
			}
		}
	}()
	for i := 0; i < 100; i++ {
		ft := <-c.Timers()
		ft.Fire(base)
	}
	<-done
	if got := c.Created(); got != 100 {
		t.Fatalf("Created = %d, want 100", got)
	}
	if got := c.Outstanding(); got != 0 {
		t.Fatalf("Outstanding = %d, want 0 (every timer fired or stopped)", got)
	}
}

func TestRealClockTimerFires(t *testing.T) {
	tm := RealClock().NewTimer(time.Millisecond)
	defer tm.Stop()
	select {
	case <-tm.C():
	case <-time.After(5 * time.Second):
		t.Fatal("a 1ms real timer did not fire within 5s")
	}
}

func TestUnknownLocatorAnswersUnknownToEverything(t *testing.T) {
	ref, l := UnknownLocator.PaneFor(model.Session{PID: 1})
	if ref != "" || l != model.LivenessUnknown {
		t.Errorf("PaneFor = (%q, %v)", ref, l)
	}
	if l := UnknownLocator.PaneAlive("7"); l != model.LivenessUnknown {
		t.Errorf("PaneAlive = %v", l)
	}
}

func TestTreeAtResolvesTheExistingAncestorAndKeepsTheMissingRemainder(t *testing.T) {
	real := t.TempDir()
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	tr, err := TreeAt(filepath.Join(link, "jobs", "not-yet"))
	if err != nil {
		t.Fatalf("TreeAt of a missing directory: %v", err)
	}
	if tr.Path != filepath.Join(link, "jobs", "not-yet") {
		t.Errorf("Path = %q: it must be the configured spelling, not symlink-resolved", tr.Path)
	}
	if tr.Dir != filepath.Join(resolvedReal, "jobs", "not-yet") {
		t.Errorf("Dir = %q, want the resolved ancestor plus the missing remainder", tr.Dir)
	}
	if tr.FS == nil || tr.IsZero() {
		t.Error("a missing directory still yields a usable Tree")
	}
}

func TestTreeAtRefusesASymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	tr, err := TreeAt(filepath.Join(a, "x"))
	if err == nil {
		t.Fatalf("TreeAt of a symlink loop succeeded: %+v", tr)
	}
	if !tr.IsZero() {
		t.Errorf("an error came with a non-zero Tree: %+v", tr)
	}
}

func TestRootAtNamesTheThreeTreesAndTheBase(t *testing.T) {
	r, err := RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, tr := range map[string]Tree{"sessions": r.Sessions, "jobs": r.Jobs, "projects": r.Projects} {
		if filepath.Base(tr.Path) != name || filepath.Base(tr.Dir) != name {
			t.Errorf("%s tree = %+v", name, tr)
		}
		if filepath.Dir(tr.Dir) != r.Base {
			t.Errorf("%s is not under Base %s: %s", name, r.Base, tr.Dir)
		}
	}
}

// RootFromEnv may not be called by a test (G8); its rule is tested through
// the function it delegates to, with both environment reads injected.
func TestTheEnvironmentRuleIsConfigDirElseHome(t *testing.T) {
	cfg := t.TempDir()
	r, err := rootFrom(cfg, func() (string, error) { return "", errors.New("home must not be asked") })
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(cfg); r.Base != want {
		t.Errorf("with CLAUDE_CONFIG_DIR: Base = %s, want %s", r.Base, want)
	}

	home := t.TempDir()
	r, err = rootFrom("", func() (string, error) { return home, nil })
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(r.Base) != ".claude" {
		t.Errorf("without CLAUDE_CONFIG_DIR: Base = %s, want <home>/.claude", r.Base)
	}

	if _, err := rootFrom("", func() (string, error) { return "", errors.New("no home") }); err == nil {
		t.Error("no config dir and no home must be an error")
	}
	if _, err := rootFrom("", func() (string, error) { return "", nil }); err == nil {
		t.Error("an empty home must be an error, not the current directory")
	}
}
