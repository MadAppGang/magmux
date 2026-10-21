package pipeline

import (
	"runtime"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/registry"
)

func fleetOf(epoch time.Time, pids ...int) obs.Fleet {
	f := obs.Fleet{Epoch: epoch}
	for _, pid := range pids {
		f.Sessions = append(f.Sessions, obs.Session{
			ID:     "s" + string(rune('a'+pid%26)),
			PID:    pid,
			PaneID: obs.Ptr(pid),
		})
	}
	return f
}

func mustSubscribe(t *testing.T, h *Hub, name string, buffer int) *Subscription {
	t.Helper()
	sub, err := h.Subscribe(name, buffer)
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", name, err)
	}
	return sub
}

func mustReceive(t *testing.T, sub *Subscription, within time.Duration) Delivery {
	t.Helper()
	select {
	case d, ok := <-sub.C():
		if !ok {
			t.Fatalf("%s: the subscription closed", sub.Name())
		}
		return d
	case <-time.After(within):
		t.Fatalf("%s: no document within %v", sub.Name(), within)
		return Delivery{}
	}
}

// TestOneClonePerPublishIsSharedByEverySubscriber pins the decision the phase
// had to make explicitly: ONE deep copy per publish, handed to everybody,
// rather than one copy per sink.
//
// The assertion is identity, because that is the only way to tell the two
// designs apart from outside: per-sink clones would give these two
// subscribers different backing arrays. Documented in the package doc; if that
// decision is ever reversed, this test is the thing that must change with it,
// deliberately rather than by accident.
func TestOneClonePerPublishIsSharedByEverySubscriber(t *testing.T) {
	h := StartHub()
	defer h.Stop()

	a := mustSubscribe(t, h, "a", 0)
	b := mustSubscribe(t, h, "b", 0)

	original := fleetOf(time.Unix(1, 0), 11, 12)
	h.Publish(original, nil)

	da := mustReceive(t, a, 5*time.Second)
	db := mustReceive(t, b, 5*time.Second)

	if len(da.Doc.Sessions) != 2 || len(db.Doc.Sessions) != 2 {
		t.Fatalf("documents have %d and %d sessions, want 2", len(da.Doc.Sessions), len(db.Doc.Sessions))
	}
	if &da.Doc.Sessions[0] != &db.Doc.Sessions[0] {
		t.Error("each subscriber got its own copy; the design is one clone per publish, shared")
	}
	if &da.Doc.Sessions[0] == &original.Sessions[0] {
		t.Error("the subscribers were handed the publisher's own document, not a copy")
	}
	if da.Doc.Sessions[0].PaneID == original.Sessions[0].PaneID {
		t.Error("the copy is shallow: a pointer field is shared with the publisher's document")
	}
}

// TestThePublishersOwnDocumentCanKeepChanging is freeze point 2 from the
// publisher's side.
//
// A WRONG IMPLEMENTATION that sent the caller's value straight through passes
// every other test here — right up until the coordinator reuses its slice on
// the next cycle, at which point a document a sink is still rendering changes
// under it. The clone happens on the publisher's goroutine precisely so that
// this cannot even be a race.
func TestThePublishersOwnDocumentCanKeepChanging(t *testing.T) {
	h := StartHub()
	defer h.Stop()
	sub := mustSubscribe(t, h, "sink", 0)

	original := fleetOf(time.Unix(1, 0), 21)
	h.Publish(original, []registry.Problem{{Path: "p", Kind: registry.ProblemTorn, Detail: "first"}})

	d := mustReceive(t, sub, 5*time.Second)

	original.Sessions[0].PID = 999
	original.Sessions[0].Name = "renamed"
	*original.Sessions[0].PaneID = 999

	if d.Doc.Sessions[0].PID != 21 || d.Doc.Sessions[0].Name != "" || *d.Doc.Sessions[0].PaneID != 21 {
		t.Fatalf("the delivered document changed when the publisher changed its own: %+v",
			d.Doc.Sessions[0])
	}
	if len(d.Problems) != 1 || d.Problems[0].Detail != "first" {
		t.Fatalf("problems did not survive the publish: %+v", d.Problems)
	}
}

// TestTheHubRetainsNothingAfterAPublish is the part of the decision that a
// test can actually assert: the shared copy is unreachable from any writer,
// not merely conventionally read-only.
//
// A hub that cached the last document — for a "latest" accessor, or to replay
// to a late subscriber — would hand the second publish's subscriber something
// derived from the first, and a sink that mutated what it was given would
// corrupt a document the hub still held. There is no such accessor, and this
// pins that two publishes share nothing at all.
func TestTheHubRetainsNothingAfterAPublish(t *testing.T) {
	h := StartHub()
	defer h.Stop()
	sub := mustSubscribe(t, h, "sink", 0)

	h.Publish(fleetOf(time.Unix(1, 0), 31), nil)
	first := mustReceive(t, sub, 5*time.Second)

	// A badly behaved sink, doing the one thing freeze point 3 forbids.
	first.Doc.Sessions[0].PID = -1
	first.Doc.Epoch = time.Unix(999, 0)

	h.Publish(fleetOf(time.Unix(2, 0), 31), nil)
	second := mustReceive(t, sub, 5*time.Second)

	if second.Doc.Sessions[0].PID != 31 {
		t.Fatalf("the second document carries the first sink's damage: pid %d", second.Doc.Sessions[0].PID)
	}
	if !second.Doc.Epoch.Equal(time.Unix(2, 0)) {
		t.Fatalf("the second document's epoch is %v — the hub is replaying something it kept",
			second.Doc.Epoch)
	}
}

// TestASlowSubscriberDelaysNeitherThePublisherNorAnotherSubscriber is the rule
// the whole design rests on, measured.
//
// A WRONG IMPLEMENTATION with a blocking send does not fail an assertion here
// — it hangs, which is why the elapsed time is measured with a wide margin and
// why every receive has a deadline. The margin is deliberately enormous: the
// claim is "the publisher does not wait for a sink", and a sink that is asleep
// for two seconds is either invisible or it is not.
func TestASlowSubscriberDelaysNeitherThePublisherNorAnotherSubscriber(t *testing.T) {
	const publishes = 50
	const sinkSleep = 2 * time.Second

	h := StartHub()
	defer h.Stop()

	fast := mustSubscribe(t, h, "fast", publishes+1)
	slow := mustSubscribe(t, h, "slow", 1)

	// The slow sink takes one document and then goes away for a long time,
	// which is what a sink doing real work looks like.
	blocked := make(chan struct{})
	go func() {
		<-slow.C()
		close(blocked)
		time.Sleep(sinkSleep)
		for range slow.C() {
		}
	}()

	h.Publish(fleetOf(time.Unix(0, 0), 1), nil)
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow sink never received its first document; the fixture is wrong, not the hub")
	}
	<-fast.C()

	start := time.Now()
	for i := 1; i <= publishes; i++ {
		if !h.Publish(fleetOf(time.Unix(int64(i), 0), i), nil) {
			t.Fatalf("publish %d was refused", i)
		}
	}
	elapsed := time.Since(start)

	if elapsed > sinkSleep/4 {
		t.Errorf("%d publishes took %v while a sink was asleep for %v — the hub is blocking on it",
			publishes, elapsed, sinkSleep)
	}

	// And the fast subscriber saw every one of them, at the time they were
	// published rather than at the time the slow one woke up.
	for i := 1; i <= publishes; i++ {
		d := mustReceive(t, fast, 5*time.Second)
		if d.Doc.Sessions[0].PID != i {
			t.Fatalf("the fast sink received document %d in position %d", d.Doc.Sessions[0].PID, i)
		}
	}
	if total := time.Since(start); total > sinkSleep/2 {
		t.Errorf("the fast sink took %v to drain %d documents behind a sleeping sink", total, publishes)
	}
}

// TestDropsAreCountedAndPublished pins the other half of the rule: a drop is
// never silent.
//
// The count arrives two ways on purpose, and they are different numbers.
// Delivery.Dropped is the gap since the last document this sink actually
// received — the same vocabulary fswatch.Event.Dropped already uses, for the
// same reason — and SubscriberStats.Dropped is the lifetime total.
func TestDropsAreCountedAndPublished(t *testing.T) {
	h := StartHub()
	defer h.Stop()
	sub := mustSubscribe(t, h, "sink", 1)

	// One fills the buffer; the next three have nowhere to go.
	for i := 1; i <= 4; i++ {
		h.Publish(fleetOf(time.Unix(int64(i), 0), i), nil)
	}

	// Stats travels the same channel as the publishes, so this answer is
	// ordered after all four of them. Without it the test would be racing the
	// hub loop: draining the buffer early frees a slot that a publish still in
	// flight would fill, and the drop count would depend on scheduling.
	if mid := h.Stats(); len(mid) != 1 || mid[0].Dropped != 3 || mid[0].Delivered != 1 {
		t.Fatalf("after 4 publishes into a buffer of 1, stats are %+v", mid)
	}

	first := mustReceive(t, sub, 5*time.Second)
	if first.Doc.Sessions[0].PID != 1 {
		t.Fatalf("the buffered document is %d, want the first one", first.Doc.Sessions[0].PID)
	}
	if first.Dropped != 0 {
		t.Fatalf("the first document reports %d drops before it", first.Dropped)
	}

	h.Publish(fleetOf(time.Unix(5, 0), 5), nil)
	next := mustReceive(t, sub, 5*time.Second)
	if next.Dropped != 3 {
		t.Fatalf("the next delivered document reports %d drops, want the 3 that were thrown away",
			next.Dropped)
	}

	stats := h.Stats()
	if len(stats) != 1 {
		t.Fatalf("Stats reports %d subscribers, want 1", len(stats))
	}
	if stats[0].Name != "sink" {
		t.Errorf("Stats names the subscriber %q", stats[0].Name)
	}
	if stats[0].Dropped != 3 {
		t.Errorf("lifetime drops are %d, want 3", stats[0].Dropped)
	}
	if stats[0].Delivered != 2 {
		t.Errorf("deliveries are %d, want 2", stats[0].Delivered)
	}
}

// TestADroppedSubscriberKeepsItsSubscription — dropped from THAT broadcast,
// not unsubscribed. A sink that fell behind once must go on receiving, or a
// transient stall becomes a permanent silence and nothing says why.
func TestADroppedSubscriberKeepsItsSubscription(t *testing.T) {
	h := StartHub()
	defer h.Stop()
	sub := mustSubscribe(t, h, "sink", 1)

	for i := 1; i <= 5; i++ {
		h.Publish(fleetOf(time.Unix(int64(i), 0), i), nil)
	}
	// The same ordering barrier as above, and it is load-bearing rather than
	// belt-and-braces: a publish is asynchronous, so draining the buffer
	// without knowing the five have been processed frees a slot that one of
	// them fills — and then the document published below is the one that gets
	// dropped, and the test waits for it forever. Stats travels the hub's own
	// channel, so its answer is ordered after all five.
	if mid := h.Stats(); len(mid) != 1 || mid[0].Dropped != 4 {
		t.Fatalf("after 5 publishes into a buffer of 1, stats are %+v", mid)
	}
	mustReceive(t, sub, 5*time.Second) // drain the one it kept

	h.Publish(fleetOf(time.Unix(9, 0), 9), nil)
	d := mustReceive(t, sub, 5*time.Second)
	if d.Doc.Sessions[0].PID != 9 {
		t.Fatalf("after dropping, the sink received %d rather than the document published next",
			d.Doc.Sessions[0].PID)
	}
	if d.Dropped != 4 {
		t.Errorf("the document after the gap reports %d drops, want 4", d.Dropped)
	}
}

// TestUnsubscribeClosesOnlyThatSubscription — and closes it, so a sink ranging
// over its channel returns rather than blocking forever.
func TestUnsubscribeClosesOnlyThatSubscription(t *testing.T) {
	h := StartHub()
	defer h.Stop()
	going := mustSubscribe(t, h, "going", 0)
	staying := mustSubscribe(t, h, "staying", 0)

	h.Unsubscribe(going)
	select {
	case _, ok := <-going.C():
		if ok {
			t.Fatal("an unsubscribed sink received a document")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Unsubscribe did not close the subscription")
	}

	h.Publish(fleetOf(time.Unix(1, 0), 1), nil)
	if d := mustReceive(t, staying, 5*time.Second); d.Doc.Sessions[0].PID != 1 {
		t.Fatal("the remaining sink stopped receiving")
	}
	// Twice, and on a nil, must both be harmless.
	h.Unsubscribe(going)
	h.Unsubscribe(nil)

	if stats := h.Stats(); len(stats) != 1 || stats[0].Name != "staying" {
		t.Fatalf("Stats still reports %+v after an unsubscribe", stats)
	}
}

// TestStopClosesEverySubscriptionAndLeaksNoGoroutine — the shutdown barrier's
// hub half. A sink ranging over its channel is finished by the time Stop
// returns, which is what stops one being mid-write when the process exits.
func TestStopClosesEverySubscriptionAndLeaksNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()

	h := StartHub()
	a := mustSubscribe(t, h, "a", 0)
	b := mustSubscribe(t, h, "b", 0)
	h.Publish(fleetOf(time.Unix(1, 0), 1), nil)
	mustReceive(t, a, 5*time.Second)
	mustReceive(t, b, 5*time.Second)

	h.Stop()
	h.Stop() // idempotent

	for _, sub := range []*Subscription{a, b} {
		select {
		case _, ok := <-sub.C():
			if ok {
				t.Fatalf("%s received a document after Stop", sub.Name())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not closed by Stop", sub.Name())
		}
	}

	if _, err := h.Subscribe("late", 0); err == nil {
		t.Error("subscribing to a stopped hub reported success, which is indistinguishable " +
			"from a subscription that has simply seen nothing yet")
	}
	if h.Publish(fleetOf(time.Unix(2, 0), 2), nil) {
		t.Error("publishing to a stopped hub reported success")
	}
	if stats := h.Stats(); stats != nil {
		t.Errorf("a stopped hub reported stats: %+v", stats)
	}

	if after := settledGoroutines(before, 2*time.Second); after > before {
		t.Errorf("goroutines went from %d to %d across the hub's whole life", before, after)
	}
}
