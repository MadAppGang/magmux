package main

// THE DRIVER: a TUI of real interactions with the running demo.
//
//	cd demo/rc/driver && go run . --state $STATE --id rc-demo
//
// `demo/rc/selftest.ts` is the regression net and it is not the demo. This is
// the demo: a human moves a cursor, something genuinely happens to a real
// magmux over its real public HTTP API, and it is visible on both mirrors a
// second later.
//
// THE CONTRAST IS THE POINT. This process holds the FULL SESSION TOKEN, read
// off disk from `{sock-dir}/magmux-{id}.token`. Every mirror — the tmux pane
// above and the browser tab — holds the READ-ONLY VIEW TOKEN. So this driver
// can type into a shell and they provably cannot, and actions 8 and 9 make
// magmux say so in its own words rather than in mine: a real `403 forbidden`
// off the wire, shown verbatim.
//
// WHY THIS IS ITS OWN GO MODULE. magmux's root module has zero third-party
// dependencies and that is a load-bearing property of the project — the
// control panel is raw ANSI through magmux's own VT parser rather than a TUI
// library, precisely to keep it. A Bubble Tea driver in the root module would
// spend it on a demo. So `demo/rc/driver/go.mod` is a separate module with a
// `replace` back to the repository, and `go list -m all` at the root still
// answers x/sys and x/term and nothing else. `go test ./...` at the root does
// not descend into a nested module, so the suite is untouched too.
//
// THREE MODES, and only the first is the demo:
//
//	a TUI          when stdin and stdout are both a terminal
//	a line menu    when either is not — a pipe, a test, a `<<<` heredoc
//	--run N        one action, the same evidence as plain text, a real exit code
//
// The line menu is not a lesser copy: it is the SAME actions emitting the SAME
// Line stream through a different renderer, which is what stops the demo and
// its regression net telling two different stories. A TUI cannot be driven by
// piping stdin, so `--run N` is what `demo/rc/selftest.ts` drives.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
)

// Args is the command line, before any file has been read.
type Args struct {
	State     string
	ID        string
	URL       string
	TokenFile string
	ViewFile  string
	Place     string
	Mirror    string
	MirrorKey string
	Prefix    string
	QuitHint  string

	// Run, when set, is a single action key: do it, print the evidence, exit.
	Run string
	// Pane overrides the target. -1 means "ask magmux which pane the mirrors
	// are watching", which is what every other caller wants.
	Pane int
	// Theme is light|dark|auto. `auto` asks the terminal, which is the right
	// answer everywhere except a screenshot harness, where nothing answers.
	Theme string
	// Yes pre-confirms the one action that needs confirming, for --run 12.
	Yes bool
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Println(`magmux remote-control demo — the driver

  go run ./demo/rc/driver [--state DIR] [--id ID] [--url URL] [--run N]

  --state DIR       the demo's state directory (default $RC_DEMO_STATE)
  --id ID           the demo's id, which names the token file (default $RC_DEMO_ID)
  --url URL         magmux's URL (default: read from $STATE/magmux.url)
  --token-file F    the session token file (default $STATE/magmux-$ID.token)
  --view-token-file F  the read-only token file (default $STATE/view.token)
  --pane N          drive pane N instead of the one the mirrors are watching
  --run N           perform action N, print the evidence as plain text, and exit
  --yes             pre-confirm the one action that asks (12, kill magmux)
  --theme T         light | dark | auto (default $MAGMUX_THEME, else auto)
  --place P         'pane' or 'window' — where this driver is, for the hints
  --mirror M        'pane', 'window' or 'none' — where the terminal mirror is
  --mirror-key N    the tmux window the mirror is in, for the prefix hints
  --prefix K        the tmux prefix key as a human says it (default Ctrl-B)
  --quit-hint S     how to end the whole demo, for the quit line

  With no --run and a terminal on both ends it is a full-screen TUI. Given a
  pipe it falls back to a line-driven menu, so a test and a keyboard drive the
  same actions.`)
}

func parseArgs(argv []string) (Args, error) {
	a := Args{
		State:     os.Getenv("RC_DEMO_STATE"),
		ID:        env("RC_DEMO_ID", "rc-demo"),
		Place:     env("RC_DEMO_DRIVER_PLACE", "pane"),
		Mirror:    env("RC_DEMO_MIRROR_PLACE", "pane"),
		MirrorKey: env("RC_DEMO_MIRROR_KEY", "1"),
		Prefix:    env("RC_DEMO_PREFIX", "Ctrl-B"),
		QuitHint:  env("RC_DEMO_QUIT_HINT", "Ctrl-C the launcher"),
		Theme:     env("MAGMUX_THEME", "auto"),
		Pane:      -1,
	}
	next := func(i *int) string {
		*i++
		if *i < len(argv) {
			return argv[*i]
		}
		return ""
	}
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--state":
			a.State = next(&i)
		case "--id":
			a.ID = next(&i)
		case "--url":
			a.URL = next(&i)
		case "--token-file":
			a.TokenFile = next(&i)
		case "--view-token-file":
			a.ViewFile = next(&i)
		case "--pane":
			if _, err := fmt.Sscanf(next(&i), "%d", &a.Pane); err != nil {
				return a, fmt.Errorf("--pane wants a number")
			}
		case "--run":
			a.Run = next(&i)
		case "--yes":
			a.Yes = true
		case "--theme":
			a.Theme = next(&i)
		case "--place":
			a.Place = next(&i)
		case "--mirror":
			a.Mirror = next(&i)
		case "--mirror-key":
			a.MirrorKey = next(&i)
		case "--prefix":
			a.Prefix = next(&i)
		case "--quit-hint":
			a.QuitHint = next(&i)
		case "--help", "-h":
			usage()
			os.Exit(0)
		default:
			return a, fmt.Errorf("unknown argument %s", argv[i])
		}
	}
	if a.State == "" {
		a.State = filepath.Join(repoRoot(), ".task-grids", a.ID)
	}
	return a, nil
}

// repoRoot walks up from the working directory for the repository's own go.mod.
// The binary is built into .task-grids, so its own path says nothing useful.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/MadAppGang/magmux\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\x1b[31mdrive: "+format+"\x1b[0m\n", a...)
	os.Exit(1)
}

func main() {
	args, err := parseArgs(os.Args[1:])
	if err != nil {
		die("%v", err)
	}
	cfg, err := LoadConfig(args)
	if err != nil {
		die("%v", err)
	}

	switch {
	case args.Run != "":
		os.Exit(runOne(cfg, args))
	case isTTY():
		if err := runTUI(cfg, args); err != nil {
			die("%v", err)
		}
	default:
		os.Exit(runMenu(cfg, args))
	}
}

// isTTY decides which shape this process takes, and it asks about BOTH ends.
// stdout alone is not enough: the launcher's adopt mode hands the driver a real
// pane, while `demo.sh` under a test harness hands it two pipes, and a TUI on a
// pipe paints escape sequences into somebody's transcript.
func isTTY() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// resolveTheme picks the palette. `auto` means "ask the terminal", which in a
// TUI happens through tea.BackgroundColorMsg and here through a direct query;
// an explicit light|dark is what a screenshot harness and `MAGMUX_THEME` use,
// and magmux exports MAGMUX_THEME into every pane it opens, so a driver started
// inside a magmux pane inherits the resolution magmux already made.
func resolveTheme(pref string) Theme {
	switch strings.ToLower(pref) {
	case "light":
		return NewTheme(false)
	case "dark":
		return NewTheme(true)
	default:
		return NewTheme(lipgloss.HasDarkBackground(os.Stdin, os.Stdout))
	}
}
