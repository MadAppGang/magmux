package mux

// The magmux side of the observation layer's pane port, and the pipeline's
// lifecycle.
//
// internal/obs may not import mux — TestImportDirection forbids it — so the
// pane model reaches the observation layer through an interface obs declares
// and this file implements. That is the whole of the coupling: obs knows
// "something can tell me which pane owns a pid", and magmux knows what a pane
// is. Neither knows the other's vocabulary.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/claudecode"
	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/registry"
	"github.com/MadAppGang/magmux/proc"
)

// maxAncestryHops bounds the walk up the process tree.
//
// magmux spawns a shell, the shell spawns the tool, the tool spawns a
// subagent: three. Sixteen is generous by an order of magnitude and is a
// BOUND, not a budget — its job is that a cycle, a reparented orphan or an
// unusually deep tree cannot hang the caller. Exceeding it is "we could not
// tell", never "no": a walk that ran out of hops established nothing.
const maxAncestryHops = 16

// muxPaneLocator answers [obs.PaneLocator] against magmux's pane table.
//
// One of these is built per pipeline and handed to the pane-gate middleware,
// which calls it from an actor goroutine. It holds no state of its own beyond
// the seams below, so it is safe to share between actors — unlike the
// middleware itself, which remembers its session's pane and may never be
// shared.
type muxPaneLocator struct {
	m *Magmux
	// ppid is the parent lookup, indirected so a test can stage a process
	// tree — and a slow one — without a live process anywhere.
	//
	// It is a FIELD rather than a package-level seam like the ppidLookup mcp
	// already indirects for the same purpose: a test that reassigns a global
	// changes the answer for every other test in the binary, and these two
	// walks are asked at different times by different goroutines. nil takes
	// proc.PPIDOf.
	ppid func(int) (int, error)
	// hops overrides maxAncestryHops. Non-positive takes it.
	hops int
	// foreground is the tcgetpgrp seam, indirected for the same reason ppid
	// is and with more force: the real one is an ioctl on a pty master, so a
	// test that could not replace it would have to spawn a shell and race it
	// to exercise a latch whose whole subject is the ORDER two readings
	// arrive in. It answers both numbers — foreground group and session
	// leader — because their comparison is the entire signal. nil takes
	// foregroundGroup; see obsshell.go, where the latch it feeds lives.
	foreground func(*Pane) (fg, leader int, ok bool)
}

func newPaneLocator(m *Magmux) *muxPaneLocator { return &muxPaneLocator{m: m} }

// PaneFor walks UP the process tree from pid until it reaches a pid that is a
// pane's.
//
// # Why a walk and not a comparison
//
// magmux spawns a SHELL and the shell spawns the tool, so the pane's
// cmd.Process.Pid is the shell's and the session record's pid is the tool's.
// They are never equal, and a direct comparison matches nothing at all — which
// is a failure that looks exactly like "no sessions are running in magmux".
// This is the same question the MCP server's self-pane guard asks in the other
// direction, and it is answered the same way.
//
// # The pane pids are snapshotted under treeMu.RLock and the walk happens
// after the release
//
// proc.PPIDOf is a sysctl on Darwin and a /proc read on Linux. CLAUDE.md's first
// lock rule — never hold treeMu across blocking I/O — has no exception, and
// controller.Poll was documented as one and was the bug. So: resolve under the
// lock, release, then do the syscalls. A pane table that changes during the
// walk costs at worst one stale answer on one cycle, and the next cycle
// re-asks; holding the lock instead would put every keystroke, SIGWINCH and
// socket verb behind a process-tree walk.
func (l *muxPaneLocator) PaneFor(pid int) (int, obs.Liveness) {
	if pid <= 0 {
		// Not a pid. Nothing was established, so nothing is claimed.
		return 0, obs.LivenessUnknown
	}

	byPID := l.panePIDs()
	if len(byPID) == 0 {
		// No pane in this magmux has a process, so no pane owns this one.
		// Determined, and worth answering without a single syscall: it is the
		// answer for every session on a machine running some other magmux.
		return 0, obs.LivenessNo
	}

	return walkToPane(pid, byPID, l.ppid, l.hops)
}

// walkToPane is the ancestry walk itself: from pid, up the process tree, until
// it meets a pid that is a pane's.
//
// # It is a free function because there are now TWO pane tables
//
// magmux's own (muxPaneLocator, above) and one assembled over the socket from
// every reachable magmux's `list` reply (sockPaneLocator, in obssock.go). The
// walk is identical for both — the hard part was never the table, it was the
// rule that a session's pid is the TOOL's and a pane's is the SHELL's — and a
// second copy of it would drift the first time one of them learned something
// the other did not.
//
// lookup nil takes proc.PPIDOf; hops non-positive takes maxAncestryHops.
//
// # It returns whatever the table maps a pane pid TO
//
// Generic over the value since fix round 1, and that is the fix rather than a
// convenience. magmux's own table maps a pane pid to a pane id, which is all
// one magmux needs. The socket table must map it to a (socket, id) pair,
// because a pane id is unique within ONE magmux and not across several — and
// the walk is the only thing that knows which magmux it reached. Flattening
// that to a bare id threw the answer away one step after finding it, and
// `magmux agents focus` then refused most sessions on any machine with two
// magmuxes as ambiguous. A miss returns the zero value, which is meaningless
// unless the Liveness is LivenessYes, exactly as before.
func walkToPane[V any](pid int, byPID map[int]V, lookup func(int) (int, error), hops int) (V, obs.Liveness) {
	var none V
	if lookup == nil {
		lookup = proc.PPIDOf
	}
	if hops <= 0 {
		hops = maxAncestryHops
	}

	seen := make(map[int]bool, hops)
	cur := pid
	for i := 0; i < hops; i++ {
		if id, ok := byPID[cur]; ok {
			return id, obs.LivenessYes
		}
		if cur <= 1 {
			// Walked all the way to init without meeting one of ours. A plain
			// miss, and a determined one: this session is not magmux's.
			return none, obs.LivenessNo
		}
		parent, err := lookup(cur)
		if err != nil {
			// "The chain ends here" is what proc.PPIDOf's own doc comment says an
			// error means. Everything above this link is unreadable, so the
			// pane may be up there and we cannot see it — undetermined, not
			// absent.
			return none, obs.LivenessUnknown
		}
		if parent <= 0 {
			// The top of the tree, reached honestly.
			return none, obs.LivenessNo
		}
		if seen[parent] {
			// A cycle is impossible in a process tree and therefore means a
			// bad read. Refusing to spin is the point of the bound; claiming
			// "no" on the strength of a bad read is not.
			return none, obs.LivenessUnknown
		}
		seen[parent] = true
		cur = parent
	}
	return none, obs.LivenessUnknown
}

// PaneAlive reports whether the pane still EXISTS in magmux, which is a
// different question from whether anything is running in it.
//
// # Which of magmux's three pane states this gate means
//
// CLAUDE.md is explicit that `dead`, `closed` and `hidden` are three states and
// not degrees of one, and only ONE of them means the pane is gone:
//
//   - closed — the pane was detached by close_pane and its id slot is a
//     permanent tombstone. LivenessNo. Nothing will ever run in it again and
//     no id will ever name it again.
//   - dead — the process exited but the pane is still on screen with its
//     ✓ DONE / ✗ FAIL overlay. LivenessYes. The process being gone is the
//     OTHER gate, [obs.Session.ProcessAlive], and the two are separate fields
//     precisely because they fail separately: "the agent finished but its pane
//     is still there" and "the pane was closed out from under it" are
//     different situations with different remedies, and answering No here
//     would collapse them at the wire, where it is least recoverable.
//   - hidden — the pane is ALIVE AND COMPLETE (every byte of its history, its
//     id, its entry in results) and merely not spliced into the layout tree.
//     LivenessYes. Hidden is a fact about magmux's chrome, not about the
//     session; the control panel is hidden by default and answering No for it
//     would make Ctrl-G p look like a session dying.
//
// An id past the end of the append-only table was never allocated, which is a
// determined no. A NEGATIVE id is not an address at all, so it is answered
// undetermined rather than no: "there is no such pane" would claim we looked.
//
// The resolution goes through paneByIDLocked because that and livePanesLocked
// are the only legal ways to turn an int into a *Pane — Pane.id IS the index
// into an append-only slot table, and a raw subscript reaches a detached pane
// with no error, no repaint and no log.
func (l *muxPaneLocator) PaneAlive(paneID int) obs.Liveness {
	if paneID < 0 {
		return obs.LivenessUnknown
	}
	l.m.treeMu.RLock()
	defer l.m.treeMu.RUnlock()
	if l.m.paneByIDLocked(paneID) == nil {
		return obs.LivenessNo
	}
	return obs.LivenessYes
}

// Activity answers [obs.PaneActivity]: when the pane last produced output, and
// when something was last pushed into it. It is the whole of the inferred
// tier's input.
//
// # It reads the two stamps phase 1 put on Pane, and nothing else
//
// lastOutputAt is set in ONE place, readLoop, where bytes actually arrive from
// a child. lastInputAt is set in ONE place, clearCompletionLocked, which is
// already the single function a human keystroke and a controller's injected
// instruction both go through. Neither is a general "this pane changed" stamp,
// and that is the point: p.dirty is that, and about twenty of its ~20 setters
// are magmux repainting its own chrome, so a staleness measured from p.dirty
// could never observe a five-minute silence because something repainted three
// seconds ago.
//
// # Locks
//
// treeMu.RLock to resolve the id, and Pane.mu inside it — the documented order
// (treeMu -> p.mu) — for the two fields. No I/O happens under either, which is
// what makes holding both legal here where PaneFor's process walk has to
// release first. Pane.activity takes p.mu itself, which is why this cannot
// hold it.
//
// # A closed pane answers false, not zero
//
// Zero instants are a REAL answer — a pane that has printed nothing — so a
// vanished pane must not report them. false is "there is no pane to ask
// about", and the inferred tier then has no opinion at all rather than the
// opinion that this pane has been silent since the epoch.
func (l *muxPaneLocator) Activity(paneID int) (lastOutput, lastInput time.Time, ok bool) {
	if paneID < 0 {
		// Not an address. Nothing was established, so nothing is claimed.
		return time.Time{}, time.Time{}, false
	}
	l.m.treeMu.RLock()
	defer l.m.treeMu.RUnlock()
	p := l.m.paneByIDLocked(paneID)
	if p == nil {
		return time.Time{}, time.Time{}, false
	}
	out, in := p.activity()
	return out, in, true
}

// Focus answers [obs.PaneFocuser]: it moves keyboard focus to a pane and
// reports which pane held it before.
//
// # It is the ONLY thing in magmux the observation layer can change
//
// Everything else the layer does is a read. This one method is the whole of
// R5.1, and the three other action tiers requirements.md names — answering a
// permission prompt, sending input, restarting a session — are refused by
// internal/obs/act before anything in this file is reached. Focus touches the
// view: no process is signalled, no byte reaches a child, and the worst
// outcome is a human looking at the wrong pane.
//
// # It refuses exactly what sockFocus refuses, and for the same reasons
//
// A pane that is not in the table (never allocated, or a closed tombstone),
// and a HIDDEN pane. Hidden is the one worth stating: a hidden pane is alive
// and complete and merely not spliced into the tree, so focusing it would send
// every later keystroke somewhere nobody can see — which is the state
// hidePanelLocked repairs and focusNext skips, and which the socket verb
// already closed the last door into. Two doors now, one rule, said in both
// places rather than cross-referenced, because a reader of either needs to
// know.
//
// It is NOT routed through sockFocus itself: that function speaks the socket's
// vocabulary — it parses a wire value, builds a JSON map and returns a socket
// error code — and reusing it would make the observation layer a socket client
// inside its own process. What is shared is the rule, not the plumbing, and
// TestObsFocusRefusesWhatSockFocusRefuses pins the two against each other.
//
// # Locks
//
// treeMu.Lock for the resolution and the pointer move, released before
// setFocused (which takes cp.mu, a legal order, but there is no reason to hold
// both) and before the pane's own dirty flag. No I/O happens under any lock —
// CLAUDE.md's first rule has no exception — and this is called from an actor
// goroutine, which is why it must not block.
func (l *muxPaneLocator) Focus(paneID int) (int, error) {
	if paneID < 0 {
		// Not an address. Refused rather than reported as a missing pane,
		// because "there is no pane -1" would claim we looked.
		return -1, fmt.Errorf("%d is not a pane id", paneID)
	}
	m := l.m
	m.treeMu.Lock()
	p := m.paneByIDLocked(paneID)
	if p == nil {
		m.treeMu.Unlock()
		return -1, fmt.Errorf("no pane %d (it may have been closed)", paneID)
	}
	// `hidden` is a structural fact about the tree, so it is treeMu's and is
	// read here rather than before the lock.
	if p.hidden {
		hint := ""
		if p.isControl {
			hint = " It is magmux's own control panel, hidden by default; Ctrl-G p reveals it."
		}
		m.treeMu.Unlock()
		return -1, fmt.Errorf("pane %d is not on screen, so focusing it would send every "+
			"keystroke somewhere nobody can see.%s", paneID, hint)
	}
	previous := -1
	if m.focused != nil {
		previous = m.focused.id
	}
	m.focused = p
	m.treeMu.Unlock()

	m.control.setFocused(paneID)
	p.mu.Lock()
	p.dirty = true
	p.mu.Unlock()
	return previous, nil
}

// LaunchDir answers where the session running as pid was LAUNCHED, which is
// the input Claude Code's transcript layout is derived from.
//
// # This is the value the whole 4% problem is about
//
// Claude Code files a session's transcript under an encoding of the directory
// it was STARTED in. Its own registry record carries the LIVE working
// directory instead, and feeding that to a correct encoder resolves about 96%
// of sessions — while the 4% that miss land in a directory that EXISTS and
// holds another session's transcripts. magmux is exempt from that ceiling for
// exactly one reason: it spawned the session, so it still holds what it passed
// to the child. This method is that value and nothing else.
//
// An empty cmd.Dir means the child inherited magmux's own directory, which is
// therefore the answer. magmux never chdirs, so it is stable for the life of
// the process.
//
// # A wrong answer here costs a search, never a wrong session
//
// One case is knowingly imperfect: a pane launched as `cd /foo && claude …`
// has an empty cmd.Dir and really starts in /foo, so this returns magmux's
// directory instead. That is safe because a transcript is addressed by SESSION
// ID — a wrong directory can only MISS, and a miss falls through to the scan,
// which finds it. The same is true of a session that moved after startup. What
// would not be safe is a resolver that picked a file by anything other than
// the session id; see adapters.Transcripts.Locate.
//
// Ancestry, not identity: the session's pid is the tool's and the pane's is
// the shell's, so the pane is found by the same walk PaneFor does rather than
// by a comparison that never matches.
func (l *muxPaneLocator) LaunchDir(pid int) (string, bool) {
	id, found := l.PaneFor(pid)
	if found != obs.LivenessYes {
		return "", false
	}

	// Resolved under the lock, used after it. os.Getwd below is a syscall, and
	// CLAUDE.md's first lock rule — never hold treeMu across blocking I/O —
	// has no exception.
	l.m.treeMu.RLock()
	p := l.m.paneByIDLocked(id)
	dir := ""
	if p != nil {
		p.mu.Lock()
		if p.cmd != nil {
			dir = p.cmd.Dir
		}
		p.mu.Unlock()
	}
	l.m.treeMu.RUnlock()

	if p == nil {
		return "", false
	}
	if dir != "" {
		return dir, true
	}
	wd, err := os.Getwd()
	if err != nil {
		// Undetermined rather than "", which would be read as a directory.
		return "", false
	}
	return wd, true
}

// panePIDs maps each pane's own process pid to its pane id.
//
// Held locks: treeMu.RLock, and Pane.mu inside it for the process pointer,
// which is the documented order (treeMu -> p.mu). Both are released before
// this returns, and no I/O happens under either.
//
// The control panel has no process and therefore no pid, so it drops out here
// without needing a special case — cmd is nil for it. A HIDDEN pane does not
// drop out and must not: it is alive, and a session running in it is a session
// magmux owns.
//
// # A REAPED pane contributes no pid
//
// Its pane stays — `dead` is not `closed`, and PaneAlive still says yes — but
// its number does not. Once cmd.Wait has returned the kernel may give that pid
// to any new process, and a session descended from the new owner would be
// matched to this finished pane: reported as running in it, weighed against
// its silence, and focused there by `magmux agents focus`. Until fix round 2 it
// was.
//
// The condition is `reaped` and NOT `dead`, for the reason CLAUDE.md keeps the
// two apart. `dead` is set by readLoop when the PTY closes, and the process
// entry outlives it — a zombie still holds its pid, and a child that closed
// its PTY can still be RUNNING, with real descendants. Only `reaped` proves
// the number has been released, and reapChild sets it under this same p.mu,
// together with `dead` and the exit status.
func (l *muxPaneLocator) panePIDs() map[int]int {
	l.m.treeMu.RLock()
	defer l.m.treeMu.RUnlock()

	live := l.m.livePanesLocked(nil)
	byPID := make(map[int]int, len(live))
	for _, p := range live {
		p.mu.Lock()
		pid := 0
		if p.cmd != nil && p.cmd.Process != nil && !p.reaped {
			pid = p.cmd.Process.Pid
		}
		p.mu.Unlock()
		if pid > 0 {
			byPID[pid] = p.id
		}
	}
	return byPID
}

// ── lifecycle ───────────────────────────────────────────────────────────────

// obsEnvVar switches the observation pipeline on.
//
// # Why it is opt-in, which is a deliberate deviation from the phase brief
//
// The pipeline reads Claude Code's session registry under ~/.claude and asks
// kill(pid, 0) about every pid in it. Both are safe and read-only in
// production, and both are forbidden to this repository's TEST SUITE, which
// spawns the real magmux binary in at least six files with the developer's own
// environment: TestPTYRunStillPaintsStdout, the socket end-to-end tests and the
// startup tests would all begin reading a live registry belonging to thirty
// running agents and signal-probing their pids, on every `go test .`.
//
// There is no in-process seam that helps — those tests exec a fresh binary —
// so the switch is the honest fix, and it is an environment variable rather
// than a flag for the same reason MAGMUX_SOCK_DIR and MAGMUX_THEME are: it
// reaches a nested magmux without anybody rewriting a command line. The
// default flips when there is something on screen to show for it and the suite
// has a registry root of its own to point at.
const obsEnvVar = "MAGMUX_AGENTS"

// startObservationIfEnabled starts the pipeline when MAGMUX_AGENTS asked for it.
//
// enabled is Main's ONE read of MAGMUX_AGENTS, the same value it wrote into
// panelPrivate on the struct literal; this function reads no environment
// variable of its own, so the switch and the panel's privacy cannot disagree.
//
// Called from Main() AFTER the layout is built, the panel is attached and the
// read loops are running — so pane ids are final. Starting it earlier would
// observe a pane table that is about to change: the panel takes the last id,
// and a session matched to a pane that then moves is a wrong answer that looks
// like a right one.
//
// It is deliberately NOT started from init() and NOT stopped from restore().
// Both of those return early when headless, and headless is exactly where this
// matters most — a -w run or a controller-driven session has no human reading
// the screen, so an instrument that only works under a tty is an instrument
// that does not work.
//
// Failures are not fatal and not printed to a terminal magmux is painting. A
// missing registry is the normal case on a machine with no Claude Code on it,
// and a terminal multiplexer that refused to start over it would be absurd.
//
// # The oracle is a PARAMETER, and that is what keeps the test suite off live
// pids
//
// It would be shorter to name obsOracle here. It used to, and the consequence
// was that every test reaching this function — or startObservationWith beneath
// it — sent kill(pid, 0) to whatever pid its temporary registry happened to
// name, on the machine running the suite. That is forbidden outright by
// testdata/PROVENANCE.md rule 2, and it was true of four call sites.
//
// Passing it in makes the rule STRUCTURAL rather than a habit: Main() is the
// only caller that names obsOracle, a test physically cannot reach the signal
// probe without naming it too, and TestNoTestCanReachTheSignalOracle fails if
// one ever does. nil is a legitimate argument and means "nobody can answer",
// which registry answers with LivenessUnknown rather than with a probe — see
// registry.Config.Oracle, where phase 2 chose that default precisely so
// forgetting to inject one is honest rather than dangerous.
func (m *Magmux) startObservationIfEnabled(enabled bool, oracle registry.ProcessOracle) {
	if !enabled {
		return
	}
	root, err := defaultRegistryRoot()
	if err == nil {
		// The transcript tree is resolved separately and its absence is not
		// fatal: a machine with a registry and no transcripts yet is a machine
		// with sessions that have not written one, and the harness-derived
		// columns are undetermined rather than the layer being off.
		projects, _ := defaultProjectsRoot()

		// The action policy and the audit trail. Both are resolved HERE, in
		// the one function that is a program entry point, for the same reason
		// the oracle is a parameter: a default that reached the real home
		// directory would put the developer's own audit trail into `go test .`
		// through the back door, and the struct below is what a test points
		// somewhere else.
		//
		policy, perr := loadObsPolicy()
		if perr != nil {
			// SAID OUT LOUD, and this is now reachable. A config the user
			// wrote and mistyped that silently becomes the default is how
			// somebody discovers months later that their rules never applied,
			// which is the failure R6.3 is written against. It goes to stderr
			// for the same reason the "observation is off" line does: under a
			// tty it will be painted over by the next frame, and headless —
			// where nobody is watching — it is the only report there is.
			fmt.Fprintf(os.Stderr, "magmux: %v\n", perr)
		}
		auditPath, aerr := act.DefaultAuditPath()
		if aerr != nil {
			// No home directory, so no trail. Observation still starts; the
			// gate then refuses every action with act.ErrUnaudited, which is
			// the honest consequence rather than a silent unaudited action.
			auditPath = ""
		}
		err = m.startObservationWith(obsConfig{
			Root:      root,
			Projects:  projects,
			Oracle:    oracle,
			Policy:    policy,
			AuditPath: auditPath,
			// The one place in the repository that names the real
			// notification centre. newNotifier returns nil off darwin, and a
			// nil port is recorded honestly rather than passed off as sent.
			Notify: newNotifier(),
		})
	}
	if err != nil && m.headless {
		// Headless only: there is no frame to corrupt, and a run with no human
		// watching is the one that most needs to be told the instrument is
		// off. Under a tty this line would land on the alternate screen and be
		// painted over by the next frame, which is worse than silence because
		// it looks like a glitch.
		fmt.Fprintf(os.Stderr, "magmux: session observation is off: %v\n", err)
	}
}

// loadObsPolicy resolves requirements.md R6's configuration file.
//
// # The whole of R6.3 is act.Load's, and this adds only the loader
//
// An absent or unparseable file yields the DEFAULT posture, never a more
// permissive one, and the error rides back BESIDE the policy rather than
// instead of it — so a caller that ignores the error still gets the safe
// posture, and one that reports it can say why the file it expected is not in
// force. Phase 6 built that and tested it against injected failures; phase 7
// added act.FileLoader underneath, and this function is the only place in
// magmux that names it.
//
// # Why it is a function and not four lines at each entry point
//
// Because a break was watched passing. Making the file loader return a
// half-read config on a parse error was caught by four tests in
// internal/obs/act and by NOTHING in package mux, because both entry points
// that name the loader are unreachable from a test — one installs the signal
// oracle and one reads the real home directory. A seam a test can call is the
// difference between the wiring being asserted and being assumed.
//
// It reads the environment (XDG_CONFIG_HOME, else the home directory) and one
// file, and writes nothing at all: a loader that created a skeleton config on
// first run would be writing to a directory nobody asked it to.
func loadObsPolicy() (act.Policy, error) {
	var loader act.Loader
	if path, err := act.ConfigPath(); err == nil {
		loader = act.FileLoader(path)
	}
	// A nil loader — no home directory and no XDG_CONFIG_HOME — is R6.3's
	// third case and act.Load answers it with the default posture and no
	// error, which is right: there is nowhere a config COULD be, so nothing
	// the user wrote is being ignored.
	return act.Load(loader)
}

// obsConfig is everything the observation pipeline needs that magmux does not
// already hold. A plain value, copied on the way in: nothing mutates it
// afterwards, so "who may change this while a cycle is running" never arises.
//
// Every field is a thing a TEST must be able to point somewhere else. That is
// not incidental — it is the whole reason the struct exists rather than three
// parameters with defaults. A default that reached $HOME would put thirty live
// agents' records into `go test .` through the back door, and a default oracle
// would signal-probe their pids.
type obsConfig struct {
	// Root is the session registry directory. Required.
	Root string

	// Projects is the transcript tree, or "" for none. Empty is not an error:
	// the Claude Code middleware built with an empty root adds nothing at all,
	// and every harness-derived fact stays undetermined — which is exactly
	// what a machine with no transcripts looks like.
	Projects string

	// Oracle answers the process-liveness gate.
	//
	// nil means NOBODY can answer, which yields obs.LivenessUnknown for every
	// session — honest, and what a test that did not inject one should get
	// rather than a probe that signals whatever pids happen to exist on the
	// machine running the suite. The real probe is obsOracle, named by Main()
	// and by nothing else in this file.
	Oracle registry.ProcessOracle

	// Policy is the action policy. THE ZERO VALUE REFUSES EVERYTHING, and a
	// test that wants to exercise a navigate has to say act.Default() out
	// loud. That is the point: every unit test in this repository builds its
	// structs as literals, so a policy that defaulted to permissive here
	// would be permissive in every test that never mentions it.
	Policy act.Policy

	// AuditPath is the file every action and every refusal is recorded in.
	//
	// EMPTY MEANS NO TRAIL, and no trail means no action: the gate refuses
	// with act.ErrUnaudited rather than acting unrecorded. It is spelled this
	// way round for the same reason Oracle is — a default that resolved
	// ${XDG_STATE_HOME:-~/.local/state} would have `go test .` appending to
	// the developer's own audit trail, which is exactly the class of accident
	// testdata/PROVENANCE.md exists to prevent. Main() names the real path;
	// nothing else does.
	AuditPath string

	// Notify delivers a notification to a human.
	//
	// nil MEANS NOTHING IS DELIVERED, and it is the same kind of seam as
	// Oracle and AuditPath: a default that reached the real notification
	// centre would have `go test .` raising banners on the developer's own
	// desktop. Main() names newNotifier(); nothing else does.
	//
	// It does not decide WHETHER to notify — Policy.Notify does, and its zero
	// value is silence. A magmux with a port and no notify configuration says
	// nothing at all.
	Notify notify.Port
}

// startObservation starts the pipeline against a registry root and NOTHING
// else: no transcript tree and no liveness oracle.
//
// Both absences are the reason this form exists, and both are about the test
// suite rather than about production. Nothing in this repository may read the
// real ~/.claude, so a form that quietly defaulted to $HOME/.claude/projects
// would put thirty live agents' transcripts into `go test .` through the back
// door — and nothing in this repository may probe a live pid, so a form that
// quietly defaulted to obsOracle would send kill(pid, 0) to whatever pid a
// temporary registry happened to name. It used to, and that was a real
// violation of testdata/PROVENANCE.md rule 2 rather than a hypothetical one.
//
// With neither, the harness-derived facts and both liveness answers are
// undetermined, which is what a machine with no transcripts and no oracle
// honestly looks like.
func (m *Magmux) startObservation(root string) error {
	return m.startObservationWith(obsConfig{Root: root})
}

// startObservationWith starts the pipeline. This is the production form; see
// startObservation for why the other one exists.
func (m *Magmux) startObservationWith(cfg obsConfig) error {
	if m.obs != nil {
		return fmt.Errorf("the observation pipeline is already running")
	}
	if _, err := os.Stat(cfg.Root); err != nil {
		return fmt.Errorf("no session registry at %s: %w", cfg.Root, err)
	}
	// THIRD, after "already running" and the registry stat, so a caller that
	// expects either of those errors still gets it. A magmux whose panel is
	// public must not observe: the attention view would put every session on
	// the machine into a pane any remote viewer may watch. See panelPrivate.
	if !m.panelPrivate {
		return errPanelPublic
	}

	// The audit trail, resolved before anything starts. NewWriter creates
	// nothing — the directory and the file appear when the first record is
	// written — so this is a path check and a refusal, and the refusal that
	// matters is a path under ~/.claude, which NFR1 forbids without
	// exception.
	//
	// A refused path stops the whole pipeline rather than starting one that
	// cannot act. That is louder than degrading, and it is the right way
	// round: the caller here is startObservationIfEnabled, which treats any
	// error as "the instrument is off" and says so on stderr when headless.
	// magmux itself is unaffected either way.
	var auditor act.Auditor
	if cfg.AuditPath != "" {
		w, err := act.NewWriter(act.WriterConfig{Path: cfg.AuditPath})
		if err != nil {
			return err
		}
		auditor = w
	}

	locator := newPaneLocator(m)
	p, err := pipeline.Start(pipeline.Config{
		Root: cfg.Root,
		// Injected, never named here. A nil oracle leaves both liveness
		// answers undetermined rather than probing, which is the honest
		// answer for a caller that could not or would not supply one.
		Oracle: cfg.Oracle,
		// The pane gate. A nil locator here would be the honest answer for a
		// process with no pane table; magmux is the one process that has one.
		Locator: locator,
		// The Claude Code middleware, after the gate. This is where a session
		// stops being described from the outside: the colour, the API-error
		// count and the subagent count are the tool's own record of itself.
		//
		// The locator serves five times — as the pane port, the
		// launch-directory port, the activity port, the shell port and the
		// focuser — and that is deliberate. It is the only object that knows
		// the pane table, and every one of those questions is answered from
		// it with no state OF ITS OWN, which is what makes it safe for every
		// actor to share. The shell port advances a latch, but that latch
		// lives on the PANE; the locator still remembers nothing.
		//
		// This is the line that turns the inferred tier on. Inside magmux
		// there is a pane to be silent, so the verdict stage has two tiers to
		// weigh and a conflict is possible; `magmux agents`, running outside any
		// magmux, passes nothing here and resolves on the authoritative tier
		// alone.
		Activity: locator,
		// The GENERIC tier's direct evidence: tcgetpgrp behind its latch, and
		// the pane's OSC 133 marks. The same locator again, for the same
		// reason — it is the object that knows the pane table — but with one
		// difference worth naming here rather than only at the method: this
		// port MUTATES. Calling it is what advances each pane's job-control
		// latch, so it is the one line in this config that would silently
		// change the layer's answers by being left out, rather than merely
		// narrowing them.
		Shell: locator,
		// The WRITE port, and the last thing the one locator answers. This
		// is the line that makes R5.1 possible at all inside magmux: a
		// process that passes nothing here can observe every session and
		// focus none of them, which is what `magmux agents` honestly is.
		Focuser: locator,
		// The posture and the trail. Both come from the caller, and the zero
		// value of both refuses — a pipeline built by a test that says
		// nothing about either cannot act, which is the safe direction and
		// the one a struct literal falls into.
		Policy: cfg.Policy,
		Audit:  auditor,
		// The notification port, and the fourth thing that is a PARAMETER
		// rather than a default for the same reason the oracle is: a default
		// that reached the real notification centre would have `go test .`
		// raising banners on the developer's own desktop. Main() names
		// newNotifier(); nothing else does.
		//
		// A nil port does not silence the notifier — cfg.Policy.Notify does,
		// and its zero value is silence. A nil port means nothing here can
		// DELIVER, which is recorded as such rather than passed off as sent.
		Notify: cfg.Notify,
		Chain: []pipeline.Factory{claudecode.Middleware(claudecode.Config{
			Root:       cfg.Projects,
			LaunchDirs: locator,
		})},
		Watch: true,
		// Separate from Clock on purpose; see registry.Config.Now.
		Now: time.Now,
	})
	if err != nil {
		return err
	}
	m.obs = p

	// The chrome sink, subscribed BEFORE the refresh below. Nothing is
	// replayed, so a sink that attaches after the first cycle waits for the
	// next record change — which on a quiet machine is a long time to look at
	// an empty panel.
	//
	// A failed subscribe stops the whole pipeline rather than starting one
	// whose document nothing on magmux's side can see. It is only reachable
	// against a hub that has already stopped, which cannot happen here.
	if err := m.startObsSink(p); err != nil {
		p.Stop()
		m.obs = nil
		return err
	}

	// One cycle now, and exactly one: it gives every session an actor and
	// gives the source its first known-good state, so the first record change
	// is a change against something rather than a first sighting. There is no
	// ticker behind it and no second one.
	//
	// It is NOT a document for later subscribers — nothing is replayed, and a
	// sink that attaches after this has to ask for its own [Pipeline.Refresh].
	// Subscribe first, then refresh, is the order; see consumeObs.
	p.Refresh()
	return nil
}

// stopObservation shuts the pipeline down and waits for it.
//
// On the quit path in Main(), not in restore(): restore() is terminal
// restoration and returns early when headless, so stopping there would leak
// every goroutine in a headless run. Pipeline.Stop is the shutdown barrier —
// it returns only once the watcher, every actor and the hub have finished and
// every subscription has been closed, so no sink can be mid-write when the
// process exits. CLAUDE.md records the matching failure on the socket side: a
// snapshot still queued when m.quit closes is simply lost, and that lost
// snapshot was issue #2.
func (m *Magmux) stopObservation() {
	if m.obs == nil {
		return
	}
	m.obs.Stop()
	// AFTER Pipeline.Stop, which closes every subscription — that close is
	// what ends the sink's range, so waiting before it would wait for ever.
	// The wait itself is the barrier extended over the one sink that lives on
	// magmux's side of the hub: it writes into the control panel, and a panel
	// written to after the terminal has been restored is a write nobody can
	// see and a race nobody expected.
	m.stopObsSink()
	m.obs = nil
}

// obsFocusSession focuses the magmux pane a session is running in.
//
// It is magmux's own entry to the write path and the only one: R5.1, through
// the gate, with an audit record either way. There is no second route that
// skips the gate, and the actor commands beneath it are unexported precisely
// so that there cannot be.
//
// # What reaches it, and what reaches the same gate another way
//
// Nothing on a keyboard or a socket calls this function yet; its callers are
// the tests. A chord would be chrome, and nobody has asked for one or reviewed
// one.
//
// `magmux agents focus` reaches the SAME gate by another route, and it CAN focus
// a pane. It runs outside any magmux, so it has no pane table of its own, and
// it builds one over the socket instead — obssock.go reads every reachable
// magmux's connect-time aggregate, which already carries each pane's id and its
// child's pid, walks the session's ancestry to a pane, and sends the existing
// `focus` verb to the one magmux the walk reached. No new verb was needed.
//
// This comment said, until fix round 2, that `magmux agents focus` "holds no pane
// table, so it reports honestly that it cannot focus one", and described the
// socket route as future work. That stopped being true in phase 7; README.md
// and obsfocus.go said so, and this comment was the one place that still did
// not. It now reports only what `magmux agents focus` does with NO magmux running:
// no pane table, no capability, and a recorded refusal.
//
// The returned report carries every audit line written for the request, so a
// caller can show the trail it just made. A refusal is an ERROR: nothing here
// declines to act and returns nil.
func (m *Magmux) obsFocusSession(ctx context.Context, sessionID string, by act.Requester) (act.Report, error) {
	if m.obs == nil {
		return act.Report{}, fmt.Errorf("session observation is not running")
	}
	return m.obs.Do(ctx, act.Request{
		Action: act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate, Session: sessionID},
		By:     by,
	})
}

// obsEnabled reads the switch. Anything that is not an obvious "off" counts as
// on, because MAGMUX_AGENTS=1 and MAGMUX_AGENTS=true are both things people write
// and a variable that silently means "off" for one of them is a variable that
// gets reported as broken.
func obsEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "no", "off", "false":
		return false
	}
	return true
}

// errPanelPublic is startObservationWith's refusal on a magmux whose control
// panel is readable off-box. It is unreachable from Main, which writes
// panelPrivate from the same MAGMUX_AGENTS read that enables observation; it
// is what makes the rule hold for every other caller, struct-literal tests
// included.
var errPanelPublic = errors.New("session observation needs a private control panel: " +
	"this magmux was not started with " + obsEnvVar + "=1, so its panel is readable off-box")

// panelReadableBy reports whether caller may read the CONTROL PANEL's pane —
// watch it, capture it, read its transcript.
//
// Always, unless this magmux observes sessions (panelPrivate); then only a
// caller on the local socket. The socket adapter sets Transport "socket" once,
// in callerFor, before any verb runs. A ZERO Caller fails closed: something
// that never said how it arrived is not on-box.
//
// Every non-socket transport is refused, not only the read-only view token: the
// `agents` document is socket-only, and a full-token remote controller is
// still off-box.
func (m *Magmux) panelReadableBy(c hub.Caller) bool {
	return !m.panelPrivate || c.Transport == "socket"
}

// refusePanelOffBox is the one refusal every pane read of the panel gives an
// off-box caller of an observing magmux. nil when the read may proceed.
func (m *Magmux) refusePanelOffBox(p *Pane, c hub.Caller) error {
	if p == nil || !p.isControl || m.panelReadableBy(c) {
		return nil
	}
	return sockErrf(sockCodePanelPrivate,
		"pane %d is magmux's control panel, and this magmux observes the Claude Code sessions "+
			"on its machine (%s=1), so the panel is readable only over the local socket", p.id, obsEnvVar)
}
