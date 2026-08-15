package main

// Dynamic panes: identity, tree surgery, and the locking around them.
//
// The unit tests here run against PTY-less control panes. That is not a
// shortcut: a control pane exercises the entire tree, id and locking path with
// no process, no PTY and no timing, so a failure names a bug in the layout code
// rather than in a fixture's shell quoting. The end-to-end tests at the bottom
// cover the parts a fake pane cannot prove — that a child really started, in
// the directory it was told to, and that closing one really reaps it.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// newTestMux builds a Magmux with a real layout of PTY-less panes.
func newTestMux(t *testing.T, cfgs ...PaneConfig) *Magmux {
	t.Helper()
	m := &Magmux{
		rows:     40,
		cols:     120,
		gridMode: true,
		quit:     make(chan struct{}),
		control:  newControlPanel(),
	}
	if err := m.buildGrid(cfgs); err != nil {
		t.Fatalf("buildGrid: %v", err)
	}
	return m
}

// ctrlPanes is the config slice for n PTY-less panes.
func ctrlPanes(n int) []PaneConfig {
	cfgs := make([]PaneConfig, n)
	for i := range cfgs {
		cfgs[i] = PaneConfig{Control: true}
	}
	return cfgs
}

func eachLeaf(p *Pane, fn func(*Pane)) {
	if p == nil {
		return
	}
	if p.splitType == SplitNone {
		fn(p)
		return
	}
	eachLeaf(p.child1, fn)
	eachLeaf(p.child2, fn)
}

// deadlineWatchdog panics with EVERY goroutine's stack if the test has not
// finished within d.
//
// This is the only instrument that makes the headline risk of treeMu
// debuggable: a recursive RLock (render holding the lock and calling a
// non-Locked helper) is a silent HANG, not a race report. Without the dump the
// symptom is a bare `panic: test timed out` naming nothing at all; with it, the
// two goroutines parked on treeMu are right there in the output.
func deadlineWatchdog(t *testing.T, d time.Duration) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(d):
			fmt.Fprintf(os.Stderr, "\n=== %s exceeded %v — every goroutine stack follows ===\n", t.Name(), d)
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			panic("deadlock watchdog fired in " + t.Name())
		}
	}()
	return func() { close(done) }
}

// ── unit: tree surgery ──────────────────────────────────────────────────────

// TestSplitLeafInsertsInternalNode pins the rule that makes every external
// back-pointer survive a split: the leaf is never converted into an internal
// node. It owns its Screen, its PTY and vt.node == itself, and it is pointed at
// from outside the tree by ClaudeCodeController.pane, ControlPanel.pane,
// sel.pane and claimedSessions. Converting it in place would strand all of
// them silently — the pane would still render, and would never update again.
func TestSplitLeafInsertsInternalNode(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	leaf := m.paneByID(0)
	screen, vtNode := leaf.screen, leaf.vt.node

	id, err := m.OpenPane(OpenPaneRequest{
		PaneConfig: PaneConfig{Control: true},
		Target:     0,
		Split:      SplitHorizontal,
	})
	if err != nil {
		t.Fatalf("OpenPane: %v", err)
	}
	np := m.paneByID(id)

	if m.paneByID(0) != leaf {
		t.Fatal("pane 0 is a different *Pane after the split; the leaf was replaced, not spliced around")
	}
	if leaf.screen != screen {
		t.Error("the leaf's Screen was replaced by the split")
	}
	if leaf.vt.node != vtNode || leaf.vt.node != leaf {
		t.Error("the leaf's VT parser no longer points at the leaf; its output would land nowhere")
	}
	if leaf.splitType != SplitNone {
		t.Error("the leaf was converted into an internal node")
	}

	par := leaf.parent
	if par == nil {
		t.Fatal("the leaf has no parent after the split")
	}
	if par != m.root {
		t.Error("the new internal node did not take the leaf's place at the root")
	}
	if par.splitType != SplitHorizontal {
		t.Errorf("parent splitType = %v, want SplitHorizontal", par.splitType)
	}
	if par.child1 != leaf || par.child2 != np {
		t.Error("the new node's children are not (target, new pane) in that order")
	}
	if np.parent != par {
		t.Error("the new pane's parent back-pointer was not set")
	}
	if got := par.child1.w + par.child2.w + 1; got != par.w {
		t.Errorf("child widths %d + %d + 1 border = %d, want the parent's %d",
			par.child1.w, par.child2.w, got, par.w)
	}
	for _, p := range []*Pane{leaf, np} {
		if p.screen.rows != p.h || p.screen.cols != p.w {
			t.Errorf("pane %d screen is %dx%d but the pane is %dx%d — the reflow did not resize it",
				p.id, p.screen.rows, p.screen.cols, p.h, p.w)
		}
	}
}

// TestRemoveLeafCollapsesParentIntoSibling checks the other half: the sibling
// must inherit the vanished parent's EXACT geometry, so the closed pane's space
// plus the border between them is reclaimed with no gap and no overlap.
func TestRemoveLeafCollapsesParentIntoSibling(t *testing.T) {
	m := newTestMux(t, ctrlPanes(3)...)

	victim := m.paneByID(0)
	par := victim.parent
	if par == nil {
		t.Fatal("a 3-pane grid should not put pane 0 at the root")
	}
	sib := par.child1
	if sib == victim {
		sib = par.child2
	}
	wantY, wantX, wantH, wantW := par.y, par.x, par.h, par.w

	if err := m.ClosePane(0, false); err != nil {
		t.Fatalf("ClosePane: %v", err)
	}

	if sib.y != wantY || sib.x != wantX || sib.h != wantH || sib.w != wantW {
		t.Errorf("sibling geometry is (%d,%d %dx%d), want the removed parent's (%d,%d %dx%d)",
			sib.y, sib.x, sib.h, sib.w, wantY, wantX, wantH, wantW)
	}
	if sib.parent != nil && sib.parent.child1 != sib && sib.parent.child2 != sib {
		t.Error("the sibling was not spliced into the grandparent")
	}
	eachLeaf(m.root, func(p *Pane) {
		if p.closed {
			t.Errorf("pane %d is tombstoned but still in the tree", p.id)
		}
		if p.screen.rows != p.h || p.screen.cols != p.w {
			t.Errorf("pane %d screen is %dx%d but the pane is %dx%d after the collapse",
				p.id, p.screen.rows, p.screen.cols, p.h, p.w)
		}
	})
	if m.paneByID(0) != nil {
		t.Error("paneByID still resolves a closed pane; every verb would happily write into it")
	}
}

// TestPaneIDsAreStableAcrossClose is the reason allPanes is append-only. The
// wire protocol addresses panes by integer and nothing else, so a compacting
// slice would make `send` to pane 2 silently reach a different session the
// first time anything closed — a failure with no error and no log.
func TestPaneIDsAreStableAcrossClose(t *testing.T) {
	m := newTestMux(t, ctrlPanes(3)...)
	a, b, c := m.paneByID(0), m.paneByID(1), m.paneByID(2)
	if a == nil || b == nil || c == nil {
		t.Fatal("expected panes 0, 1 and 2")
	}

	if err := m.ClosePane(1, false); err != nil {
		t.Fatalf("ClosePane(1): %v", err)
	}
	if m.paneByID(1) != nil {
		t.Error("closed pane 1 still resolves")
	}
	if m.paneByID(0) != a {
		t.Error("pane 0 moved when pane 1 closed")
	}
	if m.paneByID(2) != c {
		t.Error("pane 2 is no longer C: ids were renumbered by the close")
	}

	d, err := m.OpenPane(OpenPaneRequest{PaneConfig: PaneConfig{Control: true}, Target: targetLargest})
	if err != nil {
		t.Fatalf("OpenPane: %v", err)
	}
	if d != 3 {
		t.Errorf("the pane opened after a close got id %d, want 3 (the closed slot must never be reused)", d)
	}

	states := map[int]string{}
	for _, entry := range m.buildPaneResults() {
		idx, _ := entry["pane"].(int)
		states[idx], _ = entry["state"].(string)
	}
	if states[1] != "closed" {
		t.Errorf("results report pane 1 as %q, want %q", states[1], "closed")
	}
	if states[2] != "panel" {
		t.Errorf("results report pane 2 as %q; C must still be reported at index 2", states[2])
	}
	if _, ok := states[3]; !ok {
		t.Error("results omit the newly opened pane 3")
	}
}

// TestCloseLastPaneQuits covers the case where the layout empties out. An empty
// tree has no root at all, and every render pass walks it.
func TestCloseLastPaneQuits(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)

	if err := m.ClosePane(0, false); err != nil {
		t.Fatalf("ClosePane(0): %v", err)
	}
	select {
	case <-m.quit:
		t.Fatal("magmux quit while a pane was still open")
	default:
	}

	if err := m.ClosePane(1, false); err != nil {
		t.Fatalf("ClosePane(1): %v", err)
	}
	select {
	case <-m.quit:
	default:
		t.Fatal("closing the last pane did not quit; magmux would sit on an empty layout forever")
	}
	if m.root != nil {
		t.Error("m.root survived the removal of every pane")
	}
	// The guard that makes the empty tree survivable rather than fatal.
	m.renderer.reset()
	m.renderer.renderPane(m.root)
}

// TestSplitRefusesBelowMinimumSize: the refusal has to happen BEFORE the fork,
// or a rejected open_pane still leaves an orphan process behind.
func TestSplitRefusesBelowMinimumSize(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	m.treeMu.Lock()
	m.root.resize(0, 0, 6, 30)
	m.treeMu.Unlock()

	if _, err := m.OpenPane(OpenPaneRequest{
		PaneConfig: PaneConfig{Control: true}, Target: 0, Split: SplitHorizontal,
	}); err == nil {
		t.Error("splitting 30 columns in half was allowed; each half is below the 20-column minimum")
	}
	if _, err := m.OpenPane(OpenPaneRequest{
		PaneConfig: PaneConfig{Control: true}, Target: 0, Split: SplitVertical,
	}); err == nil {
		t.Error("splitting 6 rows in half was allowed; each half is below the 3-row minimum")
	}
	if got := len(m.livePanes()); got != 1 {
		t.Errorf("a refused split still added a pane: %d live panes, want 1", got)
	}
}

// TestAutoSplitDirectionPicksLongerAxis pins the auto rule, which is what makes
// a sequence of open_pane calls reproduce buildGrid's two-column shape instead
// of a stack of one-line strips.
func TestAutoSplitDirectionPicksLongerAxis(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	// 120x39 — more than twice as wide as it is tall, so side by side.
	first, err := m.OpenPane(OpenPaneRequest{PaneConfig: PaneConfig{Control: true}, Target: 0})
	if err != nil {
		t.Fatalf("OpenPane: %v", err)
	}
	if m.root.splitType != SplitHorizontal {
		t.Errorf("a %dx%d pane split %v, want SplitHorizontal (the wider axis)",
			m.cols, m.rows, m.root.splitType)
	}

	// The right half is now ~59x39, no longer twice as wide as tall.
	second, err := m.OpenPane(OpenPaneRequest{PaneConfig: PaneConfig{Control: true}, Target: first})
	if err != nil {
		t.Fatalf("OpenPane: %v", err)
	}
	par := m.paneByID(second).parent
	if par.splitType != SplitVertical {
		t.Errorf("a tall-ish pane split %v, want SplitVertical", par.splitType)
	}
}

// TestReshapeClampsNegativeGeometry: w2 = p.w - w1 - 1 has no natural floor, so
// a tree that was legal when it was built goes negative when the terminal
// shrinks under it. The clamp belongs in reshapeChildren rather than in
// OpenPane precisely because SIGWINCH can do this at any time.
func TestReshapeClampsNegativeGeometry(t *testing.T) {
	m := newTestMux(t, ctrlPanes(4)...)

	for _, size := range [][2]int{{3, 3}, {1, 1}, {0, 0}, {2, 5}} {
		m.treeMu.Lock()
		m.root.resize(0, 0, size[0], size[1])
		m.treeMu.Unlock()

		eachLeaf(m.root, func(p *Pane) {
			if p.h < 0 || p.w < 0 {
				t.Fatalf("resize to %dx%d gave pane %d negative geometry %dx%d",
					size[0], size[1], p.id, p.h, p.w)
			}
			if p.screen.rows < 0 || p.screen.cols < 0 {
				t.Fatalf("resize to %dx%d gave pane %d a %dx%d screen",
					size[0], size[1], p.id, p.screen.rows, p.screen.cols)
			}
		})
	}

	// And back up again — the clamp must not have destroyed the tree.
	m.treeMu.Lock()
	m.root.resize(0, 0, 39, 120)
	m.treeMu.Unlock()
	eachLeaf(m.root, func(p *Pane) {
		if p.h <= 0 || p.w <= 0 {
			t.Errorf("pane %d is %dx%d after growing back to 120x39", p.id, p.h, p.w)
		}
	})
}

// ── unit: concurrency ───────────────────────────────────────────────────────

// TestConcurrentOpenCloseIsRaceFree runs every goroutine that touches the
// layout at once — the render loop, the input loop, the SIGWINCH handler, the
// socket dispatch and the pane-lifecycle verbs — against a layout being
// rebuilt underneath them.
//
// It is worth its runtime because three of those races existed BEFORE this
// work and were invisible only because no test resized or painted while the
// socket wrote: m.root.resize against renderPane, m.statusText, and the
// package-level sel. `-race` will report them from here.
//
// No subprocesses: PTY-less panes make the whole thing deterministic and fast,
// and the property under test is the locking, not the spawning.
func TestConcurrentOpenCloseIsRaceFree(t *testing.T) {
	defer deadlineWatchdog(t, 30*time.Second)()

	// render() writes the frame to m.out. Nothing here is checking pixels, and
	// several thousand frames of raw ANSI would make any real failure message
	// unreadable. Injecting the writer beats swapping os.Stdout: that swap was
	// itself an unsynchronised write to a package-level variable every other
	// test reads.
	m := newTestMux(t, ctrlPanes(4)...)
	m.out = io.Discard
	m.control.attach(m.paneByID(3))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					fn()
				}
			}
		}()
	}

	run(func() { m.render() })                        // the render loop
	run(func() { m.findPaneAt(3, 3); m.focusNext() }) // the input loop
	run(func() {                                      // SIGWINCH: writes geometry on every node
		m.treeMu.Lock()
		if m.root != nil {
			m.root.resize(0, 0, 20+len(m.allPanes)%15, 80+len(m.allPanes)%40)
		}
		m.treeMu.Unlock()
		m.control.markDirty()
	})
	run(func() { m.buildPaneResults() }) // a socket goroutine reading state
	run(func() {                         // a socket goroutine writing state
		m.dispatchSocketMsg(sockMsg{Type: "tint", Pane: "*", Color: "green"})
		m.dispatchSocketMsg(sockMsg{Type: "overlay", Pane: float64(0), Text: "hi"})
		m.dispatchSocketMsg(sockMsg{Type: "status", Text: "busy"})
		m.dispatchSocketMsg(sockMsg{Type: "list"})
	})
	run(func() { // pane lifecycle churn
		id, err := m.OpenPane(OpenPaneRequest{
			PaneConfig: PaneConfig{Control: true}, Target: targetLargest,
		})
		if err != nil {
			return
		}
		_ = m.ClosePane(id, false)
	})

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The original four must all still be addressable: churn that renumbered
	// or dropped one would be a silent mis-route on the wire.
	for id := 0; id < 4; id++ {
		if m.paneByID(id) == nil {
			t.Errorf("pane %d disappeared during the churn", id)
		}
	}
}

// ── end to end ──────────────────────────────────────────────────────────────

// waitForFile polls for a path, because a child's write is asynchronous with
// the reply that said the pane was open.
func waitForFile(t *testing.T, path string, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestOpenPaneOverSocket is the proof that open_pane starts a real process. The
// assertion is a file the child wrote: magmux cannot fake that, and neither can
// a reply that merely says a pane exists.
func TestOpenPaneOverSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mmxopen")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	proof := filepath.Join(dir, "proof")

	mux := startRPCMagmux(t, "-w", "-e", `sh -c "sleep 6"`)
	c := mux.dial()

	// The next free slot, asked for rather than assumed. Every session now gets
	// a control panel whether or not -c was passed — hidden by default, but
	// still holding an id in the append-only slot table — so the first pane an
	// agent opens is no longer necessarily 1. The invariant under test is that
	// the id is the next index of that table, which is what this reads.
	c.send(map[string]any{"type": "list", "id": "0"})
	before := replyOK(t, mustReply(t, c, "0"))
	beforePanes, _ := before["panes"].([]any)
	wantID := len(beforePanes)
	if wantID < 2 {
		t.Fatalf("list reported %d panes, want the session plus its hidden panel: %v", wantID, before)
	}

	c.send(map[string]any{
		"type": "open_pane", "id": "1",
		"cmd": fmt.Sprintf("echo ok > %s; sleep 1", proof),
	})
	res := replyOK(t, mustReply(t, c, "1"))
	newID, ok := res["pane"].(float64)
	if !ok {
		t.Fatalf("open_pane reply has no pane index: %v", res)
	}
	if int(newID) != wantID {
		t.Errorf("new pane got id %v, want %d (the next free slot)", newID, wantID)
	}

	if !waitForFile(t, proof, 10*time.Second) {
		t.Fatalf("the child never wrote %s: open_pane replied ok but started nothing", proof)
	}

	var exitIDs []int
	for _, ev := range c.drain() {
		if ev["type"] == "exit" {
			if v, ok := ev["pane"].(float64); ok {
				exitIDs = append(exitIDs, int(v))
			}
		}
	}
	found := false
	for _, id := range exitIDs {
		if id == int(newID) {
			found = true
		}
	}
	if !found {
		t.Errorf("no exit event for the new pane %d; exits were for panes %v", int(newID), exitIDs)
	}
}

// TestOpenPaneWithDirSetsChildCwd is the only real proof that cmd.Dir landed:
// nothing in the repo set it before this stage, so a silently ignored `cwd`
// would look exactly like success.
func TestOpenPaneWithDirSetsChildCwd(t *testing.T) {
	// Deliberately not t.TempDir(): on darwin that nests under /var/folders and
	// the resulting path is wide enough to wrap in a half-width pane, which
	// would make the screen assertion below read the wrong line.
	dir, err := os.MkdirTemp("/tmp", "mmxcwd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	base := filepath.Base(dir)

	mux := startRPCMagmux(t, "-w", "-e", `sh -c "sleep 6"`)
	c := mux.dial()

	c.send(map[string]any{
		"type": "open_pane", "id": "1",
		"cmd": "pwd; sleep 1", "cwd": dir,
	})
	res := replyOK(t, mustReply(t, c, "1"))
	newID := int(res["pane"].(float64))
	if got, _ := res["cwd"].(string); got != dir {
		t.Errorf("open_pane reply cwd = %q, want %q", got, dir)
	}

	var lastLine string
	for _, ev := range c.drain() {
		if ev["type"] == "exit" {
			if v, ok := ev["pane"].(float64); ok && int(v) == newID {
				lastLine, _ = ev["lastLine"].(string)
			}
		}
	}
	// The basename is a fresh random string, so nothing but a child that
	// actually started in that directory could have printed it.
	if !strings.Contains(lastLine, base) {
		t.Errorf("pane %d printed %q, which does not mention %q — cmd.Dir was not applied",
			newID, lastLine, base)
	}
}

// TestClosePaneTombstonesIndex checks the wire-visible half of the identity
// rule: after a close, the surviving pane keeps the index its client already
// knows, and the closed slot reports itself rather than vanishing.
func TestClosePaneTombstonesIndex(t *testing.T) {
	mux := startRPCMagmux(t, "-e", `sh -c "sleep 30"`, "-e", `sh -c "sleep 30"`)
	c := mux.dial()

	c.send(map[string]any{"type": "close_pane", "id": "1", "pane": 1})
	replyOK(t, mustReply(t, c, "1"))

	mux.quit()

	var results []any
	for _, ev := range c.drain() {
		if ev["type"] == "results" {
			results, _ = ev["panes"].([]any)
		}
	}
	if results == nil {
		t.Fatal("no results event — magmux did not shut down cleanly")
	}
	states := map[int]map[string]any{}
	for _, r := range results {
		entry, _ := r.(map[string]any)
		idx, ok := entry["pane"].(float64)
		if !ok {
			continue
		}
		states[int(idx)] = entry
	}
	zero, ok := states[0]
	if !ok {
		t.Fatalf("pane 0 is missing from results: %v", states)
	}
	if s, _ := zero["state"].(string); s == "closed" {
		t.Error("pane 0 is reported closed; closing pane 1 must not touch it")
	}
	one, ok := states[1]
	if !ok {
		t.Fatalf("pane 1 vanished from results instead of being tombstoned: %v", states)
	}
	if s, _ := one["state"].(string); s != "closed" {
		t.Errorf("pane 1 state = %q, want %q", s, "closed")
	}
	if closed, _ := one["closed"].(bool); !closed {
		t.Error("pane 1 is missing the closed flag")
	}
}

// TestClosePaneReapsChild: a leaked child leaves the pane's read loop parked on
// its PTY forever, so cleanup's wg.Wait never returns and magmux never exits.
// The failure signal is therefore a HANG, which is what the deadline catches.
func TestClosePaneReapsChild(t *testing.T) {
	mux := startRPCMagmux(t, "-w",
		"-e", `sh -c "sleep 300"`,
		"-e", `sh -c "echo hi; sleep 1"`)
	c := mux.dial()

	c.send(map[string]any{"type": "close_pane", "id": "1", "pane": 0})
	replyOK(t, mustReply(t, c, "1"))

	exited := make(chan error, 1)
	go func() { exited <- mux.cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("magmux exited with error after close_pane: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("magmux never auto-exited after close_pane: the closed pane's child or read loop leaked, " +
			"so cleanup's wg.Wait is still blocked")
	}
}

// mustReply is awaitReply with the events on the way past discarded.
func mustReply(t *testing.T, c *rpcConn, id string) map[string]any {
	t.Helper()
	ev, _ := c.awaitReply(id, nil)
	return ev
}

// keep encoding/json referenced for the map literals above when the file is
// edited down; the harness marshals every send.
var _ = json.Marshal
