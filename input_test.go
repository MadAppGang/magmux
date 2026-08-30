package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// ── typing into a pane magmux calls "done" ───────────────────────────────────
//
// magmux has two input paths into a pane and they used to disagree about what
// an idle pane is. `send` reached one over the socket; the keyboard did not.
// A program could steer an agent after its turn ended and the person sitting at
// the same terminal could not, which made an attached session read-only from
// the moment the agent first went quiet.

// inputMux builds a grid-mode Magmux around one pane with a pipe for a PTY:
// enough to drive the real inputLoop and read what the child would have seen.
func inputMux(t *testing.T) (*Magmux, *Pane, *os.File) {
	t.Helper()
	m := &Magmux{rows: 40, cols: 120, gridMode: true, quit: make(chan struct{}), control: newControlPanel()}
	p := newScrollPane(20, 60)
	p.gridMode = true
	childR, childW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { childR.Close(); childW.Close() })
	p.ptmx = childW
	m.root = p
	m.focused = p
	m.allPanes = []*Pane{p}
	m.stampPaneIDs()
	return m, p, childR
}

// markIdle puts a pane in the state renderLocked's idle sweep leaves behind: a
// live child, a ✓ DONE overlay, and both idle clocks already elapsed.
func markIdle(p *Pane) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inputReady = true
	p.inputSignal = "idle"
	p.inputReadyAt = time.Now()
	p.tint = "green"
	p.overlayText = "✓ DONE"
	p.overlayStyle = "success"
	p.hadTextOutput = true
	p.lastTextAt = time.Now().Add(-time.Minute)
	p.titleIdleAt = time.Now().Add(-time.Minute)
}

// TestKeyboardAndSocketBothReachAnIdlePane is the asymmetry itself, stated as
// one table over the two paths so neither can be fixed without the other.
//
// The socket half has always passed. The keyboard half wrote nothing at all:
// writePTY refused `p.gridMode && (p.dead || p.inputReady)`, and an idle pane
// is exactly the state a pilot — or a person — wants to act on.
func TestKeyboardAndSocketBothReachAnIdlePane(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(p *Pane, b []byte)
	}{
		{"a keystroke", func(p *Pane, b []byte) { p.writePTY(b) }},
		{"a socket send", func(p *Pane, b []byte) { p.injectPTY(b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, child := inputMux(t)
			markIdle(p)

			tc.write(p, []byte("hello"))

			if got := drain(t, child); got != "hello" {
				t.Errorf("the child saw %q, want %q: input into an idle pane was dropped", got, "hello")
			}

			// The completion state must go with the write, or the ✓ DONE
			// chrome outlives the turn it describes.
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.inputReady {
				t.Error("inputReady survived the write; the pane still reads as done")
			}
			for _, f := range []struct {
				name string
				got  string
			}{
				{"inputSignal", p.inputSignal},
				{"tint", p.tint},
				{"overlayText", p.overlayText},
				{"overlayStyle", p.overlayStyle},
			} {
				if f.got != "" {
					t.Errorf("%s = %q, want cleared", f.name, f.got)
				}
			}
			// hadTextOutput false is what stops renderLocked's 5s text-idle
			// rule re-firing on output that was already on screen and calling
			// the pane done again one frame later.
			if p.hadTextOutput {
				t.Error("hadTextOutput survived the write; the text-idle sweep can immediately re-settle the pane")
			}
			if !p.titleIdleAt.IsZero() {
				t.Error("titleIdleAt survived the write; the title-idle sweep can immediately re-settle the pane")
			}
		})
	}
}

// TestWritePTYStillRefusesADeadPaneInGridMode keeps the half of the old guard
// that was right. `q` dismissing a finished grid depends on plain keys NOT
// reaching a corpse, and there is no process on the far end to read them.
func TestWritePTYStillRefusesADeadPaneInGridMode(t *testing.T) {
	_, p, child := inputMux(t)
	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()

	p.writePTY([]byte("hello"))

	if got := drain(t, child); got != "" {
		t.Errorf("a keystroke reached a dead pane: %q", got)
	}
}

// TestPlainKeysReachAnIdlePaneThroughTheRealInputLoop drives inputLoop, because
// writePTY was only the SECOND thing standing in the way and a test that calls
// it directly would have passed while a one-pane session stayed untypeable.
//
// The first was inputLoop's own grid branch, which asked allPanesDone — true
// the moment a single pane went idle — and swallowed every byte that was not
// q/Esc/Ctrl-C before typeToFocused was ever called.
func TestPlainKeysReachAnIdlePaneThroughTheRealInputLoop(t *testing.T) {
	defer deadlineWatchdog(t, 20*time.Second)()

	m, p, child := inputMux(t)
	markIdle(p)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pw.Close()
	m.stdin = pr

	done := make(chan struct{})
	go func() { m.inputLoop(); close(done) }()
	defer func() {
		m.quitOnce.Do(func() { close(m.quit) })
		<-done
	}()

	if _, err := pw.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got += drain(t, child)
		if got == "hello" {
			break
		}
	}
	if got != "hello" {
		t.Errorf("the child saw %q, want %q: the input loop swallowed keys aimed at an idle pane", got, "hello")
	}
}

// TestTheBarAdvertisesTheQuitKeyThatActuallyWorks. The status bar is the only
// place the quit key is announced, so it has to follow inputLoop's predicate
// rather than the grid counter's. It advertised a bare `q` for any "complete"
// grid — which, at an idle agent, now types a stray q into its prompt instead.
func TestTheBarAdvertisesTheQuitKeyThatActuallyWorks(t *testing.T) {
	statusFor := func(t *testing.T, prep func(p *Pane)) string {
		t.Helper()
		m, p, _ := inputMux(t)
		m.startedAt = time.Now()
		prep(p)
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		m.markAllDirtyLocked()
		_, _, _ = m.renderLocked()
		return m.statusText
	}

	idle := statusFor(t, markIdle)
	if !strings.Contains(idle, "ctrl-g q") {
		t.Errorf("with a live idle pane the bar says %q; it must advertise the chord, not a bare q", idle)
	}

	dead := statusFor(t, func(p *Pane) {
		p.mu.Lock()
		p.dead = true
		p.mu.Unlock()
	})
	if !strings.Contains(dead, "q or Esc to quit") && !strings.Contains(dead, "q quit") {
		t.Errorf("with every pane exited the bar says %q; the bare-key quit must still be offered", dead)
	}
	if strings.Contains(dead, "ctrl-g q") {
		t.Errorf("a finished grid was told to use the chord: %q", dead)
	}
}

// TestChordHintsSurviveAnIdlePane. keyHintLocked blanks the hint set when the
// chord is inert. An idle pane does not make it inert — only a grid of dead
// ones does — and blanking it there told a person their keyboard was gone at
// the moment it still worked.
func TestChordHintsSurviveAnIdlePane(t *testing.T) {
	m, p, _ := inputMux(t)
	markIdle(p)
	m.treeMu.RLock()
	idleHints := m.keyHintLocked()
	m.treeMu.RUnlock()
	if len(idleHints) == 0 {
		t.Error("the chord hints blanked for an idle pane whose chord still works")
	}

	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()
	m.treeMu.RLock()
	deadHints := m.keyHintLocked()
	m.treeMu.RUnlock()
	if len(deadHints) != 0 {
		t.Errorf("a finished grid still advertised %d chord keys that do nothing", len(deadHints))
	}
}

// ── --no-idle-done (manual mode) ─────────────────────────────────────────────

// TestNoIdleDoneWithdrawsTheCompletionClaim. The flag's whole surface is what
// magmux SAYS about an idle pane, so this checks the three things it suppresses
// and the one thing it must not touch.
func TestNoIdleDoneWithdrawsTheCompletionClaim(t *testing.T) {
	t.Run("the bar keeps counting the session as running", func(t *testing.T) {
		m, p, _ := inputMux(t)
		m.noIdleDone = true
		m.startedAt = time.Now()
		markIdle(p)
		m.treeMu.RLock()
		m.markAllDirtyLocked()
		_, _, _ = m.renderLocked()
		got := m.statusText
		m.treeMu.RUnlock()
		if strings.Contains(got, "✓ complete") || strings.Contains(got, "1/1 done") {
			t.Errorf("--no-idle-done still called a resting session complete: %q", got)
		}
		if !strings.Contains(got, "1 running") {
			t.Errorf("the bar dropped the running count: %q", got)
		}
	})

	t.Run("no DONE overlay is painted", func(t *testing.T) {
		m, p, _ := inputMux(t)
		m.noIdleDone = true
		m.startedAt = time.Now()
		// inputReady with no chrome yet is what the idle sweep produces.
		p.mu.Lock()
		p.inputReady = true
		p.inputSignal = "idle"
		p.mu.Unlock()

		m.treeMu.RLock()
		_, _, _ = m.renderLocked()
		m.treeMu.RUnlock()

		p.mu.Lock()
		defer p.mu.Unlock()
		if p.tint != "" || p.overlayText != "" {
			t.Errorf("--no-idle-done painted completion chrome: tint=%q overlay=%q", p.tint, p.overlayText)
		}
	})

	t.Run("-w waits for the process, not the turn", func(t *testing.T) {
		m, p, _ := inputMux(t)
		m.noIdleDone = true
		markIdle(p)
		if m.allPanesDone() {
			t.Error("--no-idle-done let -w fire on an agent that was only resting")
		}
		p.mu.Lock()
		p.dead = true
		p.mu.Unlock()
		if !m.allPanesDone() {
			t.Error("--no-idle-done stopped -w firing even after the process exited")
		}
	})

	t.Run("idle is still reported to controllers", func(t *testing.T) {
		// The flag must not reach inputReady itself: `snapshot` and `results`
		// read it, and CLAUDE.md's rule is that the two can never disagree.
		// A pilot waiting for a turn to end must not be starved by a flag
		// about magmux's own chrome.
		p := &Pane{}
		p.mu.Lock()
		applyControllerSnapshot(p, Snapshot{State: CtrlAwaitingInput}, true)
		p.mu.Unlock()

		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.inputReady {
			t.Error("--no-idle-done suppressed inputReady; snapshot and results would go silent")
		}
		if p.overlayText != "" {
			t.Errorf("--no-idle-done still painted the controller's ✓ DONE card: %q", p.overlayText)
		}
		if !p.dirty {
			t.Error("the pane was left clean; the state change would not repaint")
		}
	})
}

// TestIdleStillCountsAsDoneByDefault is the other half: --no-idle-done is
// opt-in, so a plain grid run must still finish on idle or -w regresses for
// every batch that relies on it.
func TestIdleStillCountsAsDoneByDefault(t *testing.T) {
	m, p, _ := inputMux(t)
	markIdle(p)
	if !m.allPanesDone() {
		t.Error("an idle pane stopped counting as done without the flag; -w would hang")
	}
}

// TestPlainKeysQuitOnlyWhenEveryPaneIsDead pins the predicate swap from both
// sides: idle must not arm the bare-key quit, dead must.
func TestPlainKeysQuitOnlyWhenEveryPaneIsDead(t *testing.T) {
	t.Run("an idle pane does not arm it", func(t *testing.T) {
		m, p, _ := inputMux(t)
		markIdle(p)
		if !m.allPanesDone() {
			t.Fatal("an idle pane should still count as done for -w and auto-exit")
		}
		if m.allPanesDead() {
			t.Error("an idle pane counted as dead; plain keys would be stolen from a live child")
		}
	})

	t.Run("a dead pane arms it", func(t *testing.T) {
		defer deadlineWatchdog(t, 20*time.Second)()

		m, p, _ := inputMux(t)
		p.mu.Lock()
		p.dead = true
		p.mu.Unlock()
		if !m.allPanesDead() {
			t.Fatal("a dead pane did not count as dead")
		}

		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		defer pw.Close()
		m.stdin = pr

		done := make(chan struct{})
		go func() { m.inputLoop(); close(done) }()
		if _, err := pw.Write([]byte("q")); err != nil {
			t.Fatalf("write: %v", err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			m.quitOnce.Do(func() { close(m.quit) })
			<-done
			t.Fatal("`q` did not dismiss a grid whose panes had all exited")
		}
		select {
		case <-m.quit:
		default:
			t.Error("the input loop returned without closing m.quit")
		}
	})
}
