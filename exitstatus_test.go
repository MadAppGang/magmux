package main

// S-1 — the -w exit-status race.
//
// `magmux -w -e 'exit 7'` could emit results with {"state":"completed",
// "exitCode":0}: a run that FAILED recorded as having PASSED. The mechanism is
// two independent writers of p.dead. readLoop sets it alone when the PTY
// closes; reapChild sets dead, reaped and exitCode together when cmd.Wait
// returns, on a different goroutine. -w's gate tested only p.dead, so it could
// fire — and results could be built and broadcast, and every subscriber closed
// — before the real status existed anywhere.
//
// Headless is what raises the severity: with no grid to look at and no ✗ FAIL
// tombstone, `results` is the ONLY report a caller reads.

import (
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// TestPaneDoneLocked is the whole fix in one table. Every row is a state the
// production code can really be in.
func TestPaneDoneLocked(t *testing.T) {
	// A cmd that was never started: enough to make p.cmd non-nil, which is what
	// distinguishes "a child we are still waiting on" from "no child at all".
	someCmd := func() *exec.Cmd { return exec.Command("true") }

	cases := []struct {
		name string
		pane *Pane
		want bool
	}{
		{
			// THE BUG. The PTY has just closed and cmd.Wait has not returned,
			// so p.exitCode is a lie waiting to be published.
			name: "dead but not yet reaped",
			pane: &Pane{dead: true, deadAt: time.Now(), cmd: someCmd()},
			want: false,
		},
		{
			name: "dead and reaped",
			pane: &Pane{dead: true, reaped: true, deadAt: time.Now(), cmd: someCmd()},
			want: true,
		},
		{
			// Must NOT regress: an idle agent pane never exits, it finished its
			// turn. Requiring `reaped` of it would mean -w never fires for a
			// Claude Code pane at all.
			name: "idle, alive",
			pane: &Pane{inputReady: true, cmd: someCmd()},
			want: true,
		},
		{
			// The bound (R-3): a cmd.Wait that never returns must not wedge -w.
			name: "dead, unreaped, past the grace period",
			pane: &Pane{dead: true, deadAt: time.Now().Add(-time.Hour), cmd: someCmd()},
			want: true,
		},
		{
			// Zero-value compatibility. Every existing test literal is exactly
			// this shape, and every one of them must keep answering "done".
			name: "dead with deadAt unset (an existing test literal)",
			pane: &Pane{dead: true},
			want: true,
		},
		{
			// No child means no status to wait for, and reapChild returns early
			// on a nil cmd so `reaped` never becomes true. Without this branch
			// such a pane would stall -w for the whole grace period.
			name: "dead, no cmd at all",
			pane: &Pane{dead: true, deadAt: time.Now()},
			want: true,
		},
		{
			name: "alive and busy",
			pane: &Pane{cmd: someCmd()},
			want: false,
		},
		{
			// Both signals at once: idle wins, and it wins first, so a pane that
			// went idle and whose PTY then closed is not held up.
			name: "dead, unreaped, but idle",
			pane: &Pane{dead: true, deadAt: time.Now(), inputReady: true, cmd: someCmd()},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paneDoneLocked(c.pane); got != c.want {
				t.Fatalf("paneDoneLocked = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAllPanesDoneWaitsForTheRealExitStatus is the same property one level up,
// where -w actually reads it.
func TestAllPanesDoneWaitsForTheRealExitStatus(t *testing.T) {
	p := &Pane{dead: true, deadAt: time.Now(), cmd: exec.Command("true")}
	m := &Magmux{allPanes: []*Pane{p}}

	if m.allPanesDone() {
		t.Fatal("-w fired on a pane whose exit status has not been collected yet")
	}

	p.mu.Lock()
	p.reaped = true
	p.exitCode = 7
	p.mu.Unlock()

	if !m.allPanesDone() {
		t.Fatal("-w did not fire once the pane was reaped")
	}
}

// TestBuildPaneResultsWillNotClaimCompletedBeforeReaped is the reporting half.
//
// Even with -w fixed, the connect-time aggregate can still be built in the
// window between the PTY closing and cmd.Wait returning, and it would make
// exactly the same false claim.
func TestBuildPaneResultsWillNotClaimCompletedBeforeReaped(t *testing.T) {
	p := &Pane{dead: true, deadAt: time.Now(), cmd: exec.Command("true")}
	m := &Magmux{allPanes: []*Pane{p}}

	m.treeMu.RLock()
	res := m.buildPaneResultsLocked()
	m.treeMu.RUnlock()

	if len(res) != 1 {
		t.Fatalf("want 1 pane, got %d", len(res))
	}
	if res[0]["state"] == "completed" {
		t.Fatalf("a dead-but-unreaped pane was reported as completed with exitCode %v; "+
			"the status has not been collected yet", res[0]["exitCode"])
	}
	if res[0]["state"] != "running" {
		t.Fatalf("state is %v, want \"running\"", res[0]["state"])
	}

	// And once reaped it tells the truth.
	p.mu.Lock()
	p.reaped = true
	p.exitCode = 7
	p.mu.Unlock()

	m.treeMu.RLock()
	res = m.buildPaneResultsLocked()
	m.treeMu.RUnlock()
	if res[0]["state"] != "failed" {
		t.Fatalf("state is %v, want \"failed\"", res[0]["state"])
	}
}

// ── the decisive subprocess test ────────────────────────────────────────────

// TestHeadlessReportsRealExitStatus reads the exit code out of `results`, never
// out of an `exit` event: `exit` is a live broadcast from waitForChild's own
// goroutine with no delivery guarantee, and under -w it races the very teardown
// -w triggers. CLAUDE.md records that trap as diagnosed twice.
//
// Run it with -count=20. It is a RACE, and a single pass proves nothing —
// against the unfixed binary it reproduces within a handful of iterations.
func TestHeadlessReportsRealExitStatus(t *testing.T) {
	cases := []struct {
		cmd       string
		wantCode  int
		wantState string
	}{
		{"exit 7", 7, "failed"},
		{"exit 0", 0, "completed"},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			h := startHeadlessMagmux(t, "--headless", "-w", "-e", c.cmd)
			conn := dialASAP(t, h.sock, 10*time.Second)
			// Timed from HERE, not from before the spawn. The ceiling below is
			// reapGrace (2s), and an earlier version started the clock before
			// startHeadlessMagmux — so the budget also had to cover building
			// nothing, forking the binary, the socket bind/dial retry loop and
			// process teardown. Under `go test -race` with parallel packages a
			// perfectly healthy run breaches that, which makes the assertion
			// measure the machine rather than the code. The grace period, if it
			// is being paid, is paid INSIDE magmux between the PTY closing and
			// `results` being broadcast — and that window is entirely after the
			// dial, so this is both the tighter and the correct measurement.
			started := time.Now()
			rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}

			var session map[string]any
			for _, r := range rc.awaitResults() {
				if r["state"] == "panel" {
					continue
				}
				if session != nil {
					t.Fatalf("more than one session pane in results: %v", r)
				}
				session = r
			}
			if session == nil {
				t.Fatal("results carried no session pane")
			}
			if got := paneNum(t, session, "exitCode"); got != c.wantCode {
				t.Fatalf("exitCode %d, want %d — full entry: %v", got, c.wantCode, session)
			}
			if session["state"] != c.wantState {
				t.Fatalf("state %v, want %q — full entry: %v", session["state"], c.wantState, session)
			}

			h.wait(20 * time.Second)
			// R-7: the fix must not routinely cost the grace period. The two
			// events are microseconds apart in the normal case; if this ever
			// approaches reapGrace, the wait has become the common path rather
			// than the fallback.
			if elapsed := time.Since(started); elapsed > reapGrace {
				t.Errorf("the whole run took %v, longer than reapGrace (%v): "+
					"waiting for cmd.Wait has become the common path", elapsed, reapGrace)
			}
		})
	}
}

// TestHeadlessReportsRealExitStatusRepeatedly shakes the race inside one test
// invocation, so `go test ./...` exercises it even when nobody passes -count.
func TestHeadlessReportsRealExitStatusRepeatedly(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns 10 magmuxes")
	}
	const iterations = 10
	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			h := startHeadlessMagmux(t, "--headless", "-w", "-e", "exit 7")
			conn := dialASAP(t, h.sock, 10*time.Second)
			rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}
			for _, r := range rc.awaitResults() {
				if r["state"] == "panel" {
					continue
				}
				if code := paneNum(t, r, "exitCode"); code != 7 {
					t.Fatalf("iteration %d: exitCode %d, want 7 — %v", i, code, r)
				}
				if r["state"] != "failed" {
					t.Fatalf("iteration %d: state %v, want failed — %v", i, r["state"], r)
				}
			}
			h.wait(20 * time.Second)
		})
	}
}
