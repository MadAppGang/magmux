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
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/claudecode"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/registry"
	"github.com/MadAppGang/magmux/obsview"
)

// obsPaneTable is the pane half of what `magmux agents focus` needs: resolve a
// session's pid to a pane, and move focus to it. Both ports at once, because a
// process either has a pane table or it does not — see obsFocusConfig.Panes.
//
// sockPaneLocator is the one implementation; a test supplies its own.
type obsPaneTable interface {
	obs.PaneLocator
	obs.PaneFocuser
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
	// Projects is the transcript tree, or "" for none.
	Projects string
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
	Oracle registry.ProcessOracle
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
// It runs the SAME pipeline the read path runs — one cycle, no watch — because
// the fact set a decision is made against, and recorded against, has to be the
// one a person could have been looking at. A shortcut that read the registry
// directly would decide against facts no document ever carried.
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
			fmt.Fprintf(errw, "magmux agents focus: %v\n", err)
			return 1
		}
		auditor = w
	}

	// The locator and the focuser come from the SAME table or neither.
	// Passing a focuser without a locator would be worse than passing neither
	// — focus needs a pane id, and nothing could resolve one — so a nil table
	// sets neither.
	var locator obs.PaneLocator
	var focuser obs.PaneFocuser
	if cfg.Panes != nil {
		locator, focuser = cfg.Panes, cfg.Panes
	}

	// A table spanning several magmuxes cannot focus by id — every magmux has
	// a pane 0 — so the session is named FIRST, on a pipeline that can act on
	// nothing, and the table is narrowed to the magmux that session's own
	// ancestry reaches. Only then is the pipeline that acts built, with the
	// narrowed table as its focuser. See forSession, and fix round 1's HIGH
	// finding: before this, a pane found unambiguously through a unique pid
	// was refused as ambiguous whenever a second magmux was running.
	//
	// Two reads of the registry rather than a focuser that is re-pointed after
	// the pipeline has started: every port here is read-only once built, and
	// the second read costs milliseconds in a command that runs once.
	binder, binds := cfg.Panes.(obsPaneBinder)
	targetID := ""
	if binds {
		d, err := obsFocusDocument(cfg, locator)
		if err != nil {
			fmt.Fprintf(errw, "magmux agents focus: %v\n", err)
			return 1
		}
		named, err := resolveObsTarget(d.Doc, cfg.Target)
		if err != nil {
			for _, pr := range d.Problems {
				fmt.Fprintf(errw, "magmux agents focus: %s\n", pr.Error())
			}
			fmt.Fprintf(errw, "magmux agents focus: %v\n", err)
			return 1
		}
		targetID = named.ID
		// nil when the session is in no magmux on this machine. The navigate
		// stage then declares nothing and the gate records "cannot", which is
		// the same honest answer a machine with no magmux gets.
		focuser = nil
		if one := binder.bindToSession(named.PID); one != nil {
			focuser = one
		}
	}

	p, d, err := obsFocusPipeline(cfg, pipeline.Config{
		// Both ports come over the SOCKET (obssock.go). With no magmux
		// running they are nil, and then this command can name a session and
		// cannot focus one — which is what phase 6 shipped, for a reason that
		// was corrected: what was missing was never a verb, it was a client.
		//
		// The locator is the WHOLE table even when the focuser is narrowed:
		// the pane facts in this document are about every session, and a
		// locator that knew one magmux would call every other session
		// determined-absent from a pane it is sitting in.
		//
		// No Activity port either way. The inferred tier is made of a pane's
		// own traffic and the socket carries none of it, so every session here
		// still resolves on the authoritative tier alone and no conflict can
		// arise. That is the truth about this process rather than a
		// degradation of it.
		Locator: locator,
		Focuser: focuser,
		Policy:  cfg.Policy,
		Audit:   auditor,
	})
	if err != nil {
		fmt.Fprintf(errw, "magmux agents focus: %v\n", err)
		return 1
	}
	defer p.Stop()
	for _, pr := range d.Problems {
		fmt.Fprintf(errw, "magmux agents focus: %s\n", pr.Error())
	}

	var target obs.Session
	if binds {
		// The session named on the first read, found again by its exact id.
		// Not by the user's words a second time: a name or a prefix could
		// resolve to a different session in a registry that moved between the
		// two reads, and the focuser was bound to THIS one's magmux.
		found := false
		for _, s := range d.Doc.Sessions {
			if s.ID == targetID {
				target, found = s, true
				break
			}
		}
		if !found {
			fmt.Fprintf(errw, "magmux agents focus: session %s is no longer observed\n", targetID)
			return 1
		}
	} else {
		target, err = resolveObsTarget(d.Doc, cfg.Target)
		if err != nil {
			fmt.Fprintf(errw, "magmux agents focus: %v\n", err)
			return 1
		}
	}

	report, doErr := p.Do(context.Background(), act.Request{
		Action: act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate, Session: target.ID},
		By:     act.RequesterCLI,
	})

	if cfg.JSON {
		writeAuditJSON(out, report.Records)
	} else {
		writeFocusReport(out, target, report)
	}
	if auditor != nil {
		if w, isWriter := auditor.(*act.Writer); isWriter {
			fmt.Fprintf(out, "audit trail: %s\n", w.Path())
		}
	}
	if doErr != nil {
		// A refusal is a failure and exits non-zero. A caller that asked for
		// something and got exit 0 without it happening cannot tell that from
		// success — the same rule the gate applies to its own return value,
		// applied to the process's.
		fmt.Fprintf(errw, "magmux agents focus: %v\n", doErr)
		return 1
	}
	return 0
}

// obsFocusDocument reads ONE document through a pipeline that can act on
// nothing — no focuser, no audit trail, the zero policy — and stops it. It is
// the first of obsFocus's two reads, used only to name the session.
func obsFocusDocument(cfg obsFocusConfig, locator obs.PaneLocator) (pipeline.Delivery, error) {
	p, d, err := obsFocusPipeline(cfg, pipeline.Config{Locator: locator})
	if err != nil {
		return pipeline.Delivery{}, err
	}
	p.Stop()
	return d, nil
}

// obsFocusPipeline starts the command's pipeline with the ports given and
// returns it together with its first document. The caller stops it.
//
// Everything obsFocus's two reads share is here, so they cannot differ in
// anything but the ports: the same registry, the same oracle, the same
// transcript middleware.
func obsFocusPipeline(cfg obsFocusConfig, ports pipeline.Config) (*pipeline.Pipeline, pipeline.Delivery, error) {
	p, err := pipeline.Start(pipeline.Config{
		Root:    cfg.Root,
		Oracle:  cfg.Oracle,
		Locator: ports.Locator,
		Focuser: ports.Focuser,
		Policy:  ports.Policy,
		Audit:   ports.Audit,
		Chain:   []pipeline.Factory{claudecode.Middleware(claudecode.Config{Root: cfg.Projects})},
		// No watch: one document is all this needs, and a watcher opens a
		// descriptor per record on Darwin.
		Watch: false,
		Now:   time.Now,
	})
	if err != nil {
		return nil, pipeline.Delivery{}, err
	}
	sub, err := p.Subscribe("focus", 1)
	if err != nil {
		p.Stop()
		return nil, pipeline.Delivery{}, err
	}
	// Subscribe first, then ask. Nothing is published until something asks,
	// and the other order races the answer.
	p.Refresh()
	d, ok := <-sub.C()
	if !ok {
		p.Stop()
		return nil, pipeline.Delivery{}, fmt.Errorf("the pipeline stopped before it read anything")
	}
	return p, d, nil
}

// resolveObsTarget finds the one session a target string names.
//
// Four ways, in order, and the first that matches ANY session decides which
// kind of match this is: an exact pid, an exact session id, a session-id
// prefix, or an exact name. An ambiguous target is an ERROR listing the
// candidates rather than a pick — acting on the wrong session of two is the
// failure this whole layer exists to avoid, and a prefix that matches two
// sessions is exactly the shape that produces it.
func resolveObsTarget(fleet obs.Fleet, target string) (obs.Session, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return obs.Session{}, fmt.Errorf("no session named")
	}

	if pid, err := strconv.Atoi(t); err == nil {
		var hits []obs.Session
		for _, s := range fleet.Sessions {
			if s.PID == pid {
				hits = append(hits, s)
			}
		}
		return oneOf(hits, t, "pid")
	}

	for _, s := range fleet.Sessions {
		if s.ID == t {
			return s, nil
		}
	}

	const minPrefix = 6
	if len(t) >= minPrefix {
		var hits []obs.Session
		for _, s := range fleet.Sessions {
			if strings.HasPrefix(s.ID, t) {
				hits = append(hits, s)
			}
		}
		if len(hits) > 0 {
			return oneOf(hits, t, "session id prefix")
		}
	}

	var named []obs.Session
	for _, s := range fleet.Sessions {
		if s.Name == t {
			named = append(named, s)
		}
	}
	if len(named) > 0 {
		return oneOf(named, t, "name")
	}

	return obs.Session{}, fmt.Errorf("no session matches %q — %d sessions were observed; "+
		"run `magmux agents --once` to see them", t, len(fleet.Sessions))
}

// oneOf insists on exactly one match.
func oneOf(hits []obs.Session, target, kind string) (obs.Session, error) {
	switch len(hits) {
	case 0:
		return obs.Session{}, fmt.Errorf("no session matches %s %q", kind, target)
	case 1:
		return hits[0], nil
	}
	var ids []string
	for _, s := range hits {
		ids = append(ids, fmt.Sprintf("%s (pid %d)", s.ID, s.PID))
	}
	return obs.Session{}, fmt.Errorf("%s %q matches %d sessions — %s; name one exactly",
		kind, target, len(hits), strings.Join(ids, ", "))
}

// writeFocusReport prints what happened, for a person.
//
// The DECISION is printed whether or not the action ran, because "why" is the
// question a refusal raises and the audit record answers it in the same words.
func writeFocusReport(out io.Writer, target obs.Session, report act.Report) {
	fmt.Fprintf(out, "session %s (pid %d%s)\n", target.ID, target.PID, focusPaneText(target))
	if report.Decision.Rule != "" {
		fmt.Fprintf(out, "policy: %s\n", report.Decision.Rule)
	}
	if report.Result.Done {
		fmt.Fprintf(out, "done: %s\n", report.Result.Detail)
	}
	for _, r := range report.Records {
		fmt.Fprintf(out, "audit: %s\n", auditLineText(r))
	}
}

// auditLineText is one audit record in one line of prose. The JSON is the
// record; this is the reading of it.
func auditLineText(r act.Record) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "#%d %s %s/%s", r.Seq, r.Event, r.Tier, r.Verb)
	if r.Outcome != nil {
		switch {
		case r.Outcome.Refused:
			fmt.Fprintf(b, " — refused: %s", r.Outcome.Error)
		case r.Outcome.Error != "":
			fmt.Fprintf(b, " — failed: %s", r.Outcome.Error)
		case r.Outcome.Detail != "":
			fmt.Fprintf(b, " — %s", r.Outcome.Detail)
		}
	}
	return b.String()
}

// writeAuditJSON emits the records, one whole object per line, which is the
// shape the trail itself has.
//
// A record names its target session, and that name is registry text, so the
// output goes through terminalSafeJSON on the same terms as `magmux agents
// --json`: every string exact, and no DEL or C1 character raw on a terminal.
func writeAuditJSON(out io.Writer, records []act.Record) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range records {
		buf.Reset()
		if err := enc.Encode(r); err != nil {
			fmt.Fprintf(os.Stderr, "magmux agents focus: encoding an audit record: %v\n", err)
			continue
		}
		if _, err := out.Write(obsview.TerminalSafeJSON(buf.Bytes())); err != nil {
			fmt.Fprintf(os.Stderr, "magmux agents focus: writing an audit record: %v\n", err)
			return
		}
	}
}

func focusPaneText(s obs.Session) string {
	if s.PaneID == nil {
		return ""
	}
	return fmt.Sprintf(", magmux pane %d", *s.PaneID)
}

func printObsFocusHelp(w io.Writer) {
	fmt.Fprint(w, `magmux agents focus — focus the magmux pane a session is running in

Usage: magmux agents focus [--root DIR] [--projects DIR] [--audit FILE] [--json] TARGET

TARGET names one session: its pid, its session id or an unambiguous prefix of
one, or its name. A target that matches more than one session is refused with
the candidates listed rather than picked between.

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
