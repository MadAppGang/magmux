package main

// Tests for `magmux mcp`. All of them run without a magmux: pipes, a fake
// socket, and a fake sessionState. The MCP server's failure modes are protocol
// failures — a stray byte on stdout, a response to a notification, a two-phase
// wait that returns the previous turn's answer — and none of them need a real
// terminal to reproduce.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// ── handshake ───────────────────────────────────────────────────────────────

// mcpPipe drives runMCP over real pipes on stdin/stdout, which is the only way
// to prove the thing this test exists for: that runMCP itself writes protocol
// and nothing else to stdout.
type mcpPipe struct {
	t    *testing.T
	in   *os.File // test writes here
	out  *bufio.Reader
	done chan int

	restore func()
}

func startMCP(t *testing.T) *mcpPipe {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW

	done := make(chan int, 1)
	go func() { done <- runMCP(nil) }()

	p := &mcpPipe{t: t, in: inW, out: bufio.NewReader(outR), done: done}
	p.restore = func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		outW.Close()
		outR.Close()
		inR.Close()
	}
	return p
}

func (p *mcpPipe) send(v any) {
	p.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		p.t.Fatalf("marshal: %v", err)
	}
	if _, err := p.in.Write(append(data, '\n')); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

// readByte reads a single raw byte, so the very first thing the server writes
// can be inspected before any JSON decoding.
func (p *mcpPipe) readByte() byte {
	p.t.Helper()
	b, err := p.out.ReadByte()
	if err != nil {
		p.t.Fatalf("read: %v", err)
	}
	return b
}

// readResponse reads one line and decodes it. The first byte, if already
// consumed, is passed back in.
func (p *mcpPipe) readResponse(prefix []byte) map[string]any {
	p.t.Helper()
	line, err := p.out.ReadBytes('\n')
	if err != nil {
		p.t.Fatalf("read response: %v", err)
	}
	full := append(append([]byte(nil), prefix...), line...)
	var m map[string]any
	if err := json.Unmarshal(full, &m); err != nil {
		p.t.Fatalf("decode %q: %v", full, err)
	}
	return m
}

func TestMCPStdioHandshake(t *testing.T) {
	p := startMCP(t)
	defer p.restore()

	// A watchdog: every failure mode here (a missing response, a response to a
	// notification, a server that never exits) presents as a blocked read.
	deadline := time.AfterFunc(20*time.Second, func() {
		panic("TestMCPStdioHandshake deadlocked waiting on the server")
	})
	defer deadline.Stop()

	p.send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"clientInfo":      map[string]any{"name": "test-client", "version": "9.9"},
			"capabilities":    map[string]any{},
		},
	})

	// main.go has ~70 fmt.Println calls reachable from main(), and a single
	// one of them on this stream desynchronises the client's parser forever.
	// The first byte therefore has to be the start of a JSON object.
	if b := p.readByte(); b != '{' {
		t.Fatalf("first byte on stdout is %q, want '{' — something printed to stdout", b)
	}
	resp := p.readResponse([]byte{'{'})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned no result: %v", resp)
	}
	if got := result["protocolVersion"]; got != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want the requested 2025-03-26", got)
	}
	if _, ok := result["instructions"].(string); !ok {
		t.Errorf("initialize result carries no instructions string: %v", result)
	}
	if si, _ := result["serverInfo"].(map[string]any); si["name"] != "magmux" {
		t.Errorf("serverInfo = %v, want name magmux", si)
	}

	// A notification must draw no response at all — not even an error. If it
	// does, the next read below returns it and the id assertion fails.
	p.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	// An unknown notification is equally silent.
	p.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 1}})

	p.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	resp = p.readResponse(nil)
	if id, _ := resp["id"].(float64); id != 2 {
		t.Fatalf("expected the tools/list response next (id 2), got %v — a notification "+
			"drew a response", resp)
	}
	result, _ = resp["result"].(map[string]any)
	rawTools, _ := result["tools"].([]any)
	got := map[string]bool{}
	for _, rt := range rawTools {
		m, _ := rt.(map[string]any)
		name, _ := m["name"].(string)
		got[name] = true
		schema, ok := m["inputSchema"].(map[string]any)
		if !ok {
			t.Errorf("tool %s has no inputSchema", name)
			continue
		}
		if schema["additionalProperties"] != false {
			t.Errorf("tool %s schema allows additional properties", name)
		}
	}
	want := []string{
		"list_sessions", "attach_session", "request_session",
		"list_panes", "open_pane", "close_pane", "read_pane",
		"send_keys", "send_and_wait",
	}
	if len(got) != len(want) {
		t.Errorf("tools/list returned %d tools, want %d: %v", len(got), len(want), got)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("tools/list is missing %s (got %v)", w, got)
		}
	}

	p.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "ping"})
	resp = p.readResponse(nil)
	if id, _ := resp["id"].(float64); id != 3 {
		t.Fatalf("expected the ping response (id 3), got %v", resp)
	}
	res, ok := resp["result"].(map[string]any)
	if !ok || len(res) != 0 {
		t.Errorf("ping result = %v, want {}", resp["result"])
	}

	p.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "no/such/method"})
	resp = p.readResponse(nil)
	rerr, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown method returned no error: %v", resp)
	}
	if code, _ := rerr["code"].(float64); code != rpcMethodNotFound {
		t.Errorf("unknown method code = %v, want %d", rerr["code"], rpcMethodNotFound)
	}

	// An unknown tool is a protocol error too — the model cannot fix a name
	// that does not exist, so it must not be invited to retry.
	p.send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "no_such_tool", "arguments": map[string]any{}}})
	resp = p.readResponse(nil)
	if rerr, ok := resp["error"].(map[string]any); !ok {
		t.Fatalf("unknown tool returned no error: %v", resp)
	} else if code, _ := rerr["code"].(float64); code != rpcMethodNotFound {
		t.Errorf("unknown tool code = %v, want %d", rerr["code"], rpcMethodNotFound)
	}

	// Closing stdin is how a client says goodbye; the server exits cleanly.
	p.in.Close()
	select {
	case code := <-p.done:
		if code != 0 {
			t.Errorf("runMCP exited %d after stdin closed, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runMCP did not exit after stdin closed")
	}
}

func TestMCPInitializeFallsBackForUnknownProtocol(t *testing.T) {
	s := newMCPServer(io.Discard, io.Discard)
	res := s.initializeResult(json.RawMessage(`{"protocolVersion":"1999-01-01"}`))
	if res["protocolVersion"] != mcpDefaultProtocol {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], mcpDefaultProtocol)
	}
	res = s.initializeResult(json.RawMessage(`{"protocolVersion":"2024-11-05"}`))
	if res["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want the echoed 2024-11-05", res["protocolVersion"])
	}
}

// ── the two-phase wait ──────────────────────────────────────────────────────

// TestRunInstructionTwoPhase is the reason phase one exists. Without it,
// "wait for awaiting_input" returns instantly with the PREVIOUS turn's answer,
// because awaiting_input is exactly the state we send in.
func TestRunInstructionTwoPhase(t *testing.T) {
	// (a) a normal turn: settled -> working -> settled.
	t.Run("normal transition is not stalled", func(t *testing.T) {
		st := newSessionState()
		st.seedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input"}})

		send := func() error {
			go func() {
				time.Sleep(10 * time.Millisecond)
				st.applyPane(map[string]any{"pane": 0, "state": "working"})
				time.Sleep(10 * time.Millisecond)
				st.applyPane(map[string]any{"pane": 0, "state": "awaiting_input",
					"response": "42 passed", "tool": "Bash"})
			}()
			return nil
		}

		r, err := runInstruction(context.Background(), st, 0, send, 2*time.Second, 2*time.Second)
		if err != nil {
			t.Fatalf("runInstruction: %v", err)
		}
		if r.Stalled {
			t.Errorf("turn reported stalled: %+v", r)
		}
		if r.State != "awaiting_input" {
			t.Errorf("state = %q, want awaiting_input", r.State)
		}
		if r.Response != "42 passed" {
			t.Errorf("response = %q, want the NEW turn's response", r.Response)
		}
		if r.Tool != "Bash" {
			t.Errorf("tool = %q, want Bash", r.Tool)
		}
	})

	// (b) the instruction was dropped: nothing moved at all. This must be
	// reported as stalled, never as an empty success — "the instruction never
	// arrived" and "there was nothing to do" need different responses.
	t.Run("no transition and no response change is stalled", func(t *testing.T) {
		st := newSessionState()
		st.seedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input",
			"response": "previous answer"}})

		r, err := runInstruction(context.Background(), st, 0,
			func() error { return nil }, 60*time.Millisecond, time.Second)
		if err != nil {
			t.Fatalf("runInstruction: %v", err)
		}
		if !r.Stalled {
			t.Errorf("a dropped instruction was not reported as stalled: %+v", r)
		}
		if r.State != "stalled" {
			t.Errorf("state = %q, want stalled", r.State)
		}
		if r.Response != "" {
			t.Errorf("a stalled turn must not carry the previous turn's response, got %q",
				r.Response)
		}
	})

	// (c) the escape hatch (magmux.ts:202): the turn ran and finished inside a
	// single 250ms controller poll, so we never sampled a non-settled state,
	// but the response text changed — that is a real turn.
	t.Run("response change without a visible transition is a real turn", func(t *testing.T) {
		st := newSessionState()
		st.seedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input",
			"response": "previous answer"}})

		send := func() error {
			go func() {
				time.Sleep(10 * time.Millisecond)
				// Never leaves awaiting_input, but answers.
				st.applyPane(map[string]any{"pane": 0, "state": "awaiting_input",
					"response": "new answer"})
			}()
			return nil
		}

		r, err := runInstruction(context.Background(), st, 0, send, 300*time.Millisecond, time.Second)
		if err != nil {
			t.Fatalf("runInstruction: %v", err)
		}
		if r.Stalled {
			t.Errorf("escape hatch did not fire; turn reported stalled: %+v", r)
		}
		if r.Response != "new answer" {
			t.Errorf("response = %q, want \"new answer\"", r.Response)
		}
	})
}

// TestPerPaneSnapshotClearsTheResponse pins the one place the MCP client
// deliberately stops being a port of pilot/magmux.ts:145.
//
// magmux clears LastResponse at the START of every turn
// (controller_claude.go), and pollControllers always emits the `response` key
// on a per-pane snapshot — so `"response":""` there means "this turn has said
// nothing yet", not "unchanged". The aggregate (buildPaneResults) omits the key
// entirely when empty, so there an absent key really does mean unchanged.
// Treating both the same is what handed send_and_wait the previous turn's
// answer for any turn that was pure tool calls.
func TestPerPaneSnapshotClearsTheResponseButAggregateDoesNot(t *testing.T) {
	st := newSessionState()
	st.seedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input",
		"response": "Added the parser."}})

	// The aggregate omits `response` when it is empty, so an absent key must
	// leave the last answer standing — that is what stops a late attach or a
	// `list` reply wiping everything we know.
	st.seedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input"}})
	if got := st.response(0); got != "Added the parser." {
		t.Errorf("an aggregate that omits response cleared it: %q", got)
	}

	// A per-pane snapshot carries the key on every poll, so an explicit empty
	// value is magmux telling us the turn has produced no text.
	st.applyPane(map[string]any{"pane": 0, "state": "working", "response": "", "tool": "Edit"})
	if got := st.response(0); got != "" {
		t.Errorf("an explicit empty response on a per-pane snapshot was discarded: %q — "+
			"send_and_wait would report the previous turn's answer", got)
	}

	// An omitted key on a per-pane snapshot is still "unchanged": only the keys
	// magmux actually sends may overwrite what we know.
	st.applyPane(map[string]any{"pane": 0, "state": "awaiting_input", "response": "done"})
	st.applyPane(map[string]any{"pane": 0, "state": "awaiting_input"})
	if got := st.response(0); got != "done" {
		t.Errorf("a per-pane snapshot with no response key cleared it: %q", got)
	}
}

// TestRunInstructionDoesNotReportThePreviousTurnsAnswer is the failure the
// two-phase wait exists to prevent, one level up: turn 1 answers in words,
// turn 2 is Edit + Bash and says nothing. Reporting turn 1's text against turn
// 2 makes the driving model plan its next step from the wrong answer.
func TestRunInstructionDoesNotReportThePreviousTurnsAnswer(t *testing.T) {
	st := newSessionState()
	st.seedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input"}})

	// Turn 1: a normal turn that reports in words.
	turn := func(response, tool string) func() error {
		return func() error {
			go func() {
				time.Sleep(10 * time.Millisecond)
				// magmux clears the response at the start of every turn.
				st.applyPane(map[string]any{"pane": 0, "state": "working",
					"response": "", "tool": tool})
				time.Sleep(10 * time.Millisecond)
				st.applyPane(map[string]any{"pane": 0, "state": "awaiting_input",
					"response": response, "tool": tool})
			}()
			return nil
		}
	}

	first, err := runInstruction(context.Background(), st, 0,
		turn("Added the parser.", "Edit"), 2*time.Second, 2*time.Second)
	if err != nil {
		t.Fatalf("runInstruction: %v", err)
	}
	if first.Response != "Added the parser." {
		t.Fatalf("first turn response = %q, want the text it reported", first.Response)
	}

	// Turn 2: tool calls only, no assistant text — a routine Claude Code turn.
	second, err := runInstruction(context.Background(), st, 0,
		turn("", "Bash"), 2*time.Second, 2*time.Second)
	if err != nil {
		t.Fatalf("runInstruction: %v", err)
	}
	if second.Response != "" {
		t.Errorf("a tool-only turn reported %q — that is the PREVIOUS turn's answer", second.Response)
	}
	if second.State != "awaiting_input" || second.Stalled {
		t.Errorf("second turn = %+v, want a settled, non-stalled turn", second)
	}

	// …and the description must take the empty-response branch, which is the
	// only one that explains the emptiness and shows the captured screen.
	text := describeTurn(second, "make: *** [test] Error 1")
	if !strings.Contains(text, "produced no text summary") {
		t.Errorf("describeTurn did not take the empty-response branch:\n%s", text)
	}
	if !strings.Contains(text, "make: *** [test] Error 1") {
		t.Errorf("describeTurn dropped the captured screen:\n%s", text)
	}
}

func TestRunInstructionReportsSendFailure(t *testing.T) {
	st := newSessionState()
	st.seedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input"}})
	want := errors.New("no such pane")
	if _, err := runInstruction(context.Background(), st, 0,
		func() error { return want }, time.Second, time.Second); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the send error", err)
	}
}

func TestDescribeTurnKeepsTheEmptyResponseParagraph(t *testing.T) {
	text := describeTurn(turnResult{State: "awaiting_input", Tool: "Edit",
		Duration: 8 * time.Second}, "make: *** [test] Error 1")
	for _, want := range []string{"does NOT mean the instruction failed", "last tool: Edit",
		"make: *** [test] Error 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("describeTurn output missing %q:\n%s", want, text)
		}
	}
	stalled := describeTurn(turnResult{State: "stalled", Stalled: true}, "")
	if !strings.Contains(stalled, "Do not assume the step was done") {
		t.Errorf("a stalled turn must say so plainly:\n%s", stalled)
	}
}

// ── ingest ──────────────────────────────────────────────────────────────────

// TestIngestSeedsFromAggregateSnapshot covers the rule that stops a
// late-attaching agent waiting forever: magmux pushes per-pane snapshots on
// CHANGE only, so a session already sitting at awaiting_input emits nothing
// further and the connect-time aggregate is the only state we will ever see.
func TestIngestSeedsFromAggregateSnapshot(t *testing.T) {
	s := &Session{state: newSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}

	s.ingest([]byte(`{"type":"snapshot","panes":[
	  {"pane":0,"state":"running","controller":"claude-code","model":"opus"},
	  {"pane":1,"state":"awaiting_input","response":"done"},
	  {"pane":2,"state":"completed","dead":true,"exitCode":0},
	  {"pane":3,"state":"failed","dead":true,"exitCode":2},
	  {"pane":4,"state":"panel","control":true}
	]}`))

	want := map[int]string{0: "working", 1: "awaiting_input", 2: "gone", 3: "gone", 4: "panel"}
	for idx, state := range want {
		if got := s.state.paneState(idx); got != state {
			t.Errorf("pane %d state = %q, want %q (aggregate vocabulary must be translated)",
				idx, got, state)
		}
	}
	if p, _ := s.state.pane(4); !p.Control {
		t.Error("pane 4 is the control panel and must be marked as such")
	}
	if p, _ := s.state.pane(3); p.ExitCode != 2 || !p.Dead {
		t.Errorf("pane 3 = %+v, want dead with exit code 2", p)
	}
	if got := s.state.response(1); got != "done" {
		t.Errorf("pane 1 response = %q, want done", got)
	}

	// A per-pane snapshot (singular `pane`, no `panes`) is the only event that
	// tracks a turn, and its state names pass through untranslated.
	s.ingest([]byte(`{"type":"snapshot","pane":1,"state":"working","tool":"Bash"}`))
	if got := s.state.paneState(1); got != "working" {
		t.Errorf("pane 1 state = %q after a live snapshot, want working", got)
	}
	if got := s.state.tool(1); got != "Bash" {
		t.Errorf("pane 1 tool = %q, want Bash", got)
	}
	if got := s.state.response(1); got != "done" {
		t.Errorf("an omitted response means unchanged, got %q", got)
	}

	s.ingest([]byte(`{"type":"exit","pane":0,"exitCode":3}`))
	if got := s.state.paneState(0); got != "gone" {
		t.Errorf("pane 0 state = %q after exit, want gone", got)
	}

	// A reply must never touch pane state — a controller cannot be allowed to
	// fabricate an observation about a session.
	s.ingest([]byte(`{"type":"reply","id":"1","ok":true,"result":{"pane":1,"state":"nonsense"}}`))
	if got := s.state.paneState(1); got != "working" {
		t.Errorf("a reply changed pane state to %q", got)
	}
	// Unknown event types are ignored rather than fatal.
	s.ingest([]byte(`{"type":"control","dir":"out","pane":1}`))
	s.ingest([]byte(`not json at all`))
	if got := s.state.paneState(1); got != "working" {
		t.Errorf("pane 1 state = %q after junk, want working", got)
	}

	// results/shutdown end the session and wake every waiter, so nothing sits
	// on a fifteen-minute timeout after magmux has gone.
	done := make(chan bool, 1)
	go func() {
		done <- s.state.wait(context.Background(),
			func() bool { return s.state.paneState(1) == "awaiting_input" }, time.Minute)
	}()
	time.Sleep(20 * time.Millisecond)
	s.ingest([]byte(`{"type":"shutdown"}`))
	select {
	case ok := <-done:
		if ok {
			t.Error("wait returned true although the predicate never held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after shutdown — waiters must be woken when magmux goes")
	}
}

func TestReplyRoutingMatchesNumericAndStringIDs(t *testing.T) {
	s := &Session{state: newSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}
	ch := make(chan mcpReply, 1)
	s.pending["7"] = ch

	s.ingest([]byte(`{"type":"reply","id":7,"ok":true,"result":{"pane":2}}`))
	select {
	case r := <-ch:
		if !r.OK {
			t.Errorf("reply not ok: %+v", r)
		}
	default:
		t.Fatal("a numeric id did not route to the pending request registered as \"7\"")
	}

	s.pending["8"] = make(chan mcpReply, 1)
	s.ingest([]byte(`{"type":"reply","id":"8","ok":false,"code":"no_such_pane","error":"pane 4 of 3"}`))
	select {
	case r := <-s.pending["8"]:
		if r.OK || r.Code != "no_such_pane" {
			t.Errorf("reply = %+v, want the failure with its code", r)
		}
	default:
		t.Fatal("a string id did not route")
	}
}

// ── discovery ───────────────────────────────────────────────────────────────

// TestProbeSocketReadsOneLine pins the probe's contract: exactly one line is
// consumed (the connect-time aggregate is guaranteed first), and the
// connection is closed rather than left as a subscriber.
func TestProbeSocketReadsOneLine(t *testing.T) {
	path := tempSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	closed := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		snapshot := `{"type":"snapshot","panes":[{"pane":0,"state":"awaiting_input"},` +
			`{"pane":1,"state":"panel","control":true}]}`
		fmt.Fprintln(conn, snapshot)
		// Then hold the connection open, exactly as magmux does — the probe
		// must not wait for more.
		buf := make([]byte, 1)
		_, _ = conn.Read(buf) // returns when the probe closes its end
		close(closed)
	}()

	panes, err := probeSocket(context.Background(), path, 2*time.Second)
	if err != nil {
		t.Fatalf("probeSocket: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("got %d panes, want 2: %v", len(panes), panes)
	}
	if idx, _ := evInt(panes[0], "pane"); idx != 0 {
		t.Errorf("first pane index = %v, want 0", panes[0]["pane"])
	}
	if state, _ := evStr(panes[1], "state"); state != "panel" {
		t.Errorf("second pane state = %v, want panel", panes[1]["state"])
	}

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("probeSocket did not close the connection")
	}
	wg.Wait()
}

// tempSockPath returns a short-lived socket path. t.TempDir() is not usable
// here: on darwin it nests the test's own name under /var/folders/…, which
// overruns sun_path's 104-byte limit and turns the test into a silent skip.
func tempSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mmx")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestProbeSocketRejectsNonMagmuxSockets(t *testing.T) {
	path := tempSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		fmt.Fprintln(conn, `{"type":"hello"}`)
		conn.Close()
	}()
	if _, err := probeSocket(context.Background(), path, time.Second); err == nil {
		t.Fatal("probeSocket accepted a socket whose first line was not a pane snapshot")
	}
}

func TestDiscoverSessionsIgnoresForeignNames(t *testing.T) {
	// Names are matched by pattern, not by content, so this is a pure unit
	// check on the regexp — /tmp is shared and must not be assumed empty.
	cases := map[string]string{
		"magmux-1234.sock":  "1234",
		"magmux-work.sock":  "work",
		"magmux-a.b.sock":   "a.b",
		"tmux-1000":         "",
		"magmux.sock":       "",
		"magmux-1234.socks": "",
	}
	for name, want := range cases {
		m := sockNamePattern.FindStringSubmatch(name)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", name, got, want)
		}
	}
}

// ── against a fake magmux ───────────────────────────────────────────────────

// fakeLongAnswer is a session response too long for the one-line summary in
// read_pane's header. It is deliberately over 300 characters: that cut was the
// bug, and a fixture under it would prove nothing.
var fakeLongAnswer = strings.TrimSpace(strings.Repeat("the migration touched every table. ", 20))

// fakeMagmux is just enough of magmux's socket to exercise the client: the
// guaranteed connect-time aggregate, replies correlated by id, and the
// per-pane snapshots a turn produces.
type fakeMagmux struct {
	t    *testing.T
	ln   net.Listener
	path string

	mu       sync.Mutex
	verbs    []string
	msgs     []map[string]any
	replies  bool // answer `capabilities`, i.e. not a legacy magmux
	busyCaps int  // swallow this many `capabilities` probes, then behave normally
}

func startFakeMagmux(t *testing.T, replies bool) *fakeMagmux {
	t.Helper()
	path := tempSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeMagmux{t: t, ln: ln, path: path, replies: replies}
	go f.accept()
	t.Cleanup(func() { ln.Close() })
	return f
}

// startBusyFakeMagmux is a CURRENT magmux that is merely wedged while the first
// `probes` capability probes arrive — pollControllers re-scanning
// ~/.claude/projects under treeMu.RLock — and answers everything after them.
func startBusyFakeMagmux(t *testing.T, probes int) *fakeMagmux {
	t.Helper()
	f := startFakeMagmux(t, true)
	f.mu.Lock()
	f.busyCaps = probes
	f.mu.Unlock()
	return f
}

// shortProbes shrinks the capability-probe budgets so a test can watch both
// verdicts being reached without paying real seconds of silence for them.
// Nothing in this package runs in parallel, so the swap is safe.
func shortProbes(t *testing.T) {
	t.Helper()
	probe, confirm := sockProbeTimeout, sockProbeConfirmTimeout
	sockProbeTimeout = 150 * time.Millisecond
	sockProbeConfirmTimeout = 400 * time.Millisecond
	t.Cleanup(func() { sockProbeTimeout, sockProbeConfirmTimeout = probe, confirm })
}

func (f *fakeMagmux) accept() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	// Pane 2 has no controller: a dev server is a pane you can type into but
	// whose turns magmux cannot see, which is what refuseUnturnable is for.
	fmt.Fprintln(conn, `{"type":"snapshot","panes":[`+
		`{"pane":0,"state":"awaiting_input","controller":"claude-code","label":"api","cmd":"claude","pid":424242},`+
		`{"pane":1,"state":"panel","control":true},`+
		`{"pane":2,"state":"running","label":"dev","cmd":"npm run dev","pid":424243}]}`)

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var msg map[string]any
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		verb, _ := evStr(msg, "type")
		f.mu.Lock()
		f.verbs = append(f.verbs, verb)
		f.msgs = append(f.msgs, msg)
		replies := f.replies
		wedged := f.busyCaps > 0 && verb == "capabilities"
		if wedged {
			f.busyCaps--
		}
		f.mu.Unlock()
		if !replies {
			continue // a legacy magmux answers nothing at all
		}
		if wedged {
			continue // busy: this one probe goes unanswered
		}
		id, _ := msg["id"]
		reply := func(result map[string]any) {
			out, _ := json.Marshal(map[string]any{
				"type": "reply", "id": id, "ok": true, "result": result,
			})
			fmt.Fprintln(conn, string(out))
		}
		switch verb {
		case "capabilities":
			reply(map[string]any{"protocol": 1, "scrollback": 0})
		case "transcript":
			// Only pane 0 has an agent in it. Pane 2 is a dev server, and the
			// refusal it draws is magmux's own no_controller — the reply an MCP
			// client has to translate rather than flatten.
			pane, _ := evInt(msg, "pane")
			if pane != 0 {
				out, _ := json.Marshal(map[string]any{"type": "reply", "id": id, "ok": false,
					"code": "no_controller", "error": fmt.Sprintf(
						"pane %d is not running a tool magmux follows", pane)})
				fmt.Fprintln(conn, string(out))
				continue
			}
			reply(map[string]any{
				"pane": 0, "controller": "claude-code", "requested": 4,
				"turns": []any{
					map[string]any{"role": "user", "text": "run the migration",
						"timestamp": "2026-08-15T10:00:00Z"},
					map[string]any{"role": "assistant", "text": fakeLongAnswer,
						"timestamp": "2026-08-15T10:00:30Z",
						"tools": []any{map[string]any{
							"name":   "Bash",
							"input":  `{"command":"make migrate"}`,
							"result": "42 rows migrated",
						}}},
				}})
		case "list":
			reply(map[string]any{"panes": []any{
				map[string]any{"pane": 0, "state": "awaiting_input", "controller": "claude-code",
					"label": "api", "cmd": "claude", "pid": 424242, "response": fakeLongAnswer},
				map[string]any{"pane": 1, "state": "panel", "control": true},
				map[string]any{"pane": 2, "state": "running", "label": "dev",
					"cmd": "npm run dev", "pid": 424243},
			}})
		case "open_pane":
			reply(map[string]any{"pane": 3})
		case "capture":
			reply(map[string]any{"pane": 0, "rows": 24, "cols": 80, "text": "MARKER-7\n$ "})
		case "send":
			reply(map[string]any{"bytes": 12})
			// The turn: leave the settled set, then settle again. Anything
			// that skips the first half is what phase one exists to catch.
			go func() {
				time.Sleep(20 * time.Millisecond)
				fmt.Fprintln(conn, `{"type":"snapshot","pane":0,"state":"working","tool":"Bash"}`)
				time.Sleep(20 * time.Millisecond)
				fmt.Fprintln(conn, `{"type":"snapshot","pane":0,"state":"awaiting_input","response":"all green","tool":"Bash"}`)
			}()
		default:
			reply(map[string]any{})
		}
	}
}

func (f *fakeMagmux) sawVerb(v string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.verbs {
		if got == v {
			return true
		}
	}
	return false
}

// waitForVerb returns the last message of the given type, waiting for one to
// arrive — the fake reads on its own goroutine, so a write we made is not
// necessarily parsed yet.
func (f *fakeMagmux) waitForVerb(t *testing.T, verb string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		var found map[string]any
		for _, m := range f.msgs {
			if got, _ := evStr(m, "type"); got == verb {
				found = m
			}
		}
		f.mu.Unlock()
		if found != nil {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("magmux never received a %q message", verb)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionDrivesAFakeMagmux(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()

	sess, err := s.attach(ctx, "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if sess.isLegacy(ctx) {
		t.Fatal("a magmux that answered capabilities was marked legacy")
	}
	if got := sess.state.paneState(0); got != "awaiting_input" {
		t.Fatalf("pane 0 was not seeded from the connect-time aggregate: %q", got)
	}

	// A label must be resolved here and never handed to magmux.
	p, err := s.resolvePane(ctx, sess, "api")
	if err != nil {
		t.Fatalf("resolvePane by label: %v", err)
	}
	if p.Index != 0 {
		t.Errorf("label api resolved to pane %d, want 0", p.Index)
	}
	if _, err := s.resolvePane(ctx, sess, "nope"); err == nil {
		t.Error("an unknown label must be an error, not a silent pane 0")
	}

	res, rerr := toolSendAndWait(ctx, s, json.RawMessage(
		`{"pane":"api","instruction":"make the tests green","start_timeout_ms":3000,`+
			`"turn_timeout_ms":3000,"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("send_and_wait: %v", rerr)
	}
	if res["isError"] == true {
		t.Fatalf("send_and_wait failed: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "all green") {
		t.Errorf("turn text does not carry the session's response:\n%s", text)
	}
	if !f.sawVerb("send") {
		t.Error("magmux never saw a send")
	}

	res, rerr = toolReadPane(ctx, s, json.RawMessage(`{"pane":0,"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("read_pane: %v", rerr)
	}
	text = res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "MARKER-7") {
		t.Errorf("read_pane did not return the rendered screen:\n%s", text)
	}

	// The control panel has no process to type into.
	res, _ = toolSendKeys(ctx, s, json.RawMessage(`{"pane":1,"text":"hi","session_id":"fake"}`))
	if res["isError"] != true {
		t.Errorf("send_keys to the control panel was allowed: %v", res)
	}
	sess.Close()
}

// TestSendAndWaitRefusesAPaneWithNoController covers the pane that can be typed
// into but has no turns to observe. Without the refusal phase one succeeds
// vacuously ("running" -> "working" is not settled) and phase two then waits for
// a settled state nothing will ever produce — a tools/call parked for fifteen
// minutes, since send_and_wait's own timeout is 0 by design.
// ── read_pane: the transcript and the screen ────────────────────────────────

func readPaneText(t *testing.T, res map[string]any) string {
	t.Helper()
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tool result carries no content: %v", res)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// TestReadPaneServesBothSourcesAndKeepsThemApart is the point of the whole
// change. An agent must be able to get what the session ACTUALLY said — full
// text, tool inputs, tool results, from the tool's own record on disk — and
// must never confuse it with what happens to be painted on the screen.
func TestReadPaneServesBothSourcesAndKeepsThemApart(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()
	sess, err := s.attach(ctx, "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()

	res, rerr := toolReadPane(ctx, s, json.RawMessage(
		`{"pane":0,"transcript":4,"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("read_pane: %v", rerr)
	}
	if res["isError"] == true {
		t.Fatalf("read_pane failed: %v", res)
	}
	text := readPaneText(t, res)

	// The long response survives whole. Truncating it at 300 characters is the
	// bug this exists to fix: the answer to "did it finish" routinely lives
	// past character 300.
	if !strings.Contains(text, fakeLongAnswer) {
		t.Errorf("the session's full response did not come through (%d chars of it):\n%s",
			len(fakeLongAnswer), text)
	}
	// Tool calls come back with what they were asked to do and what they found,
	// not just a name.
	for _, want := range []string{"Bash", "make migrate", "42 rows migrated"} {
		if !strings.Contains(text, want) {
			t.Errorf("the transcript is missing %q — a tool name alone is what we already had:\n%s",
				want, text)
		}
	}
	// The two sources are labelled, and in that order.
	ti, si := strings.Index(text, "TRANSCRIPT"), strings.Index(text, "SCREEN")
	if ti < 0 || si < 0 {
		t.Fatalf("the two sources are not labelled; an agent cannot tell which is which:\n%s", text)
	}
	if ti > si {
		t.Errorf("the screen was rendered before the authoritative transcript:\n%s", text)
	}
	if !strings.Contains(text, "MARKER-7") {
		t.Errorf("the screen section lost the rendered screen:\n%s", text)
	}
	if !strings.Contains(text[si:], "transcript above is what the session actually said") {
		t.Errorf("nothing tells the agent which source wins when they disagree:\n%s", text[si:])
	}
	// The turn count reaches magmux.
	if msg := f.waitForVerb(t, "transcript", 2*time.Second); func() bool {
		n, _ := evInt(msg, "lines")
		return n != 4
	}() {
		t.Errorf("the transcript request did not carry the 4 turns asked for")
	}
}

// TestReadPaneWithoutTranscriptPointsAtIt keeps the one-line summary honest.
// It is still truncated — it is a header — but it now says so and says where
// the rest is, instead of silently ending mid-sentence.
func TestReadPaneWithoutTranscriptPointsAtIt(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()
	sess, err := s.attach(ctx, "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()

	res, rerr := toolReadPane(ctx, s, json.RawMessage(`{"pane":0,"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("read_pane: %v", rerr)
	}
	text := readPaneText(t, res)
	if strings.Contains(text, "TRANSCRIPT") {
		t.Errorf("a caller who did not ask for the transcript was charged for one:\n%s", text)
	}
	if !strings.Contains(text, "transcript:1") {
		t.Errorf("the truncated summary does not say where the full text is:\n%s", text)
	}
	if f.sawVerb("transcript") {
		t.Error("read_pane asked magmux for a transcript nobody requested")
	}
}

// TestReadPaneReportsAMissingTranscriptWithoutLosingTheScreen covers the two
// halves of the failure contract at once: the caller is told plainly that the
// LOOKUP failed rather than being handed an empty section that reads as "the
// session has said nothing", and the screen — which is unaffected and is the
// recovery being recommended — still comes back in the same call.
func TestReadPaneReportsAMissingTranscriptWithoutLosingTheScreen(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()
	sess, err := s.attach(ctx, "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()

	res, rerr := toolReadPane(ctx, s, json.RawMessage(
		`{"pane":2,"transcript":3,"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("read_pane: %v", rerr)
	}
	if res["isError"] != true {
		t.Errorf("a transcript that could not be produced was reported as a plain success: %v", res)
	}
	text := readPaneText(t, res)
	if !strings.Contains(text, "not running an agent magmux follows") {
		t.Errorf("the failure does not say why there is no transcript:\n%s", text)
	}
	if !strings.Contains(text, "MARKER-7") {
		t.Errorf("the screen was dropped along with the failed transcript:\n%s", text)
	}
}

// TestRenderTranscriptCapsThePayload pins the presentation budget: a driver
// that asks for 50 turns must not be able to blow its own context with one
// call. The NEWEST turns survive — they are the ones it acted on — and the
// reply says how many went and why.
func TestRenderTranscriptCapsThePayload(t *testing.T) {
	big := strings.Repeat("y", 4000)
	turns := make([]transcriptTurn, 12)
	for i := range turns {
		turns[i] = transcriptTurn{Role: "assistant", Text: fmt.Sprintf("turn%02d ", i) + big}
	}
	out := renderTranscript(turns, len(turns))
	if len(out) > transcriptSectionCap*2 {
		t.Fatalf("rendered %d characters against a %d cap", len(out), transcriptSectionCap)
	}
	if !strings.Contains(out, "turn11") {
		t.Error("the newest turn was dropped; it is the one the caller acted on")
	}
	if strings.Contains(out, "turn00") {
		t.Error("the oldest turn survived the cap")
	}
	if !strings.Contains(out, "dropped") {
		t.Errorf("the reply does not say that turns were dropped:\n%s", out[:200])
	}

	// A single oversized turn is clipped rather than dropped: returning nothing
	// is indistinguishable from a session that has said nothing.
	one := renderTranscript([]transcriptTurn{{Role: "assistant",
		Text: strings.Repeat("z", transcriptTurnTextCap*2)}}, 1)
	if !strings.Contains(one, "clipped") {
		t.Error("an oversized turn was not clipped and marked")
	}

	// Tool payloads are clipped with their size stated, so "the tool returned
	// this" is distinguishable from "the tool returned this and more".
	tooly := renderTranscript([]transcriptTurn{{Role: "assistant", Tools: []transcriptTool{
		{Name: "Read", Input: `{"file_path":"/x"}`, Result: strings.Repeat("q", 5000)}}}}, 1)
	if !strings.Contains(tooly, "clipped") || !strings.Contains(tooly, "Read") {
		t.Errorf("a large tool result was not clipped and marked:\n%s", tooly)
	}
}

func TestSendAndWaitRefusesAPaneWithNoController(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()
	sess, err := s.attach(ctx, "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()

	started := time.Now()
	res, rerr := toolSendAndWait(ctx, s, json.RawMessage(
		`{"pane":"dev","instruction":"restart","start_timeout_ms":300,`+
			`"turn_timeout_ms":300,"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("send_and_wait: %v", rerr)
	}
	if res["isError"] != true {
		t.Fatalf("send_and_wait on a controller-less pane was accepted: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"send_keys", "read_pane"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal does not point at %s:\n%s", want, text)
		}
	}
	if f.sawVerb("send") {
		t.Error("the instruction was typed into a pane whose turn cannot be observed")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the refusal took %s — it must not wait for a turn at all", elapsed)
	}
	// The same pane is still fine for send_keys, which is the point of keeping
	// this check out of refuseUndrivable.
	res, rerr = toolSendKeys(ctx, s, json.RawMessage(
		`{"pane":"dev","keys":["ctrl-c"],"session_id":"fake"}`))
	if rerr != nil {
		t.Fatalf("send_keys: %v", rerr)
	}
	if res["isError"] == true {
		t.Errorf("send_keys to a controller-less pane was refused: %v", res)
	}
}

func TestRefuseUnturnableOnlyRejectsControllerLessPanes(t *testing.T) {
	if why := refuseUnturnable(paneInfo{Index: 2, State: "working", Cmd: "npm run dev"}); why == "" {
		t.Error("a pane with no controller has no turn to wait for and must be refused")
	}
	if why := refuseUnturnable(paneInfo{Index: 0, State: "awaiting_input",
		Controller: "claude-code"}); why != "" {
		t.Errorf("a pane with a controller must be drivable, got %q", why)
	}
}

// TestOpenPaneOmitsTargetWhenNotGiven keeps the schema's promise and the wire
// behaviour together. The schema says the default target is the largest pane;
// the way that is delivered is by sending no target at all, so magmux applies
// its own default. Sending one here — or magmux defaulting to the focused pane —
// halves whatever the human is typing in.
func TestOpenPaneOmitsTargetWhenNotGiven(t *testing.T) {
	tool, ok := mcpToolByName("open_pane")
	if !ok {
		t.Fatal("open_pane is not registered")
	}
	props := tool.Schema["properties"].(map[string]any)
	desc, _ := props["target"].(map[string]any)["description"].(string)
	if !strings.Contains(desc, "largest") {
		t.Errorf("the target schema no longer documents the largest-pane default: %q", desc)
	}

	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()
	sess, err := s.attach(ctx, "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()

	if _, rerr := toolOpenPane(ctx, s, json.RawMessage(
		`{"cmd":"npm test","session_id":"fake"}`)); rerr != nil {
		t.Fatalf("open_pane: %v", rerr)
	}
	msg := f.waitForVerb(t, "open_pane", 2*time.Second)
	if _, has := msg["target"]; has {
		t.Errorf("open_pane sent target=%v although the caller named none — the documented "+
			"default is magmux's own", msg["target"])
	}

	// A target that IS given still goes through the resolver, so magmux only
	// ever sees an index.
	if _, rerr := toolOpenPane(ctx, s, json.RawMessage(
		`{"cmd":"npm test","target":"api","session_id":"fake"}`)); rerr != nil {
		t.Fatalf("open_pane with a target: %v", rerr)
	}
	msg = f.waitForVerb(t, "open_pane", 2*time.Second)
	if got, _ := evInt(msg, "target"); got != 0 {
		t.Errorf("target = %v, want the resolved index 0 (never a label)", msg["target"])
	}
}

// TestAttachAnnouncesTheClientToTheControlPanel covers the identity that was
// dead plumbing: clientName was captured at initialize and never used, so the
// panel could not name the controller and picked its layout by accident.
func TestAttachAnnouncesTheClientToTheControlPanel(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	_ = s.initializeResult(json.RawMessage(
		`{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code","version":"2.1"}}`))

	sess, err := s.attach(context.Background(), "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()

	msg := f.waitForVerb(t, "pilot", 2*time.Second)
	if ev, _ := evStr(msg, "event"); ev != "start" {
		t.Errorf("pilot event = %q, want start", ev)
	}
	if client, _ := evStr(msg, "client"); client != "claude-code/2.1" {
		t.Errorf("client = %q, want claude-code/2.1", client)
	}
	if _, has := msg["id"]; has {
		t.Error("the announcement carried an id — it needs no reply and must work against " +
			"a legacy magmux too")
	}
}

// An anonymous client sends nothing: the panel gains no header from it, and
// `pilot start` resets the panel's counters, so it is not free.
func TestAttachStaysSilentWithoutAClientName(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	sess, err := s.attach(context.Background(), "fake", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer sess.Close()
	// capabilities is the probe, and proves the fake has read our writes.
	f.waitForVerb(t, "capabilities", 2*time.Second)
	if f.sawVerb("pilot") {
		t.Error("an unnamed client announced itself anyway")
	}
}

func TestLegacyMagmuxRefusesEverythingButSending(t *testing.T) {
	shortProbes(t)
	f := startFakeMagmux(t, false)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()

	sess, err := s.attach(ctx, "old", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Two silences — the dial probe and a confirming one — and only then.
	if !sess.isLegacy(ctx) {
		t.Fatal("a magmux that answered nothing must be treated as legacy")
	}

	// list_panes still works, from the aggregate snapshot alone.
	res, rerr := toolListPanes(ctx, s, json.RawMessage(`{"session_id":"old"}`))
	if rerr != nil {
		t.Fatalf("list_panes: %v", rerr)
	}
	if res["isError"] == true {
		t.Errorf("list_panes must fall back to the seeded state: %v", res)
	}

	// send still works: it needs nothing but a fire-and-forget write.
	res, rerr = toolSendKeys(ctx, s, json.RawMessage(`{"pane":0,"text":"hello","session_id":"old"}`))
	if rerr != nil {
		t.Fatalf("send_keys: %v", rerr)
	}
	if res["isError"] == true {
		t.Errorf("send_keys must work against a legacy magmux: %v", res)
	}

	// open_pane cannot, and must say why in terms the human can act on.
	res, rerr = toolOpenPane(ctx, s, json.RawMessage(`{"cmd":"claude","session_id":"old"}`))
	if rerr != nil {
		t.Fatalf("open_pane: %v", rerr)
	}
	if res["isError"] != true {
		t.Fatalf("open_pane against a legacy magmux must fail: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "restart magmux") {
		t.Errorf("the refusal does not tell the human what to do:\n%s", text)
	}
	sess.Close()
}

// TestBusyMagmuxIsNotWrittenOffAsLegacy is the counterweight to the test above.
// Silence has two causes and only one of them is old software: a current magmux
// wedged in a controller poll answers nothing either, and a single missed probe
// used to refuse open_pane, close_pane and every read for the whole life of the
// server — against a magmux that was perfectly current.
func TestBusyMagmuxIsNotWrittenOffAsLegacy(t *testing.T) {
	shortProbes(t)
	f := startBusyFakeMagmux(t, 1)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()

	sess, err := s.attach(ctx, "busy", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if sess.capsNote() != "unproven" {
		t.Errorf("one missed probe settled the verdict at %q; the short dial budget "+
			"decides nothing", sess.capsNote())
	}

	res, rerr := toolOpenPane(ctx, s, json.RawMessage(`{"cmd":"claude","session_id":"busy"}`))
	if rerr != nil {
		t.Fatalf("open_pane: %v", rerr)
	}
	if res["isError"] == true {
		t.Fatalf("a busy magmux was written off as legacy:\n%s", readPaneText(t, res))
	}
	if sess.isLegacy(ctx) {
		t.Error("the session answered a probe and is still marked legacy")
	}
	sess.Close()
}

// TestLegacyVerdictExpiresAndIsRechecked pins the other half: the verdict is a
// guess made from an absence, so it must not outlive the absence. A magmux
// wedged past BOTH probes is refused — and then re-probed, not condemned.
func TestLegacyVerdictExpiresAndIsRechecked(t *testing.T) {
	shortProbes(t)
	recheck := legacyRecheckAfter
	legacyRecheckAfter = 50 * time.Millisecond
	t.Cleanup(func() { legacyRecheckAfter = recheck })

	f := startBusyFakeMagmux(t, 2)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()

	sess, err := s.attach(ctx, "wedged", f.path, 0)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !sess.isLegacy(ctx) {
		t.Fatal("two silences in a row must produce the legacy verdict, or a genuinely " +
			"old magmux is never detected")
	}
	time.Sleep(2 * legacyRecheckAfter)
	if sess.isLegacy(ctx) {
		t.Error("the legacy verdict was never re-tested: a magmux that was busy for a " +
			"minute stays refused for the life of the server")
	}
	sess.Close()
}

// TestALateReplyProvesTheReplyProtocol covers the cheapest recovery there is:
// a reply that arrived just after its waiter gave up is still proof that the
// plumbing exists, and proof outranks any amount of silence.
func TestALateReplyProvesTheReplyProtocol(t *testing.T) {
	sess := &Session{ID: "late", state: newSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}
	sess.capState.Store(capsSilent)
	sess.probedAt = time.Now() // a fresh verdict: isLegacy answers without probing
	ctx := context.Background()
	if !sess.isLegacy(ctx) {
		t.Fatal("setup: the session should start out on the silent verdict")
	}

	// Nobody is waiting on id 99 any more — the request timed out — and it must
	// still count.
	sess.ingest([]byte(`{"type":"reply","id":"99","ok":false,"code":"no_such_pane","error":"nope"}`))
	if sess.isLegacy(ctx) {
		t.Error("magmux replied and the session is still refused as legacy")
	}
}

func TestSendAndWaitRefusesTwoConcurrentTurnsOnOnePane(t *testing.T) {
	sess := &Session{ID: "x", state: newSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}
	if !sess.beginTurn(2) {
		t.Fatal("first turn was refused")
	}
	if sess.beginTurn(2) {
		t.Error("two concurrent turns on one pane were allowed — the second would report " +
			"the first one's answer")
	}
	if !sess.beginTurn(3) {
		t.Error("a turn on another pane was refused")
	}
	sess.endTurn(2)
	if !sess.beginTurn(2) {
		t.Error("the pane was not released")
	}
}

// ── self-pane guard ─────────────────────────────────────────────────────────

func TestAncestorPIDsIncludesOurselves(t *testing.T) {
	s := newMCPServer(io.Discard, io.Discard)
	anc := s.ancestorPIDs()
	if !anc[os.Getpid()] {
		t.Fatalf("ancestorPIDs does not contain our own pid %d: %v", os.Getpid(), anc)
	}
	if !anc[os.Getppid()] {
		t.Errorf("ancestorPIDs does not contain our parent %d: %v", os.Getppid(), anc)
	}
}

// TestAncestryRetriesAPartialWalk is the memoisation bug: a walk that broke
// after one step — one unreadable /proc/<pid>/stat, one transient sysctl
// failure — used to be cached as the final answer for the life of the process.
// The guard then knew only our own pid, our own PANE was not recognised as
// ours, and a send_and_wait at it deadlocked on a turn the caller was inside.
func TestAncestryRetriesAPartialWalk(t *testing.T) {
	s := newMCPServer(io.Discard, io.Discard)
	real := ppidLookup
	t.Cleanup(func() { ppidLookup = real })

	ppidLookup = func(int) (int, error) { return 0, errors.New("permission denied") }
	anc, complete := s.ancestry()
	if complete {
		t.Error("a walk that could not read the first link reported itself complete")
	}
	if !anc[os.Getpid()] {
		t.Errorf("even the partial walk must know our own pid: %v", anc)
	}

	// The condition clears. The next call must actually walk again.
	ppidLookup = real
	anc, complete = s.ancestry()
	if !complete {
		t.Fatalf("a readable ancestry did not complete: %v", anc)
	}
	if !anc[os.Getppid()] {
		t.Fatalf("a walk that broke after one step was cached forever: %v", anc)
	}

	// And now it IS cached: a complete answer is the only kind worth keeping.
	ppidLookup = func(int) (int, error) { return 0, errors.New("gone again") }
	if again, ok := s.ancestry(); !ok || !again[os.Getppid()] {
		t.Errorf("a complete walk was not memoised: complete=%v %v", ok, again)
	}
}

// TestUnreadableAncestrySaysSoInThePaneListing is the "degrade loudly" half.
// With no ancestry, no pane carries the YOUR OWN PANE mark — which downstream
// reads as "none of these is yours". Presenting an unchecked list as a checked
// one is what turns a missing warning into a deadlock.
func TestUnreadableAncestrySaysSoInThePaneListing(t *testing.T) {
	f := startFakeMagmux(t, true)
	s := newMCPServer(io.Discard, io.Discard)
	ctx := context.Background()
	if _, err := s.attach(ctx, "warn", f.path, 0); err != nil {
		t.Fatalf("attach: %v", err)
	}

	real := ppidLookup
	t.Cleanup(func() { ppidLookup = real })
	ppidLookup = func(int) (int, error) { return 0, errors.New("permission denied") }

	res, rerr := toolListPanes(ctx, s, json.RawMessage(`{"session_id":"warn"}`))
	if rerr != nil {
		t.Fatalf("list_panes: %v", rerr)
	}
	text := readPaneText(t, res)
	if !strings.Contains(text, "ancestry could not be read") {
		t.Errorf("an unresolvable ancestry silently permitted a self-target:\n%s", text)
	}

	ppidLookup = real
	res, rerr = toolListPanes(ctx, s, json.RawMessage(`{"session_id":"warn"}`))
	if rerr != nil {
		t.Fatalf("list_panes: %v", rerr)
	}
	if text := readPaneText(t, res); strings.Contains(text, "ancestry could not be read") {
		t.Errorf("a healthy ancestry still warned:\n%s", text)
	}
}

func TestRefuseUndrivableExplainsTheSelfDeadlock(t *testing.T) {
	if why := refuseUndrivable(paneInfo{Index: 1, Self: true}); !strings.Contains(why, "deadlock") {
		t.Errorf("a self pane must be refused with an explanation, got %q", why)
	}
	if why := refuseUndrivable(paneInfo{Index: 2, Control: true}); why == "" {
		t.Error("the control panel has no process and must be refused")
	}
	if why := refuseUndrivable(paneInfo{Index: 3, Dead: true, State: "gone"}); why == "" {
		t.Error("a dead pane must be refused")
	}
	if why := refuseUndrivable(paneInfo{Index: 0, State: "awaiting_input"}); why != "" {
		t.Errorf("an idle session pane must be drivable, got %q", why)
	}
}

// ── truncation ──────────────────────────────────────────────────────────────

// TestMCPTruncateKeepsRunesWhole: the cut lands wherever the text puts it, and
// a byte cut through an em dash or an emoji becomes U+FFFD the moment
// json.Marshal encodes the tool result — so the model reads back a mangled
// quote of what the session actually said.
func TestMCPTruncateKeepsRunesWhole(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
	}{
		// The shape read_pane caps at 300: an em dash straddling the cut.
		{"em dash at the cap", strings.Repeat("a", 298) + "—done", 300},
		{"emoji in a label", "build✅ing", 10},
		{"multibyte command", strings.Repeat("é", 40) + " --flag", 40},
	}
	for _, tc := range cases {
		got := mcpTruncate(tc.s, tc.n)
		if !utf8.ValidString(got) {
			t.Errorf("%s: cut a codepoint in half: %q", tc.name, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("%s: produced U+FFFD: %q", tc.name, got)
		}
		if n := utf8.RuneCountInString(got); n > tc.n {
			t.Errorf("%s: %d characters, want at most %d", tc.name, n, tc.n)
		}
	}

	if got := mcpTruncate("héllo", 3); got != "hé…" {
		t.Errorf("mcpTruncate(\"héllo\", 3) = %q, want %q", got, "hé…")
	}
	if got := mcpTruncate("short", 40); got != "short" {
		t.Errorf("a string within the budget was touched: %q", got)
	}
	// Wide runes must not be counted as several characters, or the pane table's
	// padded columns (fmt measures %-10s in runes) come out ragged.
	if got := mcpTruncate("日本語のコマンド", 4); utf8.RuneCountInString(got) != 4 {
		t.Errorf("mcpTruncate cut by bytes, not characters: %q", got)
	}
}

// ── argument handling ───────────────────────────────────────────────────────

func TestDecodeArgsRejectsUnknownFields(t *testing.T) {
	var args struct {
		Pane any `json:"pane"`
	}
	if err := decodeArgs(json.RawMessage(`{"pane":0,"panel":true}`), &args); err == nil {
		t.Fatal("a misspelled argument was accepted; additionalProperties:false must bite")
	}
	if err := decodeArgs(json.RawMessage(`{"pane":0}`), &args); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
	if err := decodeArgs(nil, &args); err != nil {
		t.Fatalf("absent arguments rejected: %v", err)
	}
}

func TestToolResultErrorIsASuccessfulResponse(t *testing.T) {
	r := toolResultError("no pane %d", 4)
	if r["isError"] != true {
		t.Error("an execution failure must carry isError so the model can recover")
	}
	content, _ := r["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one text block", r["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || !strings.Contains(block["text"].(string), "no pane 4") {
		t.Errorf("content block = %v", block)
	}
}
