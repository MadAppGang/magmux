package mux

// The magmux side of the generic tier: the OSC 133 marks, the job-control
// latch, and the port that hands both to the observation layer.
//
// NOTHING here reads ~/.claude, speaks to tmux, or asks about a pid it does
// not own. The tests that need a real terminal OPEN ONE AND FORK THEIR OWN
// CHILD into it — that is the carve-out phase 5 established, and it is the
// only way to measure a kernel fact about job control. Every such test kills
// and reaps what it started.

import (
	"context"
	"github.com/MadAppGang/magmux/pty"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/obsview"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// giveTerminal makes a PTY-less test pane look like one that has a terminal,
// without opening one. Nothing reads this file: every test that uses it
// replaces the tcgetpgrp seam, and the field is only ever tested for nil —
// "does this pane have a terminal to ask about at all".
func giveTerminal(t *testing.T, p *Pane) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ptmx = r
}

// staged replays a fixed sequence of (foreground, leader) readings, one per
// observation, so a latch whose whole subject is the ORDER of two readings can
// be driven exactly. Past the end it repeats the last reading, which is what a
// pane that has settled looks like.
func staged(readings ...[2]int) func(*Pane) (int, int, bool) {
	i := 0
	return func(*Pane) (int, int, bool) {
		r := readings[min(i, len(readings)-1)]
		i++
		if r[0] == 0 {
			return 0, 0, false
		}
		return r[0], r[1], true
	}
}

// shellOf drives one observation through the real port and returns what the
// layer would see.
func shellOf(t *testing.T, l *muxPaneLocator, pane int) model.ShellState {
	t.Helper()
	st, ok := l.Shell(obsview.PaneRef(pane))
	if !ok {
		t.Fatalf("pane %d: the shell port declined to answer", pane)
	}
	return st
}

// ── the latch ───────────────────────────────────────────────────────────────

// TestForegroundEqualityIsWorthNothingUntilTheLatchIsProven is architecture v4
// §0.2, stated as a test.
//
// A WRONG IMPLEMENTATION compares the foreground group to the session leader
// and answers "at a prompt" when they match. That is right for an interactive
// shell and WRONG for a non-interactive one, which is the foreground group for
// the entire run of every command it is given — so every `-e` pane in magmux
// reports a permanently idle shell. A wrong "idle" is worse than
// "undetermined" because a user acts on it.
func TestForegroundEqualityIsWorthNothingUntilTheLatchIsProven(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	p := m.paneByID(0)
	giveTerminal(t, p)

	// A non-interactive shell: the leader is the foreground group, always,
	// running or not. Ten looks, no flip.
	l := &muxPaneLocator{m: m, foreground: staged([2]int{700, 700})}
	for i := 0; i < 10; i++ {
		if got := shellOf(t, l, 0).AtPrompt; got != model.LivenessUnknown {
			t.Fatalf("look %d answered %v; foreground equality means nothing until this pane "+
				"has been seen to hand its terminal to something and take it back", i, got)
		}
	}
	if p.shell.proven {
		t.Fatal("the latch proved itself without ever seeing a flip")
	}
}

// TestTheLatchSetsOnTheFlipAndNotBefore is the latch's whole state machine,
// read as a sequence.
//
// A WRONG IMPLEMENTATION sets it on the first HALF of the flip — the moment a
// foreign foreground group is seen — and then answers "at a prompt" as soon as
// the command ends. That happens to be right, and it is right by luck: what it
// has proven at that point is that something took the terminal, not that the
// shell takes it back, and a shell that never takes it back is a shell whose
// equality still means nothing.
func TestTheLatchSetsOnTheFlipAndNotBefore(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	p := m.paneByID(0)
	giveTerminal(t, p)

	l := &muxPaneLocator{m: m, foreground: staged(
		[2]int{700, 700}, // at the prompt, unproven
		[2]int{812, 700}, // a command took the terminal: half the flip
		[2]int{812, 700}, // still running
		[2]int{700, 700}, // and back — THE FLIP
		[2]int{700, 700}, // still at the prompt
	)}

	want := []model.Liveness{
		model.LivenessUnknown,
		model.LivenessNo,
		model.LivenessNo,
		model.LivenessYes,
		model.LivenessYes,
	}
	for i, w := range want {
		if got := shellOf(t, l, 0).AtPrompt; got != w {
			t.Fatalf("look %d answered %v, want %v", i, got, w)
		}
	}
	if !p.shell.proven {
		t.Error("the latch did not record the flip it just answered from")
	}
}

// TestTheLatchOnlyEverSets.
//
// A WRONG IMPLEMENTATION recomputes it, or clears it when a reading cannot be
// taken. The proof is about the SHELL — that it does job control at all — and
// nothing it does later makes that untrue. A latch that could be un-set would
// have a pane oscillating between "at a prompt" and "cannot tell" on nothing
// but the timing of the looks.
func TestTheLatchOnlyEverSets(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	p := m.paneByID(0)
	giveTerminal(t, p)

	l := &muxPaneLocator{m: m, foreground: staged(
		[2]int{812, 700}, // a command holds the terminal
		[2]int{700, 700}, // the flip: proven
		[2]int{0, 0},     // the reading fails — a closed master, a dead session
		[2]int{812, 700}, // another command
		[2]int{700, 700}, // back at the prompt
	)}

	for i := 0; i < 5; i++ {
		got := shellOf(t, l, 0).AtPrompt
		if i >= 1 && !p.shell.proven {
			t.Fatalf("look %d: the latch came un-set", i)
		}
		if i == 2 && got != model.LivenessUnknown {
			t.Fatalf("look %d answered %v from a reading that failed", i, got)
		}
	}
	if got := shellOf(t, l, 0).AtPrompt; got != model.LivenessYes {
		t.Fatalf("after a failed reading the proven latch answered %v, want yes", got)
	}
}

// TestTheLatchIsPerPane.
//
// A WRONG IMPLEMENTATION keeps it on the locator, which is shared by every
// session's actor. One interactive shell proving job control would then make
// every other pane in the terminal — every `-e` pane, every non-interactive
// build — report a prompt it has never been seen to have.
func TestTheLatchIsPerPane(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	for _, id := range []int{0, 1} {
		giveTerminal(t, m.paneByID(id))
	}

	// Pane 0 flips; pane 1 never does. They share the locator and the seam.
	byPane := map[int][][2]int{
		0: {{812, 700}, {700, 700}},
		1: {{900, 900}, {900, 900}},
	}
	next := map[int]int{}
	l := &muxPaneLocator{m: m, foreground: func(p *Pane) (int, int, bool) {
		seq := byPane[p.id]
		r := seq[min(next[p.id], len(seq)-1)]
		next[p.id]++
		return r[0], r[1], true
	}}

	for i := 0; i < 2; i++ {
		l.Shell(obsview.PaneRef(0))
		l.Shell(obsview.PaneRef(1))
	}
	if got := shellOf(t, l, 0).AtPrompt; got != model.LivenessYes {
		t.Errorf("pane 0 (flipped) answered %v, want yes", got)
	}
	if got := shellOf(t, l, 1).AtPrompt; got != model.LivenessUnknown {
		t.Errorf("pane 1 (never flipped) answered %v, want unknown — it borrowed pane 0's proof", got)
	}
}

// TestAPaneWithNoTerminalDeclinesToAnswer.
//
// A WRONG IMPLEMENTATION returns a zero [model.ShellState] with ok true, which
// downstream reads as "this shell says nothing is running" — indistinguishable
// from a real shell that emits no marks. magmux's own control panel has no PTY
// and never will, and a closed pane's master is closed.
func TestAPaneWithNoTerminalDeclinesToAnswer(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	l := &muxPaneLocator{m: m, foreground: staged([2]int{700, 700})}

	if _, ok := l.Shell(obsview.PaneRef(0)); ok {
		t.Error("a pane with no PTY answered the shell port")
	}
	if _, ok := l.Shell(obsview.PaneRef(404)); ok {
		t.Error("a pane that does not exist answered the shell port")
	}
	if _, ok := l.Shell(obsview.PaneRef(-1)); ok {
		t.Error("a negative id is not an address, and it answered")
	}
}

// ── OSC 133, through the real parser ────────────────────────────────────────

// markPane builds a pane whose VT parser is wired up, and writes escape
// sequences into it exactly as readLoop would.
func markPane(t *testing.T) *Pane {
	t.Helper()
	m := newTestMux(t, ctrlPanes(1)...)
	return m.paneByID(0)
}

func writeOSC(p *Pane, body string) {
	p.vt.write([]byte("\x1b]" + body + "\x07"))
}

// TestOSC133IsParsedIntoTheTwoStatesThatMatter.
//
// A WRONG IMPLEMENTATION treats the four letters as four states and asks a
// consumer to know that A and B are both "at a prompt". The distinction
// between them is about DRAWING a prompt; an observer wants to know whether
// work is happening, and that has two answers.
func TestOSC133IsParsedIntoTheTwoStatesThatMatter(t *testing.T) {
	for _, tt := range []struct {
		body string
		want model.ShellMark
	}{
		{"133;A", model.ShellMarkPrompt},
		{"133;B", model.ShellMarkPrompt},
		{"133;C", model.ShellMarkRunning},
		{"133;D", model.ShellMarkPrompt},
		{"133;D;0", model.ShellMarkPrompt},
		{"133;A;aid=7", model.ShellMarkPrompt},
		{"133;C;cmdline=make%20all", model.ShellMarkRunning},
	} {
		t.Run(tt.body, func(t *testing.T) {
			p := markPane(t)
			writeOSC(p, tt.body)
			if p.shell.mark != tt.want {
				t.Fatalf("%s left mark %v, want %v", tt.body, p.shell.mark, tt.want)
			}
		})
	}
}

// TestTheExitStatusIsThreeStatesAtTheParser.
//
// A WRONG IMPLEMENTATION defaults a bare `133;D` to zero. A shell that reports
// no status is not a shell whose commands all succeed, and this is the only
// generic signal in the whole layer that can say a command FAILED — folding
// its "nobody said" into "it worked" throws it away at the first step.
func TestTheExitStatusIsThreeStatesAtTheParser(t *testing.T) {
	for _, tt := range []struct {
		body      string
		wantKnown bool
		wantCode  int
	}{
		{"133;D", false, 0},
		{"133;D;", false, 0},
		{"133;D;0", true, 0},
		{"133;D;1", true, 1},
		{"133;D;130", true, 130},
		{"133;D;0;aid=7", true, 0},
		{"133;D;SIGINT", false, 0}, // not a number: not a status
	} {
		t.Run(tt.body, func(t *testing.T) {
			p := markPane(t)
			writeOSC(p, "133;C")
			writeOSC(p, tt.body)
			if p.shell.exitKnown != tt.wantKnown || p.shell.exit != tt.wantCode {
				t.Fatalf("%s left exit=%d known=%v, want %d/%v",
					tt.body, p.shell.exit, p.shell.exitKnown, tt.wantCode, tt.wantKnown)
			}
		})
	}
}

// TestANewCommandDoesNotClearTheLastStatusAndAFinishDoes.
//
// A WRONG IMPLEMENTATION keeps a stale status across a command that finished
// without reporting one, so a shell that succeeded at 10am reports that
// success against every later command that says nothing. Exit is the LAST
// COMPLETED command's outcome, and a bare 133;D is a completion with no
// outcome reported.
func TestANewCommandDoesNotClearTheLastStatusAndAFinishDoes(t *testing.T) {
	p := markPane(t)

	writeOSC(p, "133;C")
	writeOSC(p, "133;D;3")
	writeOSC(p, "133;C") // the next command starts; the last one's status stands
	if !p.shell.exitKnown || p.shell.exit != 3 {
		t.Fatalf("a running command cleared the previous one's status: exit=%d known=%v",
			p.shell.exit, p.shell.exitKnown)
	}
	writeOSC(p, "133;D") // ... and finishes without saying how
	if p.shell.exitKnown {
		t.Fatalf("a bare 133;D left the previous command's status standing: exit=%d", p.shell.exit)
	}
}

// TestUnknownMembersOfThe133FamilyAreIgnored.
//
// A WRONG IMPLEMENTATION guesses at the letters it does not know. 133;P is a
// property, 133;E is the command line text, and neither says anything about
// whether work is happening — a default branch that moved the state would have
// a future shell's new mark silently rewriting magmux's answer.
func TestUnknownMembersOfThe133FamilyAreIgnored(t *testing.T) {
	p := markPane(t)
	writeOSC(p, "133;C")
	for _, body := range []string{"133;P;k=i", "133;E;cmdline=ls", "133;Z", "133;"} {
		writeOSC(p, body)
		if p.shell.mark != model.ShellMarkRunning {
			t.Fatalf("%s changed the mark to %v", body, p.shell.mark)
		}
	}
}

// TestOSC133IsNotAThirdWriterOfInputReady is the decision this phase made
// about magmux's own idle detection, pinned so that it is a decision and not a
// drift.
//
// magmux reconciles exactly TWO sources of pane idleness — the terminal's
// heuristics and a controller's transcript — ordered in applyTerminalIdle so
// that the fresher wins and the state cannot oscillate. The OSC handlers
// beside this one (OSC 9, 777;notify, 633;B) feed that merge. This one feeds
// the observation layer through the pull port and nothing else.
//
// A WRONG IMPLEMENTATION sets inputReady from 133;A because a shell at a
// prompt IS waiting for input — which is true, and is a change to magmux's
// existing behaviour for every shell pane on the machine, and belongs to its
// own change with its own evidence.
func TestOSC133IsNotAThirdWriterOfInputReady(t *testing.T) {
	p := markPane(t)
	// The gate the existing notification handlers are behind, opened, so that
	// this test fails for the right reason rather than passing because the
	// pane was ineligible.
	p.altMode = true

	for _, body := range []string{"133;A", "133;B", "133;C", "133;D;0", "133;D;1"} {
		writeOSC(p, body)
		if p.inputReady {
			t.Fatalf("%s set inputReady; OSC 133 feeds the observation layer, not the merge", body)
		}
		if p.inputSignal != "" {
			t.Fatalf("%s wrote inputSignal %q", body, p.inputSignal)
		}
	}

	// The positive control: 633;B is VS Code's spelling of the same family and
	// IS a writer, and has been since long before this phase. If this stops
	// firing, the test above is asserting nothing.
	writeOSC(p, "633;B")
	if !p.inputReady || p.inputSignal != "osc" {
		t.Fatalf("633;B left inputReady=%v signal=%q; the contrast this test rests on is gone",
			p.inputReady, p.inputSignal)
	}
}

// TestTheMarksReachTheLayerThroughThePort ties the two halves together: what
// the parser recorded is what the observation layer is handed.
func TestTheMarksReachTheLayerThroughThePort(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	p := m.paneByID(0)
	giveTerminal(t, p)
	l := &muxPaneLocator{m: m, foreground: staged([2]int{0, 0})}

	writeOSC(p, "133;C")
	if got := shellOf(t, l, 0); got.Mark != model.ShellMarkRunning || got.Exit != nil {
		t.Fatalf("port reported %+v, want a running command and no status", got)
	}

	writeOSC(p, "133;D;2")
	got := shellOf(t, l, 0)
	if got.Mark != model.ShellMarkPrompt {
		t.Fatalf("port reported mark %v, want prompt", got.Mark)
	}
	if got.Exit == nil {
		t.Fatal("port reported no exit status for a command that reported one")
	}
	if *got.Exit != 2 {
		t.Fatalf("port reported exit %d, want a determined 2", *got.Exit)
	}

	// A FRESH pointer per call: the value must not track the pane afterwards.
	held := got.Exit
	writeOSC(p, "133;C")
	writeOSC(p, "133;D;0")
	if *held != 2 {
		t.Fatalf("the pointer handed to a consumer changed underneath it: %d", *held)
	}
}

// TestOSC133CostsTheParserNothingItWasNotAlreadySpending measures the price of
// the three lines this phase added to the VT parser, which is the hottest path
// in the program — scrollUp, in the same file, is documented as not allocating
// for exactly this reason.
//
// The comparison is against an OSC sequence of the same length that falls
// through every case in the same switch, so what is being measured is the
// added case and not the OSC machinery around it. Equal is the requirement:
// the new case is one prefix test on a string handleOSC had already built, and
// noteShellMark slices that string rather than copying it.
//
// A WRONG IMPLEMENTATION allocates per mark — a string copy, a slice of
// fields, a *int for the exit status — and pays it once per command on every
// pane in the terminal. The pointer the model wants is built once per
// OBSERVATION instead, in the port, where allocating is free.
func TestOSC133CostsTheParserNothingItWasNotAlreadySpending(t *testing.T) {
	p := markPane(t)
	mark := []byte("\x1b]133;D;1\x07")
	// Same length, same shape, matched by nothing.
	other := []byte("\x1b]933;D;1\x07")

	got := testing.AllocsPerRun(2000, func() { p.vt.write(mark) })
	base := testing.AllocsPerRun(2000, func() { p.vt.write(other) })
	if got > base {
		t.Fatalf("an OSC 133 sequence allocates %.0f, an ignored one of the same length %.0f; "+
			"the added case is not free", got, base)
	}
	t.Logf("OSC 133: %.0f allocs/op, ignored OSC of the same length: %.0f", got, base)
}

// TestANonNumericExitStatusAllocatesNothing — fix round 1, item 5.
//
// The test above only ever fed a NUMERIC status, and a numeric status never
// allocated. A shell reporting a signal name — `133;D;SIGINT` — went through
// strconv.Atoi's rejection path, which builds a *NumError: two allocations per
// finished command, against a comment on noteShellMark saying none. The claim
// is now true for every payload, and this is the payload that made it false.
//
// Measured twice, the way the test above measures: once at noteShellMark
// directly, where the answer must be exactly zero, and once through the
// parser against an ignored OSC of the same length, so the added case is
// compared with the machinery it sits in.
func TestANonNumericExitStatusAllocatesNothing(t *testing.T) {
	p := markPane(t)
	for _, body := range []string{
		"D;SIGINT",
		"D;SIGINT;aid=7",
		"D;1a",
		"D;-",
		"D;+",
		"D;99999999999999999999999", // out of range: Atoi's other error
		"D;130",
		"D;",
		"D",
	} {
		if got := testing.AllocsPerRun(1000, func() { p.noteShellMark(body) }); got != 0 {
			t.Errorf("noteShellMark(%q) allocates %.0f per call; it is on the parser's path and "+
				"documented as allocation-free for any payload", body, got)
		}
	}

	mark := []byte("\x1b]133;D;SIGINT\x07")
	other := []byte("\x1b]933;D;SIGINT\x07")
	got := testing.AllocsPerRun(2000, func() { p.vt.write(mark) })
	base := testing.AllocsPerRun(2000, func() { p.vt.write(other) })
	if got > base {
		t.Fatalf("133;D;SIGINT allocates %.0f, an ignored OSC of the same length %.0f", got, base)
	}
	t.Logf("133;D;SIGINT: %.0f allocs/op, ignored OSC of the same length: %.0f", got, base)
}

// TestParseExitStatusAgreesWithAtoi holds the hand-rolled scan to the function
// it replaced. It exists so that removing an allocation cannot quietly change
// which statuses are statuses: every input strconv.Atoi accepts is accepted
// with the same value, and every one it rejects is rejected.
func TestParseExitStatusAgreesWithAtoi(t *testing.T) {
	inputs := []string{
		"", "0", "1", "130", "255", "-1", "+1", "007", "-0", "+", "-", "--1", "+-1",
		"1 ", " 1", "1a", "a1", "SIGINT", "0x1", "1e3", "1_000", "١",
		"9223372036854775807", "9223372036854775808", "-9223372036854775808",
		"-9223372036854775809", "99999999999999999999999",
	}
	// And a deterministic spread of short strings over the characters that
	// matter, so the boundaries are not only the ones somebody thought of.
	const alphabet = "0123456789+-a "
	seed := uint32(0x9e3779b9)
	for i := 0; i < 5000; i++ {
		seed = seed*1664525 + 1013904223
		b := make([]byte, int(seed>>28)%7)
		for j := range b {
			seed = seed*1664525 + 1013904223
			b[j] = alphabet[int(seed>>16)%len(alphabet)]
		}
		inputs = append(inputs, string(b))
	}
	for _, in := range inputs {
		want, err := strconv.Atoi(in)
		got, ok := parseExitStatus(in)
		if ok != (err == nil) || (ok && got != want) {
			t.Errorf("parseExitStatus(%q) = %d, %v; strconv.Atoi says %d, %v", in, got, ok, want, err)
		}
	}
	// The field separator still ends the status.
	if got, ok := parseExitStatus("2;aid=7"); !ok || got != 2 {
		t.Errorf("parseExitStatus(%q) = %d, %v; want 2, true", "2;aid=7", got, ok)
	}
}

// ── the wiring ──────────────────────────────────────────────────────────────

// TestStartObservationWiresTheShellPort, and it is here because the obvious
// break — deleting `Shell: locator` from the pipeline config — passed every
// other test in this repository.
//
// A WRONG IMPLEMENTATION builds magmux's pipeline without the port. Everything
// in this file still passes, because everything in this file drives the
// locator directly; the verdict package still passes, because it is tested
// against values. The whole phase is switched off INSIDE the one process that
// has the evidence for it, and the document still looks complete.
//
// The assertion is the conflict-withdrawal rule, end to end through magmux's
// own ports: a record that says busy, a pane silent for ten minutes, and a
// shell that says a command is still running. Phase 5 publishes a conflict for
// the first two. With the shell port wired, the third withdraws it — so a
// conflict HERE means the port is absent.
func TestStartObservationWiresTheShellPort(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	const pid = 4242
	m := newTestMux(t, ctrlPanes(1)...)
	p := m.paneByID(0)
	givePID(t, p, pid)
	giveTerminal(t, p)
	stampActivity(t, p, time.Now().Add(-10*time.Minute), time.Time{})
	// The shell said a command is running, and nothing has been heard since.
	writeOSC(p, "133;C")

	// The oracle is nil on this path, so no pid anywhere is probed; pid 4242 is
	// a number in a temporary file and nothing signals it.
	m.panelPrivate = true // ADR-X5: observation only runs on a magmux whose panel is on-box
	if err := m.startObservation(newSilentPaneRegistry(t, pid, "busy")); err != nil {
		t.Fatalf("startObservation: %v", err)
	}
	defer m.stopObservation()

	sub, err := m.obs.p.Subscribe(context.Background(), "test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	m.obs.p.Refresh()
	select {
	case d, ok := <-sub.C():
		if !ok || len(d.Doc.Sessions) != 1 {
			t.Fatalf("the document is %+v (ok=%v)", d.Doc.Sessions, ok)
		}
		if c := d.Doc.Sessions[0].Conflict; c != nil {
			t.Fatalf("magmux's own pipeline contradicted a busy record against a pane whose "+
				"shell says a command is still running: the shell port is not wired in (%+v)", *c)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}
}

// ── the measurement, against a real terminal and our own child ──────────────

// shellPath picks a shell to measure with. A missing shell is an environment
// fact and is skipped; a shell that is present and behaves differently is a
// failure, which is the point.
func shellPath(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/zsh", "/bin/bash"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no /bin/zsh or /bin/bash on this machine")
	return ""
}

// paneRunning spawns cmd on a REAL pty, in its own session, exactly as
// spawnPTY does, and hands back a pane wired to it. The child is this test's
// own: nothing here asks about a pid it did not fork.
func paneRunning(t *testing.T, m *Magmux, id int, name string, args ...string) *Pane {
	t.Helper()
	ptmx, pts, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.Env = append(os.Environ(), "TERM=dumb")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		pts.Close()
		ptmx.Close()
		t.Fatalf("start %s: %v", name, err)
	}
	pts.Close()

	// Drain for the whole run. A full pty buffer blocks the child, which would
	// turn a measurement of job control into a measurement of this test's
	// reading habits.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		ptmx.Close()
	})

	p := m.paneByID(id)
	p.mu.Lock()
	p.ptmx, p.cmd = ptmx, cmd
	p.mu.Unlock()
	return p
}

// TestANonInteractiveShellRunningACommandReportsUndetermined is architecture
// v4 §0.2's measurement, reproduced against the real syscall and a real shell.
//
// This is the whole reason the latch exists. `zsh -l -c 'sleep 2'` has no job
// control: the shell IS the foreground process group for the entire run of
// sleep, so foreground-group equality reports "at a prompt" throughout. The
// port must answer undetermined for every one of those readings.
//
// A WRONG IMPLEMENTATION reports at-prompt and every `-e` pane in magmux
// becomes a permanently idle shell.
func TestANonInteractiveShellRunningACommandReportsUndetermined(t *testing.T) {
	sh := shellPath(t)
	m := newTestMux(t, ctrlPanes(1)...)
	p := paneRunning(t, m, 0, sh, "-l", "-c", "sleep 2")
	l := &muxPaneLocator{m: m} // NO seam: the real ioctl

	equal, looks := 0, 0
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		fg, leader, ok := foregroundGroup(p)
		if ok {
			looks++
			if fg == leader {
				equal++
			}
		}
		if got := shellOf(t, l, 0).AtPrompt; got == model.LivenessYes {
			t.Fatalf("look %d reported at-prompt from a shell that has never been seen to "+
				"hand its terminal to anything (fg=%d leader=%d)", looks, fg, leader)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if looks < 10 {
		t.Fatalf("only %d readings were taken; the child was gone before the measurement "+
			"could be made", looks)
	}
	if equal != looks {
		t.Fatalf("the foreground group differed from the session leader in %d of %d readings; "+
			"this shell has job control and the measurement this test reproduces does not apply",
			looks-equal, looks)
	}
	if p.shell.proven {
		t.Fatal("the latch proved itself against a shell with no job control")
	}
}

// TestAnInteractiveShellProvesJobControlAndThenMeansSomething is the other
// half of the same measurement, and the positive control for the one above: a
// shell that DOES hand its terminal over is watched doing it, and its
// foreground equality then means what it looks like it means.
//
// Without this, "always undetermined" would pass every test in this file.
func TestAnInteractiveShellProvesJobControlAndThenMeansSomething(t *testing.T) {
	sh := shellPath(t)
	args := []string{"-f", "-i"}
	if strings.HasSuffix(sh, "bash") {
		args = []string{"--norc", "--noprofile", "-i"}
	}
	m := newTestMux(t, ctrlPanes(1)...)
	p := paneRunning(t, m, 0, sh, args...)
	l := &muxPaneLocator{m: m} // NO seam: the real ioctl

	// Let the shell reach its prompt, then give it something that takes the
	// terminal away from it.
	time.Sleep(400 * time.Millisecond)
	if got := shellOf(t, l, 0).AtPrompt; got == model.LivenessYes {
		t.Fatalf("answered at-prompt %v before any flip had been seen", got)
	}
	if _, err := p.ptmx.WriteString("sleep 1\n"); err != nil {
		t.Fatalf("write to the pty: %v", err)
	}

	sawForeign, sawPrompt := false, false
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && !sawPrompt {
		switch got := shellOf(t, l, 0).AtPrompt; got {
		case model.LivenessNo:
			sawForeign = true
		case model.LivenessYes:
			if !sawForeign {
				t.Fatal("answered at-prompt without ever having seen the terminal leave the shell")
			}
			sawPrompt = true
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !sawForeign {
		t.Fatal("the command never took the terminal; this shell was not measured running one")
	}
	if !sawPrompt {
		t.Fatal("the shell took its terminal back and the latch never answered at-prompt")
	}
	if !p.shell.proven {
		t.Fatal("the flip was answered from but not recorded")
	}
}
