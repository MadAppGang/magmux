package mux

// The socket on hub Subs: ordering, teardown and the lanes.
//
// The gate for this phase is the EXISTING socket suite, which must pass
// unchanged — nothing here replaces TestSocketSubscriberContract or
// TestSendVerbReachesPane. What these add is the machinery those tests cannot
// see: a torn write on a real socket, a teardown measured from the moment
// magmux decides to quit, and two instructions to one pane that must not be
// typed into each other.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
)

// ── harness ─────────────────────────────────────────────────────────────────

// sockMux builds a magmux with one pane whose PTY is a pipe, serving a real
// unix socket through the real handleSocketConn. The returned file is the
// CHILD's end: whatever a send actually delivers shows up there, which is the
// only witness that matters.
func sockMux(t *testing.T) (m *Magmux, child *os.File, dial func() net.Conn) {
	t.Helper()
	m = &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel()}
	p := newScrollPane(20, 60)
	childR, childW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { childR.Close(); childW.Close() })
	p.ptmx = childW
	m.root = p
	m.focused = p
	m.allPanes = []*Pane{p}
	m.stampPaneIDs()

	path := filepath.Join(sockTestDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go m.handleSocketConn(conn)
		}
	}()

	return m, childR, func() net.Conn {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatalf("dial %s: %v", path, err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
}

// writeLines puts one JSON message per line on a connection, as every socket
// client does.
func writeLines(t *testing.T, conn net.Conn, msgs ...any) {
	t.Helper()
	for _, msg := range msgs {
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := conn.Write(append(b, '\n')); err != nil {
			t.Fatalf("socket write: %v", err)
		}
	}
}

// childSaw waits until the pane's PTY has received want, and returns everything
// read so far. It fails if it never does, because "the bytes reached the PTY"
// is the only claim a send makes.
func childSaw(t *testing.T, child *os.File, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var got strings.Builder
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		_ = child.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := child.Read(buf)
		got.Write(buf[:n])
		if strings.Contains(got.String(), want) {
			return got.String()
		}
		if err != nil && !os.IsTimeout(err) {
			break
		}
	}
	t.Fatalf("the pane's PTY never received %q; it got %q", want, got.String())
	return ""
}

// childQuiet reads whatever the PTY has, without requiring anything.
func childQuiet(t *testing.T, child *os.File, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var got strings.Builder
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		_ = child.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		n, err := child.Read(buf)
		got.Write(buf[:n])
		if err != nil && !os.IsTimeout(err) {
			break
		}
	}
	return got.String()
}

// panelSignals copies the panel's stream so a test can read it without racing
// the recorder.
func panelSignals(cp *ControlPanel) []ctrlSignal {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]ctrlSignal(nil), cp.signals...)
}

// waitUntil polls for a condition rather than sleeping on a guess.
func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

// ── lanes ───────────────────────────────────────────────────────────────────

// TestOneShotSendSurvivesClose is README's documented one-shot client: "a shell
// script piping JSON into nc -U $MAGMUX_SOCK". It reaches EOF microseconds
// after its last line, and at that moment its second send is ALWAYS still
// queued behind the first one's 150 ms of pacing.
//
// A lane therefore outlives its connection: on close it accepts nothing new and
// drains what it already holds. Dropping the queue instead would lose an
// instruction silently, on exactly the client shape most likely to hit it.
//
// It is README's four-line block with the named-key send written as a second
// text, so both deliveries are observable on the PTY.
func TestOneShotSendSurvivesClose(t *testing.T) {
	m, child, dial := sockMux(t)
	conn := dial()
	writeLines(t, conn,
		map[string]any{"type": "pilot", "event": "start", "pane": 0, "goal": "one-shot", "steps": 2},
		map[string]any{"type": "send", "pane": 0, "text": "first", "label": "step 1/2"},
		map[string]any{"type": "send", "pane": 0, "text": "second", "label": "step 2/2"},
		map[string]any{"type": "pilot", "event": "finish", "summary": "done"},
	)
	// The client hangs up at once, exactly as a piped script does.
	conn.Close()

	got := childSaw(t, child, "second\r", 5*time.Second)
	if got != "first\rsecond\r" {
		t.Fatalf("the PTY saw %q, want both instructions in submission order, each with its own \\r", got)
	}

	// Both OUT rows were written on the reader, before the finish — that is
	// what "the request as it arrived" means, and it is what makes the panel
	// show a send the session never got round to.
	var order []string
	for _, sig := range panelSignals(m.control) {
		if sig.dir == "out" {
			order = append(order, sig.text)
		}
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("panel OUT rows = %q, want both sends in order", order)
	}
	m.control.mu.Lock()
	finished := m.control.finished
	m.control.mu.Unlock()
	if !finished {
		t.Error("the panel never saw the pilot's finish; the reader dropped a line after the sends")
	}
}

// TestDirectSendIsLaneOrdered: two instructions to one pane from one connection
// are typed in submission order, whole, whatever mix of id and no-id they are.
//
// Before the lane each send was its own `go func()`, so the second could start
// writing while the first was still pausing before its Enter — the second
// instruction typed into the middle of the first, silently, on the pane an
// agent is driving.
func TestDirectSendIsLaneOrdered(t *testing.T) {
	_, child, dial := sockMux(t)
	conn := dial()
	writeLines(t, conn,
		map[string]any{"type": "send", "pane": 0, "text": "alpha", "id": 1},
		map[string]any{"type": "send", "pane": 0, "text": "bravo"}, // no id: fire-and-forget
		map[string]any{"type": "send", "pane": 0, "text": "charlie", "id": 2},
	)
	got := childSaw(t, child, "charlie\r", 5*time.Second)
	if got != "alpha\rbravo\rcharlie\r" {
		t.Fatalf("the PTY saw %q; each send's text must be followed by its own \\r, in submission order", got)
	}

	// The id-carrying sends are each answered exactly once, when their bytes
	// reached the PTY.
	replies := map[float64]bool{}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var ev map[string]any
		if json.Unmarshal(scanner.Bytes(), &ev) != nil || ev["type"] != "reply" {
			continue
		}
		id, _ := ev["id"].(float64)
		ok, _ := ev["ok"].(bool)
		if !ok {
			t.Errorf("reply to id %v was not ok: %v", ev["id"], ev["error"])
		}
		replies[id] = true
		if len(replies) == 2 {
			break // both answered; the connection stays open, so do not wait out the deadline
		}
	}
	if !replies[1] || !replies[2] {
		t.Errorf("got replies %v, want one for each id-carrying send", replies)
	}
}

// TestShutdownQuiescesQueuedSend is the whole of Quiesce, stated on the one
// thing it can actually interrupt.
//
// A queued instruction is DISCARDED unrun and says so on the panel; an
// instruction already being typed stops at its next stop check rather than
// finishing its pacing; and `results` is built only after both have answered,
// which is what makes the final aggregate a report on a session that has
// stopped moving.
func TestShutdownQuiescesQueuedSend(t *testing.T) {
	m, child, dial := sockMux(t)
	conn := dial()

	// 40 keys is about 800 ms of pacing — comfortably longer than the 500 ms
	// grace — so this send is certainly still in flight at Quiesce.
	keys := make([]string, 40)
	for i := range keys {
		keys[i] = "x"
	}
	writeLines(t, conn,
		map[string]any{"type": "send", "pane": 0, "text": "inflight", "keys": keys},
		map[string]any{"type": "send", "pane": 0, "text": "queued"},
		// A barrier, and not a sleep: the socket reader is serial, so the status
		// text changing proves it has finished with the line before it — which
		// is the moment the second send is on the lane rather than merely
		// admitted. Shutting down before that would test the enqueue refusal
		// instead of the discard.
		map[string]any{"type": "status", "text": "both sends are on the lane"},
	)
	childSaw(t, child, "inflight", 5*time.Second)
	waitUntil(t, 5*time.Second, "the second send to be queued behind the first", func() bool {
		m.treeMu.RLock()
		defer m.treeMu.RUnlock()
		return m.statusText == "both sends are on the lane"
	})

	start := time.Now()
	m.shutdownSocket()
	elapsed := time.Since(start)
	t.Logf("teardown from m.quit with one send in flight and one queued: %v", elapsed)
	// The grace is 500 ms, and a cancelled delivery returns at its next pacing
	// check, one 20 ms key interval away. Anything near the full 800 ms of
	// pacing means the delivery ignored its ctx.
	if elapsed > 700*time.Millisecond {
		t.Errorf("teardown took %v; a cancelled send must stop before its next key, not finish its pacing", elapsed)
	}

	got := childQuiet(t, child, 300*time.Millisecond)
	if strings.Contains(got, "queued") {
		t.Errorf("the queued instruction reached the PTY (%q); Quiesce must discard it unrun", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("the cancelled delivery still submitted (%q); it must write no Enter", got)
	}

	// Both OUT rows are answered. A row with no ack is a request an operator
	// cannot account for, which is the one outcome worse than a refusal.
	var acks []string
	for _, sig := range panelSignals(m.control) {
		if sig.dir != "out" || sig.ok == nil {
			continue
		}
		if *sig.ok {
			t.Errorf("OUT row %q was acked ok during shutdown", sig.text)
		}
		if sig.code != sockCodeNotReady {
			t.Errorf("OUT row %q acked with code %q, want %q", sig.text, sig.code, sockCodeNotReady)
		}
		acks = append(acks, sig.text+": "+sig.ackText)
	}
	if len(acks) != 2 {
		t.Fatalf("panel shows %d acked OUT rows, want 2: %q", len(acks), acks)
	}
	if !strings.Contains(acks[0], "cancelled") {
		t.Errorf("the in-flight send was acked %q, want a cancellation", acks[0])
	}
	if !strings.Contains(acks[1], "discarded") {
		t.Errorf("the queued send was acked %q, want a discard", acks[1])
	}

	// And the subscriber still got the ordering guarantee.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var types []string
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var ev map[string]any
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			continue
		}
		if s, _ := ev["type"].(string); s == "results" || s == "shutdown" {
			types = append(types, s)
		}
	}
	if len(types) != 2 || types[0] != "results" || types[1] != "shutdown" {
		t.Errorf("subscriber saw %v before EOF, want results then shutdown", types)
	}
}

// TestSendAfterQuiesceIsRefused: once teardown has begun every verb is
// not_ready — the same refusal the socket gives before the layout exists.
func TestSendAfterQuiesceIsRefused(t *testing.T) {
	m, child, dial := sockMux(t)
	conn := dial()
	m.bus().Quiesce(10 * time.Millisecond)

	writeLines(t, conn, map[string]any{"type": "send", "pane": 0, "text": "too late", "id": 7})
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var ev map[string]any
		if json.Unmarshal(scanner.Bytes(), &ev) != nil || ev["type"] != "reply" {
			continue
		}
		if code, _ := ev["code"].(string); code != sockCodeNotReady {
			t.Fatalf("reply after Quiesce = %v, want code %q", ev, sockCodeNotReady)
		}
		break
	}
	if got := childQuiet(t, child, 200*time.Millisecond); got != "" {
		t.Errorf("a send admitted after Quiesce reached the PTY: %q", got)
	}
}

// ── teardown on a real socket ───────────────────────────────────────────────

// TestFinalizeTornWriteRealSocket is the torn-write rule against a real kernel
// socket buffer rather than a fake that merely claims to tear.
//
// A stalled reader faces a message larger than its buffer, so the writer is
// genuinely stuck inside Write with some bytes already gone. Finalize cuts it
// at T0+500ms, and because the line is now PARTIAL nothing may follow it: a
// `results` spliced into half an event is one corrupt message rather than a
// clean end. A second, live client gets the full guarantee at the same time.
func TestFinalizeTornWriteRealSocket(t *testing.T) {
	path := filepath.Join(sockTestDir(t), "torn.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	served := make(chan *hub.Sub, 2)
	h := hub.New()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			sub := h.Session(hub.Caller{Transport: "socket"}, &sockSink{conn: conn}, nil)
			sub.Start([]byte("{\"type\":\"snapshot\",\"panes\":[]}\n"))
			served <- sub
		}
	}()

	stalled, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial stalled: %v", err)
	}
	defer stalled.Close()
	live, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial live: %v", err)
	}
	defer live.Close()
	<-served
	<-served

	// Read the live client continuously; never read the stalled one.
	liveLines := make(chan string, 64)
	go func() {
		defer close(liveLines)
		sc := bufio.NewScanner(live)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for sc.Scan() {
			liveLines <- sc.Text()
		}
	}()

	// One message far larger than any socket buffer, so the stalled peer's
	// writer is inside Write with bytes already on the wire when Finalize runs.
	big := append([]byte(`{"type":"snapshot","pad":"`), []byte(strings.Repeat("p", 4<<20))...)
	big = append(big, []byte("\"}\n")...)
	h.Publish(big)

	// Wait until the live client has drained it, which is when the stalled one
	// is certainly stuck.
	waitUntil(t, 10*time.Second, "the live subscriber to drain the large event", func() bool {
		select {
		case line := <-liveLines:
			return strings.Contains(line, "pad")
		case <-time.After(50 * time.Millisecond):
			return false
		}
	})

	start := time.Now()
	h.Quiesce(quiesceGrace)
	h.Finalize(eventLine(map[string]any{"type": "results", "panes": []any{}}),
		eventLine(map[string]any{"type": "shutdown"}))
	elapsed := time.Since(start)
	t.Logf("teardown from m.quit with a stalled socket client: %v", elapsed)
	if elapsed > 2600*time.Millisecond {
		t.Errorf("teardown took %v; the bound is 0.5s quiesce + a 2s finalize deadline", elapsed)
	}

	// The live client: results, then shutdown, then EOF.
	var tail []string
	for line := range liveLines {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if s, _ := ev["type"].(string); s == "results" || s == "shutdown" {
			tail = append(tail, s)
		}
	}
	if len(tail) != 2 || tail[0] != "results" || tail[1] != "shutdown" {
		t.Errorf("the live subscriber saw %v before EOF, want results then shutdown", tail)
	}

	// The stalled client: a partial line, then EOF, and no results after it.
	_ = stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
	stalledBytes, _ := readAllBounded(stalled, 16<<20)
	text := string(stalledBytes)
	if !strings.Contains(text, "\"type\":\"snapshot\",\"panes\":[]") {
		t.Errorf("the stalled subscriber never got its aggregate")
	}
	if strings.Contains(text, `"type":"results"`) {
		t.Error("a final was written behind a torn line; a client parsing lines would see one corrupt message")
	}
	if strings.HasSuffix(text, "\n") {
		t.Error("the stalled subscriber's stream ended on a line boundary; this test needs a genuinely torn write")
	}
}

// readAllBounded reads until EOF or the cap, and returns whatever arrived. A
// read error is an expected outcome here — EOF is the assertion.
func readAllBounded(r net.Conn, max int) ([]byte, error) {
	buf := make([]byte, 64*1024)
	var out []byte
	for len(out) < max {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// TestSlowSocketClientDoesNotDelayRenderLoop is N1, measured the same way
// TestRenderWritesTerminalWithTreeMuReleased measures the frame: a subscriber
// that has stopped reading must cost the render goroutine nothing.
//
// The old broadcastEvent wrote to every client synchronously, under one lock,
// with a 100 ms deadline each — so one wedged subscriber cost every publisher
// 100 ms per event, and a treeMu.Lock queued behind it.
func TestSlowSocketClientDoesNotDelayRenderLoop(t *testing.T) {
	m, _, dial := sockMux(t)
	stalled := dial() // dialled and never read from

	// Give the connection time to be served, so the Sub is real.
	waitUntil(t, 5*time.Second, "the subscriber to be registered", func() bool {
		return m.bus().Subs() > 0
	})
	// Fill its queue: a message per event, none of them read.
	for i := 0; i < 64; i++ {
		m.broadcastEvent(map[string]any{"type": "snapshot", "pane": 0, "n": i,
			"pad": strings.Repeat("q", 64*1024)})
	}

	// Now measure what the render goroutine actually pays.
	const events = 200
	start := time.Now()
	for i := 0; i < events; i++ {
		m.broadcastEvent(map[string]any{"type": "snapshot", "pane": 0, "n": i})
	}
	elapsed := time.Since(start)
	t.Logf("%d broadcasts with a stalled subscriber attached: %v", events, elapsed)
	// The old path would have taken 100 ms per event per stalled client; 20 s.
	// A second is generous for what is now a marshal and an append.
	if elapsed > time.Second {
		t.Fatalf("%d broadcasts took %v with one stalled subscriber; publishing must never block", events, elapsed)
	}

	// And a treeMu writer — a keystroke, a SIGWINCH — is not queued behind it
	// either, because publishing takes no lock of magmux's at all.
	done := make(chan time.Duration, 1)
	go func() {
		s := time.Now()
		m.treeMu.Lock()
		m.treeMu.Unlock()
		done <- time.Since(s)
	}()
	for i := 0; i < events; i++ {
		m.broadcastEvent(map[string]any{"type": "snapshot", "pane": 0, "n": i})
	}
	if waited := <-done; waited > 500*time.Millisecond {
		t.Errorf("treeMu.Lock waited %v while events were published to a stalled subscriber", waited)
	}
	_ = stalled.Close()
	if t.Failed() {
		fmt.Println(m.bus().Subs())
	}
}
