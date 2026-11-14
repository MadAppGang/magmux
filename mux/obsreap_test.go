package mux

import (
	"context"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/obsview"
)

// A finished pane must not claim a session through a pid it no longer owns.
// Fix round 2, item 4.
//
// Both pane tables map a pane's child pid to the pane, and until this round
// they kept doing so after the child had been REAPED. Once cmd.Wait has
// returned, the kernel may hand that number to any new process, and a session
// descended from the new owner was attributed to the finished pane: reported
// as running in it, weighed against hours of that pane's silence, and — through
// `magmux agents focus` — focused there.
//
// CLAUDE.md keeps `dead` and `reaped` apart for exactly this reason: `dead` is
// "the PTY closed", and the process entry survives it, so the pid is still
// held; only `reaped` says the number has been released. So the rule is
// "reaped contributes no pid", and a dead-but-unreaped pane keeps its claim.
//
// Nothing here forks, signals or probes a pid: the process tree is staged, and
// the socket magmux is this test's own listener.

// finish marks a pane's child as having exited, and optionally as reaped —
// the two independent writers CLAUDE.md describes, collapsed into one call.
func finish(t *testing.T, p *Pane, reaped bool) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	p.deadAt = time.Now()
	p.reaped = reaped
}

// TestAReapedPaneClaimsNoSessionThroughItsOldPID is the scenario the review
// staged with an overlay, kept, in both tables.
func TestAReapedPaneClaimsNoSessionThroughItsOldPID(t *testing.T) {
	t.Run("magmux's own pane table", func(t *testing.T) {
		m := newTestMux(t, ctrlPanes(3)...)
		givePID(t, m.paneByID(0), 400) // finished and reaped: 400 is the OS's to reuse
		givePID(t, m.paneByID(1), 500) // finished, not yet reaped: 500 is still held
		givePID(t, m.paneByID(2), 600) // running
		finish(t, m.paneByID(0), true)
		finish(t, m.paneByID(1), false)

		loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{
			900: 400, // a session whose ancestry reaches 400 — the number's NEW owner
			400: 1,
			910: 500,
			920: 600,
		})}

		if id, found := loc.paneForPID(900); found != model.LivenessNo {
			t.Errorf("session 900 was attributed to pane %d (%v) through pid 400, which pane 0's "+
				"child held until it was reaped; after that the number belongs to whoever the "+
				"kernel gave it to", id, found)
		}
		if id, found := loc.paneForPID(910); found != model.LivenessYes || id != 1 {
			t.Errorf("PaneFor(910) = (%d, %v), want (1, yes): a DEAD pane that is not reaped still "+
				"holds its pid, and a child that closed its PTY can still be running", id, found)
		}
		if id, found := loc.paneForPID(920); found != model.LivenessYes || id != 2 {
			t.Errorf("PaneFor(920) = (%d, %v), want (2, yes)", id, found)
		}
		// The pane is still a pane. Reaped is a fact about its PID, not about
		// whether it exists: `dead` is not `closed`, and a finished pane stays on
		// screen with its tombstone.
		if got := loc.PaneAlive(obsview.PaneRef(0)); got != model.LivenessYes {
			t.Errorf("PaneAlive(0) = %v, want yes — only closed makes a pane not alive", got)
		}
	})

	t.Run("the socket pane table", func(t *testing.T) {
		defer deadlineWatchdog(t, 60*time.Second)()
		// What buildPaneResultsLocked sends for the same four situations. A
		// pid-bearing entry reads "completed" or "failed" only once the child
		// has been reaped — before that it reads "running" with dead:true,
		// precisely so that no exit status is invented.
		startFakeSockMagmux(t, "obsreaped",
			`{"pane":0,"state":"completed","exitCode":0,"dead":true,"pid":900001},`+
				`{"pane":1,"state":"failed","exitCode":1,"dead":true,"pid":900002},`+
				`{"pane":2,"state":"running","exitCode":0,"dead":true,"pid":900003},`+
				`{"pane":3,"state":"running","exitCode":0,"dead":false,"pid":900004}`)

		loc := newSockPaneLocator(context.Background())
		if loc == nil {
			t.Fatal("no pane table was built")
		}
		loc.ppid = stagedTree(map[int]int{
			700100: 900001, 900001: 1, // reaped, exit 0
			700200: 900002, 900002: 1, // reaped, exit 1
			700300: 900003, // dead, not yet reaped
			700400: 900004, // running
		})

		for _, pid := range []int{700100, 700200} {
			if id, live := loc.paneForPID(pid); live != model.LivenessNo {
				t.Errorf("session %d was attributed to pane %d (%v) through the pid of a pane "+
					"whose child has been reaped", pid, id, live)
			}
			if one, live := loc.forSession(pid); one != nil || live != model.LivenessNo {
				t.Errorf("forSession(%d) bound to a magmux (%v) through a reaped pane's pid; "+
					"`magmux agents focus` would focus a finished pane", pid, live)
			}
		}
		if id, live := loc.paneForPID(700300); live != model.LivenessYes || id != 2 {
			t.Errorf("PaneFor(700300) = (%d, %v), want (2, yes): dead and not reaped still holds "+
				"its pid", id, live)
		}
		if id, live := loc.paneForPID(700400); live != model.LivenessYes || id != 3 {
			t.Errorf("PaneFor(700400) = (%d, %v), want (3, yes)", id, live)
		}
	})
}
