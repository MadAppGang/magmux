package main

// Request/response on the IPC socket.
//
// The socket has always been fire-and-forget: a bad pane index, a dead PTY, a
// mistyped verb all failed silently, which is fine for a broadcast tap and
// useless to anything that has to report an outcome back to a caller.
//
// The extension is opt-in **per message, not per verb**. A message carrying an
// "id" gets exactly one `reply`, unicast to the connection that sent it; a
// message without one is dispatched and answered exactly as before, which is to
// say not answered at all. That is what makes this invisible to the existing
// clients (madbench, pilot/magmux.ts) rather than merely ignorable by them:
// neither sends an id, so neither ever receives a line it would have to parse —
// including for verbs it has never heard of, which stay silently ignored.
//
// `reply` is deliberately never recorded into m.finalEvents. Those are replayed
// to a client that connects during teardown, and a stale reply arriving there
// would break the results → shutdown → EOF ordering subscribers rely on.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// sockProtocol is the version reported by `capabilities`. Bumped only when a
// client that understood the previous value would misread the new one.
const sockProtocol = 1

// Stable, machine-readable failure codes. A caller branches on the code; the
// message beside it is for a human and may be reworded at any time.
const (
	sockCodeBadRequest    = "bad_request"
	sockCodeNoSuchPane    = "no_such_pane"
	sockCodePaneIsControl = "pane_is_control"
	sockCodePaneDead      = "pane_dead"
	// sockCodePaneHidden means the pane is alive and holds its id but is not in
	// the layout, so nothing paints it. Distinct from no_such_pane because the
	// pane is real and its history is intact, and distinct from pane_is_control
	// because it is about VISIBILITY: the panel is a perfectly good focus target
	// while it is on screen, and every other pane could in principle be hidden.
	sockCodePaneHidden  = "pane_hidden"
	sockCodeUnknownVerb = "unknown_verb"
	// sockCodeNotReady means the socket is up but the layout is not: magmux
	// binds before the first child forks and can therefore be reached before
	// buildGrid has run. Distinct from no_such_pane on purpose — "pane 0 does
	// not exist" and "no pane exists yet" send a caller to opposite places, and
	// the second one is fixed by waiting rather than by using another index.
	sockCodeNotReady    = "not_ready"
	sockCodeUnsupported = "unsupported"
	sockCodeBusy        = "busy"
	sockCodeTimeout     = "timeout"
	sockCodeInternal    = "internal"
	// sockCodeNoController means the pane exists and is perfectly healthy but
	// magmux is not following a tool inside it — a shell, a dev server, a REPL.
	// Distinct from unsupported because the recovery differs: there is nothing
	// to wait for and nothing to fix, so the caller should read the screen.
	sockCodeNoController = "no_controller"
	// sockCodeNoTranscript means a controller IS following this pane but has
	// not located the tool's own record of it. It is emphatically not an empty
	// success: "we cannot find its record" and "it has said nothing" send a
	// caller to opposite places, and discovery genuinely lags at session start
	// and can fail outright (see the ~/.claude/projects note in CLAUDE.md).
	sockCodeNoTranscript = "no_transcript"
)

// sockVerbs and sockEvents are what `capabilities` advertises. Maintained by
// hand alongside dispatchSocketVerb: a verb missing here is invisible to a
// client that feature-detects rather than probing, so adding a verb means
// adding it in both places.
var (
	sockVerbs = []string{"capabilities", "list", "capture", "transcript", "open_pane", "close_pane",
		"focus", "status", "tint", "overlay", "send", "pilot", "agent"}
	sockEvents = []string{"snapshot", "exit", "control", "pane_opened", "pane_closed",
		"results", "shutdown", "reply"}
)

// sockErr is a verb failure with a stable machine-readable code.
type sockErr struct{ Code, Msg string }

func (e *sockErr) Error() string { return e.Msg }

func sockErrf(code, format string, a ...any) *sockErr {
	return &sockErr{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// verbErrCode extracts the machine-readable code from a verb failure, or "".
func verbErrCode(err error) string {
	if err == nil {
		return ""
	}
	var se *sockErr
	if errors.As(err, &se) {
		return se.Code
	}
	return sockCodeInternal
}

// errReplyDeferred is returned by a verb that answers through its done callback
// instead of returning a result — the work outlives the dispatch call. It is a
// control signal between dispatch and handleSocketMsg and is never sent to a
// client.
var errReplyDeferred = errors.New("reply deferred")

// replyTo unicasts a reply to one connection.
//
// It takes sockClientsMu — the same lock broadcastEvent holds — so a reply can
// never interleave mid-line with a broadcast on the same fd; a write to a unix
// stream socket is not atomic and a spliced line would corrupt both messages.
//
// Caller must NOT hold p.mu. The established order is p.mu -> sockClientsMu
// (see the note at handleSocketConn), so a verb builds its result payload,
// releases p.mu, and only then replies.
//
// A failed write is not spliced out of m.sockClients the way broadcastEvent
// does it: this connection has its own read loop, which ends on the same
// failure and unregisters it there.
func (m *Magmux) replyTo(conn net.Conn, id json.RawMessage, result map[string]any, err error) {
	if conn == nil || len(id) == 0 {
		return
	}
	reply := map[string]any{"type": "reply", "id": id, "ok": err == nil}
	switch {
	case err != nil:
		code := sockCodeInternal
		var se *sockErr
		if errors.As(err, &se) {
			code = se.Code
		}
		reply["code"] = code
		reply["error"] = err.Error()
	case result != nil:
		reply["result"] = result
	}

	data, mErr := json.Marshal(reply)
	if mErr != nil {
		// A result we cannot serialize is our bug, not the caller's. Answering
		// with the failure keeps the one-reply-per-id contract; dropping the
		// line would leave a caller waiting on its timeout instead.
		data, mErr = json.Marshal(map[string]any{
			"type": "reply", "id": id, "ok": false,
			"code": sockCodeInternal, "error": "reply payload could not be encoded",
		})
		if mErr != nil {
			return
		}
	}
	data = append(data, '\n')

	m.sockClientsMu.Lock()
	defer m.sockClientsMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = conn.Write(data)
	_ = conn.SetWriteDeadline(time.Time{})
}

// handleSocketMsg runs one inbound message and answers it if — and only if — it
// asked to be answered.
//
// It reports whether the message was a CONTROLLER action rather than a display
// tweak, which is the whole of the protocol needed to notice a controller
// coming and going: magmux already sees the fd close, and the only missing bit
// was telling a driving connection apart from a passive subscriber's tint.
func (m *Magmux) handleSocketMsg(msg sockMsg, conn net.Conn) bool {
	driving := isControllerVerb(msg.Type)
	if !m.layoutIsReady() {
		// handleSocketConn already waited, so getting here means the wait timed
		// out or teardown began: there is no layout to run this against. Refuse
		// rather than dispatch — every verb would resolve its pane against an
		// empty table and report a missing INDEX for a missing LAYOUT. Silent
		// without an id, like every other failure on the legacy path.
		m.replyTo(conn, msg.ID, nil, sockErrf(sockCodeNotReady,
			"magmux has not finished starting up; no panes exist yet"))
		return false
	}
	if len(msg.ID) == 0 {
		// Legacy path, unchanged: dispatch and discard both returns.
		m.dispatchSocketMsg(msg)
		return driving
	}
	id := msg.ID
	result, err := m.dispatchSocketVerbExt(msg, func(r map[string]any, e error) {
		m.replyTo(conn, id, r, e)
	})
	if errors.Is(err, errReplyDeferred) {
		return driving // the verb owns the reply now, and will send exactly one
	}
	m.replyTo(conn, id, result, err)
	return driving
}

// dispatchSocketVerbExt routes the verbs implemented in this file and hands
// everything else to the main dispatch table.
//
// The split is not architecture, it is blast radius: the table lives in
// main.go beside the terminal core, and a verb that only reads a controller
// has no business being added there. The order matters in one direction only —
// a verb defined here shadows one of the same name there, and there is none.
//
// It is deliberately reached from the ID-carrying path alone. A verb added
// here is invisible to the legacy fire-and-forget path, which falls through to
// the main table and its silent unknown-verb answer, which is exactly the
// contract every pre-reply client was written against.
func (m *Magmux) dispatchSocketVerbExt(msg sockMsg, done func(map[string]any, error)) (map[string]any, error) {
	switch msg.Type {
	case "transcript":
		return m.sockTranscript(msg)
	}
	return m.dispatchSocketVerb(msg, done)
}

// isControllerVerb reports whether a verb is one an agent DRIVING magmux would
// send, as opposed to one a passive subscriber uses to decorate the display.
// madbench tints and sets status without ever being a controller, and calling
// it one would put a phantom connect/disconnect pair in the panel.
func isControllerVerb(verb string) bool {
	switch verb {
	case "send", "pilot", "open_pane", "close_pane", "focus", "capture", "transcript", "list":
		return true
	}
	return false
}

// paneForMsg resolves a verb's `pane` field to a pane, or to the sockErr its
// reply should carry. It is the single place a `pane` field becomes a *Pane,
// and it reaches the identity table only through paneByID — going around it
// would let a verb write into a tombstoned pane, which produces no error, no
// repaint and no log.
//
// Caller must NOT hold treeMu.
func (m *Magmux) paneForMsg(idx int) (*Pane, error) {
	switch {
	case idx == paneAll:
		return nil, sockErrf(sockCodeBadRequest, `"*" is not a target for this verb`)
	case idx == paneUnspecified:
		return nil, sockErrf(sockCodeBadRequest, "no pane given")
	case idx < 0:
		return nil, sockErrf(sockCodeBadRequest, "pane is not an index")
	}
	p := m.paneByID(idx)
	if p == nil {
		return nil, sockErrf(sockCodeNoSuchPane, "no pane %d (it may have been closed)", idx)
	}
	return p, nil
}

// refuseRequest files a controller request magmux turned down before it could
// resolve a pane.
//
// It is recorded at RUN level (pane -1) on purpose, and that is the whole of
// the ordering rule this exists to enforce: recordRequest opens a route for any
// pane >= 0, so filing a request against an index that resolves to nothing
// manufactures a permanent route to a pane that does not exist. A route means
// "a pane this controller has touched", and a request magmux refused touched
// none. The damage is not cosmetic — 32 phantoms exhaust ctrlMaxRoutes so real
// panes stop getting table rows, and ONE is enough for targetPane to see two
// routes where there is one, after which every pane-less `send` from a pilot
// that had exactly one session is refused for the rest of the run.
//
// The request is still recorded, with its refusal as the ack: a controller that
// got a pane id wrong should find that out from the panel rather than from the
// damage, which is the same argument targetPane's own refusal note makes.
func (m *Magmux) refuseRequest(verb, text string, err error) {
	seq := m.control.recordRequest(-1, verb, text)
	m.control.recordAck(seq, false, verbErrCode(err), err.Error())
}

// sockCapabilities describes this magmux to a client that has to decide what it
// may ask for. It doubles as the version probe: a magmux predating replies
// answers nothing at all, because an unknown verb without an id is silent.
func (m *Magmux) sockCapabilities() (map[string]any, error) {
	// rows/cols are treeMu's: SIGWINCH rewrites them from its own goroutine.
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	res := map[string]any{
		"protocol": sockProtocol,
		"version":  Version,
		"pid":      os.Getpid(),
		"sock":     m.sockPath,
		"rows":     m.rows,
		"cols":     m.cols,
		"gridMode": m.gridMode,
		// How many rows of history a PRIMARY screen keeps per pane, which is
		// what `capture`'s offset can reach back through. Declared as a number
		// so a client learns the ceiling instead of discovering it by walking
		// backwards until the answers stop changing. It is the ceiling, not a
		// promise: an alternate-screen pane records nothing at all, and each
		// capture reply carries that pane's own `scrollback` count.
		"scrollback": scrollbackLimit,
		"verbs":      sockVerbs,
		"events":     sockEvents,
	}
	if Commit != "" && Commit != "none" {
		res["commit"] = Commit
	}
	return res, nil
}

// sockList reports every pane. It is buildPaneResults verbatim — the same call
// `snapshot` and `results` are built from — so a client polling `list` and a
// subscriber reading the shutdown `results` can never be told different things
// about the same pane.
func (m *Magmux) sockList() (map[string]any, error) {
	seq := m.control.recordRequest(-1, "list", "every pane")
	panes := m.buildPaneResults()
	m.control.recordAck(seq, true, "", fmt.Sprintf("%d panes", len(panes)))
	return map[string]any{"panes": panes}, nil
}

// sockCapture renders a screenful of a pane to text. `lines` keeps the LAST N
// rows, because the part of an interactive session worth reading is at the
// bottom.
//
// `offset` reaches BACK into the pane's scrollback, in rows, measured from the
// bottom of the live screen: 0 is what is on display, `rows` is the screenful
// above it, and the reply's `scrollback` says how many rows exist to reach
// through while `atTop` says the oldest is in view. Rows, not screenfuls,
// because the caller already knows `rows` from the same reply and a screenful is
// not a stable unit across a resize.
//
// The reply reports its own offset rather than echoing the request's: an offset
// past the top clamps, and a client that walked past the end otherwise has no
// way to tell that two successive reads returned the same screen.
func (m *Magmux) sockCapture(msg sockMsg) (map[string]any, error) {
	idx := m.parsePaneIndex(msg.Pane)
	// Resolve BEFORE the panel hears about it: recording an unvalidated index
	// opens a route to a pane that does not exist. See refuseRequest.
	p, err := m.paneForMsg(idx)
	if err != nil {
		m.refuseRequest("capture", "read the screen", err)
		return nil, err
	}
	// Reading a pane is the controller touching it, so it opens a route: a
	// pane the agent looks at is a pane the panel should be able to compare.
	seq := m.control.recordRequest(idx, "capture", "read the screen")
	// The control pane is capturable on purpose: it has a screen like any
	// other, and reading the panel is observation, which is all this verb does.
	shot := p.captureAt(msg.Offset, msg.Lines, true)
	ack := fmt.Sprintf("%d×%d rows", shot.Rows, shot.Cols)
	if shot.Offset > 0 {
		ack += fmt.Sprintf(", %d back", shot.Offset)
	}
	m.control.recordAck(seq, true, "", ack)
	return map[string]any{
		"pane":      idx,
		"rows":      shot.Rows,
		"cols":      shot.Cols,
		"alt":       shot.Alt,
		"truncated": shot.Truncated,
		"cursor":    map[string]any{"y": shot.CurY, "x": shot.CurX},
		"text":      shot.Text,
		// Where this screenful came from and how much is behind it. Always
		// present, including at offset 0, so a client never has to branch on
		// whether the fields exist.
		"offset":     shot.Offset,
		"scrollback": shot.Scrollback,
		"atTop":      shot.AtTop,
	}, nil
}

// sockTranscript serves a pane's turn history from the tool's OWN record on
// disk — full text, tool inputs and tool results — rather than from the screen.
//
// It is the counterpart to `capture` and answers a different question. capture
// says what a pane is painting: wrapped, redrawn, scrolled off, and for a TUI
// often not even a record of the conversation. transcript says what the
// session actually said, which is the thing an agent driving it needs and the
// thing it could previously only guess at from 300 characters of
// Snapshot.LastResponse.
//
// The turn count arrives in `lines`, the same field capture uses for "keep the
// last N": the wire struct lives in main.go, which this change does not touch.
// A follow-up that adds a `turns` field there should accept both.
//
// Every failure is an error with its own code, never an empty success. An
// empty `turns` array reads to an agent as "this session has said nothing",
// which is a claim about the SESSION; the truth in each of these cases is a
// claim about MAGMUX, and the two lead a driver to opposite next steps.
func (m *Magmux) sockTranscript(msg sockMsg) (map[string]any, error) {
	idx := m.parsePaneIndex(msg.Pane)
	const what = "read the session's own record"
	// Resolve BEFORE the panel hears about it, for the reason refuseRequest
	// states: an unvalidated index would open a route to a pane that is not there.
	p, err := m.paneForMsg(idx)
	if err != nil {
		m.refuseRequest("transcript", what, err)
		return nil, err
	}
	seq := m.control.recordRequest(idx, "transcript", what)
	fail := func(err error) (map[string]any, error) {
		m.control.recordAck(seq, false, verbErrCode(err), err.Error())
		return nil, err
	}
	// p.controller is written by attachController while the pane is still
	// private — before it is reachable from any other goroutine — and never
	// written again. pollControllers and buildPaneResults already read it
	// unlocked from two other goroutines; this is that same read.
	ctrl := p.controller
	if ctrl == nil {
		return fail(sockErrf(sockCodeNoController,
			"pane %d is not running a tool magmux follows, so it has no transcript. This is "+
				"normal for a shell, a REPL or a dev server — use capture to read its screen.", idx))
	}
	reader, ok := ctrl.(TranscriptReader)
	if !ok {
		return fail(sockErrf(sockCodeUnsupported,
			"the %s controller on pane %d does not keep a readable record of its turns; "+
				"use capture to read the screen instead", ctrl.Name(), idx))
	}

	want := normalizeTranscriptTurns(msg.Lines)
	turns, err := reader.Transcript(want)
	if err != nil {
		if errors.Is(err, errNoTranscript) {
			return fail(sockErrf(sockCodeNoTranscript,
				"magmux has not located pane %d's transcript (%s). This does NOT mean the session "+
					"has said nothing — it means its on-disk record has not been found: discovery "+
					"lags for a second or two after a session starts, and can fail outright. "+
					"Retry, or read the screen with capture.", idx, ctrl.Name()))
		}
		return fail(sockErrf(sockCodeInternal, "reading pane %d's transcript failed: %v", idx, err))
	}

	entries := make([]any, 0, len(turns))
	for _, t := range turns {
		entries = append(entries, turnEvent(t))
	}
	m.control.recordAck(seq, true, "", fmt.Sprintf("%d turns", len(turns)))
	return map[string]any{
		"pane":       idx,
		"controller": ctrl.Name(),
		"requested":  want,
		"turns":      entries,
	}, nil
}

// turnEvent renders one Turn for the wire. Tool input and result cross as
// strings, and are NOT shortened here: this layer has no idea what the caller's
// budget is, and a socket client that wants the whole of a tool result — a
// diff, a stack trace — must be able to get it.
func turnEvent(t Turn) map[string]any {
	ev := map[string]any{"role": t.Role, "text": t.Text}
	if !t.Timestamp.IsZero() {
		ev["timestamp"] = t.Timestamp.UTC().Format(time.RFC3339)
	}
	if len(t.Tools) > 0 {
		tools := make([]any, 0, len(t.Tools))
		for _, c := range t.Tools {
			tools = append(tools, map[string]any{
				"name": c.Name, "input": c.Input, "result": c.Result,
			})
		}
		ev["tools"] = tools
	}
	return ev
}

// ── pane lifecycle ──────────────────────────────────────────────────────────
//
// These three run straight on the socket goroutine. open_pane takes tens of
// milliseconds for the fork/exec, which is fine there and is exactly why it
// must NOT be queued onto the render loop: a queue drained by render()
// serialises against the render goroutine only, and would still race
// inputLoop, SIGWINCH and every other socket connection.

// sockOpenPane splits a pane and runs a command in the new half. `cmd` is
// handed to the user's login shell exactly as -e does, so the same quoting,
// pipelines and `cd x && y` forms work.
func (m *Magmux) sockOpenPane(msg sockMsg) (map[string]any, error) {
	cmd := strings.TrimSpace(msg.Cmd)
	if cmd == "" {
		return nil, sockErrf(sockCodeBadRequest, "open_pane needs a cmd")
	}
	// cwd and dir are accepted as synonyms: `cwd` is what list/capture already
	// report a pane's directory as, and `dir` is what PaneConfig calls it.
	dir := msg.Dir
	if dir == "" {
		dir = msg.Cwd
	}
	if dir != "" {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			// Caught here rather than at fork/exec, where the failure is an
			// opaque "fork/exec: no such file or directory" against the shell.
			return nil, sockErrf(sockCodeBadRequest, "cwd %q is not a directory", dir)
		}
	}

	var split SplitType
	switch strings.ToLower(strings.TrimSpace(msg.Direction)) {
	case "", "auto":
		split = SplitNone
	case "horizontal", "h", "lr":
		split = SplitHorizontal
	case "vertical", "v", "tb":
		split = SplitVertical
	default:
		return nil, sockErrf(sockCodeBadRequest,
			"split must be auto, horizontal or vertical (got %q)", msg.Direction)
	}

	// An absent target means "the largest live leaf" — NOT the focused pane.
	//
	// The caller here is an agent, and an agent that named no target has said
	// nothing about focus; focus belongs to the human, who in the flagship
	// layout is typing into pane 0. Splitting that pane sends it a SIGWINCH
	// that reflows a Claude Code TUI mid-turn, to run something the human never
	// asked to see. "Largest" is also what the MCP tool schema advertises, so
	// this is the behaviour catching up with the promise rather than the
	// documentation being edited down to the behaviour.
	//
	// Anything present but not an index is refused rather than rounded, for the
	// same reason `send` refuses it — the wrong pane gets split and the layout
	// is silently wrong.
	target := targetLargest
	if msg.Target != nil {
		switch t := m.parsePaneIndex(msg.Target); {
		case t >= 0:
			target = t
		case t == paneUnspecified:
		case t == paneAll:
			return nil, sockErrf(sockCodeBadRequest, `open_pane has no fan-out: "*" is not a target`)
		default:
			return nil, sockErrf(sockCodeBadRequest, "target is not a pane index")
		}
	}

	focus := false
	if msg.Focus != nil {
		focus = *msg.Focus
	}

	// Run-level until the reply names an id: there is no pane to route to yet.
	seq := m.control.recordRequest(-1, "open_pane", cmd)
	id, err := m.OpenPane(OpenPaneRequest{
		PaneConfig: PaneConfig{
			Cmd:   getUserShell(),
			Args:  []string{"-l", "-c", cmd},
			Dir:   dir,
			Env:   msg.Env,
			Label: msg.Label,
		},
		Target: target,
		Split:  split,
		Ratio:  msg.Ratio,
		Focus:  focus,
	})
	if err != nil {
		m.control.recordAck(seq, false, verbErrCode(err), err.Error())
		return nil, err
	}
	title := msg.Label
	if title == "" {
		title = firstField(cmd)
	}
	m.control.recordRouteOpened(id, title)
	m.control.recordAck(seq, true, "", fmt.Sprintf("pane %d · %s", id, title))
	if focus {
		m.control.setFocused(id)
	}
	res := map[string]any{"pane": id, "cmd": cmd}
	if dir != "" {
		res["cwd"] = dir
	}
	if msg.Label != "" {
		res["label"] = msg.Label
	}
	return res, nil
}

// sockClosePane detaches a pane and reaps its child. The id is retained as a
// tombstone, so every other pane keeps the index its caller already knows.
func (m *Magmux) sockClosePane(msg sockMsg) (map[string]any, error) {
	idx := m.parsePaneIndex(msg.Pane)
	switch {
	case idx == paneAll:
		return nil, sockErrf(sockCodeBadRequest, `close_pane has no fan-out: "*" is not a target`)
	case idx < 0:
		return nil, sockErrf(sockCodeBadRequest, "close_pane needs a pane index")
	}
	text := fmt.Sprintf("pane %d", idx)
	// Resolve BEFORE the panel hears about it (see refuseRequest); the error was
	// previously discarded here and the phantom route filed anyway.
	p, err := m.paneForMsg(idx)
	if err != nil {
		m.refuseRequest("close_pane", text, err)
		return nil, err
	}
	// The panel is an instrument, not a session, and it is refused here rather
	// than only in the MCP layer: the deeper rule is that a controller may not
	// close the thing that reports on it. "Who closed pane 2" must not be a
	// question the panel has to answer about itself — the same argument that
	// keeps the panel read-only from the keyboard (see consumeControlKey). The
	// internal ClosePane stays capable, because a Control pane opened through
	// OpenPane still has to be closable.
	//
	// The refusal is filed at run level for that same reason: routing it to the
	// panel's own index would have the panel report on itself.
	if p.isControl {
		err := sockErrf(sockCodePaneIsControl,
			"pane %d is the control panel; it is magmux's own display and cannot be closed", idx)
		m.refuseRequest("close_pane", text, err)
		return nil, err
	}
	seq := m.control.recordRequest(idx, "close_pane", text)
	if err := m.ClosePane(idx, msg.Force); err != nil {
		m.control.recordAck(seq, false, verbErrCode(err), err.Error())
		return nil, err
	}
	// The route survives its pane on purpose: the history of something an agent
	// closed because it had failed is exactly what you want left to read.
	m.control.recordRouteClosed(idx)
	m.control.recordAck(seq, true, "", fmt.Sprintf("pane %d closed", idx))
	return map[string]any{"pane": idx, "closed": true}, nil
}

// firstField is the leading word of a command line, for a route's short name.
func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// sockFocus moves keyboard focus. Included because open_pane's focus flag is
// one-shot: without this, a client that opened a pane unfocused can never
// change its mind.
//
// It refuses a HIDDEN pane, and refuses nothing else. The distinction is
// deliberate: the control panel is a legitimate focus target while it is on
// screen — inputLoop routes keys to consumeControlKey, which scrolls it, and
// Ctrl-G o already cycles onto it — so refusing isControl outright would take
// away something that works. What does not work is focus on a pane that is not
// in the tree: it is never painted, the panel has no PTY, and writePTY
// therefore swallows every later keystroke with nothing on screen to explain
// where it went. The panel is hidden by DEFAULT and buildPaneResults publishes
// its id as state:"panel", hidden:true, so any client can reach that state by
// reading `list`. It is the exact state hidePanelLocked repairs and focusNext /
// resolveSplitTargetLocked skip; this was the one door left open into it.
func (m *Magmux) sockFocus(msg sockMsg) (map[string]any, error) {
	idx := m.parsePaneIndex(msg.Pane)
	p, err := m.paneForMsg(idx)
	if err != nil {
		return nil, err
	}
	m.treeMu.Lock()
	// Re-resolve under the write lock: the pane may have been closed between
	// paneForMsg and here, and focusing a detached pane means every later
	// keystroke goes nowhere with nothing on screen to explain it.
	if m.paneByIDLocked(idx) != p {
		m.treeMu.Unlock()
		return nil, sockErrf(sockCodeNoSuchPane, "no pane %d (it may have been closed)", idx)
	}
	// `hidden` is a structural fact about the tree, so it is treeMu's and is
	// read here rather than before the lock.
	if p.hidden {
		// isControl is written at construction and never again; the hint is
		// conditional because "hidden" is a property of any pane, even though
		// the panel is the only one magmux ever hides today.
		hint := ""
		if p.isControl {
			hint = " It is magmux's own control panel, hidden by default; Ctrl-G p reveals it."
		}
		m.treeMu.Unlock()
		return nil, sockErrf(sockCodePaneHidden,
			"pane %d is not on screen, so focusing it would send every keystroke somewhere "+
				"nobody can see.%s", idx, hint)
	}
	m.focused = p
	m.treeMu.Unlock()
	m.control.setFocused(idx)
	p.mu.Lock()
	p.dirty = true
	p.mu.Unlock()
	return map[string]any{"pane": idx, "focused": true}, nil
}
