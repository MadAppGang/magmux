package main

// S-2 — layout refusal instead of silently unusable panes.
//
// buildColumn's `botH := h - topH - 1` and buildGrid's `w2 := m.cols - w1 - 1`
// had no floor. At the headless default of 80x24, roughly 32 -e panes produce a
// zero-height pane and roughly 64 a negative one — and a zero-row Screen
// captures as EMPTY, so a scenario reported no output rather than an error.
//
// The rule applied here is the codebase's existing split, not a new one:
// CREATION refuses (OpenPane, splitFits, showPanelLocked), RESHAPE clamps
// (reshapeChildren, per CLAUDE.md). buildGrid is creation.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBuildGridEitherRefusesOrFits is the PROPERTY, asserted instead of a table
// of thresholds that would have to be recomputed whenever the arithmetic is
// touched:
//
//	buildGrid either returns an error, or produces a layout in which every leaf
//	has h >= minPaneRows and w >= minPaneCols.
func TestBuildGridEitherRefusesOrFits(t *testing.T) {
	geometries := []struct{ rows, cols int }{
		{24, 80},  // the headless default
		{50, 200}, // a big terminal
		{24, 40},  // too narrow to split at all
		{10, 80},  // too short for more than two
		{1, 1},    // degenerate
		{0, 0},    // what an uninitialised Magmux would have had
	}
	counts := []int{1, 2, 3, 4, 8, 12, 13, 16, 32, 64}

	for _, g := range geometries {
		for _, n := range counts {
			for _, withPanel := range []bool{false, true} {
				name := fmt.Sprintf("%dx%d_n%d_panel=%v", g.cols, g.rows, n, withPanel)
				t.Run(name, func(t *testing.T) {
					m := &Magmux{rows: g.rows, cols: g.cols, control: newControlPanel()}
					cfgs := ctrlPanes(n)
					if withPanel {
						// -c appends the panel to commands, so it COSTS a pane
						// in the layout. Without -c installHiddenPanel adds it
						// outside the tree, where it costs nothing.
						cfgs = append(cfgs, PaneConfig{Control: true})
					}
					err := m.buildGrid(cfgs)
					if err != nil {
						// A refusal is always an acceptable answer. What is not
						// acceptable is a layout with an unusable pane in it.
						if m.root != nil {
							t.Errorf("buildGrid refused but still installed a tree")
						}
						return
					}
					eachLeaf(m.root, func(p *Pane) {
						if p.h < minPaneRows || p.w < minPaneCols {
							t.Errorf("leaf %d is %dx%d, below the %dx%d floor, and buildGrid did not refuse",
								p.id, p.w, p.h, minPaneCols, minPaneRows)
						}
					})
				})
			}
		}
	}
}

// TestErrPanesDontFitNamesCountAndGeometry. "No room" without either number is
// a message a caller cannot act on: it does not say whether to drop a pane or
// find a bigger window.
func TestErrPanesDontFitNamesCountAndGeometry(t *testing.T) {
	m := &Magmux{rows: 24, cols: 80, control: newControlPanel()}
	err := m.buildGrid(ctrlPanes(40))
	if err == nil {
		t.Fatal("40 panes on an 80x24 terminal must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"40", "80x23", strconv.Itoa(minPaneRows), strconv.Itoa(minPaneCols), "COLUMNS/LINES"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// TestRefusedLayoutSpawnsNoChild. Refusal must happen BEFORE newPaneFor, the
// same ordering OpenPane uses: a rejected layout that has already forked 39 of
// its 40 shells has done real work and left it un-reaped.
func TestRefusedLayoutSpawnsNoChild(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")

	m := &Magmux{rows: 24, cols: 80, control: newControlPanel()}
	cfgs := make([]PaneConfig, 40)
	for i := range cfgs {
		cfgs[i] = PaneConfig{Cmd: "/bin/sh", Args: []string{"-c", "touch " + marker + "; sleep 5"}}
	}
	if err := m.buildGrid(cfgs); err == nil {
		t.Fatal("40 panes on an 80x24 terminal must be refused")
	}
	// Generous, because a child that WAS forked would need a moment to run.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child process was spawned for a layout that was then refused")
	}
}

// TestControlPanelCostsAPaneAtTheBoundary states the asymmetry out loud: with
// -c the panel is appended to commands and therefore consumes layout, so N
// panes can succeed while N panes + -c refuses. Without -c the panel is
// installed outside the tree and costs nothing.
func TestControlPanelCostsAPaneAtTheBoundary(t *testing.T) {
	// Find the largest N that fits at 80x24 with no panel.
	max := 0
	for n := 1; n <= 64; n++ {
		m := &Magmux{rows: 24, cols: 80, control: newControlPanel()}
		if err := m.buildGrid(ctrlPanes(n)); err == nil {
			max = n
		}
	}
	if max < 2 {
		t.Fatalf("only %d panes fit at 80x24; the floor has moved and this test is no longer measuring what it thinks", max)
	}
	t.Logf("largest layout that fits at 80x24: %d panes", max)

	// The panel is just another pane to the builders, so max+1 must refuse.
	m := &Magmux{rows: 24, cols: 80, control: newControlPanel()}
	withPanel := append(ctrlPanes(max), PaneConfig{Control: true})
	if err := m.buildGrid(withPanel); err == nil {
		t.Fatalf("%d panes plus -c fitted at 80x24, but %d panes is the maximum", max, max)
	}

	// And the hidden-panel path is unaffected at exactly that boundary: the
	// panel goes into m.allPanes, not into the tree.
	m2 := &Magmux{rows: 24, cols: 80, control: newControlPanel()}
	if err := m2.buildGrid(ctrlPanes(max)); err != nil {
		t.Fatalf("%d panes must still fit: %v", max, err)
	}
	before := len(m2.allPanes)
	m2.installHiddenPanel()
	if len(m2.allPanes) != before+1 {
		t.Fatalf("installHiddenPanel added %d panes, want 1", len(m2.allPanes)-before)
	}
	eachLeaf(m2.root, func(p *Pane) {
		if p.h < minPaneRows || p.w < minPaneCols {
			t.Errorf("the hidden panel reshaped a leaf below the floor: %dx%d", p.w, p.h)
		}
	})
}

// TestHeadlessRefusesTooManyPanes is the subprocess half: exit 1, the message on
// STDERR, and stdout still byte-empty. AC1.4 has to survive the new error path,
// which runs through main()'s `mux.restore(); os.Exit(1)` unwind.
func TestHeadlessRefusesTooManyPanes(t *testing.T) {
	args := []string{"--headless", "-w"}
	for i := 0; i < 40; i++ {
		args = append(args, "-e", fmt.Sprintf("echo pane-%d", i))
	}
	h := startHeadlessMagmux(t, args...)
	if code := h.wait(20 * time.Second); code != 1 {
		t.Fatalf("exit code %d, want 1\nstderr: %s", code, h.stderr.String())
	}
	h.requireSilentStdout()
	msg := h.stderr.String()
	for _, want := range []string{"40", "80x23", "cannot lay out"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr %q does not mention %q", msg, want)
		}
	}
}

// TestHeadlessAcceptsALayoutThatFits is the other side of the same boundary: the
// refusal must be exactly the unusable cases, not a blanket cap.
func TestHeadlessAcceptsALayoutThatFits(t *testing.T) {
	args := []string{"--headless", "-w"}
	for i := 0; i < 12; i++ {
		args = append(args, "-e", fmt.Sprintf("echo pane-%d", i))
	}
	h := startHeadlessMagmux(t, args...)
	if code := h.wait(30 * time.Second); code != 0 {
		t.Fatalf("12 panes at 80x24 must still start; exit %d\nstderr: %s", code, h.stderr.String())
	}
	h.requireSilentStdout()
}
