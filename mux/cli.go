package mux

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/MadAppGang/magmux/buildinfo"
	"github.com/MadAppGang/magmux/sockdir"
	"github.com/MadAppGang/magmux/theme"
)

// ── Main ──────────────────────────────────────────────────────────────────────

// getUserShell returns the user's preferred shell (matching MTM's getshell())
func getUserShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// Main is the whole magmux process after the cmd/magmux shim: it takes the
// command-line arguments without the program name and returns the exit
// status. Most failure paths still call os.Exit themselves.
func Main(args []string) int {
	// `magmux mcp` never reaches here: cmd/magmux dispatches it to package mcp
	// as its first statement, before this --version/--help scan could claim a
	// --help meant for `magmux mcp`.

	// Handle --version / -v / --help / -h first pass
	for _, arg := range args {
		if arg == "--version" || arg == "-v" {
			fmt.Printf("magmux %s (%s)\n", buildinfo.Version, buildinfo.Commit)
			os.Exit(0)
		}
		if arg == "--help" || arg == "-h" {
			fmt.Println("magmux — Minimal Go Terminal Multiplexer")
			fmt.Printf("Version: %s (%s)\n\n", buildinfo.Version, buildinfo.Commit)
			fmt.Println("Usage: magmux [options]")
			fmt.Println("       magmux -e 'command1' -e 'command2'")
			fmt.Println("       magmux -e 'bun test' --label tests -e 'bun dev' --label server")
			fmt.Println("       magmux -g gridfile.txt")
			fmt.Println()
			fmt.Println("Options:")
			fmt.Println("  -g FILE   Grid file (one command per line, overrides -e)")
			fmt.Println("  -e CMD    Run CMD in a pane (can be repeated)")
			fmt.Println("            Every pane needs at least 3 rows and 20 columns; magmux refuses")
			fmt.Println("            a layout that cannot give every pane that much (about 12 panes")
			fmt.Println("            on an 80x24 terminal). -c costs one of them.")
			fmt.Println("  --label NAME  Name the pane the PRECEDING -e created. Echoed back in")
			fmt.Println("            `list`, `results` and live `snapshot` events, so a client can")
			fmt.Println("            address panes by name instead of by index. Ignored with a")
			fmt.Println("            message if no -e precedes it, and dropped by -g.")
			fmt.Println("  -w        Auto-exit when all panes are done (dead or idle)")
			fmt.Println("            Needs at least one -e/-g pane. `magmux -w` with no command runs")
			fmt.Println("            the default shell layout and never auto-exits, and so does")
			fmt.Println("            `magmux -w -c` with no -e (the panel is not a session); end")
			fmt.Println("            either with kill -TERM.")
			fmt.Println("  --headless    Run with no terminal: no raw mode, no alternate screen, and")
			fmt.Println("            not one byte on stdout. The socket is the whole interface — read")
			fmt.Println("            `results` for the outcome. Turned on automatically when stdin is")
			fmt.Println("            not a terminal, so `magmux -w -e CMD < /dev/null` just works.")
			fmt.Println("            Geometry comes from COLUMNS/LINES, else 80x24.")
			fmt.Println("  -c        Start with the control panel visible (it always exists;")
			fmt.Println("            without -c it starts hidden — Ctrl-G p reveals it)")
			fmt.Println("  --no-status   Start with the status bar hidden (Ctrl-G s toggles)")
			fmt.Println("  --no-idle-done  Manual mode: an idle pane is a resting session, not a")
			fmt.Println("            finished one. No ✓ DONE overlay, the counter keeps saying")
			fmt.Println("            running, and -w waits for the process to exit rather than")
			fmt.Println("            for a turn to end. Idle is still REPORTED on the socket, so")
			fmt.Println("            controllers are unaffected. Typing into an idle pane works")
			fmt.Println("            with or without this flag.")
			fmt.Println("  -x SECS   Close SECS after a pilot finishes (default: wait for a keypress)")
			fmt.Println("  --id NAME Bind magmux-NAME.sock instead of the pid socket ([A-Za-z0-9_-],")
			fmt.Println("            max 64, not all digits — an all-digit name is ambiguous with")
			fmt.Println("            a pid socket and the startup reaper could remove it)")
			fmt.Println("  --sock-dir DIR  Bind the IPC socket in DIR instead of /tmp. Ignored with")
			fmt.Println("            a message if DIR is missing, is not a directory, or would make")
			fmt.Println("            the socket path too long for the OS.")
			fmt.Println("  --theme MODE  light | dark | auto (default: auto). Resolution order:")
			fmt.Println("            --theme, MAGMUX_THEME, TERM_THEME, OSC 11 probe (interactive")
			fmt.Println("            tty only), COLORFGBG, then dark. auto means \"no opinion here\":")
			fmt.Println("            no CLI value forces the probe over a set MAGMUX_THEME; unset")
			fmt.Println("            the variable instead.")
			fmt.Println("  -v        Show version")
			fmt.Println("  -h        Show this help")
			fmt.Println()
			fmt.Println("Grid file format:")
			fmt.Println("  # Lines starting with # are comments")
			fmt.Println("  # Blank lines are skipped")
			fmt.Println("  echo 'hello world'")
			fmt.Println("  sleep 5 && echo done")
			fmt.Println()
			fmt.Println("Controls:")
			fmt.Println("  Ctrl-G q      Quit (always)")
			fmt.Println("  ↑/↓ PgUp/PgDn Scroll the control panel (when focused); wheel works anywhere")
			fmt.Println("  End / G       Control panel: resume following the newest exchange")
			fmt.Println("  q / Esc       Quit (grid mode, once every pane's process has exited)")
			fmt.Println("                A pane that is merely idle still takes your keystrokes:")
			fmt.Println("                typing into a finished turn is how you drive it by hand.")
			fmt.Println("  Ctrl-G Tab    Switch focus to next pane")
			fmt.Println("  Ctrl-G p      Show / hide the control panel (keeps its history)")
			fmt.Println("  Ctrl-G s      Show / hide the status bar")
			fmt.Println("  Ctrl-G [      Scroll the focused pane back through its scrollback.")
			fmt.Println("                Then k/j ↑/↓ PgUp/PgDn g/G move, q / Enter / Esc go live.")
			fmt.Println("                Alternate-screen panes (Claude Code, vim, htop) keep none.")
			fmt.Println("  Mouse wheel   Scroll a non-alt pane's scrollback; forwarded to TUIs")
			fmt.Println("  Mouse click   Switch focus to clicked pane")
			fmt.Println("  Mouse drag    Select text (auto-copies to clipboard)")
			fmt.Println()
			fmt.Println("IPC Socket:")
			fmt.Println("  /tmp/magmux-{pid}.sock — JSON line protocol")
			fmt.Println("  /tmp/magmux-{name}.sock with --id NAME (known before magmux starts)")
			fmt.Println("  --sock-dir DIR / MAGMUX_SOCK_DIR move the directory; /tmp is the default")
			fmt.Println("  Env var MAGMUX_SOCK exported to child processes")
			fmt.Println()
			fmt.Println("  On startup magmux removes sockets in that directory whose owning")
			fmt.Println("  process is gone — the only recovery from a SIGKILL, a crash or a")
			fmt.Println("  power loss, none of which run the normal teardown. It only ever")
			fmt.Println("  removes a pid-named socket whose pid it can prove is dead, so a")
			fmt.Println("  --id socket and a running session are both left alone.")
			fmt.Println()
			fmt.Println("Agent Status Monitoring:")
			fmt.Println("  Send agent hook events via IPC socket:")
			fmt.Println("  {\"type\":\"agent\",\"pane\":0,\"event\":\"Stop\",\"project\":\"myapp\"}")
			fmt.Println()
			fmt.Println("Controlled Sessions:")
			fmt.Println("  An external agent (the \"pilot\") reads pane state off the socket and")
			fmt.Println("  pushes the next instruction back in. Run with -c to watch the traffic.")
			fmt.Println()
			fmt.Println("  {\"type\":\"pilot\",\"event\":\"start\",\"pane\":0,\"goal\":\"...\",\"steps\":3}")
			fmt.Println("  {\"type\":\"send\",\"pane\":0,\"text\":\"run the tests\",\"label\":\"step 1/3\"}")
			fmt.Println("  {\"type\":\"send\",\"pane\":0,\"keys\":[\"escape\"],\"enter\":false}")
			fmt.Println("  {\"type\":\"pilot\",\"event\":\"finish\",\"summary\":\"all green\"}")
			fmt.Println()
			fmt.Println("  `send` writes to a pane even when it is idle — steering a finished")
			fmt.Println("  turn is the point — and clears its done state like a real keystroke.")
			fmt.Println()
			fmt.Println("Pane lifecycle (a message with an \"id\" gets one `reply` back):")
			fmt.Println("  {\"type\":\"open_pane\",\"id\":1,\"cmd\":\"claude\",\"cwd\":\"/proj\",\"split\":\"vertical\"}")
			fmt.Println("  {\"type\":\"close_pane\",\"id\":2,\"pane\":1,\"force\":false}")
			fmt.Println("  {\"type\":\"focus\",\"id\":3,\"pane\":0}")
			fmt.Println()
			fmt.Println("  Panes created from -e get ids 0..N-1 in ARGUMENT ORDER; magmux's own")
			fmt.Println("  panes (the control panel) are appended after them, whether or not -c")
			fmt.Println("  was passed. So the first pane an agent opens is not necessarily 1.")
			fmt.Println()
			fmt.Println("  Pane indices are permanent: closing a pane leaves a tombstone rather")
			fmt.Println("  than renumbering, so ids go sparse and never move under a caller.")
			fmt.Println()
			fmt.Println("  Events: SessionStart, UserPromptSubmit, PreToolUse, PostToolUse,")
			fmt.Println("          Stop, Notification, PermissionRequest, PreCompact, PostCompact,")
			fmt.Println("          SessionEnd")
			fmt.Println()
			fmt.Println("  Claude Code hook example (.claude/settings.json):")
			fmt.Println("    {\"hooks\":{\"Stop\":[{\"matcher\":\".*\",\"hooks\":[{")
			fmt.Println("      \"type\":\"command\",")
			fmt.Println("      \"command\":\"echo '{\\\"type\\\":\\\"agent\\\",\\\"pane\\\":0,\\\"event\\\":\\\"Stop\\\"}' | socat - UNIX:$MAGMUX_SOCK\"")
			fmt.Println("    }]}]}}")
			fmt.Println()
			fmt.Println("Environment:")
			fmt.Println("  MAGMUX_THEME    light | dark | auto. Second in the order: --theme beats it,")
			fmt.Println("                  and it beats TERM_THEME, the OSC 11 probe and COLORFGBG.")
			fmt.Println("                  Set it when a terminal answers OSC 11 wrongly.")
			fmt.Println("  TERM_THEME      light | dark, as set by the terminal or shell. Third in the")
			fmt.Println("                  order; when it answers, magmux does not probe the terminal")
			fmt.Println("                  at all. Any other value (including auto) is ignored without")
			fmt.Println("                  a warning: the variable is not magmux's to police.")
			fmt.Println("  COLORFGBG       fg;bg or fg;x;bg colour indexes (rxvt, konsole, iTerm2).")
			fmt.Println("                  Consulted only when the probe was skipped or did not answer.")
			fmt.Println("                  Background 0-6 or 8 is dark, 7 or 9-15 is light.")
			fmt.Println("  MAGMUX_SCROLLBACK  Lines of history kept per pane (default: 1000, 0 off).")
			fmt.Println("                  Only the primary screen records; the ring fills lazily,")
			fmt.Println("                  so a pane that never scrolls costs nothing.")
			fmt.Println("  MAGMUX_SOCK_DIR Directory for the IPC socket (default: /tmp). --sock-dir")
			fmt.Println("                  wins over it. Set this in an MCP client's own env block")
			fmt.Println("                  as well: --sock-dir cannot reach a `magmux mcp` that the")
			fmt.Println("                  client started in its own process tree.")
			fmt.Println("  MAGMUX_SEL_FG   Selection foreground (256-color index, default: 0)")
			fmt.Println("  MAGMUX_SEL_BG   Selection background (256-color index, default: 220)")
			fmt.Println("  MAGMUX_DEBUG    Enable debug logging to /tmp/magmux-debug.log")
			os.Exit(0)
		}
	}

	shell := getUserShell()

	// Parse flags (args is Main's parameter: os.Args[1:], passed by the shim)
	var gridFile string
	var customCmds []PaneConfig
	autoExit := false
	withControl := false
	noStatus := false
	noIdleDone := false
	headless := false
	var autoClose time.Duration
	var sockID string
	var sockDirArg string
	var themePref string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-g":
			if i+1 < len(args) {
				i++
				gridFile = args[i]
			}
		case "-e":
			if i+1 < len(args) {
				i++
				customCmds = append(customCmds, PaneConfig{
					Cmd:  shell,
					Args: []string{"-l", "-c", args[i]},
				})
			}
		case "--label":
			if i+1 < len(args) {
				i++
				// Binds to the PRECEDING -e by mutating the last element of the
				// list -e appends to, so "preceding" needs no extra state.
				//
				// Deliberate behaviours: two --label after one -e, last wins
				// silently (as a repeated --id does); a value that looks like a
				// flag is consumed as the label, identically to -g/-e/-x/--id/
				// --theme; and --label before any -e is warned and DROPPED, not
				// deferred — a flag that attaches forwards and backwards has no
				// readable rule.
				if len(customCmds) == 0 {
					// Warned, not fatal: the file's convention for --id and
					// --theme. A dropped label costs the caller a name and
					// nothing else, but silence would leave them wondering why
					// `list` never shows it.
					fmt.Fprintf(os.Stderr, "magmux: ignoring --label %q (no preceding -e)\n", args[i])
					break
				}
				customCmds[len(customCmds)-1].Label = args[i]
			}
		case "-w":
			autoExit = true
		case "-c", "--control":
			// Now means "start with the panel VISIBLE". Every session gets a
			// panel either way; without this it starts hidden and the sessions
			// have the whole terminal. Kept on the same flag deliberately, so
			// `magmux -c …` looks exactly as it always has.
			withControl = true
		case "--no-status":
			noStatus = true
		case "--no-idle-done":
			noIdleDone = true
		case "--headless":
			// Only ever sets it. The flag FORCES the mode; init() can still
			// turn it on by itself when stdin is not a terminal, and that
			// auto-degrade lives there rather than here so a test can reach the
			// mode without spawning a process.
			headless = true
		case "-x", "--close-after":
			if i+1 < len(args) {
				i++
				var secs int
				if _, err := fmt.Sscanf(args[i], "%d", &secs); err == nil && secs > 0 {
					autoClose = time.Duration(secs) * time.Second
				}
			}
		case "--theme":
			if i+1 < len(args) {
				i++
				if theme.ValidSetting(args[i]) {
					themePref = args[i]
				} else {
					// Ignored rather than fatal, like --id: auto-detection
					// still runs, so a typo costs the chosen palette and
					// nothing else. Said out loud, because "my --theme did
					// nothing" is otherwise silent.
					fmt.Fprintf(os.Stderr, "magmux: ignoring --theme %q (want light, dark or auto)\n", args[i])
				}
			}
		case "--id":
			if i+1 < len(args) {
				i++
				if sockdir.ValidSocketID(args[i]) {
					sockID = args[i]
				} else {
					// Ignored rather than fatal: the pid socket still binds, so
					// a bad name costs the caller its chosen path and nothing
					// else. Said out loud, because an agent polling
					// /tmp/magmux-<name>.sock would otherwise just hang.
					fmt.Fprintf(os.Stderr, "magmux: ignoring --id %q (allowed: A-Z a-z 0-9 _ -, max 64, not all digits)\n", args[i])
				}
			}
		case "--sock-dir":
			if i+1 < len(args) {
				i++
				// Only recorded here. It is RESOLVED after the loop, once
				// sockID is settled — see below.
				sockDirArg = args[i]
			}
		}
	}

	// --sock-dir, resolved after the loop and not inside it, because the
	// sun_path length check has to be made against the path that will REALLY be
	// bound. Probing with an empty id would happily pass a directory that fits
	// a 5-digit pid and is 40 bytes too long for a 64-character --id, and
	// net.Listen reports that as the unhelpful "invalid argument".
	//
	// Never fatal: a bad --sock-dir costs the caller its chosen path and
	// nothing else, exactly as a bad --id does. Said out loud on stderr,
	// because an agent polling the directory it asked for would otherwise just
	// hang.
	var muxSockDir string
	if sockDirArg != "" {
		id := sockID
		if id == "" {
			id = strconv.Itoa(os.Getpid())
		}
		if dir, ok, why := sockdir.ValidDir(sockDirArg, id); ok {
			// Three carriers, because there are three audiences. The package
			// var reaches the four MCP call sites in this process that have no
			// *Magmux to ask; the field is what this magmux binds; the env var
			// reaches CHILDREN, including a `magmux mcp` started from inside a
			// pane. It cannot reach an MCP server the client started in its own
			// process tree — nothing magmux does at runtime can — which is why
			// MAGMUX_SOCK_DIR exists as an env var at all and not as a
			// flag-only feature.
			sockdir.Dir = dir
			muxSockDir = dir
			os.Setenv("MAGMUX_SOCK_DIR", dir)
		} else {
			fmt.Fprintf(os.Stderr, "magmux: ignoring --sock-dir %q (%s); using %s\n", sockDirArg, why, sockdir.Dir)
		}
	}

	// Determine commands and mode
	useGrid := false
	var commands []PaneConfig

	if gridFile != "" {
		// -g overrides -e, and so it also discards anything --label attached to
		// an -e. Said out loud once, because a label that silently vanishes is
		// indistinguishable from a label magmux does not support.
		for _, c := range customCmds {
			if c.Label != "" {
				fmt.Fprintf(os.Stderr, "magmux: -g overrides -e, so --label names are ignored\n")
				break
			}
		}
		absPath, err := filepath.Abs(gridFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
		cmds, err := parseGridFile(absPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
		if len(cmds) == 0 {
			fmt.Fprintf(os.Stderr, "magmux: grid file has no commands\n")
			os.Exit(1)
		}
		commands = cmds
		useGrid = true
	} else if len(customCmds) > 0 {
		commands = customCmds
		useGrid = true
	} else if withControl {
		// -c with no commands is a bare control panel: useful for watching a
		// pilot drive nothing yet, and it keeps the flag from silently
		// falling through to the default 3-shell layout.
		commands = nil
		useGrid = true
	} else {
		// Default: 3 panes running user's login shell
		commands = []PaneConfig{
			{Cmd: shell, Args: []string{"-l"}},
			{Cmd: shell, Args: []string{"-l"}},
			{Cmd: shell, Args: []string{"-l"}},
		}
	}

	// The control panel is the last pane, so session panes keep the indices a
	// pilot would naturally use (pane 0 is the first -e command).
	//
	// Only when it starts VISIBLE. A panel that starts hidden is installed
	// after the layout is built (installHiddenPanel) rather than handed to the
	// builders, so the visible layout is byte-identical to a magmux with no
	// panel at all — see that function for why the builders cannot take it.
	if withControl && useGrid {
		commands = append(commands, PaneConfig{Control: true})
	}

	mux := &Magmux{
		gridMode:       useGrid,
		autoExit:       autoExit,
		hideStatus:     noStatus,
		noIdleDone:     noIdleDone,
		headless:       headless,
		autoCloseAfter: autoClose,
		sockID:         sockID,
		sockDir:        muxSockDir,
		themePref:      themePref,
		sockDone:       make(chan struct{}),
		layoutReady:    make(chan struct{}),
		control:        newControlPanel(),
		controllerFactories: []ControllerFactory{
			claudeCodeFactory,
		},
	}

	if err := mux.init(); err != nil {
		fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
		os.Exit(1)
	}
	defer mux.restore()

	// Publish MAGMUX_SOCK synchronously. socketServer does this too, but it
	// runs on its own goroutine and the sleep below is a delay, not a
	// synchronisation primitive: on a loaded machine that goroutine can still
	// be unscheduled when the first child forks, and the child then inherits an
	// empty MAGMUX_SOCK with nothing to say why. The path is derived from the
	// pid or --id, so it is known before the listener exists.
	mux.sockPath = mux.socketPath()
	os.Setenv("MAGMUX_SOCK", mux.sockPath)

	// Start socket server before spawning children (so MAGMUX_SOCK is set)
	go mux.socketServer()
	// A connection is accepted from this moment on, but the layout does not
	// exist yet, so nothing is served until markLayoutReady below. The defer is
	// the safety net: an early os.Exit takes the waiters with the process, but
	// any other way out of main must release them or a client sits on the
	// timeout for no reason.
	defer mux.markLayoutReady()
	// Give the socket a moment to bind before spawning children
	sleepMs(10)

	if useGrid {
		if err := mux.buildGrid(commands); err != nil {
			mux.restore()
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := mux.buildLayout(commands); err != nil {
			mux.restore()
			fmt.Fprintf(os.Stderr, "magmux: %v\n", err)
			os.Exit(1)
		}
	}

	// Every session gets a panel, whether or not -c was passed; without -c it
	// starts hidden and Ctrl-G p reveals it. This runs after the layout so the
	// panel takes the last id and the session layout is untouched.
	if !withControl {
		mux.installHiddenPanel()
	}

	// Bind the panel to whichever pane was built for it, and keep focus on a
	// real session — typing into the panel does nothing, so starting there
	// would look broken.
	//
	// Under treeMu because the socket server is already accepting connections:
	// a client that connects right now runs buildPaneResults, which reads
	// m.focused. The panel's own pane pointer is cp.mu's (attach takes it), and
	// the order treeMu -> cp.mu is the documented one.
	mux.treeMu.Lock()
	live := mux.livePanesLocked(nil)
	for _, p := range live {
		if p.isControl {
			mux.control.attach(p)
			// The pane Ctrl-G p toggles. Already set on the hidden path; this
			// is the -c one, where the builders made it as an ordinary pane.
			mux.panel = p
			if mux.focused == p {
				for _, q := range live {
					if !q.isControl {
						mux.focused = q
						break
					}
				}
			}
		}
	}
	mux.treeMu.Unlock()
	mux.control.markDirty()

	mux.startReadLoops()
	mux.attachControllers()

	// The layout is now real AND live: panes exist, their read loops are
	// running and their controllers are attached. Only now is the connect-time
	// aggregate worth anything — published here rather than straight after
	// buildGrid so a client's first snapshot describes panes that can actually
	// change state, not a layout still being wired up.
	mux.markLayoutReady()

	// Start a waiter for every child, grid mode or not: waitForChild is the
	// only caller of cmd.Wait, so gating it on grid mode left the default
	// three-shell layout leaking a zombie per exited shell. The grid-only part
	// (the ✓ DONE / ✗ FAIL tombstone and the `exit` event) is gated inside
	// waitForChild by p.gridMode, so nothing about non-grid rendering changes.
	for _, p := range live {
		if p.isControl {
			continue // no child to wait on
		}
		go mux.waitForChild(p)
	}

	// Registered HERE, after markLayoutReady, and not one line earlier — see
	// handleSignals for the startup data race that placement exists to avoid.
	// Grouped with handleSIGWINCH so the two signal.Notify registrations sit
	// together.
	mux.handleSignals()
	mux.handleSIGWINCH()

	go mux.renderLoop()
	if mux.headless {
		// inputLoop cannot be the thing that blocks here. Its stdin goroutine
		// closes stdinCh on the first Read error, and stdin of /dev/null is EOF
		// on the first read — so inputLoop would return in microseconds, main
		// would fall through to waitSocketShutdown, and a -w run would exit
		// before a single pane had produced output. There is also nothing to
		// route: no keystrokes, no mouse, no chords.
		//
		// m.quit is closed by exactly two things that can reach a headless run:
		// render()'s quitOnce for -w / -x, and the signal loop. See --help for
		// the two shapes where -w never fires.
		<-mux.quit
	} else {
		mux.inputLoop()
	}

	// The socket server broadcasts the final `results` event and then closes
	// each subscriber, giving it a clean EOF *after* that event. That happens
	// in a goroutine woken by m.quit, so we must let it finish — exiting here
	// races it and drops results entirely.
	mux.waitSocketShutdown(3 * time.Second)

	// Cleanup socket (normally already removed by the teardown above)
	if mux.sockPath != "" {
		os.Remove(mux.sockPath)
	}

	mux.cleanup()
	return 0
}

// Suppress unused import warning
var _ = io.EOF
var _ = math.MaxInt
