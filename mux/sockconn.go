package mux

// One socket connection = one hub Sub.
//
// Every byte magmux sends a socket client now leaves through that Sub's single
// writer goroutine: the connect-time aggregate, every broadcast, every unicast
// reply, the two shutdown finals and the close. There is no second writer and
// no per-write loop on anybody else's goroutine, which is the whole of N1 —
// `broadcastEvent` used to write to every client synchronously, under one lock,
// with a 100 ms deadline each, from the render goroutine.
//
// The order the wire sees is unchanged, and that is deliberate: the aggregate
// is still the first line (the subscribe cut below), a reply is still unicast
// and still never replayed at teardown, and `results` still precedes
// `shutdown` which still precedes EOF.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// sockSink is a unix-socket connection as the hub sees it.
//
// It is the one Sink that can honestly report n == 0 on a failed write: a plain
// net.Conn either put bytes on the wire or it did not, and that distinction is
// what lets the finals follow a refused write but never a torn one. A buffered
// or TLS sink cannot make that claim and reports every failure as torn.
type sockSink struct {
	conn net.Conn
	once sync.Once
}

// Write puts one whole message on the wire. net.Conn.Write returns the bytes
// actually written, including on a deadline error, so the torn/clean
// distinction the hub needs arrives here for free.
func (s *sockSink) Write(b []byte) (int, error) { return s.conn.Write(b) }

// SetWriteDeadline also interrupts a Write that is ALREADY blocked, which is
// the net.Conn rule Finalize's cut depends on.
func (s *sockSink) SetWriteDeadline(t time.Time) error { return s.conn.SetWriteDeadline(t) }

// Close ends the connection, which is this transport's EOF. The reason is not
// on the wire — a raw socket has nowhere to put it — and is kept for the debug
// log alone. Closing also unblocks a writer sitting inside Write.
func (s *sockSink) Close(reason string) {
	s.once.Do(func() {
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "socket subscriber closed: %s\n", reason)
		}
		_ = s.conn.Close()
	})
}

// sockConnSeq numbers socket connections for Caller.Conn ("sock#12"). It is
// magmux's own label and is never reused within a run, so a panel row or a log
// line names one connection and not "whoever was second".
var sockConnSeq atomic.Uint64

// handleSocketConn serves one connection for its whole life.
//
// The SUBSCRIBE CUT is steps 0-3 below, and its point is that there is no gap
// between "this connection is registered for events" and "this connection has
// been told the current state". The old code built the aggregate, then took a
// lock, then wrote and registered under it; anything published in between was
// simply lost, and it was also why an event could never be published from a
// goroutine that held that lock.
func (m *Magmux) handleSocketConn(conn net.Conn) {
	// Step 0, BEFORE the hub hears about this connection: wait for the layout.
	// The listener is bound before the first child forks, on purpose —
	// MAGMUX_SOCK must be in that child's environment — so connections do
	// arrive while m.root and m.allPanes are still nil. Serving one there writes
	// an EMPTY aggregate as the first line, and a subscriber seeds its whole
	// pane map from that line, so it would then wait forever for per-pane
	// snapshots that only ever fire on change.
	//
	// It must happen here rather than inside Hub.Session: a Sub registered
	// across this wait is BUFFERING, and five seconds of events would overflow
	// its cap and close it before it ever started.
	m.waitLayoutReady(layoutReadyTimeout)

	connID := fmt.Sprintf("sock#%d", sockConnSeq.Add(1))

	// The plugin host's view of this connection. It exists before the Sub
	// because the Sub needs it: a connection registers as a plugin AFTER it and
	// its Sub already exist, so identity cannot be captured — Sub.Caller
	// resolves it through pc.Plugin at every single message. A connection that
	// never registers has pc.Plugin() == "" forever, which costs one map-free
	// mutex per call and nothing else.
	pc := m.plugins().Conn(connID, nil)

	// Step 1: register, BUFFERING. From this moment every Publish is queued for
	// this connection, and nothing is written yet.
	sub := m.bus().Session(hub.Caller{Transport: "socket", Conn: connID}, &sockSink{conn: conn}, pc.Plugin)
	// The host writes to a plugin through the SAME Sub as everything else: one
	// writer per connection is what stops an `invoke` interleaving mid-line with
	// a broadcast, and Sub.Send never blocks, so a wedged plugin fills its own
	// queue and is closed with slow_consumer like any other slow peer.
	pc.SetSend(sub.Send)

	// Step 2: build the aggregate with NO hub lock held. buildPaneResults takes
	// treeMu and each p.mu, and the order is treeMu -> p.mu -> hub.mu; building
	// it under hub.mu would invert that.
	//
	// NOTE: distinct from the per-pane live "snapshot" event (which carries a
	// singular "pane" field). This connect-time aggregate carries a "panes"
	// array — subscribers disambiguate on that field.
	head, err := json.Marshal(map[string]any{
		"type":  "snapshot",
		"panes": m.buildPaneResults(),
	})
	if err != nil {
		sub.Close("aggregate could not be encoded")
		return
	}
	head = append(head, '\n')

	// Step 3: the aggregate goes in the Sub's HEAD, not its queue, and the
	// writer starts. The head cannot be discarded, so a Finalize that landed
	// between steps 1 and 3 still lets it through first: the first line a
	// subscriber sees is always the aggregate, which is what
	// TestSocketSubscriberContract pins and what pilot/magmux.ts seeds from.
	sub.Start(head)

	// Whether this connection ever DROVE anything, as opposed to subscribing
	// and tinting. It is the only thing the panel needs to report a controller
	// arriving and going away: the fd close below is already the disconnect.
	// An operator staring at a frozen panel needs to know the controller went
	// away rather than got slow.
	driving := false
	defer func() {
		// The plugin goes first, and it is not conditional on this connection
		// having been one. A plugin's connection ending IS the plugin ending —
		// magmux cannot invoke what it cannot reach — so its ops are withdrawn
		// here, before the Sub is closed, so `ops_changed` and `plugin_exited`
		// still reach every OTHER subscriber through a live bus.
		pc.Close()
		// Close, not kill: whatever is already queued for this connection is
		// still written (and lost at the socket, which is the peer's business),
		// and whatever is already queued on its LANES is still delivered.
		sub.Close("client disconnected")
		if driving {
			m.control.noteController(false)
		}
	}()

	scanner := bufio.NewScanner(conn)
	// Raise the token limit off the 64KB default. An oversized line is not
	// skipped: Scan returns false and this loop ends, so one long message
	// (a pasted instruction, a large payload) silently drops the *whole*
	// client connection rather than one line — the subscriber stops receiving
	// broadcasts with no error anywhere. 4MB matches the transcript scanners
	// in controller_claude.go.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		// PEEK first, and route the plugin protocol off the RAW line before
		// anything decodes it as a verb.
		//
		// It has to happen here rather than in the verb table for two reasons.
		// The legacy no-id path silently drops unknown verbs, so a plugin
		// message that reached it would vanish with no error anywhere; and a
		// plugin message carries a TOKEN and an identity claim, which must not
		// share a decode path with the ordinary verbs — a field on sockMsg is a
		// field every verb's handler can see.
		var env protocol.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if pc.Handle(env, line) {
			continue
		}
		var msg sockMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		// Identity is attached AFTER decode and by the adapter alone. See
		// sockMsg.caller: the field is unexported precisely so this is the
		// only way it can ever be set.
		msg.caller = callerFor(sub, msg)
		if m.handleSocketMsg(msg, sub) && !driving {
			driving = true
			m.control.noteController(true)
		}
	}
}

// ── teardown ────────────────────────────────────────────────────────────────

const (
	// quiesceGrace is how long in-flight work gets to finish after m.quit and
	// BEFORE `results` is built. It bounds the wait, it does not kill anything:
	// an op still running at the end is abandoned, and only a PTY that stopped
	// reading or a plugin that ignores cancellation can get there.
	quiesceGrace = 500 * time.Millisecond
)

// shutdownSocket is everything that happens between m.quit and the listener
// going away, in the one order that makes the guarantee hold.
//
// The arithmetic, measured from m.quit: Quiesce takes at most 0.5 s, building
// results takes ε, and Finalize is bounded by its own absolute deadline of
// T0+2 s — the in-flight write cut at T0+0.5 s, the finals by T0+2 s. Total
// 2.5 s + ε, against waitSocketShutdown's 3 s, which leaves half a second of
// margin. It replaces closeSockClients(2s), which bounded only the close.
func (m *Magmux) shutdownSocket() {
	// T0 for the whole teardown. Everything bounded below is bounded against
	// THIS instant, so the budgets add up to one number instead of three.
	t0 := time.Now()
	h := m.bus()

	// 0. Stop the framers. A frame is a picture of a session that is still
	//    moving, and from here on magmux is answering the opposite question:
	//    `results` is the authoritative final state, and a screen written after
	//    it would contradict the report. Finalize discards every slot anyway;
	//    stopping the producers first means no goroutine is still diffing a
	//    screen while teardown is measured.
	m.streamer().Shutdown()

	// 1. Stop taking work, cancel what is running, and wait for it — BEFORE
	//    results is built, so the aggregate reports a session that has stopped
	//    moving rather than one still typing into a PTY.
	h.Quiesce(quiesceGrace)

	// 2. Build the final aggregate. In memory, no I/O: subscribers use it as
	//    the authoritative final state of every pane.
	results := map[string]any{
		"type":    "results",
		"panes":   m.buildPaneResults(),
		"endedAt": time.Now().UTC().Format(time.RFC3339),
	}
	shutdown := map[string]any{"type": "shutdown"}

	// 3. results, then shutdown, then EOF — for every subscriber, whatever its
	//    backlog was, and including one that connects after this point (it is
	//    replayed the same two finals by Hub.Session and closed).
	finals := make([][]byte, 0, 2)
	for _, ev := range []any{results, shutdown} {
		if line := eventLine(ev); len(line) > 0 {
			finals = append(finals, line)
		}
	}
	h.Finalize(finals...)

	// 4. The Firebase mirror's own final flush, under the SAME absolute
	//    deadline D the finals were written against.
	//
	//    After Finalize and not beside it: Finalize is what hands the mirror
	//    `results` through the ordinary Write path, so this is the flush that
	//    carries the authoritative end state of every pane, and `alive:false`
	//    with it.
	//
	//    D is ABSOLUTE and shared, not a second two-second budget stacked on
	//    the first. Finalize almost always returns in milliseconds, so the
	//    mirror inherits nearly all of D; a Finalize that spent the whole of it
	//    leaves the floor below, which is enough for one small PATCH and is
	//    better than a session that never records that it ended cleanly.
	m.finalizeFirebase(max(time.Until(t0.Add(finalizeDeadline)), finalizeFloor))
}

const (
	// finalizeDeadline is D, measured from the start of shutdownSocket. It is
	// the hub's own bound, stated here so the mirror's last write is measured
	// against the same instant rather than a number that could drift from it.
	finalizeDeadline = 2 * time.Second
	// finalizeFloor is what the mirror gets when Finalize used all of D.
	finalizeFloor = 250 * time.Millisecond
)

// eventLine marshals one event exactly as the bus carries it: the whole
// message, newline included, because the framing belongs to the transport and
// the hub adds none.
func eventLine(event any) []byte {
	data, err := json.Marshal(event)
	if err != nil {
		return nil
	}
	return append(data, '\n')
}
