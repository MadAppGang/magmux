package main

// magmux's own chrome: the control panel and the status bar, and the two
// chords that show and hide them.
//
// The subject here is a DEFAULT, not a feature: `magmux -e claude` must be
// indistinguishable from a bare terminal running Claude Code. Everything that
// makes magmux visible — the panel, the border between panes, the status row —
// is either absent or one keystroke away, and -c means "start with the panel
// already up" so that every invocation written before this change looks exactly
// as it did.
//
// The unit tests run against PTY-less panes for the same reason panes_test.go
// does: the whole tree, id and locking path is exercised with no process and no
// timing, so a failure names the layout code rather than a fixture's quoting.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// muxWithPanel is a layout of n fake sessions plus a hidden control panel —
// what `magmux -e … ` now builds.
func muxWithPanel(t *testing.T, n int) (*Magmux, *Pane) {
	t.Helper()
	m := newTestMux(t, ctrlPanes(n)...)
	p := m.installHiddenPanel()
	if !p.hidden {
		t.Fatalf("installHiddenPanel produced a panel that is not hidden")
	}
	if p.parent != nil || m.nodeInTreeLocked(p) {
		t.Fatalf("the hidden panel is still spliced into the layout tree")
	}
	return m, p
}

// muxWithVisiblePanel is the -c shape: the panel built INTO the layout as the
// last pane, which for four or more panes puts it nested in the right-hand
// column rather than directly under the root. That nesting is the only case in
// which the remembered anchor differs from "the root", so it is the only case
// that can prove the anchor is used at all.
func muxWithVisiblePanel(t *testing.T, n int) (*Magmux, *Pane) {
	t.Helper()
	m := newTestMux(t, ctrlPanes(n+1)...)
	p := m.paneByID(n)
	m.treeMu.Lock()
	m.panel = p
	m.treeMu.Unlock()
	m.control.attach(p)
	if p.hidden || p.parent == nil {
		t.Fatalf("the -c panel should start in the tree")
	}
	return m, p
}

// geom is every live pane's rectangle, keyed by id: the thing a show/hide cycle
// has to return to byte for byte.
type geom map[int][4]int

func snapGeom(m *Magmux) geom {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	g := geom{}
	for _, p := range m.livePanesLocked(nil) {
		if p.hidden {
			continue
		}
		g[p.id] = [4]int{p.y, p.x, p.h, p.w}
	}
	return g
}

func (g geom) String() string {
	var b strings.Builder
	for id, r := range g {
		fmt.Fprintf(&b, "p%d=(y%d x%d h%d w%d) ", id, r[0], r[1], r[2], r[3])
	}
	return b.String()
}

func sameGeom(a, b geom) bool {
	if len(a) != len(b) {
		return false
	}
	for id, r := range a {
		if b[id] != r {
			return false
		}
	}
	return true
}

// statusBarWidth is what renderStatusBar will actually paint, measured with
// control.go's own visWidth so the digest is held to the same ruler the panel
// is. renderStatusBar's column accounting is per-segment, so this reproduces it
// segment by segment rather than measuring the escape-laden frame.
func statusBarWidth(text string) int {
	w := 1 // the bar's leading pad
	first := true
	for _, seg := range strings.Split(text, "\t") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		txt := seg
		code := ""
		if parts := strings.SplitN(seg, ":", 2); len(parts) == 2 {
			code = strings.TrimSpace(parts[0])
			txt = strings.TrimSpace(parts[1])
		}
		if !first {
			w += 3 // " │ "
		}
		first = false
		w += visWidth(txt)
		switch code {
		case "P", "Pr", "Py":
			w += 2 // the pill's padding
		case "*":
			w += 2 // the "* " renderStatusBar writes in front of the label
		}
	}
	return w
}

// paneName identifies a pane in a failure message without dumping the struct,
// which is several hundred cells of Screen.
func paneName(p *Pane) string {
	if p == nil {
		return "nil"
	}
	if p.isControl {
		return fmt.Sprintf("the control panel (pane %d)", p.id)
	}
	return fmt.Sprintf("pane %d", p.id)
}

// ── the panel starts hidden ─────────────────────────────────────────────────

// TestPanelVisibilityAtStartup is the back-compat contract in one test.
//
// Without -c the panel exists, holds an id, is reported as state:"panel" — and
// is not on screen. With -c it is on screen, which is what every existing
// invocation (task pilot:demo, task mcp:demo, test/ui/case3.ts and case4.ts)
// depends on for its appearance.
//
// End-to-end rather than in-process because the flag parsing that decides this
// lives in main(), and a unit test would be asserting against a hand-built
// struct instead of against the flag.
func TestPanelVisibilityAtStartup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantHidden bool
	}{
		{"bare", []string{"-e", `sh -c "sleep 6"`}, true},
		{"with -c", []string{"-c", "-e", `sh -c "sleep 6"`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := startRPCMagmux(t, tc.args...)
			c := mux.dial()
			c.send(map[string]any{"type": "list", "id": "l"})
			res := replyOK(t, mustReply(t, c, "l"))
			panes, _ := res["panes"].([]any)

			var panel map[string]any
			for _, v := range panes {
				e, _ := v.(map[string]any)
				if e != nil && e["state"] == "panel" {
					panel = e
				}
			}
			if panel == nil {
				b, _ := json.Marshal(panes)
				t.Fatalf("no control panel in the layout; every session gets one: %s", b)
			}
			if hidden, _ := panel["hidden"].(bool); hidden != tc.wantHidden {
				t.Errorf("panel hidden=%v, want %v (args %v)", hidden, tc.wantHidden, tc.args)
			}
			// The guard test/ui/case3.ts makes: hidden or not, it is a panel.
			if panel["state"] != "panel" {
				t.Errorf("panel reported state=%v, want \"panel\"", panel["state"])
			}
		})
	}
}

// TestHiddenPanelIsStillReportedAsAPanel pins the same thing at the source, so
// a regression names buildPaneResults rather than a subprocess.
func TestHiddenPanelIsStillReportedAsAPanel(t *testing.T) {
	m, panel := muxWithPanel(t, 1)

	var entry map[string]any
	for _, e := range m.buildPaneResults() {
		if e["pane"] == panel.id {
			entry = e
		}
	}
	if entry == nil {
		t.Fatalf("the hidden panel is missing from results entirely")
	}
	if entry["state"] != "panel" {
		t.Errorf("hidden panel reported state=%v, want \"panel\" — test/ui/case3.ts asserts this", entry["state"])
	}
	if hidden, _ := entry["hidden"].(bool); !hidden {
		t.Errorf("results does not say the panel is hidden: %v", entry)
	}
	if closed, _ := entry["closed"].(bool); closed {
		t.Errorf("a hidden panel is reported as CLOSED; hidden is a third state, not a tombstone: %v", entry)
	}
}

// ── a lone session pane is a bare terminal ──────────────────────────────────

// TestLoneSessionPaneFillsTheTerminalWithNoBorder is part 3 of the feature and
// the reason the panel starts hidden at all: with one pane there is nothing to
// divide, so magmux must draw nothing.
//
// renderBorder only ever paints a SPLIT node, so the assertion on the painted
// frame is what proves the tree really did collapse to a single leaf rather
// than to a split with one empty half.
func TestLoneSessionPaneFillsTheTerminalWithNoBorder(t *testing.T) {
	m, panel := muxWithPanel(t, 1)

	if m.root != m.paneByID(0) {
		t.Fatalf("the session is not the whole tree; the hidden panel is still in the layout")
	}
	if !panel.hidden {
		t.Fatalf("the panel is not marked hidden; every geometry and paint guard reads that flag")
	}
	if panel.parent != nil {
		t.Errorf("the hidden panel still has a parent: it was not detached")
	}
	p := m.paneByID(0)
	wantH, wantW := m.rows-1, m.cols // minus the status row
	if p.y != 0 || p.x != 0 || p.h != wantH || p.w != wantW {
		t.Errorf("lone pane is (y%d x%d h%d w%d), want (y0 x0 h%d w%d)",
			p.y, p.x, p.h, p.w, wantH, wantW)
	}

	var r Renderer
	r.reset()
	m.treeMu.RLock()
	r.renderPane(m.root)
	m.treeMu.RUnlock()
	if f := r.frame(); strings.ContainsAny(f, "│─") {
		t.Errorf("a single pane painted a border rule; magmux must be invisible here")
	}
}

// ── Ctrl-G p ────────────────────────────────────────────────────────────────

// TestPanelToggleRestoresGeometryExactly is the invariant that makes the toggle
// safe to use mid-session: showing and hiding again must land every pane back
// on the same rectangle, or a TUI redrawing after the SIGWINCH comes back
// subtly wrong every time somebody glances at the panel.
//
// Both directions are checked, because they use different machinery —
// splitNodeLocked on the way in, removeLeafLocked on the way out.
func TestPanelToggleRestoresGeometryExactly(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("%dpanes", n), func(t *testing.T) {
			m, panel := muxWithPanel(t, n)
			before := snapGeom(m)

			m.togglePanel()
			if panel.hidden {
				t.Fatalf("Ctrl-G p did not show the panel")
			}
			shown := snapGeom(m)
			if sameGeom(before, shown) {
				t.Errorf("showing the panel changed no geometry; it is not taking any space")
			}
			if panel.w <= 0 || panel.h <= 0 {
				t.Errorf("the shown panel is %dx%d — degenerate", panel.w, panel.h)
			}

			m.togglePanel()
			if !panel.hidden {
				t.Fatalf("Ctrl-G p did not hide the panel again")
			}
			if after := snapGeom(m); !sameGeom(before, after) {
				t.Errorf("a show/hide cycle did not restore the layout\n before: %v\n  after: %v", before, after)
			}

			// And the cycle must be idempotent, not merely reversible once.
			m.togglePanel()
			if again := snapGeom(m); !sameGeom(shown, again) {
				t.Errorf("the second show landed somewhere else\n first: %v\nsecond: %v", shown, again)
			}
		})
	}
}

// TestVisiblePanelHideShowCycleIsExact is the same property from the -c side,
// and it is the one that proves the ANCHOR is doing work.
//
// A panel that starts hidden is always re-inserted at the root, so "remember
// where it was" and "put it beside everything" are indistinguishable there. A
// -c layout with four or more panes nests the panel inside the right-hand
// column, and only an anchor puts it back in that column: falling back to the
// root would return it as a full-height column beside the whole tree, which
// looks plausible and is not where it was.
func TestVisiblePanelHideShowCycleIsExact(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("%dsessions", n), func(t *testing.T) {
			m, panel := muxWithVisiblePanel(t, n)
			before := snapGeom(m)
			parentSplit, parentRatio := panel.parent.splitType, panel.parent.ratio
			wasChild2 := panel.parent.child2 == panel

			m.togglePanel()
			if !panel.hidden {
				t.Fatalf("the panel did not hide")
			}
			m.togglePanel()
			if panel.hidden {
				t.Fatalf("the panel did not come back")
			}

			if after := snapGeom(m); !sameGeom(before, after) {
				t.Errorf("the -c layout did not survive a hide/show\n before: %v\n  after: %v", before, after)
			}
			if panel.parent == nil {
				t.Fatalf("the panel came back with no parent")
			}
			if panel.parent.splitType != parentSplit {
				t.Errorf("the panel came back under a %v split, was %v", panel.parent.splitType, parentSplit)
			}
			if panel.parent.ratio != parentRatio {
				t.Errorf("the panel came back at ratio %v, was %v", panel.parent.ratio, parentRatio)
			}
			if (panel.parent.child2 == panel) != wasChild2 {
				t.Errorf("the panel came back on the other side of its split")
			}
		})
	}
}

// TestShownPanelReturnsToTheRightHandColumn pins WHERE it comes back. The
// anchor is the sibling that inherited the space, so the panel goes back beside
// it on the side it left from — not merely somewhere with room.
func TestShownPanelReturnsToTheRightHandColumn(t *testing.T) {
	m, panel := muxWithPanel(t, 2)

	m.togglePanel()
	if panel.parent == nil {
		t.Fatalf("the shown panel has no parent")
	}
	if panel.parent.child2 != panel {
		t.Errorf("the panel came back as child1 (the left-hand column); it belongs on the right")
	}
	sessions := m.paneByID(0)
	if panel.x <= sessions.x {
		t.Errorf("the panel (x%d) is not to the right of the sessions (x%d)", panel.x, sessions.x)
	}
}

// TestHidingThePanelKeepsItsHistory is the hidden-vs-closed distinction stated
// as a test. `closed` is a permanent tombstone; hiding must not be one.
func TestHidingThePanelKeepsItsHistory(t *testing.T) {
	m, panel := muxWithPanel(t, 1)
	m.control.attach(panel)
	m.control.recordSend(0, "step 1/1", "run the tests")

	m.togglePanel() // show
	m.togglePanel() // hide

	if panel.closed {
		t.Fatalf("hiding the panel closed it; a tombstone is permanent and this is not")
	}
	if panel.dead {
		t.Fatalf("hiding the panel marked it dead; it has no process to die")
	}
	if d := m.control.digest(); d.sent != 1 {
		t.Errorf("the panel lost its ledger across a hide: sent=%d, want 1", d.sent)
	}
	if m.panelLocked() != panel {
		t.Errorf("a hidden panel is no longer reachable; Ctrl-G p could never bring it back")
	}
}

// ── focus ───────────────────────────────────────────────────────────────────

// TestHidingAFocusedPanelMovesFocusToASession: focus left on something that is
// not on screen sends every keystroke into the void.
func TestHidingAFocusedPanelMovesFocusToASession(t *testing.T) {
	m, panel := muxWithPanel(t, 2)
	m.togglePanel()

	m.treeMu.Lock()
	m.focused = panel
	m.treeMu.Unlock()

	m.togglePanel()
	if f := m.focusedPane(); f == nil || f == panel {
		t.Fatalf("focus stayed on the hidden panel (%v); every keystroke would go nowhere", f)
	}
}

// TestShowingThePanelDoesNotStealFocus is the other half, and the more
// important one: the human is typing into their agent, and revealing an
// instrument must not take the keyboard away mid-sentence. main() moves focus
// off the panel at startup for exactly this reason.
func TestShowingThePanelDoesNotStealFocus(t *testing.T) {
	m, panel := muxWithPanel(t, 2)
	want := m.paneByID(1)

	m.treeMu.Lock()
	m.focused = want
	m.treeMu.Unlock()

	m.togglePanel()
	if got := m.focusedPane(); got != want {
		t.Errorf("showing the panel moved focus to %v, want pane %d", got, want.id)
	}
	if m.focusedPane() == panel {
		t.Errorf("showing the panel focused it")
	}
}

// TestFocusCyclingSkipsAHiddenPanel — a pane nobody can see is a pane focus
// must not stop on, the same rule tombstones follow.
func TestFocusCyclingSkipsAHiddenPanel(t *testing.T) {
	m, panel := muxWithPanel(t, 2)
	for i := 0; i < 6; i++ {
		m.focusNext()
		if m.focusedPane() == panel {
			t.Fatalf("focusNext landed on the hidden panel after %d steps", i+1)
		}
	}
}

// ── refusing gracefully ─────────────────────────────────────────────────────

// TestShowingThePanelRefusesOnANarrowTerminal. reshapeChildren clamps at zero
// rather than going negative, so a too-narrow show would not crash — it would
// produce a panel with no columns in it, which is worse than not showing one,
// because the session has lost the space and gained nothing.
func TestShowingThePanelRefusesOnANarrowTerminal(t *testing.T) {
	m := &Magmux{rows: 24, cols: 30, gridMode: true, quit: make(chan struct{}), control: newControlPanel()}
	if err := m.buildGrid(ctrlPanes(1)); err != nil {
		t.Fatalf("buildGrid: %v", err)
	}
	panel := m.installHiddenPanel()
	before := snapGeom(m)

	m.togglePanel()

	if !panel.hidden {
		t.Fatalf("the panel was shown on a %d-column terminal; each half would be under %d columns",
			m.cols, minPaneCols)
	}
	if after := snapGeom(m); !sameGeom(before, after) {
		t.Errorf("a refused show still disturbed the layout\n before: %v\n  after: %v", before, after)
	}
	m.treeMu.RLock()
	note := m.chromeNoteLocked()
	m.treeMu.RUnlock()
	if note == "" {
		t.Errorf("the refusal was silent; it must be said in the status bar")
	}

	// Wide enough, and it works — proving the refusal is about the width and
	// not about the toggle being broken.
	m.treeMu.Lock()
	m.cols = 120
	m.reflowLocked()
	m.treeMu.Unlock()
	m.togglePanel()
	if panel.hidden {
		t.Errorf("the panel still refuses at 120 columns")
	}
}

// TestHiddenPanelIsNeverASplitTarget. Its geometry is whatever it was when it
// left the tree, so an unqualified h*w in largestLiveLeafLocked would nominate
// it and an agent's open_pane would land in a subtree nothing paints.
func TestHiddenPanelIsNeverASplitTarget(t *testing.T) {
	m, panel := muxWithPanel(t, 2)

	// The stale geometry that makes this a hazard rather than a hypothetical:
	// a panel hidden from a maximised state keeps the numbers it had, and they
	// beat every pane that is actually on screen.
	m.treeMu.Lock()
	panel.h, panel.w = m.rows*4, m.cols*4
	m.focused = panel
	m.treeMu.Unlock()

	m.treeMu.RLock()
	largest := m.largestLiveLeafLocked()
	byID := m.resolveSplitTargetLocked(panel.id)
	focusedT := m.resolveSplitTargetLocked(targetFocused)
	m.treeMu.RUnlock()

	if largest == panel {
		t.Errorf("largestLiveLeafLocked picked the hidden panel")
	}
	if byID != nil {
		t.Errorf("open_pane targeting the hidden panel resolved to %v, want a refusal", byID)
	}
	if focusedT == panel {
		t.Errorf("the focused-split target resolved to the hidden panel")
	}
}

// ── Ctrl-G s ────────────────────────────────────────────────────────────────

// visiblePanelMux is hintMux with the panel brought on screen: n real sessions
// plus a panel a human can Tab onto, which is the state the tests below are
// about. hintMux alone leaves the panel hidden, and hidden is already covered.
func visiblePanelMux(t *testing.T, cols, n int) (*Magmux, *Pane) {
	t.Helper()
	m := hintMux(t, cols, n)
	m.treeMu.RLock()
	panel := m.panel
	m.treeMu.RUnlock()
	if panel == nil {
		t.Fatalf("hintMux built no panel")
	}
	m.togglePanel()
	if panel.hidden {
		t.Fatalf("the panel refused to come on screen at %d columns", cols)
	}
	return m, panel
}

// TestVisiblePanelIsNeverASplitTarget. largestLiveLeafLocked and the
// targetFocused branch of resolveSplitTargetLocked filtered nil, closed, hidden
// and non-leaf — but not isControl, unlike firstLiveLeaf, allPanesDone and
// buildPaneResults. focusNext filters only !p.hidden, so Ctrl-G Tab really does
// park focus on a VISIBLE panel: with `magmux -c -e claude`, a human Tabs onto
// the panel to read the ledger, an MCP client calls open_pane with no target,
// and the agent's pane is halved out of magmux's own chrome and nested inside
// it. The panel is an instrument, not a place to put a session.
func TestVisiblePanelIsNeverASplitTarget(t *testing.T) {
	m, panel := visiblePanelMux(t, 120, 2)

	// The door: focus really can land on a visible panel.
	m.treeMu.Lock()
	m.focused = panel
	m.treeMu.Unlock()
	for i := 0; i < 6; i++ {
		if m.focusedPane() == panel {
			break
		}
		m.focusNext()
	}
	m.treeMu.Lock()
	m.focused = panel
	m.treeMu.Unlock()

	m.treeMu.RLock()
	byFocus := m.resolveSplitTargetLocked(targetFocused)
	m.treeMu.RUnlock()
	if byFocus == panel {
		t.Errorf("with focus on the panel, an untargeted open_pane would split magmux's own chrome")
	}
	if byFocus == nil || byFocus.isControl {
		t.Errorf("resolveSplitTargetLocked(targetFocused) returned %s; it must fall back to a session", paneName(byFocus))
	}

	// And the fallback it falls back TO must not nominate it either. The panel
	// is frequently the biggest thing on screen — it is a full column.
	m.treeMu.Lock()
	panel.h, panel.w = 1000, 1000
	m.treeMu.Unlock()

	m.treeMu.RLock()
	largest := m.largestLiveLeafLocked()
	m.treeMu.RUnlock()
	if largest == panel {
		t.Errorf("largestLiveLeafLocked nominated the control panel")
	}
	if largest == nil || largest.isControl {
		t.Errorf("largestLiveLeafLocked returned %s, want a session", paneName(largest))
	}
}

// TestOpenPaneWithFocusMovesThePanelMarker. focusNext, parseSGRMouse, ClosePane
// and sockFocus all tell the panel where focus went; OpenPane's `focus:true`
// set m.focused and told it nothing, so the route table's ▸ stayed on the pane
// the agent had just navigated away from — the panel disagreeing with magmux
// about a fact magmux owns.
func TestOpenPaneWithFocusMovesThePanelMarker(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	m.control.setFocused(0)

	id, err := m.OpenPane(OpenPaneRequest{
		PaneConfig: PaneConfig{Control: true},
		Target:     0,
		Split:      SplitVertical,
		Focus:      true,
	})
	if err != nil {
		t.Fatalf("OpenPane: %v", err)
	}
	if got := m.focusedPane(); got == nil || got.id != id {
		t.Fatalf("focus:true did not move magmux's own focus to pane %d", id)
	}

	m.control.mu.Lock()
	marked := m.control.focused
	m.control.mu.Unlock()
	if marked != id {
		t.Errorf("the panel still marks pane %d as focused after open_pane focus:true opened pane %d",
			marked, id)
	}
}

// TestOpenPaneReVerifyCatchesAConcurrentHide. The post-fork re-verify checked
// only that the target was still in the id table, which catches a concurrent
// close and not a concurrent HIDE — and hidden is the third state, not a
// synonym for either. If the human presses Ctrl-G p during the fork/exec
// window, removeLeafLocked detaches the panel and splitLeafLocked would splice
// the new pane onto a node no longer reachable from m.root: alive, never
// painted, undismissable.
func TestOpenPaneReVerifyCatchesAConcurrentHide(t *testing.T) {
	m, panel := visiblePanelMux(t, 120, 2)

	m.treeMu.RLock()
	session := m.largestLiveLeafLocked()
	okBefore := m.splitTargetIntactLocked(session) && m.splitTargetIntactLocked(panel)
	m.treeMu.RUnlock()
	if !okBefore {
		t.Fatalf("setup: a live on-screen leaf did not survive its own re-verify")
	}

	// What Ctrl-G p does during the window.
	m.togglePanel()
	if !panel.hidden {
		t.Fatalf("setup: the panel did not hide")
	}

	m.treeMu.RLock()
	stillOK := m.splitTargetIntactLocked(panel)
	inTree := m.nodeInTreeLocked(panel)
	m.treeMu.RUnlock()
	if inTree {
		t.Fatalf("setup: the hidden panel is still spliced into the tree")
	}
	if stillOK {
		t.Errorf("the re-verify accepted a target that was hidden while the child was forking; " +
			"the new pane would be spliced onto a node m.root cannot reach")
	}

	// A close is still caught, which is the case the check already had.
	m.treeMu.RLock()
	victim := m.largestLiveLeafLocked()
	m.treeMu.RUnlock()
	if err := m.ClosePane(victim.id, true); err != nil {
		t.Fatalf("ClosePane: %v", err)
	}
	m.treeMu.RLock()
	closedOK := m.splitTargetIntactLocked(victim)
	m.treeMu.RUnlock()
	if closedOK {
		t.Errorf("the re-verify accepted a closed target")
	}
}

// TestStatusBarToggleGivesItsRowToTheLayout. Hiding is not "stop painting the
// bar" — the row has to go somewhere, and it goes to the panes.
func TestStatusBarToggleGivesItsRowToTheLayout(t *testing.T) {
	m, _ := muxWithPanel(t, 2)
	withBar := snapGeom(m)

	m.toggleStatusBar()
	m.treeMu.RLock()
	hidden := m.hideStatus
	m.treeMu.RUnlock()
	if !hidden {
		t.Fatalf("Ctrl-G s did not hide the status bar")
	}
	noBar := snapGeom(m)
	for id, r := range withBar {
		if got := noBar[id][2]; got != r[2]+1 {
			t.Errorf("pane %d is %d rows without the status bar, want %d (its row plus one)",
				id, got, r[2]+1)
		}
	}

	m.toggleStatusBar()
	if back := snapGeom(m); !sameGeom(withBar, back) {
		t.Errorf("showing the status bar again did not restore the layout\n before: %v\n  after: %v",
			withBar, back)
	}
}

// TestHiddenStatusBarIsNotPainted — the row belongs to a pane now, so writing a
// bar over it would overwrite the child's own output.
func TestHiddenStatusBarIsNotPainted(t *testing.T) {
	m, _ := muxWithPanel(t, 1)
	m.statusText = "*: magmux\tD: unmistakable"

	paint := func() string {
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		m.markAllDirtyLocked()
		_, out, _ := m.renderLocked()
		return out
	}
	if !strings.Contains(paint(), "unmistakable") {
		t.Fatalf("the status bar is not painted even when it is meant to be")
	}
	m.toggleStatusBar()
	m.statusText = "*: magmux\tD: unmistakable"
	if got := paint(); strings.Contains(got, "unmistakable") {
		t.Errorf("the status bar was painted over a pane that now owns that row")
	}
}

// TestNoStatusFlagStartsWithoutTheRow covers the flag rather than the chord.
func TestNoStatusFlagStartsWithoutTheRow(t *testing.T) {
	m := &Magmux{rows: 40, cols: 120, gridMode: true, hideStatus: true,
		quit: make(chan struct{}), control: newControlPanel()}
	if err := m.buildGrid(ctrlPanes(1)); err != nil {
		t.Fatalf("buildGrid: %v", err)
	}
	if h := m.paneByID(0).h; h != 40 {
		t.Errorf("the pane is %d rows with the status bar off, want the whole terminal (40)", h)
	}
}

// ── the chords themselves ───────────────────────────────────────────────────

// TestChordsToggleTheChrome drives the real inputLoop, because everything above
// calls the toggles directly and a chord that was never wired up would let all
// of it pass.
func TestChordsToggleTheChrome(t *testing.T) {
	defer deadlineWatchdog(t, 20*time.Second)()

	m, panel := muxWithPanel(t, 2)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pw.Close()
	m.stdin = pr

	done := make(chan struct{})
	go func() { m.inputLoop(); close(done) }()

	press := func(key byte, want func() bool, what string) {
		t.Helper()
		if _, err := pw.Write([]byte{0x07, key}); err != nil {
			t.Fatalf("write chord: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if want() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("ctrl-g %c did not %s", key, what)
	}

	hiddenNow := func() bool { m.treeMu.RLock(); defer m.treeMu.RUnlock(); return panel.hidden }
	barOff := func() bool { m.treeMu.RLock(); defer m.treeMu.RUnlock(); return m.hideStatus }

	press('p', func() bool { return !hiddenNow() }, "show the panel")
	press('p', hiddenNow, "hide the panel again")
	press('s', barOff, "hide the status bar")
	press('s', func() bool { return !barOff() }, "show the status bar again")

	m.quitOnce.Do(func() { close(m.quit) })
	<-done
}

// ── the status digest ───────────────────────────────────────────────────────

// digestMux is a mux whose panel has a run in it, ready to be digested.
func digestMux(t *testing.T, cols int) *Magmux {
	t.Helper()
	m := &Magmux{rows: 40, cols: cols, gridMode: true,
		quit: make(chan struct{}), control: newControlPanel(), startedAt: time.Now()}
	if err := m.buildGrid(ctrlPanes(1)); err != nil {
		t.Fatalf("buildGrid: %v", err)
	}
	m.installHiddenPanel()
	m.control.recordStart(0, "make the tests pass", "claude-opus", "pilot", 3)
	m.control.recordSend(0, "step 1/3", "make the tests pass, then run the linter over the whole tree")
	m.control.recordSend(0, "step 2/3", "now fix the linter")
	m.control.recordSend(0, "step 3/3", "and commit it")
	m.control.recordObserved(0, "working", "", "Bash")
	m.control.recordObserved(0, "awaiting_input", "done", "Bash")
	return m
}

// TestStatusDigestCarriesThePanelCounters. The panel starts hidden, so the
// status bar is the only place a controlled run announces itself until somebody
// asks for the panel — and the counters keep the panel's provenance: ▶ is what
// the controller asked for, ◀ is what magmux observed. No third number.
func TestStatusDigestCarriesThePanelCounters(t *testing.T) {
	m := digestMux(t, 200)
	d := m.control.digest()
	if !d.active {
		t.Fatalf("a panel with three sends and two observations reports nothing to digest")
	}

	m.treeMu.RLock()
	hints := m.keyHintLocked()
	got := appendKeyHint(m.appendPanelDigestLocked("*: magmux", d, hintFloorWidth(hints)), hints, m.cols)
	m.treeMu.RUnlock()

	want := fmt.Sprintf("▶%d ◀%d", d.sent, d.observed)
	if !strings.Contains(got, want) {
		t.Errorf("the digest does not carry the counters %q: %q", want, got)
	}
	if d.sent == d.observed {
		t.Errorf("this fixture cannot tell the two counters apart: sent=%d observed=%d", d.sent, d.observed)
	}
	if !strings.Contains(got, "p panel") {
		t.Errorf("a hidden panel's bar does not say how to reveal it: %q", got)
	}
}

// TestStatusDigestDegradesWithTheTerminal: the signal text goes first, then the
// state, and the counters are what stay — but the whole thing is bounded by the
// terminal, counters included. A status bar that overruns wraps onto the pane
// above it, and corrupting a session's output to announce magmux is the exact
// opposite of what the hidden-by-default default is for.
//
// The base is short on purpose. magmux's own segments already overrun a
// 40-column terminal without any digest at all — a pre-existing wart, not this
// change's — and measuring the digest against a base that has already
// overflowed proves nothing about the digest.
func TestStatusDigestDegradesWithTheTerminal(t *testing.T) {
	const base = "*: magmux\tP: 0/1 done"

	var sawSignal, sawDropped, sawCounters bool
	// 26 and 28 are the widths at which even the counters have to go: the base
	// alone fits, adding "▶3 ◀1" and its divider does not.
	for _, cols := range []int{26, 28, 30, 40, 50, 60, 72, 80, 100, 120, 160, 200, 240} {
		m := digestMux(t, cols)
		m.treeMu.RLock()
		got := m.appendPanelDigestLocked(base, m.control.digest(), hintFloorWidth(m.keyHintLocked()))
		m.treeMu.RUnlock()

		if w := statusBarWidth(got); w > cols {
			t.Errorf("at %d columns the digest paints %d: %q", cols, w, got)
		}
		hasCounters := strings.Contains(got, "▶") && strings.Contains(got, "◀")
		if hasCounters {
			sawCounters = true
		} else if got != base {
			t.Errorf("at %d columns the digest dropped its counters but kept something else: %q", cols, got)
		}
		if strings.Contains(got, "awaiting_input") {
			sawSignal = true
		} else if hasCounters {
			sawDropped = true
		}
	}
	if !sawCounters {
		t.Errorf("the counters never rendered, at any width")
	}
	if !sawSignal {
		t.Errorf("the signal text never appeared, at any width")
	}
	if !sawDropped {
		t.Errorf("the signal text was never dropped; the digest does not degrade")
	}
}

// TestStatusDigestIsAbsentUntilAControllerActs. A bare `magmux -e claude` has
// nothing to digest, and a status bar reading "▶0 ◀0" is chrome announcing
// itself for no reason — which is the thing this whole change is against.
func TestStatusDigestIsAbsentUntilAControllerActs(t *testing.T) {
	m, _ := muxWithPanel(t, 1)
	m.treeMu.RLock()
	got := m.appendPanelDigestLocked("*: magmux", m.control.digest(), hintFloorWidth(m.keyHintLocked()))
	m.treeMu.RUnlock()
	if got != "*: magmux" {
		t.Errorf("an untouched panel still put a digest in the status bar: %q", got)
	}
}

// TestRenderedStatusBarNeverOverrunsTheTerminal closes the loop: the assertions
// above measure the string this builds, so this one measures what renderLocked
// actually installs, digest and attribution and all.
func TestRenderedStatusBarNeverOverrunsTheTerminal(t *testing.T) {
	for _, cols := range []int{40, 60, 80, 100, 140, 200} {
		m := digestMux(t, cols)
		m.treeMu.RLock()
		m.markAllDirtyLocked()
		_, _, _ = m.renderLocked()
		text := m.statusText
		m.treeMu.RUnlock()
		if w := statusBarWidth(text); w > cols {
			t.Errorf("at %d columns renderLocked built a %d-column status bar: %q", cols, w, text)
		}
		// Not vacuous: renderLocked must actually be reaching the digest.
		if cols >= 80 && !strings.Contains(text, "▶") {
			t.Errorf("at %d columns renderLocked never installed the digest: %q", cols, text)
		}
	}
}

// ── the key hints ───────────────────────────────────────────────────────────
//
// The status row is the ONLY thing magmux draws by default, so it is the only
// place the control panel can be discovered. `p` therefore has to be on it, and
// has to be the last thing dropped after `q` as the terminal narrows.

// hintMux is a grid of n fake SESSIONS plus a hidden panel — `magmux -e …`
// with the counter able to see its panes. ctrlPanes builds PTY-less panes, and
// the run counter skips isControl panes on purpose (the panel is chrome, not a
// session), so the flag is cleared here: what is wanted is a pane with no
// process that still counts, which is exactly a session for these purposes.
func hintMux(t *testing.T, cols, n int) *Magmux {
	t.Helper()
	m := &Magmux{rows: 40, cols: cols, gridMode: true,
		quit: make(chan struct{}), control: newControlPanel(), startedAt: time.Now()}
	if err := m.buildGrid(ctrlPanes(n)); err != nil {
		t.Fatalf("buildGrid: %v", err)
	}
	m.treeMu.Lock()
	for _, p := range m.livePanesLocked(nil) {
		p.isControl = false
	}
	m.treeMu.Unlock()
	m.installHiddenPanel()
	return m
}

// statusBarAt renders one frame and returns the row renderLocked installed,
// asserting on the way past that it cannot wrap onto the pane above it.
func statusBarAt(t *testing.T, m *Magmux) string {
	t.Helper()
	m.treeMu.RLock()
	m.markAllDirtyLocked()
	_, _, _ = m.renderLocked()
	text := m.statusText
	m.treeMu.RUnlock()
	if w := statusBarWidth(text); w > m.cols {
		t.Errorf("at %d columns the status bar paints %d columns: %q", m.cols, w, text)
	}
	return text
}

// TestKeyHintAdvertisesThePanelToggle is the gap this closes: before it, the
// bar read "ctrl-g Tab · q quit" and mentioned neither p nor s, so the control
// panel — the whole reason the chord exists — could only be found in the
// README.
func TestKeyHintAdvertisesThePanelToggle(t *testing.T) {
	for _, cols := range []int{80, 120, 200} {
		m := hintMux(t, cols, 2)

		got := statusBarAt(t, m)
		if !strings.Contains(got, "ctrl-g p panel") {
			t.Errorf("at %d columns the bar does not advertise the panel key: %q", cols, got)
		}
		if !strings.Contains(got, "q quit") {
			t.Errorf("at %d columns the bar does not say how to get out: %q", cols, got)
		}

		// With the panel on screen the same key hides it, and says so: a hint
		// reading "p panel" against a panel already up is ambiguous about
		// which way the key goes.
		m.togglePanel()
		m.treeMu.RLock()
		hidden := m.panelHiddenLocked()
		m.treeMu.RUnlock()
		if hidden {
			t.Fatalf("at %d columns the panel refused to show; the visible-state hint is untested", cols)
		}
		got = statusBarAt(t, m)
		if !strings.Contains(got, "p hide") {
			t.Errorf("at %d columns a visible panel's hint does not say it hides: %q", cols, got)
		}
		if strings.Contains(got, "p panel") {
			t.Errorf("at %d columns a visible panel still offers to show itself: %q", cols, got)
		}
	}
}

// TestKeyHintDegradesInPriorityOrder. Everything on this row is bounded by the
// terminal, the hints included — so they have to drop in the order a user needs
// them: s first (hiding the bar hides its own hint), then Tab (only useful with
// two panes on screen), then p, and q last of all.
func TestKeyHintDegradesInPriorityOrder(t *testing.T) {
	// Ranked widest-surviving last, the same order the implementation drops in.
	items := []string{"s bar", "Tab", "p panel", "q quit"}
	seen := make([]int, len(items)) // widths at which each was present
	gone := make([]int, len(items)) // widths at which each was absent
	for _, cols := range []int{20, 24, 30, 36, 40, 50, 60, 72, 80, 100, 120, 160, 200} {
		m := hintMux(t, cols, 2)
		got := statusBarAt(t, m)

		present := make([]bool, len(items))
		for i, it := range items {
			present[i] = strings.Contains(got, it)
			if present[i] {
				seen[i]++
			} else {
				gone[i]++
			}
		}
		// Nested sets: nothing survives a width that dropped something ranked
		// above it.
		for i := 0; i < len(items)-1; i++ {
			if present[i] && !present[i+1] {
				t.Errorf("at %d columns the bar kept %q but dropped %q: %q",
					cols, items[i], items[i+1], got)
			}
		}
		// Whatever is left of the chord, the way out is part of it.
		if strings.Contains(got, "ctrl-g") && !strings.Contains(got, "q quit") {
			t.Errorf("at %d columns the chord is advertised without the way out: %q", cols, got)
		}
	}
	for i, it := range items {
		if seen[i] == 0 {
			t.Errorf("%q never appeared at any width", it)
		}
		if i < len(items)-1 && gone[i] == 0 {
			t.Errorf("%q was never dropped; the hints do not degrade", it)
		}
	}
	if gone[len(items)-1] != 0 {
		t.Errorf("the way out was dropped at some width; q must survive longest")
	}
}

// TestStatusBarNeverOverrunsAtAnyWidth is the invariant the degradation exists
// to protect: a status row wider than the terminal wraps onto the pane above
// it and corrupts a session's output. Every state the row can be in is measured
// — a bare run, a finished one, a controlled one, and the chord's own menu.
func TestStatusBarNeverOverrunsAtAnyWidth(t *testing.T) {
	widths := []int{20, 30, 40, 60, 80, 120, 200}

	for _, cols := range widths {
		// statusBarAt asserts the width itself, on a running grid...
		m := hintMux(t, cols, 2)
		statusBarAt(t, m)

		// ...with the panel on screen...
		m.togglePanel()
		statusBarAt(t, m)

		// ...and once every pane has finished, which is a different bar.
		m.treeMu.Lock()
		for _, p := range m.livePanesLocked(nil) {
			if !p.isControl {
				p.dead = true
			}
		}
		m.treeMu.Unlock()
		statusBarAt(t, m)

		// A controlled run, whose digest competes with the hints.
		d := digestMux(t, cols)
		statusBarAt(t, d)

		// And the chord's menu, which replaces the row while Ctrl-G is armed.
		m.treeMu.RLock()
		menu := m.chordMenuLocked(m.keyHintLocked())
		m.treeMu.RUnlock()
		if w := statusBarWidth(menu); w > cols {
			t.Errorf("at %d columns the chord menu paints %d columns: %q", cols, w, menu)
		}
	}
}

// TestChromeNoteNeverOverrunsTheStatusBar. The refusal notes are the only
// status-bar text that was never measured: renderLocked checked whether
// note+"\t"+text fitted, and dropped `text` when it did not — but bare `note`
// was written out whatever its length, and renderStatusBar pads rather than
// truncating.
//
// The two live notes are both longer than the terminals that produce them, and
// that is not a coincidence — it is the same condition twice:
//
//   - panelTooNarrow is 41 runes and splitFits refuses whenever cols <= 40, so
//     Ctrl-G p on a 40-column terminal wrote 42 columns into a 40-column row.
//   - the alternate-screen note is 50 runes and fires on Ctrl-G [ against any
//     Claude Code, vim or htop pane, at every width.
//
// A status row wider than the terminal wraps onto the pane above it and
// corrupts the session's output — announcing magmux by damaging the thing
// magmux exists to carry.
func TestChromeNoteNeverOverrunsTheStatusBar(t *testing.T) {
	notes := []string{
		panelTooNarrow,
		"no scrollback: this pane is on the alternate screen",
		"nothing has scrolled off this pane yet",
		"the panel scrolls with k/j/g/G",
	}
	for _, cols := range []int{20, 30, 40, 60, 80, 120, 200} {
		for _, note := range notes {
			m := hintMux(t, cols, 2)
			m.treeMu.Lock()
			m.noteChromeLocked(note)
			m.markAllDirtyLocked()
			_, _, _ = m.renderLocked()
			row := m.noteRowLocked(m.statusText)
			m.treeMu.Unlock()

			if w := statusBarWidth(row); w > cols {
				t.Errorf("at %d columns the note %q paints a %d-column status row: %q",
					cols, note, w, row)
			}
			// Truncated is not the same as dropped: the refusal still has to be
			// said, or a keystroke that did nothing is indistinguishable from a
			// broken one.
			if !strings.HasPrefix(row, "R: ") || len(strings.TrimSpace(row)) <= len("R:") {
				t.Errorf("at %d columns the note %q was swallowed entirely: %q", cols, note, row)
			}
		}
	}
}

// TestChordMenuTeachesItsSecondKeys. A prefix key with nothing on screen to
// explain it is a prefix key nobody presses twice. While Ctrl-G is armed the
// row it already owns lists what the second key can be — and it goes away
// again on that keystroke, so no pane is disturbed by it.
func TestChordMenuTeachesItsSecondKeys(t *testing.T) {
	m := hintMux(t, 120, 2)

	m.armChord(true)
	m.treeMu.RLock()
	armed := m.chordArmed
	m.markAllDirtyLocked()
	_, frame, _ := m.renderLocked()
	m.treeMu.RUnlock()
	if !armed {
		t.Fatalf("Ctrl-G did not arm the chord")
	}
	for _, want := range []string{"ctrl-g", "p panel", "Tab", "s bar", "q quit"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the armed chord's row does not offer %q", want)
		}
	}

	m.armChord(false)
	m.treeMu.RLock()
	stillArmed := m.chordArmed
	m.markAllDirtyLocked()
	_, frame, _ = m.renderLocked()
	m.treeMu.RUnlock()
	if stillArmed {
		t.Fatalf("the chord stayed armed after its second key")
	}
	if strings.Contains(frame, "ctrl-g …") {
		t.Errorf("the chord menu outlived the chord: %q", frame)
	}
}
