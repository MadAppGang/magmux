package hub

// Watching: frame slots, and the port mux's Streamer plugs into.
//
// A frame is not an event, and the difference is the whole design. An event is
// NEWS — it happened, and a subscriber that missed it is wrong about the
// session — so events queue, and a subscriber whose queue overflows is closed.
// A frame is STATE: the newest one supersedes every frame behind it for the
// same pane, and delivering an older screen after a newer one is worse than
// delivering neither. So frames never queue. Each (Sub, pane) has ONE slot, a
// later offer merges into whatever is still sitting in it, and a subscriber
// that reads slowly simply sees fewer frames of the same screen.
//
// Two producer races are closed here by STATE rather than by the writer's
// priority order, because priority can only order work the writer can already
// see:
//
//   - No frame after pane_closed. Each Sub keeps a closed-pane set.
//     Hub.PaneClosed adds the pane and clears its slot under sub.mu, and every
//     later Offer for it is dropped under the same lock. mux calls it BEFORE it
//     publishes pane_closed, so a framer still mid-tick offers into nothing.
//     Pane ids are never reused (the id table is append-only), so the set never
//     needs pruning.
//   - No frame before the watch reply that created it. A slot is created
//     INACTIVE: offers land in it and the writer skips it. Sub.Call enqueues the
//     watch reply and activates the slot in ONE sub.mu acquisition, so there is
//     no instant at which a frame could overtake the answer that announced it.

import (
	"sort"

	"github.com/MadAppGang/magmux/protocol"
)

// Watcher is the driven side of watching: mux's Streamer implements it, and the
// hub calls it with NO hub lock held.
//
// It is a port and not a callback set because all five methods are one
// subject — which Subs want which panes — and splitting them would let a
// transport wire up three of them.
type Watcher interface {
	// Watch starts (or joins) a pane's framer for this Sub. The returned
	// WatchInfo is the pane's geometry and the mode and rate actually settled
	// on, which may be clamped from what was asked for.
	Watch(s *Sub, pane int, mode protocol.WatchMode, fps int) (protocol.WatchInfo, error)
	// Unwatch stops this Sub watching one pane. It is idempotent: a client that
	// unwatches twice, or unwatches a pane that has closed underneath it, is
	// not making an error worth an answer.
	Unwatch(s *Sub, pane int)
	// Resync asks for a keyframe for this Sub alone. It is how a client that
	// lost track of its screen — a canvas resize, a reconnect on the same Sub —
	// gets back to a known state without dropping the subscription.
	Resync(s *Sub, pane int) error
	// WatchAll subscribes this Sub to every pane, including panes opened later.
	// It is what a transport that streams the whole session (SSE with ?watch,
	// the Firebase mirror) asks for once instead of tracking pane lifecycle
	// itself.
	WatchAll(s *Sub, fps int)
	// Drop forgets this Sub entirely: every pane, and any WatchAll. It is
	// called when the Sub's connection ends.
	Drop(s *Sub)
}

// SetWatcher installs the implementation. It is called once at startup, before
// any connection exists; a hub with no watcher answers every watch with
// `unsupported`, which is what a transport-only build would honestly report.
func (h *Hub) SetWatcher(w Watcher) {
	h.mu.Lock()
	h.watcher = w
	h.mu.Unlock()
}

// watcherRef reads the installed Watcher. hub.mu is a leaf and is never held
// across a Watcher call, so every caller copies the pointer out first.
func (h *Hub) watcherRef() Watcher {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.watcher
}

// PaneClosed tells every subscriber that a pane is gone, BEFORE the pane_closed
// event is published. From this moment an Offer for that pane is dropped, so no
// frame can arrive after the news that the pane went away.
func (h *Hub) PaneClosed(pane int) {
	h.mu.RLock()
	subs := make([]*Sub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.RUnlock()
	for _, s := range subs {
		s.paneClosed(pane)
	}
}

// ── the slot ────────────────────────────────────────────────────────────────

// slot is one pane's pending frame for one subscriber.
//
// rows is keyed by row index, so merging two offers is a map write per changed
// row and the result is always "the newest version of every row either offer
// carried". key is STICKY: once a keyframe is in the slot, whatever merges into
// it is still a keyframe, because the merged set still covers every row.
type slot struct {
	active bool
	full   bool
	key    bool
	hdr    []byte
	rows   map[int][]byte
	// raw is the whole-message form, used by notify mode: there is nothing to
	// merge in a "this pane changed" line, so the newest one simply replaces
	// the one waiting.
	raw []byte
}

func (sl *slot) pending() bool {
	return sl.active && sl.full
}

// assemble renders the slot to one wire message.
//
// The two fields the hub owns are exactly the two that cannot be decided when a
// frame is built: `key`, because a keyframe and the deltas merged behind it are
// one keyframe, and `lines`, because the merge IS the line set. Everything else
// is in hdr, which the encoder wrote and which is a JSON object left open on
// purpose.
func (sl *slot) assemble() []byte {
	if sl.raw != nil {
		return sl.raw
	}
	ys := make([]int, 0, len(sl.rows))
	for y := range sl.rows {
		ys = append(ys, y)
	}
	sort.Ints(ys)

	n := len(sl.hdr) + 32
	for _, y := range ys {
		n += len(sl.rows[y]) + 1
	}
	out := make([]byte, 0, n)
	out = append(out, sl.hdr...)
	if sl.key {
		out = append(out, `,"key":true,"lines":[`...)
	} else {
		out = append(out, `,"key":false,"lines":[`...)
	}
	for i, y := range ys {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, sl.rows[y]...)
	}
	return append(out, ']', '}', '\n')
}

// Offer hands one pane's newest state to this subscriber. It NEVER blocks and
// never fails: the slot is the back-pressure.
//
// hdr is the frame's header as a JSON object that has been opened and not
// closed (no trailing comma); the hub appends `key` and `lines` and closes it.
// rows maps row index to that row's encoded JSON object. A nil rows means hdr
// is a whole message on its own, which is how notify mode's `changed` line
// travels through the same slot as a frame.
//
// key true says rows covers the entire screen, so whatever was pending is
// superseded outright rather than merged into.
func (s *Sub) Offer(pane int, key bool, rows map[int][]byte, hdr []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead || s.finalized || s.closed || s.closedPanes[pane] {
		return
	}
	sl := s.slots[pane]
	if sl == nil {
		// Not watching this pane (or the watch was already torn down). An offer
		// into nothing is the normal outcome of a framer that is one tick behind
		// an unwatch, and is not an error.
		return
	}
	if rows == nil {
		sl.raw = hdr
		sl.full = true
		s.signalLocked()
		return
	}
	sl.raw = nil
	if key {
		// A keyframe describes the whole screen, so every row pending behind it
		// is already included in it.
		sl.rows = nil
		sl.key = true
	}
	if sl.rows == nil {
		sl.rows = make(map[int][]byte, len(rows))
	}
	for y, line := range rows {
		sl.rows[y] = line
	}
	sl.hdr = hdr
	sl.full = true
	s.signalLocked()
}

// signalLocked is signal for a caller that already holds sub.mu. The wake
// channel has room for one, so sending under the lock cannot block.
func (s *Sub) signalLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// addSlotLocked creates a pane's slot, INACTIVE. Returns false if the pane has
// already closed for this Sub, which is the one case where a watch must fail
// rather than open a subscription to nothing.
func (s *Sub) addSlot(pane int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead || s.finalized || s.closed || s.closedPanes[pane] {
		return false
	}
	if s.slots == nil {
		s.slots = make(map[int]*slot)
	}
	if s.slots[pane] == nil {
		s.slots[pane] = &slot{}
		s.slotOrder = append(s.slotOrder, pane)
	}
	return true
}

// activateSlot lets the writer see a pane's slot. Used by an adapter that
// watches with no reply to order against.
func (s *Sub) activateSlot(pane int) {
	s.mu.Lock()
	if sl := s.slots[pane]; sl != nil {
		sl.active = true
	}
	s.mu.Unlock()
	s.signal()
}

// replyAndActivate is the ONE path that answers a `watch`: the reply is queued
// and the slot goes active in a single sub.mu acquisition, so a frame offered
// the instant the framer starts cannot be written before the answer that
// announced the subscription.
func (s *Sub) replyAndActivate(pane int, msg []byte) {
	s.mu.Lock()
	if sl := s.slots[pane]; sl != nil {
		sl.active = true
	}
	over := false
	if len(msg) > 0 && !s.closed && !s.finalized && !s.dead {
		if len(s.fifo)+1 > fifoMaxMsgs || s.bytes+len(msg) > fifoMaxBytes {
			over = true
		} else {
			s.fifo = append(s.fifo, msg)
			s.bytes += len(msg)
		}
	}
	s.mu.Unlock()
	if over {
		s.kill("slow_consumer")
		return
	}
	s.signal()
}

// removeSlot drops a pane's slot, discarding whatever was pending in it. It is
// unwatch, and it is also how a failed watch leaves nothing behind.
func (s *Sub) removeSlot(pane int) {
	s.mu.Lock()
	s.dropSlotLocked(pane)
	s.mu.Unlock()
}

func (s *Sub) dropSlotLocked(pane int) {
	if s.slots[pane] == nil {
		return
	}
	delete(s.slots, pane)
	for i, p := range s.slotOrder {
		if p == pane {
			s.slotOrder = append(s.slotOrder[:i], s.slotOrder[i+1:]...)
			break
		}
	}
}

// paneClosed is Hub.PaneClosed's per-Sub half: remember the pane, drop its
// slot, and refuse every later offer for it.
func (s *Sub) paneClosed(pane int) {
	s.mu.Lock()
	if s.closedPanes == nil {
		s.closedPanes = make(map[int]bool)
	}
	s.closedPanes[pane] = true
	s.dropSlotLocked(pane)
	s.mu.Unlock()
}

// takeSlotLocked hands the writer one pending frame, round-robin across panes.
//
// Round-robin rather than lowest-id-first: a pane painting at 30 fps would
// otherwise starve every pane with a higher id for as long as it kept painting.
// The rotation is the slot ORDER, so each pane's turn comes once per pass.
//
// Caller holds sub.mu.
func (s *Sub) takeSlotLocked() []byte {
	for i := 0; i < len(s.slotOrder); i++ {
		s.slotNext++
		if s.slotNext >= len(s.slotOrder) {
			s.slotNext = 0
		}
		sl := s.slots[s.slotOrder[s.slotNext]]
		if sl == nil || !sl.pending() {
			continue
		}
		msg := sl.assemble()
		sl.full, sl.key, sl.rows, sl.raw, sl.hdr = false, false, nil, nil, nil
		return msg
	}
	return nil
}

// hasSlotLocked reports whether any pane has an active, filled slot.
func (s *Sub) hasSlotLocked() bool {
	for _, sl := range s.slots {
		if sl.pending() {
			return true
		}
	}
	return false
}

// dropSlotsLocked clears every slot. Finalize calls it: results supersedes
// every screen behind it.
func (s *Sub) dropSlotsLocked() {
	s.slots = nil
	s.slotOrder = nil
	s.slotNext = 0
}

// ── the Sub-level verbs ─────────────────────────────────────────────────────

// errNoWatcher is what a hub with no Watcher installed answers. It is
// `unsupported` and not `internal`: a build with no streaming is a magmux that
// honestly does not offer the feature.
func errNoWatcher() error {
	return protocol.Errf(protocol.CodeUnsupported, "this magmux does not support streaming")
}

// Watch subscribes this connection to a pane's screen and activates the slot
// immediately. It is for an adapter with no reply to order the first frame
// against — SSE's `?watch=`, the Firebase mirror. A `watch` REQUEST goes
// through Sub.Call instead, which activates the slot in the same breath as the
// reply.
func (s *Sub) Watch(pane int, mode protocol.WatchMode, fps int) (protocol.WatchInfo, error) {
	info, err := s.watchInactive(pane, mode, fps)
	if err != nil {
		return info, err
	}
	s.activateSlot(pane)
	return info, nil
}

// watchInactive creates the slot and starts the framer, leaving the slot
// invisible to the writer.
func (s *Sub) watchInactive(pane int, mode protocol.WatchMode, fps int) (protocol.WatchInfo, error) {
	w := s.hub.watcherRef()
	if w == nil {
		return protocol.WatchInfo{}, errNoWatcher()
	}
	mode = protocol.ResolveWatchMode(mode)
	if !protocol.ValidWatchMode(mode) {
		return protocol.WatchInfo{}, protocol.Errf(protocol.CodeBadRequest,
			"watch mode must be %q or %q (got %q)", protocol.WatchFrames, protocol.WatchNotify, mode)
	}
	// The slot exists BEFORE the framer does, so a keyframe offered by the very
	// first tick has somewhere to land.
	if !s.addSlot(pane) {
		return protocol.WatchInfo{}, protocol.Errf(protocol.CodeNoSuchPane,
			"no pane %d (it may have been closed)", pane)
	}
	info, err := w.Watch(s, pane, mode, fps)
	if err != nil {
		s.removeSlot(pane)
		return info, err
	}
	return info, nil
}

// Unwatch ends one pane's subscription and throws away whatever frame was
// waiting: a client that stopped watching does not want one more screen.
func (s *Sub) Unwatch(pane int) {
	if w := s.hub.watcherRef(); w != nil {
		w.Unwatch(s, pane)
	}
	s.removeSlot(pane)
}

// Resync asks for a keyframe on a pane this connection already watches.
func (s *Sub) Resync(pane int) error {
	w := s.hub.watcherRef()
	if w == nil {
		return errNoWatcher()
	}
	return w.Resync(s, pane)
}

// WatchAll subscribes to every pane, now and later.
func (s *Sub) WatchAll(fps int) error {
	w := s.hub.watcherRef()
	if w == nil {
		return errNoWatcher()
	}
	w.WatchAll(s, fps)
	return nil
}

// dropWatches tells the Watcher this connection is finished. Called from Close
// and kill with no sub lock held, because Drop reaches into the streamer.
func (s *Sub) dropWatches() {
	s.watchOnce.Do(func() {
		if w := s.hub.watcherRef(); w != nil {
			w.Drop(s)
		}
	})
}
