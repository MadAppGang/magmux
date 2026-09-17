package hub

// Lanes: per-(Sub, pane) ordered delivery.
//
// A `send` is not one write. It is text, then a key every 20 ms, then a pause
// and an Enter — hundreds of milliseconds of pacing during which the transport
// reader must stay free to read the next line. Today that is a bare `go func()`
// per send, which means two sends to the same pane race each other and the
// second instruction can be typed into the middle of the first.
//
// A lane is the fix and nothing more: a FIFO per (Sub, pane), drained by ONE
// goroutine, so writes from one connection to one pane happen in submission
// order and a PTY that stopped reading stalls only its own lane.
//
// Three properties are load-bearing:
//
//   - A lane OUTLIVES its connection. A one-shot client (README's `nc -U`)
//     reaches EOF microseconds after its last line, and at that moment its
//     second send is always still queued behind the first one's pacing. On Sub
//     close the lane accepts nothing new and drains what it already holds.
//   - Quiesce is the ONLY thing that discards a queued item unrun, and it tells
//     the item so (Discard) rather than dropping it silently, because the panel
//     has already shown that request as an OUT row.
//   - The item's ctx is cancelled by Quiesce alone. A caller's own timeout ends
//     that caller's WAIT; it never reorders or truncates a lane.

import (
	"context"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

const (
	// laneMaxItems and laneMaxBytes bound one (Sub, pane) queue. Past either,
	// an item is refused with `busy` — that item alone, because a lane that is
	// 256 instructions deep is a controller that has stopped reading its own
	// answers, and refusing the new one is the only honest thing left.
	laneMaxItems = 256
	laneMaxBytes = 1 << 20

	// laneIdle is how long a drained lane waits before its goroutine exits. A
	// session that is driven once a minute must not hold a goroutine per pane
	// for the life of the run, and the next item simply starts a new one.
	laneIdle = 5 * time.Second
)

// LaneItem is one piece of work on a (Sub, pane) lane.
//
// Run does the work, synchronously on the lane's own goroutine: the item is
// complete only when Run returns, which is what makes the lane ordered rather
// than merely serialised at submission. ctx is cancelled by Quiesce and by
// nothing else.
//
// Discard is called INSTEAD of Run when Quiesce drops the item before it ran,
// so the caller can answer for a request that will now never happen. Size is
// the item's weight against the lane's byte cap.
type LaneItem struct {
	Run     func(ctx context.Context)
	Discard func()
	Size    int
}

// pushResult is what one attempt to enqueue found.
type pushResult int

const (
	pushOK      pushResult = iota
	pushFull               // the lane is at its cap: busy
	pushRefused            // the lane is closing: not_ready
	pushGone               // the drainer has exited; a fresh lane is needed
)

// lane is one (Sub, pane) FIFO and the goroutine that drains it.
type lane struct {
	sub  *Sub
	pane int

	wake   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	q     []LaneItem
	bytes int
	// quiesced means Quiesce has emptied this lane and cancelled its ctx.
	// Nothing new is accepted and the drainer stops at its next look.
	quiesced bool
	// closed means the Sub is gone. Nothing new is accepted; what is already
	// queued still runs.
	closed bool
	// retired means the drainer has exited, so this lane object is a corpse and
	// a pusher must ask the Sub for a new one.
	retired bool
	// running counts items INSIDE Run. Quiesce waits on it, bounded by its
	// grace, which is how "a send already delivering stops at its next stop
	// check" becomes something teardown can actually observe.
	running sync.WaitGroup
}

func newLane(s *Sub, pane int) *lane {
	ctx, cancel := context.WithCancel(context.Background())
	return &lane{sub: s, pane: pane, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
}

// Deliver puts one item on this Sub's lane for a pane.
//
// It never blocks: either the item is queued, or it is refused with a code the
// caller can answer with. `busy` means this connection has that pane backed up;
// `not_ready` means magmux is shutting down or this connection is closing.
func (s *Sub) Deliver(pane int, item LaneItem) error {
	if item.Run == nil {
		return protocol.Errf(protocol.CodeInternal, "a lane item with nothing to run")
	}
	// Bounded because each iteration drops a lane the drainer has already
	// retired, and a retired lane is never re-inserted.
	for i := 0; i < 8; i++ {
		l, err := s.laneFor(pane)
		if err != nil {
			return err
		}
		switch l.push(item) {
		case pushOK:
			return nil
		case pushFull:
			return protocol.Errf(protocol.CodeBusy,
				"pane %d already has %d requests queued on this connection", pane, laneMaxItems)
		case pushRefused:
			return protocol.Errf(protocol.CodeNotReady,
				"magmux is shutting down and is not taking new requests")
		case pushGone:
			s.dropLane(pane, l)
		}
	}
	return protocol.Errf(protocol.CodeBusy, "pane %d's lane could not be opened", pane)
}

// laneFor returns this Sub's lane for a pane, creating it if there is none.
//
// The hub is told about a new lane BEFORE sub.mu is taken, never under it: the
// one lock order in this package is hub.mu -> sub.mu, and a Sub that reached
// for hub.mu while holding its own would invert it.
func (s *Sub) laneFor(pane int) (*lane, error) {
	s.mu.Lock()
	gone := s.closed || s.dead || s.finalized
	l := s.lanes[pane]
	s.mu.Unlock()
	switch {
	case gone:
		return nil, protocol.Errf(protocol.CodeNotReady, "this connection is closing")
	case l != nil:
		return l, nil
	}

	fresh := newLane(s, pane)
	if !s.hub.addLane(fresh) {
		fresh.cancel()
		return nil, protocol.Errf(protocol.CodeNotReady,
			"magmux is shutting down and is not taking new requests")
	}

	s.mu.Lock()
	if s.closed || s.dead || s.finalized {
		s.mu.Unlock()
		s.hub.dropLane(fresh)
		fresh.cancel()
		return nil, protocol.Errf(protocol.CodeNotReady, "this connection is closing")
	}
	if cur := s.lanes[pane]; cur != nil {
		s.mu.Unlock()
		s.hub.dropLane(fresh)
		fresh.cancel()
		return cur, nil
	}
	if s.lanes == nil {
		s.lanes = make(map[int]*lane)
	}
	s.lanes[pane] = fresh
	s.mu.Unlock()
	go fresh.drain()
	return fresh, nil
}

// dropLane forgets a lane, if it is still the one this pane points at. A lane
// that retired while a pusher held a reference to it is dropped by that pusher;
// one that retired on its own drops itself.
func (s *Sub) dropLane(pane int, l *lane) {
	s.mu.Lock()
	if s.lanes[pane] == l {
		delete(s.lanes, pane)
	}
	s.mu.Unlock()
}

// closeLanes tells every lane this Sub owns that its connection has gone. What
// is queued still runs; nothing new is accepted.
func (s *Sub) closeLanes() {
	s.mu.Lock()
	lanes := make([]*lane, 0, len(s.lanes))
	for _, l := range s.lanes {
		lanes = append(lanes, l)
	}
	s.mu.Unlock()
	for _, l := range lanes {
		l.close()
	}
}

// push enqueues one item and reports what happened.
func (l *lane) push(item LaneItem) pushResult {
	l.mu.Lock()
	switch {
	case l.retired:
		l.mu.Unlock()
		return pushGone
	case l.quiesced || l.closed:
		l.mu.Unlock()
		return pushRefused
	case len(l.q)+1 > laneMaxItems || l.bytes+item.Size > laneMaxBytes:
		l.mu.Unlock()
		return pushFull
	}
	l.q = append(l.q, item)
	l.bytes += item.Size
	l.mu.Unlock()
	l.signal()
	return pushOK
}

func (l *lane) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// next hands the drainer its next item, or tells it to wait (ok=false) or to
// stop (stop=true). The running counter is incremented HERE, under the same
// lock that Quiesce sets `quiesced` under, so no item can start running after
// Quiesce has decided what to wait for.
func (l *lane) next() (item LaneItem, ok, stop bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.quiesced:
		l.retired = true
		return LaneItem{}, false, true
	case len(l.q) > 0:
		item = l.q[0]
		l.q = l.q[1:]
		l.bytes -= item.Size
		l.running.Add(1)
		return item, true, false
	case l.closed:
		l.retired = true
		return LaneItem{}, false, true
	}
	return LaneItem{}, false, false
}

// drain is the ONE goroutine that runs this lane's items.
func (l *lane) drain() {
	defer l.retire()
	for {
		item, ok, stop := l.next()
		switch {
		case stop:
			return
		case ok:
			l.run(item)
			continue
		}
		select {
		case <-l.wake:
		case <-l.ctx.Done():
			// Quiesce: the next look stops the lane rather than waiting out the
			// idle timer.
		case <-time.After(laneIdle):
			if l.idleOut() {
				return
			}
		}
	}
}

func (l *lane) run(item LaneItem) {
	defer l.running.Done()
	item.Run(l.ctx)
}

// idleOut retires a lane that has been empty for laneIdle. It re-checks under
// the lock, because an item pushed while the timer was firing must not be left
// on a lane nobody drains.
func (l *lane) idleOut() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.q) > 0 {
		return false
	}
	l.retired = true
	return true
}

// retire takes the lane out of both sets. Called from the drainer's own defer,
// with no lane lock held while reaching for sub.mu or hub.mu.
func (l *lane) retire() {
	l.cancel()
	l.mu.Lock()
	l.retired = true
	l.mu.Unlock()
	l.sub.dropLane(l.pane, l)
	l.sub.hub.dropLane(l)
}

// close is the Sub going away: refuse new work, keep what is queued.
func (l *lane) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.signal()
}

// quiesce discards everything queued, unrun, and cancels whatever is running.
// It is the only path that drops an item, and it tells each one so — the panel
// has already shown that request as an OUT row, and a row that never gets an
// answer is worse than one answered "discarded".
func (l *lane) quiesce() {
	l.mu.Lock()
	if l.quiesced {
		l.mu.Unlock()
		return
	}
	l.quiesced = true
	q := l.q
	l.q, l.bytes = nil, 0
	l.mu.Unlock()

	// Both of these are call-outs, so neither happens under the lane lock.
	l.cancel()
	for _, item := range q {
		if item.Discard != nil {
			item.Discard()
		}
	}
	l.signal()
}

// ── the hub's view ──────────────────────────────────────────────────────────

// addLane registers a lane so Quiesce can reach it even after its Sub is
// closed, and reports whether it may exist at all: once Quiesce has run there
// are no new lanes.
func (h *Hub) addLane(l *lane) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	if h.lanes == nil {
		h.lanes = make(map[*lane]struct{})
	}
	h.lanes[l] = struct{}{}
	return true
}

// dropLane forgets a retired lane. Idempotent.
func (h *Hub) dropLane(l *lane) {
	h.mu.Lock()
	delete(h.lanes, l)
	h.mu.Unlock()
}

// laneSet snapshots the live lanes so they can be quiesced with no lock held.
func (h *Hub) laneSet() []*lane {
	h.mu.RLock()
	defer h.mu.RUnlock()
	lanes := make([]*lane, 0, len(h.lanes))
	for l := range h.lanes {
		lanes = append(lanes, l)
	}
	return lanes
}
