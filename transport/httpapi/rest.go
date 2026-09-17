package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/protocol"
)

// restTimeout bounds one stateless request. It is shorter than the socket's
// 30 s `call` default on purpose: an HTTP client has its own timeout and a
// proxy in between has another, so a request that outlives both is answered to
// nobody. A caller that wants a long `send` uses the WebSocket, where the reply
// arrives whenever it arrives.
const restTimeout = 30 * time.Second

// callOp is the one path every REST endpoint takes.
//
// Every endpoint here — /v1/panes, /v1/panes/{n}/screen, /v1/ops/{name} — is an
// op call with a different way of naming the op and its args. Routing them all
// through one function is what keeps the auth check, the status mapping and the
// body shape from drifting between a convenience URL and the generic one.
//
// Stateless REST calls do NOT make the caller a controller in the panel. There
// is no connection to announce arriving or going away, so a panel row would
// name something that had already ended. That is a known limitation, and the
// remedy is the WebSocket.
func (s *Server) callOp(w http.ResponseWriter, r *http.Request, transport, name string, args json.RawMessage, cred credential, envelope bool) {
	readOnly, err := s.authorizeOp(cred.kind, name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if s.cfg.Hub.Closing() {
		writeErr(w, protocol.Errf(protocol.CodeNotReady, "magmux is shutting down and is not taking new requests"))
		return
	}
	// Wait for the layout, exactly as the socket does before it serves a
	// connection. The listener is bound before the first child forks — that is
	// what makes an accepting port a readiness signal for the token file — so
	// requests DO arrive while m.root and m.allPanes are still nil, and serving
	// one there answers `list` with an empty pane array. An empty array is not
	// an error a caller can see: it reads as "this session has no panes", which
	// is a different and permanent-looking fact.
	if !s.cfg.Ready(LayoutWait) {
		writeErr(w, protocol.Errf(protocol.CodeNotReady, "magmux has no layout yet"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), restTimeout)
	defer cancel()
	result, err := s.cfg.Hub.Call(ctx, s.caller(r, transport, cred, readOnly), name, args)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !envelope {
		// The convenience endpoints return the op's own object, because that is
		// what their URL promised: GET /v1/panes is the pane list, not a
		// wrapper around it.
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

// handleCapabilities answers what this magmux is, plus what its transports are.
//
// The transports block is added HERE and not by the op, because it is a fact
// about this adapter — whether TLS is on, whether the bind is loopback — and
// the op is shared with the socket, which has no such fact.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	cred, ok := s.authenticate(w, r, noTicket)
	if !ok {
		return
	}
	if !s.cfg.Ready(LayoutWait) {
		writeErr(w, protocol.Errf(protocol.CodeNotReady, "magmux has no layout yet"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), restTimeout)
	defer cancel()
	result, err := s.cfg.Hub.Call(ctx, s.caller(r, "http", cred, cred.kind.ReadOnly()), "capabilities", nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	if len(s.cfg.Transports) > 0 {
		result["transports"] = s.cfg.Transports
	}
	result["readOnly"] = cred.kind.ReadOnly()
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleOps(w http.ResponseWriter, r *http.Request) {
	cred, ok := s.authenticate(w, r, noTicket)
	if !ok {
		return
	}
	s.callOp(w, r, "http", "ops", nil, cred, false)
}

func (s *Server) handlePanes(w http.ResponseWriter, r *http.Request) {
	cred, ok := s.authenticate(w, r, noTicket)
	if !ok {
		return
	}
	s.callOp(w, r, "http", "list", nil, cred, false)
}

// handleScreen is `capture` with the pane in the path and the two knobs in the
// query, so a screen is a GET-able resource a browser can open directly.
func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	cred, ok := s.authenticate(w, r, noTicket)
	if !ok {
		return
	}
	pane, err := strconv.Atoi(r.PathValue("pane"))
	if err != nil {
		writeErr(w, protocol.Errf(protocol.CodeBadRequest, "pane must be an index"))
		return
	}
	args := map[string]any{"pane": pane}
	for _, k := range []string{"lines", "offset"} {
		v := r.URL.Query().Get(k)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			writeErr(w, protocol.Errf(protocol.CodeBadRequest, "%s must be an integer", k))
			return
		}
		args[k] = n
	}
	raw, _ := json.Marshal(args)
	s.callOp(w, r, "http", "capture", raw, cred, false)
}

// handleCallOp is the generic surface: any registered op, including ops a
// plugin added after this magmux started, with no endpoint to add per op.
func (s *Server) handleCallOp(w http.ResponseWriter, r *http.Request) {
	cred, ok := s.authenticate(w, r, noTicket)
	if !ok {
		return
	}
	args, err := readArgs(w, r)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.callOp(w, r, "http", r.PathValue("name"), args, cred, true)
}

// handleTicket mints a single-use credential for the two endpoints that cannot
// carry a header.
//
// It is adapter-level and is NOT a registered op: it never appears in /v1/ops,
// in an MCP tool list, or in --view-op's reach. A viewer may mint one, and it
// inherits the viewer's kind, so the ticket is not a way up.
func (s *Server) handleTicket(w http.ResponseWriter, r *http.Request) {
	cred, ok := s.authenticate(w, r, noTicket)
	if !ok {
		return
	}
	t, err := s.cfg.Tickets.Mint(cred.kind)
	if err != nil {
		writeErr(w, protocol.Errf(protocol.CodeInternal, "minting a ticket: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":    t,
		"expiresIn": int(s.cfg.Tickets.TTL() / time.Second),
		"readOnly":  cred.kind == auth.View,
	})
}
