package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/ws"
)

// credential is the outcome of authenticating one request.
type credential struct {
	kind auth.Kind
	// via names the channel it arrived on, for the debug log and for the
	// Caller's Conn label. It is never authorisation input.
	via string
}

// ticketAllowed says which endpoints may present a ticket instead of a token.
//
// It is an allow-list of exactly two, and it is a list rather than a flag
// because a ticket is a WEAKER credential in one specific way: it travels in
// the URL, where it is logged. Only the two endpoints that genuinely cannot set
// a header — EventSource, and a browser WebSocket that chose not to use the
// subprotocol channel — may take one.
type ticketAllowed bool

const (
	noTicket  ticketAllowed = false
	useTicket ticketAllowed = true
)

// authenticate resolves the caller's credential, or writes the refusal and
// returns false.
//
// Order: ticket (where allowed), then the WebSocket subprotocol, then the
// Authorization header. A request carrying more than one presents whichever
// comes first; there is no "best" credential to pick, and trying several in
// turn would make a wrong token indistinguishable from a spent ticket.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, allowTicket ticketAllowed) (credential, bool) {
	if allowTicket == useTicket {
		if t := r.URL.Query().Get("ticket"); t != "" {
			kind, ok := s.cfg.Tickets.Redeem(t)
			if !ok {
				// Spent, expired and never-existed are one answer on purpose:
				// telling them apart would make the ticket set enumerable.
				// EventSource treats 401 as fatal and stops retrying, which is
				// what the documented reconnect wants — mint a new ticket and
				// open a new stream.
				s.unauthorized(w, "the ticket is spent, expired or unknown")
				return credential{}, false
			}
			return credential{kind: kind, via: "ticket"}, true
		}
	}

	if tok := ws.TokenFromSubprotocols(r.Header); tok != "" {
		kind := s.cfg.Tokens.Check(tok)
		if kind == auth.None {
			s.unauthorized(w, "the token in Sec-WebSocket-Protocol is not this magmux's")
			return credential{}, false
		}
		return credential{kind: kind, via: "subprotocol"}, true
	}

	h := r.Header.Get("Authorization")
	if h == "" {
		s.unauthorized(w, "no credential; send Authorization: Bearer <token>")
		return credential{}, false
	}
	scheme, value, _ := strings.Cut(h, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		s.unauthorized(w, "Authorization must use the Bearer scheme")
		return credential{}, false
	}
	kind := s.cfg.Tokens.Check(strings.TrimSpace(value))
	if kind == auth.None {
		s.unauthorized(w, "the bearer token is not this magmux's")
		return credential{}, false
	}
	return credential{kind: kind, via: "bearer"}, true
}

func (s *Server) unauthorized(w http.ResponseWriter, why string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="magmux"`)
	writeErr(w, protocol.Errf(protocol.CodeUnauthorized, "%s", why))
}

// caller builds the hub identity for one request.
//
// Client is self-declared (X-Magmux-Client, or a WebSocket `hello`) and is a
// panel label only: nothing is ever authorised on it, and it is truncated
// because it ends up on a status bar with a fixed width.
func (s *Server) caller(r *http.Request, transport string, cred credential, readOnly bool) hub.Caller {
	return hub.Caller{
		Transport: transport,
		Conn:      fmt.Sprintf("%s#%d", transport, s.connSeq.Add(1)),
		Client:    clientLabel(r.Header.Get("X-Magmux-Client")),
		ReadOnly:  readOnly,
	}
}

func clientLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// authorizeOp is the view token's whole enforcement point on this transport.
//
// A full-token caller passes. A viewer passes for a BUILT-IN read op, or for an
// op --view-op named; everything else is forbidden here, before the hub is
// asked, so a refused call never reaches a lane or a pane.
//
// It returns the ReadOnly flag to put on the Caller. For a --view-op grant that
// is false, because the hub enforces the class rule on its own and would
// otherwise refuse the very op the operator just granted. The decision is made
// once, here, rather than argued twice in two places that could disagree.
func (s *Server) authorizeOp(kind auth.Kind, name string) (readOnly bool, err error) {
	spec, registered := s.cfg.Hub.Spec(name)
	if err := s.cfg.Tokens.Authorize(kind, spec, registered); err != nil {
		return true, err
	}
	return s.cfg.Tokens.EffectiveReadOnly(kind, name), nil
}

// readArgs decodes a request body into op args, bounded at MaxBody.
//
// An empty body is an empty args object rather than an error: half the ops take
// no arguments, and `curl -X POST` with no body is the obvious way to call one.
func readArgs(w http.ResponseWriter, r *http.Request) (json.RawMessage, error) {
	if r.Body == nil {
		return nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, protocol.Errf(protocol.CodeTooLarge,
				"the request body exceeds the %d-byte limit", MaxBody)
		}
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, protocol.Errf(protocol.CodeBadRequest, "the body must be a JSON object (%v)", err)
	}
	// A body that is present must be an object: an op's args are named, and a
	// bare array or number would decode into an empty sockMsg and run the op
	// with no arguments at all, which is worse than refusing it.
	if t := firstNonSpace(raw); t != 0 && t != '{' && t != 'n' {
		return nil, protocol.Errf(protocol.CodeBadRequest, "the body must be a JSON object")
	}
	return raw, nil
}

func firstNonSpace(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			return c
		}
	}
	return 0
}
