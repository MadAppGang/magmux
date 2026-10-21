package mux

// The sink side of the observation layer: the one subscriber magmux runs, the
// value it keeps, and the socket verb that publishes it.
//
// # One sink, two hand-offs, and why it is one and not two
//
// The hub supports any number of subscribers and this could have been two: one
// holding the document for the socket verb, one feeding the panel. It is one,
// because both hand-offs are a lock acquisition and a value copy, they always
// want the SAME document, and two sinks would let the panel and the socket
// answer differently about the same instant for no benefit at all. The drop
// accounting names the sink, so one entry in Hub.Stats is one honest number
// rather than two that have to be reconciled.
//
// # The rule this file exists to obey
//
// architecture-v4 §2:
//
//	The hub never blocks on a subscriber, which is what makes it safe for a
//	subscriber to block on the hub.
//
// Both halves are live here. This sink blocks on its subscription — it ranges
// over the channel, which is the whole of its main loop — and that is only
// safe because the other direction never waits. The way to break it from THIS
// side is to hold a lock across the receive: cp.mu is taken by the render
// loop under treeMu.RLock, so a sink parked in `for range` while holding cp.mu
// stops every frame magmux draws. So the shape below is not a style: derive on
// this goroutine, take each lock, copy, release, and never hold one across a
// channel operation.
//
// Freeze point 3 is the other half: the library's hub hands every subscriber its
// own deep copy of the document, and this sink keeps the WIRE form of it —
// obsview.FromModel, the one mapping every surface uses — derived on this
// goroutine before anything is stored.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/obsview"
)

// obsStore is the newest document, for the surfaces that answer a question.
//
// # Why magmux keeps one at all, when the pipeline deliberately does not
//
// The pipeline's hub has no "latest document" accessor for a socket goroutine:
// the only reader of the coordinator's state is Exec, which serialises behind
// every cycle, and a socket verb must not queue behind one. A socket verb
// still has to answer NOW, with the last thing that was published, so the
// holding is done on THIS side of the hub by an ordinary subscriber — which
// is exactly the shape the hub's contract invites, rather than an exception
// to it.
//
// # Locking
//
// Its own mutex and nothing else. It never takes treeMu or cp.mu and is never
// held across a channel operation, so it cannot participate in any ordering
// question: the mutex is taken, a value is copied, and it is released before
// the caller does anything at all with what it got.
type obsStore struct {
	mu sync.Mutex

	// running is whether the observation pipeline is up in this magmux.
	//
	// It is here rather than read off Magmux.obs because that field is
	// documented as Main()'s goroutine only, and the socket verb is not on
	// Main()'s goroutine. "Observation is off" and "observation is on and has
	// published nothing yet" are different answers with different remedies —
	// set MAGMUX_AGENTS, versus wait — so both are kept.
	running bool

	have bool
	doc  obsview.Fleet
	at   time.Time

	// dropped is how many documents the hub threw away for this sink over its
	// whole life. A non-zero value means the document below may be older than
	// the newest one published, and a surface that shows the document should
	// be able to say so.
	dropped int
}

// start marks observation as up. Called before the sink's first delivery, so
// that a query arriving in between is answered "nothing yet" rather than "off".
func (s *obsStore) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = true
	s.have = false
	s.doc = obsview.Fleet{}
	s.dropped = 0
}

// stop marks observation as down and DROPS the document.
//
// Dropped rather than kept, because the document's whole value is that it is
// current: a stopped pipeline's last document keeps aging and nothing will
// ever correct it, and a surface that served it would be presenting a figure
// as durable when it is an instant — requirements.md R2.8's error, in the one
// place magmux would be making it on its own behalf.
func (s *obsStore) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.have = false
	s.doc = obsview.Fleet{}
}

// set stores one delivery. The document must already be this sink's own copy.
func (s *obsStore) set(doc obsview.Fleet, at time.Time, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = doc
	s.at = at
	s.have = true
	s.dropped += dropped
}

// obsSnapshot is what a surface gets: the document, when it arrived, and the
// two facts that say whether it means anything.
type obsSnapshot struct {
	Doc     obsview.Fleet
	At      time.Time
	Running bool
	Have    bool
	Dropped int
}

// snapshot returns a copy. The clone is taken under the lock and the caller
// marshals it outside, so a socket goroutine encoding thirty sessions cannot
// hold up the sink.
func (s *obsStore) snapshot() obsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return obsSnapshot{
		Doc:     s.doc.Clone(),
		At:      s.at,
		Running: s.running,
		Have:    s.have,
		Dropped: s.dropped,
	}
}

// startObsSink subscribes the chrome sink and starts its goroutine.
//
// The subscription is made by the CALLER's goroutine before the goroutine
// starts, and the pipeline is refreshed after — subscribe first, then ask, is
// the order everywhere in this layer, because nothing is replayed and a
// document published before anybody subscribed is a document nobody receives.
func (m *Magmux) startObsSink(p *pipeline.Pipeline) error {
	sub, err := p.Subscribe(context.Background(), "chrome", 0)
	if err != nil {
		return err
	}
	m.obsDoc.start()
	done := make(chan struct{})
	m.obsSinkDone = done
	go func() {
		defer close(done)
		m.obsSinkLoop(sub)
	}()
	return nil
}

// obsSinkLoop is the sink. It ends when the subscription is closed, which
// Pipeline.Stop does after the last document.
//
// Every line of it is deliberate about locks. The derivation happens first,
// holding nothing; then each hand-off takes ONE lock, copies, and releases.
// Neither lock is held when this goroutine goes back to the receive, which is
// the rule at the top of this file.
func (m *Magmux) obsSinkLoop(sub *pipeline.Subscription) {
	for d := range sub.C() {
		// The wire form, once, here: the socket verb, MCP and the panel all
		// read this one value, so they cannot render the session differently.
		doc := obsview.FromModel(d.Doc)
		at := time.Now()
		view := attentionOf(doc, at, d)

		m.obsDoc.set(doc, at, d.Dropped)
		m.control.setAttention(view)
	}
}

// stopObsSink waits for the sink's goroutine to finish.
//
// Called after Pipeline.Stop, which closes the subscription — the range above
// then returns. Waiting rather than assuming is what makes this a shutdown
// barrier: the sink writes into the control panel, and a panel written to
// after magmux has restored the terminal is a write nobody can see and a race
// nobody expected.
func (m *Magmux) stopObsSink() {
	if m.obsSinkDone == nil {
		return
	}
	<-m.obsSinkDone
	m.obsSinkDone = nil
	m.obsDoc.stop()
	m.control.clearAttention()
}

// fleetPayload turns the document into the map the socket reply carries.
//
// # It is a round trip through encoding/json, deliberately
//
// requirements.md R4.1 says the socket verb, the MCP tool and the CLI return
// "the identical document", and R4.3 says the panel's content must match it
// exactly. The cheapest way to make that a PROPERTY rather than a promise is
// for every surface to render the same obsview.Fleet, and for the wire one to be
// the marshalled Fleet itself rather than a map assembled by hand beside it.
// A hand-built map is a second copy of architecture-v4 §9's key table, and a
// second copy of a table is how two spellings drift.
//
// The cost is one marshal and one unmarshal per request, at human rate, on a
// socket goroutine. The socket protocol's reply type is a map, so something
// has to produce one; this produces it from the only definition there is.
func fleetPayload(f obsview.Fleet) (map[string]any, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encoding the session document: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("re-reading the session document: %w", err)
	}
	return out, nil
}
