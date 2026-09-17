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
	"encoding/json"
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

// laneKeyFor tells the hub which ops are DELIVERED to a pane, and which pane's
// lane they belong on.
//
// Two ops reach a PTY — `send` and `input` — and both are paced or blocking
// enough that two of them to one pane from one connection must not be
// interleaved. Everything else runs concurrently.
//
// The pane is resolved HERE rather than in the hub because every rule about it
// is magmux's: the wire takes an index or a string, `"*"` is a fan-out that
// neither op has, and an absent pane means the pilot's announced target for
// `send` and nothing at all for `input`. Resolving it at enqueue time is what
// makes a pane-less `send` through `call` land on the same lane as one that
// named the pane.
func (m *Magmux) laneKeyFor(op string, args json.RawMessage) (int, bool, error) {
	switch op {
	case "send", "input":
	default:
		return 0, false, nil
	}
	var a struct {
		Pane any `json:"pane"`
	}
	if len(args) > 0 {
		// A payload that will not decode is the op's problem to report, with
		// its own wording. Here it simply means "no pane named".
		_ = json.Unmarshal(args, &a)
	}
	switch idx := m.parsePaneIndex(a.Pane); {
	case idx >= 0:
		return idx, true, nil
	case idx == paneAll:
		return 0, true, sockErrf(sockCodeBadRequest, `%s has no fan-out: "*" is not a target`, op)
	case idx == paneUnspecified:
		if op == "input" {
			return 0, true, sockErrf(sockCodeBadRequest, "input needs a pane index")
		}
		// One open route means the single-session case. Several means the
		// default would be a guess, and targetPane refuses rather than typing
		// into whichever session happened to be first.
		t, err := m.control.targetPane()
		if err != nil {
			return 0, true, err
		}
		return t, true, nil
	}
	return 0, true, sockErrf(sockCodeBadRequest, "pane is not an index")
}

// subCall hands one request to the connection's Sub, which decides how it runs:
// inline for the watch verbs, on the pane's lane for `send` and `input`, on its
// own goroutine for everything else. It never blocks the socket reader.
//
// This is what closes P2's open item. The direct `send` verb was already
// lane-ordered while `call {op:"send"}` still spawned a goroutine per send, so
// two identical requests by two different routes had different ordering
// guarantees. They now share one.
func (m *Magmux) subCall(msg sockMsg, sub *hub.Sub) {
	op := msg.Type
	args := msg.Args
	if op == "call" {
		op = strings.TrimSpace(msg.Op)
		if op == "" {
			m.replyTo(sub, msg.ID, nil, sockErrf(sockCodeBadRequest,
				`call needs an op: {"type":"call","op":"list","id":1}`))
			return
		}
	} else {
		// watch/unwatch/resync carry their fields at the top level on this
		// transport, because they are socket VERBS here and an `args` object
		// would be a second shape for the same request.
		var err error
		if args, err = m.watchArgsOf(msg); err != nil {
			m.replyTo(sub, msg.ID, nil, err)
			return
		}
	}
	if sub == nil {
		// No connection behind this message (a unit test, an in-process
		// caller): run it on the hub directly, which is what the Sub would have
		// done minus the ordering.
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout(msg.TimeoutMs))
		defer cancel()
		result, err := m.bus().Call(ctx, msg.caller, op, args)
		m.replyTo(sub, msg.ID, result, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout(msg.TimeoutMs))
	id := msg.ID
	sub.Call(ctx, op, args, func(result map[string]any, err error) []byte {
		// The reply is what ends this request, so it is also what releases the
		// budget: Sub.Call calls this exactly once on every path, including the
		// timeout, so cancel is never missed and never early.
		cancel()
		return m.replyBytes(id, result, err)
	})
}

// watchArgsOf builds the args object for the three Sub-level verbs out of the
// message's own top-level fields.
//
// The pane is normalised to an INTEGER here, and refused here: the hub accepts
// only an index, because "which strings name a pane" is magmux's question and
// the answer (parsePaneIndex) lives on this side of the boundary. An absent
// pane is left out entirely rather than passed as a sentinel, so the hub's own
// "watch needs a pane" is what a caller reads instead of "no pane -2".
func (m *Magmux) watchArgsOf(msg sockMsg) (json.RawMessage, error) {
	out := map[string]any{}
	switch idx := m.parsePaneIndex(msg.Pane); {
	case idx >= 0:
		out["pane"] = idx
	case idx == paneAll:
		return nil, sockErrf(sockCodeBadRequest,
			`%s takes one pane: "*" is not a target (watch each pane you want)`, msg.Type)
	case idx == paneUnspecified:
	default:
		return nil, sockErrf(sockCodeBadRequest, "pane is not an index")
	}
	if msg.Mode != "" {
		out["mode"] = msg.Mode
	}
	if msg.FPS != 0 {
		out["fps"] = msg.FPS
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, sockErrf(sockCodeInternal, "%s: args could not be encoded", msg.Type)
	}
	return raw, nil
}

// callerFor labels one socket MESSAGE with the identity of the connection it
// arrived on.
//
// Per message, not per connection, and that is the whole point. Sub.Caller
// resolves the connection's PLUGIN name through the host's conn→plugin registry
// at the moment of the call, because a connection registers as a plugin after
// it — and its Sub — already exist: an identity captured once at connect time
// is the one that is always wrong, and it is the identity
// open_pane {controller:"self"} authorises on.
//
// Client is the opposite kind of field. It is SELF-DECLARED, it rides on each
// message, and it is a panel label and nothing else. Nothing may be authorised
// on it, which is why it is safe for it to change from one line to the next.
func callerFor(sub *hub.Sub, msg sockMsg) hub.Caller {
	c := hub.Caller{Transport: "socket"}
	if sub != nil {
		c = sub.Caller()
	}
	c.Client = msg.Client
	return c
}
