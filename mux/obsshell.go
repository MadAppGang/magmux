package mux

// The magmux side of the observation layer's SHELL port: the two things a pane
// running something nobody wrote an adapter for can still be asked about.
//
// One is a kernel fact — who owns the terminal — and one is the shell
// volunteering a mark about itself. Both live here rather than in pane.go and
// vt.go because neither is part of the terminal core: pane.go holds one field
// and vt.go three lines of parser dispatch, and everything that decides what those mean
// is in this file.
//
// The claudestate library cannot import mux, so this is the same shape every
// other port takes: the library declares the interface (ports.PaneShell), this
// implements it, and the two vocabularies never meet.

import (
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/obsview"
	"golang.org/x/sys/unix"
)

// shellObs is one pane's generic-tier state: what its OSC 133 marks last said,
// and how far its job-control latch has got.
//
// It is a struct in this file rather than four fields on Pane so that pane.go
// carries one line for the whole feature, and so that the latch's two halves
// cannot be read from anywhere that has not read what they mean.
//
// GUARDED BY Pane.mu, like every other content field. The marks are written by
// the VT parser, which runs inside the p.mu readLoop already holds; the latch
// is advanced by the observation layer's actor goroutine through
// [muxPaneLocator.Shell]. Those are different goroutines, which is the whole
// reason it is under a lock at all.
type shellObs struct {
	// mark is what OSC 133 last said. Zero is "this shell emits no marks",
	// which is the common case and is not a negative.
	mark model.ShellMark

	// exit is the status the last 133;D carried, and exitKnown says whether
	// it carried one at all. Two fields rather than a *int deliberately: this
	// is written from the VT parser, which is the hottest path in the
	// program, and a pointer here would be an allocation per finished
	// command. The pointer the model wants is built once per OBSERVATION, in
	// Shell below, where allocating is free.
	exit      int
	exitKnown bool

	// sawForeign is the first half of the latch: a foreground process group
	// other than the session leader has been observed, so something in this
	// pane can take the terminal.
	//
	// proven is the second half, and it is the FLIP — the foreground group
	// came back to the session leader after having left it. Only then does
	// foreground-equality mean "the shell is at a prompt".
	//
	// Both only ever go true. A pane that has proven job control does not
	// un-prove it: the proof is about the SHELL — that it does job control at
	// all — and nothing a shell does later makes that untrue. See
	// model.ShellState.AtPrompt for the measurement that makes the latch
	// necessary.
	sawForeign bool
	proven     bool
}

// noteShellMark records one OSC 133 sequence. body is everything after
// "133;", so "C", "D;1" or "A;aid=7".
//
// # Called from the VT parser, which is the hottest path in the program
//
// It allocates nothing, for ANY payload, with the debug log off — MAGMUX_DEBUG
// unset, dbgFile nil, which is every ordinary run; the Fprintf at the end is
// the one allocation, and only a debugging session pays it. body is a slice of
// the string handleOSC already built, the kind is a slice of that, and the
// exit status is read by parseExitStatus, which scans digits by hand. The work
// is one IndexByte over a string that is almost always one or three bytes
// long, and it happens once per OSC 133 sequence — which is at most twice per
// command a human runs, against the hundreds of thousands of printable
// characters between them.
//
// "For any payload" is new in fix round 1. The status used to go through
// strconv.Atoi, which allocates a *NumError for input it rejects, so a shell
// reporting `133;D;SIGINT` cost two allocations while this comment said none.
// TestANonNumericExitStatusAllocatesNothing pins the non-numeric case, which
// is the one TestOSC133CostsTheParserNothingItWasNotAlreadySpending never
// exercised.
//
// # It does NOT touch inputReady, and that is a decision rather than an
// omission
//
// magmux's own idle detection has exactly TWO sources — the terminal's
// heuristics and a controller's transcript — reconciled in
// applyTerminalIdle by ordering Pane.inputReadyAt against the controller's
// lastApplyAt "so the fresher signal wins and the state cannot oscillate".
// The OSC handlers beside this one (OSC 9, 777;notify, 633;B) are inputs to
// that merge. This is not: it feeds the observation layer through the pull
// port and nothing else, so the merge keeps its two sources and the invariant
// that the live snapshot and the shutdown results can never disagree is
// untouched.
//
// Making 133;A a third writer of inputReady would be a change to magmux's
// existing idle behaviour for every shell pane on the machine, dressed as an
// addition to a new layer. It may well be worth making — a plain shell at a
// prompt IS waiting for input, and that is exactly what inputReady means —
// but it is its own change, with its own evidence, and it would have to say
// what it does to that ordering. It is not smuggled in here.
//
// Caller holds p.mu — readLoop takes it around vt.write.
func (p *Pane) noteShellMark(body string) {
	kind, rest := body, ""
	if i := strings.IndexByte(body, ';'); i >= 0 {
		kind, rest = body[:i], body[i+1:]
	}
	switch kind {
	case "A", "B":
		// Prompt start, and the command line beginning. Both say the shell is
		// at a prompt with nothing running. They are not distinguished
		// because the distinction is about DRAWING, and an observer only
		// wants to know whether work is happening.
		p.shell.mark = model.ShellMarkPrompt
	case "C":
		// Pre-execution. Whatever was typed is about to run.
		p.shell.mark = model.ShellMarkRunning
		// The previous command's status stays exactly where it is. It is the
		// last COMPLETED command's outcome, which is the only thing it could
		// mean while another one runs.
	case "D":
		p.shell.mark = model.ShellMarkPrompt
		// A bare 133;D is legal and common: the command finished and the
		// shell did not say how. That is NOT a zero — see model.ShellState.Exit
		// — so the previous status is cleared rather than replaced with a
		// success nobody reported.
		p.shell.exit, p.shell.exitKnown = 0, false
		if code, ok := parseExitStatus(rest); ok {
			p.shell.exit, p.shell.exitKnown = code, true
		}
	default:
		// Everything else in the 133 family — P (property), E (command line
		// text), and whatever a future shell adds — says nothing about
		// whether work is happening. Ignored rather than guessed at.
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[OSC] 133;%s → mark=%s exit=%d known=%v\n",
			body, p.shell.mark, p.shell.exit, p.shell.exitKnown)
	}
}

// parseExitStatus reads the status off a 133;D payload: the first
// semicolon-separated field, which may be absent, empty, or followed by
// key=value parameters this build does not read.
//
// A non-numeric status is NOT a status. Some shells report a signal name
// there; reporting that as an exit code would invent a number, and the model
// already spells "nobody said" as nil.
//
// # Why this does not call strconv.Atoi
//
// It accepts exactly what strconv.Atoi accepts — an optional sign, then one or
// more ASCII digits, within the range of an int — and it is held to that by
// TestParseExitStatusAgreesWithAtoi. What it does not do is allocate on the
// way to "no": Atoi returns a *NumError for everything it rejects, and the
// path this is on is documented, and tested, as allocation-free.
func parseExitStatus(rest string) (int, bool) {
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		rest = rest[:i]
	}
	neg := false
	if rest != "" && (rest[0] == '+' || rest[0] == '-') {
		neg = rest[0] == '-'
		rest = rest[1:]
	}
	if rest == "" {
		return 0, false
	}
	// The magnitude is accumulated unsigned against the largest one an int can
	// carry, which for a negative number is one more than for a positive one.
	limit := uint64(math.MaxInt)
	if neg {
		limit++
	}
	var n uint64
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if n > (limit-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	if neg {
		// -int(n) is also right for n == limit: int(n) wraps to math.MinInt,
		// whose negation is itself.
		return -int(n), true
	}
	return int(n), true
}

// foregroundGroup reports a pane's foreground process group and its session
// leader — the two numbers whose comparison is the whole tcgetpgrp signal.
//
// It is the DEFAULT for muxPaneLocator.foreground and is replaced wholesale by
// a test, which is why the seam takes the pane and returns both numbers: a
// test that had to stage a real pty to exercise the latch would be testing the
// kernel, and one that reassigned a package-level variable would change the
// answer for every other test in the binary. Same reasoning, and the same
// shape, as muxPaneLocator.ppid.
//
// ok is false for anything that leaves the comparison meaningless: a pane with
// no PTY (the control panel has none), a pane whose child was never started, a
// master that has been closed, and the state a pty reports after its session
// leader has exited — measured on Darwin 27, where TIOCGPGRP then answers 0.
//
// Caller must NOT hold p.mu or treeMu: this is a syscall, and magmux's rule is
// that the pane pointer is resolved under the lock and the lock released
// before anything reaches the kernel. TIOCGPGRP does not block, but the rule
// has no exceptions and the one that was documented as an exception was the
// bug.
func foregroundGroup(p *Pane) (fg, leader int, ok bool) {
	p.mu.Lock()
	ptmx := p.ptmx
	p.mu.Unlock()
	if ptmx == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0, 0, false
	}
	leader = p.cmd.Process.Pid
	if leader <= 0 {
		return 0, 0, false
	}
	fg, err := tcgetpgrp(ptmx)
	if err != nil || fg <= 0 {
		return 0, 0, false
	}
	return fg, leader, true
}

// tcgetpgrp is the ioctl, through SyscallConn rather than Fd.
//
// Fd() would work — magmux already calls it on this same file for TIOCSWINSZ —
// but SyscallConn holds a reference for the duration of the call, so a pane
// closing underneath the observation layer's goroutine is an error rather than
// an ioctl against a descriptor number the process has since reused for
// something else. The window is small and the consequence of losing it would
// be a foreground group belonging to a different terminal entirely.
func tcgetpgrp(f *os.File) (int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pgrp int
	var ioctlErr error
	if err := rc.Control(func(fd uintptr) {
		pgrp, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}); err != nil {
		return 0, err
	}
	return pgrp, ioctlErr
}

// advance moves the job-control latch on one observation and reports what
// foreground-group equality is worth.
//
// It is the whole of architecture v4 §0.2's fix, and it is a pure function of
// (fg, leader) and the latch's own two bits:
//
//	fg != leader  -> NO, determined, and the latch's first half is set. Something
//	                 other than the shell holds the terminal. This needs no proof:
//	                 a foreign foreground group is unambiguous.
//	fg == leader  -> the shell is the foreground group, which is what an
//	                 interactive shell at a prompt looks like AND what a
//	                 non-interactive `zsh -l -c 'make'` looks like for the entire
//	                 run of make. Worth YES only once this pane has been seen to
//	                 flip — leave the leader and come back. Until then, UNKNOWN.
//
// A pane that never flips reports unknown forever, and that is correct rather
// than a gap to be closed with a timeout: nothing about waiting longer turns
// an ambiguous reading into an unambiguous one.
//
// Caller holds p.mu.
func (sh *shellObs) advance(fg, leader int) model.Liveness {
	if fg != leader {
		sh.sawForeign = true
		return model.LivenessNo
	}
	if sh.sawForeign {
		// THE FLIP. It left the session leader and came back, so this shell
		// does job control and equality means something from here on.
		sh.proven = true
	}
	if !sh.proven {
		return model.LivenessUnknown
	}
	return model.LivenessYes
}

// Shell answers ports.PaneShell: the terminal facts for one pane.
//
// # It is the only port here that MUTATES, and the mutation is the latch
//
// Every other pane port reads. This one advances shellObs.sawForeign and
// shellObs.proven, because a latch that proves itself from a flip can only see
// the flip if somebody looks. That state belongs to the PANE and not to the
// locator, which is what keeps the locator shareable between actor goroutines
// — it still holds nothing of its own — and it is why the mutation is under
// p.mu.
//
// # Locks
//
// treeMu.RLock to resolve the id, RELEASED before the syscall; then p.mu for
// the latch and the marks. That is the documented order (treeMu -> p.mu) and
// it never holds both across the kernel call. Unlike Activity, which reads two
// stamps and can do it under both locks, this one asks the kernel a question,
// and "never hold treeMu across I/O" is the rule with no exceptions.
//
// # A pane with no terminal answers false
//
// The control panel has no PTY and never will, and a closed pane's master is
// closed. Neither is "nothing is running in there" — it is "there is nothing
// to ask", and the model spells those differently.
func (l *muxPaneLocator) Shell(ref model.PaneRef) (model.ShellState, bool) {
	paneID, ok := obsview.PaneID(ref)
	if !ok {
		return model.ShellState{}, false
	}
	l.m.treeMu.RLock()
	p := l.m.paneByIDLocked(paneID)
	l.m.treeMu.RUnlock()
	if p == nil {
		return model.ShellState{}, false
	}

	read := l.foreground
	if read == nil {
		read = foregroundGroup
	}
	fg, leader, ok := read(p)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ptmx == nil {
		// No terminal to have an opinion about. Said as "no pane to ask"
		// rather than as an undetermined ShellState, because the marks would
		// be undetermined too and a caller reading a struct full of zeroes
		// cannot tell that from a shell that simply says nothing.
		return model.ShellState{}, false
	}

	st := model.ShellState{Mark: p.shell.mark}
	if ok {
		st.AtPrompt = p.shell.advance(fg, leader)
	}
	if p.shell.exitKnown {
		// A FRESH pointer per call. The model's *int is "a status was
		// reported"; handing out a pointer into the pane would let a consumer
		// read a value that changes underneath it, and would put the pane's
		// lock in the caller's hands.
		exit := p.shell.exit
		st.Exit = &exit
	}
	return st, true
}
