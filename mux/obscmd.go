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

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/claudecode"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/obsview"
	"github.com/MadAppGang/magmux/sockdir"
)

// `magmux agents` — print what every Claude Code session on this machine is
// doing, and keep printing it as they change.
//
// # It only ever READS ~/.claude, and that is a hard rule rather than a habit
//
// The registry directory belongs to the Claude Code processes writing it —
// thirty of them on the machine this was built on — and they rewrite their
// records IN PLACE while we are reading. So this command opens files and stats
// them, and creates, writes, renames and removes nothing at all, not even a
// temporary file. The `<pid>.<sha256>.key` siblings are not read either: they
// are named like key material and nothing here needs them.
//
// # This file DEFINES the signal oracle, and exactly two places INSTALL it
//
// internal/obs/registry takes the liveness answer through a port, and a nil
// oracle there means "undetermined" rather than a live probe — so a caller
// that does not inject one gets an honest non-answer instead of sending
// signals to whatever pids happen to exist on the machine running it. The real
// probe lives HERE, on magmux's side of the port, next to
// `sockdir.KillErrMeansGone` (sockdir/sockdir.go), which already owns the errno classification and already has
// the test that pins its load-bearing branch.
//
// `obsOracle{}` is NAMED in exactly two places, both of them a program entry
// point: `RunAgents` below, and the `mux.startObservationIfEnabled(obsOracle{})`
// call in Main (cli.go). It is a parameter there rather than a default because the
// alternative was measured and was wrong — four tests reached a default oracle
// and sent kill(pid, 0) to whatever pid their temporary registry named, which
// testdata/PROVENANCE.md rule 2 forbids outright. Making it an argument turns
// that rule from a habit into something a test cannot break without naming the
// type, and TestNoTestCanReachTheSignalOracle fails when one does.

// obsOracle answers the registry's liveness question with kill(pid, 0), which
// SENDS NOTHING: signal 0 performs the permission and existence checks and
// stops there.
//
// Four answers, not two, which is why [obs.Liveness] is tri-state:
//
//	nil    the process exists                      -> yes
//	ESRCH  no such process                         -> no
//	EPERM  it exists and belongs to another user   -> yes
//	other  the oracle could not tell               -> undetermined
//
// The EPERM row is the one a bool-shaped helper cannot express, and getting it
// wrong is not hypothetical: treating EPERM as "gone" once had magmux's socket
// reaper unlink other users' LIVE sockets. The ESRCH classification is
// therefore delegated to `sockdir.KillErrMeansGone` rather than rewritten here —
// see killErrMeansGone's comment in sockdir/sockdir.go for why that branch is
// split out at all, and TestKillErrMeansGone for the only way it can be
// asserted without a setuid test binary.
type obsOracle struct{}

func (obsOracle) Alive(pid int) obs.Liveness {
	if pid <= 0 {
		return obs.LivenessUnknown
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return obs.LivenessYes
	case sockdir.KillErrMeansGone(err):
		return obs.LivenessNo
	case errors.Is(err, syscall.EPERM):
		return obs.LivenessYes
	default:
		return obs.LivenessUnknown
	}
}

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

	root := fs.String("root", "", "registry directory (default: $CLAUDE_CONFIG_DIR/sessions, else ~/.claude/sessions)")
	projects := fs.String("projects", "", "transcript directory (default: $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects)")
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

	dir := *root
	if dir == "" {
		resolved, err := defaultRegistryRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "magmux agents: %v\n", err)
			return 1
		}
		dir = resolved
	}

	// The transcript tree. A machine with no Claude Code on it has none, and
	// that is not an error: the middleware built with an empty root adds
	// nothing at all and every harness-derived column prints "-", which is the
	// honest report rather than a failure to start.
	transcripts := *projects
	if transcripts == "" {
		if resolved, err := defaultProjectsRoot(); err == nil {
			transcripts = resolved
		}
	}

	// The whole pipeline, not just the source: the document this prints is the
	// one an actor's middleware chain produced, published through the hub and
	// received here as an ordinary sink. That is deliberate — the command is
	// the only place a person can see the layer working, so it must exercise
	// the same path magmux does rather than a shortcut around it.
	//
	// With ONE honest difference: no PaneLocator and no PaneActivity. This
	// process is not a magmux and has no pane table, so the pane questions are
	// answered "undetermined" rather than guessed at — and the INFERRED tier,
	// which is made of nothing but a pane's own traffic, has no input here at
	// all. Every row therefore resolves on the authoritative tier and no
	// conflict can arise, which is the truth about this process rather than a
	// degradation of it. See printObsHelp.
	p, err := pipeline.Start(pipeline.Config{
		Root:   dir,
		Oracle: obsOracle{},
		// The Claude Code middleware: the session's OWN record of what it did.
		// Everything upstream of it is terminal-derived — a guess about a
		// session made from the outside — while the colour, the error count
		// and the subagent count come from the harness itself.
		//
		// No LaunchDirs: this process spawned nothing, so it holds no launch
		// directory and every transcript is located by scanning for its
		// session id. That is correct, and for a session that moved after
		// startup it is the only thing that is.
		Chain: []pipeline.Factory{claudecode.Middleware(claudecode.Config{Root: transcripts})},
		// A watch only when streaming. --once has nothing to wait for, and a
		// watcher opens a descriptor per record on Darwin.
		Watch: !*once,
		// Now is separate from the Clock on purpose: the clock's job is to
		// create timers, and an epoch is a fact about this READ rather than
		// about any session. See registry.Config.Now.
		Now: time.Now,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents: %v\n", err)
		return 1
	}

	sub, err := p.Subscribe("stdout", 0)
	if err != nil {
		p.Stop()
		fmt.Fprintf(os.Stderr, "magmux agents: %v\n", err)
		return 1
	}

	code := consumeObs(p, sub, *once, *asJSON)

	// Stats BEFORE Stop, because Stop takes the hub with it and a stopped hub
	// has nothing to report. They go to stderr with the problems, for the same
	// reason: --json on stdout stays machine-readable.
	reportObsStats(os.Stderr, p.Stats())
	p.Stop()
	return code
}

// consumeObs is the sink: it prints every document the hub delivers until the
// process is signalled, or until the first one when --once was asked for.
//
// There is no poll and no ticker here or anywhere beneath it: an idle registry
// costs a few blocked goroutines and nothing else. The measured event rate on
// a busy machine was three rewrites in nine minutes across 32 sessions, where
// a poller tuned to catch a thirteen-second gap would wake about forty times
// per rewrite to find nothing.
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
				// The hub stopped underneath us. Not reachable from here —
				// this goroutine is the only thing that stops it — so it is an
				// error rather than a quiet exit.
				fmt.Fprintf(os.Stderr, "magmux agents: the pipeline stopped unexpectedly\n")
				return 1
			}
			// Problems go to STDERR so that --json on stdout stays machine
			// readable, and they are printed rather than counted because a
			// source that silently skips a record it could not read is
			// indistinguishable from a machine with fewer sessions on it.
			for _, pr := range d.Problems {
				fmt.Fprintf(os.Stderr, "magmux agents: %s\n", pr.Error())
			}
			if d.Dropped > 0 {
				// Said out loud rather than swallowed: the document below is
				// still correct — it is rebuilt from the directory, not from
				// the events — but a consumer counting changes would be short.
				fmt.Fprintf(os.Stderr, "magmux agents: %d documents were dropped before this one\n", d.Dropped)
			}
			if asJSON {
				obsview.WriteFleetJSON(os.Stdout, d.Doc)
			} else {
				obsview.WriteFleetTable(os.Stdout, d.Doc)
			}
			if once {
				return 0
			}
		case <-quit:
			return 0
		}
	}
}

// reportObsStats prints what the pipeline lost, and to whom.
//
// Only the losses. A run where nothing was dropped prints nothing at all,
// because a line saying "0 dropped" on every exit trains a reader to skip the
// place the real number will appear.
func reportObsStats(w io.Writer, st pipeline.Stats) {
	if st.WatchDropped > 0 {
		fmt.Fprintf(w, "magmux agents: %d change notifications were dropped by the watcher\n", st.WatchDropped)
	}
	if st.WatchErrors > 0 {
		fmt.Fprintf(w, "magmux agents: the watcher reported %d errors\n", st.WatchErrors)
	}
	if st.ObservationsDropped > 0 {
		fmt.Fprintf(w, "magmux agents: %d observations were dropped by a busy session actor\n", st.ObservationsDropped)
	}
	if st.DuplicateSessions > 0 {
		fmt.Fprintf(w, "magmux agents: %d records claimed a session identity another record already had\n",
			st.DuplicateSessions)
	}
	for _, s := range st.Subscribers {
		if s.Dropped == 0 {
			continue
		}
		fmt.Fprintf(w, "magmux agents: the %q sink missed %d of %d documents\n",
			s.Name, s.Dropped, s.Dropped+s.Delivered)
	}
}

// defaultRegistryRoot is where Claude Code keeps its session registry.
//
// CLAUDE_CONFIG_DIR is Claude Code's own override and takes precedence, for
// the same reason magmux honours MAGMUX_SOCK_DIR: a user who has moved the
// directory has moved it for everything that reads it.
func defaultRegistryRoot() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory, so cannot find the session "+
			"registry; pass --root: %w", err)
	}
	return filepath.Join(home, ".claude", "sessions"), nil
}

// defaultProjectsRoot is where Claude Code keeps its transcripts, on the same
// terms as defaultRegistryRoot: CLAUDE_CONFIG_DIR wins, because a user who has
// moved the directory has moved it for everything that reads it.
func defaultProjectsRoot() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory, so cannot find the "+
			"transcripts; pass --projects: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

func printObsHelp(w io.Writer) {
	fmt.Fprint(w, `magmux agents — what every Claude Code session on this machine is doing

Usage: magmux agents [--root DIR] [--projects DIR] [--json] [--once]
       magmux agents focus TARGET          (see magmux agents focus --help)

Reads Claude Code's session registry and prints one document describing every
session it can see: status, what a waiting session is blocked on, whether the
process is still alive, and how long it has been in its current status.

ATTENTION IS THE ANSWER TO "WHICH SESSION NEEDS ME". It has THREE values and
they are spelled apart on purpose: "YES" a human should look now, "no" we
looked and it does not need anyone, "?" undetermined — the evidence supports
neither verdict. "?" is NOT a failure and it is NOT a no; it is the honest
answer, and it is the one column where undetermined is not written "-", because
a dash beside a "no" in the same column is read as another no. The WHY block
under the table says which rule reached each verdict.

TWO TIERS produce the STATUS, and TIER says which one this row came from.
"authoritative" is the session's own record of itself — its registry entry and
its transcript. "inferred" is what magmux worked out from the pane's own
traffic, and it is a guess made from the outside. Authoritative wins whenever
both have something to say. When they DISAGREE the loser is published too,
rather than being overruled in silence: the WHY block prints it as a "conflict"
line and --json carries it in a "conflict" field. A conflict is not an error —
both tiers were working correctly, and they disagree because the session is in
a state one of them cannot see.

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
was found for that session, so there was nothing to count from. A "0" means
magmux looked — which for SUB includes a session with no subagent directory at
all, because the tool creates that directory on the first spawn and 94% of live
sessions have never had one.

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
// Because it names obsOracle, and obsOracle may be named in exactly two files
// — this one and cli.go — which is not a convention but a rule enforced by
// reading the source. TestNoTestCanReachTheSignalOracle caught this function
// in obsfocus.go and refused it, which is the guard working: a third site is
// meant to be a deliberate edit to that test's own table, and this one did not
// need to be. The mechanics of the command live in obsfocus.go; the entry
// point lives beside the probe it installs.
//
// The work is in obsFocus, in obsfocus.go. The split is the same one startObservation
// and startObservationWith make, and for the same reason: everything that
// reaches a live machine — the registry root, the transcript tree, the
// liveness probe and the audit trail's real path — is named HERE, in a
// function that is a program entry point, so that a test exercising the
// command cannot reach any of them by forgetting to override a default.
func runObsFocus(args []string) int {
	fs := flag.NewFlagSet("magmux agents focus", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printObsFocusHelp(os.Stderr) }

	root := fs.String("root", "", "registry directory (default: $CLAUDE_CONFIG_DIR/sessions, else ~/.claude/sessions)")
	projects := fs.String("projects", "", "transcript directory (default: $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects)")
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

	dir := *root
	if dir == "" {
		resolved, err := defaultRegistryRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "magmux agents focus: %v\n", err)
			return 1
		}
		dir = resolved
	}
	transcripts := *projects
	if transcripts == "" {
		if resolved, err := defaultProjectsRoot(); err == nil {
			transcripts = resolved
		}
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
	//
	// Notifications belong to the long-running magmux. This command starts a
	// pipeline, reads one document and exits; with the block in force it would
	// decide a notification for every session that qualified, and a person who
	// ran it twice would be notified twice about sessions they were already
	// being notified about. "Thirty sessions firing unfiltered" is the failure
	// R4.4 exists to prevent, and a CLI that joins in is a second copy of it.
	//
	// It is dropped rather than left to fail quietly on the absent port: the
	// decision would still be MADE, and each one would write a record saying
	// nothing could deliver it, so a `magmux agents focus` would leave audit
	// lines about sessions it was not asked about.
	policy.Notify = act.Notify{}

	// The pane table, over the socket, and the fifth live-machine seam named
	// HERE and nowhere else. newSockPaneLocator dials every magmux-*.sock it
	// can find, so a default reaching it from anywhere a test can get to would
	// have `go test .` talking to the developer's own running magmuxes.
	//
	// nil when nothing is reachable, and that is the honest answer rather than
	// an error: a machine with no magmux running is the ordinary case, and the
	// gate then refuses the navigate with "nothing can do this", which is
	// true.
	//
	// The socket directory first, through the same resolver magmux's own
	// --sock-dir and `magmux mcp` use. Nothing sets sockdir.Dir at package
	// init any more, so without this the discovery would scan the default
	// directory while the user's magmuxes are bound under MAGMUX_SOCK_DIR. An
	// unusable MAGMUX_SOCK_DIR is refused, as `magmux mcp` refuses it: a scan
	// of a directory nobody looked in would report "no magmux running".
	sockDir, err := sockdir.Resolve("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "magmux agents focus: %v\n", err)
		return 1
	}
	sockdir.Dir = sockDir
	panes := newSockPaneLocator(context.Background())

	return obsFocus(obsFocusConfig{
		Root:      dir,
		Projects:  transcripts,
		AuditPath: trail,
		Target:    fs.Arg(0),
		Oracle:    obsOracle{},
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
