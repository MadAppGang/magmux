package hub

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSink is a Sink with a net.Conn's manners: a write can be made to block,
// SetWriteDeadline interrupts a write that is ALREADY blocked, and Close
// interrupts it too. Anything less would let a test pass against a hub that
// only works on a peer which never stalls, which is the one peer that does not
// need any of this machinery.
type fakeSink struct {
	mu sync.Mutex
	// blockWrites is how many of the first writes block. They are released by
	// closing release, by the deadline, or by Close.
	blockWrites int
	release     chan struct{}
	// tornBytes is what a failed write reports as n. A plain socket reports 0;
	// an SSE or TLS sink reports the whole message, because by the time it
	// knows, part of it may be on the wire.
	tornBytes int

	deadline  time.Time
	dlChanged chan struct{}
	closedCh  chan struct{}

	msgs   []string
	writes int
	closed bool
	reason string
}

func newFakeSink() *fakeSink {
	return &fakeSink{
		release:   make(chan struct{}),
		dlChanged: make(chan struct{}),
		closedCh:  make(chan struct{}),
	}
}

func (f *fakeSink) Write(b []byte) (int, error) {
	f.mu.Lock()
	f.writes++
	blocking := f.writes <= f.blockWrites
	f.mu.Unlock()

	for blocking {
		f.mu.Lock()
		dl, changed := f.deadline, f.dlChanged
		f.mu.Unlock()
		var timer <-chan time.Time
		if !dl.IsZero() {
			t := time.NewTimer(time.Until(dl))
			timer = t.C
			defer t.Stop()
		}
		select {
		case <-f.release:
			blocking = false
		case <-f.closedCh:
			return f.fail(b), fmt.Errorf("sink closed")
		case <-timer:
			return f.fail(b), os.ErrDeadlineExceeded
		case <-changed:
			// A new deadline: recompute and keep waiting.
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, string(b))
	return len(b), nil
}

// fail is what a failed write reports as n: 0 for a line-aligned refusal,
// the message length for a torn one.
func (f *fakeSink) fail(b []byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tornBytes > 0 {
		return min(f.tornBytes, len(b))
	}
	return 0
}

func (f *fakeSink) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	f.deadline = t
	close(f.dlChanged)
	f.dlChanged = make(chan struct{})
	f.mu.Unlock()
	return nil
}

func (f *fakeSink) Close(reason string) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.reason = reason
	f.mu.Unlock()
	close(f.closedCh)
}

func (f *fakeSink) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

func (f *fakeSink) closedWith() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed, f.reason
}

func waitDone(t *testing.T, s *Sub, within time.Duration, what string) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(within):
		t.Fatalf("%s: the Sub was still open after %v", what, within)
	}
}

// waitFor polls until cond holds, or fails. Used instead of a sleep so the
// tests below assert on a state rather than on a guess about scheduling.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

// TestPublishNeverBlocksOnAStalledSink is N1 in one assertion: the render loop
// publishes, and a subscriber that has stopped reading must cost it nothing.
// Today's broadcastEvent writes to every client synchronously under one lock,
// so this is the property the bus exists to add.
func TestPublishNeverBlocksOnAStalledSink(t *testing.T) {
	h := New()
	sink := newFakeSink()
	sink.blockWrites = 1 << 30 // never completes on its own
	sub := h.Session(Caller{Conn: "stalled"}, sink, nil)
	sub.Start([]byte("head\n"))
	waitFor(t, 2*time.Second, "the writer to block inside Write", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.writes > 0
	})

	start := time.Now()
	for i := 0; i < 4000; i++ {
		h.Publish([]byte(fmt.Sprintf("{\"n\":%d}\n", i)))
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("4,000 publishes into a stalled subscriber took %v; Publish blocked", elapsed)
	}

	// Past its cap, that subscriber — and only it — is closed.
	waitDone(t, sub, 5*time.Second, "a subscriber that overflowed its queue")
	closed, reason := sink.closedWith()
	if !closed || reason != "slow_consumer" {
		t.Fatalf("stalled sink closed=%v reason=%q, want a slow_consumer close", closed, reason)
	}
}

// TestOverflowClosesOnlyThatSubscriber is the other half of N1: one wedged
// peer must not cost a healthy one a single message. The messages are large so
// the BYTE cap is what trips, which keeps the test to a dozen publishes and
// out of the reader's way.
func TestOverflowClosesOnlyThatSubscriber(t *testing.T) {
	h := New()
	stalled, live := newFakeSink(), newFakeSink()
	stalled.blockWrites = 1 << 30
	slow := h.Session(Caller{Conn: "stalled"}, stalled, nil)
	good := h.Session(Caller{Conn: "live"}, live, nil)
	slow.Start(nil)
	good.Start(nil)

	big := strings.Repeat("x", 1<<20) + "\n" // 1 MiB, against an 8 MiB cap
	const n = 16
	for i := 0; i < n; i++ {
		h.Publish([]byte(big))
		// Pace against the healthy subscriber so IT cannot overflow: this test
		// is about the stalled one, and a race between the publisher and a
		// working writer would prove nothing either way.
		want := i + 1
		waitFor(t, 5*time.Second, "the healthy subscriber to drain", func() bool {
			return len(live.seen()) >= want
		})
	}

	waitDone(t, slow, 5*time.Second, "the stalled subscriber")
	if closed, reason := stalled.closedWith(); !closed || reason != "slow_consumer" {
		t.Fatalf("stalled sink closed=%v reason=%q, want slow_consumer", closed, reason)
	}
	if got := len(live.seen()); got != n {
		t.Errorf("the healthy subscriber received %d of %d messages", got, n)
	}
	if closed, _ := live.closedWith(); closed {
		t.Error("the healthy subscriber was closed as well; overflow must be per subscriber")
	}
	if h.Subs() != 1 {
		t.Errorf("hub has %d subscribers, want 1 (the closed one must be dropped)", h.Subs())
	}
}

// TestFinalizeReplayOrder is the ordering guarantee every integrator relies
// on: the aggregate first, then results, then shutdown, then EOF — for a
// subscriber that was already connected, for one that connects while teardown
// is under way, and with the backlog discarded rather than drained.
func TestFinalizeReplayOrder(t *testing.T) {
	h := New()
	sink := newFakeSink()
	sub := h.Session(Caller{Conn: "a"}, sink, nil)
	sub.Start([]byte("aggregate\n"))
	h.Publish([]byte("event\n"))
	waitFor(t, 2*time.Second, "the aggregate and the event to be written", func() bool {
		return len(sink.seen()) == 2
	})

	h.Finalize([]byte("results\n"), []byte("shutdown\n"))
	waitDone(t, sub, 3*time.Second, "a live subscriber at Finalize")

	got := sink.seen()
	want := []string{"aggregate\n", "event\n", "results\n", "shutdown\n"}
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Fatalf("stream was %q, want %q", got, want)
	}
	if closed, _ := sink.closedWith(); !closed {
		t.Error("the sink was never closed, so the subscriber never gets its EOF")
	}

	// A connection that arrives after Finalize is not registered for anything:
	// it is handed its aggregate, then the same two finals, then closed.
	lateSink := newFakeSink()
	late := h.Session(Caller{Conn: "late"}, lateSink, nil)
	if h.Subs() != 0 {
		t.Errorf("a post-Finalize Session was registered as a subscriber (%d subs); nothing will ever be published to it", h.Subs())
	}
	late.Start([]byte("late-aggregate\n"))
	waitDone(t, late, 3*time.Second, "a subscriber that arrived after Finalize")
	if got := strings.Join(lateSink.seen(), ""); got != "late-aggregate\nresults\nshutdown\n" {
		t.Fatalf("late subscriber saw %q, want the aggregate then results then shutdown", got)
	}
}

// TestFinalizeJumpsBacklog: results supersedes every snapshot queued behind
// it, so a subscriber a thousand events behind gets the ANSWER rather than the
// history. The second case lands Finalize between registration and Start, and
// the first line must still be the aggregate — that is what the
// non-discardable head is for.
func TestFinalizeJumpsBacklog(t *testing.T) {
	h := New()
	sink := newFakeSink()
	sink.blockWrites = 1
	sub := h.Session(Caller{Conn: "behind"}, sink, nil)
	sub.Start([]byte("aggregate\n"))
	waitFor(t, 2*time.Second, "the writer to block on the aggregate", func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.writes == 1
	})
	for i := 0; i < 1000; i++ {
		h.Publish([]byte(fmt.Sprintf("{\"n\":%d}\n", i)))
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(sink.release)
	}()
	start := time.Now()
	h.Finalize([]byte("results\n"), []byte("shutdown\n"))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Finalize took %v with 1,000 events queued; the backlog must be discarded, not drained", elapsed)
	}
	waitDone(t, sub, time.Second, "the backlogged subscriber")
	if got := strings.Join(sink.seen(), ""); got != "aggregate\nresults\nshutdown\n" {
		t.Fatalf("backlogged subscriber saw %q, want the aggregate then the two finals", got)
	}

	// Finalize between register and Start.
	h2 := New()
	s2 := newFakeSink()
	sub2 := h2.Session(Caller{Conn: "mid-cut"}, s2, nil)
	h2.Publish([]byte("event\n"))
	go h2.Finalize([]byte("results\n"), []byte("shutdown\n"))
	time.Sleep(20 * time.Millisecond)
	sub2.Start([]byte("aggregate\n"))
	waitDone(t, sub2, 3*time.Second, "a subscriber started during Finalize")
	if got := strings.Join(s2.seen(), ""); got != "aggregate\nresults\nshutdown\n" {
		t.Fatalf("subscriber started during Finalize saw %q; its first line must still be the aggregate", got)
	}
}

// TestFinalizeCutsBlockedWrite is the torn-write rule. A peer stuck inside one
// write is cut at T0+500ms; whether the finals may follow depends on whether
// any byte of that message left the process, because a final spliced into a
// partial line corrupts both.
func TestFinalizeCutsBlockedWrite(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tornBytes int
		want      string
	}{
		{"line-aligned refusal still gets the finals", 0, "results\nshutdown\n"},
		{"torn write gets EOF and no finals", 4, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New()
			sink := newFakeSink()
			sink.blockWrites = 1
			sink.tornBytes = tc.tornBytes
			sub := h.Session(Caller{Conn: "stuck"}, sink, nil)
			sub.Start([]byte("aggregate\n"))
			waitFor(t, 2*time.Second, "the writer to block", func() bool {
				sink.mu.Lock()
				defer sink.mu.Unlock()
				return sink.writes == 1
			})

			start := time.Now()
			h.Finalize([]byte("results\n"), []byte("shutdown\n"))
			elapsed := time.Since(start)
			if elapsed < 400*time.Millisecond {
				t.Errorf("Finalize returned in %v; the in-flight write gets its 500ms cut", elapsed)
			}
			if elapsed > 2500*time.Millisecond {
				t.Errorf("Finalize took %v; everything after the cut is bounded by one absolute deadline", elapsed)
			}
			waitDone(t, sub, time.Second, "the cut subscriber")
			if got := strings.Join(sink.seen(), ""); got != tc.want {
				t.Fatalf("subscriber saw %q, want %q", got, tc.want)
			}
			if closed, _ := sink.closedWith(); !closed {
				t.Error("the sink was never closed")
			}
		})
	}
}

// TestSubCallerResolvesPluginPerCall: a connection registers as a plugin AFTER
// its Sub exists, so an identity captured at Session time is the one identity
// that is always wrong.
func TestSubCallerResolvesPluginPerCall(t *testing.T) {
	h := New()
	name := ""
	sub := h.Session(Caller{Transport: "socket", Conn: "sock#1"}, newFakeSink(), func() string { return name })
	defer sub.Close("done")
	if got := sub.Caller().Plugin; got != "" {
		t.Errorf("Plugin = %q before registration, want empty", got)
	}
	name = "ticket"
	if got := sub.Caller().Plugin; got != "ticket" {
		t.Errorf("Plugin = %q after registration, want ticket", got)
	}
	if got := sub.Caller().Conn; got != "sock#1" {
		t.Errorf("Conn = %q, want sock#1", got)
	}
}

// TestSubDrainsQueuedSendAfterClose: a reply queued before the adapter closed
// the Sub is still written. A one-shot client that hangs up immediately is a
// documented shape (README's `nc -U`), and losing its last message silently is
// the failure that shape is most likely to hit.
func TestSubDrainsQueuedSendAfterClose(t *testing.T) {
	h := New()
	sink := newFakeSink()
	sub := h.Session(Caller{Conn: "one-shot"}, sink, nil)
	sub.Start(nil)
	sub.Send([]byte("reply\n"))
	sub.Close("peer went away")
	waitDone(t, sub, 2*time.Second, "a closed subscriber")
	if got := strings.Join(sink.seen(), ""); got != "reply\n" {
		t.Fatalf("subscriber saw %q, want the reply that was already queued", got)
	}
	if h.Subs() != 0 {
		t.Errorf("hub still holds %d subscribers after close", h.Subs())
	}
}
