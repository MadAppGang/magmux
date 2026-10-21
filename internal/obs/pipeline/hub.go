package pipeline

import (
	"fmt"
	"slices"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/registry"
)

// Delivery is one broadcast: the document, everything that could not be read
// while taking it, and how far this particular subscriber has fallen behind.
type Delivery struct {
	// Doc is the document as of Doc.Epoch. It is SHARED with every other
	// subscriber of this broadcast — see the package doc for why one clone and
	// not N — so it is read-only to a sink. Freeze point 3: a sink may not
	// retain a pointer into it past its own send.
	Doc obs.Fleet

	// Problems is everything the source could not read while building Doc.
	// It rides with the document rather than being logged, because a source
	// that silently skips a record it could not read is indistinguishable from
	// a machine with fewer sessions on it, and the difference between those
	// two is the entire value of the report.
	Problems []registry.Problem

	// Dropped is how many documents were thrown away for THIS subscriber,
	// because it was not reading, between the previous one it received and
	// this one. Carried on the next delivered value rather than reported
	// separately, exactly as fswatch.Event.Dropped is, so a sink that fell
	// behind learns how far and cannot mistake a gap for a quiet machine.
	//
	// It is a per-gap number. The lifetime total is in
	// [SubscriberStats.Dropped].
	Dropped int
}

// SubscriberStats is one subscriber's record, answered by [Hub.Stats].
type SubscriberStats struct {
	// Name is what the subscriber called itself. Free text, for diagnostics.
	Name string
	// Delivered is how many documents it has received.
	Delivered int
	// Dropped is how many were dropped for it over its whole life — the
	// lifetime total, where [Delivery.Dropped] is the count since the last
	// delivered document.
	Dropped int
	// Pending is how many delivered documents are still sitting in its
	// channel unread. A number that stays near the buffer size is a sink that
	// is about to start dropping.
	Pending int
}

// Subscription is a sink's end of the hub. Read [Subscription.C] until it is
// closed, which happens when the subscription is cancelled or the hub stops.
type Subscription struct {
	name string
	c    chan Delivery
}

// Name is what this subscriber called itself.
func (s *Subscription) Name() string { return s.name }

// C yields documents. It is CLOSED when the subscription ends, so a sink may
// range over it — and a sink that does is its own shutdown barrier: the range
// returns only after the hub has stopped and will send nothing further.
func (s *Subscription) C() <-chan Delivery { return s.c }

// Hub broadcasts documents to sinks and never blocks on one.
//
// # Freeze point 2
//
// The document is deep-copied ONCE per publish, on the publisher's goroutine,
// before it crosses the channel into the hub — so a publisher that keeps and
// mutates its own document cannot race the copy. The hub then hands that one
// copy to every subscriber and retains no reference to it. There is
// deliberately no "latest document" accessor: the copy the sinks share is
// unreachable from any writer, which is stronger than conventionally
// read-only and is the part a test can assert.
//
// # Ownership
//
// One goroutine owns the subscriber list, the delivery counts and the drop
// counts. None of them is a field anything outside can reach, and the only
// ways in are the methods below — all of which are messages on one channel, so
// a subscribe that returns is always visible to a publish that follows it.
type Hub struct {
	in   chan hubMessage
	done chan struct{}
}

type hubMessageKind int

const (
	hubPublish hubMessageKind = iota
	hubSubscribe
	hubUnsubscribe
	hubStats
	hubStop
)

type hubMessage struct {
	kind     hubMessageKind
	delivery Delivery
	name     string
	buffer   int
	sub      *Subscription
	reply    chan hubReply
}

type hubReply struct {
	sub   *Subscription
	stats []SubscriberStats
}

// hubBacklog absorbs a burst of publishes. The hub loop never waits on
// anybody, so this only ever covers the interval between a publisher handing a
// document over and the loop picking it up.
const hubBacklog = 4

// DefaultSubscriberBuffer is how many documents a subscriber may fall behind
// by before it starts losing them.
//
// Small on purpose. A sink's job is to render or forward a document; one that
// is four documents behind is not going to catch up, and a deep buffer would
// only delay the moment it is told so while holding four copies of the
// document alive. The drop is the signal, and it is counted.
const DefaultSubscriberBuffer = 4

// StartHub starts the broadcast goroutine. The caller owns the result and must
// [Hub.Stop] it.
func StartHub() *Hub {
	h := &Hub{
		in:   make(chan hubMessage, hubBacklog),
		done: make(chan struct{}),
	}
	go h.loop()
	return h
}

// Publish broadcasts one document.
//
// The clone happens here, on the caller's goroutine, and it is the only copy
// made: every subscriber receives the same [obs.Fleet].
//
// This call waits for the hub loop to take the document and no longer. That is
// safe — and is the other half of the rule — precisely because the loop itself
// never waits on a subscriber: the longest it can be busy is one non-blocking
// send per sink.
func (h *Hub) Publish(doc obs.Fleet, problems []registry.Problem) bool {
	d := Delivery{
		Doc: doc.Clone(),
		// Problems hold nothing but strings and ints, so the slice copy is the
		// deep copy. Cloned for the same reason the document is: the publisher
		// reuses its own slice on the next cycle.
		Problems: slices.Clone(problems),
	}
	select {
	case <-h.done:
		return false
	default:
	}
	select {
	case h.in <- hubMessage{kind: hubPublish, delivery: d}:
		return true
	case <-h.done:
		return false
	}
}

// Subscribe registers a sink. buffer is how many documents it may fall behind
// by; non-positive takes [DefaultSubscriberBuffer].
//
// It returns an error only when the hub has stopped, which a sink needs to
// tell apart from "subscribed and nothing has happened yet" — the two look
// identical from a channel that never yields.
func (h *Hub) Subscribe(name string, buffer int) (*Subscription, error) {
	if buffer <= 0 {
		buffer = DefaultSubscriberBuffer
	}
	r, ok := h.ask(hubMessage{kind: hubSubscribe, name: name, buffer: buffer})
	if !ok {
		return nil, fmt.Errorf("pipeline: the hub has stopped, so %q cannot subscribe", name)
	}
	return r.sub, nil
}

// Unsubscribe removes a sink and closes its channel. It is safe to call twice
// and safe to call after the hub has stopped; a sink that is ranging over its
// channel sees the close and returns.
func (h *Hub) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}
	h.ask(hubMessage{kind: hubUnsubscribe, sub: sub})
}

// Stats reports what every current subscriber has received and lost. It is a
// command with a reply like every other read of hub-owned state; see the
// package doc for why this is not a field.
func (h *Hub) Stats() []SubscriberStats {
	r, ok := h.ask(hubMessage{kind: hubStats})
	if !ok {
		return nil
	}
	return r.stats
}

// Stop shuts the hub down, closing every subscription. It is idempotent and
// may be called from any goroutine, and it returns only once the broadcast
// goroutine has finished — so no send to any sink is in flight after it
// returns. That is the shutdown barrier a caller needs before the process
// exits.
func (h *Hub) Stop() {
	select {
	case h.in <- hubMessage{kind: hubStop}:
	case <-h.done:
	}
	<-h.done
}

func (h *Hub) ask(m hubMessage) (hubReply, bool) {
	m.reply = make(chan hubReply, 1)
	select {
	case <-h.done:
		return hubReply{}, false
	default:
	}
	select {
	case h.in <- m:
	case <-h.done:
		return hubReply{}, false
	}
	select {
	case r := <-m.reply:
		return r, true
	case <-h.done:
		return hubReply{}, false
	}
}

// subscriber is the hub's own record of a sink. It lives on the hub goroutine
// and nothing else can reach it.
type subscriber struct {
	sub       *Subscription
	delivered int
	dropped   int
	// pending is what Delivery.Dropped will carry on the next successful
	// send: the drops since the last one that got through.
	pending int
}

func (h *Hub) loop() {
	defer close(h.done)

	var subs []*subscriber

	for m := range h.in {
		switch m.kind {
		case hubPublish:
			for _, s := range subs {
				d := m.delivery
				d.Dropped = s.pending
				select {
				case s.sub.c <- d:
					s.delivered++
					s.pending = 0
				default:
					// Dropped from THIS broadcast, not unsubscribed. The sink
					// keeps its place and is told how many it missed on the
					// next one it does receive.
					s.dropped++
					s.pending++
				}
			}
			// The hub's reference to the document ends here, with this
			// iteration. Nothing is cached, which is what makes the shared
			// copy unreachable from any writer.

		case hubSubscribe:
			sub := &Subscription{name: m.name, c: make(chan Delivery, m.buffer)}
			subs = append(subs, &subscriber{sub: sub})
			m.reply <- hubReply{sub: sub}

		case hubUnsubscribe:
			for i, s := range subs {
				if s.sub == m.sub {
					subs = append(subs[:i], subs[i+1:]...)
					// Removed from the list BEFORE the close, so no send can
					// race it: the only sender is this goroutine.
					close(s.sub.c)
					break
				}
			}
			m.reply <- hubReply{}

		case hubStats:
			stats := make([]SubscriberStats, 0, len(subs))
			for _, s := range subs {
				stats = append(stats, SubscriberStats{
					Name:      s.sub.name,
					Delivered: s.delivered,
					Dropped:   s.dropped,
					Pending:   len(s.sub.c),
				})
			}
			m.reply <- hubReply{stats: stats}

		case hubStop:
			for _, s := range subs {
				close(s.sub.c)
			}
			return
		}
	}
}
