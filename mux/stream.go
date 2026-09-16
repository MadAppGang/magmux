package mux

// Live screen streaming.
//
// The cost model is the design. magmux's read loop is the hottest path it has —
// every byte a child prints goes through it — and until something is watching a
// pane, streaming must cost nothing measurable there. So:
//
//   - The hook in readLoop is one atomic increment and one atomic nil load
//     (Pane.noteOutputLocked). No allocation, no lock, no branch that touches
//     the screen. TestReadLoopHookIsAllocationFree pins it at zero allocations.
//   - A pane with no watcher has NO framer goroutine at all. The first watcher
//     starts one; the last one to leave, or the pane closing, stops it.
//   - The framer is WAKE-DRIVEN, not polled. It sleeps on a channel; a wake
//     costs a non-blocking send into a cap-1 channel, so a thousand writes
//     between two frames coalesce into one wake. A watched pane that is idle
//     produces no frames and does no work: the tick sees frameGen unchanged and
//     returns before it has allocated anything.
//
// Everything the framer reads from the pane it reads under p.mu and NOTHING
// else — never treeMu. It compares the live screen against its own shadow copy,
// copies the rows that differ into the shadow, and releases the lock. Encoding
// happens afterwards, unlocked, from the shadow: a JSON encode of 24 rows must
// never sit between the read loop and its next byte.
//
// Lock order: streamMu -> fr.mu -> sub.mu, and nothing here takes treeMu while
// holding any of them (pane lookups resolve and release first).

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// paneStream is the read loop's handle on a pane's framer: a way to say
// "something happened" that cannot block and cannot allocate.
//
// It is a separate object from the framer so the hook is a single atomic load
// of a pointer that is usually nil, and so a framer that is stopping cannot be
// woken by a read loop that has not noticed yet.
type paneStream struct {
	// wakeCh has room for exactly one wake, which is all a framer that re-reads
	// the whole screen needs. A second wake before the first is consumed is not
	// information.
	wakeCh chan struct{}
	// geom says the pane changed SHAPE since the last frame, so the next one
	// must be a keyframe: a client cannot apply row deltas across a resize.
	geom atomic.Bool
}

func (ps *paneStream) wake() {
	select {
	case ps.wakeCh <- struct{}{}:
	default:
	}
}

// noteOutputLocked is the read loop's streaming hook.
//
// Caller HOLDS p.mu — this runs inside the lock readLoop already takes, so it
// adds no lock of its own. Unwatched, it is one atomic add and one atomic load
// that finds nil.
func (p *Pane) noteOutputLocked() {
	p.frameGen.Add(1)
	if s := p.stream.Load(); s != nil {
		s.wake()
	}
}

// noteGeometryLocked is the same hook for a resize, which additionally forces
// the next frame to be a keyframe.
//
// Caller HOLDS p.mu.
func (p *Pane) noteGeometryLocked() {
	p.frameGen.Add(1)
	if s := p.stream.Load(); s != nil {
		s.geom.Store(true)
		s.wake()
	}
}

// size reports the pane's screen geometry. Caller must NOT hold p.mu.
func (p *Pane) size() (rows, cols int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.screen == nil {
		return 0, 0
	}
	return p.screen.rows, p.screen.cols
}

// ── the Streamer ────────────────────────────────────────────────────────────

// Streamer is magmux's implementation of hub.Watcher: it owns one framer per
// WATCHED pane and the set of subscribers that asked for everything.
type Streamer struct {
	m *Magmux

	// mu is streamMu. It guards the framer table and the watch-all set, and is
	// never held across a join, an Offer or anything that takes treeMu or p.mu.
	mu      sync.Mutex
	framers map[int]*framer
	// all is every Sub that asked to watch every pane, with its rate. A pane
	// opened later is attached to each of them, which is what lets a transport
	// stream a whole session without tracking pane lifecycle itself.
	all map[*hub.Sub]int
	// closed stops new framers once magmux is tearing down: a framer started
	// during teardown would offer frames into slots Finalize has already thrown
	// away.
	closed bool
}

func newStreamer(m *Magmux) *Streamer {
	return &Streamer{m: m, framers: map[int]*framer{}, all: map[*hub.Sub]int{}}
}

// streamer returns this magmux's Streamer, building it on first use.
//
// Lazy for the same reason the hub is: every unit test in this package builds a
// Magmux as a struct literal and never calls init(), and a pane can be opened
// and closed in one of those.
//
// It deliberately does NOT reach for m.bus(). Registration runs the other way —
// bus() installs the Streamer as the hub's Watcher inside its own Once — because
// a Once that calls a Once that calls it back is a deadlock, and because the
// watcher has to exist from the moment the hub does: a `watch` arriving before
// any pane has been opened dynamically would otherwise be answered
// `unsupported` by a magmux that streams perfectly well.
func (m *Magmux) streamer() *Streamer {
	m.streamOnce.Do(func() {
		if m.stream == nil {
			m.stream = newStreamer(m)
		}
	})
	return m.stream
}

// Watch starts or joins a pane's framer for one subscriber.
func (st *Streamer) Watch(s *hub.Sub, pane int, mode protocol.WatchMode, fps int) (protocol.WatchInfo, error) {
	fps = protocol.ClampFPS(fps)
	// Resolved through the identity table and RELEASED before streamMu: the one
	// rule is that no new lock is ever held while reaching for treeMu.
	p := st.m.paneByID(pane)
	if p == nil {
		return protocol.WatchInfo{}, sockErrf(sockCodeNoSuchPane, "no pane %d (it may have been closed)", pane)
	}
	rows, cols := p.size()

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return protocol.WatchInfo{}, sockErrf(sockCodeNotReady, "magmux is shutting down and is not taking new watchers")
	}
	fr := st.framers[pane]
	if fr == nil {
		fr = newFramer(st, p, pane)
		st.framers[pane] = fr
		// The read loop learns about the framer here, and only here. A pane
		// whose stream pointer is nil costs one atomic load per read.
		p.stream.Store(fr.stream)
		go fr.run()
	}
	st.mu.Unlock()

	fr.add(s, mode, fps)
	return protocol.WatchInfo{Pane: pane, Rows: rows, Cols: cols, Mode: mode, FPS: fps}, nil
}

// Unwatch drops one subscriber from one pane, and stops the framer if that was
// the last one. Idempotent: unwatching twice, or unwatching a pane that closed
// underneath you, is not an error worth an answer.
func (st *Streamer) Unwatch(s *hub.Sub, pane int) {
	st.mu.Lock()
	fr := st.framers[pane]
	empty := false
	if fr != nil {
		empty = fr.remove(s)
		if empty {
			delete(st.framers, pane)
			fr.p.stream.Store(nil)
		}
	}
	st.mu.Unlock()
	if empty {
		// Joined outside streamMu: the framer takes p.mu and sub.mu on its way
		// round, and holding the table lock across that would put streamMu above
		// both.
		fr.shutdown()
	}
}

// Resync asks for a keyframe for one subscriber. The pane must already be
// watched — resync is "I lost my copy of the screen", not "start watching".
func (st *Streamer) Resync(s *hub.Sub, pane int) error {
	st.mu.Lock()
	fr := st.framers[pane]
	st.mu.Unlock()
	if fr == nil || !fr.resync(s) {
		return sockErrf(sockCodeBadRequest, "pane %d is not being watched on this connection", pane)
	}
	return nil
}

// WatchAll subscribes one connection to every live pane, and to every pane
// opened afterwards.
func (st *Streamer) WatchAll(s *hub.Sub, fps int) {
	fps = protocol.ClampFPS(fps)
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.all[s] = fps
	st.mu.Unlock()
	for _, p := range st.m.livePanes() {
		// Through the Sub, so the slot is created and activated exactly as a
		// watch request's would be. There is no reply to order it against here.
		_, _ = s.Watch(p.id, protocol.WatchFrames, fps)
	}
}

// Drop forgets a connection entirely. Called when its Sub closes.
func (st *Streamer) Drop(s *hub.Sub) {
	st.mu.Lock()
	delete(st.all, s)
	var emptied []*framer
	for pane, fr := range st.framers {
		if fr.remove(s) {
			delete(st.framers, pane)
			fr.p.stream.Store(nil)
			emptied = append(emptied, fr)
		}
	}
	st.mu.Unlock()
	for _, fr := range emptied {
		fr.shutdown()
	}
}

// paneOpened attaches every watch-all subscriber to a new pane.
//
// It runs AFTER pane_opened has been published, so the event is already in each
// subscriber's queue before the first keyframe can be offered into its slot:
// a client learns a pane exists before it is shown one.
func (st *Streamer) paneOpened(pane int) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	subs := make([]*hub.Sub, 0, len(st.all))
	rates := make([]int, 0, len(st.all))
	for s, fps := range st.all {
		subs = append(subs, s)
		rates = append(rates, fps)
	}
	st.mu.Unlock()
	for i, s := range subs {
		_, _ = s.Watch(pane, protocol.WatchFrames, rates[i])
	}
}

// paneClosed stops a pane's framer and waits for it to be gone.
//
// The JOIN is the point, and it is stronger than the closed-pane set alone
// needs: by the time this returns, no goroutine exists that could offer a frame
// for this pane, so the pane_closed published next cannot be followed by a
// picture of the pane it announced the end of.
func (st *Streamer) paneClosed(pane int) {
	st.mu.Lock()
	fr := st.framers[pane]
	delete(st.framers, pane)
	st.mu.Unlock()
	if fr != nil {
		fr.p.stream.Store(nil)
		fr.shutdown()
	}
}

// Shutdown stops every framer. Teardown calls it before the finals go out.
func (st *Streamer) Shutdown() {
	st.mu.Lock()
	st.closed = true
	frs := make([]*framer, 0, len(st.framers))
	for pane, fr := range st.framers {
		frs = append(frs, fr)
		delete(st.framers, pane)
	}
	st.all = map[*hub.Sub]int{}
	st.mu.Unlock()
	for _, fr := range frs {
		fr.p.stream.Store(nil)
		fr.shutdown()
	}
}

// framerCount is how many panes have a running framer. Tests use it to prove
// the N2 property: no goroutine without a watcher.
func (st *Streamer) framerCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.framers)
}

// ── the framer ──────────────────────────────────────────────────────────────

// paneWatcher is one subscriber's view of one pane.
//
// dirty is the load-bearing field. The shadow is SHARED by every watcher, so a
// row diffed once is never diffed again; a watcher whose frame rate made it skip
// this tick would lose that row forever if the framer did not remember, per
// watcher, which rows it still owes. Rows are encoded from the shadow when they
// are actually offered, so what a slow watcher receives is the row as it is NOW,
// not as it was when it changed.
// pending is the same idea for the OTHER mode. A notify watcher has no rows, so
// "this one still owes a message" cannot be read off dirty; and the thing it
// owes outlives the tick that discovered it, because the pane may fall silent
// before its rate limit expires. See framer.deferred.
type paneWatcher struct {
	sub     *hub.Sub
	mode    protocol.WatchMode
	fps     int
	key     bool
	pending bool
	dirty   map[int]bool
	lastAt  time.Time
}

// notifyMinInterval is how often a notify-mode watcher may be told its pane
// moved. A `changed` costs the client a whole read of the pane, so 30 of them a
// second is not a notification — it is a poll with extra steps.
const notifyMinInterval = time.Second

// tooSoon is one watcher's rate limit, and the ONE place the two modes' limits
// are written down. They used to be stated in two places — the fps check in the
// tick loop and a second, longer check inside offerChanged — and the second one
// silently swallowed work the first had already let through.
func (w *paneWatcher) tooSoon(now time.Time) bool {
	if w.lastAt.IsZero() {
		return false
	}
	limit := time.Second / time.Duration(w.fps)
	if w.mode == protocol.WatchNotify {
		limit = notifyMinInterval
	}
	return now.Sub(w.lastAt) < limit
}

// framer is one pane's frame producer: one goroutine, one shadow copy of the
// screen, and the set of subscribers watching it.
type framer struct {
	st     *Streamer
	p      *Pane
	pane   int
	stream *paneStream
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	watchers map[*hub.Sub]*paneWatcher
	// pendingKey is set when any watcher needs a keyframe — it joined, or it
	// asked to resync. It forces the next frame to be a full one for EVERYONE
	// watching the pane: a keyframe is a correct frame for a watcher that only
	// needed a delta, and one shared full frame is cheaper than maintaining two
	// encodings of the same screen.
	pendingKey bool
	// deferred is set when a tick found work for some watcher and could not hand
	// it over yet, because that watcher's own rate limit had not expired.
	//
	// It exists because NOTHING ELSE REMEMBERS. tick advances lastGen as soon as
	// it reads the screen and folds the change into the shared shadow, so the
	// next wake finds `gen == lastGen` and, even past that, a diff with no rows
	// in it — and if the pane has fallen silent there will never be another
	// generation to bring the work back. The `skipped` wake at the end of tick
	// was already there for exactly this case and could not work on its own,
	// because both of tick's early returns stood in front of it.
	//
	// The symptom was worst in NOTIFY mode, whose limit is a whole second: a
	// client subscribed to a pane, the pane changed once a few hundred
	// milliseconds later and then went quiet, and the `changed` was dropped for
	// good. The client sat on a screen it believed was current with no way to
	// discover otherwise — a silent stale read, which is the one failure a
	// notification channel exists to prevent. test/rc/case4-mcp.ts caught it and
	// TestNotifyModeDoesNotDropTheChangeItRateLimited pins it.
	deferred bool

	// Below here is the run goroutine's alone.
	shadow         [][]Cell
	shRows, shCols int
	seq            uint64
	lastGen        uint64
	lastAlt        bool
	// lastHead is the last frame's header with its seq left at zero, so it can
	// be compared whole: everything a client draws that is not a row lives in
	// it, and a change to any of it is a frame.
	lastHead protocol.FrameHeader
	changed  []int
	lines    map[int][]byte
	style    rowStyle
}

func newFramer(st *Streamer, p *Pane, pane int) *framer {
	return &framer{
		st:       st,
		p:        p,
		pane:     pane,
		stream:   &paneStream{wakeCh: make(chan struct{}, 1)},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		watchers: map[*hub.Sub]*paneWatcher{},
		lines:    map[int][]byte{},
	}
}

// add registers a watcher and asks for the keyframe it needs to start from.
func (fr *framer) add(s *hub.Sub, mode protocol.WatchMode, fps int) {
	fr.mu.Lock()
	w := fr.watchers[s]
	if w == nil {
		w = &paneWatcher{sub: s, dirty: map[int]bool{}}
		fr.watchers[s] = w
	}
	w.mode, w.fps, w.key = mode, fps, true
	w.lastAt = time.Time{}
	fr.pendingKey = true
	fr.mu.Unlock()
	// Wake even though nothing was printed: a new watcher's first frame must not
	// wait for the pane to say something.
	fr.stream.wake()
}

// remove drops a watcher and reports whether the framer now has none.
func (fr *framer) remove(s *hub.Sub) bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	delete(fr.watchers, s)
	return len(fr.watchers) == 0
}

// resync marks one watcher as needing a keyframe. False means it is not
// watching this pane at all.
func (fr *framer) resync(s *hub.Sub) bool {
	fr.mu.Lock()
	w := fr.watchers[s]
	if w != nil {
		w.key = true
		fr.pendingKey = true
	}
	fr.mu.Unlock()
	if w == nil {
		return false
	}
	fr.stream.wake()
	return true
}

func (fr *framer) shutdown() {
	fr.once.Do(func() { close(fr.stop) })
	<-fr.done
}

// interval is the fastest watcher's frame interval: the framer looks at the
// screen no more often than the quickest subscriber can use, and each watcher
// is then offered at its own rate.
func (fr *framer) interval() time.Duration {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	best := protocol.FPSMin
	for _, w := range fr.watchers {
		if w.fps > best {
			best = w.fps
		}
	}
	return time.Second / time.Duration(best)
}

// run is the framer's one goroutine.
//
// Wake, then wait out the interval, THEN look. That order is what turns a burst
// of output into one frame: the wait is where a hundred writes coalesce, and
// looking first would encode a screen that is about to change again.
func (fr *framer) run() {
	defer close(fr.done)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	for {
		select {
		case <-fr.stream.wakeCh:
		case <-fr.stop:
			return
		}
		timer.Reset(fr.interval())
		select {
		case <-timer.C:
		case <-fr.stop:
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
		fr.tick()
	}
}

// tick builds at most one frame per watcher.
//
// The early return is the steady state and must stay allocation-free: a watched
// pane that is doing nothing costs one atomic load, one atomic swap and a mutex
// round-trip per wake, and nothing else. TestIdleWatchedPaneTickIsFree pins it.
func (fr *framer) tick() {
	gen := fr.p.frameGen.Load()
	geom := fr.stream.geom.Swap(false)

	fr.mu.Lock()
	n := len(fr.watchers)
	key := fr.pendingKey
	deferred := fr.deferred
	fr.mu.Unlock()
	if n == 0 || (gen == fr.lastGen && !geom && !key && !deferred) {
		return
	}
	fr.lastGen = gen

	// ── under p.mu, and nothing else ────────────────────────────────────────
	fr.changed = fr.changed[:0]
	fr.p.mu.Lock()
	s := fr.p.screen
	if s == nil {
		fr.p.mu.Unlock()
		return
	}
	rows, cols := s.rows, s.cols
	alt := fr.p.altMode
	full := geom || key || alt != fr.lastAlt || rows != fr.shRows || cols != fr.shCols
	if rows != fr.shRows || cols != fr.shCols {
		fr.resizeShadow(rows, cols)
	}
	for i := 0; i < rows; i++ {
		// viewRow(0, …) is deliberate: the stream shows the LIVE screen whatever
		// the local human has scrolled back to. Two viewers must not fight over
		// one viewport, and `scrolled` in the header is how a client learns the
		// person at the keyboard is looking at something else.
		row := s.viewRow(0, i)
		if !full && rowSame(fr.shadow[i], row) {
			continue
		}
		copyRow(fr.shadow[i], row)
		fr.changed = append(fr.changed, i)
	}
	head := protocol.FrameHeader{
		Type: protocol.EventFrame, Pane: fr.pane,
		Rows: rows, Cols: cols, Alt: alt, Sb: s.sbLen, Scrolled: s.sbOff > 0,
		Cur: protocol.Cursor{Y: s.curY, X: s.curX, Vis: !fr.p.curHidden},
	}
	fr.p.mu.Unlock()
	// ── unlocked from here ──────────────────────────────────────────────────

	fr.lastAlt = alt
	// A frame is not only its rows. Moving the cursor, hiding it (DECTCEM),
	// scrolling back locally and filling the history ring all change the screen
	// a client is drawing without changing a single cell, and a client that was
	// only ever sent rows would leave its caret wherever it last saw one. The
	// comparison is against the whole header, which is why it is one struct.
	headChanged := head != fr.lastHead
	if len(fr.changed) == 0 && !full && !headChanged {
		// Nothing new on the screen. That is the steady state — but it is also
		// where a tick lands when the only outstanding work is something an
		// EARLIER tick had to put off, because that tick already folded the
		// change into the shared shadow. This is the one thing that ever comes
		// back for it.
		if deferred {
			fr.retryDeferred(rows)
		}
		return
	}
	fr.lastHead = head
	fr.seq++
	head.Seq = fr.seq
	hdr, err := frameHeader(head)
	if err != nil {
		return
	}
	clear(fr.lines)

	fr.mu.Lock()
	fr.pendingKey = false
	for _, w := range fr.watchers {
		if full {
			w.key = true
		}
		for _, y := range fr.changed {
			w.dirty[y] = true
		}
		// Every watcher now owes its client something about THIS screen. Whether
		// it gets it in this tick or a later one is deliver's business.
		w.pending = true
	}
	fr.mu.Unlock()

	if fr.deliver(hdr, rows, time.Now()) {
		// Nothing else will wake this framer if the pane has gone quiet, and a
		// watcher with pending work and no wake would simply never see it.
		fr.stream.wake()
	}
}

// retryDeferred re-offers work an earlier tick could not hand over, on a screen
// that has not changed since.
//
// It reads no screen and invents no content: the rows come from each watcher's
// own dirty set, and the header is the last frame's with a fresh seq, because to
// whoever receives it this IS a new message. fr.lines still holds that screen's
// encodings and is deliberately NOT cleared — the shadow it was built from is
// the shadow being sent.
//
// The sequence number is only spent once somebody is actually ready for it.
// Without that guard a notify watcher waiting out its second would burn one per
// wake and leave a gap in every other watcher's stream.
func (fr *framer) retryDeferred(rows int) {
	now := time.Now()
	if !fr.anyReady(now) {
		fr.stream.wake()
		return
	}
	head := fr.lastHead
	fr.seq++
	head.Seq = fr.seq
	hdr, err := frameHeader(head)
	if err != nil {
		return
	}
	if fr.deliver(hdr, rows, now) {
		fr.stream.wake()
	}
}

// anyReady reports whether any watcher with pending work has come out of its own
// rate limit.
func (fr *framer) anyReady(now time.Time) bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, w := range fr.watchers {
		if w.pending && !w.tooSoon(now) {
			return true
		}
	}
	return false
}

// deliver hands every watcher with pending work whatever it is owed, and reports
// whether any had to be put off again.
//
// It is the ONE offering path: the tick that discovered a change and the retry
// that comes back for a deferred one go through it, so a watcher cannot be
// served by two rules that disagree. Caller must NOT hold fr.mu.
func (fr *framer) deliver(hdr []byte, rows int, now time.Time) (deferred bool) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, w := range fr.watchers {
		if !w.pending {
			continue
		}
		if w.tooSoon(now) {
			// Too soon for this one. Its rows stay dirty and are encoded from
			// the shadow when its turn comes, so what it eventually receives is
			// the current row rather than a stale one.
			deferred = true
			continue
		}
		if w.mode == protocol.WatchNotify {
			fr.offerChanged(w, now)
			continue
		}
		fr.offerFrame(w, hdr, rows, now)
	}
	// Remembered ACROSS ticks, not just to the end of this one.
	fr.deferred = deferred
	return deferred
}

// offerFrame hands one watcher its pending rows. Caller holds fr.mu.
func (fr *framer) offerFrame(w *paneWatcher, hdr []byte, rows int, now time.Time) {
	// No early return for an empty row set. tick only reaches here when there IS
	// a frame — rows changed, or the header did — and a header-only frame is how
	// a cursor move, a DECTCEM and a scroll reach a client at all. Second-
	// guessing it here is what made the cursor invisible to every viewer.
	if w.key {
		// A keyframe is every row, blanks included: the client clears first and
		// then has a complete screen, with no memory of what came before.
		for y := 0; y < rows; y++ {
			w.dirty[y] = true
		}
	}
	out := make(map[int][]byte, len(w.dirty))
	for y := range w.dirty {
		if line := fr.encodedRow(y); line != nil {
			out[y] = line
		}
	}
	w.sub.Offer(fr.pane, w.key, out, hdr)
	clear(w.dirty)
	w.key = false
	w.pending = false
	w.lastAt = now
}

// offerChanged is notify mode: the news that the pane moved, with no screen
// attached. Caller holds fr.mu and has already cleared the rate limit through
// tooSoon.
func (fr *framer) offerChanged(w *paneWatcher, now time.Time) {
	clear(w.dirty)
	w.key = false
	w.pending = false
	w.lastAt = now
	if line := changedLine(fr.pane); line != nil {
		w.sub.Offer(fr.pane, false, nil, line)
	}
}

// encodedRow renders one shadow row, once per tick however many watchers want
// it. Caller holds fr.mu (the cache is the framer's, and offering runs under
// it).
func (fr *framer) encodedRow(y int) []byte {
	if b, ok := fr.lines[y]; ok {
		return b
	}
	if y < 0 || y >= len(fr.shadow) {
		return nil
	}
	b, err := encodeLine(y, fr.shadow[y], fr.shCols, &fr.style)
	if err != nil {
		return nil
	}
	fr.lines[y] = b
	return b
}

// resizeShadow rebuilds the shadow for a new geometry. Everything in it is
// stale by definition, so the frame that follows is a keyframe.
func (fr *framer) resizeShadow(rows, cols int) {
	fr.shadow = make([][]Cell, rows)
	for i := range fr.shadow {
		fr.shadow[i] = make([]Cell, cols)
	}
	fr.shRows, fr.shCols = rows, cols
}

// rowSame compares a shadow row with a live one. Cell is comparable, so this
// allocates nothing — which matters, because it runs rows-times per tick.
func rowSame(shadow, live []Cell) bool {
	if len(shadow) != len(live) {
		return false
	}
	for i := range shadow {
		if shadow[i] != live[i] {
			return false
		}
	}
	return true
}

// copyRow updates one shadow row, padding when the live row is shorter. A short
// row means a resize raced the diff; the blanks keep the shadow rectangular so
// the next comparison is against the screen and not against a ragged copy.
func copyRow(dst, src []Cell) {
	n := copy(dst, src)
	for i := n; i < len(dst); i++ {
		dst[i] = Cell{Ch: ' ', Fg: defaultColor, Bg: defaultColor}
	}
}
