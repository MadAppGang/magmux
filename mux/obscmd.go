package mux

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/proclive"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/obsview"
	"github.com/MadAppGang/magmux/sockdir"
)

// `magmux agents` — print what every Claude Code session on this machine is
// doing, and keep printing it as they change.
//
// # It only ever READS ~/.claude, and that is a hard rule rather than a habit
//
// The registry and jobs directories belong to the Claude Code processes
// writing them — thirty of them on the machine this was built on — and they
// rewrite their records IN PLACE while we are reading. So this command opens
// files and stats them, and creates, writes, renames and removes nothing at
// all, not even a temporary file. The `<pid>.<sha256>.key` siblings are not
// read either. The reading is the claudestate library's, which has its own
// guard that it writes nothing (G2).
//
// # This file and cli.go are the two that INSTALL the signal oracle
//
// The library takes the liveness answer through ports.ProcessOracle, and a nil
// oracle there means "undetermined" rather than a live probe — so a caller
// that does not inject one gets an honest non-answer instead of sending
// signals to whatever pids happen to exist on the machine running it. The real
// probe is claudestate's proclive.New() — kill(pid, 0), which SENDS NOTHING:
// nil and EPERM are alive (EPERM is "it exists and belongs to another user",
// which a bool-shaped helper once misread as gone and unlinked other users'
// live sockets), ESRCH is gone, anything else undetermined.
//
// proclive.New() is called in exactly two places, both of them a program entry
// point: `RunAgents`/`runObsFocus` below, and Main (cli.go). It is a parameter
// everywhere else, so no test can reach kill(pid, 0) without naming it, and
// TestNoTestCanReachTheSignalOracle fails when one does.

// RunAgents is the `magmux agents` entry point, dispatched by cmd/magmux before
// mux.Main sees any argument. It returns a process exit code.
func RunAgents(args []string) int {
	// `magmux agents focus …` is the one subcommand, and it is dispatched before
	// the flag set so that its own flags are parsed by its own parser rather
	// than being rejected here. It is the only thing under `magmux agents` that
	// writes anything at all; everything else on this path is a read.
	if len(args) > 0 && args[0] == "focus" {
		return runObsFocus(args[1:])
	}

	fs := flag.NewFlagSet("magmux agents", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printObsHelp(os.Stderr) }

	trees := agentsTreeFlags(fs)
	asJSON := fs.Bool("json", false, "emit the document as JSON instead of a table")
	once := fs.Bool("once", false, "print one document and exit instead of streaming")
	help := fs.Bool("help", false, "show this help")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printObsHelp(os.Stdout)
			return 0
		}
		return 2
	}
	if *help {
		printObsHelp(os.Stdout)
		return 0
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "magmux agents: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	root, err := trees.root(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents: %v\n", err)
		return 1
	}

	// The whole pipeline, not just a source: the document this prints is the
	// one each session's actor chain produced, published by the library's hub
	// and received here as an ordinary subscriber. The command is the only
	// place a person can see the layer working, so it exercises the same path
	// magmux does rather than a shortcut around it.
	//
	// With ONE honest difference: no pane ports and no host stages. This
	// process is not a magmux and has no pane table, so the pane questions are
	// answered "undetermined" rather than guessed at, no launch directory is
	// known (no row carries launchDir), and the INFERRED tier — which is made of
	// nothing but a pane's own traffic — has no input here at all. Every row
	// therefore resolves on the authoritative tier and no conflict can arise,
	// which is the truth about this process rather than a degradation of it.
	p, err := pipeline.Start(pipeline.Config{
		Root:   root,
		Oracle: proclive.New(),
		// A watch only when streaming. --once has nothing to wait for, and a
		// watcher opens a descriptor per record on Darwin.
		Watch: !*once,
		Clock: ports.RealClock(),
		// The epoch is a fact about this READ rather than about any session,
		// and is separate from the clock, whose job is to make timers.
		Now: time.Now,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents: %v\n", err)
		return 1
	}

	ctx := context.Background()
	sub, err := p.Subscribe(ctx, "stdout", 0)
	if err != nil {
		p.Stop()
		fmt.Fprintf(os.Stderr, "magmux agents: %v\n", err)
		return 1
	}

	code := consumeObs(p, sub, *once, *asJSON)

	// Stats BEFORE Stop, because a stopped pipeline answers nothing. They go
	// to stderr with the problems, for the same reason: --json on stdout stays
	// machine-readable.
	if st, err := p.Stats(ctx); err == nil {
		reportObsStats(os.Stderr, st)
	}
	p.Stop()
	return code
}

// agentsTrees is the tree flags `magmux agents` and `magmux agents focus`
// share, so the two cannot read different trees for the same arguments.
type agentsTrees struct {
	sessions, jobs, projects *string
}

func agentsTreeFlags(fs *flag.FlagSet) agentsTrees {
	return agentsTrees{
		sessions: fs.String("root", "", "registry directory (default: $CLAUDE_CONFIG_DIR/sessions, else ~/.claude/sessions)"),
		jobs: fs.String("jobs", "", "background-jobs directory (default: $CLAUDE_CONFIG_DIR/jobs, else ~/.claude/jobs; "+
			"off when --root is given without it)"),
		projects: fs.String("projects", "", "transcript directory (default: $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects)"),
	}
}

// root is §4.9's one rule from the tree flags to the library's Root.
//
//   - No tree flag given: the whole Claude root, $CLAUDE_CONFIG_DIR else
//     ~/.claude (ports.RootFromEnv), with Base set, so creating jobs/ later
//     wakes a streaming run (tt's M12).
//   - Any tree flag given: each given flag is that directory and Base is "".
//     An explicitly empty value (`--jobs ""`) is an absent source, reported
//     Off. An omitted --root or --projects keeps the default directory, as
//     before. An omitted --jobs keeps the default too — UNLESS --root was
//     given, in which case the jobs source is Off: a flag added by the
//     library must not add a read of the real jobs tree to an invocation that
//     staged its own registry and made none before (D6).
func (a agentsTrees) root(fs *flag.FlagSet) (ports.Root, error) {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if !given["root"] && !given["jobs"] && !given["projects"] {
		return ports.RootFromEnv()
	}
	sessions, err := pick(given["root"], *a.sessions, defaultRegistryRoot)
	if err != nil {
		return ports.Root{}, err
	}
	projects, err := pick(given["projects"], *a.projects, defaultProjectsRoot)
	if err != nil {
		// A transcript tree that cannot be found is not fatal: every
		// harness-derived column prints "-", which is the honest report.
		projects = ""
	}
	jobs := ""
	switch {
	case given["jobs"]:
		jobs = *a.jobs
	case !given["root"]:
		if dir, err := defaultJobsRoot(); err == nil {
			jobs = dir
		}
	}
	return claudeRoot(sessions, jobs, projects, "")
}

// pick is a given flag's value, else the default directory.
func pick(given bool, value string, def func() (string, error)) (string, error) {
	if given {
		return value, nil
	}
	return def()
}

// consumeObs is the sink: it prints every document the hub delivers until the
// process is signalled, or until the first one when --once was asked for.
//
// There is no poll and no ticker here or anywhere beneath it: an idle registry
// costs a few blocked goroutines and nothing else.
func consumeObs(p *pipeline.Pipeline, sub *pipeline.Subscription, once, asJSON bool) int {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(quit)

	// Nothing is published until something asks. Subscribing first and asking
	// second is what stops --once racing its own first document.
	p.Refresh()

	for {
		select {
		case d, ok := <-sub.C():
			if !ok {
				// The pipeline stopped underneath us. Not reachable from here
				// — this goroutine is the only thing that stops it — so it is
				// an error rather than a quiet exit.
				fmt.Fprintf(os.Stderr, "magmux agents: the pipeline stopped unexpectedly\n")
				return 1
			}
			// Problems go to STDERR so that --json on stdout stays machine
			// readable, and they are printed rather than counted because a
			// source that silently skips a record it could not read is
			// indistinguishable from a machine with fewer sessions on it.
			writeObsProblems(os.Stderr, d)
			if asJSON {
				obsview.WriteFleetJSON(os.Stdout, obsview.FromModel(d.Doc))
			} else {
				obsview.WriteFleetTable(os.Stdout, obsview.FromModel(d.Doc))
			}
			if once {
				return 0
			}
		case <-quit:
			return 0
		}
	}
}

// writeObsProblems prints one delivery's problems and losses, one line each,
// through the one display rule: a problem can quote a record's own text.
func writeObsProblems(w io.Writer, d pipeline.Delivery) {
	for _, pr := range d.Problems {
		fmt.Fprintf(w, "magmux agents: %s\n", obsview.DisplaySafe(pr.Error()))
	}
	if d.Dropped > 0 {
		// Said out loud rather than swallowed: the document below is still
		// correct — it is rebuilt from the directories, not from the events
		// — but a consumer counting changes would be short.
		fmt.Fprintf(w, "magmux agents: %d documents were dropped before this one\n", d.Dropped)
	}
}

// reportObsStats prints what the pipeline lost.
//
// Only the losses. A run where nothing was dropped prints nothing at all,
// because a line saying "0 dropped" on every exit trains a reader to skip the
// place the real number will appear.
func reportObsStats(w io.Writer, st pipeline.Stats) {
	if st.WatchDropped > 0 {
		fmt.Fprintf(w, "magmux agents: %d change notifications were dropped by the watcher\n", st.WatchDropped)
	}
	if st.ObservationsDropped > 0 {
		fmt.Fprintf(w, "magmux agents: %d observations were dropped by a busy session actor\n", st.ObservationsDropped)
	}
	if st.HubDropped > 0 {
		fmt.Fprintf(w, "magmux agents: %d documents were dropped by a slow subscriber\n", st.HubDropped)
	}
}

// The directories under one Claude root. They are the LAYOUT magmux was told
// about, not a decode of any record, so they are spelled here rather than in
// the library's schema (whose contract is the files' contents).
const (
	claudeSessionsDir = "sessions"
	claudeJobsDir     = "jobs"
	claudeProjectsDir = "projects"
)

// claudeConfigRoot is where Claude Code keeps its state: CLAUDE_CONFIG_DIR,
// Claude Code's own override, else ~/.claude. A user who has moved the
// directory has moved it for everything that reads it.
func claudeConfigRoot(what, flagName string) (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory, so cannot find the %s; pass %s: %w", what, flagName, err)
	}
	return filepath.Join(home, ".claude"), nil
}

// defaultRegistryRoot is where Claude Code keeps its session registry.
func defaultRegistryRoot() (string, error) {
	dir, err := claudeConfigRoot("session registry", "--root")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeSessionsDir), nil
}

// defaultProjectsRoot is where Claude Code keeps its transcripts.
func defaultProjectsRoot() (string, error) {
	dir, err := claudeConfigRoot("transcripts", "--projects")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeProjectsDir), nil
}

// defaultJobsRoot is where Claude Code keeps its background jobs.
func defaultJobsRoot() (string, error) {
	dir, err := claudeConfigRoot("background jobs", "--jobs")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claudeJobsDir), nil
}

func printObsHelp(w io.Writer) {
	fmt.Fprint(w, `magmux agents — what every Claude Code session on this machine is doing

Usage: magmux agents [--root DIR] [--jobs DIR] [--projects DIR] [--json] [--once]
       magmux agents focus TARGET          (see magmux agents focus --help)

Reads Claude Code's session registry and its background jobs, and prints one
document describing every session it can see: status, what a waiting session
is blocked on, whether the process is still alive, and how long it has been in
its current status.

ATTENTION IS THE ANSWER TO "WHICH SESSION NEEDS ME". It has THREE values and
they are spelled apart on purpose: "YES" a human should look now, "no" we
looked and it does not need anyone, "?" undetermined — the evidence supports
neither verdict. "?" is NOT a failure and it is NOT a no; it is the honest
answer, and it is the one column where undetermined is not written "-", because
a dash beside a "no" in the same column is read as another no. The WHY block
under the table says which rule reached each verdict.

TWO TIERS produce the STATUS, and TIER says which one this row came from.
"authoritative" is the session's own record of itself — its registry entry, its
job record and its transcript. "inferred" is what magmux worked out from the
pane's own traffic, and it is a guess made from the outside. Authoritative wins
whenever both have something to say. When they DISAGREE the loser is published
too, rather than being overruled in silence: the WHY block prints it as a
"conflict" line and --json carries it in a "conflict" field. A conflict is not
an error — both tiers were working correctly, and they disagree because the
session is in a state one of them cannot see.

BACKGROUND JOBS are rows too. A job is keyed by its job (--json carries it as
"job"), and a job's process and its job record are ONE row. A job no process
claims has pid 0. A blocked job is "waiting" and needs a human; a working one
is "busy"; a finished or failed one has no status magmux has a word for, so
STATUS is its own word in angle brackets and TIER says the job said it.

TWO INDEPENDENT GATES decide whether a session should be reported at all, and
they are shown separately because they fail separately: PROCESS is whether the
process is still there, PANE ALIVE is whether the magmux pane it was running in
is. GATE names whichever of them rejected the session, so that a dead process
and a closed pane — different situations, different remedies — do not arrive
as one word.

SUB AND ERR COME FROM THE SESSION'S OWN RECORD rather than from the screen. SUB
is how many subagents it has, counted from the tool's metadata files and never
from its transcript, which undercounts them — by 100% in the worst measured
case. ERR is how many API-level errors it has hit RECENTLY: within the last
200 KB of its transcript, which for a long session is its most recent part and
not all of it. When that window is only part of the transcript, --json's
fault.detail says how much was examined, on a 0 as well as on a count. A "-" in
either column is "nobody counted", which is NOT zero: it means no transcript
was found for that session — or that its id names transcripts in two project
directories, which is ambiguous and is never opened — so there was nothing to
count from. A "0" means magmux looked — which for SUB includes a session with
no subagent directory at all, because the tool creates that directory on the
first spawn and 94% of live sessions have never had one.

THE PANE COLUMNS ARE EMPTY HERE, and that is honest rather than broken: this
command is not a magmux and has no pane table to consult, so it answers the
pane questions "undetermined" instead of guessing. Inside magmux the same
pipeline fills them in by walking up from each session's process until it
reaches a pane's own child.

FOR THE SAME REASON, TIER IS ALWAYS "authoritative" HERE and no conflict can
ever appear: the inferred tier is made of a pane's traffic, and this process has
no panes. Run magmux with MAGMUX_AGENTS=1 to see both tiers weighed against each
other.

Options:
  --root DIR  the registry directory. Default: $CLAUDE_CONFIG_DIR/sessions,
              else ~/.claude/sessions
  --jobs DIR  the background-jobs directory. Default: $CLAUDE_CONFIG_DIR/jobs,
              else ~/.claude/jobs — but OFF when --root is given without it, so
              a staged registry is read on its own. --jobs "" turns it off
  --projects DIR
              the transcript directory. Default: $CLAUDE_CONFIG_DIR/projects,
              else ~/.claude/projects. SUB and ERR are read from here
  --json      emit the document as JSON on stdout instead of a table. One whole
              document per change, which is what encoding/json reads natively
  --once      print one document and exit. The default is to keep printing,
              driven by the filesystem: no polling, so an idle registry costs
              nothing at all
  --help      this

THE ONE ACTION IS `+"`magmux agents focus TARGET`"+`, which moves keyboard focus to the
magmux pane a session is running in and writes an audit record saying what it
did, to which session, why, and by which rule. Three further action tiers exist
in the schema — answering a permission prompt, sending input, and restarting a
session — and all three are hard-disabled: they cannot run even if a
configuration asks for them. See `+"`magmux agents focus --help`"+`.

IT ONLY READS. Nothing under ~/.claude is created, written, renamed or removed
by this command, not even a temporary file — those records belong to the Claude
Code processes writing them, and they rewrite them in place while this is
reading. The <pid>.<sha256>.key files beside them are not read either.

Reading a record that is being rewritten can catch it half-written. That is a
TORN READ, not a dead session: it is retried once, and if it fails again the
session keeps the state it had a moment ago and the problem is reported.

Anything that could not be read is reported on STDERR, so --json on stdout
stays machine-readable. A session that is absent because its record could not
be read is ABSENT, which is not the same as dead, and the report says which.

Undetermined values print as "-" and marshal as JSON null. That is never a
"no": a status nobody could determine and a session that is idle are different
answers, and this command will not collapse them. ATTENTION is the one column
that spells its undetermined value "?" instead, and it is spelled differently
for exactly that reason — see above.
`)
}

// runObsFocus parses `magmux agents focus`'s arguments and installs the real
// signal oracle. It returns a process exit code.
//
// # Why it is in THIS file and not beside the rest of the command
//
// Because it calls proclive.New(), and that may happen in exactly two files —
// this one and cli.go — which is not a convention but a rule enforced by
// reading the source (TestNoTestCanReachTheSignalOracle). The mechanics of the
// command live in obsfocus.go; the entry point lives beside the probe it
// installs.
//
// Everything that reaches a live machine — the Claude root, the liveness probe,
// the audit trail's real path and the socket discovery — is named HERE, in a
// function that is a program entry point, so that a test exercising the
// command cannot reach any of them by forgetting to override a default.
func runObsFocus(args []string) int {
	fs := flag.NewFlagSet("magmux agents focus", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printObsFocusHelp(os.Stderr) }

	trees := agentsTreeFlags(fs)
	auditPath := fs.String("audit", "", "audit trail file (default: ${XDG_STATE_HOME:-~/.local/state}/magmux/"+act.AuditFileName+")")
	asJSON := fs.Bool("json", false, "emit the audit records as JSON instead of prose")
	help := fs.Bool("help", false, "show this help")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printObsFocusHelp(os.Stdout)
			return 0
		}
		return 2
	}
	if *help {
		printObsFocusHelp(os.Stdout)
		return 0
	}
	switch fs.NArg() {
	case 1:
	case 0:
		fmt.Fprintf(os.Stderr, "magmux agents focus: name a session — a pid, a session id or a name\n")
		return 2
	default:
		fmt.Fprintf(os.Stderr, "magmux agents focus: one session at a time; got %d arguments\n", fs.NArg())
		return 2
	}

	// The same flags, by the same rule, as `magmux agents` (§4.9): a target
	// found nowhere is refused, and a background session cannot be found in a
	// jobs tree that was not read — obsFocus says so.
	root, err := trees.root(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents focus: %v\n", err)
		return 1
	}
	trail := *auditPath
	if trail == "" {
		resolved, err := act.DefaultAuditPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "magmux agents focus: %v\n", err)
			return 1
		}
		trail = resolved
	}

	// requirements.md R6.3 through the file loader. An absent or unparseable
	// config yields the default posture and says why on stderr.
	policy, err := loadObsPolicy()
	if err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents focus: %v\n", err)
	}
	// The user's `[notify]` block is DROPPED here, deliberately, and this is
	// the only place in the repository that drops part of a loaded policy.
	// Notifications belong to the long-running magmux; this command builds no
	// notify stage at all, and the drop keeps the policy it records honest
	// about what was in force.
	policy.Notify = act.Notify{}

	// The pane table, over the socket, and the live-machine seam named HERE
	// and nowhere else. newSockPaneLocator dials every magmux-*.sock it can
	// find, so a default reaching it from anywhere a test can get to would have
	// `go test .` talking to the developer's own running magmuxes.
	//
	// The socket directory first, through the same resolver magmux's own
	// --sock-dir and `magmux mcp` use. An unusable MAGMUX_SOCK_DIR is refused,
	// as `magmux mcp` refuses it: a scan of a directory nobody looked in would
	// report "no magmux running".
	sockDir, err := sockdir.Resolve("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents focus: %v\n", err)
		return 1
	}
	sockdir.Dir = sockDir
	panes := newSockPaneLocator(context.Background())

	return obsFocus(obsFocusConfig{
		Root:      root.Sessions.Path,
		Jobs:      root.Jobs.Path,
		Projects:  root.Projects.Path,
		Base:      root.Base,
		AuditPath: trail,
		Target:    fs.Arg(0),
		Oracle:    proclive.New(),
		Policy:    policy,
		Panes:     obsPaneTableOrNil(panes),
		JSON:      *asJSON,
		Out:       os.Stdout,
		Err:       os.Stderr,
	})
}

// obsPaneTableOrNil converts a possibly-nil *sockPaneLocator into a possibly
// -nil interface.
//
// It exists because a nil POINTER in a non-nil INTERFACE is not nil, and
// obsFocus branches on exactly that: `cfg.Panes != nil` would be true for a
// typed nil, the pipeline would be handed a locator, every PaneFor would
// dereference it, and the failure would be a panic in an actor goroutine
// rather than the honest "there is no pane table here".
func obsPaneTableOrNil(l *sockPaneLocator) obsPaneTable {
	if l == nil {
		return nil
	}
	return l
}
