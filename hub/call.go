package hub

// Sub.Call: one request from one connection, answered exactly once.
//
// Three shapes of work arrive here and they must not share a queue:
//
//   - watch / unwatch / resync are SUB-LEVEL verbs, not registry ops. They do
//     no I/O — a pane lookup, a map write, starting a framer — so they run
//     inline on the caller's goroutine and keep submission order per Sub. That
//     is also what lets `watch` enqueue its reply and activate its slot in one
//     lock acquisition (watch.go), which is the only reason no frame can
//     overtake the answer that announced it.
//   - input and send are DELIVERED to a pane: hundreds of milliseconds of paced
//     PTY writes. They go onto this connection's lane for that pane, so two
//     instructions to one pane from one connection cannot be typed into each
//     other, and a PTY that stopped reading stalls that lane and nothing else.
//   - everything else runs on its own goroutine, up to maxInFlightCalls at a
//     time. Concurrency is what keeps a slow `transcript` from blocking a
//     `list` behind it; the bound is what stops one client spawning goroutines
//     without limit.
//
// In every shape, the transport READER is never blocked: Call returns as soon
// as the work is placed.

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/MadAppGang/magmux/protocol"
)

// maxInFlightCalls bounds the ops one connection may have running at once.
// Sixteen is well past any real client's concurrency (MCP fans out a handful of
// tool calls) and far short of a goroutine leak.
const maxInFlightCalls = 16

// Reply turns one op's outcome into the bytes for this connection, or nil when
// the request asked for no answer (the socket's legacy no-id path).
//
// It is a function and not a channel because the WIRE SHAPE of a reply belongs
// to the transport — a socket line, a WS text frame, an SSE event — and the hub
// has no business knowing which. It returns bytes rather than sending them so
// the hub can queue the answer and change its own state in ONE lock
// acquisition, which is what `watch` needs.
//
// It is called EXACTLY ONCE per Call, on some goroutine, possibly after Call
// has returned.
type Reply func(result map[string]any, err error) []byte

// LaneKeyFunc says whether an op is delivered to a pane, and which one.
//
// ok=false means "an ordinary op": run it concurrently. ok=true with a non-nil
// error means the op IS lane-bound but its target could not be resolved, which
// is the caller's mistake and is answered without queueing anything.
//
// It is a port rather than a hardcoded switch because resolving a pane is
// magmux's question: the wire accepts an index or a string, an absent pane may
// have a default, and the id table is append-only with tombstones. None of that
// is the hub's to know.
type LaneKeyFunc func(op string, args json.RawMessage) (pane int, ok bool, err error)

// SetLaneKey installs the resolver. With none, defaultLaneKey applies: `input`
// and `send` are lane-bound and their `pane` must be an integer.
func (h *Hub) SetLaneKey(f LaneKeyFunc) {
	h.mu.Lock()
	h.laneKey = f
	h.mu.Unlock()
}

// LaneKey resolves an op's delivery lane, exactly as Sub.Call does internally.
//
// It is exported for the one adapter that cannot go through Sub.Call: the
// Firebase command executor writes a durable claim as the FIRST STEP INSIDE the
// lane item, so it has to build the item itself. Every other transport should
// use Sub.Call, which does this and the concurrency bound together.
func (h *Hub) LaneKey(op string, args json.RawMessage) (pane int, ok bool, err error) {
	return h.laneKeyFor(op, args)
}

// laneKeyFor resolves an op's lane with no lock held across the call.
func (h *Hub) laneKeyFor(op string, args json.RawMessage) (int, bool, error) {
	h.mu.RLock()
	f := h.laneKey
	h.mu.RUnlock()
	if f == nil {
		f = defaultLaneKey
	}
	return f(op, args)
}

// defaultLaneKey is the fallback: the two ops that reach a PTY, keyed on an
// integer `pane`.
func defaultLaneKey(op string, args json.RawMessage) (int, bool, error) {
	switch op {
	case "input", "send":
	default:
		return 0, false, nil
	}
	var a protocol.PaneArg
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	if len(a.Pane) == 0 {
		return 0, true, protocol.Errf(protocol.CodeBadRequest, "%s needs a pane", op)
	}
	n, err := strconv.Atoi(string(a.Pane))
	if err != nil {
		return 0, true, protocol.Errf(protocol.CodeBadRequest, "%s: pane is not an index", op)
	}
	return n, true, nil
}

// Call runs one op for this connection and answers it exactly once.
//
// ctx bounds the WAIT FOR THE ANSWER and nothing else. At expiry the caller is
// told `timeout`, and a lane item already running keeps running and still holds
// its place: a caller giving up must never reorder a queue or truncate an
// instruction half-typed. The RUN context of a lane item is the lane's, which
// only Quiesce cancels.
//
// The caller owns ctx's cancel func and may call it from inside reply, which is
// invoked exactly once on every path.
func (s *Sub) Call(ctx context.Context, op string, args json.RawMessage, reply Reply) {
	a := &answerer{sub: s, reply: reply}

	switch op {
	case "watch":
		s.callWatch(args, a)
		return
	case "unwatch":
		s.callUnwatch(args, a)
		return
	case "resync":
		s.callResync(args, a)
		return
	}

	pane, lane, err := s.hub.laneKeyFor(op, args)
	if lane {
		if err != nil {
			a.send(nil, err)
			return
		}
		s.callOnLane(ctx, pane, op, args, a)
		return
	}

	if !s.takeCallSlot() {
		a.send(nil, protocol.Errf(protocol.CodeBusy,
			"this connection already has %d requests running", maxInFlightCalls))
		return
	}
	c := s.Caller()
	go func() {
		defer s.freeCallSlot()
		result, err := s.hub.Call(ctx, c, op, args)
		a.send(result, err)
	}()
}

// callOnLane queues a delivery and answers when it is done — or when the
// caller's own budget runs out, whichever comes first, WITHOUT letting the
// second case disturb the lane.
func (s *Sub) callOnLane(ctx context.Context, pane int, op string, args json.RawMessage, a *answerer) {
	c := s.Caller()
	err := s.Deliver(pane, LaneItem{
		Run: func(runCtx context.Context) {
			done := make(chan struct{})
			var (
				result  map[string]any
				callErr error
			)
			go func() {
				defer close(done)
				result, callErr = s.hub.Call(runCtx, c, op, args)
			}()
			select {
			case <-done:
				a.send(result, callErr)
			case <-ctx.Done():
				// The caller stopped waiting. It gets an answer now; the item
				// stays on the lane until it finishes, so the next instruction
				// still follows this one.
				a.send(nil, protocol.Errf(protocol.CodeTimeout,
					"%s is still being delivered to pane %d; the wait for its outcome expired", op, pane))
				<-done
			}
		},
		Discard: func() {
			a.send(nil, protocol.Errf(protocol.CodeNotReady,
				"pane %d: the request was discarded unrun; magmux is shutting down", pane))
		},
		Size: len(args) + 64,
	})
	if err != nil {
		a.send(nil, err)
	}
}

// ── the Sub-level verbs ─────────────────────────────────────────────────────

// watchArgs is the shape of `watch`. Mode and FPS are optional and are resolved
// rather than refused when absent.
type watchArgs struct {
	Pane *int               `json:"pane"`
	Mode protocol.WatchMode `json:"mode"`
	FPS  int                `json:"fps"`
}

func decodeWatchArgs(verb string, args json.RawMessage) (watchArgs, error) {
	var a watchArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return a, protocol.Errf(protocol.CodeBadRequest, "%s: args must be a JSON object (%v)", verb, err)
		}
	}
	if a.Pane == nil {
		return a, protocol.Errf(protocol.CodeBadRequest, `%s needs a pane: {"type":%q,"pane":3,"id":1}`, verb, verb)
	}
	return a, nil
}

// callWatch is the one path that answers a watch REQUEST. The reply is queued
// and the slot activated together, so the first frame can never precede it.
func (s *Sub) callWatch(args json.RawMessage, a *answerer) {
	w, err := decodeWatchArgs("watch", args)
	if err != nil {
		a.send(nil, err)
		return
	}
	info, err := s.watchInactive(*w.Pane, w.Mode, w.FPS)
	if err != nil {
		a.send(nil, err)
		return
	}
	msg := a.bytes(map[string]any{
		"pane": info.Pane, "rows": info.Rows, "cols": info.Cols,
		"mode": string(info.Mode), "fps": info.FPS,
	}, nil)
	s.replyAndActivate(*w.Pane, msg)
}

func (s *Sub) callUnwatch(args json.RawMessage, a *answerer) {
	w, err := decodeWatchArgs("unwatch", args)
	if err != nil {
		a.send(nil, err)
		return
	}
	s.Unwatch(*w.Pane)
	a.send(map[string]any{"pane": *w.Pane, "watching": false}, nil)
}

func (s *Sub) callResync(args json.RawMessage, a *answerer) {
	w, err := decodeWatchArgs("resync", args)
	if err != nil {
		a.send(nil, err)
		return
	}
	if err := s.Resync(*w.Pane); err != nil {
		a.send(nil, err)
		return
	}
	a.send(map[string]any{"pane": *w.Pane, "resync": true}, nil)
}

// ── plumbing ────────────────────────────────────────────────────────────────

// answerer guarantees one answer per request.
//
// Two goroutines race for it on the lane path — the item that finished and the
// caller whose budget expired — and a connection that received two replies to
// one id would leave a client matching the second against a request it had
// already retired.
type answerer struct {
	sub   *Sub
	reply Reply
	once  sync.Once
}

// send answers and queues the bytes through the ordinary FIFO.
func (a *answerer) send(result map[string]any, err error) {
	if msg := a.bytes(result, err); len(msg) > 0 {
		a.sub.Send(msg)
	}
}

// bytes answers and hands the bytes back instead of queueing them, for the one
// caller that must queue the reply under the same lock as a state change:
// `watch`, whose slot must go active in the same breath as its answer.
func (a *answerer) bytes(result map[string]any, err error) []byte {
	var msg []byte
	a.once.Do(func() {
		if a.reply != nil {
			msg = a.reply(result, err)
		}
	})
	return msg
}

func (s *Sub) takeCallSlot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= maxInFlightCalls {
		return false
	}
	s.calls++
	return true
}

func (s *Sub) freeCallSlot() {
	s.mu.Lock()
	if s.calls > 0 {
		s.calls--
	}
	s.mu.Unlock()
}
