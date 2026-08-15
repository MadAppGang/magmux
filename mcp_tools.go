package main

// The MCP tool surface: 3 session tools + 6 pane tools.
//
// Two conventions run through this file.
//
// Error discipline. A malformed call — unknown tool, arguments that do not fit
// the schema — is a JSON-RPC `error`, because the model cannot fix it and
// should not be invited to retry. A tool that ran and failed — no such pane,
// the session went away, a turn stalled — is a normal `result` with
// isError:true and text saying what to do next, because the model *can* fix
// that and the recovery is the whole point.
//
// Pane addressing. `pane` accepts an index or a label, and the label is
// resolved here, in the server. A non-numeric pane reference must never reach
// magmux: its parser has historically fallen back to pane 0 for anything it
// could not parse, which is silent, wrong, and — once close_pane exists —
// destructive.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// mcpTool is one tool: its schema, how long the server will let it run, and
// its implementation.
type mcpTool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]any
	// timeout bounds the whole call. 0 means unbounded, which only
	// send_and_wait uses — its own two-phase timeouts are the real bound.
	timeout time.Duration
	run     func(ctx context.Context, s *mcpServer, args json.RawMessage) (map[string]any, *rpcError)
}

// ── result helpers ──────────────────────────────────────────────────────────

func toolText(text string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

// toolResultError is an execution failure the model can act on. It is a
// successful JSON-RPC response carrying isError — not a protocol error.
func toolResultError(format string, a ...any) map[string]any {
	r := toolText(fmt.Sprintf(format, a...))
	r["isError"] = true
	return r
}

func invalidParams(err error) *rpcError {
	return &rpcError{Code: rpcInvalidParams, Message: "invalid arguments: " + err.Error()}
}

// ── schema helpers ──────────────────────────────────────────────────────────

func objectSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// paneProp is the shared `pane` schema. anyOf rather than `"type":["integer",
// "string"]` because several clients validate the latter poorly.
func paneProp() map[string]any {
	return map[string]any{
		"anyOf":       []any{map[string]any{"type": "integer"}, map[string]any{"type": "string"}},
		"description": "Pane index (0-based) or the label given at open_pane.",
	}
}

func sessionProp() map[string]any {
	return strProp("Session id from list_sessions. Optional: defaults to the magmux this " +
		"server is running inside, or to the only one that is reachable.")
}

// ── the tools ───────────────────────────────────────────────────────────────

var mcpTools = []mcpTool{
	{
		Name:  "list_sessions",
		Title: "List magmux sessions",
		Description: "List every magmux session on this machine, with its panes and their " +
			"states. Start here when you do not know what is running. Sessions marked " +
			"stale or inaccessible cannot be used; never delete their socket files.",
		Schema:  objectSchema(map[string]any{}),
		timeout: 15 * time.Second,
		run:     toolListSessions,
	},
	{
		Name:  "attach_session",
		Title: "Attach to a magmux session",
		Description: "Attach to one magmux session and make it the default for every " +
			"later call. Identify it by id (the name in /tmp/magmux-<id>.sock), by pid, " +
			"or by socket path.",
		Schema: objectSchema(map[string]any{
			"id":   strProp("Session id, e.g. \"work\" or the pid as a string."),
			"pid":  intProp("Pid of the magmux process."),
			"sock": strProp("Absolute path to the session's unix socket."),
		}),
		timeout: 15 * time.Second,
		run:     toolAttachSession,
	},
	{
		Name:  "request_session",
		Title: "Get instructions for starting a session",
		Description: "Ask how to get a magmux session when none is available. Returns " +
			"instructions — it never starts magmux itself, because a magmux nobody can " +
			"see defeats the point: the human watches every pane you drive.",
		Schema: objectSchema(map[string]any{
			"name": strProp("Short name for the new session, e.g. \"agent\". Default: \"agent\"."),
			"cmd":  strProp("Command the first pane should run, e.g. \"claude\"."),
		}),
		timeout: 10 * time.Second,
		run:     toolRequestSession,
	},
	{
		Name:  "list_panes",
		Title: "List panes",
		Description: "List the panes of a session: index, label, state, command and pid. " +
			"Use it to find the pane you mean before driving it.",
		Schema: objectSchema(map[string]any{
			"session_id": sessionProp(),
		}),
		timeout: 15 * time.Second,
		run:     toolListPanes,
	},
	{
		Name:  "open_pane",
		Title: "Open a pane",
		Description: "Split the layout and run a command in the new pane. The command runs " +
			"through the login shell, exactly like magmux -e. Give it a label so you can " +
			"address it by name later.",
		Schema: objectSchema(map[string]any{
			"cmd":   strProp("Command to run, e.g. \"claude\" or \"npm run dev\"."),
			"cwd":   strProp("Working directory for the command. Default: magmux's own."),
			"label": strProp("Short name for this pane, used as a pane reference later."),
			"target": map[string]any{
				"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "string"}},
				"description": "Pane to split. Default: the largest pane, never the focused one " +
					"— the focused pane is usually the human's, and halving it reflows their " +
					"session mid-turn. Name a target when you mean a specific pane.",
			},
			"split": map[string]any{
				"type":        "string",
				"enum":        []any{"auto", "horizontal", "vertical"},
				"description": "Split direction. Default: auto, which splits the longer axis.",
			},
			"focus": boolProp("Focus the new pane. Default: false."),

			"session_id": sessionProp(),
		}, "cmd"),
		timeout: 30 * time.Second,
		run:     toolOpenPane,
	},
	{
		Name:  "close_pane",
		Title: "Close a pane",
		Description: "Close a pane and reap its process. A pane whose process already " +
			"exited stays on screen as a tombstone so the human can read it — closing it " +
			"is how you reclaim the space. Pane indices never shift when one closes.",
		Schema: objectSchema(map[string]any{
			"pane":       paneProp(),
			"force":      boolProp("SIGKILL the process group if it will not exit. Default: false."),
			"session_id": sessionProp(),
		}, "pane"),
		timeout: 30 * time.Second,
		run:     toolClosePane,
	},
	{
		Name:  "read_pane",
		Title: "Read a pane's transcript and screen",
		Description: "Read a pane from two independent sources. transcript:N returns the last N " +
			"turns of an agent session from the tool's OWN record on disk — full text, tool " +
			"inputs and tool results, nothing screen-scraped — and is authoritative. screen " +
			"returns what the pane is painting right now, which is all there is for a shell, a " +
			"REPL or a dev server. Ask for both when you want to know what was said AND what is " +
			"on display. offset:N reads back through the pane's scrollback for output that has " +
			"scrolled off — that works for shells, builds and dev servers, but an agent pane " +
			"runs on the alternate screen and keeps no scrollback, so for those the transcript " +
			"is the history.",
		Schema: objectSchema(map[string]any{
			"pane":   paneProp(),
			"screen": boolProp("Include the rendered screen. Default: true."),
			"lines":  intProp("Keep only the last N rows of the screen. Default: the whole screen."),
			"offset": intProp("Read this many rows FURTHER BACK than the visible screen. 0 or " +
				"omitted: what is on display. Pass the pane's row count to get the screenful " +
				"directly above it, twice that for the one above that, and so on. The reply says " +
				"how many rows of scrollback exist and whether you have reached the oldest."),
			"transcript": intProp("Return the last N turns from the session's own record on disk, " +
				"with full response text and every tool's input and result. 0 or omitted: do not " +
				"read it. Only panes running an agent magmux follows have one."),
			"session_id": sessionProp(),
		}, "pane"),
		timeout: 20 * time.Second,
		run:     toolReadPane,
	},
	{
		Name:  "send_keys",
		Title: "Send keystrokes to a pane",
		Description: "Type text and/or press named keys in a pane, without waiting for a " +
			"turn. This is the tool for permission prompts, menus, and interrupts " +
			"(keys:[\"ctrl-c\"]). For giving an agent an instruction, use send_and_wait.",
		Schema: objectSchema(map[string]any{
			"pane": paneProp(),
			"text": strProp("Text to type."),
			"keys": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Named keys pressed after the text: enter, tab, escape, up, down, " +
					"left, right, backspace, ctrl-c, ctrl-d, a single character, …",
			},
			"enter":      boolProp("Press Enter after the text and keys. Default: true when text is given."),
			"wait":       intProp("Milliseconds to wait afterwards before reading the screen back. Default: 0."),
			"session_id": sessionProp(),
		}, "pane"),
		timeout: 60 * time.Second,
		run:     toolSendKeys,
	},
	{
		Name:  "send_and_wait",
		Title: "Instruct a session and wait for its turn",
		Description: "Give an interactive agent in a pane one instruction and wait for it " +
			"to finish the resulting turn. This is the workhorse. It waits for the turn to " +
			"visibly START before waiting for it to finish, so you are never handed the " +
			"previous turn's answer. If it reports stalled, do NOT assume the step was done. " +
			"Only panes running an agent magmux can follow have turns; for a shell, a REPL or " +
			"a dev server use send_keys and read_pane instead.",
		Schema: objectSchema(map[string]any{
			"pane":             paneProp(),
			"instruction":      strProp("What you want to be true. Brief it like an engineer, not a keyboard macro."),
			"label":            strProp("Short tag shown in magmux's control panel, e.g. \"step 2/5\"."),
			"start_timeout_ms": intProp("How long to wait for the turn to begin. Default: 45000."),
			"turn_timeout_ms":  intProp("How long to wait for the turn to finish. Default: 900000."),
			"session_id":       sessionProp(),
		}, "pane", "instruction"),
		timeout: 0, // its own two-phase timeouts bound it
		run:     toolSendAndWait,
	},
}

func mcpToolByName(name string) (*mcpTool, bool) {
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			return &mcpTools[i], true
		}
	}
	return nil, false
}

// mcpToolSchemas renders the tools/list payload.
func mcpToolSchemas() []map[string]any {
	out := make([]map[string]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": t.Schema,
		})
	}
	return out
}

func (s *mcpServer) handleToolCall(req rpcRequest) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.respondError(req.ID, rpcInvalidParams, "invalid params: "+err.Error(), nil)
			return
		}
	}
	if p.Name == "" {
		s.respondError(req.ID, rpcInvalidParams, "invalid params: missing tool name", nil)
		return
	}
	tool, ok := mcpToolByName(p.Name)
	if !ok {
		s.respondError(req.ID, rpcMethodNotFound, "unknown tool: "+p.Name, nil)
		return
	}

	ctx, cancel := ctxWithTimeout(context.Background(), tool.timeout)
	defer cancel()

	started := time.Now()
	res, rerr := tool.run(ctx, s, p.Arguments)
	s.logf("tools/call %s in %s", tool.Name, time.Since(started).Round(time.Millisecond))
	if rerr != nil {
		s.respondError(req.ID, rerr.Code, rerr.Message, rerr.Data)
		return
	}
	s.respond(req.ID, res)
}

// ── session resolution ──────────────────────────────────────────────────────

// resolveSession finds the session a tool should act on: an explicit id, then
// the current default, then the magmux we are running inside, then the only
// reachable one. Ambiguity is an error rather than a guess — picking the wrong
// session means typing into someone else's terminal.
func (s *mcpServer) resolveSession(ctx context.Context, id string) (*Session, error) {
	s.sessMu.Lock()
	if id != "" {
		if sess, ok := s.sessions[id]; ok {
			s.sessMu.Unlock()
			return sess, nil
		}
	} else if s.defID != "" {
		if sess, ok := s.sessions[s.defID]; ok {
			s.sessMu.Unlock()
			return sess, nil
		}
	}
	s.sessMu.Unlock()

	if id != "" {
		sess, err := s.attach(ctx, id, "", 0)
		if err != nil {
			return nil, fmt.Errorf("cannot attach to session %q: %v", id, err)
		}
		return sess, nil
	}

	// The host session: magmux exports MAGMUX_SOCK to every pane, and clients
	// pass their environment to the servers they spawn, so this is set exactly
	// when the agent is already running inside a magmux the human is watching.
	if sock := os.Getenv("MAGMUX_SOCK"); sock != "" {
		if sess, err := s.attach(ctx, "host", sock, 0); err == nil {
			return sess, nil
		} else {
			s.logf("host session %s unreachable: %v", sock, err)
		}
	}

	found := discoverSessions(ctx, time.Second)
	var reachable []SessionInfo
	for _, f := range found {
		if f.Reachable {
			reachable = append(reachable, f)
		}
	}
	switch len(reachable) {
	case 0:
		return nil, fmt.Errorf("no magmux session is running — call request_session for the " +
			"command to give the human, then list_sessions once they have run it")
	case 1:
		sess, err := s.attach(ctx, reachable[0].ID, reachable[0].SockPath, reachable[0].PID)
		if err != nil {
			return nil, fmt.Errorf("cannot attach to session %q: %v", reachable[0].ID, err)
		}
		return sess, nil
	default:
		names := make([]string, 0, len(reachable))
		for _, r := range reachable {
			names = append(names, r.ID)
		}
		return nil, fmt.Errorf("%d magmux sessions are reachable (%s) — pass session_id, or "+
			"call attach_session first", len(reachable), strings.Join(names, ", "))
	}
}

// attach dials a session and registers it, becoming the default if there is
// none yet.
func (s *mcpServer) attach(ctx context.Context, id, sock string, pid int) (*Session, error) {
	if sock == "" {
		if id == "" {
			return nil, fmt.Errorf("no id or socket given")
		}
		sock = fmt.Sprintf("%s/magmux-%s.sock", sockDir, id)
	}
	if id == "" {
		id = strings.TrimSuffix(strings.TrimPrefix(sock, sockDir+"/magmux-"), ".sock")
	}

	s.sessMu.Lock()
	if existing, ok := s.sessions[id]; ok {
		s.sessMu.Unlock()
		return existing, nil
	}
	s.sessMu.Unlock()

	sess, err := dialSession(ctx, id, sock, pid)
	if err != nil {
		return nil, err
	}

	s.sessMu.Lock()
	if existing, ok := s.sessions[id]; ok {
		// Lost a race with another tool call; keep one connection per session.
		s.sessMu.Unlock()
		sess.Close()
		return existing, nil
	}
	s.sessions[id] = sess
	if s.defID == "" {
		s.defID = id
	}
	client := s.clientName
	s.sessMu.Unlock()

	// Announce who is driving, once per session, with the same `pilot` event the
	// pi pilot has always used — the promise being that anything speaking these
	// verbs fills the control panel in, with no MCP-specific verb.
	//
	// It is not decoration: `client` is what the panel names in its header, and
	// what makes it choose the routing layout (a controller driving N panes)
	// over the legacy single-lane one. Without this the panel could only ever
	// land on the legacy layout by accident, for a run that is not a pilot's.
	//
	// Fire-and-forget: it needs no reply, so it also works against a legacy
	// magmux, which has always understood `pilot`. Sent only when the client
	// named itself at initialize — an anonymous controller gains nothing from
	// the event, and `pilot start` resets the panel's counters.
	if client != "" {
		if err := sess.fire(map[string]any{
			"type": "pilot", "event": "start", "client": client,
		}); err != nil {
			s.logf("could not announce %q to session %s: %v", client, id, err)
		}
	}
	s.logf("attached session %s (%s), capabilities=%s", id, sock, sess.capsNote())
	return sess, nil
}

// ── pane resolution ─────────────────────────────────────────────────────────

// resolvePane turns a pane reference — an index, a numeric string, or a label
// — into an index magmux can be given. Labels are matched against the label a
// pane was opened with, then against its command.
func (s *mcpServer) resolvePane(ctx context.Context, sess *Session, ref any) (paneInfo, error) {
	panes, err := sess.listPanes(ctx)
	if err != nil {
		panes = sess.state.all()
	}
	s.markSelfPanes(panes)

	byIndex := func(idx int) (paneInfo, error) {
		for _, p := range panes {
			if p.Index == idx {
				if p.Closed {
					return paneInfo{}, fmt.Errorf("pane %d has been closed; %s", idx, paneMenu(panes))
				}
				return p, nil
			}
		}
		if len(panes) == 0 {
			return paneInfo{}, fmt.Errorf("this session reports no panes")
		}
		return paneInfo{}, fmt.Errorf("no pane %d; %s", idx, paneMenu(panes))
	}

	switch v := ref.(type) {
	case nil:
		return paneInfo{}, fmt.Errorf("no pane given; %s", paneMenu(panes))
	case float64:
		return byIndex(int(v))
	case int:
		return byIndex(v)
	case string:
		name := strings.TrimSpace(v)
		if name == "" {
			return paneInfo{}, fmt.Errorf("empty pane reference; %s", paneMenu(panes))
		}
		if idx, err := strconv.Atoi(name); err == nil {
			return byIndex(idx)
		}
		lower := strings.ToLower(name)
		for _, p := range panes {
			if strings.ToLower(p.Label) == lower && !p.Closed {
				return p, nil
			}
		}
		for _, p := range panes {
			if p.Closed {
				continue
			}
			if strings.Contains(strings.ToLower(p.Cmd), lower) {
				return p, nil
			}
		}
		return paneInfo{}, fmt.Errorf("no pane labelled %q; %s", name, paneMenu(panes))
	default:
		return paneInfo{}, fmt.Errorf("pane must be an index or a label, got %T", ref)
	}
}

func paneMenu(panes []paneInfo) string {
	if len(panes) == 0 {
		return "this session has no panes"
	}
	parts := make([]string, 0, len(panes))
	for _, p := range panes {
		if p.Closed {
			continue
		}
		desc := strconv.Itoa(p.Index)
		switch {
		case p.Label != "":
			desc += " (" + p.Label + ")"
		case p.Control:
			desc += " (control panel)"
		case p.Cmd != "":
			desc += " (" + mcpFirstWord(p.Cmd) + ")"
		}
		parts = append(parts, desc)
	}
	return "panes are: " + strings.Join(parts, ", ")
}

func mcpFirstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return s
	}
	return f[0]
}

// markSelfPanes flags panes whose process is one of our own ancestors, and
// reports whether that answer can be trusted.
//
// It cannot when the ancestry walk broke partway (an unreadable /proc entry, a
// restricted container) AND no pane matched what little was walked: the guard
// then has no way to know it would recognise our own pane, and an unmarked pane
// means "not ours" everywhere downstream. Finding our pane anyway settles it —
// the walk got far enough for the only question being asked.
func (s *mcpServer) markSelfPanes(panes []paneInfo) bool {
	anc, complete := s.ancestry()
	found := false
	for i := range panes {
		if panes[i].PID > 0 && anc[panes[i].PID] {
			panes[i].Self = true
			found = true
		}
	}
	return complete || found
}

// selfGuardWarning is what a pane listing says when markSelfPanes could not
// vouch for its answer. Saying nothing would present an unchecked list as a
// checked one, and the agent would drive its own pane believing the guard had
// looked — the deadlock the guard exists to prevent, arrived at politely.
const selfGuardWarning = "\n\nNOTE: this process's ancestry could not be read (a container, a " +
	"restricted /proc, or a transient failure), so magmux cannot tell which of these panes it " +
	"is running inside and has marked none of them as your own. Driving your own pane " +
	"deadlocks — the turn you would wait for is the one you are inside — so if one of these " +
	"panes is the agent making this call, do not send to it."

// refuseUndrivable rejects the panes that cannot be driven at all. The self
// check is the important one: an agent that instructs its own pane waits for a
// turn it is itself inside, so it waits forever.
func refuseUndrivable(p paneInfo) string {
	switch {
	case p.Self:
		return fmt.Sprintf("pane %d is the pane you are running in. Driving it would "+
			"deadlock: the turn you would wait for is the one you are inside. Open another "+
			"pane and drive that instead.", p.Index)
	case p.Control:
		return fmt.Sprintf("pane %d is magmux's control panel, not a session — it has no "+
			"process to type into.", p.Index)
	case p.State == "gone" || p.Dead:
		return fmt.Sprintf("pane %d's process has exited (exit code %d), so it cannot take "+
			"input. Read it to see how it ended, then close_pane it.", p.Index, p.ExitCode)
	}
	return ""
}

// refuseUnturnable rejects a pane that can be typed into but whose turns magmux
// cannot observe, i.e. one with no ToolController attached: a dev server, a
// shell, a REPL.
//
// It is deliberately NOT part of refuseUndrivable, which send_keys shares:
// typing into such a pane is fine and is exactly what send_keys is for. Only
// send_and_wait needs a controller, and without one it waits vacuously —
// buildPaneResults reports such a pane as "running", aggregateState maps that
// to "working", "working" is not settled, so phase one succeeds instantly
// without anything having happened and phase two then waits for a settled state
// that can only ever arrive from a controller snapshot or from the process
// exiting. Since send_and_wait's tool.timeout is 0, nothing else bounds it: the
// tool call blocks for the full turn timeout (15 minutes by default) and comes
// back "stalled", with the pane held by beginTurn the whole time.
func refuseUnturnable(p paneInfo) string {
	if p.Controller != "" {
		return ""
	}
	return fmt.Sprintf("pane %d is not running an agent magmux can follow (no controller is "+
		"attached to it), so there is no turn to wait for — send_and_wait would block until it "+
		"timed out. Use send_keys to type into it, then read_pane to see what happened.", p.Index)
}

// ── session tools ───────────────────────────────────────────────────────────

func toolListSessions(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}

	found := discoverSessions(ctx, time.Second)
	anc, ancComplete := s.ancestry()
	sawSelf := false
	host := os.Getenv("MAGMUX_SOCK")

	s.sessMu.Lock()
	def := s.defID
	attached := make(map[string]bool, len(s.sessions))
	for id, sess := range s.sessions {
		attached[id] = true
		_ = sess
	}
	s.sessMu.Unlock()

	var b strings.Builder
	if len(found) == 0 {
		b.WriteString("No magmux sockets found in /tmp.\n\nCall request_session for the " +
			"command that starts one.")
		return toolText(b.String()), nil
	}

	fmt.Fprintf(&b, "%d magmux socket(s) in /tmp:\n", len(found))
	for _, f := range found {
		mark := " "
		if f.SockPath == host || attached[f.ID] {
			mark = "*"
		}
		status := "reachable"
		switch {
		case f.Stale:
			status = "stale (nothing listening — the process was killed; leave the file alone)"
		case !f.Reachable && f.Err != "":
			status = "unusable: " + f.Err
		case !f.Reachable:
			status = "unreachable"
		}
		fmt.Fprintf(&b, "\n%s id=%s  sock=%s  %s", mark, f.ID, f.SockPath, status)
		if f.PID > 0 {
			fmt.Fprintf(&b, "  pid=%d alive=%v", f.PID, f.Alive)
		}
		if f.SockPath == host {
			b.WriteString("  [this is the magmux you are running inside]")
		}
		if f.ID == def {
			b.WriteString("  [default]")
		}
		for _, entry := range f.Panes {
			idx, _ := evInt(entry, "pane")
			state, _ := evStr(entry, "state")
			label, _ := evStr(entry, "label")
			cmd, _ := evStr(entry, "cmd")
			pid, _ := evInt(entry, "pid")
			line := fmt.Sprintf("\n    pane %d  %s", idx, aggregateState(state))
			if label != "" {
				line += "  " + label
			}
			if cmd != "" {
				line += "  " + mcpFirstWord(cmd)
			}
			if pid > 0 && anc[pid] {
				line += "  [your own pane — cannot be driven]"
				sawSelf = true
			}
			b.WriteString(line)
		}
	}
	if !ancComplete && !sawSelf {
		b.WriteString(selfGuardWarning)
	}
	b.WriteString("\n\nAttach with attach_session, or just call a pane tool: the session you " +
		"are running inside, or the only reachable one, is used automatically.")
	return toolText(b.String()), nil
}

func toolAttachSession(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		ID   string `json:"id"`
		PID  int    `json:"pid"`
		Sock string `json:"sock"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	if args.ID == "" && args.PID == 0 && args.Sock == "" {
		return toolResultError("give one of id, pid or sock. Call list_sessions to see what " +
			"is available."), nil
	}
	id, sock := args.ID, args.Sock
	if id == "" && args.PID > 0 {
		id = strconv.Itoa(args.PID)
	}
	sess, err := s.attach(ctx, id, sock, args.PID)
	if err != nil {
		return toolResultError("could not attach to %s: %v\n\nCall list_sessions to see which "+
			"sessions are reachable.", mcpFirstNonEmpty(id, sock), err), nil
	}

	s.sessMu.Lock()
	s.defID = sess.ID
	s.sessMu.Unlock()

	panes, listErr := sess.listPanes(ctx)
	reliable := s.markSelfPanes(panes)
	var b strings.Builder
	fmt.Fprintf(&b, "Attached to session %s (%s). It is now the default.\n", sess.ID, sess.SockPath)
	if sess.isLegacy(ctx) {
		b.WriteString("\nThis magmux is an older build without the request/reply socket " +
			"protocol: send_keys and send_and_wait work, everything else does not. Ask the " +
			"human to restart magmux with the current binary.\n")
	}
	if listErr != nil {
		fmt.Fprintf(&b, "\nCould not list panes: %v\n", listErr)
	}
	b.WriteString("\n" + renderPaneTable(panes))
	if !reliable {
		b.WriteString(selfGuardWarning)
	}
	return toolText(b.String()), nil
}

func toolRequestSession(_ context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		Name string `json:"name"`
		Cmd  string `json:"cmd"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		name = "agent"
	}
	cmd := strings.TrimSpace(args.Cmd)
	if cmd == "" {
		cmd = "claude"
	}
	start := fmt.Sprintf("magmux --id %s -c -e '%s'", name, cmd)

	if sock := os.Getenv("MAGMUX_SOCK"); sock != "" {
		return toolText(fmt.Sprintf(
			"You are already inside a magmux session (%s) and it is used automatically — "+
				"no new session is needed.\n\nCall list_panes to see it, and open_pane to add "+
				"panes to it. Remember you cannot drive your own pane.", sock)), nil
	}

	if os.Getenv("TMUX") != "" {
		return toolText(fmt.Sprintf(
			"No magmux is running, but you are inside tmux — so you can start one yourself "+
				"using YOUR OWN tmux tools (magmux never runs tmux).\n\n"+
				"1. Split the current tmux window into a new pane.\n"+
				"2. Run this in it:\n\n    %s\n\n"+
				"3. Then call attach_session with id \"%s\".\n\n"+
				"The -c flag adds magmux's control panel, so the human can watch every "+
				"instruction you send and what the session actually did.", start, name)), nil
	}

	return toolText(fmt.Sprintf(
		"No magmux session is running, and I do not start one myself: every pane you drive "+
			"is meant to be visible to a human, so a human starts the session.\n\n"+
			"Ask them to run:\n\n    %s\n\n"+
			"Then call attach_session with id \"%s\" (its socket will be /tmp/magmux-%s.sock), "+
			"or just call list_sessions again.\n\n"+
			"-c adds the control panel so they can see what you ask for and what the session "+
			"does about it.", start, name, name)), nil
}

// ── pane tools ──────────────────────────────────────────────────────────────

func toolListPanes(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	sess, err := s.resolveSession(ctx, args.SessionID)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	panes, err := sess.listPanes(ctx)
	if err != nil {
		return toolResultError("could not list panes of session %s: %v", sess.ID, err), nil
	}
	reliable := s.markSelfPanes(panes)
	body := fmt.Sprintf("session %s (%s)\n\n", sess.ID, sess.SockPath)
	body += renderPaneTable(panes)
	if !reliable {
		body += selfGuardWarning
	}
	return toolText(body), nil
}

func renderPaneTable(panes []paneInfo) string {
	if len(panes) == 0 {
		return "This session reports no panes."
	}
	sorted := append([]paneInfo(nil), panes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })

	var b strings.Builder
	b.WriteString("pane  state                label       command\n")
	for _, p := range sorted {
		state := p.State
		if p.Control {
			state = "panel"
		}
		if p.Closed {
			state = "closed"
		}
		fmt.Fprintf(&b, "%-4d  %-19s  %-10s  %s", p.Index, state, mcpTruncate(p.Label, 10), mcpTruncate(p.Cmd, 40))
		var notes []string
		if p.Self {
			notes = append(notes, "YOUR OWN PANE — cannot be driven")
		}
		if p.Dead {
			notes = append(notes, fmt.Sprintf("exited %d", p.ExitCode))
		}
		if p.Model != "" {
			notes = append(notes, p.Model)
		}
		if len(notes) > 0 {
			b.WriteString("  [" + strings.Join(notes, "; ") + "]")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func toolOpenPane(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		Cmd       string `json:"cmd"`
		Cwd       string `json:"cwd"`
		Label     string `json:"label"`
		Target    any    `json:"target"`
		Split     string `json:"split"`
		Focus     *bool  `json:"focus"`
		SessionID string `json:"session_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	if strings.TrimSpace(args.Cmd) == "" {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid arguments: cmd is required"}
	}
	switch args.Split {
	case "", "auto", "horizontal", "vertical":
	default:
		return nil, &rpcError{Code: rpcInvalidParams,
			Message: "invalid arguments: split must be auto, horizontal or vertical"}
	}

	sess, err := s.resolveSession(ctx, args.SessionID)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	if sess.isLegacy(ctx) {
		return toolResultError("%v", errLegacyMagmux), nil
	}

	req := map[string]any{"cmd": args.Cmd}
	if args.Cwd != "" {
		req["cwd"] = args.Cwd
	}
	if args.Label != "" {
		req["label"] = args.Label
	}
	if args.Split != "" && args.Split != "auto" {
		req["split"] = args.Split
	}
	if args.Focus != nil {
		req["focus"] = *args.Focus
	}
	// No target key when the caller named none: the omission is what selects
	// magmux's own default, and that default is the LARGEST live leaf, which is
	// what the schema promises. Sending an explicit target here — or magmux
	// defaulting to the focused pane — halves whatever the human happens to be
	// typing in, reflowing a Claude Code TUI mid-render. Schema text and
	// behaviour have to keep agreeing; see TestOpenPaneOmitsTargetWhenNotGiven.
	if args.Target != nil {
		// A target is a pane like any other, so it goes through the same
		// resolver — magmux must never see a label.
		t, err := s.resolvePane(ctx, sess, args.Target)
		if err != nil {
			return toolResultError("target: %v", err), nil
		}
		req["target"] = t.Index
	}

	res, err := sess.openPane(ctx, req)
	if err != nil {
		return toolResultError("could not open a pane: %v%s", err, openPaneHint(err)), nil
	}
	idx, ok := evInt(res, "pane")
	if !ok {
		return toolText(fmt.Sprintf("Opened a pane running %q, but magmux did not report its "+
			"index. Call list_panes to find it.", args.Cmd)), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Opened pane %d running %q", idx, args.Cmd)
	if args.Label != "" {
		fmt.Fprintf(&b, " labelled %q", args.Label)
	}
	if args.Cwd != "" {
		fmt.Fprintf(&b, " in %s", args.Cwd)
	}
	b.WriteString(".\n\nInteractive programs take a moment to draw their first screen. " +
		"Use read_pane to see it, or send_and_wait if it is an agent ready for instructions.")
	return toolText(b.String()), nil
}

func openPaneHint(err error) string {
	switch sockErrCode(err) {
	case "unknown_verb", "unsupported":
		return "\n\nThis magmux cannot open panes yet. Ask the human to add the pane by hand, " +
			"then call list_panes."
	case "too_small":
		return "\n\nThe layout has no room left. Close a pane first, or ask the human for a " +
			"bigger window."
	}
	return ""
}

func toolClosePane(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		Pane      any    `json:"pane"`
		Force     bool   `json:"force"`
		SessionID string `json:"session_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	sess, err := s.resolveSession(ctx, args.SessionID)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	if sess.isLegacy(ctx) {
		return toolResultError("%v", errLegacyMagmux), nil
	}
	p, err := s.resolvePane(ctx, sess, args.Pane)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	if p.Self {
		return toolResultError("pane %d is the pane you are running in — closing it would "+
			"kill you.", p.Index), nil
	}
	if p.Control {
		return toolResultError("pane %d is magmux's control panel; it is the human's view of "+
			"what you are doing and is not yours to close.", p.Index), nil
	}
	if _, err := sess.closePane(ctx, p.Index, args.Force); err != nil {
		return toolResultError("could not close pane %d: %v", p.Index, err), nil
	}
	return toolText(fmt.Sprintf("Closed pane %d. Pane indices do not shift, so every other "+
		"pane keeps the index you already know.", p.Index)), nil
}

// toolReadPane serves a pane from its two independent sources, and keeps them
// visibly apart.
//
// They answer different questions and only one of them is authoritative. The
// transcript is what the session ITSELF recorded: full text, every tool's
// input and its result, and turns that have long since scrolled away. The
// screen is a rendering — wrapped, redrawn, possibly a TUI mid-frame — and for
// a pane with no agent in it, the only thing there is. An agent that confuses
// the two plans against something a session never said, so the labelling here
// is load-bearing rather than cosmetic.
func toolReadPane(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		Pane       any    `json:"pane"`
		Screen     *bool  `json:"screen"`
		Lines      int    `json:"lines"`
		Offset     int    `json:"offset"`
		Transcript int    `json:"transcript"`
		SessionID  string `json:"session_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	sess, err := s.resolveSession(ctx, args.SessionID)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	p, err := s.resolvePane(ctx, sess, args.Pane)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	wantScreen := args.Screen == nil || *args.Screen
	wantTranscript := args.Transcript > 0

	// The transcript is fetched first because it decides what the header says:
	// when it comes through, it carries the last response in full and the
	// truncated one-line summary would be nothing but a worse copy of it.
	transcriptText, transcriptOK := "", false
	if wantTranscript {
		transcriptText, transcriptOK = readPaneTranscript(ctx, sess, p, args.Transcript)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "pane %d", p.Index)
	if p.Label != "" {
		fmt.Fprintf(&b, " (%s)", p.Label)
	}
	fmt.Fprintf(&b, "\nstate: %s", p.State)
	if p.Dead {
		fmt.Fprintf(&b, " (process exited with %d)", p.ExitCode)
	}
	if p.Cmd != "" {
		fmt.Fprintf(&b, "\ncommand: %s", p.Cmd)
	}
	if p.Model != "" {
		fmt.Fprintf(&b, "\nmodel: %s", p.Model)
	}
	if p.Tool != "" {
		fmt.Fprintf(&b, "\nlast tool: %s", p.Tool)
	}
	if p.Response != "" && !transcriptOK {
		// A one-line summary, and it says so. This used to be the ONLY way to
		// see what a session had said, which is why a 300-character cut was
		// worth a bug: the answer to "did it finish the migration" routinely
		// lives past character 300. It is a summary again now that there is
		// somewhere to send a caller who wants the whole thing.
		flat := mcpFlatten(p.Response)
		fmt.Fprintf(&b, "\nlast response: %s", mcpTruncate(flat, 300))
		// Counted in runes, like the cut itself: telling a model it is seeing
		// "the first 300 of 412 characters" when 412 was a byte count sends it
		// after text that is not missing.
		if total := utf8.RuneCountInString(flat); total > 300 {
			fmt.Fprintf(&b, "\n  (that is the first 300 of %d characters — call read_pane with "+
				"transcript:1 for the full text)", total)
		}
	}

	if wantTranscript {
		b.WriteString("\n\n" + transcriptText)
	}

	if !wantScreen {
		out := toolText(b.String())
		if wantTranscript && !transcriptOK {
			out["isError"] = true
		}
		return out, nil
	}
	if sess.isLegacy(ctx) {
		b.WriteString("\n\nThis magmux is too old to render a pane's screen, so state above is " +
			"all there is. Ask the human to restart magmux with the current binary.")
		return toolText(b.String()), nil
	}

	res, err := sess.capture(ctx, p.Index, args.Lines, args.Offset)
	if err != nil {
		b.WriteString("\n\nCould not read the screen: " + err.Error())
		return toolResultError("%s", b.String()), nil
	}
	text, _ := evStr(res, "text")
	rows, _ := evInt(res, "rows")
	cols, _ := evInt(res, "cols")
	// The offset magmux SETTLED on, not the one asked for: it clamps to the
	// history that exists, and a model that asked for 5000 and was silently
	// given 300 would otherwise conclude the pane repeats itself.
	offset, _ := evInt(res, "offset")
	scrollback, _ := evInt(res, "scrollback")
	alt := evBool(res, "alt")

	if offset > 0 {
		fmt.Fprintf(&b, "\n\n══ SCREEN — pane %d, %d rows back in its scrollback ══", p.Index, offset)
	} else {
		fmt.Fprintf(&b, "\n\n══ SCREEN — what pane %d is painting right now ══", p.Index)
	}
	if rows > 0 && cols > 0 {
		fmt.Fprintf(&b, "\n%dx%d", cols, rows)
	}
	if alt {
		b.WriteString(" (alternate screen)")
	}
	if evBool(res, "truncated") {
		b.WriteString(" — truncated to the last requested rows")
	}
	b.WriteString("\n\n```\n" + strings.TrimRight(text, "\n") + "\n```")

	// What is behind this screenful, said plainly enough to act on. The three
	// cases lead a reader to three different next steps, which is why they are
	// three sentences and not one hedge.
	switch {
	case alt:
		b.WriteString("\n\nThis pane is on the alternate screen (a full-screen app), which by " +
			"design records no scrollback — so this is all the screen can show you.")
		if !wantTranscript {
			b.WriteString(" If it is an agent session, read_pane with transcript:N is its history.")
		}
	case scrollback <= 0:
		b.WriteString("\n\nNothing has scrolled off this pane yet, so this is its whole output.")
	case evBool(res, "atTop"):
		fmt.Fprintf(&b, "\n\nThat is the oldest of the %d rows of scrollback magmux still holds "+
			"for this pane; anything before it has been dropped.", scrollback)
	default:
		fmt.Fprintf(&b, "\n\n%d rows of scrollback sit above the live screen and %d of them are "+
			"still further back than this; call read_pane again with offset:%d to keep going.",
			scrollback, scrollback-offset, offset+rows)
	}
	if wantTranscript && transcriptOK {
		b.WriteString(" The screen is a rendering and may lag, wrap or be mid-redraw; where the " +
			"two disagree, the transcript above is what the session actually said.")
	}

	out := toolText(b.String())
	if wantTranscript && !transcriptOK {
		out["isError"] = true
	}
	return out, nil
}

// Presentation caps for the transcript section. These exist only here, at the
// boundary that has a context window to defend — the controller and the socket
// deliberately truncate nothing.
const (
	// transcriptSectionCap bounds the whole section. A driver that asks for 50
	// turns must not be able to blow its own context with one call, so the
	// OLDEST turns are dropped until it fits and the reply says how many went.
	transcriptSectionCap = 16000

	// transcriptTurnTextCap is the hard stop on one turn's own words. Well
	// above any real answer: this is a runaway guard, not an editorial one, and
	// the 300-character cut it replaces was the bug being fixed.
	transcriptTurnTextCap = 20000

	// transcriptToolFieldCap bounds ONE tool input or ONE tool result. Tool
	// payloads are where a transcript's bulk lives — a Read of a large file, a
	// full test log — and they are the part a driver most often needs only the
	// shape of. Both are marked with what was left out.
	transcriptToolFieldCap = 600
)

// readPaneTranscript fetches and renders the transcript section, returning it
// and whether it actually carries turns.
//
// Every failure returns text that says what went wrong and what to do about
// it, because the alternatives are all worse: an empty section reads as "the
// session said nothing", and dropping the section entirely reads as "you did
// not ask". Both are answers about the SESSION to a question that failed on
// MAGMUX's side.
func readPaneTranscript(ctx context.Context, sess *Session, p paneInfo, turns int) (string, bool) {
	head := fmt.Sprintf("══ TRANSCRIPT — pane %d's own record on disk ══", p.Index)
	if sess.isLegacy(ctx) {
		return head + "\nThis magmux predates transcripts, so there is no record to read. Ask " +
			"the human to restart magmux with the current binary; until then the screen below " +
			"is all there is.", false
	}

	got, err := sess.transcript(ctx, p.Index, turns)
	if err != nil {
		return head + "\n" + transcriptFailure(p, err), false
	}
	if len(got) == 0 {
		return head + "\nThe record was found and holds no turns yet — the session has started " +
			"but has not been given anything to do. This is a real answer, not a lookup " +
			"failure.", true
	}
	return head + "\n" + renderTranscript(got, turns), true
}

// transcriptFailure turns magmux's error code into the one sentence that tells
// the model what it is actually looking at. The three codes lead to three
// different next steps, which is the whole reason they are separate codes.
func transcriptFailure(p paneInfo, err error) string {
	switch sockErrCode(err) {
	case "no_controller":
		return fmt.Sprintf("Pane %d is not running an agent magmux follows — a shell, a REPL and "+
			"a dev server have no transcript. Nothing is wrong with the pane: read its screen "+
			"instead (read_pane with screen:true, which is the default).", p.Index)
	case "unsupported":
		return fmt.Sprintf("The controller watching pane %d cannot produce a transcript. Read "+
			"the screen instead.", p.Index)
	case "no_transcript":
		return fmt.Sprintf("magmux has NOT located this session's record yet, so it cannot show "+
			"you what was said. This is not the same as the session having said nothing — do "+
			"not read it as an empty session. Discovery lags for a second or two after a "+
			"session starts and can fail outright; retry, or read pane %d's screen.", p.Index)
	case "unknown_verb", "unsupported_verb":
		return "This magmux is too old to serve transcripts. Ask the human to restart it with " +
			"the current binary; the screen below still works."
	}
	return "Could not read the transcript: " + err.Error() + ". The screen below is unaffected."
}

// renderTranscript lays the turns out oldest first and enforces the payload
// cap by dropping the OLDEST turns — the newest is what a driver acted on last
// and is never the one to lose.
func renderTranscript(turns []transcriptTurn, requested int) string {
	blocks := make([]string, len(turns))
	for i, t := range turns {
		blocks[i] = renderTranscriptTurn(t)
	}
	// Walk backwards accumulating until the cap; the newest block is always
	// kept, even alone and even oversized (its own text cap already bounds it).
	first, total := len(blocks)-1, 0
	for i := len(blocks) - 1; i >= 0; i-- {
		if i < len(blocks)-1 && total+len(blocks[i]) > transcriptSectionCap {
			break
		}
		total += len(blocks[i])
		first = i
	}

	var b strings.Builder
	kept := blocks[first:]
	fmt.Fprintf(&b, "%d turn(s), oldest first.", len(kept))
	if dropped := first; dropped > 0 {
		fmt.Fprintf(&b, " %d older turn(s) were dropped to keep this reply under %d characters — "+
			"ask for fewer turns to see them without the newest crowding them out.",
			dropped, transcriptSectionCap)
	} else if len(turns) < requested {
		fmt.Fprintf(&b, " You asked for %d; the session has no more history than this.", requested)
	}
	for _, block := range kept {
		b.WriteString("\n\n" + block)
	}
	return b.String()
}

func renderTranscriptTurn(t transcriptTurn) string {
	var b strings.Builder
	role := t.Role
	if role == "" {
		role = "unknown"
	}
	b.WriteString("[" + role + "]")
	if t.Timestamp != "" {
		b.WriteString(" " + t.Timestamp)
	}
	switch {
	case t.Text != "":
		b.WriteString("\n" + mcpClip(t.Text, transcriptTurnTextCap))
	case len(t.Tools) == 0:
		b.WriteString("\n(no text recorded for this turn)")
	default:
		b.WriteString("\n(no text — this turn was tool calls only, which is normal)")
	}
	for _, call := range t.Tools {
		name := call.Name
		if name == "" {
			name = "(unnamed tool)"
		}
		fmt.Fprintf(&b, "\n  · %s", name)
		if call.Input != "" {
			fmt.Fprintf(&b, "\n    input:  %s", mcpFlatten(mcpClip(call.Input, transcriptToolFieldCap)))
		}
		if call.Result == "" {
			b.WriteString("\n    result: (none recorded — the tool had not returned when this was read)")
			continue
		}
		b.WriteString("\n    result:\n" + indentLines(mcpClip(call.Result, transcriptToolFieldCap), "      "))
	}
	return b.String()
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func toolSendKeys(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		Pane      any      `json:"pane"`
		Text      string   `json:"text"`
		Keys      []string `json:"keys"`
		Enter     *bool    `json:"enter"`
		Wait      int      `json:"wait"`
		SessionID string   `json:"session_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	if args.Text == "" && len(args.Keys) == 0 {
		return nil, &rpcError{Code: rpcInvalidParams,
			Message: "invalid arguments: give text, keys, or both"}
	}
	sess, err := s.resolveSession(ctx, args.SessionID)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	p, err := s.resolvePane(ctx, sess, args.Pane)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	if why := refuseUndrivable(p); why != "" {
		return toolResultError("%s", why), nil
	}

	// Enter defaults to true for text (that is what submits it) and to false
	// for a bare key press, where an unrequested Return would answer a prompt
	// the caller had not read.
	enter := args.Text != "" && len(args.Keys) == 0
	if args.Enter != nil {
		enter = *args.Enter
	}

	if err := sess.sendKeys(ctx, p.Index, args.Text, args.Keys, enter, "send_keys"); err != nil {
		return toolResultError("could not send to pane %d: %v", p.Index, err), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Sent to pane %d", p.Index)
	if args.Text != "" {
		fmt.Fprintf(&b, ": %q", mcpTruncate(mcpFlatten(args.Text), 120))
	}
	if len(args.Keys) > 0 {
		fmt.Fprintf(&b, " keys: %s", strings.Join(args.Keys, " "))
	}
	if enter {
		b.WriteString(" then Enter")
	}
	b.WriteString(".")
	if sess.isLegacy(ctx) {
		b.WriteString("\n\n(This magmux cannot confirm delivery — it predates the reply " +
			"protocol. Read the pane to check.)")
	}

	if args.Wait > 0 {
		wait := time.Duration(args.Wait) * time.Millisecond
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		if !sess.isLegacy(ctx) {
			if res, err := sess.capture(ctx, p.Index, 20, 0); err == nil {
				if text, _ := evStr(res, "text"); strings.TrimSpace(text) != "" {
					b.WriteString("\n\nThe pane now shows:\n```\n" +
						strings.TrimRight(text, "\n") + "\n```")
				}
			}
		}
	} else {
		b.WriteString("\n\nNothing was waited for. Use read_pane to see the effect, or " +
			"send_and_wait when you want a whole turn.")
	}
	return toolText(b.String()), nil
}

func toolSendAndWait(ctx context.Context, s *mcpServer, raw json.RawMessage) (map[string]any, *rpcError) {
	var args struct {
		Pane           any    `json:"pane"`
		Instruction    string `json:"instruction"`
		Label          string `json:"label"`
		StartTimeoutMs int    `json:"start_timeout_ms"`
		TurnTimeoutMs  int    `json:"turn_timeout_ms"`
		SessionID      string `json:"session_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, invalidParams(err)
	}
	if strings.TrimSpace(args.Instruction) == "" {
		return nil, &rpcError{Code: rpcInvalidParams,
			Message: "invalid arguments: instruction is required"}
	}
	sess, err := s.resolveSession(ctx, args.SessionID)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	p, err := s.resolvePane(ctx, sess, args.Pane)
	if err != nil {
		return toolResultError("%v", err), nil
	}
	if why := refuseUndrivable(p); why != "" {
		return toolResultError("%s", why), nil
	}
	if why := refuseUnturnable(p); why != "" {
		return toolResultError("%s", why), nil
	}

	// Two concurrent turns on one pane is nonsense: the second would watch the
	// first one's turn and report its answer.
	if !sess.beginTurn(p.Index) {
		return toolResultError("pane %d already has a send_and_wait in flight. Wait for it to "+
			"return before sending another instruction to that pane.", p.Index), nil
	}
	defer sess.endTurn(p.Index)

	startTimeout := defaultStartTimeout
	if args.StartTimeoutMs > 0 {
		startTimeout = time.Duration(args.StartTimeoutMs) * time.Millisecond
	}
	turnTimeout := defaultTurnTimeout
	if args.TurnTimeoutMs > 0 {
		turnTimeout = time.Duration(args.TurnTimeoutMs) * time.Millisecond
	}
	label := args.Label
	if label == "" {
		label = "send_and_wait"
	}

	send := func() error {
		// The send itself gets its own deadline: a session that never
		// acknowledges the bytes is a different failure from one that never
		// starts a turn, and they read differently to the driver.
		sendCtx, cancel := context.WithTimeout(ctx, sockSendTimeout)
		defer cancel()
		return sess.sendKeys(sendCtx, p.Index, args.Instruction, nil, true, label)
	}

	res, err := runInstruction(ctx, sess.state, p.Index, send, startTimeout, turnTimeout)
	if err != nil {
		return toolResultError("could not deliver the instruction to pane %d: %v", p.Index, err), nil
	}

	// A turn that settles with nothing to say used to leave the driver
	// guessing; now that capture exists, show it the screen instead.
	screen := ""
	if res.Response == "" && !sess.isLegacy(ctx) {
		capCtx, cancel := context.WithTimeout(context.Background(), sockReadTimeout)
		if cres, cerr := sess.capture(capCtx, p.Index, 15, 0); cerr == nil {
			screen, _ = evStr(cres, "text")
		}
		cancel()
	}

	text := describeTurn(res, screen)
	out := toolText(fmt.Sprintf("pane %d · %s · %.0fs\n\n%s",
		p.Index, res.State, res.Duration.Seconds(), text))
	if res.Stalled || res.State == "error" || res.State == "gone" {
		out["isError"] = true
	}
	return out, nil
}

// ── small helpers ───────────────────────────────────────────────────────────

// mcpTruncate shortens s to n CHARACTERS, the last of which is the ellipsis.
//
// Characters and not bytes, for two reasons that happen to agree. The cut has
// to land on a rune boundary — an em dash or an emoji straddling the cut point
// becomes U+FFFD once json.Marshal sees it, and the model then reads back a
// mangled quote of what a session said (p.Response is cut at 300, where a real
// answer is very likely to have one). And the two callers that pad the result
// into a column (`%-10s`, and the width of the command column) are measured by
// fmt in runes as well, so a byte budget would misalign the table on exactly
// the same strings.
//
// mcpClip is NOT interchangeable with this: it appends "[clipped: N more
// characters]", which is right for a transcript payload and wrong inside a
// table cell or a quoted echo of what was sent.
func mcpTruncate(s string, n int) string {
	if n <= 1 || utf8.RuneCountInString(s) <= n {
		return s
	}
	cut, seen := len(s), 0
	for i := range s {
		if seen == n-1 {
			cut = i
			break
		}
		seen++
	}
	return s[:cut] + "…"
}

// mcpClip shortens s to n bytes and says how much it left out, so a model can
// tell "the tool returned this" from "the tool returned this and more".
// Rune-aware, because cutting a transcript mid-codepoint puts a replacement
// character into a payload the model then quotes back.
func mcpClip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… [clipped: %d more characters]", s[:cut], len(s)-cut)
}

func mcpFlatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func mcpFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
