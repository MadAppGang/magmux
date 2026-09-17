package firebase

// The mirror: a hub.Sink whose peer is a database.
//
// Every other Sink in magmux wraps a connection, so back-pressure arrives for
// free — the peer stops reading, the queue fills, and the hub closes that
// subscriber. A database has none of that. It will accept everything magmux can
// send, bill for it, and rate-limit at the worst moment. So the bounds are
// built here:
//
//   - **One flusher, one request in flight, one tick.** Write() only ever
//     touches a map. Nothing in the hub's path ever does I/O.
//   - **Coalescing, not queueing.** Two state changes to one pane between ticks
//     are one write. A frame that arrives while an older frame is still pending
//     merges into it, exactly as the hub's slot does.
//   - **Priority, then a budget.** State, events and results go first and are
//     never shed. Frames go last and are cut the moment the token bucket is
//     empty, because a frame is a picture and the state beside it is the
//     report.
//
// The mark-and-sweep is worth stating. A flush takes a SNAPSHOT under the lock,
// recording a stamp per item, releases the lock, and does its I/O. On success it
// clears exactly the items whose stamp is unchanged, so anything Write recorded
// during the request survives. On failure it clears nothing. That is what makes
// a failed flush lose no state, and it is why every dirty item carries a stamp
// rather than a bool.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

const (
	// flushTick is the mirror's whole pace. Two writes a second, whatever the
	// session is doing.
	flushTick = 500 * time.Millisecond
	// heartbeatEvery is how often `meta/heartbeatAt` is refreshed. A reader
	// treats a session as dead after three missed beats (90 s), which is what
	// covers a SIGKILL — after one, `alive` stays true forever.
	heartbeatEvery = 30 * time.Second
	// eventRing is how many events the mirror keeps. Older keys are deleted in
	// the same PATCH that writes the newest one.
	eventRing = 200
	// maxEventBytes bounds one event's `data` string. A plugin that emits a
	// megabyte of JSON per event is not a thing to mirror at 2 Hz.
	maxEventBytes = 16 << 10
)

// stamped is one dirty item. The stamp is what makes the sweep safe; a stamp of
// zero means clean.
//
// A pane's meta and state nodes are CUMULATIVE, so the sweep zeroes the stamp
// and keeps the value: a PATCH replaces the node a path names, and an `exit`
// carrying two fields must not delete the controller's response from the
// mirror. Events and frames are not cumulative and are deleted outright —
// an event is written once, and a frame is superseded by the next one.
type stamped struct {
	val   any
	stamp uint64
}

// paneFrame is one pane's pending screen, merged the same way the hub's slot
// merges: a keyframe supersedes whatever was behind it, a delta replaces the
// rows it names, and `key` is sticky because a keyframe plus the deltas merged
// into it still covers every row.
type paneFrame struct {
	key   bool
	hdr   protocol.FrameHeader
	lines map[int]protocol.Line
	// changed is the delta row set: which rows must be written when key is
	// false. Unused when key is true, where the whole node is replaced.
	changed map[int]bool
	stamp   uint64
}

// merge folds one decoded frame in.
func (pf *paneFrame) merge(f *protocol.Frame) {
	pf.hdr = f.FrameHeader
	if f.Key {
		pf.key = true
		pf.lines = make(map[int]protocol.Line, len(f.Lines))
		pf.changed = nil
	}
	if pf.lines == nil {
		pf.lines = make(map[int]protocol.Line, len(f.Lines))
	}
	for _, l := range f.Lines {
		pf.lines[l.Y] = l
		if !pf.key {
			if pf.changed == nil {
				pf.changed = make(map[int]bool, len(f.Lines))
			}
			pf.changed[l.Y] = true
		}
	}
}

// mirror is the Sink and the flusher together.
type mirror struct {
	c    *client
	sess string
	log  func(string)
	fps  int

	mu sync.Mutex
	// nextStamp is the monotonic counter every dirty item takes its stamp
	// from. One counter for everything, so a stamp is unique across kinds.
	nextStamp uint64

	sessMeta      map[string]any
	sessMetaStamp uint64

	opsNode  map[string]any
	opsStamp uint64

	meta   map[int]*stamped // pane -> whole meta node
	state  map[int]*stamped // pane -> whole state node
	extra  map[string]*stamped
	frames map[int]*paneFrame

	// dropped is every path RTDB refused with a 400. It is never retried, and
	// it is logged once — a value the database will not take is a bug, and
	// re-sending it every 500 ms would hide it behind a wall of identical
	// failures.
	dropped map[string]bool
	// noted is set once a 400 has been reported to the human-visible log, so
	// the chrome note happens once per session.
	noted bool

	eventSeq uint64

	budget  *bucket
	closed  bool
	stopped bool
	// tick is the flush pace. It is a field rather than the constant so a test
	// can run the real Run loop without spending half a second per assertion;
	// nothing but a test ever changes it.
	tick time.Duration

	wake chan struct{}
	done chan struct{}
	once sync.Once
}

func newMirror(c *client, sess string, cfg *Config, log func(string)) *mirror {
	return &mirror{
		c:       c,
		sess:    sess,
		log:     log,
		fps:     cfg.FrameFPS,
		meta:    make(map[int]*stamped),
		state:   make(map[int]*stamped),
		extra:   make(map[string]*stamped),
		frames:  make(map[int]*paneFrame),
		dropped: make(map[string]bool),
		budget:  newBucket(cfg.ByteBudgetPerSec),
		tick:    flushTick,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// ── the Sink ────────────────────────────────────────────────────────────────

// Write takes one bus message. It never fails and never blocks: the message is
// decoded, projected onto the layout, and left in the dirty map for the
// flusher.
//
// It returns len(b) with a nil error unconditionally. The hub's torn-write rule
// is about bytes half-written to a connection, and there is no connection here —
// a value either reaches the database on some later tick or is superseded by a
// newer one, and neither outcome can corrupt a stream.
func (m *mirror) Write(b []byte) (int, error) {
	m.ingest(b)
	return len(b), nil
}

// SetWriteDeadline is a no-op. There is no blocked write to interrupt: Write
// returns in microseconds, always.
func (m *mirror) SetWriteDeadline(time.Time) error { return nil }

// Close is the hub saying it is finished with this subscriber. The FLUSHER is
// not stopped here — the adapter's Finalize still has a last flush to make,
// carrying `results` and `alive:false` — so this only stops new messages being
// accepted.
func (m *mirror) Close(reason string) {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}

// ── projection ──────────────────────────────────────────────────────────────

// ingest decodes one bus message and writes it into the dirty map.
func (m *mirror) ingest(b []byte) {
	var env struct {
		Type  string           `json:"type"`
		Pane  *int             `json:"pane"`
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Type == "" {
		return
	}
	switch env.Type {
	case protocol.EventFrame:
		m.onFrame(b)
		return
	case protocol.EventSnapshot:
		if len(env.Panes) > 0 {
			// The connect-time aggregate. It SEEDS the mirror and is not news,
			// so it goes into meta and state and not into the event ring.
			m.onAggregate(env.Panes)
			return
		}
		m.onPaneEvent(b, env.Pane)
	case protocol.EventResults:
		m.onAggregate(env.Panes)
	case protocol.EventPaneOpened, protocol.EventPaneClosed, protocol.EventExit:
		m.onPaneEvent(b, env.Pane)
	case protocol.EventOpsChanged:
		m.MarkOpsChanged()
	}
	m.recordEvent(env.Type, env.Pane, b)
}

// onAggregate projects an aggregate — the connect-time `snapshot` or the
// shutdown `results` — onto every pane it names.
func (m *mirror) onAggregate(panes []map[string]any) {
	for _, entry := range panes {
		id, ok := paneIDOf(entry)
		if !ok {
			continue
		}
		m.mergePane(id, entry)
	}
}

// onPaneEvent projects one pane-scoped message. The decode is into a generic
// map on purpose: every one of these events is an idempotent upsert of whatever
// fields it happens to carry, and enumerating them in a struct here would mean
// a field added in mux went silently unmirrored.
func (m *mirror) onPaneEvent(b []byte, pane *int) {
	if pane == nil {
		return
	}
	var entry map[string]any
	if err := json.Unmarshal(b, &entry); err != nil {
		return
	}
	m.mergePane(*pane, entry)
}

// metaFields and stateFields are the split in the layout. A field in neither is
// not mirrored per pane — it still reaches a reader through the event ring,
// which is where a one-off belongs.
var (
	metaFields  = []string{"label", "cmd", "cwd", "controller", "closed", "hidden", "control", "pid", "rows", "cols"}
	stateFields = []string{"state", "controller", "response", "tool", "prompt", "model", "project",
		"dead", "exitCode", "focused", "altMode", "inputSignal", "startedAt", "completedAt", "duration"}
)

// mergePane folds one map of fields into a pane's meta and state nodes.
//
// The nodes are kept WHOLE in memory and written whole, because a multi-path
// PATCH replaces the node a path names: writing `panes/p0/state` with only the
// two fields an `exit` carried would delete the controller's response from the
// mirror. Merging here and writing the merged node is the only shape that keeps
// a partial event partial.
func (m *mirror) mergePane(id int, entry map[string]any) {
	if id < 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.mergeNodeLocked(m.meta, id, entry, metaFields)
	m.mergeNodeLocked(m.state, id, entry, stateFields)
	// pane_closed carries no `closed` field of its own — the type IS the
	// statement — so it is made explicit here rather than left to a later
	// aggregate.
	if t, _ := entry["type"].(string); t == protocol.EventPaneClosed {
		m.mergeNodeLocked(m.meta, id, map[string]any{"closed": true}, []string{"closed"})
		m.mergeNodeLocked(m.state, id, map[string]any{"state": "closed", "dead": true}, []string{"state", "dead"})
	}
	m.signalLocked()
}

// vals is the node's field map, created on demand.
func (s *stamped) vals() map[string]any {
	v, _ := s.val.(map[string]any)
	if v == nil {
		v = map[string]any{}
		s.val = v
	}
	return v
}

// mergeNodeLocked copies the named fields of entry into node[id], creating it if
// needed, and stamps it dirty when anything actually changed.
//
// "Actually changed" is what keeps an idle session silent: a controller poll
// that reports the same state twice must not cost a write, or a magmux watching
// three shells would PATCH twice a second forever.
func (m *mirror) mergeNodeLocked(node map[int]*stamped, id int, entry map[string]any, fields []string) {
	cur := node[id]
	if cur == nil {
		cur = &stamped{}
		node[id] = cur
	}
	vals := cur.vals()
	changed := false
	for _, f := range fields {
		v, ok := entry[f]
		if !ok || v == nil {
			continue
		}
		if old, had := vals[f]; had && sameScalar(old, v) {
			continue
		}
		vals[f] = v
		changed = true
	}
	if !changed {
		return
	}
	m.nextStamp++
	cur.stamp = m.nextStamp
}

// sameScalar compares two decoded JSON scalars. Anything that is not a scalar
// is reported different, which costs one redundant write and never a missed
// one.
func sameScalar(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	}
	return false
}

// paneIDOf reads the `pane` field out of an aggregate entry.
func paneIDOf(entry map[string]any) (int, bool) {
	v, ok := entry["pane"]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// onFrame merges one frame into the pane's pending screen.
func (m *mirror) onFrame(b []byte) {
	var f protocol.Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	pf := m.frames[f.Pane]
	if pf == nil {
		pf = &paneFrame{}
		m.frames[f.Pane] = pf
	}
	pf.merge(&f)
	m.nextStamp++
	pf.stamp = m.nextStamp
	m.signalLocked()
}

// recordEvent appends to the ring and schedules the trim of the entry that fell
// off the end. The trim is a null in the SAME multi-path PATCH, so the ring
// never grows past its bound even for one tick.
func (m *mirror) recordEvent(typ string, pane *int, raw []byte) {
	if len(raw) > maxEventBytes {
		raw = raw[:0]
	}
	rec := map[string]any{
		"type": typ,
		"at":   serverTimestamp(),
		"data": jsonString(json.RawMessage(raw)),
	}
	if pane != nil {
		rec["pane"] = *pane
	}
	// A plugin event carries the plugin and the event name as their own
	// fields, so a reader can filter the ring without parsing every `data`.
	if typ == protocol.EventPlugin {
		var pe struct {
			Plugin string `json:"plugin"`
			Event  string `json:"event"`
		}
		if json.Unmarshal(raw, &pe) == nil {
			rec["plugin"], rec["event"] = pe.Plugin, pe.Event
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.eventSeq++
	seq := m.eventSeq
	m.setPathLocked("events/"+eventKey(seq), rec)
	if seq > eventRing {
		m.setPathLocked("events/"+eventKey(seq-eventRing), nil)
	}
	m.signalLocked()
}

// SetPath queues one arbitrary path, relative to the session node. It is how
// the command listener writes a result and deletes the command it answered, in
// the same PATCH as everything else — which is what makes both survive a
// backoff instead of being lost with the request that carried them.
func (m *mirror) SetPath(path string, value any) {
	m.mu.Lock()
	if !m.stopped {
		m.setPathLocked(path, value)
		m.signalLocked()
	}
	m.mu.Unlock()
}

func (m *mirror) setPathLocked(path string, value any) {
	if m.dropped[path] {
		return
	}
	m.nextStamp++
	m.extra[path] = &stamped{val: value, stamp: m.nextStamp}
}

// SetSessionMeta merges fields into the session's own `meta` node.
func (m *mirror) SetSessionMeta(fields map[string]any) {
	m.mu.Lock()
	if m.sessMeta == nil {
		m.sessMeta = make(map[string]any, len(fields))
	}
	for k, v := range fields {
		m.sessMeta[k] = v
	}
	m.nextStamp++
	m.sessMetaStamp = m.nextStamp
	m.signalLocked()
	m.mu.Unlock()
}

// MarkOpsChanged schedules a wholesale rewrite of the `ops` node.
//
// Wholesale because an op list is not a set of independent facts: a plugin that
// died has ops that must VANISH, and a PATCH of the ops that remain would leave
// the dead ones standing. The node is replaced, so the mirror's op list is
// always exactly the hub's.
func (m *mirror) MarkOpsChanged() {
	m.mu.Lock()
	m.nextStamp++
	m.opsStamp = m.nextStamp
	m.opsNode = nil // rebuilt at flush, from the hub, outside this lock
	m.signalLocked()
	m.mu.Unlock()
}

// ── the flusher ─────────────────────────────────────────────────────────────

// Run is the ONE goroutine that writes. It ticks every 500 ms, sends at most
// one request, and exits when ctx ends or Stop is called.
func (m *mirror) Run(ctx context.Context, ops func() map[string]any) {
	defer close(m.done)
	m.mu.Lock()
	every := m.tick
	m.mu.Unlock()
	if every <= 0 {
		every = flushTick
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()

	var bo backoff
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			m.SetSessionMeta(map[string]any{"heartbeatAt": serverTimestamp()})
			continue
		case <-tick.C:
		case <-m.wake:
			// A wake does NOT flush early. The tick is the pace, and honouring
			// a wake immediately would turn a busy pane into one request per
			// event. It exists so Stop is noticed between ticks.
			if m.isStopped() {
				return
			}
			continue
		}
		if m.isStopped() {
			return
		}
		err := m.flush(ctx, ops)
		if err == nil {
			bo.reset()
			continue
		}
		if ctx.Err() != nil {
			return
		}
		wait := bo.next()
		m.logf("flush failed (%v); retrying in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Stop ends the flusher after the current request.
func (m *mirror) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.signal()
}

func (m *mirror) isStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

// Done is closed when Run returns.
func (m *mirror) Done() <-chan struct{} { return m.done }

func (m *mirror) signal() {
	m.mu.Lock()
	m.signalLocked()
	m.mu.Unlock()
}

func (m *mirror) signalLocked() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *mirror) logf(format string, a ...any) {
	if m.log != nil {
		m.log("firebase: " + fmt.Sprintf(format, a...))
	}
}

// batch is one flush's worth of work: the payload to send and the stamps to
// clear if it lands.
type batch struct {
	payload map[string]any
	// taken records what went in, so the sweep can clear exactly the items
	// that were not superseded during the request.
	sessMeta uint64
	ops      uint64
	meta     map[int]uint64
	state    map[int]uint64
	extra    map[string]uint64
	frames   map[int]uint64
}

func newBatch() *batch {
	return &batch{
		payload: make(map[string]any),
		meta:    make(map[int]uint64),
		state:   make(map[int]uint64),
		extra:   make(map[string]uint64),
		frames:  make(map[int]uint64),
	}
}

// flush sends at most one multi-path PATCH.
func (m *mirror) flush(ctx context.Context, ops func() map[string]any) error {
	b := m.take(ops)
	if b == nil {
		return nil
	}
	err := m.c.patch(ctx, m.sess, b.payload)
	if err == nil {
		m.sweep(b)
		return nil
	}
	if he, ok := asHTTPError(err); ok && he.badPath() {
		// One path in the batch is unacceptable to RTDB. Find it by sending
		// each path alone: whatever still fails is dropped and logged, and
		// everything else has now landed. The alternative — giving up on the
		// batch — lets one bad value stall the whole mirror forever.
		m.retryIndividually(ctx, b)
		m.sweep(b)
		return nil
	}
	return err
}

// take builds the batch under the lock, and does no I/O.
func (m *mirror) take(ops func() map[string]any) *batch {
	var opsNode map[string]any
	m.mu.Lock()
	needOps := m.opsStamp != 0
	m.mu.Unlock()
	if needOps && ops != nil {
		// Built with the lock RELEASED: it reaches into the hub's registry, and
		// no lock of this package is ever held across a call-out.
		opsNode = ops()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	b := newBatch()
	add := func(path string, v any) {
		if m.dropped[path] {
			return
		}
		b.payload[path] = v
	}

	if m.sessMetaStamp != 0 {
		add("meta", copyMap(m.sessMeta))
		b.sessMeta = m.sessMetaStamp
	}
	if m.opsStamp != 0 && opsNode != nil {
		add("ops", opsNode)
		b.ops = m.opsStamp
	}
	// State, events and results before anything else: they are the report, and
	// the frames are the illustration.
	for path, e := range m.extra {
		add(path, e.val)
		b.extra[path] = e.stamp
	}
	// The node maps are COPIED into the payload. They are cumulative and live:
	// a Write during the request would otherwise mutate a map json.Marshal is
	// walking, outside the lock.
	for id, e := range m.meta {
		if e.stamp == 0 {
			continue
		}
		add("panes/"+paneKey(id)+"/meta", copyMap(e.vals()))
		b.meta[id] = e.stamp
	}
	for id, e := range m.state {
		if e.stamp == 0 {
			continue
		}
		add("panes/"+paneKey(id)+"/state", copyMap(e.vals()))
		b.state[id] = e.stamp
	}

	// Frames last, and only while the budget holds. A pane whose frame is cut
	// keeps it pending: the next tick merges the newer rows into it, so the
	// mirror falls behind rather than going wrong.
	m.budget.refill()
	for id, pf := range m.frames {
		paths, size := framePaths(id, pf)
		if !m.budget.take(size) {
			break
		}
		for p, v := range paths {
			add(p, v)
		}
		b.frames[id] = pf.stamp
	}

	if len(b.payload) == 0 {
		return nil
	}
	return b
}

// framePaths renders one pending frame as paths and bytes.
//
// A keyframe is ONE path holding the whole `frame` node, which is what makes a
// shrink correct: replacing the node drops the rows past the new height, where a
// per-row PATCH would leave them standing forever. A delta writes the scalars
// and the changed rows as siblings under `frame`, so no path in the batch is an
// ancestor of another — RTDB refuses a multi-path write that holds both.
func framePaths(id int, pf *paneFrame) (map[string]any, int) {
	base := "panes/" + paneKey(id) + "/frame"
	out := make(map[string]any, len(pf.lines)+8)
	size := 0
	if pf.key {
		lines := make(map[string]any, len(pf.lines))
		for y, l := range pf.lines {
			v := lineValue(l)
			lines[rowKey(y)] = v
			size += approxSize(v)
		}
		node := frameScalars(pf.hdr)
		node["lines"] = lines
		out[base] = node
		return out, size + 64
	}
	for k, v := range frameScalars(pf.hdr) {
		out[base+"/"+k] = v
	}
	size += 64
	for y := range pf.changed {
		l, ok := pf.lines[y]
		if !ok {
			continue
		}
		v := lineValue(l)
		out[base+"/lines/"+rowKey(y)] = v
		size += approxSize(v)
	}
	return out, size
}

// frameScalars is everything about a frame except its rows.
func frameScalars(h protocol.FrameHeader) map[string]any {
	return map[string]any{
		"seq":  h.Seq,
		"rows": h.Rows,
		"cols": h.Cols,
		"alt":  h.Alt,
		"sb":   h.Sb,
		"cur":  map[string]any{"y": h.Cur.Y, "x": h.Cur.X, "vis": h.Cur.Vis},
	}
}

// lineValue is one row: text, style runs, wide-character indexes. Empty
// members are omitted, which on a mostly-blank screen is most of the bytes.
func lineValue(l protocol.Line) map[string]any {
	v := map[string]any{"t": l.T}
	if len(l.R) > 0 {
		v["r"] = l.R
	}
	if len(l.Wd) > 0 {
		v["wd"] = l.Wd
	}
	return v
}

// approxSize is a cheap size estimate for the budget. Exact JSON length would
// mean marshalling every row twice; the budget is a shed threshold, not an
// accounting record.
func approxSize(v map[string]any) int {
	n := 16
	if t, ok := v["t"].(string); ok {
		n += len(t)
	}
	if r, ok := v["r"].([]protocol.Run); ok {
		n += len(r) * 24
	}
	if w, ok := v["wd"].([]int); ok {
		n += len(w) * 4
	}
	return n
}

// sweep clears every item the batch carried whose stamp is unchanged. An item
// Write touched during the request keeps its newer stamp and stays dirty.
func (m *mirror) sweep(b *batch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b.sessMeta != 0 && m.sessMetaStamp == b.sessMeta {
		m.sessMetaStamp = 0
	}
	if b.ops != 0 && m.opsStamp == b.ops {
		m.opsStamp = 0
	}
	// Zeroed, not deleted: the node stays so the next partial event merges into
	// it rather than replacing it with two fields.
	for id, s := range b.meta {
		if e := m.meta[id]; e != nil && e.stamp == s {
			e.stamp = 0
		}
	}
	for id, s := range b.state {
		if e := m.state[id]; e != nil && e.stamp == s {
			e.stamp = 0
		}
	}
	for path, s := range b.extra {
		if e := m.extra[path]; e != nil && e.stamp == s {
			delete(m.extra, path)
		}
	}
	for id, s := range b.frames {
		if pf := m.frames[id]; pf != nil && pf.stamp == s {
			delete(m.frames, id)
		}
	}
}

// retryIndividually is the 400 recovery: each path alone, and whatever still
// fails is dropped from the mirror for the rest of the session.
func (m *mirror) retryIndividually(ctx context.Context, b *batch) {
	for path, v := range b.payload {
		if err := m.c.patch(ctx, m.sess, map[string]any{path: v}); err == nil {
			continue
		} else if he, ok := asHTTPError(err); !ok || !he.badPath() {
			// Not this path's fault — a 429 or a network fault during the
			// recovery. Leave it dirty by NOT dropping it; the next tick
			// retries it with everything else.
			continue
		}
		m.drop(path)
	}
}

// drop removes a path from the mirror permanently and says so once.
func (m *mirror) drop(path string) {
	m.mu.Lock()
	first := !m.noted
	m.dropped[path] = true
	delete(m.extra, path)
	m.noted = true
	m.mu.Unlock()
	m.logf("dropping %s: the database refused it (400). The rest of the mirror keeps flushing.", path)
	if first && m.log != nil {
		m.log("magmux: firebase: a value was refused by the database and dropped; see the debug log")
	}
}

// FinalFlush is the last write: everything still pending, plus `alive:false`,
// under one deadline.
//
// `alive:false` is a CLEAN-shutdown marker and nothing more. A reader must not
// depend on it — a SIGKILL leaves it true forever — which is why the heartbeat
// exists and is the actual liveness signal.
func (m *mirror) FinalFlush(ctx context.Context, ops func() map[string]any) {
	m.SetSessionMeta(map[string]any{"alive": false, "heartbeatAt": serverTimestamp()})
	m.mu.Lock()
	// The budget must not shed the last frames: this is the final picture, and
	// there is no next tick to carry them.
	m.budget.grant()
	m.mu.Unlock()
	if err := m.flush(ctx, ops); err != nil {
		m.logf("final flush failed: %v", err)
	}
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ── the token bucket ────────────────────────────────────────────────────────

// bucket is the byte budget. It is charged for FRAMES only: state, events and
// results are small, bounded by the session's own activity, and are the thing a
// reader actually needs — shedding them to stay under a budget would make the
// mirror cheap and wrong.
type bucket struct {
	perSec int
	tokens int
	last   time.Time
	now    func() time.Time
}

func newBucket(perSec int) *bucket {
	b := &bucket{perSec: perSec, now: time.Now}
	b.last = b.now()
	b.tokens = perSec
	return b
}

// refill adds the tokens that accrued since the last call, capped at one
// second's worth so an idle session cannot bank a burst.
func (b *bucket) refill() {
	now := b.now()
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	b.last = now
	b.tokens += int(float64(b.perSec) * elapsed.Seconds())
	if b.tokens > b.perSec {
		b.tokens = b.perSec
	}
}

// take spends n tokens if there are any left. A frame larger than the whole
// budget is still sent once the bucket is full, rather than never: a 200-column
// screen on a 4 KB budget would otherwise be permanently invisible.
func (b *bucket) take(n int) bool {
	if b.tokens <= 0 {
		return false
	}
	b.tokens -= n
	return true
}

// grant fills the bucket. The final flush uses it.
func (b *bucket) grant() { b.tokens = b.perSec }
