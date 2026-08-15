package main

// Regression tests for the locking discipline around the render loop, the
// panel's pane pointer, and child reaping.
//
// The property every test here defends is the same one: treeMu is taken for
// pointer surgery and nothing else. Every millisecond it is held is a
// millisecond of keystroke, SIGWINCH and socket latency, and the failure mode
// is not a crash or a race report — it is a magmux that looks healthy while an
// MCP open_pane times out against it.

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// blockingWriter is a terminal that has stopped draining — an ssh session with
// a full window, which is the everyday version of this.
type blockingWriter struct {
	mu      sync.Mutex
	delay   time.Duration
	inWrite chan struct{} // one token per write, sent as the write begins
	n       int
}

func newBlockingWriter(d time.Duration) *blockingWriter {
	return &blockingWriter{delay: d, inWrite: make(chan struct{}, 64)}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	select {
	case w.inWrite <- struct{}{}:
	default:
	}
	time.Sleep(w.delay)
	w.mu.Lock()
	w.n += len(p)
	w.mu.Unlock()
	return len(p), nil
}

func (w *blockingWriter) bytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// slowController stands in for ClaudeCodeController before it has found a
// transcript, which is when it is at its most expensive: it re-scans every
// directory under ~/.claude/projects on every 250ms tick.
type slowController struct {
	delay   time.Duration
	inPoll  chan struct{}
	polls   int
	pollsMu sync.Mutex
}

func (c *slowController) Name() string                    { return "slow" }
func (c *slowController) Start(ctx context.Context) error { return nil }
func (c *slowController) Stop() error                     { return nil }

func (c *slowController) Poll() (Snapshot, error) {
	select {
	case c.inPoll <- struct{}{}:
	default:
	}
	time.Sleep(c.delay)
	c.pollsMu.Lock()
	c.polls++
	c.pollsMu.Unlock()
	return Snapshot{State: CtrlWorking}, nil
}

// lockDelay measures how long it takes to acquire treeMu for writing, which is
// what a keystroke, a SIGWINCH and every socket lifecycle verb must do.
func lockDelay(m *Magmux) time.Duration {
	start := time.Now()
	m.treeMu.Lock()
	d := time.Since(start)
	m.treeMu.Unlock()
	return d
}

// TestRenderWritesTerminalWithTreeMuReleased is the lock-hold test for the
// frame write.
//
// render()'s doc comment claimed for a while that "terminal output is flushed
// after the unlock" while r.flush() sat inside renderLocked, under RLock. On a
// slow tty that is the whole frame's write time added to the latency of the
// next keystroke, focus change, SIGWINCH or socket open_pane — and Go's RWMutex
// puts later readers behind a waiting writer, so the render loop and the input
// loop take turns making each other wait.
func TestRenderWritesTerminalWithTreeMuReleased(t *testing.T) {
	defer deadlineWatchdog(t, 30*time.Second)()

	const writeFor = 400 * time.Millisecond
	m := newTestMux(t, ctrlPanes(2)...)
	w := newBlockingWriter(writeFor)
	m.out = w

	// A frame is only painted when something is dirty.
	p := m.paneByID(0)
	p.mu.Lock()
	p.dirty = true
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.render()
	}()

	select {
	case <-w.inWrite:
	case <-time.After(5 * time.Second):
		t.Fatal("render never wrote a frame; the fixture is wrong, not the lock")
	}

	// The write is in flight right now. Taking the write lock must not wait on
	// it. A little slack for scheduling, but nothing close to writeFor.
	if d := lockDelay(m); d > writeFor/4 {
		t.Errorf("treeMu.Lock waited %v while a %v terminal write was in flight — "+
			"the frame is being written under treeMu", d, writeFor)
	}

	<-done
	if w.bytes() == 0 {
		t.Error("no bytes reached the injected terminal writer")
	}
}

// TestControllerPollRunsWithTreeMuReleased is the same test for the other half
// of the frame: the controller poll.
//
// Until a Claude Code controller finds its transcript it walks
// ~/.claude/projects on every tick, and the fallback CLAUDE.md mandates scans
// EVERY project directory. Under RLock that filesystem walk is charged to the
// next keystroke and to every socket verb.
func TestControllerPollRunsWithTreeMuReleased(t *testing.T) {
	defer deadlineWatchdog(t, 30*time.Second)()

	const pollFor = 400 * time.Millisecond
	m := newTestMux(t, ctrlPanes(2)...)
	m.out = io.Discard
	ctrl := &slowController{delay: pollFor, inPoll: make(chan struct{}, 8)}
	m.paneByID(0).controller = ctrl

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.render()
	}()

	select {
	case <-ctrl.inPoll:
	case <-time.After(5 * time.Second):
		t.Fatal("the controller was never polled; the fixture is wrong, not the lock")
	}

	if d := lockDelay(m); d > pollFor/4 {
		t.Errorf("treeMu.Lock waited %v while a %v controller poll was in flight — "+
			"Poll is running under treeMu", d, pollFor)
	}

	<-done
}

// TestControlPanelPanePointerIsRaceFree closes the pane the panel is bound to
// while other goroutines ask the panel whether it has one.
//
// cp.pane used to be written under treeMu (by ClosePane) and read with no lock
// at all. That was sound only because the single reader happened to be
// render(), which happened to hold treeMu.RLock — a coincidence, not an
// invariant, and nothing in ControlPanel's API said so. Under -race this test
// reports the unsynchronised read directly.
func TestControlPanelPanePointerIsRaceFree(t *testing.T) {
	defer deadlineWatchdog(t, 30*time.Second)()

	m := newTestMux(t, ctrlPanes(2)...)
	m.out = io.Discard

	for i := 0; i < 20; i++ {
		id, err := m.OpenPane(OpenPaneRequest{
			PaneConfig: PaneConfig{Control: true}, Target: targetLargest,
		})
		if err != nil {
			t.Fatalf("OpenPane: %v", err)
		}
		m.control.attach(m.paneByID(id))

		// The readers spin for the WHOLE of the close, which is the only way
		// the detector is guaranteed to see the two overlap: ClosePane does
		// several other things (releaseSessions, reapPane, a broadcast) before
		// it reaches the panel, and a reader loop with a fixed trip count can
		// easily be finished by then.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.control.enabled()
					m.control.render()
				}
			}
		}()
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					m.control.recordRouteOpened(id, "churn")
					m.control.markDirty()
				}
			}
		}()

		_ = m.ClosePane(id, false)
		close(stop)
		wg.Wait()
	}
}

// TestClosePaneMovesThePanelFocusMarker: ClosePane fixes m.focused, and the
// route table's ▸ has to follow it. focusNext and sockFocus both tell the
// panel; a close that did not left the marker pointing at a pane that no longer
// existed until the next focus change — the one moment the table is most likely
// to be read.
func TestClosePaneMovesThePanelFocusMarker(t *testing.T) {
	m := newTestMux(t, ctrlPanes(3)...)
	m.control.setFocused(0)

	m.treeMu.Lock()
	m.focused = m.paneByIDLocked(0)
	m.treeMu.Unlock()

	if err := m.ClosePane(0, false); err != nil {
		t.Fatalf("ClosePane: %v", err)
	}

	m.treeMu.RLock()
	want := m.focused
	m.treeMu.RUnlock()
	if want == nil {
		t.Fatal("nothing is focused after the close")
	}

	m.control.mu.Lock()
	got := m.control.focused
	m.control.mu.Unlock()
	if got != want.id {
		t.Errorf("panel focus marker is on pane %d; magmux is focused on pane %d", got, want.id)
	}
}

// TestOpenPaneReapsChildOutsideGridMode: waitForChild is the only caller of
// cmd.Wait, and it used to be started only in grid mode. Every close_pane in a
// non-grid session therefore left a zombie for the life of magmux, and p.reaped
// stayed false — so the force path's delayed SIGKILL always fired, on a pid the
// OS was still free to recycle.
func TestOpenPaneReapsChildOutsideGridMode(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	m := newTestMux(t, ctrlPanes(1)...)
	m.gridMode = false // a plain multiplexer session, not a grid

	id, err := m.OpenPane(OpenPaneRequest{
		PaneConfig: PaneConfig{Cmd: "/bin/sh", Args: []string{"-c", "exit 7"}},
		Target:     targetLargest,
	})
	if err != nil {
		t.Fatalf("OpenPane: %v", err)
	}
	p := m.paneByID(id)
	if p.gridMode {
		t.Fatal("the new pane inherited gridMode; the fixture is wrong")
	}

	if !waitFor(5*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.reaped
	}) {
		t.Fatal("the child was never waited on: it is a zombie for the life of magmux, " +
			"and reapPane's delayed SIGKILL can now land on a recycled pid")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("cmd.ProcessState is nil — cmd.Wait never returned")
	}
	p.mu.Lock()
	code := p.exitCode
	p.mu.Unlock()
	if code != 7 {
		t.Errorf("exitCode = %d, want 7 — the exit status was not collected", code)
	}
}

// TestUnwindPaneReapsChild covers OpenPane's two unwind paths, where the child
// is fully forked but never published: wg.Add, readLoop and waitForChild have
// not run, so reapPane's SIGHUP ends the process and nothing collects it.
func TestUnwindPaneReapsChild(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	m := newTestMux(t, ctrlPanes(1)...)
	np, err := newPaneFor(0, 0, 10, 40, PaneConfig{
		Cmd: "/bin/sh", Args: []string{"-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("newPaneFor: %v", err)
	}
	m.unwindPane(np)

	if !waitFor(10*time.Second, func() bool {
		np.mu.Lock()
		defer np.mu.Unlock()
		return np.reaped
	}) {
		t.Fatal("the unwound pane's child was never waited on — it stays a zombie " +
			"for the life of magmux, one per raced open_pane")
	}
	if np.cmd.ProcessState == nil {
		t.Fatal("cmd.ProcessState is nil — cmd.Wait never returned on the unwind path")
	}
}

// TestOpenPaneDefaultsToLargestLeaf pins the socket default for a caller that
// named no target.
//
// The MCP schema advertises "the largest pane", and the caller is an agent that
// has said nothing about focus. Focus belongs to the human: in the flagship
// layout they are typing into pane 0, and splitting it reflows a Claude Code
// TUI mid-turn to make room for something they never asked to see.
func TestOpenPaneDefaultsToLargestLeaf(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	m := newTestMux(t, ctrlPanes(2)...)
	m.out = io.Discard

	// Halve pane 1, so pane 0 is unambiguously the largest leaf...
	small, err := m.OpenPane(OpenPaneRequest{
		PaneConfig: PaneConfig{Control: true}, Target: 1,
	})
	if err != nil {
		t.Fatalf("OpenPane: %v", err)
	}
	// ...and focus one of the small halves, which is what the human is typing in.
	m.treeMu.Lock()
	m.focused = m.paneByIDLocked(small)
	big := m.paneByIDLocked(0)
	m.treeMu.Unlock()

	res, err := m.sockOpenPane(sockMsg{Type: "open_pane", Cmd: "sleep 2"})
	if err != nil {
		t.Fatalf("sockOpenPane: %v", err)
	}
	newID := int(res["pane"].(int))
	np := m.paneByID(newID)
	defer m.ClosePane(newID, true)

	m.treeMu.RLock()
	sibling := np.parent.child1
	m.treeMu.RUnlock()
	if sibling != big {
		t.Errorf("open_pane with no target split pane %d; want the largest leaf, pane %d",
			sibling.id, big.id)
	}
}

// TestSocketRefusesToCloseTheControlPane: the panel is the instrument that
// answers "what did the agent do", so it must not be able to be asked "who
// closed the panel". The MCP layer already refused; the socket verb underneath
// it did not, which made the refusal a matter of which client you used.
func TestSocketRefusesToCloseTheControlPane(t *testing.T) {
	m := newTestMux(t, PaneConfig{Cmd: "/bin/sh"}, PaneConfig{Control: true})
	m.control.attach(m.paneByID(1))

	_, err := m.sockClosePane(sockMsg{Type: "close_pane", Pane: float64(1)})
	if err == nil {
		t.Fatal("close_pane on the control pane succeeded; the panel is not a session")
	}
	if code := verbErrCode(err); code != sockCodePaneIsControl {
		t.Errorf("close_pane error code = %q, want %q", code, sockCodePaneIsControl)
	}
	if m.paneByID(1) == nil {
		t.Error("the control pane was closed anyway")
	}
	if m.control.paneRef() == nil {
		t.Error("the panel was detached from its pane")
	}
}

// waitFor polls cond until it holds or the budget runs out.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// keep os/exec referenced: the reaping assertions read cmd.ProcessState, whose
// type comes from there.
var _ = exec.ErrNotFound
var _ = os.Getpid
