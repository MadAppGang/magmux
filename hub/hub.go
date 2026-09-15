package hub

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// Caller is who is asking. It is assembled by the adapter that received the
// request and passed to every OpFunc, which is the only identity an op ever
// sees: no op is handed a connection, a socket or an HTTP request.
type Caller struct {
	// Transport is how the request arrived: "socket", "http", "ws", "sse" or
	// "firebase". An MCP client arrives as "socket", because `magmux mcp` is a
	// separate process that speaks the socket like anything else.
	Transport string
	// Conn identifies the connection within that transport ("sock#12",
	// "ws#3"). It is magmux's own label, not the peer's.
	Conn string
	// Client is SELF-DECLARED and is a panel label only. Nothing may be
	// authorised on it.
	Client string
	// Plugin is the registered plugin name of the connection this call came
	// in on, or "" for anything that is not a plugin. It is RESOLVED at each
	// call by Sub.Caller and never captured, because a connection registers
	// as a plugin after its Sub already exists.
	Plugin string
	// ReadOnly is set when the caller authenticated with the view token. It
	// is refused everything that is not protocol.ClassRead.
	ReadOnly bool
}

// OpFunc runs one op. It is called with NO hub lock held, on whatever
// goroutine the adapter dispatched from, and may block: ctx is what bounds it.
type OpFunc func(ctx context.Context, c Caller, args json.RawMessage) (map[string]any, error)

// Op is one registered op: how it is advertised, and what it does.
type Op struct {
	Spec protocol.OpSpec
	Fn   OpFunc
}

// emptyObjectSchema is what an op that takes no arguments advertises. A nil
// Schema would marshal as null, which is not a valid JSON Schema and not a
// valid MCP inputSchema, so the registry fills it in rather than letting the
// hole reach the wire.
var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)

// Hub is the registry and the bus. The zero value is not usable; call New.
type Hub struct {
	// mu is a LEAF: see the package doc. It guards the registry, the Sub set
	// and the shutdown flags, and is never held across an OpFunc, a Sink call
	// or a Watcher call. The one order it takes part in is mu -> sub.mu.
	mu   sync.RWMutex
	ops  map[string]Op
	rev  int
	subs map[*Sub]struct{}
	// closing is set by Quiesce: from then on every Call is refused with
	// not_ready, which is the same refusal the socket gives before the layout
	// exists.
	closing bool
	// finalized is set by Finalize. A Session opened after it does not
	// subscribe to anything: it is replayed the finals and closed.
	finalized bool
	finals    [][]byte
	// calls holds the cancel func of every in-flight Call, so Quiesce can
	// cancel them all. inflight counts the same calls; every Add happens under
	// mu together with the closing check, so no Add can race Quiesce's Wait.
	calls    map[uint64]context.CancelFunc
	nextCall uint64
	inflight sync.WaitGroup
	// lanes is every live (Sub, pane) lane, INCLUDING those whose Sub is closed
	// and merely draining. Quiesce is the only thing that discards a queued
	// item, so it has to be able to reach a lane its connection has already
	// outlived (see lane.go).
	lanes map[*lane]struct{}
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{
		ops:   make(map[string]Op),
		subs:  make(map[*Sub]struct{}),
		calls: make(map[uint64]context.CancelFunc),
		lanes: make(map[*lane]struct{}),
	}
}

// ── registry ────────────────────────────────────────────────────────────────

// Register adds ops under one source: "magmux" for the built-ins, a plugin's
// own name otherwise. The source is stamped onto every spec, so a registrant
// cannot claim to be somebody else.
//
// It is all-or-nothing. A batch with one duplicate or one malformed spec
// registers NOTHING — a plugin that half-registered would advertise an op list
// magmux disagrees with, and the only way back would be to unregister a source
// that never fully existed.
//
// Rev advances once per successful call, not once per op: it is the version of
// the op LIST, which is what a client caches and what ops_changed announces.
func (h *Hub) Register(source string, ops ...Op) error {
	if source == "" {
		return protocol.Errf(protocol.CodeBadRequest, "ops must be registered under a source name")
	}
	if len(ops) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ops))
	for i := range ops {
		spec := ops[i].Spec
		switch {
		case spec.Name == "":
			return protocol.Errf(protocol.CodeBadRequest, "%s: an op needs a name", source)
		case ops[i].Fn == nil:
			return protocol.Errf(protocol.CodeBadRequest, "%s: op %q has no implementation", source, spec.Name)
		case !protocol.ValidClass(spec.Class):
			return protocol.Errf(protocol.CodeBadRequest,
				"%s: op %q has class %q; it must be read, control, display or input",
				source, spec.Name, spec.Class)
		case seen[spec.Name]:
			return protocol.Errf(protocol.CodeBadRequest, "%s: op %q appears twice in one registration", source, spec.Name)
		}
		seen[spec.Name] = true
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range ops {
		if prev, exists := h.ops[ops[i].Spec.Name]; exists {
			return protocol.Errf(protocol.CodeBadRequest,
				"%s: op %q is already registered by %s", source, ops[i].Spec.Name, prev.Spec.Source)
		}
	}
	for _, op := range ops {
		op.Spec.Source = source
		if len(op.Spec.Schema) == 0 {
			op.Spec.Schema = emptyObjectSchema
		}
		h.ops[op.Spec.Name] = op
	}
	h.rev++
	return nil
}

// UnregisterSource removes every op one source registered and reports how many
// went. It is how a dead plugin stops being callable: the ops go, Rev changes,
// and a call to one of them is an ordinary unknown_verb from then on.
func (h *Hub) UnregisterSource(source string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for name, op := range h.ops {
		if op.Spec.Source == source {
			delete(h.ops, name)
			n++
		}
	}
	if n > 0 {
		h.rev++
	}
	return n
}

// Ops returns every registered spec, ordered by name, with the current rev.
// Ordered because this is a wire payload: an unstable order would make two
// identical op lists compare unequal for every client that diffs them.
func (h *Hub) Ops() ([]protocol.OpSpec, int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	specs := make([]protocol.OpSpec, 0, len(h.ops))
	for _, op := range h.ops {
		specs = append(specs, op.Spec)
	}
	slices.SortFunc(specs, func(a, b protocol.OpSpec) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return specs, h.rev
}

// Rev is the version of the op list.
func (h *Hub) Rev() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rev
}

// Spec returns one op's advertisement. Callers use it to ask what an op IS
// without calling it — the socket adapter reads the class to decide whether a
// `call` makes that connection a controller.
func (h *Hub) Spec(name string) (protocol.OpSpec, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	op, ok := h.ops[name]
	return op.Spec, ok
}

// Call runs one op and returns its result.
//
// Errors are protocol's vocabulary and nothing else's: unknown_verb for a name
// that is not registered, forbidden for a read-only caller asking for anything
// that is not class read, not_ready once Quiesce has run. An error the OpFunc
// itself returns is passed through untouched, so a verb's own code and wording
// reach the caller exactly as they do on the socket today.
//
// The func is copied out under RLock and called after the lock is released:
// hub.mu is a leaf and is never held across an op.
func (h *Hub) Call(ctx context.Context, c Caller, name string, args json.RawMessage) (map[string]any, error) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, protocol.Errf(protocol.CodeNotReady, "magmux is shutting down and is not taking new requests")
	}
	op, ok := h.ops[name]
	if !ok {
		h.mu.Unlock()
		return nil, protocol.Errf(protocol.CodeUnknownVerb, "unknown op %q", name)
	}
	if c.ReadOnly && op.Spec.Class != protocol.ClassRead {
		h.mu.Unlock()
		return nil, protocol.Errf(protocol.CodeForbidden,
			"op %q is class %s and this connection is read-only", name, op.Spec.Class)
	}
	runCtx, cancel := context.WithCancel(ctx)
	id := h.nextCall
	h.nextCall++
	h.calls[id] = cancel
	// Add under mu, in the same critical section as the closing check above,
	// so no Add can ever happen after Quiesce set closing — which is what
	// makes the WaitGroup safe to Wait on from Quiesce.
	h.inflight.Add(1)
	h.mu.Unlock()

	defer func() {
		cancel()
		h.mu.Lock()
		delete(h.calls, id)
		h.mu.Unlock()
		h.inflight.Done()
	}()
	return op.Fn(runCtx, c, args)
}

// ── shutdown ────────────────────────────────────────────────────────────────

// Quiesce is the first step of teardown, and it runs BEFORE the results
// aggregate is built: it stops new work, cancels what is running, and waits up
// to grace for it to return. After it, every Call is not_ready.
//
// It is deliberately not the same thing as Finalize. Quiesce is about WORK
// (ops in flight, and the queued PTY writes in each lane); Finalize is about
// the STREAM. Building results in between is what makes the aggregate a report
// on a session that has stopped moving.
//
// An op still running when grace expires is abandoned, not killed: its reply
// is refused and its effect can still land. Only two things can get there — a
// PTY that stopped reading, and a plugin that ignores invoke_cancel.
func (h *Hub) Quiesce(grace time.Duration) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return
	}
	h.closing = true
	cancels := make([]context.CancelFunc, 0, len(h.calls))
	for _, cancel := range h.calls {
		cancels = append(cancels, cancel)
	}
	h.mu.Unlock()

	// Outside the lock: a cancel runs whatever the ctx's waiters do next, and
	// hub.mu is never held across a call-out.
	for _, cancel := range cancels {
		cancel()
	}

	// Every QUEUED lane item is discarded unrun — including in a closed Sub's
	// draining lane — and whatever is delivering is cancelled at its next stop
	// check. This is the ONLY place an item is dropped, which is why a
	// discarded send is told about it rather than vanishing.
	lanes := h.laneSet()
	for _, l := range lanes {
		l.quiesce()
	}

	done := make(chan struct{})
	go func() {
		h.inflight.Wait()
		for _, l := range lanes {
			l.running.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

// Closing reports whether Quiesce has run. Adapters use it to refuse a request
// with not_ready before they even decode it.
func (h *Hub) Closing() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.closing
}

// Finalize ends every subscription with the same three things in the same
// order: the final messages (results, then shutdown), then EOF.
//
// It is the whole of the ordering guarantee. Each Sub's backlog is DISCARDED
// — results supersedes every snapshot queued behind it, and a subscriber that
// was 900 events behind gets the answer rather than the history — the finals
// jump to the front, and the in-flight write is cut short at T0+500ms so a
// blocked peer cannot hold the queue. Everything written after that is bounded
// by one absolute deadline, T0+2s: Finalize returns by then whatever any peer
// does.
//
// A subscriber whose in-flight write was TORN (some bytes out, then an error)
// is closed without finals. A final cannot follow a partial line: it would be
// spliced into it, and a client parsing lines would see one corrupt message
// instead of a clean end.
func (h *Hub) Finalize(finals ...[]byte) {
	t0 := time.Now()
	cut := t0.Add(finalCut)
	deadline := t0.Add(finalDeadline)

	h.mu.Lock()
	if h.finalized {
		h.mu.Unlock()
		return
	}
	h.finalized = true
	h.finals = finals
	subs := make([]*Sub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	// Sink calls happen with no hub lock held.
	for _, s := range subs {
		s.finalize(finals, cut, deadline)
	}
	for _, s := range subs {
		select {
		case <-s.Done():
		case <-time.After(time.Until(deadline)):
		}
	}
	// Whatever is still open at D goes now, so Finalize's own return is
	// bounded: the caller is magmux's teardown, which has its own bound.
	for _, s := range subs {
		s.kill("shutdown deadline")
	}
}

// Finalized reports whether Finalize has run.
func (h *Hub) Finalized() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.finalized
}

// ── bus ─────────────────────────────────────────────────────────────────────

// Publish hands one already-marshalled message to every live subscriber. It
// never blocks: each Sub has its own bounded queue and its own writer, and a
// subscriber that has stopped reading overflows its queue and is closed with
// slow_consumer while everyone else is unaffected.
//
// The bytes are the WHOLE message, framing included — the socket adapter
// publishes the line it already built, newline and all. hub adds no framing of
// its own, because the framing is the transport's and the hub does not know
// which transport a given Sub is.
//
// The same slice goes to every Sub, so a caller must not mutate it afterwards.
func (h *Hub) Publish(msg []byte) {
	if len(msg) == 0 {
		return
	}
	h.mu.RLock()
	var overflowed []*Sub
	for s := range h.subs {
		if !s.enqueue(msg) {
			overflowed = append(overflowed, s)
		}
	}
	h.mu.RUnlock()

	// Closing a sink is a call-out, so it happens after the lock is released.
	for _, s := range overflowed {
		s.kill("slow_consumer")
	}
}

// Session opens a connection-scoped port onto the hub: the Sub.
//
// It is step 1 of the subscribe cut. The Sub is registered here, BUFFERING —
// from this moment every Publish is queued for it — and the adapter then
// builds its connect-time aggregate with no hub lock held and hands it to
// Sub.Start. Nothing published in between is lost, and nothing is written
// before the aggregate.
//
// The caller must have waited for whatever readiness its transport requires
// before getting here: a Sub that is registered but not started still fills,
// and its cap would close it.
//
// pluginOf resolves the connection's plugin name at each call and may be nil
// for any transport that cannot carry a plugin.
//
// If Finalize has already run, the Sub subscribes to nothing: Start writes the
// aggregate, replays the finals and closes. A late client still gets
// results -> shutdown -> EOF.
func (h *Hub) Session(c Caller, sink Sink, pluginOf func() string) *Sub {
	s := newSub(h, c, sink, pluginOf)
	h.mu.Lock()
	if h.finalized {
		// Not registered: nothing will ever be published to it.
		s.finals = append([][]byte(nil), h.finals...)
		s.finalized = true
		s.deadline = time.Now().Add(lateDeadline)
		h.mu.Unlock()
		return s
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// drop removes a Sub from the bus. Idempotent, and safe to call from the Sub's
// own writer goroutine: it takes only hub.mu.
func (h *Hub) drop(s *Sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Subs is the number of live subscribers.
func (h *Hub) Subs() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
