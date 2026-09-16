package mux

// Streaming, in process, against the real hub.
//
// These tests use a real Hub, a real Sub and a real framer with a fake SINK —
// the one thing a unit test cannot have is a socket. That is deliberate: the
// properties under test (a keyframe first, deltas after, no frame after
// pane_closed, no goroutine without a watcher) are properties of the framer and
// the slot, and a real connection would only add timing.
//
// The panes are PTY-less control panes, for the same reason the dynamic-pane
// tests use them: a Screen and a VT parser are all a framer touches, so a
// failure names a bug in the streaming code rather than in a fixture's shell.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// ── harness ─────────────────────────────────────────────────────────────────

// capSink collects whole messages. It is the honest shape of a socket sink
// minus the socket: every Write is one complete line.
type capSink struct {
	mu     sync.Mutex
	lines  []string
	closed bool
	// block, when non-nil, holds every write until it is closed. It is how a
	// test makes a subscriber slow on purpose.
	block chan struct{}
}

func (s *capSink) Write(b []byte) (int, error) {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	s.lines = append(s.lines, strings.TrimRight(string(b), "\n"))
	s.mu.Unlock()
	return len(b), nil
}

func (s *capSink) SetWriteDeadline(time.Time) error { return nil }

func (s *capSink) Close(string) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *capSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// frames returns every frame this sink has received for one pane, decoded.
func (s *capSink) frames(pane int) []protocol.Frame {
	var out []protocol.Frame
	for _, line := range s.snapshot() {
		var f protocol.Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue
		}
		if f.Type == protocol.EventFrame && f.Pane == pane {
			out = append(out, f)
		}
	}
	return out
}

// watcherOn opens a subscriber on m's hub with a collecting sink.
func watcherOn(t *testing.T, m *Magmux) (*hub.Sub, *capSink) {
	t.Helper()
	// Installs the Streamer as the hub's Watcher; without it every watch is
	// answered `unsupported`.
	m.streamer()
	sink := &capSink{}
	sub := m.bus().Session(hub.Caller{Transport: "socket", Conn: "test"}, sink, nil)
	sub.Start(nil)
	t.Cleanup(func() { sub.Close("test over") })
	return sub, sink
}

// feedWatched writes bytes into a pane and then rings the streaming hook, which
// together is what readLoop does under one p.mu. It is `feed` plus the hook
// rather than a second copy of the write, so a change to how bytes reach the
// parser cannot be made in one of them and not the other.
func feedWatched(p *Pane, text string) {
	feed(p, text)
	p.mu.Lock()
	p.noteOutputLocked()
	p.mu.Unlock()
}

// awaitFrames waits for at least n frames for a pane, or fails.
func awaitFrames(t *testing.T, sink *capSink, pane, n int) []protocol.Frame {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fs := sink.frames(pane); len(fs) >= n {
			return fs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d frames for pane %d after 3s; lines: %v", len(sink.frames(pane)), pane, sink.snapshot())
	return nil
}

// textAt pulls one row's text out of a frame, or "" when the frame does not
// carry that row.
func textAt(f protocol.Frame, y int) string {
	for _, l := range f.Lines {
		if l.Y == y {
			return l.T
		}
	}
	return ""
}

// ── the hook ────────────────────────────────────────────────────────────────

// TestReadLoopHookIsAllocationFree is the cost model, stated as a number.
//
// Streaming is a feature almost nobody uses on almost every pane, and it sits
// in the hottest path magmux has: every byte a child prints goes through
// readLoop. The hook is therefore one atomic add and one atomic load, and this
// test is what stops it becoming anything else — a map lookup, a closure, a
// slice append — where the cost would be invisible in review and obvious in a
// profile six months later.
//
// Both cases are pinned. The unwatched one is the one that matters; the watched
// one is here because a wake that allocated would be just as bad, and it is one
// non-blocking send into a cap-1 channel.
func TestReadLoopHookIsAllocationFree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		watched bool
	}{
		{"unwatched", false},
		{"watched", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newScrollPane(24, 80)
			if tc.watched {
				p.stream.Store(&paneStream{wakeCh: make(chan struct{}, 1)})
			}
			if n := testing.AllocsPerRun(1000, func() {
				p.mu.Lock()
				p.noteOutputLocked()
				p.mu.Unlock()
			}); n != 0 {
				t.Errorf("the readLoop hook allocated %v times per call; it must be free", n)
			}
			if n := testing.AllocsPerRun(1000, func() {
				p.mu.Lock()
				p.noteGeometryLocked()
				p.mu.Unlock()
			}); n != 0 {
				t.Errorf("the resize hook allocated %v times per call", n)
			}
		})
	}
	// And the counter really counts: a hook that did nothing would pass the
	// assertion above and break everything below it.
	p := newScrollPane(24, 80)
	before := p.frameGen.Load()
	feedWatched(p, "hello")
	if p.frameGen.Load() == before {
		t.Error("frameGen did not advance on output; a framer would never look at the screen")
	}
}

// TestIdleWatchedPaneCostsNothing is N2: a pane that is being watched and is
// doing nothing produces no frames and allocates nothing per tick.
//
// This is the difference between wake-driven and polled. A 15 fps poll over
// eight watched panes would diff 120 screens a second forever; here the tick
// compares one integer and returns.
func TestIdleWatchedPaneCostsNothing(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, _ := watcherOn(t, m)
	p := m.allPanes[0]

	// A framer driven by hand, so a tick is a tick and not a race with a
	// goroutine. It has a watcher, which is what gets tick past its first gate;
	// what it must not get past is the second.
	fr := newFramer(m.streamer(), p, p.id)
	fr.add(sub, protocol.WatchFrames, protocol.FPSMax)
	fr.tick() // the joining keyframe, and the shadow is now the screen

	if n := testing.AllocsPerRun(200, func() { fr.tick() }); n != 0 {
		t.Errorf("an idle watched pane's tick allocated %v times; the steady state must be free", n)
	}
	if fr.seq != 1 {
		t.Errorf("seq = %d after one keyframe and 200 idle ticks; an idle pane built %d extra frames", fr.seq, fr.seq-1)
	}
}

// TestIdleWatchedPaneSendsNothing is the same claim from the subscriber's end,
// through the real framer goroutine: a pane that says nothing produces no
// frames, however long anybody watches it.
func TestIdleWatchedPaneSendsNothing(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[0]
	if _, err := sub.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}
	awaitFrames(t, sink, p.id, 1)

	before := len(sink.frames(p.id))
	time.Sleep(300 * time.Millisecond) // ~9 frames at 30fps, if it polled
	if after := len(sink.frames(p.id)); after != before {
		t.Errorf("an idle watched pane produced %d frames in 300ms; the framer is polling, not waking", after-before)
	}
}

// TestNoFramerWithoutAWatcher is the other half of N2: the goroutine exists
// only while somebody is watching.
func TestNoFramerWithoutAWatcher(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	sub, _ := watcherOn(t, m)
	st := m.streamer()
	p := m.allPanes[0]

	if st.framerCount() != 0 {
		t.Fatalf("%d framers before anything watched", st.framerCount())
	}
	if _, err := sub.Watch(p.id, protocol.WatchFrames, 15); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if st.framerCount() != 1 {
		t.Fatalf("%d framers with one watcher, want 1", st.framerCount())
	}
	if p.stream.Load() == nil {
		t.Error("the pane's read loop was never told about its framer, so nothing will wake it")
	}
	sub.Unwatch(p.id)
	if st.framerCount() != 0 {
		t.Errorf("%d framers after the last watcher left; the goroutine outlived its reason to exist", st.framerCount())
	}
	if p.stream.Load() != nil {
		t.Error("the pane still carries a stream handle, so its read loop is waking a framer that is gone")
	}
}

// ── frames ──────────────────────────────────────────────────────────────────

// TestFirstFrameIsAKeyframeThenDeltas is the shape of a stream: one complete
// screen, then only what changed.
//
// The delta half is the claim worth testing. A frame that repeated all 24 rows
// on every keystroke would work perfectly and cost thirty times as much, and
// nothing else in the system would notice.
func TestFirstFrameIsAKeyframeThenDeltas(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[0]

	info, err := sub.Watch(p.id, protocol.WatchFrames, protocol.FPSMax)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	rows, cols := p.size()
	if info.Rows != rows || info.Cols != cols {
		t.Errorf("watch reported %dx%d, the pane is %dx%d", info.Rows, info.Cols, rows, cols)
	}

	first := awaitFrames(t, sink, p.id, 1)[0]
	if !first.Key {
		t.Fatalf("the first frame is not a keyframe: %+v", first)
	}
	if len(first.Lines) != rows {
		t.Errorf("a keyframe carried %d lines for a %d-row pane; a client that clears first would be left with holes",
			len(first.Lines), rows)
	}

	feedWatched(p, "MAGMUX_STREAM_OK")
	frames := awaitFrames(t, sink, p.id, 2)
	delta := frames[len(frames)-1]
	if delta.Key {
		t.Fatalf("the frame after the keyframe is another keyframe: %+v", delta)
	}
	if len(delta.Lines) >= rows {
		t.Errorf("a delta carried %d of %d rows; only the changed row should be in it", len(delta.Lines), rows)
	}
	if got := textAt(delta, 0); !strings.Contains(got, "MAGMUX_STREAM_OK") {
		t.Errorf("row 0 of the delta = %q, want the text that was printed (lines: %+v)", got, delta.Lines)
	}
	if delta.Seq <= first.Seq {
		t.Errorf("seq went backwards: %d then %d", first.Seq, delta.Seq)
	}
}

// TestResizeForcesAKeyframe. A client cannot apply a row delta across a
// geometry change — the rows it has are a different width — so magmux must not
// send it one.
func TestResizeForcesAKeyframe(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[0]
	if _, err := sub.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}
	awaitFrames(t, sink, p.id, 1)

	m.treeMu.Lock()
	p.resize(p.y, p.x, 12, 40)
	m.treeMu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fs := sink.frames(p.id)
		last := fs[len(fs)-1]
		if last.Cols == 40 && last.Rows == 12 {
			if !last.Key {
				t.Fatalf("the frame after a resize is a delta: %+v", last)
			}
			if len(last.Lines) != 12 {
				t.Errorf("the post-resize keyframe carried %d lines for 12 rows", len(last.Lines))
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no frame at the new geometry after 3s: %v", sink.frames(p.id))
}

// TestResyncSendsAKeyframeWithoutNewOutput. A client that lost its copy of the
// screen — a canvas resize, a reload — must be able to get back to a known
// state without dropping and re-opening the subscription, and without the pane
// having to say anything.
func TestResyncSendsAKeyframeWithoutNewOutput(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[0]
	if _, err := sub.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}
	// The keyframe first: watch is answered before the pane says anything, so
	// waiting for it here keeps the counting below about the resync.
	awaitFrames(t, sink, p.id, 1)
	feedWatched(p, "before resync")
	awaitFrames(t, sink, p.id, 2)
	before := len(sink.frames(p.id))

	if err := sub.Resync(p.id); err != nil {
		t.Fatalf("resync: %v", err)
	}
	frames := awaitFrames(t, sink, p.id, before+1)
	last := frames[len(frames)-1]
	if !last.Key {
		t.Fatalf("resync produced a delta: %+v", last)
	}
	if !strings.Contains(textAt(last, 0), "before resync") {
		t.Errorf("the resync keyframe does not carry what is on the screen: %+v", last.Lines)
	}

	// A resync for a pane this connection does not watch is a mistake worth an
	// answer, not a silent no-op: the client believes it has a subscription.
	if err := sub.Resync(p.id + 99); err == nil {
		t.Error("resync on a pane nobody is watching succeeded")
	}
}

// TestASecondWatcherGetsItsOwnKeyframe. Two connections on one pane share a
// framer and a shadow, and the second one must still start from a complete
// screen rather than from whatever the first one's next delta happens to be.
func TestASecondWatcherGetsItsOwnKeyframe(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	subA, sinkA := watcherOn(t, m)
	p := m.allPanes[0]
	if _, err := subA.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch A: %v", err)
	}
	awaitFrames(t, sinkA, p.id, 1)
	feedWatched(p, "already on screen")
	awaitFrames(t, sinkA, p.id, 2)

	subB, sinkB := watcherOn(t, m)
	if _, err := subB.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch B: %v", err)
	}
	first := awaitFrames(t, sinkB, p.id, 1)[0]
	if !first.Key {
		t.Fatalf("the second watcher's first frame is a delta: %+v", first)
	}
	if !strings.Contains(textAt(first, 0), "already on screen") {
		t.Errorf("the second watcher's keyframe is missing what was printed before it joined: %+v", first.Lines)
	}
	if m.streamer().framerCount() != 1 {
		t.Errorf("%d framers for one pane with two watchers; they must share one", m.streamer().framerCount())
	}
}

// TestNotifyModeSendsNewsNotScreens. A subscriber that has its own way to read
// a pane wants to know WHEN, not WHAT, and paying for a screen it will not use
// is the whole thing this mode avoids.
func TestNotifyModeSendsNewsNotScreens(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[0]
	if _, err := sub.Watch(p.id, protocol.WatchNotify, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}
	feedWatched(p, "something happened")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range sink.snapshot() {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			if ev["type"] == protocol.EventChanged {
				if len(sink.frames(p.id)) != 0 {
					t.Errorf("a notify watcher was sent a frame as well: %v", sink.snapshot())
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no `changed` after 3s: %v", sink.snapshot())
}

// TestNotifyModeDoesNotDropTheChangeItRateLimited is the regression that
// test/rc/case4-mcp.ts found.
//
// Notify mode sends at most one `changed` a second. The first change after a
// watch is delivered at once, which STARTS that second — so a change arriving
// inside it is declined. tick had already advanced fr.lastGen past the screen
// it read, so the next wake returned early, and if the pane then fell silent
// there was no further generation to bring the work back. The notification was
// gone for good and the client sat on a screen it believed was current: a
// silent stale read, which is the one failure a notification channel exists to
// prevent.
//
// The shape here is exactly the failing one: change, take the first `changed`,
// change again WELL INSIDE the second, and then go quiet. It must still arrive.
// Reverting either half of the fix (framer.deferred, or offerChanged's bool)
// hangs this test on the second wait.
func TestNotifyModeDoesNotDropTheChangeItRateLimited(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[0]
	if _, err := sub.Watch(p.id, protocol.WatchNotify, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}

	changes := func() int {
		n := 0
		for _, line := range sink.snapshot() {
			var ev map[string]any
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			if ev["type"] == protocol.EventChanged {
				n++
			}
		}
		return n
	}
	awaitChanges := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if changes() >= want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("only %d `changed` events after 5s, want %d — %s\n%v", changes(), want, what, sink.snapshot())
	}

	feedWatched(p, "first change")
	awaitChanges(1, "the first change is never rate-limited")

	// Inside the one-second budget the first `changed` opened, and then silence:
	// nothing after this point produces another generation for the framer.
	time.Sleep(50 * time.Millisecond)
	feedWatched(p, "second change, inside the rate-limit window")
	awaitChanges(2, "a rate-limited `changed` must be delivered when its budget expires, not dropped")

	// And it really is news, not a screen: notify mode never pays for rows.
	if fr := sink.frames(p.id); len(fr) != 0 {
		t.Errorf("a notify watcher was sent %d frames as well", len(fr))
	}
}

// TestNoFrameAfterPaneClosed is a race closed by state, and the reason
// ClosePane stops the framer BEFORE it publishes.
//
// A client that received a picture of a pane after being told the pane was gone
// has to decide which of the two magmux meant. There is no good answer, so the
// case is made unreachable rather than handled.
func TestNoFrameAfterPaneClosed(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[1]
	if _, err := sub.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}
	awaitFrames(t, sink, p.id, 1)

	// Keep the pane talking while it is closed, so a framer that survived would
	// have something to say.
	stop := make(chan struct{})
	var chatty sync.WaitGroup
	chatty.Add(1)
	go func() {
		defer chatty.Done()
		for {
			select {
			case <-stop:
				return
			default:
				feedWatched(p, "x")
				time.Sleep(time.Millisecond)
			}
		}
	}()

	if err := m.ClosePane(p.id, true); err != nil {
		t.Fatalf("close_pane: %v", err)
	}
	close(stop)
	chatty.Wait()
	time.Sleep(100 * time.Millisecond)

	lines := sink.snapshot()
	closedAt := -1
	for i, line := range lines {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev["type"] == protocol.EventPaneClosed {
			closedAt = i
		}
	}
	if closedAt < 0 {
		t.Fatalf("no pane_closed event at all: %v", lines)
	}
	for i := closedAt + 1; i < len(lines); i++ {
		var f protocol.Frame
		if json.Unmarshal([]byte(lines[i]), &f) == nil && f.Type == protocol.EventFrame && f.Pane == p.id {
			t.Fatalf("a frame for pane %d arrived after pane_closed:\n  %s", p.id, lines[i])
		}
	}
	if m.streamer().framerCount() != 0 {
		t.Errorf("%d framers after the only watched pane closed", m.streamer().framerCount())
	}
}

// TestSlotCoalescesFramesForASlowSubscriber is the reason frames do not queue.
//
// A subscriber that reads slowly must see FEWER frames, never OLDER ones. The
// sink here is blocked while a hundred screens go by; when it unblocks it must
// receive one frame carrying the LAST of them, not a hundred frames replaying
// the session.
func TestSlotCoalescesFramesForASlowSubscriber(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	m.streamer()
	sink := &capSink{block: make(chan struct{})}
	sub := m.bus().Session(hub.Caller{Transport: "socket", Conn: "slow"}, sink, nil)
	sub.Start(nil)
	t.Cleanup(func() { sub.Close("test over") })
	p := m.allPanes[0]

	if _, err := sub.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}
	for i := 0; i < 100; i++ {
		feedWatched(p, "\r\nline")
		time.Sleep(time.Millisecond)
	}
	feedWatched(p, "\r\nMAGMUX_LAST_LINE")
	time.Sleep(150 * time.Millisecond)
	close(sink.block)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fs := sink.frames(p.id)
		if len(fs) > 0 {
			whole := strings.Join(sink.snapshot(), "\n")
			if !strings.Contains(whole, "MAGMUX_LAST_LINE") {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if len(fs) > 10 {
				t.Errorf("a blocked subscriber received %d frames; the slot must coalesce, not queue", len(fs))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the slow subscriber never received a frame: %v", sink.snapshot())
}

// TestCursorVisibilityRidesTheFrame: DECTCEM was ignored until now, so every
// remote viewer drew a cursor in the middle of a TUI that had deliberately
// taken it away. It is PANE state, not screen state, which is what makes it
// survive the alternate-screen switch a TUI exits through.
func TestCursorVisibilityRidesTheFrame(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	sub, sink := watcherOn(t, m)
	p := m.allPanes[0]
	if _, err := sub.Watch(p.id, protocol.WatchFrames, protocol.FPSMax); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if f := awaitFrames(t, sink, p.id, 1)[0]; !f.Cur.Vis {
		t.Error("the cursor starts hidden; a pane that never said anything about DECTCEM has a visible cursor")
	}

	feedWatched(p, "\x1b[?25l") // DECTCEM off
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fs := sink.frames(p.id)
		if !fs[len(fs)-1].Cur.Vis {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fs := sink.frames(p.id)
	if fs[len(fs)-1].Cur.Vis {
		t.Fatal("the frame still says the cursor is visible after CSI ?25l")
	}

	// It must survive an alternate-screen round trip in both directions, which
	// is why it lives on the pane. A per-screen flag would hand the shell back a
	// cursor magmux believes is hidden.
	p.mu.Lock()
	hidden := p.curHidden
	p.mu.Unlock()
	if !hidden {
		t.Fatal("curHidden was not recorded on the pane")
	}
	feedWatched(p, "\x1bc") // RIS
	p.mu.Lock()
	hidden = p.curHidden
	p.mu.Unlock()
	if hidden {
		t.Error("a full reset left the cursor hidden; `reset` after a crashed TUI must give it back")
	}
}
