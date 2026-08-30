package main

// `magmux mcp` — a Model Context Protocol server that lets an agent open,
// drive, observe and close real interactive panes in a running magmux.
//
// The server is a separate process from the multiplexer: it shares no memory
// with magmux and reaches it only over the documented unix socket, exactly the
// way the pi pilot does. That keeps the whole feature out of the terminal core
// — from magmux's side an MCP client is just one more socket subscriber.
//
// Transport is MCP's stdio flavour: newline-delimited JSON-RPC 2.0, NOT the
// LSP `Content-Length` framing. The one rule that matters more than any other
// is that stdout carries protocol and nothing else: a single stray byte —
// a stray fmt.Println, a debug print, a panic trace — desynchronises the
// client's parser and the session is over. All logging therefore goes to
// stderr, or to MAGMUX_MCP_LOG when set.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// ── JSON-RPC 2.0 ────────────────────────────────────────────────────────────

// rpcRequest is one inbound message. ID is a json.RawMessage so a numeric or
// string id round-trips verbatim, and — load-bearing — so that a *missing* id
// is distinguishable from `"id": null`. JSON-RPC decides notification vs
// request on the presence of the member, not on its value: a notification must
// never draw a response, while `{"id": null}` is a (malformed but answerable)
// request.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC error codes. The discipline for which of these to use — versus an
// `isError` tool result — is documented on toolResultError in mcp_tools.go.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

// mcpProtocolVersions are the protocol revisions we know how to speak, newest
// first. We echo the client's requested version when it is one of these and
// answer with mcpDefaultProtocol otherwise — the spec's "server picks" branch.
var mcpProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

const mcpDefaultProtocol = "2025-06-18"

// mcpInstructions is placed in the initialize result, which clients fold into
// the model's system prompt. It is the single highest-leverage string in this
// file: everything here is guidance the agent gets for free, before it has
// made its first mistake.
const mcpInstructions = `magmux gives you real interactive terminal panes — TUIs, REPLs, dev servers,
and other coding agents — inside a terminal a human is watching.

Two rules do most of the work:

1. send_and_wait is two-phase and is the tool you want. It waits for the pane
   to visibly START the turn before waiting for it to finish, so you are never
   handed the previous turn's answer. If it reports "stalled", the instruction
   may never have been received: do NOT assume the step was done — read the
   pane and decide.
2. read_pane is your eyes, and it has two sources. transcript:N returns the
   last N turns from the session's OWN record on disk — full text, tool inputs
   and tool results — and is authoritative. The screen is what is painted right
   now, plus a bounded scrollback you reach with offset:N (rows further back).
   Ask for both when you need to know what was said AND what is showing. If it
   says the transcript could not be located, that is NOT an empty session:
   retry, or read the screen.

   Scrollback has one shape you must know: only the ordinary screen records it.
   A full-screen app — which is every coding agent, and vim, htop, less — runs
   on the terminal's ALTERNATE screen, and that is never recorded, by magmux or
   by any other terminal. So offset is how you recover a build log, a test run
   or a dev server's output, and for an agent pane the transcript is the
   history. Each reply tells you how many rows of scrollback that pane has and
   whether you have reached the oldest one.

Other things worth knowing:

- Panes are addressed by index, or by the label you gave them at open_pane.
- A pane whose process exited stays on screen as a tombstone so the human can
  read it. Reclaim the space with close_pane.
- send_keys is for the moments send_and_wait cannot handle: answering a
  permission prompt, choosing from a menu, or pressing ctrl-c.
- Never drive the pane you are running in — the server refuses it, because a
  turn you are inside can never finish.
- No session yet? Call list_sessions, then request_session for instructions on
  how to get one started.`

// ── server ──────────────────────────────────────────────────────────────────

type mcpServer struct {
	out   *bufio.Writer
	outMu sync.Mutex

	logw  io.Writer
	logMu sync.Mutex

	sessMu   sync.Mutex
	sessions map[string]*Session
	defID    string

	ancMu     sync.Mutex
	ancestors map[int]bool

	clientName string

	wg sync.WaitGroup
}

func newMCPServer(out io.Writer, logw io.Writer) *mcpServer {
	return &mcpServer{
		out:      bufio.NewWriter(out),
		logw:     logw,
		sessions: map[string]*Session{},
	}
}

// runMCP is the `magmux mcp` entry point. It returns a process exit code and
// must never write anything but protocol to stdout.
func runMCP(args []string) int {
	for _, a := range args {
		switch a {
		case "--help", "-h", "help":
			// Only ever printed when explicitly asked for. A client that spawns
			// us with an unexpected flag gets a stderr warning, not a help
			// screen on stdout that would corrupt its parser.
			printMCPHelp(os.Stdout)
			return 0
		}
	}

	logw := io.Writer(os.Stderr)
	if p := os.Getenv("MAGMUX_MCP_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			defer f.Close()
			logw = f
		} else {
			fmt.Fprintf(os.Stderr, "magmux mcp: cannot open MAGMUX_MCP_LOG %q: %v\n", p, err)
		}
	}

	s := newMCPServer(os.Stdout, logw)
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			s.logf("ignoring unknown flag %q", a)
		}
	}
	return s.serve(os.Stdin)
}

func printMCPHelp(w io.Writer) {
	fmt.Fprint(w, `magmux mcp — MCP server over stdio

Speaks JSON-RPC 2.0 as newline-delimited JSON on stdin/stdout, exposing a
running magmux to an MCP client so an agent can open, drive, read and close
interactive panes.

Register it with Claude Code:

  claude mcp add magmux -- magmux mcp

It attaches, in priority order, to:
  1. the magmux this process is already running inside ($MAGMUX_SOCK)
  2. the single reachable magmux-*.sock in the socket directory
  3. whatever attach_session is pointed at

Environment:
  MAGMUX_SOCK      socket of the host magmux, exported to every pane
  MAGMUX_SOCK_DIR  where to look for sockets (default: /tmp)
  MAGMUX_MCP_LOG   log file; without it, logs go to stderr

If you run magmux with --sock-dir, set MAGMUX_SOCK_DIR to the same directory
in this client's own configuration (its "env" block). --sock-dir reaches
children of that magmux, so a "magmux mcp" started from inside a pane inherits
it — but it cannot reach a server the client launched in its own process tree,
and no runtime mechanism can. The env var is the only channel that spans both.

stdout carries protocol only — never print to it.
`)
}

func (s *mcpServer) logf(format string, a ...any) {
	if s.logw == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	fmt.Fprintf(s.logw, "%s magmux-mcp: %s\n",
		time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

// serve reads newline-delimited JSON-RPC from in until EOF.
//
// The 4MB scanner budget matters: a read_pane of a wide alt-screen, or a tool
// result carrying a captured screen, comfortably exceeds bufio's 64KB default,
// and an oversized token does not merely get skipped — Scan() returns false
// and the whole session dies silently.
func (s *mcpServer) serve(in io.Reader) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		s.handleLine(append([]byte(nil), line...))
	}
	if err := sc.Err(); err != nil {
		s.logf("stdin: %v", err)
	}
	s.shutdown()
	return 0
}

// shutdown closes every attached session. In-flight tool calls are abandoned:
// stdin closing means the client is gone, so there is nobody left to answer.
func (s *mcpServer) shutdown() {
	s.sessMu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessions = map[string]*Session{}
	s.defID = ""
	s.sessMu.Unlock()
	for _, sess := range sessions {
		sess.Close()
	}
	s.outMu.Lock()
	_ = s.out.Flush()
	s.outMu.Unlock()
}

func (s *mcpServer) handleLine(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.respondError(nil, rpcParseError, "parse error: "+err.Error(), nil)
		return
	}

	// Notifications never draw a response — not a result, and not an error
	// either, however malformed they are. `notifications/*` is checked before
	// the id so a client that (wrongly) tags one with an id still gets silence.
	if req.Method == "" || strings.HasPrefix(req.Method, "notifications/") || req.ID == nil {
		if req.Method == "" && req.ID != nil {
			s.respondError(req.ID, rpcInvalidRequest, "invalid request: no method", nil)
		}
		return
	}

	switch req.Method {
	case "initialize":
		s.respond(req.ID, s.initializeResult(req.Params))
	case "ping":
		// struct{}{} rather than an empty map: `omitempty` drops an empty map,
		// which would emit a response with no result member at all.
		s.respond(req.ID, struct{}{})
	case "tools/list":
		s.respond(req.ID, map[string]any{"tools": mcpToolSchemas()})
	case "tools/call":
		// Off the reader goroutine: send_and_wait can legitimately block for
		// fifteen minutes, and a client's pings must still be answered.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleToolCall(req)
		}()
	case "resources/list":
		s.respond(req.ID, map[string]any{"resources": []any{}})
	case "prompts/list":
		s.respond(req.ID, map[string]any{"prompts": []any{}})
	case "shutdown":
		s.respond(req.ID, struct{}{})
	default:
		s.respondError(req.ID, rpcMethodNotFound, "method not found: "+req.Method, nil)
	}
}

func (s *mcpServer) initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}

	version := mcpDefaultProtocol
	for _, v := range mcpProtocolVersions {
		if p.ProtocolVersion == v {
			version = v
			break
		}
	}

	if p.ClientInfo.Name != "" {
		name := p.ClientInfo.Name
		if p.ClientInfo.Version != "" {
			name += "/" + p.ClientInfo.Version
		}
		s.sessMu.Lock()
		s.clientName = name
		s.sessMu.Unlock()
	}
	s.logf("initialize from %q, protocol %s -> %s", p.ClientInfo.Name, p.ProtocolVersion, version)

	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "magmux",
			"version": Version,
		},
		"instructions": mcpInstructions,
	}
}

func (s *mcpServer) respond(id json.RawMessage, result any) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: rpcID(id), Result: result})
}

func (s *mcpServer) respondError(id json.RawMessage, code int, msg string, data any) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: rpcID(id), Error: &rpcError{Code: code, Message: msg, Data: data}})
}

// rpcID normalises a possibly-absent id into the literal that belongs in a
// response. An error answering an unparseable message carries `"id": null`.
func rpcID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func (s *mcpServer) write(resp rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		// Marshalling our own response failed, so the result is untrustworthy;
		// answer the request rather than leaving the client hanging.
		s.logf("marshal response: %v", err)
		data, err = json.Marshal(rpcResponse{
			JSONRPC: "2.0",
			ID:      resp.ID,
			Error:   &rpcError{Code: rpcInternalError, Message: "failed to encode result"},
		})
		if err != nil {
			return
		}
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(data)
	_ = s.out.WriteByte('\n')
	if err := s.out.Flush(); err != nil {
		s.logf("stdout: %v", err)
	}
}

// ── shared helpers ──────────────────────────────────────────────────────────

// decodeArgs unmarshals a tool's arguments strictly. Unknown fields are a
// hard error so that `additionalProperties: false` in the schema means
// something at runtime too — a misspelled argument that silently defaults is
// far more expensive to debug than one that is rejected outright.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

// ppidLookup is ppidOf, indirected so a test can stage the unreadable
// /proc/<pid>/stat this guard has to survive. Never reassigned in production.
var ppidLookup = ppidOf

// ancestorPIDs returns our own pid and every ancestor pid.
//
// This is the self-pane guard's raw material: if a pane's process is one of
// these, that pane is the one we are running inside, and driving it would
// block forever on a turn we are ourselves in the middle of.
func (s *mcpServer) ancestorPIDs() map[int]bool {
	pids, _ := s.ancestry()
	return pids
}

// ancestry returns the pid set and whether the walk actually REACHED THE TOP.
//
// Only a complete walk is memoised, and that is the whole point. One unreadable
// /proc/<pid>/stat — a restrictive container, an LSM, a transient sysctl
// failure — breaks the walk after a single step, and caching that leaves the
// guard knowing nothing but our own pid for the life of the process. The pane
// the MCP server is running inside then fails to be recognised as
// self-targeting, and a send_and_wait aimed at it deadlocks on a turn the
// caller is itself inside: exactly what the guard exists to prevent.
//
// So a partial walk is returned but not kept, and the next call retries. The
// second return value is the "degrade loudly" half: callers that would
// otherwise silently permit a self-target say instead that they could not tell.
func (s *mcpServer) ancestry() (map[int]bool, bool) {
	s.ancMu.Lock()
	defer s.ancMu.Unlock()
	if s.ancestors != nil {
		return s.ancestors, true
	}
	// Seeded rather than filled by the loop: we are always our own ancestor, and
	// under `os.Getpid() == 1` the loop body never runs at all.
	self := os.Getpid()
	seen := map[int]bool{self: true}
	pid, complete := self, false
	for i := 0; i < 64; i++ {
		if pid <= 1 {
			complete = true // walked all the way to init
			break
		}
		parent, err := ppidLookup(pid)
		if err != nil {
			break // this link is unreadable, so everything above it is unknown
		}
		if parent <= 0 {
			complete = true // no parent: the top of the tree, reached honestly
			break
		}
		if seen[parent] {
			break // a cycle is impossible, but a bad read must not spin
		}
		seen[parent] = true
		pid = parent
	}
	if complete {
		s.ancestors = seen
	}
	return seen, complete
}

// ctxWithTimeout is context.WithTimeout with 0 meaning "no deadline", which is
// what send_and_wait needs — its own two-phase timeouts are the real bound.
func ctxWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, d)
}
