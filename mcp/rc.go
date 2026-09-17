package mcp

// MCP's half of remote control: the session binding, dynamic plugin tools, and
// resources.
//
// Three ideas, and each exists because of a failure the plain tool surface had.
//
// 1. THE BINDING IS QUIET UNTIL SOMETHING IS DRIVEN. `magmux mcp` resolves a
//    session eagerly now — it has to, because tools/list and resources/list must
//    answer before any tool has been called — and an eager resolve that
//    announced itself would put a phantom controller in every controlled pane's
//    panel and RESET the counters of the pilot that was actually driving it.
//    So dial+register is silent, every read goes through verbs magmux does not
//    count as driving, and `pilot start` is fired by announceOnce from the first
//    tools/call that resolves to the session. Its sync.Once semantics are load
//    bearing: tools/call runs on its own goroutine, so a concurrent first caller
//    must BLOCK until the announce is on the wire, or its request would reach
//    magmux ahead of the `pilot start` that zeroes the panel's counters — and the
//    turn it opened would not be counted.
//
// 2. DYNAMIC TOOLS ARE A TABLE, NOT A STRING SPLIT. A plugin op is
//    `<plugin>.<op>` to magmux and `<plugin>__<op>` to MCP, and the mapping back
//    is a lookup in a table built from `ops` — never SplitToolName on whatever
//    the client sent. A name that is not in the table is not a tool, which is
//    what stops a crafted tool name reaching an op by a route the registry never
//    advertised.
//
// 3. A RESOURCE IS A THING TO READ, AND `updated` IS ONLY A NUDGE. The spec's
//    notifications/resources/updated carries a uri and nothing else, so every
//    payload question is answered by the following resources/read. That is why
//    a pane subscription is `watch {mode:"notify"}` — the cheap half, one line
//    per change — rather than a frame stream nobody would decode.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/client"
	"github.com/MadAppGang/magmux/protocol"
)

// rpcResourceNotFound is MCP's own code for "no such resource" (spec 2025-06-18,
// resources/read). It is NOT one of the JSON-RPC reserved codes, and it is the
// one error in this server that a client is expected to branch on: a pane that
// has closed is a resource that was real a moment ago, and a client seeing
// -32002 re-lists rather than retrying.
const rpcResourceNotFound = -32002

const (
	// resourceScheme prefixes every magmux resource URI.
	resourceScheme = "magmux://"

	// notifyInterval is the coalescing window. One notification per URI per
	// window is ≤4/s, which is the rate the architecture fixes: a notification
	// costs the client a resources/read, so a pane changing at 30 fps must not
	// buy 30 reads a second.
	notifyInterval = 250 * time.Millisecond

	// eagerResolveBudget bounds the one quiet resolve after
	// notifications/initialized, and eagerWait bounds how long a tools/list or
	// resources/list will WAIT for it. They are different numbers on purpose:
	// the resolve may take its time (a legacy magmux costs a lifecycle probe),
	// and a list that blocked on it would look like a hung server.
	eagerResolveBudget = 8 * time.Second
	eagerWait          = 2 * time.Second

	// opsTimeout bounds one `ops` fetch, and dynamicCallTimeout one plugin op.
	// The latter is generous because a plugin op is whatever the plugin does —
	// the ticket demo opens a pane and waits for a prompt — and magmux clamps
	// anything past its own ceiling anyway.
	opsTimeout         = 5 * time.Second
	dynamicCallTimeout = 2 * time.Minute
)

// ── resource URIs ───────────────────────────────────────────────────────────

// resourceRef is a parsed magmux resource URI.
type resourceRef struct {
	Session string
	Kind    string // "pane" or "plugin"
	Pane    int
	Plugin  string
}

func paneScreenURI(session string, pane int) string {
	return fmt.Sprintf("%s%s/pane/%d/screen", resourceScheme, session, pane)
}

func pluginEventsURI(session, plugin string) string {
	return fmt.Sprintf("%s%s/plugin/%s/events", resourceScheme, session, plugin)
}

// parseResourceURI reverses the two builders above, and accepts nothing else.
//
// Strict by shape rather than by regexp: four segments, a known middle word and
// a known last word. A session id is whatever the human called their magmux
// (`--id a.b` is legal), so it is taken verbatim rather than validated here —
// the lookup that follows is what decides whether it names anything.
func parseResourceURI(uri string) (resourceRef, bool) {
	rest, ok := strings.CutPrefix(uri, resourceScheme)
	if !ok {
		return resourceRef{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] == "" {
		return resourceRef{}, false
	}
	ref := resourceRef{Session: parts[0]}
	switch {
	case parts[1] == "pane" && parts[3] == "screen":
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 0 {
			return resourceRef{}, false
		}
		ref.Kind, ref.Pane = "pane", n
	case parts[1] == "plugin" && parts[3] == "events":
		if parts[2] == "" {
			return resourceRef{}, false
		}
		ref.Kind, ref.Plugin = "plugin", parts[2]
	default:
		return resourceRef{}, false
	}
	return ref, true
}

// ── the binding ─────────────────────────────────────────────────────────────

// binding is everything `magmux mcp` knows about one attached session beyond
// the connection itself: its op table, the tool names that table produces, the
// resources this client has subscribed to, and whether the session has been
// announced to the control panel.
type binding struct {
	srv  *mcpServer
	id   string
	sess *client.Session

	// announce has sync.Once semantics deliberately — see the file comment.
	announce sync.Once

	mu      sync.Mutex
	ops     []protocol.OpSpec
	rev     int
	fetched bool
	// tools is the REVERSIBLE table: MCP tool name -> the op spec it stands
	// for, whose Name is the qualified `<plugin>.<op>` magmux answers to.
	tools   map[string]protocol.OpSpec
	plugins []string
	subs    map[string]bool
	watched map[int]bool
}

func newBinding(s *mcpServer, id string, sess *client.Session) *binding {
	return &binding{
		srv: s, id: id, sess: sess,
		tools:   map[string]protocol.OpSpec{},
		subs:    map[string]bool{},
		watched: map[int]bool{},
	}
}

// announceOnce fires `pilot start` for this session, exactly once, and blocks
// every concurrent first caller until it is written.
//
// Silent for an anonymous client: the panel gains no header from a controller
// with no name, and `pilot start` resets its counters, so it is not free.
func (b *binding) announceOnce() {
	b.announce.Do(func() {
		b.srv.sessMu.Lock()
		name := b.srv.clientName
		b.srv.sessMu.Unlock()
		if name == "" {
			return
		}
		// Fire-and-forget, with no id: it needs no reply, so it also works
		// against a legacy magmux, which has always understood `pilot`.
		if err := b.sess.Fire(map[string]any{
			"type": "pilot", "event": "start", "client": name,
		}); err != nil {
			b.srv.logf("could not announce %q to session %s: %v", name, b.id, err)
		}
	})
}

// ensureOps fetches the op table once. `ops` is not a controller verb, so this
// is as quiet as a read gets.
func (b *binding) ensureOps(ctx context.Context) error {
	b.mu.Lock()
	done := b.fetched
	b.mu.Unlock()
	if done {
		return nil
	}
	return b.refreshOps(ctx)
}

// refreshOps re-fetches unconditionally, which is what `ops_changed` asks for.
//
// It must never run on a session's own reader goroutine: the reply it waits for
// is read by that goroutine, so calling it from the raw event hook would
// deadlock the connection. Every caller reaching it from an event goes through
// a goroutine of its own.
func (b *binding) refreshOps(ctx context.Context) error {
	specs, rev, err := b.sess.Ops(ctx)
	if err != nil {
		return err
	}
	tools := map[string]protocol.OpSpec{}
	plugins := map[string]bool{}
	for _, spec := range specs {
		if spec.Source == "" || spec.Source == protocol.SourceBuiltin {
			continue
		}
		// The op's own name is qualified (`ticket.run_ticket`); the tool name is
		// built from the two halves the registry itself separated, never by
		// re-splitting a string that arrived from outside.
		_, bare, ok := protocol.SplitQualifiedOp(spec.Name)
		if !ok {
			continue
		}
		tools[protocol.ToolName(spec.Source, bare)] = spec
		plugins[spec.Source] = true
	}
	names := make([]string, 0, len(plugins))
	for name := range plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	b.mu.Lock()
	b.ops, b.rev, b.tools, b.plugins, b.fetched = specs, rev, tools, names, true
	b.mu.Unlock()
	return nil
}

func (b *binding) toolSpec(name string) (protocol.OpSpec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	spec, ok := b.tools[name]
	return spec, ok
}

func (b *binding) toolNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.tools))
	for name := range b.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (b *binding) pluginNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.plugins...)
}

func (b *binding) revision() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rev
}

func (b *binding) subscribed(uri string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subs[uri]
}

func (b *binding) addSub(uri string, pane int, isPane bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[uri] = true
	if isPane {
		b.watched[pane] = true
	}
}

func (b *binding) dropSub(uri string, pane int, isPane bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, uri)
	if isPane {
		delete(b.watched, pane)
	}
}

// forgetPane drops the bookkeeping for a pane that magmux has closed. The watch
// is already gone on magmux's side — a closed pane clears every watcher's slot —
// so there is nothing to unwatch, only state that would otherwise outlive it.
func (b *binding) forgetPane(pane int) {
	uri := paneScreenURI(b.id, pane)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, uri)
	delete(b.watched, pane)
}

// ── the binding registry on the server ──────────────────────────────────────

func (s *mcpServer) bindingByID(id string) *binding {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	return s.binds[id]
}

// bindings returns every attached session's binding, ordered by id so that
// resources/list is stable from one call to the next.
func (s *mcpServer) bindings() []*binding {
	s.sessMu.Lock()
	ids := make([]string, 0, len(s.binds))
	for id := range s.binds {
		ids = append(ids, id)
	}
	s.sessMu.Unlock()
	sort.Strings(ids)
	out := make([]*binding, 0, len(ids))
	for _, id := range ids {
		if b := s.bindingByID(id); b != nil {
			out = append(out, b)
		}
	}
	return out
}

// defaultBinding is the session dynamic tools come from: the same default the
// static tools resolve to when a call names no session.
func (s *mcpServer) defaultBinding() *binding {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if s.defID == "" {
		return nil
	}
	return s.binds[s.defID]
}

// ── the eager, quiet resolve ────────────────────────────────────────────────

// startEagerResolve runs the one non-fatal resolve after
// notifications/initialized.
//
// It exists because tools/list and resources/list are asked BEFORE any tool is
// called, and a server with no session has neither dynamic tools nor resources
// to report. Failure is silent by design: no session, several sessions, or an
// unreachable one all mean "nothing dynamic yet", and an attach later emits
// list_changed.
func (s *mcpServer) startEagerResolve() {
	s.eagerOnce.Do(func() {
		s.eagerRunning.Store(true)
		go func() {
			defer close(s.eagerDone)
			ctx, cancel := context.WithTimeout(context.Background(), eagerResolveBudget)
			defer cancel()
			sess, err := s.resolveSessionQuiet(ctx, "")
			if err != nil {
				s.logf("eager resolve found no session: %v", err)
				return
			}
			b := s.bindingByID(sess.ID)
			if b == nil {
				return
			}
			if err := b.ensureOps(ctx); err != nil {
				s.logf("eager ops fetch for %s: %v", sess.ID, err)
			}
		}()
	})
}

// waitEager blocks a list request until the eager resolve has settled, so the
// first tools/list a client makes does not race it into reporting nine tools
// for a session that has eleven. Bounded: a slow resolve costs one late
// list_changed, never a hung list.
func (s *mcpServer) waitEager() {
	if !s.eagerRunning.Load() {
		return
	}
	select {
	case <-s.eagerDone:
	case <-time.After(eagerWait):
	}
}

// ── notifications ───────────────────────────────────────────────────────────

// rpcNotification is a server->client JSON-RPC notification: no id, and
// therefore no response, ever.
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func (s *mcpServer) notify(method string, params any) {
	data, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		s.logf("marshal notification %s: %v", method, err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(data)
	_ = s.out.WriteByte('\n')
	if err := s.out.Flush(); err != nil {
		s.logf("stdout: %v", err)
	}
}

// notifier coalesces notifications and paces them.
//
// It has its own goroutine for one reason: every notification here originates on
// a SESSION's reader goroutine, and writing to stdout from there would let a
// client that has stopped reading wedge the socket connection that feeds it.
type notifier struct {
	srv *mcpServer

	mu      sync.Mutex
	started bool
	uris    map[string]bool
	tools   bool
	res     bool
	wake    chan struct{}
	done    chan struct{}
}

func newNotifier(s *mcpServer) *notifier {
	return &notifier{
		srv:  s,
		uris: map[string]bool{},
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

func (n *notifier) updated(uri string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.uris[uri] = true
	n.kickLocked()
}

func (n *notifier) toolsChanged() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tools = true
	n.kickLocked()
}

func (n *notifier) resourcesChanged() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.res = true
	n.kickLocked()
}

// kickLocked starts the loop on first use and then never blocks: the wake
// channel holds one token, and a second kick before the loop has run is already
// represented by the pending state.
func (n *notifier) kickLocked() {
	if !n.started {
		n.started = true
		go n.loop()
	}
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// loop flushes immediately and then rate-limits: a change is announced as soon
// as it happens, and the window after it absorbs the storm that usually
// follows.
func (n *notifier) loop() {
	for {
		select {
		case <-n.done:
			return
		case <-n.wake:
		}
		n.flush()
		select {
		case <-n.done:
			return
		case <-time.After(notifyInterval):
		}
	}
}

func (n *notifier) flush() {
	n.mu.Lock()
	uris := make([]string, 0, len(n.uris))
	for u := range n.uris {
		uris = append(uris, u)
	}
	n.uris = map[string]bool{}
	tools, res := n.tools, n.res
	n.tools, n.res = false, false
	n.mu.Unlock()

	// The list notices go first: a client that re-lists and then re-reads sees
	// the new set before it is told one of its members moved.
	if tools {
		n.srv.notify("notifications/tools/list_changed", nil)
	}
	if res {
		n.srv.notify("notifications/resources/list_changed", nil)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		n.srv.notify("notifications/resources/updated", map[string]any{"uri": uri})
	}
}

func (n *notifier) stop() {
	n.mu.Lock()
	defer n.mu.Unlock()
	select {
	case <-n.done:
	default:
		close(n.done)
	}
}

// ── the raw event hook ──────────────────────────────────────────────────────

// onSessionEvent sees every line one attached magmux sends.
//
// It runs on that session's reader goroutine, so it does two things and no
// more: it decides which notification the line implies, and it hands that to
// the notifier. Anything that would make a REQUEST — re-fetching `ops` after
// ops_changed — goes on a goroutine of its own, because the reply to that
// request is read by this very goroutine.
func (s *mcpServer) onSessionEvent(id string, line []byte) {
	var ev map[string]any
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	typ, _ := evStr(ev, "type")
	b := s.bindingByID(id)
	if b == nil {
		// The connect-time aggregate is ingested before attach has registered
		// the binding. There is nothing to notify about a session no request
		// has yet been able to name.
		return
	}
	switch typ {
	case protocol.EventChanged:
		if pane, ok := evInt(ev, "pane"); ok {
			if uri := paneScreenURI(id, pane); b.subscribed(uri) {
				s.notif.updated(uri)
			}
		}
	case protocol.EventPlugin:
		if name, _ := evStr(ev, "plugin"); name != "" {
			if uri := pluginEventsURI(id, name); b.subscribed(uri) {
				s.notif.updated(uri)
			}
		}
	case protocol.EventPaneOpened:
		s.notif.resourcesChanged()
	case protocol.EventPaneClosed:
		if pane, ok := evInt(ev, "pane"); ok {
			b.forgetPane(pane)
		}
		s.notif.resourcesChanged()
	case protocol.EventOpsChanged:
		go s.refreshBinding(b)
	case protocol.EventPluginExited:
		// ops_changed always precedes this, so the table is already correct;
		// what changes here is the resource list, which loses an events URI.
		s.notif.resourcesChanged()
	case protocol.EventResults, protocol.EventShutdown:
		// The session is going away: every resource it owned goes with it, and
		// so do its dynamic tools.
		s.notif.resourcesChanged()
		s.notif.toolsChanged()
	}
}

// refreshBinding re-reads the op table and announces what changed.
func (s *mcpServer) refreshBinding(b *binding) {
	ctx, cancel := context.WithTimeout(context.Background(), opsTimeout)
	defer cancel()
	before := b.revision()
	if err := b.refreshOps(ctx); err != nil {
		s.logf("refresh ops for %s: %v", b.id, err)
		return
	}
	if b.revision() == before {
		return
	}
	if def := s.defaultBinding(); def == b {
		s.notif.toolsChanged()
	}
	// A plugin registering or dying changes the set of events URIs either way,
	// default session or not.
	s.notif.resourcesChanged()
}

// ── tools/list ──────────────────────────────────────────────────────────────

// toolsListResult is the nine static tools plus the default session's plugin
// ops. Dynamic tools come from the DEFAULT session because that is where a call
// that names no session_id would land; a tool that appeared for one session and
// ran against another would be a silent mis-target.
func (s *mcpServer) toolsListResult() map[string]any {
	s.waitEager()
	tools := mcpToolSchemas()
	if b := s.defaultBinding(); b != nil {
		ctx, cancel := context.WithTimeout(context.Background(), opsTimeout)
		if err := b.ensureOps(ctx); err != nil {
			s.logf("ops for %s: %v", b.id, err)
		}
		cancel()
		for _, name := range b.toolNames() {
			spec, ok := b.toolSpec(name)
			if !ok {
				continue
			}
			tools = append(tools, dynamicToolSchema(name, spec))
		}
	}
	return map[string]any{"tools": tools}
}

// dynamicToolSchema renders one plugin op as an MCP tool.
//
// The plugin's own schema is used verbatim apart from one addition: `session_id`,
// so a dynamic tool is addressable exactly like a static one. It is added to a
// FRESH decode of the raw schema each time, so nothing this server holds can be
// mutated by rendering it.
func dynamicToolSchema(name string, spec protocol.OpSpec) map[string]any {
	schema := map[string]any{}
	if len(spec.Schema) > 0 {
		if err := json.Unmarshal(spec.Schema, &schema); err != nil {
			schema = map[string]any{}
		}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props["session_id"] = sessionProp()
	schema["properties"] = props

	return map[string]any{
		"name":        name,
		"title":       spec.Name,
		"description": spec.Description,
		"inputSchema": schema,
	}
}

// isDynamicTool reports whether ANY attached session advertises this name. It
// is what keeps an unknown tool a JSON-RPC error — the model cannot fix a name
// that does not exist — while a name that exists somewhere but not on the
// session asked for is a tool RESULT the model can act on.
func (s *mcpServer) isDynamicTool(name string) bool {
	for _, b := range s.bindings() {
		if _, ok := b.toolSpec(name); ok {
			return true
		}
	}
	return false
}

// callDynamicTool runs one plugin op on behalf of a tools/call.
func (s *mcpServer) callDynamicTool(req rpcRequest, name string, raw json.RawMessage) {
	args := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			s.respondError(req.ID, rpcInvalidParams,
				"invalid arguments: "+err.Error(), nil)
			return
		}
	}
	sessionID := ""
	if v, ok := args["session_id"]; ok {
		if err := json.Unmarshal(v, &sessionID); err != nil {
			s.respondError(req.ID, rpcInvalidParams, "session_id must be a string", nil)
			return
		}
		delete(args, "session_id")
	}

	ctx, cancel := ctxWithTimeout(context.Background(), dynamicCallTimeout)
	defer cancel()

	// resolveSession, not the quiet form: this is a tools/call, and it is what
	// announceOnce fires from.
	sess, err := s.resolveSession(ctx, sessionID)
	if err != nil {
		s.respond(req.ID, toolResultError("%v", err))
		return
	}
	b := s.bindingByID(sess.ID)
	if b == nil {
		s.respond(req.ID, toolResultError("session %s is no longer attached", sess.ID))
		return
	}
	if err := b.ensureOps(ctx); err != nil {
		s.logf("ops for %s: %v", b.id, err)
	}
	spec, ok := b.toolSpec(name)
	if !ok {
		s.respond(req.ID, toolResultError(
			"unknown_verb: session %s does not have a plugin op called %q. Call tools/list "+
				"again, or pass session_id for the session whose plugin provides it.",
			sess.ID, name))
		return
	}

	body, err := json.Marshal(args)
	if err != nil {
		s.respondError(req.ID, rpcInvalidParams, "invalid arguments: "+err.Error(), nil)
		return
	}
	started := time.Now()
	res, err := sess.Call(ctx, spec.Name, body, dynamicCallTimeout)
	s.logf("tools/call %s (%s) in %s", name, spec.Name, time.Since(started).Round(time.Millisecond))
	if err != nil {
		code := protocol.CodeOf(err)
		if code == "" {
			code = "error"
		}
		s.respond(req.ID, toolResultError("%s failed (%s): %v", name, code, err))
		return
	}
	s.respond(req.ID, toolText(renderOpResult(res)))
}

// renderOpResult prints an op's result as indented JSON. A plugin's result is
// its own shape and this server knows nothing about it, so it is shown whole
// rather than summarised into a sentence that would drop the field the caller
// needed.
func renderOpResult(res map[string]any) string {
	if len(res) == 0 {
		return "ok (the op returned no result)"
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", res)
	}
	return string(data)
}

// ── resources ───────────────────────────────────────────────────────────────

// resourcesListResult spans every attached session.
//
// Panes come from the client's own live view rather than from a `list` request:
// the connect-time aggregate seeded it and every pane_opened/closed keeps it
// current, so listing costs nothing and — the part that matters — makes no
// request magmux would count as driving.
func (s *mcpServer) resourcesListResult() map[string]any {
	s.waitEager()
	out := []any{}
	for _, b := range s.bindings() {
		ctx, cancel := context.WithTimeout(context.Background(), opsTimeout)
		if err := b.ensureOps(ctx); err != nil {
			s.logf("ops for %s: %v", b.id, err)
		}
		cancel()

		panes := b.sess.State().All()
		sort.Slice(panes, func(i, j int) bool { return panes[i].Index < panes[j].Index })
		for _, p := range panes {
			if p.Closed {
				continue
			}
			out = append(out, map[string]any{
				"uri":         paneScreenURI(b.id, p.Index),
				"name":        fmt.Sprintf("%s pane %d", b.id, p.Index),
				"title":       paneResourceTitle(p),
				"description": paneResourceDescription(b.id, p),
				"mimeType":    "text/plain",
			})
		}
		for _, name := range b.pluginNames() {
			out = append(out, map[string]any{
				"uri":         pluginEventsURI(b.id, name),
				"name":        fmt.Sprintf("%s plugin %s events", b.id, name),
				"title":       name + " events",
				"description": "Recent events emitted by the " + name + " plugin in session " + b.id + ", newest last.",
				"mimeType":    "application/json",
			})
		}
	}
	return map[string]any{"resources": out}
}

func paneResourceTitle(p client.PaneInfo) string {
	switch {
	case p.Label != "":
		return p.Label
	case p.Control:
		return "control panel"
	case p.Cmd != "":
		return mcpFirstWord(p.Cmd)
	}
	return fmt.Sprintf("pane %d", p.Index)
}

func paneResourceDescription(id string, p client.PaneInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The live screen of pane %d in magmux session %s", p.Index, id)
	if p.Cmd != "" {
		fmt.Fprintf(&b, " (%s)", p.Cmd)
	}
	b.WriteString(".")
	if p.State != "" {
		fmt.Fprintf(&b, " State: %s.", p.State)
	}
	return b.String()
}

// resourceTemplatesResult advertises both URI shapes, so a client can construct
// a resource for a pane it learned about some other way.
func (s *mcpServer) resourceTemplatesResult() map[string]any {
	return map[string]any{"resourceTemplates": []any{
		map[string]any{
			"uriTemplate": resourceScheme + "{session}/pane/{pane}/screen",
			"name":        "pane-screen",
			"title":       "magmux pane screen",
			"description": "The live screen of one pane, as text. {session} is a magmux session " +
				"id (see list_sessions) and {pane} a pane index (see list_panes).",
			"mimeType": "text/plain",
		},
		map[string]any{
			"uriTemplate": resourceScheme + "{session}/plugin/{plugin}/events",
			"name":        "plugin-events",
			"title":       "magmux plugin events",
			"description": "Recent events from one controller plugin, as JSON: " +
				"{plugin, epoch, next, events:[{seq, event, pane, data, at}]}. Read past your " +
				"last seq; a new epoch means the connection was replaced and nothing was replayed.",
			"mimeType": "application/json",
		},
	}}
}

// resourceNotFound is the -32002 every unknown or closed resource gets.
func resourceNotFound(uri, why string) *rpcError {
	return &rpcError{
		Code:    rpcResourceNotFound,
		Message: "resource not found: " + uri + " — " + why,
		Data:    map[string]any{"uri": uri},
	}
}

// handleResourceCall runs read/subscribe/unsubscribe off the reader goroutine:
// each of them makes a request to magmux, and a client's pings must still be
// answered while one is in flight.
func (s *mcpServer) handleResourceCall(req rpcRequest) {
	var p struct {
		URI string `json:"uri"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.respondError(req.ID, rpcInvalidParams, "invalid params: "+err.Error(), nil)
			return
		}
	}
	if p.URI == "" {
		s.respondError(req.ID, rpcInvalidParams, "invalid params: missing uri", nil)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), client.ReadTimeout+time.Second)
	defer cancel()

	var (
		res  map[string]any
		rerr *rpcError
	)
	switch req.Method {
	case "resources/read":
		res, rerr = s.readResource(ctx, p.URI)
	case "resources/subscribe":
		res, rerr = s.subscribeResource(ctx, p.URI)
	case "resources/unsubscribe":
		res, rerr = s.unsubscribeResource(ctx, p.URI)
	default:
		rerr = &rpcError{Code: rpcMethodNotFound, Message: "method not found: " + req.Method}
	}
	if rerr != nil {
		s.respondError(req.ID, rerr.Code, rerr.Message, rerr.Data)
		return
	}
	s.respond(req.ID, res)
}

// resolveResource turns a URI into the binding it names, refusing anything that
// does not currently exist. Every -32002 in this server comes from here.
func (s *mcpServer) resolveResource(ctx context.Context, uri string) (*binding, resourceRef, *rpcError) {
	ref, ok := parseResourceURI(uri)
	if !ok {
		return nil, ref, resourceNotFound(uri, "not a magmux resource URI")
	}
	b := s.bindingByID(ref.Session)
	if b == nil {
		return nil, ref, resourceNotFound(uri, "session "+ref.Session+" is not attached")
	}
	switch ref.Kind {
	case "pane":
		p, ok := b.sess.State().Pane(ref.Pane)
		if !ok {
			return nil, ref, resourceNotFound(uri, fmt.Sprintf("session %s has no pane %d", ref.Session, ref.Pane))
		}
		if p.Closed {
			return nil, ref, resourceNotFound(uri, fmt.Sprintf("pane %d has been closed", ref.Pane))
		}
	case "plugin":
		if err := b.ensureOps(ctx); err != nil {
			s.logf("ops for %s: %v", b.id, err)
		}
		known := false
		for _, name := range b.pluginNames() {
			if name == ref.Plugin {
				known = true
				break
			}
		}
		if !known {
			return nil, ref, resourceNotFound(uri,
				"session "+ref.Session+" has no plugin called "+ref.Plugin)
		}
	}
	return b, ref, nil
}

func (s *mcpServer) readResource(ctx context.Context, uri string) (map[string]any, *rpcError) {
	b, ref, rerr := s.resolveResource(ctx, uri)
	if rerr != nil {
		return nil, rerr
	}
	switch ref.Kind {
	case "pane":
		// `call {op:"capture"}`, never the `capture` VERB: the verb is on
		// magmux's isControllerVerb list and would make this reader a
		// controller in the panel, which is the phantom the whole binding is
		// built to avoid.
		args, err := json.Marshal(map[string]any{"pane": ref.Pane})
		if err != nil {
			return nil, &rpcError{Code: rpcInternalError, Message: err.Error()}
		}
		res, err := b.sess.Call(ctx, "capture", args, client.ReadTimeout)
		if err != nil {
			if protocol.CodeOf(err) == protocol.CodeNoSuchPane {
				return nil, resourceNotFound(uri, err.Error())
			}
			return nil, &rpcError{Code: rpcInternalError,
				Message: fmt.Sprintf("could not capture pane %d of session %s: %v", ref.Pane, ref.Session, err)}
		}
		text, _ := res["text"].(string)
		return resourceContents(uri, "text/plain", text), nil

	case "plugin":
		evs, next := b.sess.PluginEvents(ref.Plugin, 0)
		if evs == nil {
			evs = []client.PluginEvent{}
		}
		body, err := json.MarshalIndent(map[string]any{
			"plugin": ref.Plugin,
			"epoch":  b.sess.Epoch(),
			"next":   next,
			"events": evs,
		}, "", "  ")
		if err != nil {
			return nil, &rpcError{Code: rpcInternalError, Message: err.Error()}
		}
		return resourceContents(uri, "application/json", string(body)), nil
	}
	return nil, resourceNotFound(uri, "not a magmux resource URI")
}

func resourceContents(uri, mime, text string) map[string]any {
	return map[string]any{"contents": []any{map[string]any{
		"uri": uri, "mimeType": mime, "text": text,
	}}}
}

// subscribeResource maps MCP's subscription onto magmux's own.
//
// A pane becomes `watch {mode:"notify"}`, whose `changed` line is exactly what
// notifications/resources/updated means: something moved, read it again. A
// plugin's events URI needs no verb — the events are already broadcast to every
// subscriber — so subscribing to one is pure bookkeeping.
func (s *mcpServer) subscribeResource(ctx context.Context, uri string) (map[string]any, *rpcError) {
	b, ref, rerr := s.resolveResource(ctx, uri)
	if rerr != nil {
		return nil, rerr
	}
	if ref.Kind == "pane" {
		if _, err := b.sess.Watch(ctx, ref.Pane, protocol.WatchNotify, 0); err != nil {
			if protocol.CodeOf(err) == protocol.CodeNoSuchPane {
				return nil, resourceNotFound(uri, err.Error())
			}
			if !errors.Is(err, client.ErrLegacyMagmux) {
				return nil, &rpcError{Code: rpcInternalError,
					Message: fmt.Sprintf("could not watch pane %d of session %s: %v", ref.Pane, ref.Session, err)}
			}
			// A legacy magmux has no watch verb. The subscription is still
			// recorded, so the client is not told it failed, and it simply
			// receives no updates from a build that cannot produce them.
			s.logf("session %s cannot watch: %v", ref.Session, err)
		}
	}
	b.addSub(uri, ref.Pane, ref.Kind == "pane")
	return map[string]any{}, nil
}

func (s *mcpServer) unsubscribeResource(ctx context.Context, uri string) (map[string]any, *rpcError) {
	ref, ok := parseResourceURI(uri)
	if !ok {
		return nil, resourceNotFound(uri, "not a magmux resource URI")
	}
	b := s.bindingByID(ref.Session)
	if b == nil {
		// Nothing to undo. Unsubscribing from a session that has gone is the
		// caller's intent already satisfied, not an error.
		return map[string]any{}, nil
	}
	if ref.Kind == "pane" && b.subscribed(uri) {
		if err := b.sess.Unwatch(ctx, ref.Pane); err != nil {
			s.logf("unwatch pane %d of %s: %v", ref.Pane, ref.Session, err)
		}
	}
	b.dropSub(uri, ref.Pane, ref.Kind == "pane")
	return map[string]any{}, nil
}
