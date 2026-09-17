package mux

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func (m *Magmux) buildLayout(commands []PaneConfig) error {
	statusH := m.statusRowsLocked()
	availH := m.rows - statusH

	if len(commands) == 0 {
		return fmt.Errorf("no commands specified")
	}

	// Special layout for POC: top half split horizontal, bottom pane, status bar
	switch len(commands) {
	case 1:
		p, err := newPaneFor(0, 0, availH, m.cols, commands[0])
		if err != nil {
			return err
		}
		m.root = p
		m.allPanes = []*Pane{p}
		m.focused = p

	case 2:
		// Horizontal split
		m.root = &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: 0.5,
		}
		w1 := m.cols / 2
		w2 := m.cols - w1 - 1
		p1, err := newPaneFor(0, 0, availH, w1, commands[0])
		if err != nil {
			return err
		}
		p2, err := newPaneFor(0, w1+1, availH, w2, commands[1])
		if err != nil {
			return err
		}
		m.root.child1 = p1
		m.root.child2 = p2
		p1.parent = m.root
		p2.parent = m.root
		m.allPanes = []*Pane{p1, p2}
		m.focused = p1

	default: // 3+ panes: top row horizontal split, bottom pane(s)
		topH := availH * 2 / 3
		botH := availH - topH - 1

		// Top: horizontal split of first two commands
		topPane := &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: topH, w: m.cols,
			ratio: 0.5,
		}
		w1 := m.cols / 2
		w2 := m.cols - w1 - 1
		p1, err := newPaneFor(0, 0, topH, w1, commands[0])
		if err != nil {
			return err
		}
		p2, err := newPaneFor(0, w1+1, topH, w2, commands[1])
		if err != nil {
			return err
		}
		topPane.child1 = p1
		topPane.child2 = p2
		p1.parent = topPane
		p2.parent = topPane

		// Bottom pane
		p3, err := newPaneFor(topH+1, 0, botH, m.cols, commands[2])
		if err != nil {
			return err
		}

		// Root: vertical split (top | bottom)
		m.root = &Pane{
			splitType: SplitVertical,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: float64(topH) / float64(availH),
		}
		m.root.child1 = topPane
		m.root.child2 = p3
		topPane.parent = m.root
		p3.parent = m.root

		m.allPanes = []*Pane{p1, p2, p3}
		m.focused = p1
	}

	m.stampPaneIDs()
	return nil
}

// stampPaneIDs assigns each pane its permanent id — its index in m.allPanes.
// Called by both layout builders as the last step, before any goroutine can
// see a pane. Ids are written once and never touched again, which is what lets
// every other goroutine read Pane.id without treeMu.
func (m *Magmux) stampPaneIDs() {
	for i, p := range m.allPanes {
		p.id = i
		p.mux = m
	}
}

// layoutSpec is what an unusable-pane error has to say and what buildColumn's
// recursion cannot reconstruct: how many panes the CALLER asked for, and the
// terminal it asked for them in.
//
// The base case knows only its own one-element slice and its own box, so on its
// own it would report "cannot lay out 1 panes in 40x1" for a 40-pane request on
// an 80x24 terminal — true of the recursion and useless to the human, who has
// to decide between fewer -e commands and a bigger window.
type layoutSpec struct {
	panes      int
	rows, cols int
}

// errPanesDontFit names the count AND the geometry, because "no room" without
// either is a message a caller cannot act on. The COLUMNS/LINES hint is aimed
// at the headless caller, for whom those two env vars ARE the terminal.
//
// The floor is minPaneRows/minPaneCols — the SAME constants OpenPane and
// splitFits use, deliberately and with no second copy: "usable" cannot mean one
// thing for an agent's open_pane, another for Ctrl-G p, and a third for -e.
func errPanesDontFit(spec layoutSpec) error {
	return fmt.Errorf(
		"cannot lay out %d panes in %dx%d: every pane needs at least %d rows and %d columns "+
			"(use fewer -e commands, a larger terminal, or set COLUMNS/LINES)",
		spec.panes, spec.cols, spec.rows, minPaneRows, minPaneCols)
}

// buildColumn recursively splits a slice of commands into a vertical binary tree.
//
// It REFUSES a box too small to be usable rather than clamping it. That is the
// codebase's existing split, not a new rule: CREATION refuses, because the
// caller asked for something impossible and can still be told (OpenPane returns
// errPaneTooSmall, splitFits returns false, showPanelLocked says so in
// chromeNote); RESHAPE clamps, because a SIGWINCH has nobody to tell
// (reshapeChildren). buildColumn is creation. A clamped pane is alive,
// addressable and captures as empty, which turns a layout mistake into a
// scenario that silently reports no output — the same misattributed-diagnostics
// shape S-1 fixes on the other side.
func buildColumn(cmds []PaneConfig, y, x, h, w int, spec layoutSpec) (*Pane, []*Pane, error) {
	if len(cmds) == 1 {
		// EXHAUSTIVE: every leaf of every layout passes through here, so this
		// check alone is sufficient for correctness. The ones below it exist
		// only to refuse earlier.
		if h < minPaneRows || w < minPaneCols {
			return nil, nil, errPanesDontFit(spec)
		}
		p, err := newPaneFor(y, x, h, w, cmds[0])
		if err != nil {
			return nil, nil, err
		}
		return p, []*Pane{p}, nil
	}
	// Split: top half gets ceil(N/2), bottom gets rest
	topN := (len(cmds) + 1) / 2
	topH := h * topN / len(cmds)
	botH := h - topH - 1 // -1 for border

	// Not strictly necessary — a too-small half fails its own base case — but
	// refusing BEFORE recursing means no child process is spawned for a layout
	// already known to be impossible, and it keeps a negative height from being
	// passed down at all.
	if topH < minPaneRows || botH < minPaneRows {
		return nil, nil, errPanesDontFit(spec)
	}

	topPane, topLeaves, err := buildColumn(cmds[:topN], y, x, topH, w, spec)
	if err != nil {
		return nil, nil, err
	}
	botPane, botLeaves, err := buildColumn(cmds[topN:], y+topH+1, x, botH, w, spec)
	if err != nil {
		return nil, nil, err
	}

	parent := &Pane{
		splitType: SplitVertical,
		y:         y, x: x, h: h, w: w,
		ratio: float64(topH) / float64(h),
	}
	parent.child1 = topPane
	parent.child2 = botPane
	topPane.parent = parent
	botPane.parent = parent

	leaves := append(topLeaves, botLeaves...)
	return parent, leaves, nil
}

// buildGrid creates a balanced 2-column grid layout from a list of commands.
func (m *Magmux) buildGrid(commands []PaneConfig) error {
	statusH := m.statusRowsLocked()
	availH := m.rows - statusH

	if len(commands) == 0 {
		return fmt.Errorf("no commands specified")
	}

	// The count and the terminal, carried down so the refusal below can name
	// what the caller asked for rather than what the recursion is looking at.
	spec := layoutSpec{panes: len(commands), rows: availH, cols: m.cols}

	switch len(commands) {
	case 1:
		// Single pane — fullscreen. No border, because there is no split, so it
		// is charged for neither. The asymmetry with the branches below is
		// deliberate and mirrors splitFits, which also only charges for a border
		// when there is one.
		if availH < minPaneRows || m.cols < minPaneCols {
			return errPanesDontFit(spec)
		}
		p, err := newPaneFor(0, 0, availH, m.cols, commands[0])
		if err != nil {
			return err
		}
		m.root = p
		m.allPanes = []*Pane{p}
		m.focused = p

	case 2:
		// Horizontal split: left | right
		w1 := m.cols / 2
		w2 := m.cols - w1 - 1
		// Refused before newPaneFor, so no child process is ever spawned for a
		// layout that will be rejected — the ordering OpenPane already uses.
		if availH < minPaneRows || w1 < minPaneCols || w2 < minPaneCols {
			return errPanesDontFit(spec)
		}
		p1, err := newPaneFor(0, 0, availH, w1, commands[0])
		if err != nil {
			return err
		}
		p2, err := newPaneFor(0, w1+1, availH, w2, commands[1])
		if err != nil {
			return err
		}
		m.root = &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: 0.5,
		}
		m.root.child1 = p1
		m.root.child2 = p2
		p1.parent = m.root
		p2.parent = m.root
		m.allPanes = []*Pane{p1, p2}
		m.focused = p1

	default:
		// 3+ commands: left column gets ceil(N/2), right gets rest
		leftN := (len(commands) + 1) / 2
		leftW := m.cols / 2
		rightW := m.cols - leftW - 1

		// The column WIDTHS are this branch's own arithmetic; the heights are
		// buildColumn's and are checked there.
		if leftW < minPaneCols || rightW < minPaneCols {
			return errPanesDontFit(spec)
		}

		leftPane, leftLeaves, err := buildColumn(commands[:leftN], 0, 0, availH, leftW, spec)
		if err != nil {
			return err
		}
		rightPane, rightLeaves, err := buildColumn(commands[leftN:], 0, leftW+1, availH, rightW, spec)
		if err != nil {
			return err
		}

		m.root = &Pane{
			splitType: SplitHorizontal,
			y:         0, x: 0, h: availH, w: m.cols,
			ratio: float64(leftW) / float64(m.cols),
		}
		m.root.child1 = leftPane
		m.root.child2 = rightPane
		leftPane.parent = m.root
		rightPane.parent = m.root

		leaves := append(leftLeaves, rightLeaves...)
		m.allPanes = leaves
		m.focused = leaves[0]
	}

	// Mark all panes as grid mode
	for _, p := range m.allPanes {
		p.gridMode = true
	}

	m.stampPaneIDs()
	return nil
}

// ── Grid File Parser ─────────────────────────────────────────────────────────

func parseGridFile(path string) ([]PaneConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open grid file: %w", err)
	}
	defer f.Close()

	shell := getUserShell()
	var cmds []PaneConfig
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmds = append(cmds, PaneConfig{
			Cmd:  shell,
			Args: []string{"-l", "-c", line},
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read grid file: %w", err)
	}
	return cmds, nil
}

// ── Status File Polling ─────────────────────────────────────────────────────

// ── Grid Mode Exit Handling ─────────────────────────────────────────────────

// reapChild waits for a pane's child to exit and records the outcome. It is
// the half of waitForChild that has nothing to do with presentation, and it
// must run for EVERY spawned child, in every mode, or the child becomes a
// zombie for the life of magmux:
//
//   - nobody else calls cmd.Wait. readLoop notices the PTY closing and sets
//     p.dead, which looks like reaping and is not — the process entry survives
//     until it is waited on.
//   - p.reaped stays false without it, so reapPane's delayed SIGKILL always
//     fires on the force path, and the check that stops that kill landing on a
//     recycled pid becomes decorative.
//
// It is safe to call on a pane that was never published (an OpenPane unwind):
// it touches only that pane's own fields.
//
// Reports whether it actually waited, so waitForChild can skip painting a
// tombstone for a pane that had no child.
func (m *Magmux) reapChild(p *Pane) bool {
	if p == nil || p.cmd == nil {
		return false
	}
	err := p.cmd.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	if p.deadAt.IsZero() {
		p.deadAt = time.Now()
	}
	// The pid has now been collected and is free for the OS to reuse, so no
	// delayed force-kill may target its process group from here on.
	p.reaped = true
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			p.exitCode = exitErr.ExitCode()
		} else {
			p.exitCode = 1
		}
	} else {
		p.exitCode = 0
	}
	return true
}

// waitForChild reaps a pane's child and, in grid mode, turns its exit into the
// tombstone overlay and the `exit` event.
//
// The reaping is unconditional; only the PRESENTATION is grid-mode's. Outside
// grid mode there is no ✓ DONE overlay to paint and never has been — a plain
// multiplexer pane whose shell exits just goes quiet — but the child still has
// to be collected, which is why this is started for every pane rather than only
// for grid ones.
func (m *Magmux) waitForChild(p *Pane) {
	if !m.reapChild(p) {
		return
	}
	if !p.gridMode {
		return
	}
	p.mu.Lock()

	// Build completion overlay with duration + last output line
	var duration string
	if !p.startedAt.IsZero() {
		duration = formatDuration(time.Since(p.startedAt))
	}
	lastMsg := p.lastNonEmptyLine(40)

	var header string
	if p.exitCode == 0 {
		header = "\u2713 DONE"
		p.overlayStyle = "success"
		p.tint = "green"
	} else {
		header = fmt.Sprintf("\u2717 FAIL (exit %d)", p.exitCode)
		p.overlayStyle = "error"
		p.tint = "red"
	}

	var lines []string
	lines = append(lines, header)
	if duration != "" {
		lines = append(lines, "took "+duration)
	}
	if lastMsg != "" {
		lines = append(lines, lastMsg)
	}
	p.overlayText = strings.Join(lines, "\n")
	p.dirty = true
	// Capture fields for broadcast under the lock, then release before I/O.
	exitCode := p.exitCode
	snap := p.controllerSnap
	p.mu.Unlock()

	// Push an exit event over the IPC socket so subscribers (e.g. claudish)
	// learn about per-pane completion without polling files. p.id replaces the
	// old linear scan of m.allPanes: the id IS the index and never changes, so
	// the scan was both slower and — once panes can be appended concurrently —
	// a read of the slice header from the wrong goroutine.
	m.broadcastEvent(map[string]any{
		"type":     "exit",
		"pane":     p.id,
		"exitCode": exitCode,
		"duration": duration,
		"lastLine": lastMsg,
		"response": snap.LastResponse,
		"prompt":   snap.LastUserPrompt,
		"tool":     snap.LastTool,
		"model":    snap.Model,
	})
}

// buildPaneResults collects the state of every pane into a serializable slice.
// It backs three things — the connect-time aggregate `snapshot`, the shutdown
// `results` event, and the `list` verb — and that is deliberate: one code path
// means a client polling `list` and a subscriber reading `results` can never be
// told different things about the same pane.
//
// Fields beyond the original state/exitCode/dead set are added only when they
// can be sourced; an observer that cannot see a pane's pid is better served by
// the key being absent than by a zero. Unknown fields are documented as
// ignorable (README), so the additions are safe for existing subscribers.
func (m *Magmux) buildPaneResults() []map[string]any {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.buildPaneResultsLocked()
}

// buildPaneResultsLocked is the twin for callers already holding treeMu.
func (m *Magmux) buildPaneResultsLocked() []map[string]any {
	results := make([]map[string]any, 0, len(m.allPanes))
	focused := m.focused
	for _, p := range m.allPanes {
		if p == nil {
			continue
		}
		if p.closed {
			// Tombstones are reported, not omitted. Ids are sparse after a
			// close and a subscriber that treated results as a dense array
			// would otherwise silently shift every pane after the hole.
			entry := map[string]any{
				"pane": p.id, "state": "closed", "closed": true, "dead": true,
			}
			if p.label != "" {
				entry["label"] = p.label
			}
			results = append(results, entry)
			continue
		}
		if p.isControl {
			// Reported rather than omitted, so a subscriber walking results
			// still sees every pane index and can tell why this one has no
			// session state of its own.
			//
			// state stays "panel" whether the panel is on screen or hidden.
			// Hidden is not a state of the SESSION — there is no session — it
			// is a fact about magmux's own chrome, so it rides as its own
			// field. test/ui/case3.ts asserts state == "panel"; a magmux
			// started without -c must not answer that differently.
			results = append(results, map[string]any{
				"pane": p.id, "state": "panel", "control": true,
				"hidden": p.hidden,
			})
			continue
		}
		p.mu.Lock()
		snap := p.controllerSnap
		var state string
		switch {
		case p.dead && !p.reaped && p.cmd != nil:
			// The PTY closed but cmd.Wait has not recorded the status, so
			// p.exitCode is still its zero value and "completed" is a claim we
			// cannot back. Report the state it had an instant ago rather than
			// invent a success — a failed run reported as passing is the worst
			// failure shape available, and headless makes `results` the only
			// report anybody reads.
			//
			// REACHABLE IN THE FINAL RESULTS, and an earlier version of this
			// comment claimed otherwise. `-w` does not wait for `reaped`: it
			// waits up to reapGrace and then gives up (paneDoneLocked), so a
			// child that closes its PTY and keeps running — `sh -c 'exec
			// 0<&- 1>&- 2>&-; sleep 300'`, or merely a cmd.Wait lagging EOF by
			// more than reapGrace on a loaded machine — lands here in the very
			// last report the run produces. Do not delete this branch as dead
			// code; it is the only thing standing between that pane and a
			// fabricated "completed".
			//
			// `exitCode` still rides along as 0 because the key is part of the
			// schema and removing it for one state would be a breaking change
			// (CLAUDE.md: "exitCode and dead keep their keys"). A consumer must
			// read `state` first — which is exactly why `state` is the honest
			// one here.
			state = "running"
		case p.dead && p.exitCode == 0:
			state = "completed"
		case p.dead && p.exitCode != 0:
			state = "failed"
		case p.inputReady:
			state = "awaiting_input"
		default:
			state = "running"
		}
		entry := map[string]any{
			"pane":     p.id,
			"state":    state,
			"exitCode": p.exitCode,
			"dead":     p.dead,
			"focused":  p == focused,
			"altMode":  p.altMode,
		}
		if p.label != "" {
			entry["label"] = p.label
		}
		if s := p.screen; s != nil {
			// The geometry an observer needs to read the text `capture`
			// returns: without the width there is no telling a wrapped line
			// from a hard one.
			entry["rows"], entry["cols"] = s.rows, s.cols
		}
		if p.inputSignal != "" {
			// Which heuristic called the pane idle — "osc", "2004", "title",
			// "idle", "ctrl", "perm". Two of those are far weaker evidence than
			// the others, and a reader deciding whether to trust awaiting_input
			// has no way to tell them apart from the state alone.
			entry["inputSignal"] = p.inputSignal
		}
		if c := p.cmd; c != nil {
			if len(c.Args) > 0 {
				entry["cmd"] = strings.Join(c.Args, " ")
			} else if c.Path != "" {
				entry["cmd"] = c.Path
			}
			if c.Dir != "" {
				entry["cwd"] = c.Dir
			}
			if c.Process != nil {
				entry["pid"] = c.Process.Pid
			}
		}
		if p.controller != nil {
			entry["controller"] = p.controller.Name()
		}
		if snap.Model != "" {
			entry["model"] = snap.Model
		}
		if snap.Project != "" {
			entry["project"] = snap.Project
		}
		if snap.LastUserPrompt != "" {
			entry["prompt"] = snap.LastUserPrompt
		}
		if snap.LastResponse != "" {
			entry["response"] = snap.LastResponse
		}
		if snap.LastTool != "" {
			entry["tool"] = snap.LastTool
		}
		if !snap.StartedAt.IsZero() {
			entry["startedAt"] = snap.StartedAt.UTC().Format(time.RFC3339)
		}
		if !snap.CompletedAt.IsZero() {
			entry["completedAt"] = snap.CompletedAt.UTC().Format(time.RFC3339)
		}
		p.mu.Unlock()
		results = append(results, entry)
	}
	return results
}

// allPanesDone returns true if every session pane is either dead or
// inputReady. The control panel is not a session — it is never "done" and has
// no process to exit, so counting it would deadlock -w forever.
func (m *Magmux) allPanesDone() bool {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.allPanesDoneLocked()
}

// allPanesDoneLocked is the twin render() calls, which holds treeMu.RLock for
// its whole body. Taking a second RLock there would deadlock the moment a
// writer queued between the two — silently, with no race report, which is why
// the twin is mandatory rather than a style preference.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) allPanesDoneLocked() bool {
	sessions := 0
	for _, p := range m.allPanes {
		// A tombstone is not an unfinished session — counting one would mean
		// -w could never fire after an agent closed a pane.
		if p == nil || p.closed || p.isControl {
			continue
		}
		sessions++
		p.mu.Lock()
		done := paneDoneLocked(p, m.noIdleDone)
		p.mu.Unlock()
		if !done {
			return false
		}
	}
	return sessions > 0
}

// allPanesDead is allPanesDone's stricter twin: every session pane's PROCESS is
// gone, not merely resting.
//
// The two exist because "done" answers two questions that have different right
// answers. For -w and auto-exit, done means "there is nothing left to wait
// for", and an agent idling between turns qualifies. For the input loop, the
// question is "is there still something to type into", and an idle agent
// emphatically is: the process is alive and blocked on a read.
//
// inputLoop used to ask allPanesDone, so a single idle pane turned every plain
// key into a candidate for quit and swallowed the rest — a lone `magmux -e
// claude` was untypeable from the first frame, before writePTY was ever
// reached. Dismissing a FINISHED grid with `q` was always the point of that
// branch, and a finished grid is a dead one.
//
// Caller must NOT hold treeMu.
func (m *Magmux) allPanesDead() bool {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.allPanesDeadLocked()
}

// allPanesDeadLocked is the twin for callers already inside treeMu. See
// allPanesDoneLocked for why the twin is mandatory rather than a style
// preference.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) allPanesDeadLocked() bool {
	sessions := 0
	for _, p := range m.allPanes {
		if p == nil || p.closed || p.isControl {
			continue
		}
		sessions++
		p.mu.Lock()
		alive := !p.dead
		p.mu.Unlock()
		if alive {
			return false
		}
	}
	return sessions > 0
}

// reapGrace bounds how long -w waits, after a pane's PTY has closed, for
// cmd.Wait to record the real exit status. Beyond it the pane is called done
// anyway: a child that is stopped (SIGSTOP), or whose PTY was inherited by a
// grandchild that outlives it, must not be able to make -w hang forever.
//
// Two seconds is far longer than the microseconds the two events are normally
// apart — reapChild is parked in wait4 and returns when the process exits,
// while readLoop returns on PTY EOF — and far shorter than any human's patience.
const reapGrace = 2 * time.Second

// paneDoneLocked is the ONE definition of "this session pane has finished".
//
// Extracted from allPanesDoneLocked so its three consumers — the q/Esc dismiss
// of a finished grid, the completion timestamp behind the status bar's "done"
// count, and -w's auto-exit — cannot drift apart. One definition of done is the
// point; the fix is not special-cased to -w.
//
// Caller holds p.mu.
func paneDoneLocked(p *Pane, noIdleDone bool) bool {
	// An idle agent pane never exits; it finished its TURN. The reaped
	// requirement below applies only to the DEAD branch, or -w would never fire
	// for a Claude Code pane at all.
	//
	// noIdleDone (--no-idle-done) withdraws exactly this CLAIM and nothing
	// else: p.inputReady is still set and still reported, but magmux stops
	// treating an idle turn as a finished session, so -w waits for the process
	// to exit. It is a parameter rather than a second predicate beside this one
	// because "done" having two definitions is the defect this function exists
	// to close.
	if p.inputReady && !noIdleDone {
		return true
	}
	if !p.dead {
		return false
	}
	// DEAD needs proof. reapChild writes dead, reaped and exitCode under ONE
	// p.mu acquisition, so reaped == true IMPLIES exitCode is final. readLoop
	// sets dead ALONE on PTY EOF, and that is the state that used to be
	// published as {"state":"completed","exitCode":0} for a child that exited 7.
	if p.reaped {
		return true
	}
	// A pane with no child has no status to wait for and will never be reaped
	// (reapChild returns early on a nil cmd). It cannot be allowed to hold -w
	// open for the grace period, let alone forever.
	if p.cmd == nil {
		return true
	}
	// Bounded, so a cmd.Wait that never returns cannot wedge -w. deadAt's zero
	// value is what keeps every existing Pane{dead:true} literal behaving as it
	// does today: time.Since(zero time) is centuries, so this fires at once.
	return time.Since(p.deadAt) > reapGrace
}
