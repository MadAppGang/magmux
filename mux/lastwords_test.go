package mux

// The quitting render's events must reach every subscriber, ahead of results.
//
// The bug (issue #2, back through the hub; red on macOS CI for weeks as
// TestControllerSnapshotReachesAwaitingInput): render published the final
// awaiting_input snapshot and then closed m.quit, and the teardown's Finalize
// discards every subscriber's backlog so results can jump it. Whenever the
// subscriber's writer had not yet reached the snapshot — a fast teardown made
// that routine — it was discarded, and the live stream ended on "starting"
// while results said the pane was idle. Here the writer is HELD on the
// aggregate across the whole teardown, which is that race made certain.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
)

// heldSink blocks its first write until release closes, and records lines.
type heldSink struct {
	inWrite chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	lines   []string
}

func (s *heldSink) Write(b []byte) (int, error) {
	s.once.Do(func() {
		close(s.inWrite)
		<-s.release
	})
	s.mu.Lock()
	s.lines = append(s.lines, strings.TrimSuffix(string(b), "\n"))
	s.mu.Unlock()
	return len(b), nil
}
func (s *heldSink) SetWriteDeadline(time.Time) error { return nil }
func (s *heldSink) Close(string)                     {}

// idleController reports awaiting_input on every poll.
type idleController struct{}

func (idleController) Name() string                { return "idle" }
func (idleController) Start(context.Context) error { return nil }
func (idleController) Stop() error                 { return nil }
func (idleController) Poll() (Snapshot, error)     { return Snapshot{State: CtrlAwaitingInput}, nil }

func TestQuittingRendersEventsReachSubscribersAheadOfResults(t *testing.T) {
	defer deadlineWatchdog(t, 30*time.Second)()

	m := newTestMux(t, ctrlPanes(1)...)
	m.out = io.Discard
	m.autoExit = true
	// One finished session with a controller: done, so -w quits on this
	// render, and the final unthrottled poll yields the awaiting_input
	// snapshot. No process (cmd nil) is "done" without a reap.
	p := m.paneByID(0)
	p.mu.Lock()
	p.isControl = false
	p.dead = true
	p.mu.Unlock()
	p.controller = idleController{}

	sink := &heldSink{inWrite: make(chan struct{}), release: make(chan struct{})}
	sub := m.bus().Session(hub.Caller{Conn: "held"}, sink, nil)
	sub.Start([]byte("aggregate\n"))
	select {
	case <-sink.inWrite:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never started on the aggregate")
	}

	m.render()
	select {
	case <-m.quit:
	default:
		t.Fatal("render did not decide to quit; the fixture is wrong, not the fix")
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(sink.release)
	}()
	m.shutdownSocket()
	select {
	case <-sub.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber never finished")
	}

	sink.mu.Lock()
	lines := append([]string(nil), sink.lines...)
	sink.mu.Unlock()
	snapAt, resultsAt := -1, -1
	for i, l := range lines {
		var ev map[string]any
		if json.Unmarshal([]byte(l), &ev) != nil {
			continue
		}
		switch {
		case ev["type"] == "snapshot" && ev["state"] == "awaiting_input" && snapAt < 0:
			snapAt = i
		case ev["type"] == "results" && resultsAt < 0:
			resultsAt = i
		}
	}
	if snapAt < 0 {
		t.Fatalf("the final awaiting_input snapshot never reached the subscriber (issue #2); it saw:\n%s",
			strings.Join(lines, "\n"))
	}
	if resultsAt < 0 || snapAt > resultsAt {
		t.Fatalf("the snapshot must arrive BEFORE results (snapshot at %d, results at %d):\n%s",
			snapAt, resultsAt, strings.Join(lines, "\n"))
	}
}
