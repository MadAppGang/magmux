package main

// The magmux socket client used by `magmux mcp`.
//
// magmux speaks line-delimited JSON on a unix socket. Every connection is
// automatically a subscriber, and the first line a connection receives is
// guaranteed to be an aggregate snapshot of every pane
// (`{"type":"snapshot","panes":[...]}`, main.go:3151-3183). A message carrying
// an `id` gets exactly one `reply` unicast back to the sending connection;
// a message without one behaves as it always did, silently.
//
// The ingest rules below are a port of pilot/magmux.ts:112-154. They must stay
// a port: the aggregate-snapshot seeding in particular is the only thing that
// stops a late-attaching client waiting forever for a state it already missed,
// because magmux pushes per-pane snapshots on *change* only — a session
// already sitting at awaiting_input emits nothing further, ever.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Reply timeouts. Reads are cheap and local; open_pane pays for a fork/exec;
// send pays pilotSendDelay plus a possibly-busy TUI.
const (
	sockReadTimeout      = 5 * time.Second
	sockLifecycleTimeout = 10 * time.Second
	sockSendTimeout      = 20 * time.Second
	sockProbeTimeout     = 1500 * time.Millisecond
)

// Default two-phase waits, matching pilot/magmux.ts.
const (
	defaultStartTimeout = 45 * time.Second
	defaultTurnTimeout  = 15 * time.Minute
)

// settledStates are the states in which a session has finished with a turn and
// can accept another. Kept identical to magmux.ts's SETTLED.
var settledStates = map[string]bool{
	"awaiting_input":      true,
	"awaiting_permission": true,
	"error":               true,
	"gone":                true,
}

// errLegacyMagmux is returned for every verb that needs the reply plumbing
// when the magmux on the other end predates it. `send` still works, because it
// needs nothing but a fire-and-forget write plus the broadcasts magmux has
// always emitted — which is exactly the pilot's proven capability set.
var errLegacyMagmux = errors.New(
	"this magmux predates the request/reply socket protocol, so only send_keys and " +
		"send_and_wait work against it — ask the human to restart magmux with the current binary")

// mcpReply is a `reply` event: magmux's answer to one request.
//
// Named for this file rather than the wire type in main.go on purpose — the
// server is a separate process and shares no types with the multiplexer.
type mcpReply struct {
	Type   string          `json:"type"`
	ID     json.RawMessage `json:"id"`
	OK     bool            `json:"ok"`
	Result map[string]any  `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// sockRequestError is a structured failure reported by magmux itself. The code
// is one of magmux's error codes (no_such_pane, pane_dead, unknown_verb, …)
// and is worth showing the model: "no_such_pane" and "pane_dead" call for
// different recoveries.
type sockRequestError struct {
	Code string
	Msg  string
}

func (e *sockRequestError) Error() string {
	if e.Msg == "" {
		return e.Code
	}
	if e.Code == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s (%s)", e.Msg, e.Code)
}

func sockErrCode(err error) string {
	var re *sockRequestError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

// ── pane state ──────────────────────────────────────────────────────────────

// paneInfo is everything the client knows about one pane. Fields are filled
// from whichever source spoke last: the connect-time aggregate, a per-pane
// snapshot, an `exit` event, or a `list` reply.
type paneInfo struct {
	Index      int
	State      string
	Label      string
	Cmd        string
	Cwd        string
	PID        int
	Controller string
	Model      string
	Project    string
	Prompt     string
	Response   string
	Tool       string
	ExitCode   int
	Dead       bool
	Control    bool
	Closed     bool
	Focused    bool
	Rows       int
	Cols       int
	Alt        bool
	Self       bool // set by the MCP layer, not by magmux
}

// aggregateState translates magmux's pane-level vocabulary (built from
// dead/exitCode/inputReady in buildPaneResults) into the controller lifecycle
// names the live per-pane snapshots carry, so the rest of the server reasons
// about exactly one set of state names.
func aggregateState(s string) string {
	switch s {
	case "completed", "failed":
		return "gone"
	case "running":
		return "working"
	default:
		// "awaiting_input" and the controller names pass through unchanged.
		return s
	}
}

// sessionState is the client's view of every pane, plus a broadcast channel so
// waiters re-check their predicate on every event. It is the Go equivalent of
// magmux.ts's EventEmitter + waitFor.
type sessionState struct {
	mu     sync.Mutex
	panes  map[int]*paneInfo
	order  []int
	ended  bool
	notify chan struct{}
}

func newSessionState() *sessionState {
	return &sessionState{panes: map[int]*paneInfo{}, notify: make(chan struct{})}
}

// bumpLocked wakes every waiter. The closed-and-replaced channel is a
// broadcast with no per-waiter bookkeeping: a waiter that grabbed the old
// channel sees it close, and one that arrives later grabs the new one.
// Caller holds mu.
func (st *sessionState) bumpLocked() {
	close(st.notify)
	st.notify = make(chan struct{})
}

func (st *sessionState) sub() <-chan struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.notify
}

// paneLocked returns the entry for idx, creating it if needed. Caller holds mu.
func (st *sessionState) paneLocked(idx int) *paneInfo {
	if p, ok := st.panes[idx]; ok {
		return p
	}
	p := &paneInfo{Index: idx, State: "unknown"}
	st.panes[idx] = p
	st.order = append(st.order, idx)
	return p
}

// seedAggregate applies an aggregate `snapshot`/`results` payload: the whole
// pane table in one event, in magmux's pane-level vocabulary.
func (st *sessionState) seedAggregate(entries []any) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, raw := range entries {
		e, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx, ok := evInt(e, "pane")
		if !ok {
			continue
		}
		p := st.paneLocked(idx)
		if s, ok := evStr(e, "state"); ok {
			p.State = aggregateState(s)
		}
		if evBool(e, "control") || p.State == "panel" {
			// The control panel has no process and never takes a turn; every
			// "every pane" loop has to skip it.
			p.Control = true
		}
		if p.State == "closed" {
			p.Closed = true
		}
		st.mergeCommonLocked(p, e)
	}
	st.bumpLocked()
}

// applyPane applies a per-pane live `snapshot` (singular `pane`, no `panes`),
// which is the only event that tracks a turn.
func (st *sessionState) applyPane(e map[string]any) {
	idx, ok := evInt(e, "pane")
	if !ok {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	p := st.paneLocked(idx)
	if s, ok := evStr(e, "state"); ok && s != "" {
		p.State = s
	}
	// `tool` is cleared rather than merged: it describes the current turn, and
	// magmux.ts resets it on every per-pane snapshot for exactly that reason.
	tool, _ := evStr(e, "tool")
	p.Tool = tool
	st.mergeCommonLocked(p, e)
	// `response` is cleared too — and this is the ONE place the client
	// deliberately stops being a port of pilot/magmux.ts:145.
	//
	// magmux zeroes LastResponse at the start of every turn
	// (controller_claude.go) and pollControllers puts the `response` key on
	// every per-pane snapshot, so `"response":""` here is magmux saying "this
	// turn has produced no text yet". Keeping the pilot's sticky merge on this
	// path means a turn that is pure tool calls — Edit + Bash, no assistant
	// text, entirely routine — reports the PREVIOUS turn's answer, which is the
	// single thing send_and_wait's two-phase wait promises cannot happen.
	//
	// The pilot can live with that: one pane, and a human watching it. The MCP
	// client is autonomous across N panes, so a stale answer is planned against
	// rather than noticed. The aggregate seed keeps the sticky rule (see
	// mergeCommonLocked) because buildPaneResults genuinely omits the key when
	// it is empty — the two paths differ on purpose; do not collapse them.
	if v, ok := evStr(e, "response"); ok {
		p.Response = v
	}
	st.bumpLocked()
}

// mergeCommonLocked copies the fields that are optional-and-sticky: magmux
// omits them when empty, and an omitted field means "unchanged", not "gone".
// This is the aggregate rule — buildPaneResults writes each of these keys only
// when it has a value, so an absent key carries no information at all.
//
// `response` is the exception that proves it: it is sticky here, and NOT sticky
// on the per-pane snapshot path, where magmux emits the key unconditionally.
// See applyPane.
//
// Caller holds mu.
func (st *sessionState) mergeCommonLocked(p *paneInfo, e map[string]any) {
	if v, ok := evStr(e, "response"); ok && v != "" {
		p.Response = v
	}
	if v, ok := evStr(e, "prompt"); ok && v != "" {
		p.Prompt = v
	}
	if v, ok := evStr(e, "model"); ok && v != "" {
		p.Model = v
	}
	if v, ok := evStr(e, "project"); ok && v != "" {
		p.Project = v
	}
	if v, ok := evStr(e, "controller"); ok && v != "" {
		p.Controller = v
	}
	if v, ok := evStr(e, "label"); ok && v != "" {
		p.Label = v
	}
	if v, ok := evStr(e, "cmd"); ok && v != "" {
		p.Cmd = v
	}
	if v, ok := evStr(e, "cwd"); ok && v != "" {
		p.Cwd = v
	}
	if v, ok := evInt(e, "pid"); ok && v > 0 {
		p.PID = v
	}
	if v, ok := evInt(e, "exitCode"); ok {
		p.ExitCode = v
	}
	if v, ok := evInt(e, "rows"); ok && v > 0 {
		p.Rows = v
	}
	if v, ok := evInt(e, "cols"); ok && v > 0 {
		p.Cols = v
	}
	if _, has := e["dead"]; has {
		p.Dead = evBool(e, "dead")
	}
	if _, has := e["altMode"]; has {
		p.Alt = evBool(e, "altMode")
	}
	if _, has := e["focused"]; has {
		p.Focused = evBool(e, "focused")
	}
}

func (st *sessionState) markExit(idx, code int) {
	if idx < 0 {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	p := st.paneLocked(idx)
	p.State = "gone"
	p.Dead = true
	p.ExitCode = code
	st.bumpLocked()
}

// markEnded records that magmux is going away. Waiters re-check their
// predicate once more and then give up rather than burning their full timeout,
// mirroring magmux.ts's `onClosed => done(pred())`.
func (st *sessionState) markEnded() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ended {
		return
	}
	st.ended = true
	st.bumpLocked()
}

func (st *sessionState) isEnded() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.ended
}

// pane returns a copy of one pane's state.
func (st *sessionState) pane(idx int) (paneInfo, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.panes[idx]
	if !ok {
		return paneInfo{}, false
	}
	return *p, true
}

// all returns a copy of every pane, in the order magmux first mentioned them.
func (st *sessionState) all() []paneInfo {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]paneInfo, 0, len(st.order))
	for _, idx := range st.order {
		if p, ok := st.panes[idx]; ok {
			out = append(out, *p)
		}
	}
	return out
}

func (st *sessionState) paneState(idx int) string {
	if p, ok := st.pane(idx); ok {
		return p.State
	}
	return "unknown"
}

func (st *sessionState) response(idx int) string {
	if p, ok := st.pane(idx); ok {
		return p.Response
	}
	return ""
}

func (st *sessionState) tool(idx int) string {
	if p, ok := st.pane(idx); ok {
		return p.Tool
	}
	return ""
}

// wait blocks until pred holds, re-checking on every event from magmux.
// Returns pred's final value: false means it timed out, the context was
// cancelled, or the session ended first.
//
// pred must not be called with st.mu held, so it may use the accessors above.
func (st *sessionState) wait(ctx context.Context, pred func() bool, timeout time.Duration) bool {
	if pred() {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		// Subscribe *before* re-checking, so an event landing between the
		// check and the select cannot be missed.
		ch := st.sub()
		if pred() {
			return true
		}
		if st.isEnded() {
			return pred()
		}
		select {
		case <-ch:
			if pred() {
				return true
			}
		case <-ctx.Done():
			return pred()
		case <-timer.C:
			return pred()
		}
	}
}

// ── session ─────────────────────────────────────────────────────────────────

// Session is one attached magmux, its socket connection, and the client's view
// of its panes. One reader goroutine owns the scanner; everything else talks to
// it through state and pending.
type Session struct {
	ID, SockPath string
	PID          int
	Owned        bool // reserved: we never spawn magmux ourselves
	legacy       bool // no reply plumbing on the other end

	conn    net.Conn
	writeMu sync.Mutex
	nextID  atomic.Uint64
	pending map[string]chan mcpReply
	pendMu  sync.Mutex
	state   *sessionState
	closed  chan struct{}
	once    sync.Once

	// caps is the `capabilities` reply, kept for list_sessions and for
	// explaining refusals.
	caps map[string]any

	// turnMu guards inFlight: two concurrent send_and_wait on one pane is
	// nonsense — the second would watch the first one's turn.
	turnMu   sync.Mutex
	inFlight map[int]bool
}

// dialSession connects, consumes the guaranteed connect-time aggregate
// snapshot, starts the reader, and probes for the reply plumbing.
func dialSession(ctx context.Context, id, sockPath string, pid int) (*Session, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:       id,
		SockPath: sockPath,
		PID:      pid,
		conn:     conn,
		pending:  map[string]chan mcpReply{},
		state:    newSessionState(),
		closed:   make(chan struct{}),
		inFlight: map[int]bool{},
	}

	br := bufio.NewReaderSize(conn, 64*1024)
	_ = conn.SetReadDeadline(time.Now().Add(sockReadTimeout))
	first, err := br.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("no connect-time snapshot from %s: %w", sockPath, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	s.ingest(first)

	go s.readLoop(br)
	s.probeCapabilities(ctx)
	return s, nil
}

// readLoop owns the scanner for the life of the connection.
func (s *Session) readLoop(br *bufio.Reader) {
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		s.ingest(append([]byte(nil), line...))
	}
	// EOF or a read error: magmux is gone. Wake every waiter rather than
	// leaving them to burn a fifteen-minute timeout.
	s.state.markEnded()
	s.close(false)
}

// ingest applies one line from magmux. A port of pilot/magmux.ts:112-154.
func (s *Session) ingest(line []byte) {
	var ev map[string]any
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	typ, _ := evStr(ev, "type")
	switch typ {
	case "snapshot":
		// The aggregate carries `panes`; the per-pane live event carries
		// `pane`. Subscribers disambiguate on that, and only the latter tracks
		// a turn.
		if arr, ok := ev["panes"].([]any); ok {
			s.state.seedAggregate(arr)
			return
		}
		if _, ok := ev["pane"]; ok {
			s.state.applyPane(ev)
		}
	case "exit":
		idx, ok := evInt(ev, "pane")
		if !ok {
			return
		}
		code, _ := evInt(ev, "exitCode")
		s.state.markExit(idx, code)
	case "results":
		if arr, ok := ev["panes"].([]any); ok {
			s.state.seedAggregate(arr)
		}
		s.state.markEnded()
	case "shutdown":
		s.state.markEnded()
	case "reply":
		s.routeReply(line)
	default:
		// `control`, `pilot` and anything we do not know about: ignore. A
		// reply never touches pane state, and pane state never comes from a
		// controller's own claims.
	}
}

// routeReply hands a reply to whoever is waiting on its id. It deliberately
// touches no pane state: a controller must never be able to fabricate an
// observation about a session.
func (s *Session) routeReply(line []byte) {
	var r mcpReply
	if err := json.Unmarshal(line, &r); err != nil {
		return
	}
	key := replyKey(r.ID)
	if key == "" {
		return
	}
	s.pendMu.Lock()
	ch, ok := s.pending[key]
	s.pendMu.Unlock()
	if !ok {
		return // late reply for an abandoned request
	}
	select {
	case ch <- r:
	default:
	}
}

// replyKey normalises an id literal to the string form used in pending.
// magmux echoes ids verbatim, so `"7"` and `7` must map to the same key.
func replyKey(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	return s
}

// probeCapabilities decides whether this magmux has the reply plumbing.
//
// Negotiation by timeout is ugly, but a protocol with no error replies leaves
// no alternative: a legacy magmux has no `default:` case, so an unknown verb —
// and a known verb carrying an `id` — is answered with silence.
func (s *Session) probeCapabilities(ctx context.Context) {
	res, err := s.request(ctx, map[string]any{"type": "capabilities"}, sockProbeTimeout)
	if err != nil {
		s.legacy = true
		return
	}
	s.caps = res
}

func (s *Session) isLegacy() bool { return s.legacy }

// request sends a message with an id and waits for its single reply.
func (s *Session) request(ctx context.Context, msg map[string]any, timeout time.Duration) (map[string]any, error) {
	if s.legacy {
		return nil, errLegacyMagmux
	}
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	msg["id"] = id
	ch := make(chan mcpReply, 1)
	s.pendMu.Lock()
	s.pending[id] = ch
	s.pendMu.Unlock()
	// Always deleted, on every path: a pending entry left behind for a request
	// that timed out is a slow leak plus a reply delivered to nobody.
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, id)
		s.pendMu.Unlock()
	}()

	if err := s.writeLine(msg); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	verb, _ := msg["type"].(string)
	select {
	case r := <-ch:
		if !r.OK {
			return r.Result, &sockRequestError{Code: r.Code, Msg: r.Error}
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, fmt.Errorf("magmux session %s disconnected", s.ID)
	case <-timer.C:
		return nil, fmt.Errorf("magmux did not answer %q within %s", verb, timeout)
	}
}

// fire writes a message with no id, which magmux answers with nothing. Used
// for `send` against a legacy magmux, the one verb that works without replies.
func (s *Session) fire(msg map[string]any) error {
	return s.writeLine(msg)
}

func (s *Session) writeLine(msg map[string]any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	select {
	case <-s.closed:
		return fmt.Errorf("magmux session %s is closed", s.ID)
	default:
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(sockReadTimeout))
	_, err = s.conn.Write(data)
	_ = s.conn.SetWriteDeadline(time.Time{})
	return err
}

func (s *Session) Close() { s.close(true) }

func (s *Session) close(closeConn bool) {
	s.once.Do(func() {
		close(s.closed)
		if closeConn {
			_ = s.conn.Close()
		}
		s.state.markEnded()
	})
	if !closeConn {
		// The reader hit EOF: drop the fd too, but only after `closed` is set
		// so writers fail fast instead of on a dead socket.
		_ = s.conn.Close()
	}
}

// beginTurn claims a pane for one send_and_wait.
func (s *Session) beginTurn(pane int) bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.inFlight[pane] {
		return false
	}
	s.inFlight[pane] = true
	return true
}

func (s *Session) endTurn(pane int) {
	s.turnMu.Lock()
	delete(s.inFlight, pane)
	s.turnMu.Unlock()
}

// ── verbs ───────────────────────────────────────────────────────────────────

// listPanes asks magmux for its enriched pane table, falling back to the state
// the broadcasts have already given us when the verb is unavailable. The
// fallback is what keeps list_panes working against a legacy magmux.
func (s *Session) listPanes(ctx context.Context) ([]paneInfo, error) {
	if s.legacy {
		return s.state.all(), nil
	}
	res, err := s.request(ctx, map[string]any{"type": "list"}, sockReadTimeout)
	if err != nil {
		code := sockErrCode(err)
		if code == "unknown_verb" || code == "unsupported" {
			return s.state.all(), nil
		}
		return nil, err
	}
	if arr, ok := res["panes"].([]any); ok {
		s.state.seedAggregate(arr)
	}
	return s.state.all(), nil
}

// capture renders a screenful of a pane. lines>0 keeps the LAST n rows, where
// the prompt lives; offset>0 reaches back through the pane's scrollback in rows,
// measured from the bottom of the live screen.
//
// `offset` is sent only when it is non-zero, which keeps a capture against a
// magmux predating scrollback byte-identical: an older build ignores unknown
// fields, so a zero offset would work anyway, but a request that carries nothing
// new cannot be blamed for anything either.
func (s *Session) capture(ctx context.Context, pane, lines, offset int) (map[string]any, error) {
	msg := map[string]any{"type": "capture", "pane": pane}
	if lines > 0 {
		msg["lines"] = lines
	}
	if offset > 0 {
		msg["offset"] = offset
	}
	return s.request(ctx, msg, sockReadTimeout)
}

// transcriptTurn is one turn of a session's own on-disk record, as the socket
// carries it. Separate from magmux's Turn on purpose: the MCP server is a
// different process and shares no types with the multiplexer, so this is a
// wire shape and stays one.
type transcriptTurn struct {
	Role      string
	Text      string
	Timestamp string
	Tools     []transcriptTool
}

type transcriptTool struct {
	Name   string
	Input  string
	Result string
}

// transcript asks magmux for the last `turns` turns of a pane's session record.
//
// Errors are passed through untouched, codes and all: read_pane branches on
// no_controller / no_transcript / unsupported to tell the model three quite
// different things, and flattening them into "could not read the transcript"
// would put it back to guessing which one it hit.
func (s *Session) transcript(ctx context.Context, pane, turns int) ([]transcriptTurn, error) {
	msg := map[string]any{"type": "transcript", "pane": pane}
	if turns > 0 {
		// `lines` is what the verb reads the turn count from; see sockTranscript.
		msg["lines"] = turns
	}
	res, err := s.request(ctx, msg, sockReadTimeout)
	if err != nil {
		return nil, err
	}
	raw, _ := res["turns"].([]any)
	out := make([]transcriptTurn, 0, len(raw))
	for _, entry := range raw {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		t := transcriptTurn{}
		t.Role, _ = evStr(e, "role")
		t.Text, _ = evStr(e, "text")
		t.Timestamp, _ = evStr(e, "timestamp")
		tools, _ := e["tools"].([]any)
		for _, rawTool := range tools {
			tm, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			var call transcriptTool
			call.Name, _ = evStr(tm, "name")
			call.Input, _ = evStr(tm, "input")
			call.Result, _ = evStr(tm, "result")
			t.Tools = append(t.Tools, call)
		}
		out = append(out, t)
	}
	return out, nil
}

// sendKeys delivers text and/or named keys to a pane.
func (s *Session) sendKeys(ctx context.Context, pane int, text string, keys []string, enter bool, label string) error {
	msg := map[string]any{
		"type":  "send",
		"pane":  pane,
		"enter": enter,
	}
	if text != "" {
		msg["text"] = text
	}
	if len(keys) > 0 {
		msg["keys"] = keys
	}
	if label != "" {
		msg["label"] = label
	}
	if s.legacy {
		// No reply to wait for; the broadcasts are the only feedback there is.
		return s.fire(msg)
	}
	_, err := s.request(ctx, msg, sockSendTimeout)
	return err
}

func (s *Session) openPane(ctx context.Context, req map[string]any) (map[string]any, error) {
	req["type"] = "open_pane"
	return s.request(ctx, req, sockLifecycleTimeout)
}

func (s *Session) closePane(ctx context.Context, pane int, force bool) (map[string]any, error) {
	msg := map[string]any{"type": "close_pane", "pane": pane}
	if force {
		msg["force"] = true
	}
	return s.request(ctx, msg, sockLifecycleTimeout)
}

// ── the two-phase turn ──────────────────────────────────────────────────────

// turnResult is what the controlled session did in response to one
// instruction. Mirrors magmux.ts's TurnResult.
type turnResult struct {
	State    string
	Response string
	Tool     string
	Duration time.Duration
	Stalled  bool
}

// runInstruction pushes an instruction into a pane and waits for the resulting
// turn. A logic port of pilot/magmux.ts:188-228.
//
// The wait is two-phase on purpose. The session is already sitting in
// awaiting_input when we send — that is *why* we are sending — so waiting for
// awaiting_input alone would return instantly with the previous turn's
// response, and the agent would plan its next step against a stale answer. So
// we first wait for the pane to visibly leave the settled set (the turn
// started), and only then wait for it to settle again.
//
// A turn that never starts is reported as stalled rather than as an empty
// success, because "the instruction was dropped" and "the session had nothing
// to do" need different responses from the driver.
//
// send is injected so this function can be unit-tested against a fake
// sessionState with no socket at all — it is the logic most likely to rot.
func runInstruction(ctx context.Context, st *sessionState, pane int, send func() error,
	startTimeout, turnTimeout time.Duration) (turnResult, error) {

	startedAt := time.Now()
	// Sampled per call, immediately before the send, and never reused across
	// turns: it is the baseline for the escape hatch below, and a baseline from
	// two turns ago would make a repeated answer look like a fresh one. Now
	// that an explicitly empty response is honoured (applyPane), the clear that
	// begins a turn is itself a visible change from this baseline, which is
	// exactly the evidence the escape hatch wants.
	before := st.response(pane)

	if err := send(); err != nil {
		return turnResult{}, err
	}

	started := st.wait(ctx, func() bool { return !settledStates[st.paneState(pane)] }, startTimeout)
	if !started {
		// One honest escape hatch: if the response text changed while we were
		// waiting, the turn did run and we simply never sampled a non-settled
		// state — a turn shorter than the controller's 250ms poll window.
		if st.response(pane) != before {
			return turnResult{
				State:    st.paneState(pane),
				Response: st.response(pane),
				Tool:     st.tool(pane),
				Duration: time.Since(startedAt),
			}, nil
		}
		return turnResult{State: "stalled", Duration: time.Since(startedAt), Stalled: true}, nil
	}

	settled := st.wait(ctx, func() bool { return settledStates[st.paneState(pane)] }, turnTimeout)
	state := st.paneState(pane)
	if !settled {
		state = "stalled"
	}
	return turnResult{
		State:    state,
		Response: st.response(pane),
		Tool:     st.tool(pane),
		Duration: time.Since(startedAt),
		Stalled:  !settled,
	}, nil
}

// describeTurn turns a turnResult into the text the driving model reads. A
// port of pilot/pilot.ts:445-485, including the awaiting-input-with-no-response
// paragraph, which is load-bearing: saying "(no response)" reads as "nothing
// happened", and a driver then burns its budget on sanity checks — observed
// costing half a run.
//
// screen is the last few rendered lines, appended when a turn settles with no
// response text. The pilot could only *explain* the emptiness; with capture we
// can show what actually happened.
func describeTurn(r turnResult, screen string) string {
	secs := fmt.Sprintf("%.0f", r.Duration.Seconds())
	var b strings.Builder
	switch r.State {
	case "awaiting_input":
		if r.Response == "" {
			fmt.Fprintf(&b, "The session finished the turn in %ss and is waiting for the next "+
				"instruction.\n\nIt produced no text summary — normal when a turn is just tool "+
				"calls", secs)
			if r.Tool != "" {
				fmt.Fprintf(&b, " (last tool: %s)", r.Tool)
			}
			b.WriteString(". This does NOT mean the instruction failed, and the session is " +
				"working normally. If you need to know the outcome, make it part of the next " +
				"instruction — ask the session to state the result in its reply, in words.")
			appendScreen(&b, screen)
			return b.String()
		}
		fmt.Fprintf(&b, "The session finished the turn in %ss and is waiting for the next "+
			"instruction.\n\nIt reported:\n%s", secs, r.Response)
		if r.Tool != "" {
			fmt.Fprintf(&b, "\n\nLast tool used: %s", r.Tool)
		}
		return b.String()
	case "awaiting_permission":
		fmt.Fprintf(&b, "After %ss the session is BLOCKED on a permission prompt and cannot "+
			"continue on its own. Last output:\n%s", secs, orNone(r.Response))
		appendScreen(&b, screen)
		b.WriteString("\n\nAnswer it with send_keys (for example keys:[\"1\"] or keys:[\"enter\"]) " +
			"after reading the prompt.")
		return b.String()
	case "error":
		fmt.Fprintf(&b, "The session reported an error after %ss:\n%s", secs, orDetail(r.Response))
		appendScreen(&b, screen)
		return b.String()
	case "gone":
		fmt.Fprintf(&b, "The session process exited after %ss. No further instructions can be "+
			"sent to this pane.", secs)
		appendScreen(&b, screen)
		return b.String()
	default:
		fmt.Fprintf(&b, "The instruction did not produce a turn within %ss — the session never "+
			"started working. It may not have received the instruction. Do not assume the step "+
			"was done.", secs)
		appendScreen(&b, screen)
		b.WriteString("\n\nRead the pane before retrying: the session may be at a prompt, " +
			"mid-render, or waiting on something else.")
		return b.String()
	}
}

func appendScreen(b *strings.Builder, screen string) {
	if strings.TrimSpace(screen) == "" {
		return
	}
	b.WriteString("\n\nWhat the pane shows right now:\n```\n")
	b.WriteString(strings.TrimRight(screen, "\n"))
	b.WriteString("\n```")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orDetail(s string) string {
	if s == "" {
		return "(no detail)"
	}
	return s
}

// ── JSON helpers ────────────────────────────────────────────────────────────
//
// magmux's events are decoded into map[string]any because `pane` is `int or
// "*"` on the wire and the optional fields are omitted rather than zeroed;
// these keep the type assertions in one place.

func evStr(m map[string]any, k string) (string, bool) {
	v, ok := m[k]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func evInt(m map[string]any, k string) (int, bool) {
	v, ok := m[k]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

func evBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}
