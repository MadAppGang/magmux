package mux

// The size decision: screenSizeLocked decides, applySizeLocked writes,
// reflowLocked resizes — and planLayoutLocked/treeFitsLocked answer "would it
// fit?" without touching a Pane.
//
// Phase A of the resize op is a refactor that must change nothing observable,
// so most of these tests are EQUIVALENCE proofs: each extraction is run
// against the code it was extracted from, on a twin, and the two must agree
// byte for byte. A test that only checked the new function against numbers
// written down by hand would pass just as happily against a second copy of
// the arithmetic that has already started to drift.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/protocol"
)

// treeFingerprint is everything a resize could disturb, as one string: the
// tree's shape and geometry, every live pane's screen (cells, cursor,
// scrollback), the frame generation, and the four size ints. frameGen is the
// load-bearing one — Pane.resize bumps it on every leaf it touches, so an
// unchanged frameGen proves no Pane.resize ran, hence no Screen.resize, no
// keyframe and no pty.SetWinSize.
func treeFingerprint(m *Magmux) string {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	var b strings.Builder
	fmt.Fprintf(&b, "screen %dx%d auto %dx%d manual %dx%d\n",
		m.rows, m.cols, m.autoRows, m.autoCols, m.manualRows, m.manualCols)
	var walk func(n *Pane, depth int)
	walk = func(n *Pane, depth int) {
		if n == nil {
			return
		}
		fmt.Fprintf(&b, "%*snode %p parent %p split %d ratio %v at %d,%d %dx%d\n",
			depth*2, "", n, n.parent, n.splitType, n.ratio, n.y, n.x, n.h, n.w)
		walk(n.child1, depth+1)
		walk(n.child2, depth+1)
	}
	walk(m.root, 0)
	for _, p := range m.livePanesLocked(nil) {
		fmt.Fprintf(&b, "pane %d %p hidden %v at %d,%d %dx%d gen %d\n",
			p.id, p, p.hidden, p.y, p.x, p.h, p.w, p.frameGen.Load())
		p.mu.Lock()
		for _, s := range []*Screen{p.primaryScreen, p.screen} {
			if s == nil {
				continue
			}
			fmt.Fprintf(&b, "  screen %p %dx%d cur %d,%d sb %d\n", s, s.rows, s.cols, s.curY, s.curX, s.sbLen)
			for _, row := range s.cells {
				fmt.Fprintf(&b, "  |%s|\n", rowText(row, 0, len(row)))
			}
		}
		p.mu.Unlock()
	}
	return b.String()
}

// asSessions turns every fake pane except the control panel into a SESSION
// leaf. ctrlPanes builds PTY-less panes, which is what keeps these tests free
// of processes, but they are built isControl — and treeFitsLocked skips
// isControl, so without this flip every fit check would pass vacuously.
// Pane.resize already skips SetWinSize when ptmx is nil.
func asSessions(m *Magmux) *Magmux {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	for _, p := range m.livePanesLocked(nil) {
		if p != m.panel {
			p.isControl = false
		}
	}
	return m
}

// leafBoxes is every in-tree leaf's rectangle, keyed by pane pointer.
func leafBoxes(m *Magmux) map[*Pane]box {
	out := map[*Pane]box{}
	eachLeaf(m.root, func(p *Pane) { out[p] = box{p.y, p.x, p.h, p.w} })
	return out
}

// TestSplitBoxesMatchesReshapeChildren: splitBoxes is reshapeChildren's
// arithmetic with the writes taken out, so for every split it must name
// exactly the rectangles reshapeChildren hands its children — including the
// degenerate boxes where the clamps are the whole answer.
func TestSplitBoxesMatchesReshapeChildren(t *testing.T) {
	ratios := []float64{0.1, 0.33, 0.5, 0.77, 0.99}
	boxes := []box{
		{0, 0, 40, 120}, {3, 7, 24, 80}, {0, 0, 1, 1}, {0, 0, 2, 2},
		{0, 0, 0, 0}, {5, 5, 0, 10}, {5, 5, 10, 0},
		{0, 0, -3, -4}, // a parent already clamped badly: the floors must hold
		{1, 2, 3, 1},
	}
	for _, st := range []SplitType{SplitHorizontal, SplitVertical} {
		for _, r := range ratios {
			for _, b := range boxes {
				parent := &Pane{splitType: st, ratio: r, y: b.y, x: b.x, h: b.h, w: b.w}
				parent.child1 = newControlPane(0, 0, 1, 1, "a")
				parent.child2 = newControlPane(0, 0, 1, 1, "b")
				parent.reshapeChildren()
				c1, c2 := splitBoxes(st, r, b)
				got1 := box{parent.child1.y, parent.child1.x, parent.child1.h, parent.child1.w}
				got2 := box{parent.child2.y, parent.child2.x, parent.child2.h, parent.child2.w}
				if got1 != c1 || got2 != c2 {
					t.Errorf("split %d ratio %v box %+v: reshapeChildren gave %+v / %+v, splitBoxes %+v / %+v",
						st, r, b, got1, got2, c1, c2)
				}
			}
		}
	}
}

// TestSplitBoxesSplitNone pins the one branch reshapeChildren never reaches.
func TestSplitBoxesSplitNone(t *testing.T) {
	b := box{1, 2, 3, 4}
	c1, c2 := splitBoxes(SplitNone, 0.5, b)
	if c1 != b || c2 != (box{}) {
		t.Fatalf("splitBoxes(SplitNone) = %+v / %+v, want %+v / zero", c1, c2, b)
	}
}

// TestPlanLayoutMatchesReflow: the dry run must lay the tree out exactly as
// reflowLocked would, for every shape the builders produce and with the
// status row on and off. If it did not, a resize the plan approved could
// leave a pane smaller than the floor — after Screen.resize had already
// destroyed the cells, with no rollback.
func TestPlanLayoutMatchesReflow(t *testing.T) {
	sizes := []struct{ rows, cols int }{
		{40, 120}, {24, 80}, {50, 160}, {100, 300}, {61, 203},
		{10, 30}, {5, 12}, {3, 7}, {2, 2}, {1, 1},
	}
	type shape struct {
		name  string
		build func(t *testing.T) *Magmux
	}
	var shapes []shape
	for n := 1; n <= 7; n++ {
		shapes = append(shapes, shape{fmt.Sprintf("%d panes", n), func(t *testing.T) *Magmux {
			return newTestMux(t, ctrlPanes(n)...)
		}})
	}
	for _, n := range []int{1, 3, 4} {
		shapes = append(shapes, shape{fmt.Sprintf("%d panes + visible panel", n), func(t *testing.T) *Magmux {
			m, _ := muxWithVisiblePanel(t, n)
			return m
		}})
		shapes = append(shapes, shape{fmt.Sprintf("%d panes + hidden panel", n), func(t *testing.T) *Magmux {
			m, _ := muxWithPanel(t, n)
			return m
		}})
	}
	for _, sh := range shapes {
		for _, hide := range []bool{false, true} {
			for _, sz := range sizes {
				t.Run(fmt.Sprintf("%s/hideStatus=%v/%dx%d", sh.name, hide, sz.rows, sz.cols), func(t *testing.T) {
					m := sh.build(t)
					m.treeMu.Lock()
					defer m.treeMu.Unlock()
					m.hideStatus = hide
					planned := map[*Pane]box{}
					m.planLayoutLocked(sz.rows, sz.cols, func(p *Pane, b box) {
						if _, dup := planned[p]; dup {
							t.Errorf("leaf %d visited twice", p.id)
						}
						planned[p] = b
					})
					m.rows, m.cols = sz.rows, sz.cols
					m.reflowLocked()
					real := leafBoxes(m)
					if len(planned) != len(real) {
						t.Fatalf("plan visited %d leaves, the tree has %d", len(planned), len(real))
					}
					for p, want := range real {
						if got, ok := planned[p]; !ok || got != want {
							t.Errorf("pane %d: plan %+v (visited %v), reflow %+v", p.id, got, ok, want)
						}
					}
				})
			}
		}
	}
}

// TestTreeFitsSkipsPanelAndHidden: "any session leaf" means exactly that. A
// hidden panel's stale geometry is not consulted (it is not in the tree), and
// a visible panel being starved is not a refusal — it is reshaped and clamped
// as it would be on SIGWINCH. A starved session leaf is, and is named.
func TestTreeFitsSkipsPanelAndHidden(t *testing.T) {
	t.Run("hidden panel with tiny stale geometry", func(t *testing.T) {
		m, panel := muxWithPanel(t, 1)
		asSessions(m)
		m.treeMu.Lock()
		panel.y, panel.x, panel.h, panel.w = 0, 0, 1, 1
		m.treeMu.Unlock()
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		if bad, got, ok := m.treeFitsLocked(30, 90); !ok {
			t.Fatalf("hidden panel consulted: refused on pane %d with %+v", bad.id, got)
		}
	})

	t.Run("visible panel starved, session fine", func(t *testing.T) {
		m, panel := muxWithVisiblePanel(t, 1)
		asSessions(m)
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		// Find a width at which the panel is below the floor and the session
		// leaf is not. Searched rather than hardcoded, so the test states the
		// PROPERTY and survives a change to the builder's ratio.
		found := false
		for cols := minPaneCols + 2; cols < 400 && !found; cols++ {
			var panelBox, sessBox box
			m.planLayoutLocked(30, cols, func(p *Pane, b box) {
				if p == panel {
					panelBox = b
				} else {
					sessBox = b
				}
			})
			if panelBox.w >= minPaneCols || sessBox.w < minPaneCols {
				continue
			}
			found = true
			if bad, got, ok := m.treeFitsLocked(30, cols); !ok {
				t.Fatalf("cols %d: visible panel counted as a session leaf: refused on pane %d %+v", cols, bad.id, got)
			}
		}
		if !found {
			t.Fatal("no width starves the panel alone; the builder's split changed and this test needs a new shape")
		}
	})

	t.Run("session leaf starved", func(t *testing.T) {
		m := asSessions(newTestMux(t, ctrlPanes(2)...))
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		bad, got, ok := m.treeFitsLocked(40, 30)
		if ok {
			t.Fatal("two side-by-side panes in 30 columns reported as fitting")
		}
		if bad == nil || bad.isControl {
			t.Fatalf("refusal names no session pane: %v", bad)
		}
		if got.w >= minPaneCols && got.h >= minPaneRows {
			t.Fatalf("pane %d named with a box that fits: %+v", bad.id, got)
		}
		if _, _, ok := m.treeFitsLocked(40, 120); !ok {
			t.Fatal("the size the mux was built at reported as not fitting")
		}
	})
}

// TestTreeFitsWritesNothing: Screen.resize destroys the cells that do not fit
// and there is no rollback, so the check must leave every byte where it was —
// pass or fail.
func TestTreeFitsWritesNothing(t *testing.T) {
	m, _ := muxWithVisiblePanel(t, 3)
	asSessions(m)
	for _, p := range m.livePanes() {
		p.mu.Lock()
		p.vt.write([]byte(fmt.Sprintf("pane %d says hello\r\nsecond line", p.id)))
		p.mu.Unlock()
	}
	before := treeFingerprint(m)
	m.treeMu.RLock()
	_, _, okSmall := m.treeFitsLocked(4, 10)
	_, _, okBig := m.treeFitsLocked(60, 200)
	m.treeMu.RUnlock()
	if okSmall || !okBig {
		t.Fatalf("fits(4x10)=%v fits(60x200)=%v, want false/true", okSmall, okBig)
	}
	if after := treeFingerprint(m); after != before {
		t.Fatalf("treeFitsLocked wrote to the tree:\n--- before\n%s--- after\n%s", before, after)
	}
}

// TestCheckScreenSize is the op's bound. Note the 10000 boundary: inclusive
// here (the contract refuses "a side > 10000") and exclusive in headlessSize's
// per-side parse (TestHeadlessSizeBounds) — documented beside maxScreenSide.
func TestCheckScreenSize(t *testing.T) {
	cases := []struct {
		rows, cols int
		code       string // "" = accepted
	}{
		{0, 10, protocol.CodeBadRequest},
		{10, 0, protocol.CodeBadRequest},
		{-1, 10, protocol.CodeBadRequest},
		{10, -1, protocol.CodeBadRequest},
		{10001, 1, protocol.CodeTooLarge},
		{1, 10001, protocol.CodeTooLarge},
		{10000, 400, ""},
		{10000, 401, protocol.CodeTooLarge},
		{2000, 2000, ""},
		{2001, 2000, protocol.CodeTooLarge},
		{24, 80, ""},
		{1, 1, ""},
	}
	for _, c := range cases {
		err := checkScreenSize(c.rows, c.cols)
		got := ""
		if err != nil {
			got = protocol.CodeOf(err)
			if got == "" {
				t.Errorf("%dx%d: error %q carries no code", c.rows, c.cols, err)
			}
		}
		if got != c.code {
			t.Errorf("checkScreenSize(%d, %d) code %q (%v), want %q", c.rows, c.cols, got, err, c.code)
		}
	}
}

// TestScreenSizeDecision: manual, else auto, else what it already has — and
// with nothing set, applySizeLocked on a struct literal IS reflowLocked.
func TestScreenSizeDecision(t *testing.T) {
	m := &Magmux{rows: 40, cols: 120}
	if r, c := m.screenSizeLocked(); r != 40 || c != 120 {
		t.Fatalf("nothing set: %dx%d, want the current 40x120", r, c)
	}
	m.autoRows, m.autoCols = 30, 90
	if r, c := m.screenSizeLocked(); r != 30 || c != 90 {
		t.Fatalf("auto set: %dx%d, want 30x90", r, c)
	}
	m.headless = true
	m.manualRows, m.manualCols = 20, 60
	if r, c := m.screenSizeLocked(); r != 20 || c != 60 {
		t.Fatalf("manual set: %dx%d, want 20x60", r, c)
	}
	// Half a pair is not a decision.
	m.manualCols = 0
	if r, c := m.screenSizeLocked(); r != 30 || c != 90 {
		t.Fatalf("half a manual pair: %dx%d, want auto 30x90", r, c)
	}

	a := newTestMux(t, ctrlPanes(3)...)
	b := newTestMux(t, ctrlPanes(3)...)
	a.treeMu.Lock()
	a.applySizeLocked()
	a.treeMu.Unlock()
	b.treeMu.Lock()
	b.reflowLocked()
	b.treeMu.Unlock()
	if fa, fb := stripPointers(treeFingerprint(a)), stripPointers(treeFingerprint(b)); fa != fb {
		t.Fatalf("applySizeLocked on a literal is not reflowLocked:\n--- apply\n%s--- reflow\n%s", fa, fb)
	}
}

// TestSetAutoSizeIsTheOldSIGWINCH: the SIGWINCH body moved into setAutoSize.
// Run the new one on one twin and the old body, verbatim, on the other.
func TestSetAutoSizeIsTheOldSIGWINCH(t *testing.T) {
	for _, sz := range []struct{ rows, cols int }{{30, 90}, {60, 200}, {8, 25}} {
		a := newTestMux(t, ctrlPanes(4)...)
		b := newTestMux(t, ctrlPanes(4)...)

		a.setAutoSize(sz.rows, sz.cols)

		b.treeMu.Lock()
		b.rows = sz.rows
		b.cols = sz.cols
		b.reflowLocked()
		b.treeMu.Unlock()
		b.control.markDirty()
		b.autoRows, b.autoCols = sz.rows, sz.cols // the one new fact, stated

		if fa, fb := stripPointers(treeFingerprint(a)), stripPointers(treeFingerprint(b)); fa != fb {
			t.Fatalf("%dx%d: setAutoSize differs from the old SIGWINCH body:\n--- new\n%s--- old\n%s",
				sz.rows, sz.cols, fa, fb)
		}
		if a.manualRows != 0 || a.manualCols != 0 {
			t.Fatalf("setAutoSize wrote manual*: %dx%d", a.manualRows, a.manualCols)
		}
	}
}

// stripPointers removes the %p values so two twins built separately compare.
func stripPointers(s string) string {
	var b strings.Builder
	for _, f := range strings.Fields(strings.ReplaceAll(s, "\n", " \n ")) {
		if strings.HasPrefix(f, "0x") {
			f = "P"
		}
		b.WriteString(f)
		b.WriteByte(' ')
	}
	return b.String()
}
