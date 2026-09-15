package hub

import (
	"sync"
	"time"
)

// Queue and deadline bounds. They are per SUBSCRIBER: the whole point is that
// a peer that stops reading spends its own budget and nobody else's.
const (
	// fifoMaxMsgs and fifoMaxBytes bound one subscriber's backlog. Whichever
	// is hit first closes that subscriber with slow_consumer: a stream that is
	// 1,024 events behind is not a stream anyone is still reading, and the
	// honest answer is to disconnect it so it reconnects and resyncs.
	fifoMaxMsgs  = 1024
	fifoMaxBytes = 8 << 20

	// msgDeadline bounds one ordinary message. It is per message and not per
	// connection: a peer that keeps draining is never cut off, however long
	// the session runs.
	msgDeadline = 2 * time.Second

	// finalCut is how long the message already in flight when Finalize begins
	// gets to finish. finalDeadline is the ONE absolute deadline for
	// everything written after that — the aggregate if it is still pending,
	// then the finals — measured from the moment Finalize started.
	finalCut      = 500 * time.Millisecond
	finalDeadline = 2 * time.Second

	// lateDeadline bounds a Session opened after Finalize: it gets its
	// aggregate and the finals, on its own clock, because the teardown it
	// missed is already over.
	lateDeadline = 1 * time.Second
)

// Sink is one subscriber's connection, as the hub sees it. Implementations
// wrap a net.Conn (unix socket, WebSocket), an http.ResponseWriter (SSE) or a
// write-coalescing map (Firebase).
//
// Write takes one WHOLE message. n must be > 0 whenever any byte may have left
// the process, because n is what tells a torn write from a clean refusal: with
// n > 0 the stream is left mid-line and nothing may follow it, while n == 0
// leaves it line-aligned and the finals can still be written. Only a sink over
// a plain net.Conn can honestly report n == 0; a buffered or TLS sink reports
// every failure as torn.
//
// SetWriteDeadline must also interrupt a Write that is ALREADY blocked — the
// net.Conn rule — because that is how Finalize cuts a stalled peer short.
//
// Close may be called from a goroutine other than the one inside Write, and
// must interrupt it. It is called exactly once per Sub.
type Sink interface {
	Write(b []byte) (n int, err error)
	SetWriteDeadline(t time.Time) error
	Close(reason string)
}

// Sub is one subscriber: a bounded queue, one writer goroutine, and the
// connection-scoped face of the hub for the adapter that owns it.
//
// Nothing in here is written by more than one goroutine without sub.mu, and
// sub.mu is a leaf below hub.mu.
type Sub struct {
	hub      *Hub
	caller   Caller
	pluginOf func() string
	sink     Sink

	wake      chan struct{}
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
	watchOnce sync.Once

	mu sync.Mutex
	// head is the connect-time aggregate. It is NOT in the FIFO because it
	// cannot be discarded: every subscriber's first line is the aggregate, and
	// a Finalize that lands between registration and Start must still let it
	// through first.
	head    []byte
	fifo    [][]byte
	bytes   int
	started bool
	// closed means the adapter is done with this Sub: nothing new is accepted,
	// but what is already queued is still written.
	closed bool
	// finalized means Finalize has run for this Sub. The backlog is gone, the
	// finals are queued, and no later enqueue is accepted.
	finalized bool
	finals    [][]byte
	// cut bounds the message in flight when Finalize began, and any message
	// queued before it. deadline is D, the absolute bound on everything after.
	cut      time.Time
	deadline time.Time
	// failed records that a write failed; torn records that it failed after
	// putting bytes on the wire, which is what forbids writing a final behind
	// it.
	failed bool
	torn   bool
	dead   bool
	reason string
	// lanes is this connection's ordered delivery queues, one per pane. See
	// lane.go: a lane outlives its Sub, so this map is only how a PUSHER finds
	// one — the hub keeps its own set for Quiesce.
	lanes map[int]*lane
	// slots is the frame side of this connection: one latest-wins slot per
	// watched pane, which the writer drains AFTER the FIFO. See watch.go —
	// frames are state, not news, so they merge rather than queue.
	slots     map[int]*slot
	slotOrder []int
	slotNext  int
	// closedPanes is every pane that has closed while this connection was
	// alive. An Offer for one is dropped, which is what stops a frame arriving
	// after the pane_closed that announced its end. Ids are never reused, so it
	// never needs pruning.
	closedPanes map[int]bool
	// calls counts the ops running concurrently for this connection, bounded by
	// maxInFlightCalls. It is per SUB: one client that fires sixteen slow
	// requests spends its own budget and nobody else's.
	calls int
}

func newSub(h *Hub, c Caller, sink Sink, pluginOf func() string) *Sub {
	return &Sub{
		hub:      h,
		caller:   c,
		pluginOf: pluginOf,
		sink:     sink,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// Caller is the identity of this connection AT THIS MOMENT. Plugin is resolved
// on every call rather than captured, because a connection registers as a
// plugin after its Sub already exists — and it is resolved with no hub lock
// held, so the plugin host's own lock stays above hub.mu in the order.
func (s *Sub) Caller() Caller {
	c := s.caller
	if s.pluginOf != nil {
		c.Plugin = s.pluginOf()
	}
	return c
}

// Start is step 3 of the subscribe cut: it installs the connect-time aggregate
// as this subscriber's first line and starts its writer.
//
// head may be nil for a transport that has no aggregate. Calling Start twice,
// or starting a Sub that has already been killed, does nothing.
func (s *Sub) Start(head []byte) {
	s.mu.Lock()
	if s.started || s.dead {
		s.mu.Unlock()
		return
	}
	s.started = true
	if len(head) > 0 {
		s.head = head
	}
	s.mu.Unlock()
	go s.run()
}

// Send queues one message for this subscriber alone — a reply, or anything
// else unicast. Like Publish it never blocks: overflow closes this subscriber.
func (s *Sub) Send(msg []byte) {
	if !s.enqueue(msg) {
		s.kill("slow_consumer")
	}
}

// Close says the adapter is finished with this Sub, usually because its
// connection ended. What is already queued is still written — a reply to a
// request the peer sent before it went away still goes out, and is simply lost
// at the socket — and the writer then exits.
//
// Its lanes are told too, and they behave the same way: nothing new is
// accepted, what is already queued still RUNS. That is what keeps README's
// one-shot client working, whose second send is always still queued behind the
// first one's pacing when the socket closes.
func (s *Sub) Close(reason string) {
	defer s.closeLanes()
	// The watches go with the connection: a framer running for a Sub nobody
	// reads is a goroutine diffing a screen into a slot that will never be
	// written. Outside sub.mu, because Drop reaches into the streamer.
	defer s.dropWatches()
	s.mu.Lock()
	if s.closed || s.dead {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.reason == "" {
		s.reason = reason
	}
	started := s.started
	s.mu.Unlock()
	if !started {
		// Nothing will ever drain it; close it here rather than leaving a Sub
		// whose Done never fires.
		s.kill(reason)
		return
	}
	s.signal()
}

// Done is closed once this Sub's writer has finished and its sink is closed.
func (s *Sub) Done() <-chan struct{} { return s.done }

// enqueue appends to the FIFO and reports whether it fit. A refusal is an
// OVERFLOW and nothing else: a message dropped because the Sub is closed or
// finalized returns true, because there is nobody left to punish for it.
func (s *Sub) enqueue(msg []byte) bool {
	s.mu.Lock()
	if s.closed || s.finalized || s.dead {
		s.mu.Unlock()
		return true
	}
	if len(s.fifo)+1 > fifoMaxMsgs || s.bytes+len(msg) > fifoMaxBytes {
		s.mu.Unlock()
		return false
	}
	s.fifo = append(s.fifo, msg)
	s.bytes += len(msg)
	s.mu.Unlock()
	s.signal()
	return true
}

// signal wakes the writer. The channel has room for one wake, which is all a
// writer that re-reads the whole queue needs.
func (s *Sub) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// finalize is Finalize's per-Sub half, run with no hub lock held.
func (s *Sub) finalize(finals [][]byte, cut, deadline time.Time) {
	s.mu.Lock()
	if s.dead || s.finalized {
		s.mu.Unlock()
		return
	}
	s.finalized = true
	// The backlog goes. results supersedes every snapshot behind it, and a
	// queued reply's caller sees EOF, which every client already reads as
	// "the session ended".
	s.fifo = nil
	s.bytes = 0
	// The slots go with the backlog, and for the same reason: `results` is the
	// authoritative final state of every pane, and a screen written after it
	// would be a picture of a session that has already been reported on.
	s.dropSlotsLocked()
	s.finals = finals
	s.cut = cut
	s.deadline = deadline
	// Under sub.mu, so it cannot be reordered against the writer's own
	// per-message deadline: this call CUTS a write that is already blocked,
	// and a pre-final message must never widen it back.
	_ = s.sink.SetWriteDeadline(cut)
	s.mu.Unlock()
	// A Sub that is registered but not yet started is deliberately NOT started
	// here. Its adapter is between step 1 and step 3 of the subscribe cut,
	// building the aggregate; starting it now would write the finals and make
	// the aggregate that arrives a moment later unsendable. Start picks the
	// finals up, so the first line is still the aggregate, and Finalize's own
	// deadline bounds the wait.
	s.signal()
}

// kill closes this subscriber now, without finals. Any goroutine may call it —
// Publish on overflow, Finalize at its deadline, the writer on a torn write —
// and it is idempotent.
func (s *Sub) kill(reason string) {
	// The STREAM is over; the lanes are not. A send already accepted is still
	// delivered and still acked on the panel, exactly as it is for a peer that
	// merely hung up — losing it because the peer stopped READING would make
	// delivery depend on something it has nothing to do with.
	defer s.closeLanes()
	defer s.dropWatches()
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.dead = true
	if s.reason == "" {
		s.reason = reason
	}
	reason = s.reason
	started := s.started
	s.mu.Unlock()

	s.hub.drop(s)
	// Closing the sink is what interrupts a writer blocked inside Write.
	s.closeOnce.Do(func() { s.sink.Close(reason) })
	if !started {
		s.doneOnce.Do(func() { close(s.done) })
		return
	}
	s.signal()
}

// take pulls the next message to write, in priority order: the aggregate
// first, then the finals, then the backlog. Priority only orders work the
// writer can already see — the races that matter are closed by state, not by
// order.
//
// It returns stop=true when there is nothing more this subscriber will ever
// receive, and sets the sink's deadline for the message it hands back.
func (s *Sub) take() (msg []byte, stop bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.dead:
		return nil, true, s.reason
	case s.torn:
		// A final cannot follow a partial line, so this connection ends here
		// whatever is queued behind it.
		return nil, true, "torn write"
	case s.failed && !s.finalized:
		// An ordinary write failure on a live connection: the peer is gone or
		// is not draining. Today's socket drops such a client too.
		return nil, true, s.reason
	case s.head != nil:
		msg, s.head = s.head, nil
	case s.finalized && len(s.finals) > 0:
		msg, s.finals = s.finals[0], s.finals[1:]
		if len(msg) == 0 {
			// An empty final is nothing to write and must not be mistaken for
			// "nothing to do", which would park the writer on its wake channel.
			return nil, true, "shutdown"
		}
	case s.finalized:
		return nil, true, "shutdown"
	case len(s.fifo) > 0:
		msg, s.fifo = s.fifo[0], s.fifo[1:]
		s.bytes -= len(msg)
	case s.hasSlotLocked():
		// Frames go LAST, behind every event and every reply. A frame is the
		// current screen and stays current while it waits; an event behind a
		// frame would be news delayed by a picture.
		msg = s.takeSlotLocked()
	case s.closed:
		return nil, true, s.reason
	default:
		return nil, false, ""
	}
	_ = s.sink.SetWriteDeadline(s.writeDeadlineLocked())
	return msg, false, ""
}

// writeDeadlineLocked is the bound for the message about to be written. Before
// Finalize it is a per-message budget, clamped by the cut if one is already
// set; after it, it is D, the one absolute deadline the whole teardown is
// measured against.
func (s *Sub) writeDeadlineLocked() time.Time {
	if s.finalized {
		return s.deadline
	}
	d := time.Now().Add(msgDeadline)
	if !s.cut.IsZero() && s.cut.Before(d) {
		return s.cut
	}
	return d
}

// run is the ONE caller of Sink.Write.
func (s *Sub) run() {
	defer func() {
		s.hub.drop(s)
		s.doneOnce.Do(func() { close(s.done) })
	}()
	for {
		msg, stop, reason := s.take()
		if stop {
			s.closeOnce.Do(func() { s.sink.Close(reason) })
			return
		}
		if msg == nil {
			<-s.wake
			continue
		}
		n, err := s.sink.Write(msg)
		if err != nil {
			s.mu.Lock()
			s.failed = true
			// n > 0 means part of this message is on the wire. Per the Sink
			// contract a buffered or TLS sink reports every failure that way,
			// so this branch is the common one everywhere but a plain socket.
			s.torn = s.torn || n > 0
			if s.reason == "" {
				s.reason = "write failed: " + err.Error()
			}
			s.mu.Unlock()
		}
	}
}
