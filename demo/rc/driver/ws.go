package main

// The driver's own WebSocket: the telemetry behind the header, the stalled
// subscriber action 11 stages, and the raw frames action 13 prints.
//
// The codec is magmux's own `transport/ws`, reached through the `replace` in
// this module's go.mod, and the frames decode into `protocol.Frame`. That is
// deliberate: a demo that re-implemented RFC 6455 and the frame schema would be
// a SECOND decoder, and a second decoder that is subtly wrong does not fail —
// it draws a slightly different picture nobody notices.

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/ws"
)

// The liveness rule, and it is the SAME one both mirrors use.
//
// SILENCE IS NEVER THE SIGNAL. An idle pane legitimately produces no frames —
// that is the point of a wake-driven framer — so a client that read a quiet
// stream as failure would cry wolf every time the human stopped typing, and one
// that read it as health would show a still picture of a magmux that died ten
// minutes ago. So the driver ASKS: a read-class `list` every 2s with a 3s
// budget, and two consecutive misses is a verdict. Between probes the age of
// the last frame is information, not a judgement.
const (
	probeInterval = 2 * time.Second
	probeBudget   = 3 * time.Second
	probeMisses   = 2
	staleAfter    = 10 * time.Second
)

// LinkState is the connection's verdict, as a badge.
type LinkState int

const (
	LinkConnecting LinkState = iota
	LinkLive
	LinkStale
	LinkDead
)

func (s LinkState) String() string {
	switch s {
	case LinkLive:
		return "LIVE"
	case LinkStale:
		return "STALE"
	case LinkDead:
		return "DEAD"
	default:
		return "LINKING"
	}
}

// Telemetry messages. These are the only things the socket goroutine hands the
// UI, and every one of them is a MEASUREMENT — nothing here is derived from a
// timer that assumed an answer.
type (
	linkMsg struct {
		State  LinkState
		Reason string
	}
	frameMsg struct {
		Pane          int
		Seq           uint64
		Rows, Cols    int
		Alt, Scrolled bool
		Key           bool
		Bytes         int
		At            time.Time
	}
	probeMsg struct{ RTT time.Duration }
)

// dial opens a TCP (or TLS) connection and performs the WebSocket handshake
// with a credential in the SUBPROTOCOL, which is how a browser presents one and
// therefore how this demo does.
func dial(rawURL, token string) (*ws.ClientConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	var conn net.Conn
	if u.Scheme == "https" {
		conn, err = tls.Dial("tcp", host, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // a demo against a self-signed local pair
	} else {
		conn, err = net.DialTimeout("tcp", host, 10*time.Second)
	}
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	hdr.Set("Sec-WebSocket-Protocol", "magmux.v1, magmux.auth."+token)
	c, resp, err := ws.ClientHandshake(conn, host, "/v1/ws", hdr)
	if err != nil {
		_ = conn.Close()
		if resp != nil {
			return nil, fmt.Errorf("magmux answered %s to the upgrade", resp.Status)
		}
		return nil, err
	}
	return c, nil
}

// Telemetry is the driver's long-lived subscriber: it watches the target pane
// so the header can show real frame arrivals, and it probes so the LIVE/DEAD
// badge is an answer rather than an assumption.
type Telemetry struct {
	cfg  Config
	out  chan<- any
	mu   sync.Mutex
	conn *ws.ClientConn
	next int
	// probes maps an outstanding id to the moment it was sent.
	probes map[int]time.Time
	misses int
	pane   int
	closed bool
}

func NewTelemetry(cfg Config, out chan<- any) *Telemetry {
	return &Telemetry{cfg: cfg, out: out, probes: map[int]time.Time{}, pane: -1}
}

// Run connects, watches `pane`, and does not return until Close. Everything it
// learns leaves through the channel; it never touches UI state directly.
func (t *Telemetry) Run(pane int) {
	c, err := dial(t.cfg.URL, t.cfg.Token)
	if err != nil {
		t.emit(linkMsg{LinkDead, err.Error()})
		return
	}
	t.mu.Lock()
	t.conn, t.pane = c, pane
	t.mu.Unlock()

	t.send("hello", map[string]any{"client": "magmux-rc-driver"})
	t.send("watch", map[string]any{"pane": pane, "mode": "frames", "fps": 10})
	t.emit(linkMsg{LinkConnecting, ""})

	stop := make(chan struct{})
	go t.probeLoop(stop)
	defer close(stop)

	for {
		op, payload, err := c.Read()
		if err != nil {
			t.mu.Lock()
			closed := t.closed
			t.mu.Unlock()
			if !closed {
				t.emit(linkMsg{LinkDead, "the stream ended: " + err.Error()})
			}
			return
		}
		if op != ws.OpText {
			continue
		}
		t.handle(payload)
	}
}

func (t *Telemetry) handle(payload []byte) {
	var env struct {
		Type string `json:"type"`
		ID   int    `json:"id"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if env.Type == "frame" {
		var f protocol.Frame
		if err := json.Unmarshal(payload, &f); err != nil {
			return
		}
		t.emit(frameMsg{
			Pane: f.Pane, Seq: f.Seq, Rows: f.Rows, Cols: f.Cols,
			Alt: f.Alt, Scrolled: f.Scrolled, Key: f.Key,
			Bytes: len(payload), At: time.Now(),
		})
		return
	}
	// A reply. If it answers an outstanding probe, that is a measured round
	// trip and the proof magmux is still there.
	t.mu.Lock()
	at, ok := t.probes[env.ID]
	if ok {
		delete(t.probes, env.ID)
		t.misses = 0
	}
	t.mu.Unlock()
	if ok {
		t.emit(probeMsg{time.Since(at)})
		t.emit(linkMsg{LinkLive, ""})
	}
}

func (t *Telemetry) probeLoop(stop <-chan struct{}) {
	tick := time.NewTicker(probeInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			t.mu.Lock()
			for id, at := range t.probes {
				if now.Sub(at) > probeBudget {
					delete(t.probes, id)
					t.misses++
				}
			}
			misses := t.misses
			t.mu.Unlock()
			if misses >= probeMisses {
				t.emit(linkMsg{LinkDead, fmt.Sprintf(
					"magmux stopped answering: %d liveness probes went unanswered within %s each",
					misses, probeBudget)})
				t.Close()
				return
			}
			id := t.send("list", nil)
			t.mu.Lock()
			t.probes[id] = now
			t.mu.Unlock()
		}
	}
}

func (t *Telemetry) send(op string, args map[string]any) int {
	t.mu.Lock()
	t.next++
	id := t.next
	c := t.conn
	t.mu.Unlock()
	if c == nil {
		return id
	}
	msg := map[string]any{"id": id, "op": op}
	if args != nil {
		msg["args"] = args
	}
	b, _ := json.Marshal(msg)
	_ = c.WriteText(string(b))
	return id
}

// Retarget moves the driver's own telemetry to another pane. It is `unwatch`
// then `watch` on this connection — the client's own act, because there is no
// op that reaches into somebody else's connection to point it somewhere new.
func (t *Telemetry) Retarget(pane int) {
	t.mu.Lock()
	old := t.pane
	t.pane = pane
	t.mu.Unlock()
	if old >= 0 && old != pane {
		t.send("unwatch", map[string]any{"pane": old})
	}
	t.send("watch", map[string]any{"pane": pane, "mode": "frames", "fps": 10})
}

// Close unwatches first, so magmux is not left framing a pane for a subscriber
// that has gone. The flag is set before the write so the read loop can tell a
// deliberate close from a magmux that died.
func (t *Telemetry) Close() {
	t.mu.Lock()
	if t.closed || t.conn == nil {
		t.closed = true
		t.mu.Unlock()
		return
	}
	t.closed = true
	pane, c := t.pane, t.conn
	t.mu.Unlock()
	if pane >= 0 {
		b, _ := json.Marshal(map[string]any{"id": 9999, "op": "unwatch", "args": map[string]any{"pane": pane}})
		_ = c.WriteText(string(b))
	}
	_ = c.Close()
}

func (t *Telemetry) emit(m any) {
	defer func() { _ = recover() }() // the channel closes when the UI quits
	t.out <- m
}

// ── action 11: a subscriber that stops reading ──────────────────────────────

// StalledWS handshakes, watches a pane at 30fps, reads far enough to SEE a
// frame, and then reads nothing at all.
//
// Reading far enough first is not politeness: a client that is quiet because
// its handshake failed looks exactly like one that is quiet because it stopped
// reading, and the demonstration would then be of nothing. After that the
// socket's receive buffer fills, TCP advertises a zero window, and magmux's
// write to this subscriber blocks exactly as it would against a wedged browser
// tab on a laptop somebody closed.
//
// In Go this is simply not calling Read, which is why there is no equivalent of
// the platform WebSocket's always-draining reader to work around.
type StalledWS struct {
	conn      *ws.ClientConn
	BytesRead int
}

func (s *StalledWS) ConnectAndWatch(cfg Config, pane int) error {
	c, err := dial(cfg.URL, cfg.Token)
	if err != nil {
		return err
	}
	s.conn = c
	b, _ := json.Marshal(map[string]any{
		"id": 1, "op": "watch", "args": map[string]any{"pane": pane, "fps": 30},
	})
	if err := c.WriteText(string(b)); err != nil {
		_ = c.Close()
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	if err := c.SetReadDeadline(deadline); err != nil {
		_ = c.Close()
		return err
	}
	for time.Now().Before(deadline) {
		op, payload, err := c.Read()
		if err != nil {
			_ = c.Close()
			return err
		}
		s.BytesRead += len(payload)
		if op == ws.OpText && strings.Contains(string(payload), `"type":"frame"`) {
			// From here it reads nothing, for as long as it is held.
			return c.SetReadDeadline(time.Time{})
		}
	}
	_ = c.Close()
	return fmt.Errorf("no frame within 20s; it is not really watching")
}

func (s *StalledWS) Close() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

// ── action 13: the raw frames both mirrors decode ───────────────────────────

// RawFrames opens a VIEWER's connection — the view token, deliberately,
// because the point is to show what the two mirrors actually receive rather
// than something adjacent to it — and hands back the next frames as they came
// off the wire.
func RawFrames(cfg Config, pane, want int, within time.Duration) ([]string, error) {
	c, err := dial(cfg.URL, cfg.ViewToken)
	if err != nil {
		return nil, err
	}
	defer c.Close() //nolint:errcheck
	b, _ := json.Marshal(map[string]any{
		"id": 1, "op": "watch",
		"args": map[string]any{"pane": pane, "mode": "frames", "fps": 15},
	})
	if err := c.WriteText(string(b)); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(within)
	if err := c.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	var got []string
	for len(got) < want && time.Now().Before(deadline) {
		op, payload, err := c.Read()
		if err != nil {
			break // a deadline here is "the pane is idle", which is legitimate
		}
		if op == ws.OpText && strings.Contains(string(payload), `"type":"frame"`) {
			got = append(got, string(payload))
		}
	}
	return got, nil
}
