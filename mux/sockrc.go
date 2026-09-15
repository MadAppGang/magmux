package mux

// The registry's socket face: the `ops` and `call` verbs.
//
// Both are ID-PATH ONLY, and that is a property of where they are dispatched
// rather than a check they make: dispatchSocketVerbExt is reached from the
// id-carrying path alone, so a no-id `call` falls through to the main table
// and is answered with the silence every pre-reply client was written
// against. A verb whose whole purpose is to return a result has nothing to say
// to a caller that asked for no reply.
//
// `call` exists so a transport that is not this socket — HTTP, WebSocket,
// Firebase, a plugin — has one shape for every request, including ops magmux
// itself does not implement. Here it is the same dispatch the socket verbs use,
// reached through the registry, so a client may use either and get the same
// answer from the same code.

import (
	"context"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// Bounds on a `call`. The default is generous because the slowest built-in is
// `send`, whose pacing is measured in hundreds of milliseconds and whose reply
// means "the bytes reached the PTY"; the ceiling exists so a caller cannot
// park a socket goroutine forever by asking for it.
const (
	callTimeoutDefault = 30 * time.Second
	callTimeoutMax     = 15 * time.Minute
)

// sockOps advertises every op with its class and schema, plus the revision of
// that list.
//
// The revision is the point. Plugins register and die while magmux runs, so a
// client that caches the list needs to know when the list it has stopped being
// the list magmux has — without diffing two arrays of schemas on every request.
func (m *Magmux) sockOps() (map[string]any, error) {
	ops, rev := m.bus().Ops()
	return map[string]any{"ops": ops, "rev": rev}, nil
}

// sockCall runs one registered op on behalf of this connection.
//
// It runs on the socket reader goroutine, exactly as every other verb does
// today, so a `call` keeps its order relative to the verbs around it on the
// same connection. (P2 moves this onto the connection's Sub, where a send's
// PTY delivery goes into a per-pane lane; the ORDER a caller observes is the
// same either way, which is why this can land first.)
func (m *Magmux) sockCall(msg sockMsg) (map[string]any, error) {
	name := strings.TrimSpace(msg.Op)
	if name == "" {
		return nil, sockErrf(sockCodeBadRequest, `call needs an op: {"type":"call","op":"list","id":1}`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout(msg.TimeoutMs))
	defer cancel()
	return m.bus().Call(ctx, msg.caller, name, msg.Args)
}

// callTimeout resolves a request's budget. A caller that named none gets the
// default; one that named more than the ceiling is clamped rather than
// refused, because the request is well formed and the ceiling is magmux's
// policy, not the caller's mistake.
func callTimeout(ms int) time.Duration {
	switch {
	case ms <= 0:
		return callTimeoutDefault
	case time.Duration(ms)*time.Millisecond > callTimeoutMax:
		return callTimeoutMax
	}
	return time.Duration(ms) * time.Millisecond
}

// callIsDriving reports whether a `call` makes its connection a CONTROLLER —
// something the panel announces arriving and going away — as opposed to an
// observer.
//
// The rule is the op's class and nothing else: anything that is not `read`
// steers the session. It matters most for the paths that only ever read.
// `magmux mcp` resolves sessions eagerly and reads panes to serve resources,
// and if that marked it as driving, every agent that merely loaded the MCP
// server inside a controlled pane would appear in the panel as a controller and
// reset the ledger of the pilot that was actually driving.
//
// An unknown op is not driving. It cannot be: it will be refused.
func (m *Magmux) callIsDriving(msg sockMsg) bool {
	spec, ok := m.bus().Spec(strings.TrimSpace(msg.Op))
	return ok && spec.Class != protocol.ClassRead
}

// callerFor labels one socket connection for the ops it dispatches. Conn is
// magmux's own label for the connection; Client is self-declared and is a panel
// label only, never anything an op may authorise on.
//
// Plugin identity is deliberately absent here and is resolved per message from
// the connection's registration (P5): a connection registers as a plugin after
// it already exists, so an identity captured once is the one that is always
// wrong.
func callerFor(conn string, msg sockMsg) hub.Caller {
	return hub.Caller{Transport: "socket", Conn: conn, Client: msg.Client}
}
