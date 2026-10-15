package mux

// ── The screen size: one decision, one writer, one reflow ────────────────────
//
// magmux's screen size used to be two assignments in two places — init() and
// the SIGWINCH goroutine — each writing m.rows/m.cols and calling reflowLocked.
// That was fine while the terminal was the only thing with an opinion. The
// `resize` op adds a second opinion (a client asking for a size), and two
// writers that each believe they own m.rows is how one silently undoes the
// other. So the size is now split into the facts and the decision:
//
//	autoRows/autoCols      what magmux picks when nobody asked (tty or headless)
//	manualRows/manualCols  what a client asked for (0 = nobody)
//	rows/cols              the decision: screenSizeLocked, written only by
//	                       applySizeLocked after init()
//
// and every producer writes its own fact and asks applySizeLocked to decide.

// maxScreenCells caps the PRODUCT. A per-side bound is NOT enough, and the
// first version of headlessSize only had one. newScreen allocates rows*cols
// Cells and a Cell is 20 bytes here (rune 4 + two 6-byte Colors + Attr 2 + two
// bools), so LINES=9999 COLUMNS=9999 — both inside a 10000 per-side cap — is
// 99,980,001 cells and a ~2GB allocation before a single byte of output, which
// is the OOM headlessSize's comment claims to have prevented. The PRODUCT is
// what costs memory, so the product is what is capped. 4,000,000 cells is
// ~80MB: far past any real terminal (a 300x600 pane is 180,000) and nowhere
// near a container limit.
//
// Also the op's bound — see checkScreenSize.
const maxScreenCells = 4_000_000

// maxScreenSide caps one side. pty.SetWinSize carries uint16, and newScreen
// allocates rows*cols, so neither may be trusted from a request.
//
// NOTE: headlessSize's per-side parse is EXCLUSIVE of this value (LINES=10000
// falls back, pinned by TestHeadlessSizeBounds) while checkScreenSize is
// INCLUSIVE (the contract refuses "a side > 10000"). headlessSize predates the
// op and its boundary is tested behaviour, so the two differ at exactly 10000,
// which only matters for 10000 x <=400. Said here so nobody "fixes" one into
// the other.
const maxScreenSide = 10_000

// checkScreenSize is the ONE bound on a geometry. headlessSize falls back to
// 80x24 on an error (the value came from the environment and nobody can be
// told); the resize op refuses with it (somebody can).
func checkScreenSize(rows, cols int) error {
	if rows < 1 || cols < 1 {
		return sockErrf(sockCodeBadRequest, "rows and cols must be positive (got %dx%d)", rows, cols)
	}
	if rows > maxScreenSide || cols > maxScreenSide {
		return sockErrf(sockCodeTooLarge, "%dx%d: no side may exceed %d", rows, cols, maxScreenSide)
	}
	if rows*cols > maxScreenCells { // sides already bounded: no overflow
		return sockErrf(sockCodeTooLarge, "%dx%d is %d cells; the cap is %d",
			rows, cols, rows*cols, maxScreenCells)
	}
	return nil
}

// screenSizeLocked is the ONE place the virtual size is DECIDED: manual, else
// auto, else what it already has. The last branch is the struct-literal and
// pre-init case, not a bug fallback — every unit test builds
// &Magmux{rows: 40, cols: 120} with auto* and manual* zero, and for those
// applySizeLocked must be exactly reflowLocked.
//
// A pair counts only when BOTH halves are set: half a geometry is nobody's
// decision.
//
// The interim clip: on an interactive terminal the manual size is CLAMPED to
// the terminal's. The op refuses a size larger than the terminal at request
// time, but the human can shrink the window afterwards, and a virtual screen
// larger than the real one paints past its edge until renderer clipping
// exists. Releasing manual mode instead is not allowed — SIGWINCH never undoes
// it — so manual* is left alone and the window growing back restores the size
// that was asked for. This `if` and the op's request-time refusal are the two
// sites renderer clipping deletes. Headless has no terminal and never clips.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) screenSizeLocked() (rows, cols int) {
	if m.manualRows > 0 && m.manualCols > 0 {
		r, c := m.manualRows, m.manualCols
		if !m.headless && m.autoRows > 0 && m.autoCols > 0 {
			r, c = min(r, m.autoRows), min(c, m.autoCols)
		}
		return r, c
	}
	if m.autoRows > 0 && m.autoCols > 0 {
		return m.autoRows, m.autoCols
	}
	return m.rows, m.cols
}

// applySizeLocked is the ONE writer of m.rows/m.cols after init(). It does not
// compare: a caller that must not reflow on an unchanged size checks first,
// and SIGWINCH has always reflowed unconditionally — Pane.resize is never a
// no-op (it reallocates every Screen and forces a keyframe), so skipping it
// here would be a behaviour change nobody asked for.
//
// Caller holds treeMu.Lock.
func (m *Magmux) applySizeLocked() {
	m.rows, m.cols = m.screenSizeLocked()
	m.reflowLocked()
}

// setAutoSize is the SIGWINCH body, extracted so it is reachable from a test
// (headless never installs the handler). It writes auto* and NEVER manual*:
// a client's size surviving the human's window is the point of manual mode.
//
// Caller must NOT hold treeMu.
func (m *Magmux) setAutoSize(rows, cols int) {
	// Geometry is structural, and m.root.resize writes it on every node in
	// the tree — this is a writer, not a reader.
	m.treeMu.Lock()
	m.autoRows, m.autoCols = rows, cols
	m.applySizeLocked()
	m.treeMu.Unlock()
	// resize rebuilds each pane's Screen. A child process redraws itself on
	// SIGWINCH; the control panel has no process to do that, so it must be
	// repainted here or it comes back blank.
	m.control.markDirty()
}

// ── The dry run ──────────────────────────────────────────────────────────────

// box is a rectangle in screen cells.
type box struct{ y, x, h, w int }

// planLayoutLocked walks the tree exactly as reflowLocked would lay it into a
// rows x cols screen and hands each LEAF the box it would get. It writes
// nothing — Screen.resize destroys the cells that do not fit and there is no
// rollback, so the only safe "would it fit?" is one that never touches a Pane.
//
// It shares splitBoxes with reshapeChildren rather than restating the split
// arithmetic, and its root box is reflowLocked's own expression;
// TestPlanLayoutMatchesReflow pins the two together, because a dry run that
// drifts from the real layout approves a resize the real layout then clamps.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) planLayoutLocked(rows, cols int, visit func(leaf *Pane, b box)) {
	var walk func(n *Pane, b box)
	walk = func(n *Pane, b box) {
		if n == nil {
			return
		}
		if n.splitType == SplitNone {
			visit(n, b)
			return
		}
		c1, c2 := splitBoxes(n.splitType, n.ratio, b)
		walk(n.child1, c1)
		walk(n.child2, c2)
	}
	walk(m.root, box{0, 0, maxInt(0, rows-m.statusRowsLocked()), maxInt(0, cols)})
}

// treeFitsLocked reports whether a rows x cols screen gives every SESSION leaf
// at least minPaneRows x minPaneCols, and names the first one that would not.
//
// Which leaves: every leaf REACHABLE FROM m.root that is not isControl. Hidden
// panes are not in the tree (removeLeafLocked), so they are never visited —
// their stale y/x/h/w is exactly what must not be consulted. Closed panes are
// not in the tree either. Dead panes ARE in the tree and ARE checked: they are
// on screen with their DONE/FAIL card, and a zero-row tombstone captures
// empty. The visible control panel is NOT checked, per the contract's "any
// session leaf"; it is reshaped and clamped as it is on SIGWINCH.
//
// The floor is minPaneRows/minPaneCols, the same constants OpenPane, splitFits
// and buildGrid use — no second copy: "usable" cannot mean one thing for an
// agent's open_pane and another for a resize.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) treeFitsLocked(rows, cols int) (bad *Pane, got box, ok bool) {
	ok = true
	m.planLayoutLocked(rows, cols, func(p *Pane, b box) {
		if !ok || p.isControl {
			return
		}
		if b.h < minPaneRows || b.w < minPaneCols {
			bad, got, ok = p, b, false
		}
	})
	return bad, got, ok
}
