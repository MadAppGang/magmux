package mux

// `magmux agents focus` — the one command in this layer that WRITES.
//
// # What it writes, and what it does not
//
// It writes one or two lines to magmux's own audit trail under
// ${XDG_STATE_HOME:-~/.local/state}/magmux/, and that is the whole of it.
// Nothing under ~/.claude is opened for writing, ever (NFR1); nothing is
// written to a socket under /tmp/cc-socks, because writing to one injects text
// into a live agent's prompt and magmux's own PTY injection is the only route
// for that; and no tmux command is run, least of all `run-shell` (NFR2).
//
// # It CAN focus one now, over the socket — phase 7 closed that gap
//
// A navigate needs a PANE, and the pane a session runs in is resolved by
// walking up the process tree until it meets a pane's own pid. This process is
// not a magmux and has no pane table of its own, so phase 6 shipped it
// refusing with act.ErrNotCapable — "cannot", not "will not".
//
// What was missing was never a socket VERB. `list` is buildPaneResults
// verbatim and publishes each pane's id AND its child's pid
// (mux/grid.go's buildPaneResultsLocked, `entry["pid"] = c.Process.Pid`), that pid is the SHELL's — which
// is exactly what the ancestry walk walks TOWARDS — and the `focus` verb
// already takes a pane id. What was missing was a CLIENT, and obssock.go is
// it: discover every reachable magmux, build the pane table out of the
// connect-time aggregate each one sends, walk, and focus.
//
// An earlier revision of this comment said a new verb was needed. That was
// wrong, and wrong in a way worth naming — it reasoned from the verb's name
// rather than from buildPaneResults, which is the single builder behind
// `list`, `snapshot` and `results` and answers the question in one place. The
// same mistake was made once before in this project, about the pane column in
// phase 3.
//
// With NO magmux running, the old behaviour stands and is still the honest
// one: no pane table, no capability, and a recorded refusal. The route that
// never needed a socket is the same pipeline inside a magmux, where the pane
// table is right there — see Magmux.obsFocusSession.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/navigate"
	"github.com/MadAppGang/magmux/obsview"
)

// obsPaneTable is the pane half of what `magmux agents focus` needs: resolve a
// session to a pane, and move focus to it. Both ports at once, because a
// process either has a pane table or it does not — see obsFocusConfig.Panes.
//
// sockPaneLocator is the one implementation; a test supplies its own.
type obsPaneTable interface {
	ports.PaneLocator
	navigate.PaneFocuser
}

// obsPaneBinder is the optional half of a pane table that spans MORE THAN ONE
// magmux, discovered by type assertion the way InputNotifier is beside
// ToolController.
//
// Such a table can locate a session's pane — the walk reaches a globally
// unique pid — and cannot focus one by id alone, because every magmux has a
// pane 0. bindToSession narrows it to the one magmux the session's ancestry
// reaches, and returns nil when there is none. sockPaneLocator is the one
// implementation; see forSession.
type obsPaneBinder interface {
	bindToSession(pid int) obsPaneTable
}

// obsFocusConfig is everything `magmux agents focus` needs, with every live-machine
// seam as a field. See obsConfig, which is the same idea for the same reason.
type obsFocusConfig struct {
	// Root is the session registry directory. Required.
	Root string
	// Jobs is the background-jobs tree, or "" for none — and then a target
	// that is found nowhere is refused with a word that the jobs source was
	// off, because a background session cannot be found in a tree that was
	// not read.
	Jobs string
	// Projects is the transcript tree, or "" for none.
	Projects string
	// Base is the Claude root when the trees are its subdirectories; unused by
	// a one-shot read, kept so the Root this command builds is the one
	// `magmux agents` builds for the same flags.
	Base string
	// AuditPath is the trail. EMPTY MEANS NO TRAIL, and no trail means the
	// gate refuses every action with act.ErrUnaudited rather than acting
	// unrecorded — which is what a test that forgot to point it at a
	// temporary directory should get, instead of an append to the
	// developer's own trail.
	AuditPath string
	// Target names the session: a pid, a session id or a prefix of one, or a
	// name.
	Target string
	// Oracle answers the process-liveness gate. nil leaves it undetermined;
	// see obsConfig.Oracle, and TestNoTestCanReachTheSignalOracle, which is
	// what makes that rule structural.
	Oracle ports.ProcessOracle
	// Policy is the posture. THE ZERO VALUE REFUSES EVERYTHING.
	Policy act.Policy
	// Panes is the pane table, reached over the socket. nil means this
	// process cannot see any pane and therefore cannot focus one — which is
	// the honest answer on a machine with no magmux running, and the answer
	// every test that does not stage one should get rather than a client that
	// dials whatever sockets happen to be in /tmp.
	//
	// It is an interface rather than a *sockPaneLocator so that a test can
	// supply a table without a socket at all; newSockPaneLocator is named by
	// runObsFocus and by nothing else.
	Panes obsPaneTable
	// JSON emits the audit records instead of prose.
	JSON bool

	Out io.Writer
	Err io.Writer
}

// obsFocus resolves one session and asks the gate to focus it.
//
// It runs the SAME pipeline the read path runs — one cycle, no watch, so every
// listed job directory is read (C18) — because the fact set a decision is made
// against, and recorded against, has to be the one a person could have been
// looking at. A shortcut that read the registry directly would decide against
// facts no document ever carried.
func obsFocus(cfg obsFocusConfig) int {
	out, errw := cfg.Out, cfg.Err
	if out == nil {
		out = io.Discard
	}
	if errw == nil {
		errw = io.Discard
	}

	var auditor act.Auditor
	if cfg.AuditPath != "" {
		w, err := act.NewWriter(act.WriterConfig{Path: cfg.AuditPath})
		if err != nil {
			focusLine(errw, "magmux agents focus: %v", err)
			return 1
		}
		auditor = w
	}

	// The locator and the focuser come from the SAME table or neither.
	// Passing a focuser without a locator would be worse than passing neither
	// — focus needs a pane, and nothing could resolve one — so a nil table
	// sets neither.
	var locator ports.PaneLocator
	var focuser navigate.PaneFocuser
	if cfg.Panes != nil {
		locator, focuser = cfg.Panes, cfg.Panes
	}

	// A table spanning several magmuxes cannot focus by id — every magmux has
	// a pane 0 — so the session is named FIRST, on a pipeline that can act on
	// nothing, and the table is narrowed to the magmux that session's own
	// ancestry reaches. Only then is the pipeline that acts built, with the
	// narrowed table as its focuser. See forSession.
	//
	// The KEY named on that first read is the only identity the second read
	// and the action use (D12): never the user's words a second time, because
	// a name, a prefix or even an exact id could resolve to a different
	// session in a registry that moved between the two reads, and the focuser
	// was bound to THIS one's magmux.
	binder, binds := cfg.Panes.(obsPaneBinder)
	var targetKey model.Key
	if binds {
		d, err := obsFocusDocument(cfg, locator)
		if err != nil {
			focusLine(errw, "magmux agents focus: %v", err)
			return 1
		}
		named, err := resolveObsTarget(d.Doc, cfg.Target)
		if err != nil {
			writeFocusProblems(errw, d)
			focusLine(errw, "magmux agents focus: %v", explainNotFound(cfg, err))
			return 1
		}
		targetKey = named.Key
		// nil when the session is in no magmux on this machine. The navigate
		// stage then declares nothing and the gate records "cannot", which is
		// the same honest answer a machine with no magmux gets.
		focuser = nil
		if one := binder.bindToSession(named.PID); one != nil {
			focuser = one
		}
	}

	// The locator is the WHOLE table even when the focuser is narrowed: the
	// pane facts in this document are about every session, and a locator that
	// knew one magmux would call every other session determined-absent from a
	// pane it is sitting in. No Activity port either way: the inferred tier is
	// made of a pane's own traffic and the socket carries none of it.
	p, d, err := obsFocusPipeline(cfg, locator, focuser)
	if err != nil {
		focusLine(errw, "magmux agents focus: %v", err)
		return 1
	}
	defer p.Stop()
	writeFocusProblems(errw, d)

	var target model.Session
	if binds {
		found := false
		for _, s := range d.Doc.Sessions {
			if s.Key == targetKey {
				target, found = s, true
				break
			}
		}
		if !found {
			focusLine(errw, "magmux agents focus: session %s is no longer observed", targetKey)
			return 1
		}
	} else {
		target, err = resolveObsTarget(d.Doc, cfg.Target)
		if err != nil {
			focusLine(errw, "magmux agents focus: %v", explainNotFound(cfg, err))
			return 1
		}
	}

	acts := newObsActions(cfg.Policy, auditor, nil, time.Now)
	report, doErr := acts.do(context.Background(), p, act.Request{
		Action: act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate, Session: target.Key.SessionID},
		By:     act.RequesterCLI,
	}, exactKey(target.Key))

	if cfg.JSON {
		writeAuditJSON(out, report.Records)
	} else {
		writeFocusReport(out, target, report)
	}
	if auditor != nil {
		if w, isWriter := auditor.(*act.Writer); isWriter {
			focusLine(out, "audit trail: %s", w.Path())
		}
	}
	if doErr != nil {
		// A refusal is a failure and exits non-zero. A caller that asked for
		// something and got exit 0 without it happening cannot tell that from
		// success — the same rule the gate applies to its own return value,
		// applied to the process's.
		focusLine(errw, "magmux agents focus: %v", doErr)
		return 1
	}
	return 0
}

// writeFocusProblems prints a document's problems, through the display rule.
func writeFocusProblems(w io.Writer, d pipeline.Delivery) {
	for _, pr := range d.Problems {
		focusLine(w, "magmux agents focus: %s", pr.Error())
	}
}

// explainNotFound adds the one thing a "no such session" refusal must say when
// the jobs tree was not read: a background session lives there, and could not
// have been found (§4.9).
func explainNotFound(cfg obsFocusConfig, err error) error {
	if errors.Is(err, errNoSuchTarget) && cfg.Jobs == "" {
		return fmt.Errorf("%w (the background-jobs source was off — --root without --jobs, or --jobs \"\" — "+
			"so a background session could not be found; pass --jobs DIR to read it)", err)
	}
	return err
}

// obsFocusDocument reads ONE document through a pipeline that can act on
// nothing — no navigate stage, no audit trail — and stops it. It is the first
// of obsFocus's two reads, used only to name the session.
func obsFocusDocument(cfg obsFocusConfig, locator ports.PaneLocator) (pipeline.Delivery, error) {
	p, d, err := obsFocusPipelineWith(cfg, locator, nil)
	if err != nil {
		return pipeline.Delivery{}, err
	}
	p.Stop()
	return d, nil
}

// obsFocusPipeline starts the command's acting pipeline — with the navigate
// stage over focuser — and returns it together with its first document. The
// caller stops it.
func obsFocusPipeline(cfg obsFocusConfig, locator ports.PaneLocator, focuser navigate.PaneFocuser) (*pipeline.Pipeline, pipeline.Delivery, error) {
	return obsFocusPipelineWith(cfg, locator, []stage.Factory{navigate.Factory(navigate.Config{Focuser: focuser})})
}

// obsFocusPipelineWith is everything obsFocus's two reads share, so they cannot
// differ in anything but the locator and the chain: the same trees, the same
// oracle, the same transcript stage.
func obsFocusPipelineWith(cfg obsFocusConfig, locator ports.PaneLocator, chain []stage.Factory) (*pipeline.Pipeline, pipeline.Delivery, error) {
	root, err := claudeRoot(cfg.Root, cfg.Jobs, cfg.Projects, cfg.Base)
	if err != nil {
		return nil, pipeline.Delivery{}, err
	}
	p, err := pipeline.Start(pipeline.Config{
		Root:    root,
		Oracle:  cfg.Oracle,
		Locator: locator,
		Chain:   chain,
		// No watch: one document is all this needs, a watcher opens a
		// descriptor per record on Darwin, and unwatched every listed job
		// directory is read (C18).
		Watch: false,
		Clock: ports.RealClock(),
		Now:   time.Now,
	})
	if err != nil {
		return nil, pipeline.Delivery{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), obsFocusReadBound)
	defer cancel()
	sub, err := p.Subscribe(ctx, "focus", 1)
	if err != nil {
		p.Stop()
		return nil, pipeline.Delivery{}, err
	}
	// Subscribe first, then ask. Nothing is published until something asks,
	// and the other order races the answer.
	p.Refresh()
	select {
	case d, ok := <-sub.C():
		if !ok {
			p.Stop()
			return nil, pipeline.Delivery{}, fmt.Errorf("the pipeline stopped before it read anything")
		}
		return p, d, nil
	case <-ctx.Done():
		p.Stop()
		return nil, pipeline.Delivery{}, fmt.Errorf("no document within %s", obsFocusReadBound)
	}
}

// obsFocusReadBound bounds the one read: a cycle reads a few hundred small
// files, so this is a backstop against a wedged filesystem, never a budget.
const obsFocusReadBound = 30 * time.Second

// errNoSuchTarget is the refusal for a target that matches no session.
var errNoSuchTarget = errors.New("no session matches")

// resolveObsTarget finds the one session a target string names.
//
// Four ways, in order, and the first that matches ANY session decides which
// kind of match this is: a pid, an exact session id, a session-id prefix, or
// an exact name. An ambiguous target is an ERROR listing the candidates —
// every pid, and every job short — rather than a pick: acting on the wrong
// session of two is the failure this whole layer exists to avoid, and since
// the library a session id names two sessions whenever two processes run it,
// or a job was resumed as another job (D12).
//
// A pid matches a session's PID or any of its claimants, so the process a
// person can see in `ps` reaches the job it serves even when another claimant
// speaks. A pid of 0 names no process: it is what a job no registry record
// claims carries, and matching it would match every such job.
func resolveObsTarget(fleet model.Fleet, target string) (model.Session, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return model.Session{}, fmt.Errorf("no session named")
	}

	if pid, err := strconv.Atoi(t); err == nil {
		if pid <= 0 {
			return model.Session{}, fmt.Errorf("pid %d names no process; name the session by its id or its name", pid)
		}
		var hits []model.Session
		for _, s := range fleet.Sessions {
			if s.PID == pid || containsInt(s.Claimants, pid) {
				hits = append(hits, s)
			}
		}
		return oneOf(hits, t, "pid")
	}

	var exact []model.Session
	for _, s := range fleet.Sessions {
		if s.Key.SessionID == t {
			exact = append(exact, s)
		}
	}
	if len(exact) > 0 {
		return oneOf(exact, t, "session id")
	}

	const minPrefix = 6
	if len(t) >= minPrefix {
		var hits []model.Session
		for _, s := range fleet.Sessions {
			if strings.HasPrefix(s.Key.SessionID, t) {
				hits = append(hits, s)
			}
		}
		if len(hits) > 0 {
			return oneOf(hits, t, "session id prefix")
		}
	}

	var named []model.Session
	for _, s := range fleet.Sessions {
		if s.Name == t {
			named = append(named, s)
		}
	}
	if len(named) > 0 {
		return oneOf(named, t, "name")
	}

	return model.Session{}, fmt.Errorf("%w %q — %d sessions were observed; "+
		"run `magmux agents --once` to see them", errNoSuchTarget, t, len(fleet.Sessions))
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// oneOf insists on exactly one match.
func oneOf(hits []model.Session, target, kind string) (model.Session, error) {
	switch len(hits) {
	case 0:
		return model.Session{}, fmt.Errorf("%w %s %q", errNoSuchTarget, kind, target)
	case 1:
		return hits[0], nil
	}
	return model.Session{}, ambiguityError(kind, target, hits)
}

// writeFocusReport prints what happened, for a person.
//
// The DECISION is printed whether or not the action ran, because "why" is the
// question a refusal raises and the audit record answers it in the same words.
func writeFocusReport(out io.Writer, target model.Session, report act.Report) {
	job := ""
	if target.Key.Job != "" {
		job = ", job " + target.Key.Job
	}
	focusLine(out, "session %s (pid %d%s%s)", target.Key.SessionID, target.PID, job, focusPaneText(target))
	if report.Decision.Rule != "" {
		focusLine(out, "policy: %s", report.Decision.Rule)
	}
	if report.Result.Done {
		focusLine(out, "done: %s", report.Result.Detail)
	}
	for _, r := range report.Records {
		focusLine(out, "audit: %s", auditLineText(r))
	}
}

// auditLineText is one audit record in one line of prose. The JSON is the
// record; this is the reading of it.
func auditLineText(r act.Record) string {
	s := fmt.Sprintf("#%d %s %s/%s", r.Seq, r.Event, r.Tier, r.Verb)
	if r.Outcome != nil {
		switch {
		case r.Outcome.Refused:
			s += " — refused: " + r.Outcome.Error
		case r.Outcome.Error != "":
			s += " — failed: " + r.Outcome.Error
		case r.Outcome.Detail != "":
			s += " — " + r.Outcome.Detail
		}
	}
	return s
}

// writeAuditJSON emits the records, one whole object per line, which is the
// shape the trail itself has.
//
// A record names its target session, and that name is registry text, so the
// output goes through TerminalSafeJSON on the same terms as `magmux agents
// --json`: every string exact, and no DEL or C1 character raw on a terminal.
func writeAuditJSON(out io.Writer, records []act.Record) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range records {
		buf.Reset()
		if err := enc.Encode(r); err != nil {
			focusLine(os.Stderr, "magmux agents focus: encoding an audit record: %v", err)
			continue
		}
		if _, err := out.Write(obsview.TerminalSafeJSON(buf.Bytes())); err != nil {
			focusLine(os.Stderr, "magmux agents focus: writing an audit record: %v", err)
			return
		}
	}
}

func focusPaneText(s model.Session) string {
	id, ok := obsview.PaneID(s.Pane)
	if !ok {
		return ""
	}
	return fmt.Sprintf(", magmux pane %d", id)
}

func printObsFocusHelp(w io.Writer) {
	fmt.Fprint(w, `magmux agents focus — focus the magmux pane a session is running in

Usage: magmux agents focus [--root DIR] [--jobs DIR] [--projects DIR] [--audit FILE] [--json] TARGET

TARGET names one session: its pid, its session id or an unambiguous prefix of
one, or its name. A pid also reaches a background job through any process that
claims it. A target that matches more than one session is refused with the
candidates listed — every pid, and every job — rather than picked between: two
processes running one session id are two sessions.

THIS IS THE ONLY ACTION MAGMUX WILL TAKE ON A SESSION. Three further action
tiers exist in the schema — answering a permission prompt, sending input, and
lifecycle (restart, kill, resume) — and all three are HARD-DISABLED: they
cannot run even if a configuration asks for them, and asking produces a refusal
that says which requirement defers them. The reason is two recorded incidents
in which 16 live sessions were destroyed, and the fact that lifecycle is
irreversible.

FOCUS NEEDS A PANE, AND THIS PROCESS FINDS ONE OVER THE SOCKET. `+"`magmux agents`"+`
is not a magmux and has no pane table of its own, so it asks every magmux
running on this machine for theirs, walks up from the session's process until
it reaches one of their panes, and focuses that. Nothing is injected and no
process is signalled: a pane is focused, which is what R5.1 means.

WITH NO MAGMUX RUNNING there is no pane to focus and the command says so rather
than failing quietly. The refusal is "cannot", not "will not", and it is
recorded in the audit trail like every other outcome.

EVERY ACTION AND EVERY REFUSAL IS RECORDED, in
${XDG_STATE_HOME:-~/.local/state}/magmux/`+act.AuditFileName+`. One JSON object
per line, appended, never rewritten. An authorised action writes two lines —
one before it runs and one after — so that a crash mid-action leaves a record
saying "we tried this and do not know what happened", which is the honest state
and the one an operator needs. A refusal writes one.

NOTHING IS WRITTEN UNDER ~/.claude, not even a log. Those records belong to the
Claude Code processes writing them. The trail lives in magmux's own state
directory, and a path under ~/.claude is refused outright rather than trusted
not to be passed.

Options:
  --root DIR  the registry directory. Default: $CLAUDE_CONFIG_DIR/sessions,
              else ~/.claude/sessions
  --jobs DIR  the background-jobs directory, by the same rule as magmux agents:
              off when --root is given without it, and a target found nowhere
              then says so
  --projects DIR
              the transcript directory
  --audit FILE
              the audit trail. Default:
              ${XDG_STATE_HOME:-~/.local/state}/magmux/`+act.AuditFileName+`
  --json      emit the audit records as JSON, one object per line
  --help      this

Exit status is 0 only when the action happened. A refusal exits 1: a caller
that asked for something and got exit 0 without it happening cannot tell that
from success.
`)
}

// focusLine prints ONE line of this command's output, through
// obsview.DisplaySafe, the one rule for registry text reaching a person
// (review r1 #7). Every line goes through it, because almost every line
// carries registry text: a session id (which the decoder does not validate),
// a job short, a candidate list in an ambiguity refusal, an outcome that can
// quote a remote magmux's socket error. DisplaySafe also turns a newline into
// a space, so a hostile value cannot forge a line of its own.
// TestEveryAgentsFocusLineGoesThroughTheDisplayRule keeps it the only way.
func focusLine(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, obsview.DisplaySafe(fmt.Sprintf(format, args...)))
}
