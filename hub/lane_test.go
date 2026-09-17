package hub

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// recorder collects what lane items did, in the order they did it.
type recorder struct {
	mu  sync.Mutex
	log []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

// TestLaneRunsItemsInSubmissionOrder is the whole reason a lane exists. Two
// instructions to one pane from one connection are paced across hundreds of
// milliseconds each; without a lane the second starts while the first is still
// pausing before its Enter, and the two are typed into each other.
func TestLaneRunsItemsInSubmissionOrder(t *testing.T) {
	h := New()
	sub := h.Session(Caller{Conn: "a"}, newFakeSink(), nil)
	sub.Start(nil)
	defer sub.Close("done")

	var rec recorder
	for i := 0; i < 8; i++ {
		i := i
		if err := sub.Deliver(3, LaneItem{Run: func(context.Context) {
			rec.add(fmt.Sprintf("start %d", i))
			time.Sleep(2 * time.Millisecond)
			rec.add(fmt.Sprintf("end %d", i))
		}}); err != nil {
			t.Fatalf("Deliver %d: %v", i, err)
		}
	}
	waitFor(t, 5*time.Second, "all eight items to run", func() bool { return len(rec.seen()) == 16 })

	var want []string
	for i := 0; i < 8; i++ {
		want = append(want, fmt.Sprintf("start %d", i), fmt.Sprintf("end %d", i))
	}
	if got := strings.Join(rec.seen(), ","); got != strings.Join(want, ",") {
		t.Fatalf("lane ran %q; each item must complete before the next one starts, in submission order", got)
	}

	// A different pane is a different lane, and does not queue behind it.
	ran := make(chan struct{})
	if err := sub.Deliver(4, LaneItem{Run: func(context.Context) { close(ran) }}); err != nil {
		t.Fatalf("Deliver to a second pane: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("an item for a second pane never ran; a lane is per (Sub, pane)")
	}
}

// TestLaneDrainsAfterSubClose: README's one-shot client hangs up microseconds
// after its last line, with its second instruction still queued behind the
// first one's pacing. Dropping the queue on close would lose it silently.
func TestLaneDrainsAfterSubClose(t *testing.T) {
	h := New()
	sub := h.Session(Caller{Conn: "one-shot"}, newFakeSink(), nil)
	sub.Start(nil)

	var rec recorder
	release := make(chan struct{})
	if err := sub.Deliver(0, LaneItem{Run: func(context.Context) {
		<-release
		rec.add("first")
	}}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	waitFor(t, 2*time.Second, "the first item to start", func() bool {
		select {
		case release <- struct{}{}:
			return false
		default:
			return true
		}
	})
	if err := sub.Deliver(0, LaneItem{Run: func(context.Context) { rec.add("second") }}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	sub.Close("peer went away")
	close(release)
	waitFor(t, 5*time.Second, "the queued item to run after the connection closed", func() bool {
		return len(rec.seen()) == 2
	})
	if got := strings.Join(rec.seen(), ","); got != "first,second" {
		t.Fatalf("lane ran %q after close, want both items in order", got)
	}

	// Nothing NEW is accepted once the connection is gone.
	if err := sub.Deliver(0, LaneItem{Run: func(context.Context) { rec.add("late") }}); protocol.CodeOf(err) != protocol.CodeNotReady {
		t.Errorf("Deliver on a closed Sub gave %v (code %q), want not_ready", err, protocol.CodeOf(err))
	}
}

// TestQuiesceDiscardsQueuedLaneItems is the one thing that drops an item, and
// it says so: a discarded instruction has already been shown on the panel as an
// OUT row, and a row that is never answered is worse than one answered
// "discarded". Whatever is already running is cancelled and waited for.
func TestQuiesceDiscardsQueuedLaneItems(t *testing.T) {
	h := New()
	sub := h.Session(Caller{Conn: "a"}, newFakeSink(), nil)
	sub.Start(nil)
	defer sub.Close("done")

	var rec recorder
	started := make(chan struct{})
	if err := sub.Deliver(1, LaneItem{
		Run: func(ctx context.Context) {
			close(started)
			<-ctx.Done() // the stop check a paced delivery makes between keys
			rec.add("cancelled")
		},
		Discard: func() { rec.add("in-flight item was discarded") },
	}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	<-started
	for i := 0; i < 3; i++ {
		i := i
		if err := sub.Deliver(1, LaneItem{
			Run:     func(context.Context) { rec.add(fmt.Sprintf("ran %d", i)) },
			Discard: func() { rec.add(fmt.Sprintf("discarded %d", i)) },
		}); err != nil {
			t.Fatalf("Deliver %d: %v", i, err)
		}
	}

	start := time.Now()
	h.Quiesce(500 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("Quiesce took %v; it must return as soon as the cancelled item does", elapsed)
	}

	// Every outcome is already recorded when Quiesce returns — that is what
	// waiting for the in-flight item BUYS, and it is why `results` can be built
	// straight afterwards and be a report on a session that has stopped moving.
	// Their order among themselves is not a property: the cancellation lands on
	// the lane's goroutine while the discards run on Quiesce's.
	got := rec.seen()
	outcomes := map[string]bool{}
	for _, entry := range got {
		if strings.HasPrefix(entry, "ran ") {
			t.Errorf("a queued item RAN during shutdown: %q", entry)
		}
		outcomes[entry] = true
	}
	if len(got) != 4 {
		t.Fatalf("lane recorded %q; want the in-flight item cancelled and three discarded", got)
	}
	for _, want := range []string{"cancelled", "discarded 0", "discarded 1", "discarded 2"} {
		if !outcomes[want] {
			t.Errorf("no %q in %q", want, got)
		}
	}

	// After Quiesce there are no new lanes and no new items.
	if err := sub.Deliver(2, LaneItem{Run: func(context.Context) { rec.add("late") }}); protocol.CodeOf(err) != protocol.CodeNotReady {
		t.Errorf("Deliver after Quiesce gave %v (code %q), want not_ready", err, protocol.CodeOf(err))
	}
}

// TestLaneOverflowIsBusyAndLocal: a lane that is 256 deep belongs to a
// controller that has stopped reading its own answers. That ITEM is refused,
// with a code the caller can act on, and nothing else is affected.
func TestLaneOverflowIsBusyAndLocal(t *testing.T) {
	h := New()
	sub := h.Session(Caller{Conn: "a"}, newFakeSink(), nil)
	sub.Start(nil)
	defer sub.Close("done")

	block := make(chan struct{})
	defer close(block)
	if err := sub.Deliver(0, LaneItem{Run: func(context.Context) { <-block }}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var lastErr error
	queued := 0
	for i := 0; i < laneMaxItems+8; i++ {
		if err := sub.Deliver(0, LaneItem{Run: func(context.Context) {}}); err != nil {
			lastErr = err
			break
		}
		queued++
	}
	if protocol.CodeOf(lastErr) != protocol.CodeBusy {
		t.Fatalf("a full lane refused with %v (code %q), want busy", lastErr, protocol.CodeOf(lastErr))
	}
	if queued < laneMaxItems-1 {
		t.Errorf("the lane refused after only %d items, want about %d", queued, laneMaxItems)
	}
	// Another pane is untouched.
	ran := make(chan struct{})
	if err := sub.Deliver(1, LaneItem{Run: func(context.Context) { close(ran) }}); err != nil {
		t.Fatalf("a full lane refused a DIFFERENT pane: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the second pane's lane never ran")
	}
}
