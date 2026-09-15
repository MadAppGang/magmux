package mux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/pty"
	"github.com/MadAppGang/magmux/theme"
)

// ── Pane (NODE equivalent) ────────────────────────────────────────────────────

type SplitType int

const (
	SplitNone       SplitType = iota // leaf VIEW
	SplitHorizontal                  // left | right
	SplitVertical                    // top / bottom
)

type Pane struct {
	// id is this pane's permanent index into Magmux.allPanes. It is stamped
	// once, before the pane is published, and never changes — close_pane
	// tombstones the slot rather than compacting the slice, because the socket
	// protocol's only addressing mode is an integer and a renumbering would
	// silently redirect a `send` into a different session. Immutable after
	// publication, so it may be read without treeMu.
	id int
	// closed marks a pane detached from the layout by close_pane. Its allPanes
	// slot is retained forever so later ids never shift. Guarded by treeMu:
	// every "for every pane" loop must skip it, or tint/overlay/keystrokes
	// land in a pane nobody can see.
	closed bool
	// label is the short name open_pane was given, for clients that address
	// panes by name. Immutable after publication.
	label string
	// Structural fields — guarded by treeMu, NOT by mu. Everything below
	// screen is content state and stays under mu exactly as before.
	splitType     SplitType
	y, x, h, w    int // position and size in host terminal
	ratio         float64
	child1        *Pane
	child2        *Pane
	parent        *Pane
	screen        *Screen
	primaryScreen *Screen
	vt            VTParser
	ptmx          *os.File // master side of PTY
	cmd           *exec.Cmd
	mu            sync.Mutex
	dead          bool
	dirty         bool // content changed since last render
	altMode       bool // child is in alternate screen (vim, htop, etc.)
	bracketPaste  bool // child requested bracketed paste mode (2004)
	focusEvents   bool // child requested focus events (1004)
	cursorShape   int  // DECSCUSR: 0=default, 1=block blink, 2=block, 3=underline blink, 4=underline, 5=bar blink, 6=bar
	charsetG0     byte // 0='B' (ASCII), '0' (line drawing)
	charsetG1     byte
	useG1         bool // SO (shift out) active — use G1 instead of G0
	lastChar      rune // last printed character (for REP command)
	// Grid mode fields
	gridMode bool // pane is in grid mode (don't delete on exit)
	// reaped is set by waitForChild once cmd.Wait has returned, i.e. once the
	// child's pid has been collected and is free for the OS to reuse. The
	// force-kill path in reapPane checks it before signalling the process
	// GROUP, so a delayed SIGKILL can never land on a stranger. Guarded by mu.
	reaped bool
	// deadAt is when this pane FIRST went dead, whichever of the two writers of
	// p.dead got there first (readLoop on PTY EOF, reapChild on cmd.Wait). Both
	// stamp it under mu and both guard on IsZero, so neither can move it.
	//
	// Its ZERO value is load-bearing: "went dead at the dawn of time", so
	// time.Since(deadAt) is centuries and paneDoneLocked's grace period expires
	// instantly. That is what keeps every existing Pane{dead:true} test literal
	// behaving exactly as it does today. A test that wants to exercise the race
	// must set deadAt: time.Now() explicitly.
	deadAt       time.Time
	exitCode     int       // exit code of child process
	startedAt    time.Time // when the child process was started (for exec duration)
	tint         string    // "green", "red", "" — border/indicator color
	overlayText  string    // centered overlay text, may contain \n for multi-line (e.g. "✓ DONE")
	overlayStyle string    // "success", "error", "info"
	// Idle/completion detection
	inputReady  bool   // TUI app is waiting for user input
	inputSignal string // what triggered inputReady: "osc", "2004", "title", "idle", "ctrl", "perm"
	// inputReadyAt is when inputReady was last set true. Controllers order it
	// against their own transcript progress to decide which signal is fresher
	// (see ClaudeCodeController.applyTerminalIdle). Every path that sets
	// inputReady true must set this too.
	inputReadyAt      time.Time
	pasteWasOff       bool      // bracketed paste was disabled at least once (filters initial setup)
	textSincePasteOff bool      // printable text was written after the last paste-off (filters startup 2004 cycle)
	lastTextAt        time.Time // last time a printable character was output (for idle detection)
	hadTextOutput     bool      // true once any text has been printed (filters initial empty state)
	titleWasWorking   bool      // title showed a non-idle indicator at least once (filters startup ✳)
	titleIdleAt       time.Time // time the window title became idle (✳); zero if currently working
	// Agent status (set via IPC "agent" messages from coding tool hooks)
	agentStatus  string // "", "working", "idle", "waiting_input", "waiting_permission", "compacting"
	agentProject string // project name from hook
	agentTool    string // last tool being used
	agentPrompt  string // last user prompt
	// isControl marks the pane as magmux's own control panel: no PTY, no
	// child process, painted by ControlPanel.render. Every path that assumes
	// a pane owns a process (read loops, child waits, done-counting) must
	// skip it.
	isControl bool
	// hidden is the THIRD pane state, and it is neither `dead` nor `closed`:
	// the pane is alive and keeps every byte of its history, it is simply not
	// spliced into the layout tree and therefore occupies no columns. Only the
	// control panel is ever hidden (Ctrl-G p), and it starts that way unless -c
	// asked for it.
	//
	// A hidden pane is still in m.allPanes, still holds its id, and is still
	// reported by buildPaneResults — so `results` keeps saying state:"panel"
	// whether the panel is on screen or not. What it must be excluded from is
	// anything that reads GEOMETRY or paints: its y/x/h/w are whatever they
	// were when it was taken out, so largestLiveLeafLocked would happily pick
	// it as a split target and the dirty sweep would repaint a pane that is not
	// on screen. Guarded by treeMu — it is structural state, like `parent`.
	hidden bool
	// Interactive tool controller (e.g. ClaudeCodeController). Optional.
	controller     ToolController
	controllerSnap Snapshot
	// Back-reference to the owning Magmux. Set by attachControllers so
	// controllers can coordinate (e.g. claim shared resources).
	mux *Magmux
}

func newPane(y, x, h, w int, cfg PaneConfig) (*Pane, error) {
	p := &Pane{
		y:         y,
		x:         x,
		h:         h,
		w:         w,
		ratio:     0.5,
		charsetG0: 'B', // ASCII
		charsetG1: 'B',
		label:     cfg.Label,
	}
	p.screen = newScreen(h, w)
	p.primaryScreen = p.screen
	p.vt.node = p

	if err := p.spawnPTY(cfg); err != nil {
		return nil, fmt.Errorf("spawn PTY: %w", err)
	}
	return p, nil
}

// newControlPane builds a pane with no PTY and no child. It still gets a
// Screen and a VT parser, because that is how it is painted: ControlPanel
// writes ANSI into the parser exactly as a child process would, so the pane
// renders, scrolls, and selects through the ordinary pane path.
func newControlPane(y, x, h, w int, label string) *Pane {
	p := &Pane{
		y: y, x: x, h: h, w: w,
		ratio:     0.5,
		charsetG0: 'B',
		charsetG1: 'B',
		isControl: true,
		label:     label,
	}
	p.screen = newScreen(h, w)
	p.primaryScreen = p.screen
	p.vt.node = p
	return p
}

// newPaneFor builds either a normal child-process pane or the control pane,
// depending on the config. It is the single pane constructor: every layout
// builder and OpenPane goes through it, so a field added to PaneConfig cannot
// be honoured on one path and silently dropped on another.
func newPaneFor(y, x, h, w int, cfg PaneConfig) (*Pane, error) {
	if cfg.Control {
		return newControlPane(y, x, h, w, cfg.Label), nil
	}
	return newPane(y, x, h, w, cfg)
}

func (p *Pane) spawnPTY(cfg PaneConfig) error {
	ptmx, pts, err := pty.Open()
	if err != nil {
		return err
	}

	// Set initial size
	pty.SetWinSize(ptmx, p.h, p.w)

	cmd := exec.Command(cfg.Cmd, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}
	env := append(os.Environ(),
		"TERM=screen-256color",
		fmt.Sprintf("COLUMNS=%d", p.w),
		fmt.Sprintf("LINES=%d", p.h),
		// The RESOLVED theme, under the same name magmux reads as an input.
		//
		// A TUI child learns the background by querying OSC 11, which
		// answerColorQuery answers from this same resolution — but a child that
		// is not a TUI has no way to ask. pilot/pilot.ts was the case that
		// proved it: a plain bun script writing ANSI, which had to hardcode one
		// background's palette and was illegible on the other.
		//
		// It is appended AFTER os.Environ() on purpose. os/exec keeps the last
		// occurrence of a duplicated key, so magmux's own resolution wins over a
		// MAGMUX_THEME inherited from the shell — which is the whole point when
		// --theme said otherwise. cfg.Env still goes last and can still override.
		//
		// A NESTED magmux inherits it, and MAGMUX_THEME sits second in the
		// resolution chain, above the OSC 11 probe. That is the right answer:
		// the inner magmux is looking at a PTY, not at the terminal, so the
		// outer one's reading is better evidence than anything it can probe.
		"MAGMUX_THEME="+theme.Current.String(),
	)
	// Export socket path so children can discover it
	if sockPath := os.Getenv("MAGMUX_SOCK"); sockPath != "" {
		env = append(env, "MAGMUX_SOCK="+sockPath)
	}
	// Caller-supplied entries go last so they win over everything above,
	// including MAGMUX_SOCK — a pane deliberately pointed at another magmux is
	// a legitimate thing to ask for.
	env = append(env, cfg.Env...)
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		pts.Close()
		ptmx.Close()
		return err
	}
	pts.Close() // parent doesn't need slave side

	p.ptmx = ptmx
	p.cmd = cmd
	p.startedAt = time.Now()
	return nil
}

// lastNonEmptyLine returns the most recent meaningful visible line from the screen,
// trimmed and truncated to maxLen runes. Skips Claude Code status lines (anything
// containing "tokens" or starting with "* Opus"/"* Sonnet"/etc.) and the bare ❯
// prompt, preferring lines that look like response content. Caller must hold p.mu.
func (p *Pane) lastNonEmptyLine(maxLen int) string {
	if p.screen == nil {
		return ""
	}
	s := p.screen

	// Guard against uninitialized or zero-sized screens that can arise when
	// the controlling terminal reports 0x0 dimensions during startup.
	if s.rows <= 0 || s.cols <= 0 || len(s.cells) == 0 {
		return ""
	}

	// Read all rows into a slice of trimmed strings
	lines := make([]string, s.rows)
	for row := 0; row < s.rows && row < len(s.cells); row++ {
		var sb strings.Builder
		rowCells := s.cells[row]
		for c := 0; c < s.cols && c < len(rowCells); c++ {
			ch := rowCells[c].Ch
			if ch == 0 {
				sb.WriteByte(' ')
			} else {
				sb.WriteRune(ch)
			}
		}
		lines[row] = strings.TrimSpace(sb.String())
	}

	// Heuristic: skip lines that look like status/UI chrome
	isChrome := func(l string) bool {
		if l == "" {
			return true
		}
		// Bare or near-bare prompt lines
		if l == "\u276f" || strings.HasPrefix(l, "\u276f ") {
			return true
		}
		// Horizontal rules (mostly box drawing chars)
		nonRule := 0
		for _, r := range l {
			if r != '\u2500' && r != '\u2501' && r != '\u2014' && r != '-' && r != ' ' {
				nonRule++
			}
		}
		if nonRule == 0 {
			return true
		}
		// Claude Code status bar markers
		if strings.Contains(l, "tokens") {
			return true
		}
		if strings.Contains(l, "/effort") {
			return true
		}
		// Common status bar pattern: starts with "*" then model name
		if strings.HasPrefix(l, "* ") || strings.HasPrefix(l, "*  ") {
			return true
		}
		return false
	}

	// Scan from bottom to top, return first non-chrome line
	for row := s.rows - 1; row >= 0; row-- {
		l := lines[row]
		if isChrome(l) {
			continue
		}
		// Strip leading response bullet (⏺) for cleaner display
		l = strings.TrimPrefix(l, "\u23fa ")
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if utf8.RuneCountInString(l) > maxLen {
			runes := []rune(l)
			l = string(runes[:maxLen-1]) + "\u2026"
		}
		return l
	}
	return ""
}

// formatDuration renders a time.Duration compactly (e.g. "1.2s", "4m 12s", "1h 3m").
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

// clearCompletionLocked un-sticks a pane magmux had marked done, because
// something just pushed work into it.
//
// It is ONE function because it has two callers on two different paths — a
// human keystroke (writePTY) and a controller's instruction (injectPTY) — and
// they must agree on what "no longer done" means. They did not: writePTY left
// inputSignal, lastTextAt and titleIdleAt standing, so a keystroke into an
// idle Claude Code pane cleared the ✓ DONE chrome and then the text-idle sweep
// re-fired on the output that was already there and put it straight back.
//
// hadTextOutput going false is the load-bearing half: renderLocked's 5s rule
// needs new output before it can call the pane idle again. lastTextAt and
// titleIdleAt restart the two idle clocks that feed it.
//
// Caller HOLDS p.mu.
func (p *Pane) clearCompletionLocked() {
	if !p.inputReady {
		return
	}
	p.inputReady = false
	p.inputSignal = ""
	p.tint = ""
	p.overlayText = ""
	p.overlayStyle = ""
	p.hadTextOutput = false // require new output before re-detecting idle
	p.lastTextAt = time.Now()
	p.titleIdleAt = time.Time{}
	p.dirty = true // the completion chrome just went; repaint it away
}

// writePTY forwards HUMAN input to the pane's child.
//
// Every field it touches belongs to p.mu — the render goroutine reads
// inputReady, tint, overlayText, overlayStyle and hadTextOutput under that lock
// in leafTint, allPanesDoneLocked, the grid counter and renderLocked — and this
// runs on the INPUT goroutine, once per keystroke (typeToFocused) and once per
// forwarded mouse event (parseSGRMouse). It used to test and assign all of them
// with no lock at all. injectPTY is the correct twin and this now matches it,
// down to releasing p.mu before the write: a child that has stopped reading can
// block a PTY write for as long as it likes, and treeMu -> p.mu means a stalled
// keystroke would stall the next frame behind it.
//
// Caller must NOT hold p.mu.
func (p *Pane) writePTY(data []byte) {
	p.mu.Lock()
	if p.ptmx == nil {
		p.mu.Unlock()
		return
	}
	// In grid mode, don't forward input to a DEAD pane: this is what lets `q`
	// dismiss a finished grid. Idle is deliberately NOT in this guard.
	//
	// It used to be, and that made a pane a person could not type into while a
	// program could: `send` reaches an idle pane through injectPTY, which
	// refuses only a dead one, so the socket could steer a finished turn and
	// the keyboard could not. --help already promised the two behaved alike
	// ("clears its done state like a real keystroke"). An idle pane is an
	// interactive agent resting between turns, with a live process on the far
	// end of the PTY; there is something there to type into, and the reset
	// below is what stops the ✓ DONE chrome outliving the keystroke.
	//
	// This is also why magmux's own replies to a child's queries must NOT come
	// through here — see replyLocked.
	if p.gridMode && p.dead {
		p.mu.Unlock()
		return
	}
	// User input resets idle state — pane is no longer "done"
	if p.inputReady && dbgFile != nil {
		fmt.Fprintf(dbgFile, "[input] user keystroke → inputReady reset\n")
	}
	p.clearCompletionLocked()
	ptmx := p.ptmx
	p.mu.Unlock()

	ptmx.Write(data)
}

// replyLocked writes magmux's own answer to a child's query straight to the PTY.
//
// This is magmux being the TERMINAL, not a human being at a keyboard, and the
// two must not share a path. Routing replies through writePTY broke both halves
// of that:
//
//  1. writePTY refused a pane that was dead or awaiting input in grid mode. But
//     awaiting-input is exactly where a long-lived TUI sits, and Claude Code
//     re-queries OSC 11 on every SIGWINCH — which magmux sends to every pane
//     each time `Ctrl-G p` reshapes the layout. The question went into a void
//     and the child blocked: the blank-pane hang answerColorQuery exists to
//     prevent, reached by a second route. DA and DSR block a TUI just as hard.
//  2. writePTY clears inputReady, the tint, the overlay and hadTextOutput under
//     "user input resets idle state". A reply is not input, so a settled pane
//     silently read as working again and lost its ✓ DONE chrome.
//
// Issue #333 removed the idle half of reason 1: writePTY now refuses only a
// dead pane, so a reply routed through it would reach a resting TUI. Reason 2
// is untouched and is now load-bearing on its own — the merge that reason 1
// used to also forbid would still corrupt every settled pane that answers a
// SIGWINCH-triggered color query. Do not merge these paths.
//
// Caller HOLDS p.mu. Every caller is the VT parser, which runs inside
// readLoop's p.mu, so this cannot take the lock and cannot hand it back — the
// write goes out under it, exactly as it always has. That is safe for what it
// carries: a reply is a few dozen bytes and the child is by definition blocked
// reading them.
func (p *Pane) replyLocked(data []byte) {
	if p.ptmx == nil {
		return
	}
	p.ptmx.Write(data)
}

func (p *Pane) readLoop(wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 8192)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			p.mu.Lock()
			p.vt.write(buf[:n])
			p.dirty = true
			p.mu.Unlock()
		}
		if err != nil {
			p.mu.Lock()
			p.dead = true
			// Stamped, but NOT reaped: the PTY closing says the child let go of
			// its terminal, not that its status has been collected. cmd.Wait is
			// still running on waitForChild's goroutine and p.exitCode is still
			// zero. paneDoneLocked uses this stamp to bound how long -w waits
			// for the real number.
			if p.deadAt.IsZero() {
				p.deadAt = time.Now()
			}
			p.mu.Unlock()
			return
		}
	}
}

func (p *Pane) resize(y, x, h, w int) {
	p.y = y
	p.x = x
	p.h = h
	p.w = w
	if p.splitType == SplitNone {
		p.mu.Lock()
		// Both screens, reached through the PRIMARY. `p.screen` is whichever one
		// the child is currently on, and its .altScreen is nil when that is the
		// alt screen — so resizing through it left the primary at the old size
		// for the whole life of a full-screen app, and the shell came back to a
		// screen the wrong shape. Same root cause as setAltScreen's.
		prim := p.primaryScreen
		if prim == nil {
			prim = p.screen
		}
		if prim != nil {
			prim.resize(h, w)
			if prim.altScreen != nil {
				prim.altScreen.resize(h, w)
			}
		}
		p.mu.Unlock()
		if p.ptmx != nil {
			pty.SetWinSize(p.ptmx, h, w)
		}
	} else {
		p.reshapeChildren()
	}
}

// reshapeChildren reflows a split node's two children.
//
// The clamps are not defensive noise. w2 = p.w - w1 - 1 has no natural floor:
// three splits deep on an 80-column terminal, or one SIGWINCH shrinking a tree
// that was legal when it was built, yields zero or NEGATIVE dimensions. The
// zero-size guards downstream handle zero; negative reaches Screen.resize with
// a negative row count and every geometry assumption below it. Clamping here
// rather than in OpenPane is deliberate — OpenPane only covers the moment of
// creation, and the terminal can shrink at any time afterwards.
//
// Caller holds treeMu.Lock (geometry is structural state).
func (p *Pane) reshapeChildren() {
	if p.splitType == SplitHorizontal {
		w1 := clamp(int(float64(p.w)*p.ratio), 0, maxInt(0, p.w-1))
		w2 := maxInt(0, p.w-w1-1) // -1 for border
		h := maxInt(0, p.h)
		p.child1.resize(p.y, p.x, h, w1)
		p.child2.resize(p.y, p.x+w1+1, h, w2)
	} else if p.splitType == SplitVertical {
		h1 := clamp(int(float64(p.h)*p.ratio), 0, maxInt(0, p.h-1))
		h2 := maxInt(0, p.h-h1-1) // -1 for border
		w := maxInt(0, p.w)
		p.child1.resize(p.y, p.x, h1, w)
		p.child2.resize(p.y+h1+1, p.x, h2, w)
	}
}

// ── PaneConfig ────────────────────────────────────────────────────────────────

type PaneConfig struct {
	Cmd  string
	Args []string
	// Dir is the child's working directory; empty inherits magmux's own.
	// Setting it is what makes `open_pane` with a cwd real — and it is also
	// why ClaudeCodeController.Start consults p.cmd.Dir before os.Getwd(),
	// which is no longer this pane's directory.
	Dir string
	// Env is appended after magmux's own TERM/COLUMNS/LINES/MAGMUX_SOCK block,
	// so a caller can override any of them.
	Env []string
	// Label is a short human name for the pane, echoed back in `list` so a
	// client can address panes by name instead of by index.
	Label string
	// Control makes this a control-panel pane instead of a child process:
	// magmux paints it itself and Cmd/Args are ignored.
	Control bool
}
