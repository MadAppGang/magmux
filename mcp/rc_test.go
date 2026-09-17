package mcp

// Tests for the remote-control half of `magmux mcp`: resources, dynamic plugin
// tools, the quiet session binding, and the notifications that tie them
// together.
//
// They drive the server through handleLine, over a real pipe, because every
// claim here is about the BYTES on the JSON-RPC stream — a notification that
// arrives, one that does not, and the order of the two. Asserting on internal
// state would pass for a server whose notifications never left the process.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── harness ─────────────────────────────────────────────────────────────────

// rcHarness is an mcpServer whose stdout is decoded into a channel, plus the
// assertions that channel makes possible.
//
// Every line is checked for JSON-RPC shape as it goes past, which makes the
// stdout-hygiene rule a property of the whole file rather than of one test:
// any test that provokes a stray byte fails, wherever it is.
type rcHarness struct {
	t    *testing.T
	s    *mcpServer
	msgs chan map[string]any

	mu   sync.Mutex
	seen []map[string]any
	raw  []string
	// buf holds messages a previous await read past. Without it, two
	// responses that arrive out of order — which is exactly what concurrent
	// tools/call produce — leave the second waiter blocked on a message the
	// first one has already eaten.
	buf []map[string]any
}

func newRCHarness(t *testing.T) *rcHarness {
	t.Helper()
	pr, pw := io.Pipe()
	h := &rcHarness{t: t, msgs: make(chan map[string]any, 512)}
	h.s = newMCPServer(pw, io.Discard)
	shortProbes(h.s)

	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			h.mu.Lock()
			h.raw = append(h.raw, line)
			h.mu.Unlock()
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Errorf("stdout carried a line that is not JSON: %q (%v)", line, err)
				continue
			}
			if m["jsonrpc"] != "2.0" {
				t.Errorf("stdout carried a line that is not JSON-RPC 2.0: %q", line)
				continue
			}
			h.msgs <- m
		}
	}()
	t.Cleanup(func() {
		h.s.shutdown()
		_ = pw.Close()
		_ = pr.Close()
	})
	return h
}

func (h *rcHarness) send(v any) {
	h.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		h.t.Fatalf("marshal: %v", err)
	}
	h.s.handleLine(data)
}

// await returns the first message matching pred, from the buffer of messages
// earlier waits read past or from the stream, keeping everything else so a
// later wait — or a failure message — can still see it.
func (h *rcHarness) await(what string, timeout time.Duration, pred func(map[string]any) bool) map[string]any {
	h.t.Helper()
	deadline := time.After(timeout)
	for {
		if m, ok := h.takeBuffered(pred); ok {
			return m
		}
		select {
		case m, ok := <-h.msgs:
			if !ok {
				h.t.Fatalf("stdout closed while waiting for %s", what)
			}
			h.mu.Lock()
			h.seen = append(h.seen, m)
			h.mu.Unlock()
			if pred(m) {
				return m
			}
			h.mu.Lock()
			h.buf = append(h.buf, m)
			h.mu.Unlock()
		case <-deadline:
			h.mu.Lock()
			seen := fmt.Sprintf("%v", h.seen)
			h.mu.Unlock()
			h.t.Fatalf("never saw %s within %v; the stream carried: %s", what, timeout, seen)
			return nil
		}
	}
}

// rawIndex is the position of the first stdout LINE containing needle, which is
// how a test asserts the order two messages were written in rather than the
// order it got round to reading them.
func (h *rcHarness) rawIndex(needle string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, line := range h.raw {
		if strings.Contains(line, needle) {
			return i
		}
	}
	return -1
}

func (h *rcHarness) takeBuffered(pred func(map[string]any) bool) (map[string]any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, m := range h.buf {
		if pred(m) {
			h.buf = append(h.buf[:i:i], h.buf[i+1:]...)
			return m, true
		}
	}
	return nil, false
}

// reply waits for the response to one request id and returns its result.
func (h *rcHarness) reply(id float64, timeout time.Duration) map[string]any {
	h.t.Helper()
	m := h.await(fmt.Sprintf("a response to id %v", id), timeout, func(m map[string]any) bool {
		got, ok := m["id"].(float64)
		return ok && got == id
	})
	if rerr, ok := m["error"].(map[string]any); ok {
		h.t.Fatalf("request %v failed: %v", id, rerr)
	}
	res, _ := m["result"].(map[string]any)
	return res
}

// rpcErr waits for the response to one request id and returns its error.
func (h *rcHarness) rpcErr(id float64, timeout time.Duration) map[string]any {
	h.t.Helper()
	m := h.await(fmt.Sprintf("an error for id %v", id), timeout, func(m map[string]any) bool {
		got, ok := m["id"].(float64)
		return ok && got == id
	})
	rerr, ok := m["error"].(map[string]any)
	if !ok {
		h.t.Fatalf("request %v succeeded, want an error: %v", id, m)
	}
	return rerr
}

func (h *rcHarness) notification(method string, timeout time.Duration) map[string]any {
	h.t.Helper()
	return h.await(method, timeout, func(m map[string]any) bool {
		return m["method"] == method
	})
}

// noNotification asserts nothing of that kind arrives within d — including one
// buffered by an earlier wait. Everything else it reads is buffered rather than
// dropped, so it cannot silently eat a response a later assertion needs.
func (h *rcHarness) noNotification(method string, d time.Duration) {
	h.t.Helper()
	isIt := func(m map[string]any) bool { return m["method"] == method }
	if m, ok := h.takeBuffered(isIt); ok {
		h.t.Fatalf("an unwanted %s had already arrived: %v", method, m)
	}
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-h.msgs:
			if !ok {
				return
			}
			h.mu.Lock()
			h.seen = append(h.seen, m)
			h.buf = append(h.buf, m)
			h.mu.Unlock()
			if isIt(m) {
				h.t.Fatalf("an unwanted %s arrived: %v", method, m)
			}
		case <-deadline:
			return
		}
	}
}

// initialize runs the handshake as a client does, with a name, so that
// announceOnce has something to announce.
func (h *rcHarness) initialize(name string) {
	h.t.Helper()
	h.send(map[string]any{"jsonrpc": "2.0", "id": 900, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"clientInfo":      map[string]any{"name": name, "version": "1.0"},
		}})
	h.reply(900, 5*time.Second)
}

// attach binds the harness to a fake magmux, quietly, exactly as the eager
// resolve would. It returns once the op table has been fetched, so a following
// tools/list is not racing it.
func (h *rcHarness) attach(f *fakeMagmux, id string) {
	h.t.Helper()
	if _, err := h.s.attach(context.Background(), id, f.path, 0); err != nil {
		h.t.Fatalf("attach: %v", err)
	}
	b := h.s.bindingByID(id)
	if b == nil {
		h.t.Fatalf("attach produced no binding for %s", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.ensureOps(ctx); err != nil {
		h.t.Fatalf("ops: %v", err)
	}
}

func toolNamesOf(t *testing.T, res map[string]any) map[string]map[string]any {
	t.Helper()
	raw, _ := res["tools"].([]any)
	out := map[string]map[string]any{}
	for _, rt := range raw {
		m, _ := rt.(map[string]any)
		name, _ := m["name"].(string)
		out[name] = m
	}
	return out
}

func resourceURIsOf(t *testing.T, res map[string]any) map[string]map[string]any {
	t.Helper()
	raw, _ := res["resources"].([]any)
	out := map[string]map[string]any{}
	for _, rr := range raw {
		m, _ := rr.(map[string]any)
		uri, _ := m["uri"].(string)
		out[uri] = m
	}
	return out
}

func contentsTextOf(t *testing.T, res map[string]any) string {
	t.Helper()
	raw, _ := res["contents"].([]any)
	if len(raw) == 0 {
		t.Fatalf("resource read returned no contents: %v", res)
	}
	m, _ := raw[0].(map[string]any)
	text, _ := m["text"].(string)
	return text
}

// ticketOp is the demo plugin's op as `ops` reports it.
func ticketOp() map[string]any {
	return pluginOp("ticket", "run_ticket", "Open a pane and give it a ticket.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{"type": "string"},
			},
			"required": []any{"title"},
		})
}

// ── resources ───────────────────────────────────────────────────────────────

// TestMCPResourcesListReadAndTemplates covers the whole resource surface in one
// pass: what is listed, what a read returns for each kind, the two templates,
// and the two ways a URI can name nothing.
func TestMCPResourcesListReadAndTemplates(t *testing.T) {
	f := startFakeMagmux(t, true)
	f.setOps(3, ticketOp())
	h := newRCHarness(t)
	h.initialize("claude-code")
	h.attach(f, "fake")

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	res := h.reply(1, 5*time.Second)
	got := resourceURIsOf(t, res)
	for _, want := range []string{
		"magmux://fake/pane/0/screen",
		"magmux://fake/pane/1/screen",
		"magmux://fake/pane/2/screen",
		"magmux://fake/plugin/ticket/events",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("resources/list is missing %s (got %v)", want, keysOf(got))
		}
	}
	if mime := got["magmux://fake/pane/0/screen"]["mimeType"]; mime != "text/plain" {
		t.Errorf("a pane screen has mimeType %v, want text/plain", mime)
	}
	if mime := got["magmux://fake/plugin/ticket/events"]["mimeType"]; mime != "application/json" {
		t.Errorf("a plugin events resource has mimeType %v, want application/json", mime)
	}

	// A pane read goes through `call {op:"capture"}`, never the `capture` verb
	// — the verb is one magmux counts as driving, and a client that only reads
	// must not appear in the control panel as a controller.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "resources/read",
		"params": map[string]any{"uri": "magmux://fake/pane/0/screen"}})
	if text := contentsTextOf(t, h.reply(2, 5*time.Second)); !strings.Contains(text, "MARKER-7") {
		t.Errorf("the pane resource did not carry the screen:\n%s", text)
	}
	if f.sawVerb("capture") {
		t.Error("a resource read used the `capture` VERB, which magmux counts as driving")
	}
	if f.sawVerb("pilot") {
		t.Error("reading a resource announced a controller")
	}

	// The plugin events resource, with an event in it.
	f.push(`{"type":"plugin","plugin":"ticket","event":"progress","pane":3,` +
		`"data":{"step":"sent"},"at":"2026-09-15T10:00:00Z"}`)
	waitFor(t, 2*time.Second, "the event to reach the ring", func() bool {
		evs, _ := h.s.bindingByID("fake").sess.PluginEvents("ticket", 0)
		return len(evs) == 1
	})
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/read",
		"params": map[string]any{"uri": "magmux://fake/plugin/ticket/events"}})
	var ring struct {
		Plugin string `json:"plugin"`
		Epoch  uint64 `json:"epoch"`
		Next   uint64 `json:"next"`
		Events []struct {
			Seq   uint64          `json:"seq"`
			Event string          `json:"event"`
			Pane  *int            `json:"pane"`
			Data  json.RawMessage `json:"data"`
		} `json:"events"`
	}
	body := contentsTextOf(t, h.reply(3, 5*time.Second))
	if err := json.Unmarshal([]byte(body), &ring); err != nil {
		t.Fatalf("the events resource is not JSON: %v\n%s", err, body)
	}
	if ring.Plugin != "ticket" || ring.Epoch == 0 || ring.Next != 2 {
		t.Errorf("events resource header = %+v, want plugin ticket, a non-zero epoch and next 2", ring)
	}
	if len(ring.Events) != 1 || ring.Events[0].Event != "progress" || ring.Events[0].Seq != 1 {
		t.Fatalf("events = %+v, want one progress event at seq 1", ring.Events)
	}
	if ring.Events[0].Pane == nil || *ring.Events[0].Pane != 3 {
		t.Errorf("event pane = %v, want 3", ring.Events[0].Pane)
	}
	var data map[string]any
	if err := json.Unmarshal(ring.Events[0].Data, &data); err != nil || data["step"] != "sent" {
		t.Errorf("event data = %s, want the plugin's own payload", ring.Events[0].Data)
	}

	// Templates: both shapes, so a client can build a URI for a pane it learned
	// about from list_panes.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "resources/templates/list"})
	raw, _ := h.reply(4, 5*time.Second)["resourceTemplates"].([]any)
	tmpl := map[string]bool{}
	for _, rt := range raw {
		m, _ := rt.(map[string]any)
		u, _ := m["uriTemplate"].(string)
		tmpl[u] = true
	}
	for _, want := range []string{
		"magmux://{session}/pane/{pane}/screen",
		"magmux://{session}/plugin/{plugin}/events",
	} {
		if !tmpl[want] {
			t.Errorf("resources/templates/list is missing %s (got %v)", want, tmpl)
		}
	}

	// -32002, three ways: a pane that never existed, a session that is not
	// attached, and a URI that is not one of ours.
	for i, uri := range []string{
		"magmux://fake/pane/99/screen",
		"magmux://nosuch/pane/0/screen",
		"magmux://fake/plugin/nosuch/events",
		"file:///etc/passwd",
	} {
		id := float64(10 + i)
		h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "resources/read",
			"params": map[string]any{"uri": uri}})
		rerr := h.rpcErr(id, 5*time.Second)
		if code, _ := rerr["code"].(float64); int(code) != rpcResourceNotFound {
			t.Errorf("reading %s gave code %v, want %d", uri, rerr["code"], rpcResourceNotFound)
		}
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func waitFor(t *testing.T, d time.Duration, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestResourceSubscribeMapsToNotifyWatch is the subscribe half: MCP's
// subscription becomes magmux's cheapest watch, and each `changed` becomes one
// notifications/resources/updated carrying the URI and nothing else.
func TestResourceSubscribeMapsToNotifyWatch(t *testing.T) {
	f := startFakeMagmux(t, true)
	h := newRCHarness(t)
	h.initialize("claude-code")
	h.attach(f, "fake")

	const uri = "magmux://fake/pane/0/screen"
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/subscribe",
		"params": map[string]any{"uri": uri}})
	h.reply(1, 5*time.Second)

	watch := f.waitForVerb(t, "watch", 2*time.Second)
	if mode, _ := evStr(watch, "mode"); mode != "notify" {
		t.Errorf("subscribe watched in mode %q, want notify — frames are a stream nobody asked for", mode)
	}
	if pane, _ := evInt(watch, "pane"); pane != 0 {
		t.Errorf("subscribe watched pane %d, want 0", pane)
	}

	// An unsubscribed pane must stay silent, or every subscriber pays for every
	// pane in the session.
	f.push(`{"type":"changed","pane":2}`)
	h.noNotification("notifications/resources/updated", 300*time.Millisecond)

	f.push(`{"type":"changed","pane":0}`)
	note := h.notification("notifications/resources/updated", 3*time.Second)
	params, _ := note["params"].(map[string]any)
	if params["uri"] != uri {
		t.Errorf("updated carried uri %v, want %s", params["uri"], uri)
	}
	if len(params) != 1 {
		t.Errorf("updated carried %v; the spec says a uri and nothing else", params)
	}

	// Unsubscribing unwatches and stops the notices.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "resources/unsubscribe",
		"params": map[string]any{"uri": uri}})
	h.reply(2, 5*time.Second)
	f.waitForVerb(t, "unwatch", 2*time.Second)
	f.push(`{"type":"changed","pane":0}`)
	h.noNotification("notifications/resources/updated", 400*time.Millisecond)
}

// TestResourceListChangedOnPaneOpenAndClose: the resource list is the pane
// list, so it changes when panes do — and a closed pane's URI stops resolving.
func TestResourceListChangedOnPaneOpenAndClose(t *testing.T) {
	f := startFakeMagmux(t, true)
	h := newRCHarness(t)
	h.initialize("claude-code")
	h.attach(f, "fake")

	f.push(`{"type":"pane_opened","pane":7,"cmd":"sh","label":"new"}`)
	h.notification("notifications/resources/list_changed", 3*time.Second)

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/list"})
	if _, ok := resourceURIsOf(t, h.reply(1, 5*time.Second))["magmux://fake/pane/7/screen"]; !ok {
		t.Error("a pane that was opened is not in resources/list")
	}

	f.push(`{"type":"pane_closed","pane":7}`)
	h.notification("notifications/resources/list_changed", 3*time.Second)

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "resources/list"})
	if _, ok := resourceURIsOf(t, h.reply(2, 5*time.Second))["magmux://fake/pane/7/screen"]; ok {
		t.Error("a pane that was closed is still in resources/list")
	}

	// A closed pane is a resource that USED to exist, which is exactly what
	// -32002 is for: the client re-lists rather than retrying.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/read",
		"params": map[string]any{"uri": "magmux://fake/pane/7/screen"}})
	rerr := h.rpcErr(3, 5*time.Second)
	if code, _ := rerr["code"].(float64); int(code) != rpcResourceNotFound {
		t.Errorf("reading a closed pane gave code %v, want %d", rerr["code"], rpcResourceNotFound)
	}
	if msg, _ := rerr["message"].(string); !strings.Contains(msg, "closed") {
		t.Errorf("the refusal does not say the pane was closed: %q", msg)
	}
}

// ── dynamic tools ───────────────────────────────────────────────────────────

// TestToolsListChangedWhenAPluginRegisters is R6 as an MCP client sees it: a
// plugin starting mid-session adds a tool, and the client is told to re-list.
func TestToolsListChangedWhenAPluginRegisters(t *testing.T) {
	f := startFakeMagmux(t, true)
	h := newRCHarness(t)
	h.initialize("claude-code")
	h.attach(f, "fake")

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if _, ok := toolNamesOf(t, h.reply(1, 5*time.Second))["ticket__run_ticket"]; ok {
		t.Fatal("a plugin that has not registered already has a tool")
	}

	f.setOps(4, ticketOp())
	f.push(`{"type":"ops_changed","rev":4}`)
	h.notification("notifications/tools/list_changed", 5*time.Second)

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	tools := toolNamesOf(t, h.reply(2, 5*time.Second))
	tool, ok := tools["ticket__run_ticket"]
	if !ok {
		t.Fatalf("tools/list has no ticket__run_ticket: %v", keysOf(tools))
	}
	// The nine static tools are untouched by any of this.
	for _, want := range []string{"list_sessions", "attach_session", "request_session",
		"list_panes", "open_pane", "close_pane", "read_pane", "send_keys", "send_and_wait"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("the static tool %s vanished when a plugin registered", want)
		}
	}
	if len(tools) != 10 {
		t.Errorf("tools/list returned %d tools, want the 9 static ones plus one plugin op: %v",
			len(tools), keysOf(tools))
	}

	schema, _ := tool["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["title"]; !ok {
		t.Errorf("the plugin's own schema was lost: %v", schema)
	}
	if _, ok := props["session_id"]; !ok {
		t.Errorf("a dynamic tool is not addressable by session: %v", schema)
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "title" {
		t.Errorf("required = %v, want the plugin's own [title]", schema["required"])
	}

	// The name is resolved through the table `ops` built, so calling it reaches
	// the QUALIFIED op magmux knows — `ticket.run_ticket`, not the tool name.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "ticket__run_ticket",
			"arguments": map[string]any{"title": "t1"}}})
	h.reply(3, 10*time.Second)
	call := f.waitForVerb(t, "call", 3*time.Second)
	if op, _ := evStr(call, "op"); op != "ticket.run_ticket" {
		t.Errorf("the dynamic tool called op %q, want ticket.run_ticket", op)
	}
	args, _ := call["args"].(map[string]any)
	if args["title"] != "t1" {
		t.Errorf("args = %v, want the tool's own arguments", args)
	}
	if _, has := args["session_id"]; has {
		t.Error("session_id was forwarded to the plugin; it is MCP's field, not the op's")
	}

	// A name no session advertises stays a protocol error: the model cannot fix
	// a tool that does not exist and must not be invited to retry.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "ticket__nope", "arguments": map[string]any{}}})
	rerr := h.rpcErr(4, 5*time.Second)
	if code, _ := rerr["code"].(float64); int(code) != rpcMethodNotFound {
		t.Errorf("an unknown dynamic tool gave code %v, want %d", rerr["code"], rpcMethodNotFound)
	}
}

// TestDynamicToolOnASessionThatLacksItIsAToolError is the other half of the
// name rule: the tool exists (another session has it), so the failure is
// something the model can act on rather than a protocol error.
func TestDynamicToolOnASessionThatLacksItIsAToolError(t *testing.T) {
	withPlugin := startFakeMagmux(t, true)
	withPlugin.setOps(4, ticketOp())
	bare := startFakeMagmux(t, true)

	h := newRCHarness(t)
	h.initialize("claude-code")
	h.attach(withPlugin, "plugged")
	h.attach(bare, "bare")

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "ticket__run_ticket",
			"arguments": map[string]any{"title": "t1", "session_id": "bare"}}})
	res := h.reply(1, 10*time.Second)
	if res["isError"] != true {
		t.Fatalf("calling a plugin op on a session without it succeeded: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "unknown_verb") {
		t.Errorf("the refusal does not name the code a client branches on:\n%s", text)
	}
	if bare.sawVerb("call") {
		t.Error("the op was sent to a session that does not have it")
	}
}

// ── session binding ─────────────────────────────────────────────────────────

// TestEagerAttachAnnouncesNothing is the phantom-controller guard.
//
// `magmux mcp` resolves a session before any tool is called, because tools/list
// and resources/list must answer. If that resolve announced itself, every agent
// that merely LOADED this server inside a controlled pane would appear in the
// panel as a controller — and `pilot start` resets the panel's counters, so it
// would also zero the ledger of the pilot that was really driving.
func TestEagerAttachAnnouncesNothing(t *testing.T) {
	f := startFakeMagmux(t, true)
	f.setOps(4, ticketOp())
	t.Setenv("MAGMUX_SOCK", f.path)

	h := newRCHarness(t)
	h.initialize("claude-code")
	h.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := toolNamesOf(t, h.reply(1, 10*time.Second))
	if _, ok := tools["ticket__run_ticket"]; !ok {
		t.Fatalf("the eager resolve produced no dynamic tools: %v", keysOf(tools))
	}

	// The whole point: it read the op table and said nothing about itself.
	if !f.sawVerb("ops") {
		t.Error("the eager resolve never fetched the op table")
	}
	if f.sawVerb("pilot") {
		t.Fatal("listing tools announced a controller to the control panel")
	}
	for _, verb := range []string{"send", "open_pane", "close_pane", "focus"} {
		if f.sawVerb(verb) {
			t.Errorf("the eager resolve sent %q, which magmux counts as driving", verb)
		}
	}

	// And the first tools/call announces exactly once, ahead of its own request.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "list_panes", "arguments": map[string]any{}}})
	h.reply(2, 10*time.Second)
	f.waitForVerb(t, "pilot", 3*time.Second)
	assertAnnouncePrecedes(t, f, "list", 1)
}

// assertAnnouncePrecedes checks that exactly one `pilot start` was sent and
// that it came before the first `want` message — the ordering that makes a
// turn countable, because `pilot start` zeroes the panel's counters.
func assertAnnouncePrecedes(t *testing.T, f *fakeMagmux, want string, wantCount int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	pilots, firstWant, count := -1, -1, 0
	for i, v := range f.verbs {
		switch v {
		case "pilot":
			count++
			if pilots < 0 {
				pilots = i
			}
		case want:
			if firstWant < 0 {
				firstWant = i
			}
		}
	}
	if count != wantCount {
		t.Errorf("magmux saw %d pilot messages, want %d (%v)", count, wantCount, f.verbs)
	}
	if pilots < 0 {
		t.Fatalf("magmux never saw a pilot message: %v", f.verbs)
	}
	if firstWant < 0 {
		t.Fatalf("magmux never saw a %q: %v", want, f.verbs)
	}
	if pilots > firstWant {
		t.Errorf("the announce came AFTER the first %q (%v): the turn it opened would not be counted",
			want, f.verbs)
	}
}

// TestConcurrentFirstToolCallsAnnounceOnce: tools/call runs on its own
// goroutine, so two first calls race. announceOnce has sync.Once semantics —
// the loser BLOCKS until the announce is written — or the second call's request
// reaches magmux ahead of the `pilot start` that zeroes the counters, and its
// turn is not counted.
func TestConcurrentFirstToolCallsAnnounceOnce(t *testing.T) {
	f := startFakeMagmux(t, true)
	t.Setenv("MAGMUX_SOCK", f.path)

	h := newRCHarness(t)
	h.initialize("claude-code")
	h.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	h.s.waitEager()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			h.send(map[string]any{"jsonrpc": "2.0", "id": 100 + id, "method": "tools/call",
				"params": map[string]any{"name": "list_panes", "arguments": map[string]any{}}})
		}(i)
	}
	wg.Wait()
	h.reply(100, 10*time.Second)
	h.reply(101, 10*time.Second)

	waitFor(t, 3*time.Second, "both list requests", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, v := range f.verbs {
			if v == "list" {
				n++
			}
		}
		return n >= 2
	})
	assertAnnouncePrecedes(t, f, "list", 1)
}

// ── plugin events after the result (validation criterion 5) ─────────────────

// TestPluginEventReachesTheClientAfterTheToolResult is the claim R6 rests on: a
// plugin op returns as soon as the work is UNDERWAY, and what it does next
// reaches the MCP client as a notification rather than being lost because the
// request that started it has already been answered.
//
// The fake emits `plugin` strictly after the `call` reply, and the test asserts
// the ordering on the JSON-RPC stream itself — result first, notification
// second — so a server that only delivered events while a call was in flight
// would fail it.
func TestPluginEventReachesTheClientAfterTheToolResult(t *testing.T) {
	f := startFakeMagmux(t, true)
	f.setOps(4, ticketOp())

	// The op answers, and only then does the plugin start reporting.
	f.onVerb = func(f *fakeMagmux, fc *fakeConn, verb string, msg map[string]any) (map[string]any, bool) {
		if verb != "call" {
			return nil, false
		}
		if op, _ := evStr(msg, "op"); op != "ticket.run_ticket" {
			return nil, false
		}
		id := msg["id"]
		out, _ := json.Marshal(map[string]any{"type": "reply", "id": id, "ok": true,
			"result": map[string]any{"ticket": "t1", "pane": 3}})
		fc.send(string(out))
		go func() {
			time.Sleep(30 * time.Millisecond)
			f.push(`{"type":"plugin","plugin":"ticket","event":"progress","pane":3,` +
				`"data":{"step":"sent"},"at":"2026-09-15T10:00:01Z"}`)
		}()
		return nil, true
	}

	h := newRCHarness(t)
	h.initialize("claude-code")
	h.attach(f, "fake")

	const uri = "magmux://fake/plugin/ticket/events"
	h.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/subscribe",
		"params": map[string]any{"uri": uri}})
	h.reply(1, 5*time.Second)

	h.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "ticket__run_ticket",
			"arguments": map[string]any{"title": "t1"}}})
	res := h.reply(2, 10*time.Second)
	if res["isError"] == true {
		t.Fatalf("run_ticket failed: %v", res)
	}

	// AFTER the result. This is validation criterion 5, and it is asserted on
	// the RAW stream rather than on the order the test happened to read things
	// in: a harness that buffered the notification while waiting for the reply
	// would otherwise report success for a server that delivered neither in the
	// right order.
	note := h.notification("notifications/resources/updated", 5*time.Second)
	resultLine := h.rawIndex(`"id":2`)
	noteLine := h.rawIndex("notifications/resources/updated")
	if resultLine < 0 || noteLine < 0 {
		t.Fatalf("stdout did not carry both lines: result at %d, notification at %d",
			resultLine, noteLine)
	}
	if noteLine <= resultLine {
		t.Fatalf("the plugin event reached the client at line %d, before the tool result at "+
			"line %d: this test only proves anything if the event comes after", noteLine, resultLine)
	}
	params, _ := note["params"].(map[string]any)
	if params["uri"] != uri {
		t.Fatalf("updated carried %v, want %s", params["uri"], uri)
	}

	// And the event is readable, with the payload the plugin sent.
	h.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/read",
		"params": map[string]any{"uri": uri}})
	body := contentsTextOf(t, h.reply(3, 5*time.Second))
	if !strings.Contains(body, `"event": "progress"`) || !strings.Contains(body, `"step": "sent"`) {
		t.Fatalf("the event the client was told about is not in the resource:\n%s", body)
	}
}

// TestPluginEventsRingIsBoundedAndSequenced pins the two properties a reader
// depends on: seq only goes up, and the ring does not grow without limit.
func TestPluginEventsRingIsBoundedAndSequenced(t *testing.T) {
	f := startFakeMagmux(t, true)
	h := newRCHarness(t)
	h.attach(f, "fake")
	sess := h.s.bindingByID("fake").sess

	const n = 250
	for i := 0; i < n; i++ {
		f.push(fmt.Sprintf(
			`{"type":"plugin","plugin":"ticket","event":"progress","data":{"i":%d}}`, i))
	}
	waitFor(t, 5*time.Second, "every event to land", func() bool {
		_, next := sess.PluginEvents("ticket", 0)
		return next == n+1
	})

	evs, next := sess.PluginEvents("ticket", 0)
	if len(evs) > 200 {
		t.Errorf("the ring holds %d events; the bound is 200", len(evs))
	}
	if len(evs) == 0 {
		t.Fatal("the ring is empty")
	}
	if evs[len(evs)-1].Seq != n {
		t.Errorf("last seq = %d, want %d", evs[len(evs)-1].Seq, n)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("seq went backwards at %d: %d after %d", i, evs[i].Seq, evs[i-1].Seq)
		}
	}
	// Reading past the last seq returns nothing, and `next` does not move.
	rest, next2 := sess.PluginEvents("ticket", next-1)
	if len(rest) != 0 || next2 != next {
		t.Errorf("reading past the end returned %d events and next %d, want 0 and %d",
			len(rest), next2, next)
	}
}

// TestResourceURIRoundTrip is the parser's own table. A URI is the only thing a
// client sends that this server turns back into a pane index, and "close pane
// 2" arriving as "close pane 0" is the class of bug it exists to prevent.
func TestResourceURIRoundTrip(t *testing.T) {
	if got := paneScreenURI("work", 3); got != "magmux://work/pane/3/screen" {
		t.Errorf("paneScreenURI = %q", got)
	}
	if got := pluginEventsURI("work", "ticket"); got != "magmux://work/plugin/ticket/events" {
		t.Errorf("pluginEventsURI = %q", got)
	}
	good := []struct {
		uri  string
		ref  resourceRef
		note string
	}{
		{"magmux://work/pane/3/screen", resourceRef{Session: "work", Kind: "pane", Pane: 3}, "a named session"},
		{"magmux://4242/pane/0/screen", resourceRef{Session: "4242", Kind: "pane"}, "a pid-named session"},
		{"magmux://a.b/pane/0/screen", resourceRef{Session: "a.b", Kind: "pane"}, "--id a.b is legal"},
		{"magmux://work/plugin/ticket/events", resourceRef{Session: "work", Kind: "plugin", Plugin: "ticket"}, "events"},
	}
	for _, c := range good {
		ref, ok := parseResourceURI(c.uri)
		if !ok || ref != c.ref {
			t.Errorf("parse(%s) = %+v, %v; want %+v (%s)", c.uri, ref, ok, c.ref, c.note)
		}
	}
	bad := []string{
		"", "magmux://", "magmux://work", "magmux://work/pane/3",
		"magmux://work/pane/x/screen", "magmux://work/pane/-1/screen",
		"magmux://work/pane/3/frames", "magmux:///pane/3/screen",
		"magmux://work/plugin//events", "http://work/pane/3/screen",
		"magmux://work/pane/3/screen/extra",
	}
	for _, uri := range bad {
		if ref, ok := parseResourceURI(uri); ok {
			t.Errorf("parse(%q) accepted it as %+v", uri, ref)
		}
	}
}

// TestInitializeAdvertisesResourcesAndDynamicTools: the capabilities are the
// contract. A client that is not told `subscribe` never subscribes, and one
// that is not told `listChanged` never re-lists — so the whole of this file's
// behaviour is invisible without them.
func TestInitializeAdvertisesResourcesAndDynamicTools(t *testing.T) {
	s := newMCPServer(io.Discard, io.Discard)
	caps, _ := s.initializeResult(json.RawMessage(`{}`))["capabilities"].(map[string]any)
	tools, _ := caps["tools"].(map[string]any)
	if tools["listChanged"] != true {
		t.Errorf("tools.listChanged = %v, want true", tools["listChanged"])
	}
	res, _ := caps["resources"].(map[string]any)
	if res["subscribe"] != true || res["listChanged"] != true {
		t.Errorf("resources capability = %v, want subscribe and listChanged", res)
	}
}
