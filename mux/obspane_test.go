package mux

// The magmux side of the observation layer: the pane port and the pipeline's
// lifecycle.
//
// NOTHING here reads ~/.claude, probes a live pid or speaks to tmux. The
// process tree is staged as a map, the panes are PTY-less control panes
// carrying a pid nobody ever signals, and the one test that starts a real
// pipeline points it at a temporary directory.

import (
	"errors"
	"github.com/MadAppGang/magmux/proc"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// givePID makes a PTY-less test pane look like one with a child, without
// forking anything. os.Process is a plain struct with an exported Pid, so the
// locator reads exactly the field it would read in production and no process
// with that number need exist — nothing here ever signals it.
func givePID(t *testing.T, p *Pane, pid int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cmd = &exec.Cmd{Process: &os.Process{Pid: pid}}
}

// tree stages a process tree: child -> parent. A pid that is absent from the
// map has no readable parent, which is what proc.PPIDOf reports for a process that
// has gone or that we may not see.
func tree(links map[int]int) func(int) (int, error) {
	return func(pid int) (int, error) {
		if parent, ok := links[pid]; ok {
			return parent, nil
		}
		return 0, errors.New("ppidOf: staged tree ends here")
	}
}

// ── PaneFor: the ancestry walk ──────────────────────────────────────────────

// TestPaneForWalksUpThroughTheShell is the mechanism, and it is forced rather
// than chosen: magmux spawns a SHELL and the shell spawns the tool, so the
// pane's pid and the session record's pid are never equal.
//
// A WRONG IMPLEMENTATION that compared the two directly passes no test here
// and looks perfectly reasonable in review — and in production it matches
// nothing at all, which presents as "no session is running inside magmux"
// rather than as an error.
func TestPaneForWalksUpThroughTheShell(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	givePID(t, m.paneByID(0), 400) // the shell magmux spawned
	givePID(t, m.paneByID(1), 500)

	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{
		// claude -> a wrapper -> the pane's shell -> magmux -> the login shell
		900: 850,
		850: 400,
		400: 300,
		300: 1,
	})}

	id, found := loc.PaneFor(900)
	if found != obs.LivenessYes {
		t.Fatalf("PaneFor(900) reported %v; the tool's pid is never the pane's", found)
	}
	if id != 0 {
		t.Fatalf("PaneFor(900) resolved pane %d, want 0", id)
	}

	// And the pane's own pid resolves too: a tool started without a shell in
	// between is the same question with a shorter walk.
	if id, found := loc.PaneFor(500); found != obs.LivenessYes || id != 1 {
		t.Fatalf("PaneFor(500) resolved (%d, %v), want (1, yes)", id, found)
	}
}

// TestPaneForReachesInitAndReportsAMiss — a determined no. The walk completed
// and nothing on it was ours, which is the honest answer for the sessions
// belonging to some other magmux, or to no magmux at all.
func TestPaneForReachesInitAndReportsAMiss(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)

	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{900: 800, 800: 1})}
	if id, found := loc.PaneFor(900); found != obs.LivenessNo {
		t.Fatalf("a walk that reached init reported (%d, %v), want a determined no", id, found)
	}

	// The same answer when the tree tops out at 0 rather than at 1.
	loc = &muxPaneLocator{m: m, ppid: tree(map[int]int{900: 800, 800: 0})}
	if _, found := loc.PaneFor(900); found != obs.LivenessNo {
		t.Fatalf("a walk that reached the top of the tree reported %v, want a determined no", found)
	}
}

// TestAnUnreadableLinkIsUndeterminedAndNeverNo pins the tri-state where it
// matters most.
//
// proc.PPIDOf's own doc comment says an error means "the chain ends here". The pane
// may be further up and unreadable — a restricted /proc, a container, a
// transient sysctl failure — so the answer is "we could not tell". Reporting
// "no" would hand a consumer acting on the gate a session it believes has no
// pane, which is the exact failure obs.Liveness exists to prevent.
func TestAnUnreadableLinkIsUndeterminedAndNeverNo(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)

	// 900's parent is readable; 800's is not.
	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{900: 800})}
	if id, found := loc.PaneFor(900); found != obs.LivenessUnknown {
		t.Fatalf("an unreadable link reported (%d, %v), want undetermined", id, found)
	}
}

// TestTheWalkIsBounded — a cycle, and a tree deeper than the bound. Neither
// may hang the caller, and neither may be answered "no": a walk that ran out
// of hops established nothing at all.
func TestTheWalkIsBounded(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)

	t.Run("a cycle", func(t *testing.T) {
		loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{900: 800, 800: 700, 700: 900})}
		done := make(chan obs.Liveness, 1)
		go func() {
			_, found := loc.PaneFor(900)
			done <- found
		}()
		select {
		case found := <-done:
			if found != obs.LivenessUnknown {
				t.Fatalf("a cycle reported %v, want undetermined", found)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the walk did not terminate on a cycle")
		}
	})

	t.Run("deeper than the bound", func(t *testing.T) {
		links := map[int]int{}
		for i := 0; i < maxAncestryHops*3; i++ {
			links[1000+i] = 1001 + i
		}
		links[1000+maxAncestryHops*3] = 400 // the pane, well past the bound
		loc := &muxPaneLocator{m: m, ppid: tree(links)}
		if _, found := loc.PaneFor(1000); found != obs.LivenessUnknown {
			t.Fatalf("a tree deeper than %d hops reported %v, want undetermined",
				maxAncestryHops, found)
		}
	})

	t.Run("a short bound still finds a near pane", func(t *testing.T) {
		loc := &muxPaneLocator{m: m, hops: 2, ppid: tree(map[int]int{900: 400})}
		if id, found := loc.PaneFor(900); found != obs.LivenessYes || id != 0 {
			t.Fatalf("resolved (%d, %v) within the bound, want (0, yes)", id, found)
		}
	})
}

// TestPaneForRefusesANonPID — 0 and negatives are not pids, and proc.PPIDOf would
// reject them anyway. Undetermined rather than no: nothing was established.
func TestPaneForRefusesANonPID(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)
	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{})}
	for _, pid := range []int{0, -1} {
		if _, found := loc.PaneFor(pid); found != obs.LivenessUnknown {
			t.Errorf("PaneFor(%d) reported %v, want undetermined", pid, found)
		}
	}
}

// TestAMagmuxWithNoProcessesAnswersWithoutWalking — the control panel has no
// process, so a magmux showing only its panel owns no session. Determined, and
// worth answering without a syscall: on a machine with several magmuxes, this
// is the answer for most sessions.
func TestAMagmuxWithNoProcessesAnswersWithoutWalking(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...) // PTY-less, so no cmd and no pid
	walked := false
	loc := &muxPaneLocator{m: m, ppid: func(int) (int, error) {
		walked = true
		return 0, errors.New("should not be reached")
	}}
	if _, found := loc.PaneFor(900); found != obs.LivenessNo {
		t.Errorf("a magmux with no child processes reported %v, want a determined no", found)
	}
	if walked {
		t.Error("the process tree was walked even though no pane has a process")
	}
}

// TestPaneForWalksWithTreeMuReleased is CLAUDE.md's first lock rule, measured.
//
// proc.PPIDOf is a sysctl on Darwin and a /proc read on Linux — it is I/O, and the
// rule has no exception. controller.Poll was documented as one and was the
// bug. A locator that snapshotted the pane pids and walked under the lock
// passes every other test in this file; what it does instead is put every
// keystroke, every SIGWINCH and every socket verb behind a process-tree walk,
// and Go's RWMutex puts later readers behind a waiting writer, so the render
// loop and the input loop then take turns making each other wait.
func TestPaneForWalksWithTreeMuReleased(t *testing.T) {
	defer deadlineWatchdog(t, 30*time.Second)()

	const walkFor = 400 * time.Millisecond
	m := newTestMux(t, ctrlPanes(2)...)
	givePID(t, m.paneByID(0), 400)

	inWalk := make(chan struct{}, 1)
	loc := &muxPaneLocator{m: m, ppid: func(pid int) (int, error) {
		select {
		case inWalk <- struct{}{}:
		default:
		}
		time.Sleep(walkFor)
		return 400, nil
	}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		loc.PaneFor(900)
	}()

	select {
	case <-inWalk:
	case <-time.After(5 * time.Second):
		t.Fatal("the walk never started; the test setup is wrong, not the lock")
	}

	if d := lockDelay(m); d > walkFor/4 {
		t.Errorf("treeMu.Lock waited %v while a %v ancestry walk was in flight — "+
			"the walk is happening under treeMu", d, walkFor)
	}
	<-done
}

// TestTheDefaultLookupIsTheRealPPIDOf is the one test here that touches a real
// process tree, and it touches exactly two entries of it: this test binary and
// a child it forked itself. No pid belonging to anybody else is read, nothing
// is signalled except the child's own kill on the way out, and ~/.claude is
// nowhere near it.
//
// It is here because the staged trees above cannot cover the one thing that
// matters most: that the production path is wired to proc.PPIDOf at all. A WRONG
// IMPLEMENTATION that left the lookup nil, or pointed it at a stub, passes
// every other test in this file and resolves nothing whatsoever in production.
//
// The shape is production's exactly: the pane holds one pid, the session runs
// as a DESCENDANT of it, and only the walk can connect the two.
func TestTheDefaultLookupIsTheRealPPIDOf(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot fork a child here (%v); there is no real tree to walk", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	if _, err := proc.PPIDOf(child.Process.Pid); err != nil {
		t.Skipf("this platform's ancestry is unreadable (%v); there is nothing to assert against", err)
	}

	// The test binary stands in for the shell magmux spawns into a pane, and
	// the child stands in for the tool the shell started.
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), os.Getpid())

	loc := newPaneLocator(m) // no injected lookup: the production path
	id, found := loc.PaneFor(child.Process.Pid)
	if found != obs.LivenessYes || id != 0 {
		t.Fatalf("PaneFor(%d) resolved (%d, %v) against a pane holding this process's pid %d, "+
			"want (0, yes)", child.Process.Pid, id, found, os.Getpid())
	}
}

// ── PaneAlive: the three pane states ────────────────────────────────────────

// TestPaneAliveAcrossMagmuxThreePaneStates is the gate's whole definition.
//
// CLAUDE.md is explicit that dead, closed and hidden are three states and not
// degrees of one, and only ONE of them means the pane is gone. A WRONG
// IMPLEMENTATION spelling this `!p.dead` — which is the obvious one, and which
// the brief calls out — reports a finished agent's pane as vanished, and a
// consumer filtering on the gate then hides the session at the exact moment
// its ✓ DONE tombstone is what the user is looking at.
func TestPaneAliveAcrossMagmuxThreePaneStates(t *testing.T) {
	m := newTestMux(t, ctrlPanes(3)...)
	loc := newPaneLocator(m)

	t.Run("a live pane", func(t *testing.T) {
		if got := loc.PaneAlive(0); got != obs.LivenessYes {
			t.Fatalf("PaneAlive(0) is %v for a live pane, want yes", got)
		}
	})

	t.Run("dead is alive: the process went, the pane did not", func(t *testing.T) {
		p := m.paneByID(1)
		p.mu.Lock()
		p.dead = true
		p.exitCode = 1
		p.mu.Unlock()
		if got := loc.PaneAlive(1); got != obs.LivenessYes {
			t.Fatalf("PaneAlive(1) is %v for a dead-but-present pane, want yes — "+
				"the process is the OTHER gate", got)
		}
	})

	t.Run("hidden is alive: it is complete and merely not in the tree", func(t *testing.T) {
		panel := m.installHiddenPanel()
		m.treeMu.RLock()
		hidden := panel.hidden
		id := panel.id
		m.treeMu.RUnlock()
		if !hidden {
			t.Fatal("installHiddenPanel produced a pane that is not hidden")
		}
		if got := loc.PaneAlive(id); got != obs.LivenessYes {
			t.Fatalf("PaneAlive(%d) is %v for a hidden pane, want yes — hidden is a fact "+
				"about magmux's chrome, not about the session", id, got)
		}
	})

	t.Run("closed is gone, and permanently", func(t *testing.T) {
		if err := m.ClosePane(2, false); err != nil {
			t.Fatalf("ClosePane(2): %v", err)
		}
		if got := loc.PaneAlive(2); got != obs.LivenessNo {
			t.Fatalf("PaneAlive(2) is %v for a tombstoned pane, want no", got)
		}
	})
}

// TestPaneAliveOnIdsOutsideTheTable — an id past the end was never allocated,
// which is a determined no; a negative id is not an address at all, and
// answering "no" to it would claim we had looked.
func TestPaneAliveOnIdsOutsideTheTable(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	loc := newPaneLocator(m)

	if got := loc.PaneAlive(99); got != obs.LivenessNo {
		t.Errorf("PaneAlive(99) is %v for an id past the end of the table, want no", got)
	}
	if got := loc.PaneAlive(-1); got != obs.LivenessUnknown {
		t.Errorf("PaneAlive(-1) is %v, want undetermined", got)
	}
}

// ── lifecycle ───────────────────────────────────────────────────────────────

// TestObservationIsOffUnlessAskedFor pins the switch, and the reason for it:
// this repository's own suite spawns the real magmux binary in six files with
// the developer's environment, and an unconditional pipeline would have every
// one of those reading a live session registry and signal-probing the pids in
// it.
func TestObservationIsOffUnlessAskedFor(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"0", false}, {"off", false}, {"no", false}, {"false", false},
		{"OFF", false}, {" 0 ", false},
		{"1", true}, {"true", true}, {"on", true}, {"yes", true}, {"anything", true},
	} {
		if got := obsEnabled(tc.value); got != tc.want {
			t.Errorf("obsEnabled(%q) is %v, want %v", tc.value, got, tc.want)
		}
	}

	// And with the variable unset, the startup hook resolves no root and
	// touches no directory at all.
	//
	// The nil oracle is the point of the argument and not noise: this is the
	// one test that reaches the production startup hook, and a nil here means
	// it cannot signal-probe a pid even if the switch were somehow on. Only
	// main() names obsOracle. See TestNoTestCanReachTheSignalOracle.
	t.Setenv(obsEnvVar, "")
	m := newTestMux(t, ctrlPanes(1)...)
	m.startObservationIfEnabled(obsEnabled(os.Getenv(obsEnvVar)), nil)
	if m.obs != nil {
		t.Fatal("the pipeline started with MAGMUX_AGENTS unset")
	}
	m.stopObservation() // must be harmless
}

// TestStartObservationAgainstATemporaryRegistry is deliverable 4's wiring,
// exercised without going anywhere near ~/.claude.
//
// It also counts goroutines across the whole start/stop cycle. Stop is the
// shutdown barrier — it returns only once the watcher, every actor and the hub
// have finished — and a leak here is one goroutine per session for the life of
// the process.
func TestStartObservationAgainstATemporaryRegistry(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	dir := t.TempDir()
	record := filepath.Join(dir, "4242.json")
	body := `{"sessionId":"id-a","pid":4242,"name":"a","cwd":"/tmp/p","status":"busy",` +
		`"statusUpdatedAt":1789286000000,"updatedAt":1789286000000}`
	if err := os.WriteFile(record, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the record: %v", err)
	}

	before := runtime.NumGoroutine()

	m := newTestMux(t, ctrlPanes(1)...)
	m.panelPrivate = true // ADR-X5: observation only runs on a magmux whose panel is on-box
	if err := m.startObservation(dir); err != nil {
		t.Fatalf("startObservation: %v", err)
	}
	if m.obs == nil {
		t.Fatal("startObservation reported success and started nothing")
	}
	if err := m.startObservation(dir); err == nil {
		t.Error("a second startObservation started a second pipeline over the first")
	}

	sub, err := m.obs.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	m.obs.Refresh()
	select {
	case d, ok := <-sub.C():
		if !ok {
			t.Fatal("the subscription closed instead of delivering a document")
		}
		if len(d.Doc.Sessions) != 1 || d.Doc.Sessions[0].PID != 4242 {
			t.Fatalf("the document is %+v", d.Doc.Sessions)
		}
		// No pane owns pid 4242 — nothing in this magmux has a process — so
		// the pane facts are a determined absence rather than a guess.
		if d.Doc.Sessions[0].PaneID != nil {
			t.Errorf("a session with no pane was given pane %d", *d.Doc.Sessions[0].PaneID)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived from the pipeline")
	}

	m.stopObservation()
	if m.obs != nil {
		t.Error("stopObservation left the pipeline behind")
	}
	m.stopObservation() // idempotent

	// Drained rather than checked once: startObservation asks for a document
	// of its own, and that request and this test's Subscribe travel different
	// channels, so a second document may well be sitting in the buffer. What
	// the barrier promises is that the channel is CLOSED and nothing further
	// will arrive — not that it is empty.
	drained := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, ok := <-sub.C(); !ok {
			drained = true
			break
		}
	}
	if !drained {
		t.Error("the subscription was not closed by the shutdown barrier")
	}

	after := before
	deadline := time.Now().Add(10 * time.Second)
	for {
		after = runtime.NumGoroutine()
		if after <= before || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after > before {
		t.Errorf("goroutines went from %d to %d across a whole start/stop cycle", before, after)
	}
	t.Logf("goroutines: %d before, %d after", before, after)
}

// TestStartObservationRefusesAMissingRegistry — a machine with no Claude Code
// on it is the normal case, and a terminal multiplexer that refused to start
// over it would be absurd. The refusal is reported, not fatal.
func TestStartObservationRefusesAMissingRegistry(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	err := m.startObservation(filepath.Join(t.TempDir(), "not-there"))
	if err == nil {
		t.Fatal("a pipeline started against a directory that does not exist")
	}
	if m.obs != nil {
		t.Fatal("a failed start left a pipeline behind")
	}
}
