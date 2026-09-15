package mux

import (
	"syscall"
	"time"
)

// ── Dynamic panes ────────────────────────────────────────────────────────────

// A pane below these is unusable: no room for a prompt plus a line of output,
// and no room for a path. Enforced on BOTH halves of a split, because the
// interesting failure is the one that shrinks the pane you were splitting.
const (
	minPaneRows = 3
	minPaneCols = 20
)

// OpenPaneRequest.Target sentinels.
const (
	targetFocused = -1 // split whichever pane has focus
	targetLargest = -2 // split the largest live leaf
)

var (
	// errPaneTooSmall carries the code mcp_tools.go's openPaneHint branches on.
	errPaneTooSmall = sockErrf(sockCodeTooSmall,
		"no room to split: each half needs at least %d rows and %d columns", minPaneRows, minPaneCols)
	errTargetGone = sockErrf(sockCodeNoSuchPane,
		"the pane to split is no longer part of the layout")
	errClosing = sockErrf(sockCodeUnsupported, "magmux is shutting down")
)

type OpenPaneRequest struct {
	PaneConfig
	Target int       // pane id to split; -1 = focused, -2 = largest live leaf
	Split  SplitType // SplitNone = auto (split the longer axis)
	Ratio  float64   // 0 => 0.5
	Focus  bool
}

// OpenPane splits an existing leaf and spawns a child in the new half.
//
// The ordering below is the whole design: the slow part (pty.Open, fork/exec,
// the controller's filesystem probing) runs with NO lock held, and treeMu is
// taken only for the pointer surgery and reflow, which are microseconds. That
// is why this can run straight on the socket goroutine instead of being queued
// onto the render loop.
//
// Safe from any goroutine. Caller must NOT hold treeMu or p.mu.
func (m *Magmux) OpenPane(req OpenPaneRequest) (int, error) {
	// 1. Resolve the target and read its geometry, then let go.
	m.treeMu.RLock()
	if m.closing {
		m.treeMu.RUnlock()
		return -1, errClosing
	}
	t := m.resolveSplitTargetLocked(req.Target)
	if t == nil {
		m.treeMu.RUnlock()
		if req.Target >= 0 {
			return -1, sockErrf(sockCodeNoSuchPane, "no pane %d", req.Target)
		}
		return -1, errTargetGone
	}
	st := req.Split
	if st == SplitNone {
		// Split the longer axis. The 2:1 bias reproduces buildGrid's
		// two-column shape organically: on an 80x24 terminal a full pane is
		// 80 >= 48, so the first split is vertical-border/side-by-side and the
		// halves then split top/bottom, exactly like the static grid.
		if t.w >= 2*t.h {
			st = SplitHorizontal
		} else {
			st = SplitVertical
		}
	}
	ratio := req.Ratio
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.5
	}
	ty, tx, th, tw := t.y, t.x, t.h, t.w
	gridMode := m.gridMode
	m.treeMu.RUnlock()

	// 2. Refuse before spawning anything if either half would be unusable.
	var ny, nx, nh, nw int
	if st == SplitHorizontal {
		w1 := int(float64(tw) * ratio)
		w2 := tw - w1 - 1
		if w1 < minPaneCols || w2 < minPaneCols || th < minPaneRows {
			return -1, errPaneTooSmall
		}
		ny, nx, nh, nw = ty, tx+w1+1, th, w2
	} else {
		h1 := int(float64(th) * ratio)
		h2 := th - h1 - 1
		if h1 < minPaneRows || h2 < minPaneRows || tw < minPaneCols {
			return -1, errPaneTooSmall
		}
		ny, nx, nh, nw = ty+h1+1, tx, h2, tw
	}

	// 3. The slow part, outside every lock.
	np, err := newPaneFor(ny, nx, nh, nw, req.PaneConfig)
	if err != nil {
		return -1, sockErrf(sockCodeInternal, "could not start %q: %v", req.Cmd, err)
	}

	// 4. Still private — nothing else can reach np yet, so this needs no lock,
	// and attaching the controller here means p.controller is written before
	// any other goroutine could read it (and Start's filesystem work stays off
	// treeMu). Missing gridMode here would give the pane different writePTY
	// suppression and no DONE overlay compared with its siblings.
	np.gridMode = gridMode
	m.attachController(np)

	// 5. Publish.
	m.treeMu.Lock()
	if m.closing {
		m.treeMu.Unlock()
		m.unwindPane(np)
		return -1, errClosing
	}
	if !m.splitTargetIntactLocked(t) {
		m.treeMu.Unlock()
		m.unwindPane(np)
		return -1, errTargetGone
	}
	np.id = len(m.allPanes)
	m.allPanes = append(m.allPanes, np)
	m.splitLeafLocked(t, np, st, ratio)
	if !np.isControl {
		// Under the same lock cleanup() sets m.closing, which is what keeps
		// this Add from racing wg.Wait.
		m.wg.Add(1)
	}
	if req.Focus {
		m.focused = np
	}
	id := np.id
	m.treeMu.Unlock()

	// The panel's route table marks the focused pane with a ▸, and it is told
	// by whoever moved the focus: focusNext, parseSGRMouse, ClosePane and
	// sockFocus all do. This did not, so `focus:true` left the marker on the
	// pane the agent had just navigated away from — the instrument disagreeing
	// with magmux about a fact magmux owns. Outside the lock, like ClosePane's,
	// because cp.mu is a second lock and there is nothing here that needs both.
	if req.Focus {
		m.control.setFocused(id)
	}

	// 6. Start the pane's goroutines now that it is reachable. The waiter is
	// NOT conditional on grid mode: it is the only caller of cmd.Wait, so
	// without it every close_pane in a non-grid session leaves a zombie and
	// p.reaped never becomes true. What grid mode gates is the tombstone it
	// paints, inside waitForChild.
	if !np.isControl {
		go np.readLoop(&m.wg)
		go m.waitForChild(np)
	}

	// 7. Tell subscribers.
	ev := map[string]any{"type": "pane_opened", "pane": id, "cmd": req.Cmd}
	if req.Dir != "" {
		ev["cwd"] = req.Dir
	}
	if req.Label != "" {
		ev["label"] = req.Label
	}
	m.broadcastEvent(ev)
	return id, nil
}

// ClosePane detaches a pane from the layout and reaps its child. The allPanes
// slot is retained as a tombstone so ids never shift.
//
// Note the two-state model this preserves: `dead` means the process is gone but
// the pane is still on screen with its ✓ DONE / ✗ FAIL overlay — a self-exit
// never auto-collapses, because in grid mode the finished grid IS the report.
// `closed` means the pane itself is gone. close_pane on a dead pane is the
// normal way to reclaim its space.
//
// Caller must NOT hold treeMu or p.mu.
func (m *Magmux) ClosePane(id int, force bool) error {
	m.treeMu.Lock()
	p := m.paneByIDLocked(id)
	if p == nil {
		m.treeMu.Unlock()
		return sockErrf(sockCodeNoSuchPane, "no pane %d", id)
	}

	// Compute the sibling BEFORE the surgery: focus prefers a leaf under it,
	// which is what tmux does and what keeps focus near where you were.
	var sib *Pane
	if p.parent != nil {
		sib = p.parent.child1
		if sib == p {
			sib = p.parent.child2
		}
	}

	p.closed = true
	m.removeLeafLocked(p)

	refocused := -1
	if m.focused == p || m.focused == nil || m.focused.closed {
		m.focused = firstLiveLeaf(sib)
		if m.focused == nil {
			m.focused = firstLiveLeaf(m.root)
		}
		if m.focused != nil {
			refocused = m.focused.id
		}
	}
	if sel.pane == p {
		m.selClear()
	}

	// "Was that the last one?" is answered under the same lock that removed
	// it, so two concurrent closes cannot both decide it was not.
	//
	// Two conditions, because the control panel is chrome rather than a
	// session: an empty layout obviously has nothing left to show, and so does
	// a magmux whose last SESSION is gone — with only the panel left, -w can
	// never fire (allPanesDone needs at least one session) and the window would
	// sit there forever. The everSession guard keeps a panel-only magmux, which
	// never had a session to lose, from quitting on its first close.
	liveTotal, liveSessions, everSession := 0, 0, false
	for _, q := range m.allPanes {
		if q == nil {
			continue
		}
		if !q.isControl {
			everSession = true
		}
		if q.closed {
			continue
		}
		liveTotal++
		if !q.isControl {
			liveSessions++
		}
	}
	lastOne := liveTotal == 0 || (everSession && liveSessions == 0)
	m.treeMu.Unlock()

	// Everything below is blocking or takes another lock, so it runs after the
	// unlock.
	//
	// The panel stops painting rather than dereferencing a detached pane, and
	// its focus marker moves with the focus we just fixed — focusNext and
	// sockFocus both tell it, and a close that did not would leave the table's
	// ▸ pointing at a pane that no longer exists until the next focus change.
	m.control.detach(p)
	if refocused >= 0 {
		m.control.setFocused(refocused)
	}

	// Releasing the transcript claim is not optional: claimedSessions is never
	// otherwise cleaned, and a stranded entry leaves the next pane in the same
	// project stuck in `starting` silently and forever.
	m.releaseSessions(p)
	m.reapPane(p, force)
	m.broadcastEvent(map[string]any{"type": "pane_closed", "pane": id})

	if lastOne {
		m.quitOnce.Do(func() { close(m.quit) })
	}
	return nil
}

// splitTargetIntactLocked re-verifies, after the fork, that the target OpenPane
// resolved before it is still a node a new pane can be spliced onto.
//
// Everything the fork/exec window can do to it: a concurrent close_pane
// tombstones the id slot, and Ctrl-G p HIDES the panel — removeLeafLocked
// detaches it from the tree while leaving it very much alive and in the id
// table. Splicing onto either attaches the new pane to a subtree m.root cannot
// reach: alive, never painted, undismissable. Hidden is the third state, not a
// synonym for closed, and the id check alone only ever caught the second.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) splitTargetIntactLocked(t *Pane) bool {
	return t != nil && m.paneByIDLocked(t.id) == t && !t.hidden
}

// resolveSplitTargetLocked turns a Target into a live LEAF pane, or nil.
// Caller holds at least treeMu.RLock.
func (m *Magmux) resolveSplitTargetLocked(target int) *Pane {
	if target >= 0 {
		// A hidden pane has no place in the tree to splice onto, and its
		// geometry is whatever it was when it was taken out.
		p := m.paneByIDLocked(target)
		if p == nil || p.hidden || p.splitType != SplitNone {
			return nil
		}
		return p
	}
	if target == targetFocused {
		// isControl is filtered here for the same reason firstLiveLeaf,
		// allPanesDone and buildPaneResults filter it: the panel is magmux's own
		// chrome, not a place to put a session. focusNext filters only
		// !p.hidden, so `Ctrl-G Tab` really does park focus on a VISIBLE panel —
		// with `magmux -c -e claude` a human Tabs onto it to read the ledger, an
		// MCP client calls open_pane with no target, and the agent's pane gets
		// halved out of the control panel's column and nested inside it.
		if p := m.focused; p != nil && !p.closed && !p.hidden && !p.isControl && p.splitType == SplitNone {
			return p
		}
		// Focus can be nil after the last pane closed, or on a pane that was
		// just detached. Falling back beats refusing.
	}
	return m.largestLiveLeafLocked()
}

// largestLiveLeafLocked returns the live leaf with the most cells, which is the
// one a split hurts least — preferring a real SESSION over the control panel,
// exactly as firstLiveLeaf does and for the same reason.
//
// The panel is magmux's own chrome, and it is frequently the biggest thing on
// screen because it is a full column: with `magmux -c -e claude` an untargeted
// open_pane would halve it and nest the agent's session inside the instrument
// that is supposed to be watching it. The panel is only ever nominated when
// there is no session leaf left at all, which in a running magmux means the
// last session just closed and the layout is on its way out.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) largestLiveLeafLocked() *Pane {
	var best, fallback *Pane
	bestArea, fallbackArea := -1, -1
	for _, p := range m.allPanes {
		// Hidden panes carry stale geometry — the size they had when they left
		// the tree — so an unqualified h*w would happily nominate one.
		if p == nil || p.closed || p.hidden || p.splitType != SplitNone {
			continue
		}
		a := p.h * p.w
		if p.isControl {
			if a > fallbackArea {
				fallback, fallbackArea = p, a
			}
			continue
		}
		if a > bestArea {
			best, bestArea = p, a
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// firstLiveLeaf returns the first live leaf under node, preferring a real
// session over the control panel — focus on the panel does nothing visible and
// looks broken.
func firstLiveLeaf(node *Pane) *Pane {
	var fallback *Pane
	var walk func(*Pane) *Pane
	walk = func(p *Pane) *Pane {
		if p == nil || p.closed {
			return nil
		}
		if p.splitType == SplitNone {
			if p.isControl {
				if fallback == nil {
					fallback = p
				}
				return nil
			}
			return p
		}
		if f := walk(p.child1); f != nil {
			return f
		}
		return walk(p.child2)
	}
	if f := walk(node); f != nil {
		return f
	}
	return fallback
}

// splitLeafLocked splices a FRESH internal node in place of leaf t, with t as
// child1 and np as child2, then reflows.
//
// The leaf is never converted in place. It owns screen, ptmx, cmd and
// vt.node == p, and it is pointed at from outside the tree by
// ClaudeCodeController.pane, ControlPanel.pane, sel.pane and m.claimedSessions;
// turning it into an internal node would strand every one of them.
//
// Reflow is the existing reshapeChildren → resize path, which for a leaf does
// screen.resize + pty.SetWinSize. No new geometry code exists anywhere in this file.
//
// Caller holds treeMu.Lock.
func (m *Magmux) splitLeafLocked(t, np *Pane, st SplitType, ratio float64) {
	m.splitNodeLocked(t, np, st, ratio, false)
}

// splitNodeLocked is splitLeafLocked with the side chosen by the caller, and
// with `t` allowed to be an internal node.
//
// Both generalisations exist for one caller: re-showing the control panel.
// removeLeafLocked collapsed the panel's parent into its SIBLING, and that
// sibling is usually a whole column rather than a leaf; putting the panel back
// where it was means splitting that node again, on the side it was on. Nothing
// else here changes — the parent is still a FRESH node and `t` is still never
// converted in place, which is the invariant that keeps ControlPanel.pane,
// sel.pane and the controllers' back-pointers valid.
//
// Caller holds treeMu.Lock.
func (m *Magmux) splitNodeLocked(t, np *Pane, st SplitType, ratio float64, npFirst bool) {
	par := &Pane{
		splitType: st,
		y:         t.y, x: t.x, h: t.h, w: t.w,
		ratio:  ratio,
		parent: t.parent,
		child1: t,
		child2: np,
	}
	if npFirst {
		par.child1, par.child2 = np, t
	}
	switch {
	case t.parent == nil:
		m.root = par
	case t.parent.child1 == t:
		t.parent.child1 = par
	default:
		t.parent.child2 = par
	}
	t.parent = par
	np.parent = par
	par.reshapeChildren()
}

// removeLeafLocked detaches leaf t and collapses its parent into its sibling,
// which inherits the parent's EXACT geometry — so the space t occupied plus the
// border between them goes to the sibling with no gap and no overlap.
//
// Caller holds treeMu.Lock.
func (m *Magmux) removeLeafLocked(t *Pane) {
	par := t.parent
	if par == nil {
		// t was the whole tree. An empty tree is legal for exactly as long as
		// it takes ClosePane to quit; renderPane's nil guard covers the frames
		// in between.
		if m.root == t {
			m.root = nil
		}
		return
	}
	sib := par.child1
	if sib == t {
		sib = par.child2
	}
	sib.parent = par.parent
	switch {
	case par.parent == nil:
		m.root = sib
	case par.parent.child1 == par:
		par.parent.child1 = sib
	default:
		par.parent.child2 = sib
	}
	t.parent = nil
	sib.resize(par.y, par.x, par.h, par.w)
}

// unwindPane throws away a pane that was spawned but never published.
//
// OpenPane forks before it takes the write lock, so a teardown or a concurrent
// close of the split target can leave a fully-started child that no goroutine
// owns: wg.Add, readLoop and waitForChild all happen after publication, so
// nothing would ever call cmd.Wait and the child would sit as a zombie for the
// life of magmux. reapPane alone is not enough — SIGHUP and closing the PTY
// end the process, they do not collect it.
//
// The wait runs in its own goroutine, and NOT under m.wg: this pane was never
// added to the WaitGroup, and adding it here would race the wg.Wait that the
// m.closing unwind path exists to keep clear of.
func (m *Magmux) unwindPane(p *Pane) {
	m.reapPane(p, false)
	if p != nil && p.cmd != nil {
		go m.reapChild(p)
	}
}

// reapPane terminates a pane's child. Called with NO lock held: signalling and
// closing the PTY are blocking operations, and closing ptmx is precisely what
// unblocks readLoop's Read — which is what sets p.dead and calls wg.Done.
func (m *Magmux) reapPane(p *Pane, force bool) {
	if p == nil || p.isControl {
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGHUP)
	}
	p.mu.Lock()
	ptmx := p.ptmx
	p.mu.Unlock()
	if ptmx != nil {
		_ = ptmx.Close()
	}
	if !force || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	pid := p.cmd.Process.Pid
	go func() {
		time.Sleep(2 * time.Second)
		// The NEGATIVE pid is the process GROUP, which is correct because
		// spawnPTY sets Setsid — a shell that ignored SIGHUP would otherwise
		// leave its own children behind. The reaped check is what stops a
		// delayed kill landing on a stranger: only after cmd.Wait has returned
		// is the pid free for the OS to hand to somebody else.
		p.mu.Lock()
		reaped := p.reaped
		p.mu.Unlock()
		if reaped {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()
}
