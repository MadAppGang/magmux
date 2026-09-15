// magmux — Minimal Go Terminal Multiplexer
// Port of MTM (Rob King) from C to Go, zero third-party dependencies.
// Uses only golang.org/x/sys and golang.org/x/term.
package mux

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/theme"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	scrollbackLines = 1000
	maxParams       = 16
	maxOSC          = 256
	commandKey      = 'g' // Ctrl-G prefix
)

// ── Debug logging ─────────────────────────────────────────────────────────────
var dbgFile *os.File

// ── Multiplexer ───────────────────────────────────────────────────────────────

type Magmux struct {
	// treeMu guards the LAYOUT: m.root, m.allPanes (its header, its elements,
	// and each element's id/closed/label), m.focused, m.statusText, m.rows,
	// m.cols, m.closeAt, the package-level `sel`, and the structural fields of
	// every Pane (splitType, y, x, h, w, ratio, child1, child2, parent).
	// Content fields (screen, dirty, dead, tint, inputReady, …) stay under
	// Pane.mu exactly as before.
	//
	// Lock order:
	//
	//	treeMu -> p.mu -> sockClientsMu
	//	treeMu -> cp.mu
	//	treeMu -> claimedMu
	//
	// Three rules:
	//
	//  1. Never hold treeMu across blocking I/O — ptmx.Write, conn.Write,
	//     cmd.Start, os.Stdout.Write, controller.Poll, exec of pbcopy. Resolve
	//     the pointer under RLock, release, then do the I/O. There is no
	//     exception: render() paints into a buffer and writes it after the
	//     unlock, and pollControllers snapshots the pane list under RLock and
	//     polls with the lock released.
	//  2. sync.RWMutex is NOT reentrant. A second RLock on one goroutine
	//     deadlocks if a writer is queued between them, and the failure mode is
	//     a silent hang rather than a race report. Every function reachable
	//     from a site that already holds treeMu has a …Locked twin —
	//     allPanesDoneLocked above all, because renderLocked holds RLock
	//     throughout.
	//  3. Never acquire treeMu while holding p.mu, cp.mu, sockClientsMu or
	//     claimedMu.
	//
	// One deliberate exception to the usual reader/writer split: render()
	// WRITES m.statusText while holding only RLock. That is safe because
	// render is the single render-loop goroutine and every other writer of
	// statusText (the `status` verb, updateAgentStatusBar) takes the full
	// Lock, which excludes it. Anything else that wants to write under RLock
	// has to make the same argument or it is a race.
	treeMu sync.RWMutex
	// closing is set by cleanup() under treeMu.Lock BEFORE it calls wg.Wait().
	// OpenPane checks it under the same lock, because an open_pane arriving
	// during teardown would otherwise panic with "WaitGroup misuse: Add called
	// concurrently with Wait".
	closing    bool
	root       *Pane
	focused    *Pane
	allPanes   []*Pane // leaf panes only; append-only, index == Pane.id
	rows, cols int
	statusText string
	renderer   Renderer
	// out is where a painted frame goes. Nil means os.Stdout, which is what
	// every real run uses; it exists so a test can substitute a writer it
	// controls — both to keep thousands of ANSI frames out of the test log and
	// to PROVE the frame is written with treeMu released (a writer that blocks
	// for 200ms must not delay a treeMu.Lock by anything like 200ms).
	//
	// Written once before renderLoop starts and never again, so it needs no
	// lock; treat it as immutable from the moment the render loop exists.
	out io.Writer
	// headless suppresses every interaction with the terminal: no raw mode, no
	// alternate screen, no OSC 11 probe, no frame written anywhere. What it does
	// NOT suppress is the render loop — renderLocked is also where -w decides to
	// quit and where the snapshot events are collected, so only the WRITE half
	// (writeTerm) is skipped.
	//
	// It is stated as a POSITIVE that is FALSE by default for the same reason
	// hideStatus/panelFirst are stated as negatives: every unit test builds a
	// Magmux as a struct literal, so the zero value must be today's behaviour,
	// and today's behaviour is interactive. An `interactive bool` would invert
	// that and silently make every existing literal headless.
	//
	// Written in init() and nowhere else — including the auto-degrade — and read
	// everywhere after. No lock, deliberately, on the same argument as
	// pendingInput and themeAskedAt: init() returns before any goroutine exists.
	headless bool
	rawState *term.State
	quit     chan struct{}
	quitOnce sync.Once
	wg       sync.WaitGroup
	gridMode bool // -g flag was used
	autoExit bool // -w flag: quit automatically when all panes done
	// noIdleDone is --no-idle-done: manual mode, for a human driving a
	// long-lived agent rather than watching a batch finish.
	//
	// It changes what magmux DOES with idle, never whether idle is detected or
	// reported. inputReady is still set, so `snapshot`, `results` and the
	// controller state a pilot reads are untouched — the invariant that the
	// live snapshot and the shutdown results must never disagree does not bend
	// for a display flag. What it suppresses is magmux CLAIMING the session
	// finished: no ✓ DONE overlay, no green tint, the counter keeps saying
	// running, and -w waits for the process to exit instead of for a turn to
	// end. Input is not in that list, and never should be again — a keystroke
	// reaches an idle pane with or without this flag (issue #333).
	noIdleDone bool
	sockPath   string // /tmp/magmux-{pid}.sock, or {name} with --id
	sockID     string // --id NAME: bind a named socket instead of the pid one
	// sockDir overrides where this magmux binds (--sock-dir / MAGMUX_SOCK_DIR).
	// Empty means the package default in sockdir.go, i.e. /tmp — so every
	// existing struct literal keeps binding exactly where it always did. Same
	// empty-means-default shape as m.out and m.stdin, neither of which is ever
	// assigned in production either.
	//
	// Written once, in main(), before any goroutine exists.
	sockDir       string
	sockClients   []net.Conn // currently-connected socket subscribers (for push events)
	sockClientsMu sync.Mutex
	// finalEvents holds the marshaled shutdown payloads (results, then
	// shutdown) once teardown begins. Guarded by sockClientsMu. Non-empty
	// means "shutdown has started": a client connecting from that moment on
	// is never registered for broadcasts, and is instead replayed these
	// events directly before being closed. That makes "every subscriber
	// receives results before EOF" hold no matter when it connects — see
	// handleSocketConn and recordFinalEvents.
	finalEvents [][]byte
	// sockDone is closed once the socket server has finished its teardown —
	// final results/shutdown broadcast, subscribers flushed and closed,
	// listener removed. main waits on it before exiting, because that
	// teardown is what delivers `results`; exiting first drops it entirely.
	sockDone     chan struct{}
	sockDoneOnce sync.Once
	// layoutReady is closed once the layout EXISTS: buildGrid/buildLayout have
	// run, read loops are running and controllers are attached. The socket
	// binds long before that (MAGMUX_SOCK has to be in the environment of the
	// very first child), so without this a client that connects inside that
	// window is served against an empty m.allPanes — and the first thing
	// handleSocketConn writes is the connect-time aggregate snapshot, which
	// every subscriber seeds its entire pane map from precisely because magmux
	// only pushes per-pane snapshots on CHANGE. An empty aggregate therefore
	// does not merely arrive early, it strands that client forever waiting for
	// state it will never be told again. Verbs served in the window were just
	// as wrong, and reported it as `no_such_pane`, which reads as "your index
	// is bad" rather than "there is no layout yet".
	//
	// Nil means "no wait": the in-process tests build their layout
	// synchronously before anything can look at it.
	layoutReady     chan struct{}
	layoutReadyOnce sync.Once
	lastDoneCount   int       // track status bar updates to avoid redundant rewrites
	startedAt       time.Time // when magmux started (for status bar timer)
	completedAt     time.Time // when all panes reached "done" (freezes timer)
	lastTimerTick   int       // elapsed seconds at last forced status redraw
	// Interactive tool controllers
	controllerFactories []ControllerFactory
	// pollMu serialises pollControllers and guards lastControllerPoll. It is
	// its OWN lock rather than treeMu because the poll must not hold treeMu:
	// ClaudeCodeController.Poll walks ~/.claude/projects on every tick until it
	// finds a transcript. pollControllers takes treeMu.RLock inside pollMu to
	// snapshot the pane list, so nothing may ever take pollMu while holding
	// treeMu.
	pollMu             sync.Mutex
	lastControllerPoll time.Time
	ctx                context.Context
	// claimedSessions maps controller-managed session file paths to the
	// pane that owns them. Used so sibling controllers don't both pick the
	// same JSONL file when running in the same project directory.
	claimedSessions map[string]*Pane
	claimedMu       sync.Mutex
	// control is the controlled-session panel. Always non-nil so the record*
	// methods are safe to call unconditionally; it only paints if a control
	// pane was built for it (magmux -c).
	control *ControlPanel
	// hub is the op registry and the event bus (package hub). Reached ONLY
	// through m.bus(), which builds it and registers the built-in ops on first
	// use: every unit test in this package constructs a Magmux as a struct
	// literal and never calls init(), so the zero value has to work.
	//
	// It is a leaf in the lock order (treeMu -> p.mu -> hub.mu -> sub.mu) and
	// nothing of magmux's is held while calling into it.
	hub     *hub.Hub
	hubOnce sync.Once
	// autoCloseAfter is how long to wait after a pilot declares the run over
	// before quitting (-x). Zero means wait for an explicit keypress, which
	// is the default: a finished run that vanishes before it is read is
	// worse than one that lingers.
	autoCloseAfter time.Duration
	closeAt        time.Time // when the armed countdown fires; zero if not armed
	// themePref is --theme ("", "auto", "light" or "dark"). Only light and
	// dark are answers; "" and "auto" are no opinion and fall through, in
	// order, to MAGMUX_THEME, TERM_THEME, the OSC 11 probe, COLORFGBG and then
	// dark. (Earlier, `--theme auto` beat a set MAGMUX_THEME and probed; it no
	// longer does — unset the variable instead.) See theme.go.
	themePref string
	// pendingInput is input that arrived DURING startup and has not been
	// handled yet — bytes the OSC 11 theme probe read off stdin that were not
	// part of the terminal's reply.
	//
	// The probe is the only reader of stdin that runs before inputLoop's own
	// goroutine, and it cannot tell the terminal to answer without also
	// draining whatever the user typed in the meantime. Dropping those bytes
	// would mean magmux eats keystrokes at startup — including the ones the
	// PTY-driven tests send — so they are parked here and replayed into the
	// input loop ahead of everything read later, in order.
	//
	// Written in init(), read once by inputLoop, and the two cannot overlap:
	// init() returns before any goroutine exists. No lock, deliberately.
	pendingInput []byte
	// stdin is where keystrokes come from. Nil means os.Stdin, which is every
	// real run; a test points it at a pipe so it can drive inputLoop without
	// swapping a package-level variable out from under the goroutine that is
	// blocked reading it.
	stdin *os.File
	// themeAskedAt is when the OSC 11 background query was written to the
	// terminal, or the zero time if it never was (--theme, MAGMUX_THEME or
	// TERM_THEME said which palette to use, or stdin is not a tty, or magmux is
	// headless). It opens the window in which inputLoop swallows a late reply;
	// see themeReplyWindow. COLORFGBG cannot set it: that source is consulted
	// only after the probe was skipped or went unanswered.
	//
	// Same discipline as pendingInput: written in init(), read by inputLoop on
	// the same goroutine, and nothing else touches it.
	themeAskedAt time.Time

	// ── chrome (Ctrl-G p / Ctrl-G s) ────────────────────────────────────────
	//
	// Both of these are stated as NEGATIVES so the zero value is today's
	// behaviour: a Magmux built by a test literal still gets its status row and
	// still reserves it in buildGrid/buildLayout. Guarded by treeMu — the
	// status row is layout, and so is where the panel sits.

	// hideStatus removes the bottom status row and gives it to the layout
	// (--no-status, Ctrl-G s).
	hideStatus bool

	// panel is the control-panel pane, on screen or hidden. Nil when this
	// magmux has none (every test literal, and any future mode that opts out).
	// A pointer rather than "the first isControl pane in allPanes", because the
	// PTY-less pane is also what the unit tests build their fake sessions from
	// and a search would find one of those instead.
	panel *Pane

	// panelAnchor / panelSplit / panelRatio / panelFirst remember where the
	// panel was when it was hidden, so showing it puts it BACK rather than
	// somewhere plausible.
	//
	// The anchor is the sibling node that inherited the panel's space, because
	// that is exactly what removeLeafLocked leaves behind: it collapses the
	// parent into the sibling and hands the sibling the parent's exact
	// geometry. Splitting that same node again with the same type and ratio, on
	// the same side, is the inverse operation and restores the tree byte for
	// byte. It is a POINTER and not an id because the sibling is usually an
	// internal node, which has no id; showPanelLocked therefore re-verifies it
	// is still reachable from m.root before using it and falls back to the root
	// if an agent closed it in the meantime.
	//
	// panelFirst is stated as the negative for the usual reason: the zero value
	// has to be the right answer for a panel that has never been hidden, and
	// every layout builder puts the panel LAST — child2, the right-hand column.
	panelAnchor *Pane
	panelSplit  SplitType
	panelRatio  float64
	panelFirst  bool // the panel was child1 (the left-hand / upper column)

	// chromeNote is a transient refusal shown in the status bar — "no room for
	// the panel" on a terminal too narrow to split. It is deliberately NOT part
	// of m.statusText: it belongs to the keystroke that caused it, not to the
	// run, and it expires on its own.
	chromeNote   string
	chromeNoteAt time.Time

	// chordArmed is set for exactly as long as Ctrl-G has been pressed and
	// magmux is waiting for the second key. The status row shows the chord's
	// own second keys for that beat, which is the only way a prefix key can
	// teach itself on a terminal that otherwise shows nothing. Written by
	// inputLoop under treeMu.Lock, read by renderLocked under RLock.
	chordArmed bool
}

// claimSession atomically attempts to mark `path` as owned by `p`. Returns
// true if the claim succeeded (path was free). Used by controllers that
// resolve their target file by scanning a directory.
func (m *Magmux) claimSession(path string, p *Pane) bool {
	m.claimedMu.Lock()
	defer m.claimedMu.Unlock()
	if m.claimedSessions == nil {
		m.claimedSessions = make(map[string]*Pane)
	}
	if owner, ok := m.claimedSessions[path]; ok && owner != p {
		return false
	}
	m.claimedSessions[path] = p
	return true
}

// isSessionClaimed returns true if `path` is already owned by a pane other
// than `p`. Used by controllers to skip files claimed by siblings during
// directory scanning.
func (m *Magmux) isSessionClaimed(path string, p *Pane) bool {
	m.claimedMu.Lock()
	defer m.claimedMu.Unlock()
	if owner, ok := m.claimedSessions[path]; ok && owner != p {
		return true
	}
	return false
}

// releaseSessions drops every transcript claim held by p.
//
// claimedSessions was never cleaned, which was harmless while panes only ever
// appeared. Once a pane can be closed it is not: a later pane in the same
// project can never claim that transcript, so its controller sits in `starting`
// silently and forever with nothing logged anywhere.
//
// Caller must NOT hold claimedMu.
func (m *Magmux) releaseSessions(p *Pane) {
	if p == nil {
		return
	}
	m.claimedMu.Lock()
	for path, owner := range m.claimedSessions {
		if owner == p {
			delete(m.claimedSessions, path)
		}
	}
	m.claimedMu.Unlock()
}

// ── Pane identity ────────────────────────────────────────────────────────────
//
// m.allPanes is an append-only slot table and a pane's id IS its index. Nothing
// ever renumbers: close_pane tombstones the slot instead of compacting, because
// the socket protocol's only addressing mode is an integer, so compacting would
// make `send` to pane 1 quietly hit a different session with no error anywhere.
//
// The two resolvers below are the ONLY way to turn an int into a *Pane. Any
// surviving raw subscript of allPanes writes tint, overlay or keystrokes into
// a detached pane — no error, no repaint, no log — which is why the rule is
// enforced by grepping for a subscript of allPanes and expecting no hit
// outside paneByIDLocked.

// paneByIDLocked returns the live pane with this id, or nil if the id is
// negative, out of range, or tombstoned. Caller holds at least treeMu.RLock.
func (m *Magmux) paneByIDLocked(id int) *Pane {
	if id < 0 || id >= len(m.allPanes) {
		return nil
	}
	p := m.allPanes[id]
	if p == nil || p.closed {
		return nil
	}
	return p
}

// livePanesLocked appends every live (non-tombstoned) pane to dst and returns
// it. Pass a nil dst for a fresh slice, or a reused one to avoid allocating in
// the render path. Caller holds at least treeMu.RLock.
func (m *Magmux) livePanesLocked(dst []*Pane) []*Pane {
	dst = dst[:0]
	for _, p := range m.allPanes {
		if p == nil || p.closed {
			continue
		}
		dst = append(dst, p)
	}
	return dst
}

// headlessSize is the geometry a run with no terminal gets.
//
// COLUMNS/LINES first, and the consequence is DELIBERATE: magmux exports exactly
// those two to its own children, so a headless magmux started INSIDE a magmux
// pane inherits that pane's size rather than a stand-in. That is the right
// answer — the nested run's captures then line up with the space a human is
// actually looking at — but it means the same command can produce different
// `results` geometry depending on where it was launched from, which is why it is
// written down and tested rather than left to be discovered.
//
// 80x24 otherwise: the VT100 default every tool assumes.
//
// The bounds are not paranoia. newScreen(h, w) allocates h*w Cells, so
// COLUMNS=99999999 is an OOM with a stack trace instead of an error, and
// COLUMNS=0 gives every pane a zero-width screen whose every capture comes back
// empty with no error anywhere. Both arrive from the ENVIRONMENT, the input
// class nobody validates.
// A per-side bound is NOT enough, and the first version of this function only
// had one. newScreen allocates rows*cols Cells and a Cell is 20 bytes here
// (rune 4 + two 6-byte Colors + Attr 2 + two bools), so LINES=9999
// COLUMNS=9999 — both inside a 10000 per-side cap — is 99,980,001 cells and a
// ~2GB allocation before a single byte of output, which is the OOM the comment
// above claims to have prevented. The PRODUCT is what costs memory, so the
// product is what is capped. 4,000,000 cells is ~80MB: far past any real
// terminal (a 300x600 pane is 180,000) and nowhere near a container limit.
const maxHeadlessCells = 4_000_000

func headlessSize() (rows, cols int) {
	rows, cols = 24, 80
	if v, err := strconv.Atoi(os.Getenv("LINES")); err == nil && v > 0 && v < 10000 {
		rows = v
	}
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v > 0 && v < 10000 {
		cols = v
	}
	// Both together, after both are known: either alone can be legal while the
	// pair is not. Falling back to 80x24 rather than clamping one side keeps the
	// answer a geometry somebody could actually have meant, and it is said out
	// loud because a silently ignored COLUMNS is indistinguishable from a bug.
	if rows*cols > maxHeadlessCells {
		fmt.Fprintf(os.Stderr, "magmux: ignoring LINES=%d COLUMNS=%d (%d cells exceeds the %d-cell cap); using 80x24\n",
			rows, cols, rows*cols, maxHeadlessCells)
		rows, cols = 24, 80
	}
	return rows, cols
}

// armChord arms or disarms the Ctrl-G menu on the status row. It marks the
// screen dirty because an idle magmux paints nothing at all, so the menu would
// otherwise never appear — and would never leave. Caller must NOT hold treeMu.
func (m *Magmux) armChord(on bool) {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	if m.chordArmed == on {
		return
	}
	m.chordArmed = on
	m.markAllDirtyLocked()
}

// paneByID is the locking twin of paneByIDLocked, for callers that hold nothing.
// Caller must NOT hold treeMu.
func (m *Magmux) paneByID(id int) *Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.paneByIDLocked(id)
}

// livePanes is the locking twin of livePanesLocked.
// Caller must NOT hold treeMu.
func (m *Magmux) livePanes() []*Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.livePanesLocked(nil)
}

func (m *Magmux) init() error {
	m.startedAt = time.Now()
	// Debug log
	if os.Getenv("MAGMUX_DEBUG") != "" {
		dbgFile, _ = os.Create("/tmp/magmux-debug.log")
	}

	// Parse selection color config from env
	if v := os.Getenv("MAGMUX_SEL_FG"); v != "" {
		fmt.Sscanf(v, "%d", &selFg)
	}
	if v := os.Getenv("MAGMUX_SEL_BG"); v != "" {
		fmt.Sscanf(v, "%d", &selBg)
	}

	fd := int(m.stdinFile().Fd())

	// Auto-degrade. Stdin that is not a terminal cannot be put in raw mode and
	// has no geometry — term.MakeRaw AND term.GetSize both fail on a pipe — so a
	// caller redirecting stdin has already asked for headless whether or not
	// they know the flag exists. `magmux -w -e 'echo hi' < /dev/null` used to
	// exit 1 with "raw mode: operation not supported by device"; this is the one
	// line that fixes it.
	//
	// Resolved HERE and not in main() so the mode is reachable from a test that
	// never spawns a process: &Magmux{stdin: pipeReader} + init() degrades,
	// because a pipe is not a terminal.
	if !m.headless && !term.IsTerminal(fd) {
		m.headless = true
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "init: headless=%v (stdin fd %d is a terminal: %v)\n",
			m.headless, fd, term.IsTerminal(fd))
	}

	if m.headless {
		// Synthetic geometry. buildGrid divides by m.cols and subtracts
		// statusRowsLocked from m.rows; zeros there give every pane a zero-sized
		// Screen and every capture comes back empty. errPanesDontFit is what
		// turns "too small to be usable" into an error rather than that silence.
		m.rows, m.cols = headlessSize()
		m.quit = make(chan struct{})
		// Still called: --theme / MAGMUX_THEME / TERM_THEME / COLORFGBG still
		// choose a palette, and the palette is what children are told about
		// the background. Only the PROBE is skipped, by initTheme's own guard,
		// which hands theme.Resolve a nil probe so the walk reaches COLORFGBG.
		m.initTheme(fd)
		return nil
	}

	// Enter raw mode
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	m.rawState = state

	// Get terminal size
	w, h, err := term.GetSize(fd)
	if err != nil {
		m.restore()
		return fmt.Errorf("get size: %w", err)
	}
	m.rows = h
	m.cols = w
	m.quit = make(chan struct{})

	// Alternate screen + hide cursor + enable SGR mouse tracking
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[?1000h\x1b[?1002h\x1b[?1006h")

	// Pick the palette. Runs here and nowhere else: after MakeRaw (so the
	// terminal will not echo the query, and the reply arrives byte-for-byte),
	// on the alternate screen (so a terminal that prints the query instead of
	// answering it scribbles on a screen we are about to paint over anyway),
	// and before any child is spawned or any goroutine reads stdin.
	m.initTheme(fd)

	return nil
}

// initTheme resolves the palette once, in the order stated in theme.go:
// --theme, MAGMUX_THEME, TERM_THEME, the OSC 11 probe, COLORFGBG, dark. It
// gathers the inputs and decides whether a probe is POSSIBLE; the walk itself
// is theme.Resolve, and the write half is applyTheme. Any keystrokes the probe
// swallowed are parked in m.pendingInput for inputLoop; see the field's
// comment for why that matters.
func (m *Magmux) initTheme(fd int) {
	in := theme.Env()
	in.Flag = m.themePref
	// Nothing to ask and nobody to answer: a piped stdin has no background
	// colour, and a dumb terminal has no OSC at all. Both would otherwise cost
	// every run the probe timeout for nothing.
	//
	// m.headless is the third case and is NOT implied by the first two:
	// --headless forced from a real tty passes term.IsTerminal, and the probe
	// writes its query to FD 0. That is a byte on the terminal from a mode
	// whose whole contract is that it emits none, which is why this guard is
	// listed as its own site rather than folded into the stdout enumeration.
	//
	// The guard is OUTSIDE the closure, so "cannot ask" is a nil probe and the
	// walk carries on to COLORFGBG instead of stopping at dark.
	var probe func() theme.ProbeResult
	if !(m.headless || !term.IsTerminal(fd) || os.Getenv("TERM") == "dumb") {
		probe = func() theme.ProbeResult {
			// Recorded before the probe writes, and only on the path that
			// actually writes: a word-valued answer (--theme, MAGMUX_THEME,
			// TERM_THEME) never asks the terminal anything, so inputLoop must
			// not then go looking for a reply.
			m.themeAskedAt = time.Now()
			k, c, ok, rest := theme.DetectColor(m.stdinFile(), theme.ProbeTimeout)
			return theme.ProbeResult{Kind: k, Color: c, OK: ok, Leftover: rest}
		}
	}
	m.applyTheme(theme.Resolve(in, probe))
}

// applyTheme is initTheme's write half, split out so a test can run it on a
// resolution it built by hand and then ask a pane what colour the terminal is.
func (m *Magmux) applyTheme(res theme.Resolution) {
	theme.Set(res.Kind)
	// After theme.Set, which resets the reported colours to the palette's
	// assumptions. Only a probe that answered carries a measured colour; every
	// other source keeps the palette's stand-in — a coherent guess, which is
	// all a child needs.
	if res.ProbedOK {
		theme.SetDetectedBackground(res.Probed)
	}
	m.pendingInput = append(m.pendingInput, res.Leftover...)
	if dbgFile != nil {
		// Names the source that answered. "probe skipped" is a statement about
		// the probe, never about the theme: when TERM_THEME decided, the line
		// says so and claims nothing about detection.
		fmt.Fprintf(dbgFile, "theme: %s via %s (probe %s; %d bytes of input preserved; background %s)\n",
			res.Kind, res.Source, probeState(res), len(res.Leftover), theme.XColorString(theme.TermBack))
	}
}

// probeState is the debug line's word for what the OSC 11 step did.
func probeState(res theme.Resolution) string {
	switch {
	case !res.ProbeRan:
		return "skipped"
	case res.ProbedOK:
		return "answered"
	}
	return "no answer"
}

func (m *Magmux) restore() {
	// Nothing was changed, so nothing is restored. rawState is nil headless, so
	// only the escape write below actually needed guarding — but returning here
	// states the rule once instead of leaving a reader to prove it, and it is
	// what makes AC1.4 ("zero bytes on stdout") hold on every exit path,
	// including the two `mux.restore(); os.Exit(1)` unwinds in main().
	if m.headless {
		return
	}
	// Disable mouse + show cursor + exit alternate screen
	os.Stdout.WriteString("\x1b[?1006l\x1b[?1002l\x1b[?1000l\x1b[?25h\x1b[?1049l")
	if m.rawState != nil {
		term.Restore(int(m.stdinFile().Fd()), m.rawState)
	}
}

func (m *Magmux) startReadLoops() {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	for _, p := range m.livePanesLocked(nil) {
		if p.isControl {
			continue // no PTY to read from
		}
		m.wg.Add(1)
		go p.readLoop(&m.wg)
	}
}

// attachControllers walks every leaf pane and attaches a ToolController if
// any registered factory recognizes the pane's command. Called once after
// the layout is built and panes are spawned.
func (m *Magmux) attachControllers() {
	for _, p := range m.livePanes() {
		m.attachController(p)
	}
}

// attachController attaches a ToolController to one pane if a factory
// recognizes its command.
//
// It exists separately from attachControllers because OpenPane calls it while
// the new pane is still PRIVATE — before it is appended to m.allPanes — so
// p.controller is written before any other goroutine can observe the pane, and
// Start()'s filesystem work happens off treeMu entirely.
//
// Caller must NOT hold treeMu (Start touches the filesystem) or p.mu.
func (m *Magmux) attachController(p *Pane) {
	if m.ctx == nil {
		m.ctx = context.Background()
	}
	p.mux = m
	if p.controller != nil {
		return
	}
	for _, factory := range m.controllerFactories {
		c := factory(p)
		if c == nil {
			continue
		}
		p.controller = c
		if err := c.Start(m.ctx); err != nil {
			if dbgFile != nil {
				fmt.Fprintf(dbgFile, "[ctrl] %s.Start error: %v\n", c.Name(), err)
			}
			p.controller = nil
			continue
		}
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl] attached %s to pane %d\n", c.Name(), p.id)
		}
		return
	}
}

// pollControllers polls each attached controller and translates the resulting
// Snapshot into pane state (inputReady, tint, overlayText). Throttled to ~4Hz;
// `force` skips the throttle for the final poll before teardown.
//
// It COLLECTS its snapshot events and returns them rather than broadcasting
// inline. Broadcasting here would put conn.Write — with its 100ms-per-client
// deadline — on the render goroutine, so one wedged subscriber would stall the
// frame. The caller broadcasts.
//
// Caller must NOT hold treeMu. Poll is filesystem work, not memory work:
// ClaudeCodeController.Poll tails a transcript and, until it has found one,
// re-scans every directory under ~/.claude/projects on EVERY tick. Under
// treeMu.RLock that scan blocks the next queued writer — a keystroke, SIGWINCH,
// or any socket open_pane/close_pane/focus — for its whole duration, which is
// how a healthy magmux misses a 10s MCP lifecycle timeout. So the pane list is
// snapshotted under RLock and the lock is released before any Poll runs.
//
// A pane closed between the snapshot and its Poll is polled anyway: the poll
// only reads a transcript, and applying its snapshot writes p.mu-guarded
// content fields on a pane nothing paints any more. Harmless, and cheaper than
// re-resolving every pane under the lock.
func (m *Magmux) pollControllers(force bool) []any {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()

	now := time.Now()
	if !force && !m.lastControllerPoll.IsZero() && now.Sub(m.lastControllerPoll) < 250*time.Millisecond {
		return nil
	}
	m.lastControllerPoll = now

	m.treeMu.RLock()
	panes := m.livePanesLocked(nil)
	m.treeMu.RUnlock()

	var events []any
	for _, p := range panes {
		if p.controller == nil {
			continue
		}
		snap, err := p.controller.Poll()
		if err != nil {
			if dbgFile != nil {
				fmt.Fprintf(dbgFile, "[ctrl] %s.Poll error: %v\n", p.controller.Name(), err)
			}
			continue
		}
		p.mu.Lock()
		prev := p.controllerSnap
		p.controllerSnap = snap
		applyControllerSnapshot(p, snap, m.noIdleDone)
		// p.label is write-once at construction — newPane and newControlPane are
		// its only writers, both before the pane is reachable — so it is as safe
		// to read unlocked as p.id. It is captured inside this block anyway
		// because that costs nothing and removes the need for a reader to
		// reconstruct the argument.
		label := p.label
		p.mu.Unlock()

		// Broadcast on meaningful change (state transition, new response,
		// tool change, or new prompt). Skip no-op polls.
		if snapshotChanged(prev, snap) {
			// Feed the control panel from what magmux actually observed,
			// not from what the pilot claims it asked for — the panel has
			// to be able to show the two disagreeing.
			m.control.recordObserved(p.id, snap.State.String(), snap.LastResponse, snap.LastTool)
			ev := map[string]any{
				"type":        "snapshot",
				"pane":        p.id,
				"controller":  p.controller.Name(),
				"state":       snap.State.String(),
				"project":     snap.Project,
				"model":       snap.Model,
				"prompt":      snap.LastUserPrompt,
				"response":    snap.LastResponse,
				"tool":        snap.LastTool,
				"startedAt":   timeStringOrEmpty(snap.StartedAt),
				"completedAt": timeStringOrEmpty(snap.CompletedAt),
			}
			// The one emit site of the three that lacked it. `results` and the
			// `list` verb (which is buildPaneResults verbatim) already carry the
			// label, so a client that addresses panes by name could resolve one
			// at connect time and then not recognise the live events about it.
			//
			// Added conditionally, matching buildPaneResultsLocked's shape: an
			// unlabelled pane emits no `label` key at all, exactly as today.
			if label != "" {
				ev["label"] = label
			}
			events = append(events, ev)
		}
	}
	return events
}

// snapshotChanged returns true if the meaningful fields of a controller
// snapshot differ from the previous one.
func snapshotChanged(a, b Snapshot) bool {
	if a.State != b.State {
		return true
	}
	if a.LastResponse != b.LastResponse {
		return true
	}
	if a.LastUserPrompt != b.LastUserPrompt {
		return true
	}
	if a.LastTool != b.LastTool {
		return true
	}
	return false
}

func timeStringOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// applyControllerSnapshot translates a Snapshot into pane visual state.
//
// noIdleDone suppresses the COMPLETION CHROME only. inputReady is still set,
// because it is what `snapshot` and `results` report and what a pilot waits on;
// the flag withdraws magmux's claim that the session finished, not its
// observation that the turn did. The awaiting-permission tint is deliberately
// not covered: a permission prompt is a thing the person must act on, not a
// "done" badge.
//
// Caller must hold p.mu.
func applyControllerSnapshot(p *Pane, s Snapshot, noIdleDone bool) {
	switch s.State {
	case CtrlAwaitingInput:
		if !p.inputReady {
			p.inputReady = true
			p.inputSignal = "ctrl"
			p.inputReadyAt = time.Now()
			p.dirty = true // the badge below is optional; the repaint is not
			if noIdleDone {
				break
			}
			p.tint = "green"
			p.overlayStyle = "success"
			lines := []string{"\u2713 DONE"}
			if !s.StartedAt.IsZero() && !s.CompletedAt.IsZero() {
				lines = append(lines, "took "+formatDuration(s.CompletedAt.Sub(s.StartedAt)))
			}
			if s.LastResponse != "" {
				msg := s.LastResponse
				// Collapse newlines and truncate to 40 runes
				msg = strings.ReplaceAll(msg, "\n", " ")
				msg = strings.TrimSpace(msg)
				if utf8.RuneCountInString(msg) > 40 {
					runes := []rune(msg)
					msg = string(runes[:39]) + "\u2026"
				}
				lines = append(lines, msg)
			}
			p.overlayText = strings.Join(lines, "\n")
			p.dirty = true
		}

	case CtrlAwaitingPermission:
		if !p.inputReady || p.inputSignal != "perm" {
			p.inputReady = true
			p.inputSignal = "perm"
			p.inputReadyAt = time.Now()
			p.tint = "yellow"
			p.overlayText = "\u26a0 NEEDS PERMISSION"
			p.overlayStyle = "info"
			p.dirty = true
		}

	case CtrlError:
		p.tint = "red"
		if s.Error != nil {
			p.overlayText = "\u2717 " + s.Error.Error()
		} else {
			p.overlayText = "\u2717 ERROR"
		}
		p.overlayStyle = "error"
		p.dirty = true

	case CtrlWorking, CtrlStarting:
		// A new turn started, so clear any stale "done" state. Reaching here
		// means applyTerminalIdle already declined to promote — the transcript
		// advanced more recently than the terminal went idle — so a lingering
		// terminal-derived inputReady is stale and would otherwise make
		// buildPaneResults report awaiting_input mid-turn. "perm" is left
		// alone: a permission prompt is a genuine block, not a finished turn.
		if p.inputReady && p.inputSignal != "perm" {
			p.inputReady = false
			p.tint = ""
			p.overlayText = ""
			p.overlayStyle = ""
			p.dirty = true
		}
	}
}

func (m *Magmux) handleSIGWINCH() {
	// A process whose stdin is a pipe is never sent SIGWINCH, and term.GetSize
	// would fail if it were. Under a FORCED --headless on a real tty the signal
	// CAN arrive, and is still ignored on purpose: headless geometry is
	// synthetic and stable, and reflowing panes under a controller mid-run
	// because a human dragged a window is a surprise nobody asked for.
	if m.headless {
		return
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-sigCh:
				w, h, err := term.GetSize(int(m.stdinFile().Fd()))
				if err != nil {
					continue
				}
				// Geometry is structural, and m.root.resize writes it on
				// every node in the tree — this is a writer, not a reader.
				m.treeMu.Lock()
				m.rows = h
				m.cols = w
				m.reflowLocked()
				m.treeMu.Unlock()
				// resize rebuilds each pane's Screen. A child process
				// redraws itself on SIGWINCH; the control panel has no
				// process to do that, so it must be repainted here or it
				// comes back blank.
				m.control.markDirty()
			case <-m.quit:
				return
			}
		}
	}()
}

// handleSignals turns the first lifecycle signal into magmux's ORDINARY quit,
// and a second one into an immediate exit.
//
// The handler must not unlink anything. The socket is removed inside
// socketServer's <-m.quit goroutine, which first broadcasts `results`, then
// `shutdown`, then closes every subscriber to give it a clean EOF *after* that
// event. Trapping the signal and calling os.Remove(m.sockPath) is the one-line
// version of this feature and it silently deletes `results` — the one event
// every integrator relies on.
//
// SIGINT is registered for `kill -INT`, not for Ctrl-C: magmux runs the tty in
// raw mode, which clears ISIG, so ^C arrives as byte 0x03 and inputLoop routes
// it.
//
// PLACEMENT: after markLayoutReady(), and nowhere earlier. The layout builders
// write m.root, m.allPanes and m.focused WITHOUT holding treeMu — legally,
// because startup is single-threaded up to that point. Closing m.quit inside
// that window is what makes it multi-threaded: socketServer's teardown
// goroutine wakes, calls buildPaneResults, takes treeMu.RLock and walks
// m.allPanes while buildGrid is still writing it. -race would flag that, and it
// is semantically wrong besides — main() would then go on to fork children for
// a session already torn down and already reported. Not before init() either:
// m.quit is created there, and close(nil) panics.
//
// The trade, said out loud: a SIGTERM arriving in that startup window — process
// start through child spawn and controller attach, tens of milliseconds — keeps
// today's default disposition, so the process dies and leaks its socket. It is
// bounded by the socket being pid-named, which means the synchronous reaper in
// socketServer removes it on the very next magmux start. Trading a rare
// self-healing leaked file for a startup data race is the right direction.
func (m *Magmux) handleSignals() {
	// Buffered so a second signal is not dropped while phase 1 is still
	// running: signal.Notify never blocks on a full channel, it DISCARDS.
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go m.signalLoop(ch, os.Exit)
}

// signalLoop is TWO PHASES, and is deliberately not a loop.
//
// The obvious `for { select { case <-m.quit: return; case <-ch: … } }` is
// broken three ways, all of which this shape avoids:
//
//  1. once the first signal has closed m.quit, <-m.quit is PERMANENTLY ready,
//     so the next iteration returns and the hard-exit branch is unreachable;
//  2. with a second signal already buffered, select chooses at RANDOM — so it
//     is nondeterministic, not merely wrong;
//  3. once the goroutine returns, signal.Notify is still registered with no
//     receiver, which SWALLOWS every later ^C rather than letting it take its
//     default disposition. A user hammering Ctrl-C at a wedged teardown would
//     be left with only SIGKILL — worse than not trapping signals at all.
//
// Phase 2 therefore waits on the SIGNAL CHANNEL ALONE, and must not also watch
// m.quit: by then m.quit is closed and would win every select. In the normal
// case phase 2 blocks forever and the process exits through main() returning.
// That is intentional — after the first signal this goroutine's only remaining
// job is to be a live receiver.
//
// exit is injected so a test can assert the hard-exit path without terminating
// the test binary. main() supplies os.Exit at the single call site above, which
// keeps the POLICY there. This is the one os.Exit outside main(), and it has to
// be: there is no return path from a goroutine, and the premise of a second
// signal is that the graceful path is stuck. Routing it through a channel
// main() selects on fails for the same reason — main() may be blocked inside
// inputLoop, waitSocketShutdown or cleanup, and the second signal exists
// precisely because one of those is.
func (m *Magmux) signalLoop(ch <-chan os.Signal, exit func(int)) {
	// ── phase 1 ──
	// hurry carries a signal that turned out to belong to phase 2 after all;
	// see the both-ready case below.
	var hurry os.Signal
	select {
	case <-m.quit:
		// Quit came from somewhere else (-w, -x, the Ctrl-G q chord). Fall
		// through rather than returning: a signal arriving DURING teardown
		// must still be able to cut it short.
	case s := <-ch:
		first := false
		m.quitOnce.Do(func() { close(m.quit); first = true })
		if !first {
			// m.quit was ALREADY closed, and select simply happened to pick
			// this case over the equally-ready <-m.quit above — select chooses
			// among READY cases at random. So this signal is a "hurry up"
			// arriving during teardown, not a "quit", and swallowing it here
			// would make the coin toss decide whether the user's signal did
			// anything at all. Carry it into phase 2 instead.
			//
			// Without this, the two-phase design reproduces the exact fault it
			// was written to avoid in the rejected for/select version:
			// nondeterminism, not merely wrongness.
			hurry = s
		}
	}

	// ── phase 2 ──
	// Any LATER signal is a human saying the graceful path is taking too long.
	// waitSocketShutdown allows 3s and closeSockClients 2s — both CEILINGS that
	// return as soon as teardown completes, but a person who has pressed ^C
	// twice must not be made to discover that.
	sig := hurry
	if sig == nil {
		sig = <-ch
	}
	// DEREGISTER FIRST, and before restore() specifically.
	//
	// From here on nothing reads ch again, and everything below can block:
	// restore() writes escape sequences to os.Stdout, and a tty that has wedged
	// blocks that write for as long as it likes. With signal.Notify still
	// registered and no receiver, a further ^C is buffered into ch and then —
	// once the buffer of 4 fills — silently DISCARDED, because signal.Notify
	// never blocks. The user is left with only SIGKILL. That is failure mode 3
	// from the header comment, the one the two-phase shape exists to avoid,
	// reappearing in the escalation path where it matters most.
	//
	// signal.Reset, not signal.Stop, for two reasons. Reset restores the
	// DEFAULT disposition for the named signals outright, which is the property
	// wanted here — the next ^C must kill the process, not merely miss a
	// channel. Stop is also unusable at this call site: it takes a send-side
	// `chan<- os.Signal` and ch is receive-only, and it would restore the
	// default only incidentally, by leaving zero registrations behind.
	signal.Reset(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	// restore() next, and unconditionally: os.Exit runs no defers, so the
	// `defer mux.restore()` in main() will not fire, and a terminal left in raw
	// mode with the alternate screen up is unusable.
	m.restore()
	n := 15 // SIGTERM's number, if the value is somehow not a syscall.Signal
	if s, ok := sig.(syscall.Signal); ok {
		n = int(s)
	}
	exit(128 + n)
}

// focusNext moves focus to the next live pane, wrapping. Tombstoned panes are
// skipped: they are not on screen, so focusing one would look like focus
// vanishing. Caller must NOT hold treeMu.
func (m *Magmux) focusNext() {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	// A hidden pane is not on screen, so cycling onto it would look exactly
	// like focus vanishing — the same reason tombstones are skipped.
	var live []*Pane
	for _, p := range m.livePanesLocked(nil) {
		if !p.hidden {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return
	}
	// The lock order is treeMu -> cp.mu, so telling the panel from in here is
	// legal; it marks which route row the user is actually looking at.
	for i, p := range live {
		if p == m.focused {
			m.focused = live[(i+1)%len(live)]
			m.control.setFocused(m.focused.id)
			return
		}
	}
	m.focused = live[0]
	m.control.setFocused(m.focused.id)
}

// findPaneAt returns the leaf pane at terminal coordinates (row, col).
// Caller must NOT hold treeMu.
func (m *Magmux) findPaneAt(row, col int) *Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	return m.findPaneAtLocked(row, col)
}

// findPaneAtLocked is the twin for callers already holding treeMu.
func (m *Magmux) findPaneAtLocked(row, col int) *Pane {
	return findPaneAtRecursive(m.root, row, col)
}

// focusedPane resolves m.focused to a pointer the caller can use once treeMu
// is released. Every keystroke path goes through it: writePTY blocks on the
// PTY, and rule 1 says the lock must not be held across that.
//
// Caller must NOT hold treeMu.
func (m *Magmux) focusedPane() *Pane {
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	if m.focused != nil && m.focused.closed {
		return nil
	}
	return m.focused
}

// typeToFocused delivers raw keystrokes to whichever pane has focus, or drops
// them if the layout has none left. Caller must NOT hold treeMu.
func (m *Magmux) typeToFocused(data []byte) {
	if p := m.focusedPane(); p != nil {
		p.writePTY(data)
	}
}

func findPaneAtRecursive(p *Pane, row, col int) *Pane {
	if p == nil {
		return nil
	}
	// Check if point is inside this pane's bounds
	if row < p.y || row >= p.y+p.h || col < p.x || col >= p.x+p.w {
		return nil
	}
	if p.splitType == SplitNone {
		return p
	}
	if found := findPaneAtRecursive(p.child1, row, col); found != nil {
		return found
	}
	return findPaneAtRecursive(p.child2, row, col)
}

// themeReplyWindow is how long after the OSC 11 query inputLoop keeps
// swallowing the terminal's answer to it.
//
// detectTheme waits theme.ProbeTimeout (150ms) and then gives up, but giving up
// is a decision about the palette, not about the bytes: a terminal behind an
// ssh hop or another multiplexer can answer a second or two later, and those
// bytes land in stdin, where inputLoop would type them into the focused pane as
// if the user had. magmux asked the question, so magmux eats the answer
// whenever it arrives.
//
// Three seconds is the bound: it is an order of magnitude over the slowest
// round trip a terminal reply plausibly takes, and short enough that a user
// cannot reach it by hand — the window only ever swallows a *well formed* OSC
// 11 reply, and typing one of those on purpose within three seconds of startup
// is not a thing that happens. Past the window every byte is forwarded exactly
// as before.
const themeReplyWindow = 3 * time.Second

// osc11MaxHold bounds how many bytes inputLoop will hold back while it decides
// whether it is looking at an OSC 11 reply. A real one is under 30 bytes
// ("\x1b]11;rgb:ffff/ffff/eeee\x1b\\" is 27); anything longer is not a reply,
// and holding it would let an odd or hostile stream park the user's input
// indefinitely by opening a sequence it never terminates.
const osc11MaxHold = 64

// takeLateOSC11 classifies the head of the input buffer during the theme-reply
// window. It returns the number of bytes to DISCARD (a complete reply), or
// hold=true meaning buf is a strict prefix of one and the decision has to wait
// for more bytes — which is what stops half a reply being forwarded when it
// spans two reads.
//
// It is deliberately narrow. Only ESC ] 1 1 ; qualifies, only with a printable
// body, and only terminated by BEL or ST. Every other OSC, every other escape
// sequence and every ordinary keystroke returns (0, false) and is handled by
// the untouched path below it: swallowing real input would be a far worse bug
// than the one this fixes.
func takeLateOSC11(buf []byte) (n int, hold bool) {
	const prefix = "\x1b]11;"
	if len(buf) < 2 {
		// A BARE ESC is never held, and that is a deliberate asymmetry. ESC is
		// a key the user can press, and in a finished grid it is the key that
		// dismisses the window; holding it until some other byte happens to
		// arrive makes magmux look hung. Nothing is lost by letting it
		// through: the loop below already parks a lone ESC waiting for the
		// rest of its sequence, so a reply split after its first byte is still
		// reassembled here on the next read — everywhere except the finished
		// grid, where ESC has always meant quit and still does.
		return 0, false
	}
	if len(buf) < len(prefix) {
		// A partial opening is held; anything else is somebody's keystroke.
		return 0, strings.HasPrefix(prefix, string(buf))
	}
	if !strings.HasPrefix(string(buf), prefix) {
		return 0, false
	}
	for i := len(prefix); i < len(buf); i++ {
		switch c := buf[i]; {
		case c == '\x07': // BEL terminator
			return i + 1, false
		case c == '\x1b': // ST terminator, ESC \
			if i+1 >= len(buf) {
				return 0, len(buf) < osc11MaxHold
			}
			if buf[i+1] == '\\' {
				return i + 2, false
			}
			return 0, false // ESC anything-else: not a reply
		case c < 0x20 || c > 0x7e:
			return 0, false // a control byte in the body: not a reply
		}
	}
	// Well formed so far, no terminator yet.
	return 0, len(buf) < osc11MaxHold
}

func (m *Magmux) inputLoop() {
	// Buffered input reader — accumulates partial reads so escape sequences
	// that span multiple read() calls are handled correctly.
	inbuf := make([]byte, 0, 4096)
	commandMode := false

	// The theme-reply window, resolved once. Zero if the probe never ran, in
	// which case nothing is ever swallowed and this loop behaves exactly as it
	// always has. Local rather than a field because only this goroutine cares,
	// and it latches shut on the first byte that arrives after it expires.
	var swallowUntil time.Time
	if !m.themeAskedAt.IsZero() {
		swallowUntil = m.themeAskedAt.Add(themeReplyWindow)
	}

	// Startup input first. The theme probe (init()) is the only other reader
	// stdin ever has, and anything it read that was not the terminal's OSC 11
	// reply is a keystroke that has not been handled yet. It goes in front of
	// everything that arrives from here on, and it is handled WITHOUT waiting
	// for the next key: a `q` typed into a finished grid during startup must
	// quit then, not when something else is pressed.
	inbuf = append(inbuf, m.pendingInput...)
	m.pendingInput = nil
	pending := len(inbuf) > 0

	// Stdin is read on a background goroutine so the main loop can also wake
	// on m.quit. Without this, a renderLoop-driven close(m.quit) (e.g. -w
	// auto-exit) cannot unblock the main goroutine, and magmux hangs.
	//
	// Resolved once, here, rather than dereferenced per read: this goroutine
	// outlives the loop below (it can still be parked in Read when m.quit
	// closes), so reading a variable somebody else might reassign is a race
	// with no upside.
	in := m.stdinFile()
	stdinCh := make(chan []byte, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				stdinCh <- chunk
			}
			if err != nil {
				close(stdinCh)
				return
			}
		}
	}()

	for {
		var chunk []byte
		if pending {
			// One pass over the startup bytes, then never again: if they were
			// an incomplete escape sequence the inner loop leaves them in
			// inbuf and we block for the rest, exactly as for live input.
			pending = false
		} else {
			select {
			case <-m.quit:
				return
			case c, ok := <-stdinCh:
				if !ok {
					return
				}
				chunk = c
			}
		}
		inbuf = append(inbuf, chunk...)

		for len(inbuf) > 0 {
			b := inbuf[0]

			if commandMode {
				commandMode = false
				m.armChord(false)
				switch b {
				case 'q':
					m.quitOnce.Do(func() { close(m.quit) })
					return
				case '\t', 'o':
					m.focusNext()
				case 'p':
					m.togglePanel()
				case 's':
					m.toggleStatusBar()
				case '[':
					// tmux's copy-mode binding, and an ACTION rather than a
					// toggle: it scrolls back one page, which both enters the
					// mode and shows something. A toggle that entered a mode
					// with an unchanged screen would look like a key that did
					// nothing.
					m.treeMu.RLock()
					page := 20
					if f := m.focused; f != nil {
						page = scrollPage(f.h)
					}
					m.treeMu.RUnlock()
					m.scrollFocusedBy(page)
				default:
					m.typeToFocused([]byte{0x07, b})
				}
				inbuf = inbuf[1:]
				continue
			}

			// The terminal's late answer to our own OSC 11 question, eaten
			// before anything downstream can mistake it for typing. It has to
			// come before the auto-close cancel below as well as before the
			// pane write: a reply that dismissed the completion countdown
			// would be the same bug wearing a different hat.
			if !swallowUntil.IsZero() {
				if time.Now().After(swallowUntil) {
					swallowUntil = time.Time{} // latched shut; never checked again
				} else if n, hold := takeLateOSC11(inbuf); n > 0 {
					inbuf = inbuf[n:]
					if dbgFile != nil {
						fmt.Fprintf(dbgFile, "[input] swallowed a late OSC 11 reply (%d bytes)\n", n)
					}
					continue
				} else if hold {
					// Only part of a reply so far. Wait for the rest rather
					// than forwarding a fragment we would then have to
					// un-send.
					goto needMore
				}
			}

			// Any keystroke cancels an armed auto-close: someone is here and
			// reading, so the window must not disappear under them.
			//
			// Read under RLock and escalate only when it is actually armed.
			// The countdown needs -x AND a pilot's finish, so in every other
			// run this is the zero value and taking the WRITE lock to
			// re-zero it — once per keystroke — put a queued writer in front
			// of the render loop's next RLock for no reason at all. Go's
			// RWMutex blocks later readers behind a waiting writer, so that
			// one keystroke cost a whole frame of latency.
			m.treeMu.RLock()
			armed := !m.closeAt.IsZero()
			m.treeMu.RUnlock()
			if armed {
				m.treeMu.Lock()
				m.closeAt = time.Time{}
				m.treeMu.Unlock()
				m.control.cancelClose()
			}

			// Keys aimed at the control panel scroll it. The panel has no PTY,
			// so without this every key pressed while it is focused is silently
			// swallowed and the pane looks broken.
			if f := m.focusedPane(); f != nil && f.isControl {
				if n := m.consumeControlKey(inbuf); n > 0 {
					inbuf = inbuf[n:]
					continue
				}
			}

			// Grid mode: when every pane's process has EXITED, q/Esc/Ctrl-C
			// exits. Deliberately allPanesDead, not allPanesDone: an idle pane
			// still has a child blocked on a read, and stealing its keystrokes
			// to offer a quit shortcut makes a live session read-only.
			if m.gridMode && m.allPanesDead() {
				if b == 0x03 || b == 'q' || b == 0x1b { // Ctrl-C, q, or Esc
					m.quitOnce.Do(func() { close(m.quit) })
					return
				}
				inbuf = inbuf[1:]
				continue
			}

			if b == commandKey&0x1f { // Ctrl-G
				commandMode = true
				m.armChord(true)
				inbuf = inbuf[1:]
				continue
			}

			// Scroll mode. It comes AFTER the Ctrl-G arm above, so the prefix —
			// and therefore quitting — keeps working while a pane is scrolled
			// back, and before the escape parsing below, so arrows and PgUp/PgDn
			// move the viewport instead of reaching the child. It is entered
			// only by Ctrl-G [ or the wheel, so nothing here can steal a key
			// from a TUI that has not been deliberately put aside.
			if m.focusedScrollOff() > 0 {
				if n := m.consumeScrollKey(inbuf); n > 0 {
					inbuf = inbuf[n:]
					continue
				}
				if len(inbuf) < 4 {
					goto needMore
				}
			}

			// ESC — could be start of mouse sequence or other escape
			if b == 0x1b {
				consumed, handled := m.tryParseEscape(inbuf)
				if consumed > 0 {
					inbuf = inbuf[consumed:]
					_ = handled
					continue
				}
				// Not enough data yet — might be partial escape sequence.
				// If this is the only data, wait for more. If there's plenty
				// of data and it's not a recognized sequence, pass it through.
				if len(inbuf) < 3 {
					// Need more data — break and read again
					goto needMore
				}
				// Not a recognized escape sequence, pass ESC through
				m.typeToFocused(inbuf[:1])
				inbuf = inbuf[1:]
				continue
			}

			// Regular byte — pass through to focused pane
			// Find the extent of non-escape bytes to batch-write
			end := 1
			for end < len(inbuf) && inbuf[end] != 0x1b && inbuf[end] != commandKey&0x1f {
				end++
			}
			m.typeToFocused(inbuf[:end])
			inbuf = inbuf[end:]
		}
		continue
	needMore:
		// Keep remaining bytes in inbuf, read more
	}
}

// tryParseEscape attempts to parse an escape sequence starting at buf[0]==ESC.
// Returns (bytes consumed, true) if handled, (0, false) if incomplete/not recognized.
func (m *Magmux) tryParseEscape(buf []byte) (int, bool) {
	if len(buf) < 2 {
		return 0, false // need more data
	}

	// CSI sequence: ESC [
	if buf[1] == '[' {
		if len(buf) < 3 {
			return 0, false // need more
		}

		// SGR mouse: ESC [ < params M/m
		if buf[2] == '<' {
			return m.parseSGRMouse(buf)
		}

		// Other CSI sequences (arrow keys, function keys, etc.)
		// Find the terminator: a byte in 0x40-0x7e range
		end := 2
		for end < len(buf) {
			if buf[end] >= 0x40 && buf[end] <= 0x7e {
				// Complete CSI sequence — forward to focused pane
				m.typeToFocused(buf[:end+1])
				return end + 1, true
			}
			end++
		}
		return 0, false // incomplete CSI
	}

	// OSC or other ESC sequences — forward as-is
	// ESC + single char (like ESC O for SS3)
	if buf[1] == 'O' {
		if len(buf) < 3 {
			return 0, false
		}
		m.typeToFocused(buf[:3])
		return 3, true
	}

	// Default: ESC + char, forward both
	m.typeToFocused(buf[:2])
	return 2, true
}

// cleanup reaps every child and waits for its read loop to finish.
//
// m.closing is set under treeMu BEFORE wg.Wait, and OpenPane refuses under the
// same lock: an open_pane landing between the two would call wg.Add while
// wg.Wait was running, which panics with "WaitGroup misuse: Add called
// concurrently with Wait".
//
// Caller must NOT hold treeMu.
func (m *Magmux) cleanup() {
	m.treeMu.Lock()
	m.closing = true
	panes := m.livePanesLocked(nil)
	m.treeMu.Unlock()

	// Signalling and closing PTYs is blocking I/O, so it happens off the lock.
	for _, p := range panes {
		if p.cmd != nil && p.cmd.Process != nil {
			p.cmd.Process.Signal(syscall.SIGHUP)
		}
		if p.ptmx != nil {
			p.ptmx.Close()
		}
	}
	m.wg.Wait()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func runeWidth(r rune) int {
	if r < 0x20 || r == 0x7f {
		return 0
	}
	// Common fast path: ASCII
	if r < 0x80 {
		return 1
	}
	// CJK ranges (rough check for wide chars)
	if (r >= 0x1100 && r <= 0x115f) ||
		r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x20000 && r <= 0x2fffd) ||
		(r >= 0x30000 && r <= 0x3fffd) {
		return 2
	}
	return 1
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sleepMs(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}
