package hub

// The slot, tested as the thing it is: a place where at most one frame per pane
// can be waiting, and where a later frame REPLACES rather than follows.
//
// The Watcher here is a fake, because the hub's half of streaming is exactly
// this — nothing in this package knows what a screen is.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// fakeWatcher records what the hub asked for and lets a test decide what a
// watch answers. Watch may also run a hook, which is how a test offers a frame
// at the exact instant a watch is being answered.
type fakeWatcher struct {
	mu        sync.Mutex
	watched   map[int]int // pane -> how many times
	unwatched []int
	resynced  []int
	dropped   int
	all       int
	err       error
	onWatch   func(s *Sub, pane int)
}

func newFakeWatcher() *fakeWatcher { return &fakeWatcher{watched: map[int]int{}} }

func (w *fakeWatcher) Watch(s *Sub, pane int, mode protocol.WatchMode, fps int) (protocol.WatchInfo, error) {
	w.mu.Lock()
	w.watched[pane]++
	err, hook := w.err, w.onWatch
	w.mu.Unlock()
	if err != nil {
		return protocol.WatchInfo{}, err
	}
	if hook != nil {
		hook(s, pane)
	}
	return protocol.WatchInfo{Pane: pane, Rows: 24, Cols: 80, Mode: protocol.ResolveWatchMode(mode), FPS: protocol.ClampFPS(fps)}, nil
}

func (w *fakeWatcher) Unwatch(s *Sub, pane int) {
	w.mu.Lock()
	w.unwatched = append(w.unwatched, pane)
	w.mu.Unlock()
}

func (w *fakeWatcher) Resync(s *Sub, pane int) error {
	w.mu.Lock()
	w.resynced = append(w.resynced, pane)
	w.mu.Unlock()
	return nil
}

func (w *fakeWatcher) WatchAll(s *Sub, fps int) {
	w.mu.Lock()
	w.all++
	w.mu.Unlock()
}

func (w *fakeWatcher) Drop(s *Sub) {
	w.mu.Lock()
	w.dropped++
	w.mu.Unlock()
}

func (w *fakeWatcher) drops() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// frameOf is the encoder's half of the contract, as the streamer produces it:
// a header left open, and one encoded row per changed line.
func frameOf(pane int, seq uint64, ys ...int) (map[int][]byte, []byte) {
	hdr := []byte(`{"type":"frame","pane":` + itoa(pane) + `,"seq":` + itoa(int(seq)) + `,"rows":24,"cols":80`)
	rows := map[int][]byte{}
	for _, y := range ys {
		rows[y] = []byte(`{"y":` + itoa(y) + `,"t":"row ` + itoa(y) + ` seq ` + itoa(int(seq)) + `"}`)
	}
	return rows, hdr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// subWithWatcher opens a started Sub on a hub that has a fake Watcher.
func subWithWatcher(t *testing.T) (*Hub, *fakeWatcher, *Sub, *fakeSink) {
	t.Helper()
	h := New()
	w := newFakeWatcher()
	h.SetWatcher(w)
	sink := newFakeSink()
	s := h.Session(Caller{Transport: "socket", Conn: "sock#1"}, sink, nil)
	s.Start(nil)
	t.Cleanup(func() { s.kill("test over") })
	return h, w, s, sink
}

// TestSlotCoalescesByRow is the merge rule.
//
// Frames are STATE, not news: a subscriber that is not reading must see fewer
// frames, never older ones. A hundred offers behind a blocked write collapse
// into one message whose every row is the newest version of that row — which is
// only sound because a delta row is a WHOLE row and replaces the row it names.
func TestSlotCoalescesByRow(t *testing.T) {
	_, _, s, sink := subWithWatcher(t)
	sink.mu.Lock()
	sink.blockWrites = 1
	sink.mu.Unlock()

	if _, err := s.Watch(3, protocol.WatchFrames, 15); err != nil {
		t.Fatalf("watch: %v", err)
	}
	// One write is blocked, so everything below lands in the slot.
	rows, hdr := frameOf(3, 1, 0, 1)
	s.Offer(3, false, rows, hdr)
	waitFor(t, time.Second, "the writer to pick up the first frame", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.writes == 1
	})

	rows, hdr = frameOf(3, 2, 1, 2)
	s.Offer(3, false, rows, hdr)
	rows, hdr = frameOf(3, 3, 2, 5)
	s.Offer(3, false, rows, hdr)

	close(sink.release)
	waitFor(t, time.Second, "the merged frame", func() bool { return len(sink.seen()) >= 2 })

	var merged protocol.Frame
	if err := json.Unmarshal([]byte(sink.seen()[1]), &merged); err != nil {
		t.Fatalf("the merged frame is not valid JSON: %v\n%s", err, sink.seen()[1])
	}
	if merged.Seq != 3 {
		t.Errorf("the merged frame carries seq %d; the NEWEST header must win", merged.Seq)
	}
	got := map[int]string{}
	for _, l := range merged.Lines {
		got[l.Y] = l.T
	}
	if len(got) != 3 {
		t.Fatalf("merged lines = %v, want rows 1, 2 and 5 (the union of both offers)", got)
	}
	if !strings.Contains(got[1], "seq 2") {
		t.Errorf("row 1 = %q, want the version from seq 2 (its last update)", got[1])
	}
	if !strings.Contains(got[2], "seq 3") {
		t.Errorf("row 2 = %q, want the version from seq 3; a later offer must WIN, not queue", got[2])
	}
	if merged.Key {
		t.Error("two deltas merged into a keyframe")
	}
}

// TestKeyframeSupersedesPendingDeltas. A keyframe describes the whole screen,
// so anything waiting behind it is already in it — and the merged result is
// still a keyframe, because it still covers every row.
func TestKeyframeSupersedesPendingDeltas(t *testing.T) {
	_, _, s, sink := subWithWatcher(t)
	sink.mu.Lock()
	sink.blockWrites = 1
	sink.mu.Unlock()
	if _, err := s.Watch(1, protocol.WatchFrames, 15); err != nil {
		t.Fatalf("watch: %v", err)
	}
	rows, hdr := frameOf(1, 1, 0)
	s.Offer(1, false, rows, hdr)
	waitFor(t, time.Second, "the first write to start", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.writes == 1
	})

	rows, hdr = frameOf(1, 2, 7)
	s.Offer(1, false, rows, hdr) // a delta, queued in the slot
	rows, hdr = frameOf(1, 3, 0, 1, 2)
	s.Offer(1, true, rows, hdr) // a keyframe over the top of it
	close(sink.release)
	waitFor(t, time.Second, "the keyframe", func() bool { return len(sink.seen()) >= 2 })

	var f protocol.Frame
	if err := json.Unmarshal([]byte(sink.seen()[1]), &f); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if !f.Key {
		t.Fatal("the merged frame is not a keyframe")
	}
	for _, l := range f.Lines {
		if l.Y == 7 {
			t.Errorf("row 7 from the superseded delta survived into the keyframe: %+v", f.Lines)
		}
	}
	if len(f.Lines) != 3 {
		t.Errorf("keyframe lines = %d, want the 3 the keyframe itself carried", len(f.Lines))
	}
}

// TestPaneClosedRefusesLaterOffers is the race the closed-pane set exists for.
//
// A client that is told a pane is gone and is then shown a picture of it has to
// decide which one magmux meant. There is no good answer, so PaneClosed makes
// the second one unreachable: the slot is cleared and every later offer is
// dropped under the same lock.
func TestPaneClosedRefusesLaterOffers(t *testing.T) {
	h, _, s, sink := subWithWatcher(t)
	if _, err := s.Watch(4, protocol.WatchFrames, 15); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// A frame already waiting in the slot must go too: it is a picture of a pane
	// that no longer exists.
	sink.mu.Lock()
	sink.blockWrites = 1
	sink.mu.Unlock()
	rows, hdr := frameOf(4, 1, 0)
	s.Offer(4, false, rows, hdr)
	waitFor(t, time.Second, "the writer to be busy", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.writes == 1
	})
	rows, hdr = frameOf(4, 2, 1)
	s.Offer(4, false, rows, hdr)

	h.PaneClosed(4)

	// And so must one offered afterwards, by a framer that has not stopped yet.
	rows, hdr = frameOf(4, 3, 2)
	s.Offer(4, false, rows, hdr)

	close(sink.release)
	time.Sleep(50 * time.Millisecond)
	for i, line := range sink.seen() {
		if i == 0 {
			continue // the one already in flight when the pane closed
		}
		if strings.Contains(line, `"pane":4`) {
			t.Errorf("a frame for pane 4 was written after PaneClosed: %s", line)
		}
	}

	// A watch on a pane that has closed for this connection fails rather than
	// opening a subscription to nothing.
	if _, err := s.Watch(4, protocol.WatchFrames, 15); err == nil {
		t.Error("watching a closed pane succeeded")
	} else if protocol.CodeOf(err) != protocol.CodeNoSuchPane {
		t.Errorf("watching a closed pane: code %q, want no_such_pane", protocol.CodeOf(err))
	}
}

// TestWatchReplyPrecedesItsFirstFrame is the other producer race, and the
// reason the slot is created INACTIVE.
//
// The fake Watcher offers a frame from inside Watch — which is exactly what a
// framer that starts and immediately has a keyframe ready does. The reply must
// still come first: a client that is shown a pane before it is told the
// subscription exists cannot tell which request the frame belongs to.
func TestWatchReplyPrecedesItsFirstFrame(t *testing.T) {
	_, w, s, sink := subWithWatcher(t)
	w.mu.Lock()
	w.onWatch = func(sub *Sub, pane int) {
		rows, hdr := frameOf(pane, 1, 0)
		sub.Offer(pane, true, rows, hdr)
	}
	w.mu.Unlock()

	s.Call(context.Background(), "watch", json.RawMessage(`{"pane":2,"fps":15}`),
		func(result map[string]any, err error) []byte {
			if err != nil {
				t.Errorf("watch failed: %v", err)
				return nil
			}
			line, _ := json.Marshal(map[string]any{"type": "reply", "id": 7, "ok": true, "result": result})
			return append(line, '\n')
		})

	waitFor(t, time.Second, "the watch reply and its frame", func() bool { return len(sink.seen()) >= 2 })
	seen := sink.seen()
	if !strings.Contains(seen[0], `"type":"reply"`) {
		t.Fatalf("the first line is not the watch reply:\n  %s\n  %s", seen[0], seen[1])
	}
	if !strings.Contains(seen[0], `"rows":24`) || !strings.Contains(seen[0], `"cols":80`) {
		t.Errorf("the watch reply does not carry the geometry a client sizes itself from: %s", seen[0])
	}
	if !strings.Contains(seen[1], `"type":"frame"`) {
		t.Errorf("the second line is not the frame: %s", seen[1])
	}
}

// TestFramesGoBehindEventsAndFinals. A frame is the current screen and stays
// current while it waits; an event is news, and news delayed behind a picture
// is worse. And once Finalize has run, `results` is the authoritative final
// state of every pane — a screen written after it would contradict the report.
func TestFramesGoBehindEventsAndFinals(t *testing.T) {
	h, _, s, sink := subWithWatcher(t)
	sink.mu.Lock()
	sink.blockWrites = 1
	sink.mu.Unlock()
	if _, err := s.Watch(1, protocol.WatchFrames, 15); err != nil {
		t.Fatalf("watch: %v", err)
	}
	// Something to block on, so everything after it queues.
	s.Send([]byte("{\"type\":\"first\"}\n"))
	waitFor(t, time.Second, "the writer to be busy", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.writes == 1
	})

	rows, hdr := frameOf(1, 1, 0)
	s.Offer(1, true, rows, hdr)
	h.Publish([]byte("{\"type\":\"snapshot\",\"pane\":1}\n"))
	close(sink.release)

	waitFor(t, time.Second, "both to be written", func() bool { return len(sink.seen()) >= 3 })
	seen := sink.seen()
	if !strings.Contains(seen[1], `"type":"snapshot"`) {
		t.Errorf("a frame was written before an event that was queued after it:\n  %s", strings.Join(seen, "\n  "))
	}

	// Finalize discards the slots along with the backlog.
	rows, hdr = frameOf(1, 2, 0)
	s.Offer(1, false, rows, hdr)
	h.Finalize([]byte("{\"type\":\"results\"}\n"), []byte("{\"type\":\"shutdown\"}\n"))
	waitDone(t, s, 3*time.Second, "finalize")
	seen = sink.seen()
	for i, line := range seen {
		if strings.Contains(line, `"type":"results"`) {
			for _, later := range seen[i:] {
				if strings.Contains(later, `"type":"frame"`) {
					t.Errorf("a frame was written at or after results:\n  %s", later)
				}
			}
			return
		}
	}
	t.Errorf("results never arrived: %v", seen)
}

// TestSubCloseDropsItsWatches. A framer running for a connection nobody reads
// is a goroutine diffing a screen into a slot that will never be written.
func TestSubCloseDropsItsWatches(t *testing.T) {
	_, w, s, _ := subWithWatcher(t)
	if _, err := s.Watch(1, protocol.WatchFrames, 15); err != nil {
		t.Fatalf("watch: %v", err)
	}
	s.Close("client disconnected")
	waitFor(t, time.Second, "the watcher to be dropped", func() bool { return w.drops() > 0 })
}

// TestWatchVerbRefusals. Each one is a protocol code, because a caller branches
// on the code and a transport maps it to its own status.
func TestWatchVerbRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   string
		args string
		want string
	}{
		{"watch with no pane", "watch", `{}`, protocol.CodeBadRequest},
		{"watch with bad args", "watch", `"nope"`, protocol.CodeBadRequest},
		{"watch with an unknown mode", "watch", `{"pane":1,"mode":"movie"}`, protocol.CodeBadRequest},
		{"unwatch with no pane", "unwatch", `{}`, protocol.CodeBadRequest},
		{"resync with no pane", "resync", `{}`, protocol.CodeBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, s, _ := subWithWatcher(t)
			var got string
			done := make(chan struct{})
			s.Call(context.Background(), tc.op, json.RawMessage(tc.args), func(_ map[string]any, err error) []byte {
				got = protocol.CodeOf(err)
				close(done)
				return nil
			})
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("no answer")
			}
			if got != tc.want {
				t.Errorf("code = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWatchWithoutAWatcherIsUnsupported: a hub with no streaming says so, in
// its own vocabulary, rather than failing as an internal error.
func TestWatchWithoutAWatcherIsUnsupported(t *testing.T) {
	h := New()
	sink := newFakeSink()
	s := h.Session(Caller{Transport: "socket", Conn: "sock#1"}, sink, nil)
	s.Start(nil)
	t.Cleanup(func() { s.kill("test over") })

	if _, err := s.Watch(1, protocol.WatchFrames, 15); protocol.CodeOf(err) != protocol.CodeUnsupported {
		t.Errorf("watch with no Watcher: %v (code %q), want unsupported", err, protocol.CodeOf(err))
	}
}
