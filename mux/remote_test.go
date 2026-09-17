package mux

// Remote control end to end: a real magmux process, a real TCP listener, a real
// token on disk, and real clients over REST, WebSocket and server-sent events.
//
// The package's own unit tests prove each piece. These prove the wiring, which
// is where every bug in this phase would live: the token file existing before
// the port accepts, the hub's aggregate reaching a WebSocket as its first
// frame, a keystroke crossing HTTP and coming back as a frame, and the teardown
// ordering holding on a transport that has no line framing of its own.

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/sse"
	"github.com/MadAppGang/magmux/transport/ws"
)

// ── harness ─────────────────────────────────────────────────────────────────

type remoteMagmux struct {
	*headlessMagmux
	base      string
	addr      string
	token     string
	viewToken string
}

var listenLine = regexp.MustCompile(`listening on (https?://[^\s]+)`)

// startRemoteMagmux starts a headless magmux with --listen on an ephemeral
// port, and waits until it is actually serving.
//
// The readiness signal is the one the design promises: the startup line names
// the URL, and by the time the port accepts, the generated token file is at its
// final name with its final contents. The harness reads the token off disk
// rather than being handed it, because that is exactly what a real operator
// does and it is the property worth testing.
func startRemoteMagmux(t *testing.T, extra ...string) *remoteMagmux {
	t.Helper()
	args := append([]string{"--headless", "--listen", "127.0.0.1:0"}, extra...)
	h := startHeadlessMagmux(t, args...)

	r := &remoteMagmux{headlessMagmux: h}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m := listenLine.FindStringSubmatch(h.stderr.String()); m != nil {
			r.base = m[1]
			r.addr = strings.TrimPrefix(strings.TrimPrefix(r.base, "http://"), "https://")
			break
		}
		select {
		case <-h.exited:
			t.Fatalf("magmux exited before it announced a listener\nstderr: %s", h.stderr.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.base == "" {
		t.Fatalf("magmux never announced a listener within 15s\nstderr: %s", h.stderr.String())
	}

	// An accepting port implies a final token file. No polling and no retry
	// here on purpose: if the file is not readable RIGHT NOW, that contract is
	// broken and the test must say so.
	path := filepath.Join(h.dir, fmt.Sprintf("magmux-%d.token", h.cmd.Process.Pid))
	tok, err := auth.LoadFile(path)
	if err != nil {
		t.Fatalf("the port accepts but the token file is not readable: %v\nstderr: %s", err, h.stderr.String())
	}
	r.token = tok
	return r
}

func (r *remoteMagmux) req(t *testing.T, method, path, token string, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		t.Fatalf("%s %s: body is not a JSON object: %q", method, path, raw)
	}
	return resp, m
}

// ── REST ────────────────────────────────────────────────────────────────────

// TestRemoteRESTDrivesARealSession is the curl-equivalent gate: list, open,
// capture, close, with the token; 401 without it; 403 for the view token on a
// write op.
func TestRemoteRESTDrivesARealSession(t *testing.T) {
	dir := sockTestDir(t)
	viewTok, _ := auth.Generate()
	viewPath := filepath.Join(dir, "view.token")
	if err := os.WriteFile(viewPath, []byte(viewTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := startRemoteMagmux(t, "--view-token-file", viewPath, "-e", "sh")
	m.viewToken = viewTok

	t.Run("no credential", func(t *testing.T) {
		resp, body := m.req(t, "GET", "/v1/panes", "", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if body["code"] != protocol.CodeUnauthorized {
			t.Errorf("code = %v", body["code"])
		}
	})

	t.Run("a wrong token", func(t *testing.T) {
		other, _ := auth.Generate()
		if resp, _ := m.req(t, "GET", "/v1/panes", other, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	var pane int
	t.Run("list the panes", func(t *testing.T) {
		resp, body := m.req(t, "GET", "/v1/panes", m.token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		panes, _ := body["panes"].([]any)
		if len(panes) < 2 {
			t.Fatalf("want at least the -e pane and the hidden panel, got %v", panes)
		}
		for _, p := range panes {
			pm, _ := p.(map[string]any)
			if pm["state"] == "panel" {
				continue
			}
			pane = int(pm["pane"].(float64))
			break
		}
	})

	var opened int
	t.Run("open a pane", func(t *testing.T) {
		resp, body := m.req(t, "POST", "/v1/ops/open_pane", m.token,
			`{"cmd":"echo MAGMUX_REST_OPENED; sleep 30","label":"rest"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		if body["ok"] != true {
			t.Fatalf("ok = %v: %v", body["ok"], body)
		}
		result := body["result"].(map[string]any)
		opened = int(result["pane"].(float64))
		if opened == pane {
			t.Fatalf("open_pane returned an existing pane id %d", opened)
		}
	})

	t.Run("capture it", func(t *testing.T) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			resp, body := m.req(t, "GET", fmt.Sprintf("/v1/panes/%d/screen", opened), m.token, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %v", resp.StatusCode, body)
			}
			if text, _ := body["text"].(string); strings.Contains(text, "MAGMUX_REST_OPENED") {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("the opened pane's output never reached GET /v1/panes/{n}/screen")
	})

	t.Run("the view token may read", func(t *testing.T) {
		resp, body := m.req(t, "GET", "/v1/panes", m.viewToken, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		resp, body = m.req(t, "GET", fmt.Sprintf("/v1/panes/%d/screen", opened), m.viewToken, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("capture with the view token = %d: %v", resp.StatusCode, body)
		}
	})

	t.Run("the view token may not type", func(t *testing.T) {
		for _, tc := range []struct{ op, args string }{
			{"input", fmt.Sprintf(`{"pane":%d,"text":"whoami\r"}`, opened)},
			{"send", fmt.Sprintf(`{"pane":%d,"text":"whoami"}`, opened)},
			{"open_pane", `{"cmd":"true"}`},
			{"close_pane", fmt.Sprintf(`{"pane":%d}`, opened)},
			{"tint", fmt.Sprintf(`{"pane":%d,"color":"red"}`, opened)},
		} {
			resp, body := m.req(t, "POST", "/v1/ops/"+tc.op, m.viewToken, tc.args)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s with the view token = %d, want 403: %v", tc.op, resp.StatusCode, body)
			}
			if body["code"] != protocol.CodeForbidden {
				t.Errorf("%s: code = %v, want forbidden", tc.op, body["code"])
			}
		}
	})

	t.Run("close it", func(t *testing.T) {
		resp, body := m.req(t, "POST", "/v1/ops/close_pane", m.token,
			fmt.Sprintf(`{"pane":%d,"force":true}`, opened))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		// The id is a tombstone afterwards, which is the documented behaviour
		// and is why the status is 404 and not 200 with an empty screen.
		resp, body = m.req(t, "GET", fmt.Sprintf("/v1/panes/%d/screen", opened), m.token, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("capturing a closed pane = %d, want 404: %v", resp.StatusCode, body)
		}
		if body["code"] != protocol.CodeNoSuchPane {
			t.Errorf("code = %v, want no_such_pane", body["code"])
		}
	})

	t.Run("capabilities names the transport", func(t *testing.T) {
		resp, body := m.req(t, "GET", "/v1/capabilities", m.token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		tr, _ := body["transports"].(map[string]any)
		h, _ := tr["http"].(map[string]any)
		if h == nil {
			t.Fatalf("no transports.http in %v", body)
		}
		if h["tls"] != false || h["insecure"] != false {
			t.Errorf("a loopback plaintext bind is not TLS and not insecure: %v", h)
		}
	})

	t.Run("ops lists the registry", func(t *testing.T) {
		resp, body := m.req(t, "GET", "/v1/ops", m.token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		ops, _ := body["ops"].([]any)
		if len(ops) < 12 {
			t.Fatalf("want every built-in op, got %d", len(ops))
		}
		if body["rev"] == nil {
			t.Error("a client caches the op list and needs the revision to know when it is stale")
		}
	})
}

// ── WebSocket ───────────────────────────────────────────────────────────────

// TestRemoteWebSocketWatchesAndTypes is the claim the whole transport exists
// for: a browser-shaped client watches a shell pane, types, and sees the result
// come back as a frame.
func TestRemoteWebSocketWatchesAndTypes(t *testing.T) {
	m := startRemoteMagmux(t, "-e", "sh")

	conn, err := net.DialTimeout("tcp", m.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	// The browser credential channel: the token rides as a second subprotocol,
	// and magmux echoes only magmux.v1 back.
	hdr.Set("Sec-WebSocket-Protocol", ws.Subprotocol+", "+ws.AuthPrefix+m.token)
	cc, resp, err := ws.ClientHandshake(conn, m.addr, "/v1/ws", hdr)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer cc.Close()
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != ws.Subprotocol {
		t.Errorf("echoed subprotocol = %q, want %q", got, ws.Subprotocol)
	}
	if strings.Contains(resp.Header.Get("Sec-WebSocket-Protocol"), m.token) {
		t.Fatal("the token must never be echoed into a response header")
	}

	read := func() (map[string]any, string) {
		t.Helper()
		for {
			_ = cc.SetReadDeadline(time.Now().Add(15 * time.Second))
			op, msg, err := cc.Read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if op != ws.OpText {
				continue
			}
			var mm map[string]any
			if err := json.Unmarshal(msg, &mm); err != nil {
				t.Fatalf("frame %q: %v", msg, err)
			}
			return mm, string(msg)
		}
	}

	// The aggregate is always first, on every transport.
	first, _ := read()
	if first["type"] != "snapshot" {
		t.Fatalf("the first message must be the aggregate, got %v", first["type"])
	}
	panes, _ := first["panes"].([]any)
	pane := -1
	for _, p := range panes {
		pm, _ := p.(map[string]any)
		if pm["state"] == "panel" {
			continue
		}
		pane = int(pm["pane"].(float64))
		break
	}
	if pane < 0 {
		t.Fatalf("no session pane in the aggregate: %v", panes)
	}

	if err := cc.WriteText(`{"id":1,"op":"hello","args":{"client":"magmux-test/1"}}`); err != nil {
		t.Fatal(err)
	}
	if reply, _ := read(); reply["ok"] != true {
		t.Fatalf("hello = %v", reply)
	}

	// watch, then wait for the keyframe: a client sizes itself from the reply
	// and clears its screen for the keyframe, and only then is it ready to type.
	if err := cc.WriteText(fmt.Sprintf(`{"id":2,"op":"watch","args":{"pane":%d,"fps":30}}`, pane)); err != nil {
		t.Fatal(err)
	}
	sawKey := false
	for !sawKey {
		ev, raw := read()
		switch ev["type"] {
		case protocol.EventReply:
			if fmt.Sprint(ev["id"]) == "2" {
				res, _ := ev["result"].(map[string]any)
				if ev["ok"] != true {
					t.Fatalf("watch = %v", ev)
				}
				if rows, _ := res["rows"].(float64); rows <= 0 {
					t.Fatalf("the watch reply must carry geometry: %v", res)
				}
			}
		case protocol.EventFrame:
			var f protocol.Frame
			if err := json.Unmarshal([]byte(raw), &f); err != nil {
				t.Fatalf("a frame did not decode as protocol.Frame: %v\n%s", err, raw)
			}
			if f.Pane == pane && f.Key {
				sawKey = true
			}
		}
	}
	// Let the shell finish its first prompt, so the frame measured below is the
	// one this test's own input produced.
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	if err := cc.WriteText(fmt.Sprintf(
		`{"id":3,"op":"input","args":{"pane":%d,"text":"echo MAGMUX_RC_OK\r"}}`, pane)); err != nil {
		t.Fatal(err)
	}

	var replyAt, frameAt time.Duration
	deadline := time.Now().Add(15 * time.Second)
	for frameAt == 0 && time.Now().Before(deadline) {
		ev, raw := read()
		if ev["type"] == protocol.EventReply && fmt.Sprint(ev["id"]) == "3" {
			replyAt = time.Since(started)
			if ev["ok"] != true {
				t.Fatalf("input = %v", ev)
			}
			res := ev["result"].(map[string]any)
			if fmt.Sprint(res["bytes"]) != "18" {
				t.Errorf("input reported %v bytes, want 18 (the text plus its CR)", res["bytes"])
			}
			continue
		}
		if ev["type"] != protocol.EventFrame {
			continue
		}
		var f protocol.Frame
		if err := json.Unmarshal([]byte(raw), &f); err != nil || f.Pane != pane {
			continue
		}
		for _, l := range f.Lines {
			// The terminal echoes the command line too; the OUTPUT is the line
			// that is the marker alone, and that is the one that proves the
			// shell ran it.
			if strings.TrimSpace(l.T) == "MAGMUX_RC_OK" {
				frameAt = time.Since(started)
			}
		}
	}
	if frameAt == 0 {
		t.Fatal("no frame carrying MAGMUX_RC_OK arrived within 15s")
	}
	t.Logf("WS input -> reply %v; WS input -> frame carrying MAGMUX_RC_OK %v", replyAt, frameAt)
	if frameAt > 2*time.Second {
		t.Errorf("the echo took %v to come back as a frame; a remote terminal must be inside a second "+
			"(one frame interval at 30 fps is 33ms)", frameAt)
	}
}

// TestRemoteWebSocketRefusesAViewTokenTheInputOp is the 403 on the transport
// that has no statuses: the refusal arrives as a reply carrying `forbidden`.
func TestRemoteWebSocketRefusesAViewTokenTheInputOp(t *testing.T) {
	dir := sockTestDir(t)
	viewTok, _ := auth.Generate()
	viewPath := filepath.Join(dir, "view.token")
	if err := os.WriteFile(viewPath, []byte(viewTok), 0o600); err != nil {
		t.Fatal(err)
	}
	m := startRemoteMagmux(t, "--view-token-file", viewPath, "-e", "sh")

	conn, err := net.DialTimeout("tcp", m.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+viewTok)
	cc, _, err := ws.ClientHandshake(conn, m.addr, "/v1/ws", hdr)
	if err != nil {
		t.Fatalf("a view token is a genuine credential and must still open a socket: %v", err)
	}
	defer cc.Close()

	read := func() map[string]any {
		t.Helper()
		for {
			_ = cc.SetReadDeadline(time.Now().Add(10 * time.Second))
			op, msg, err := cc.Read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if op != ws.OpText {
				continue
			}
			var mm map[string]any
			_ = json.Unmarshal(msg, &mm)
			return mm
		}
	}
	if first := read(); first["type"] != "snapshot" {
		t.Fatalf("first = %v", first)
	}

	if err := cc.WriteText(`{"id":1,"op":"input","args":{"pane":0,"text":"whoami\r"}}`); err != nil {
		t.Fatal(err)
	}
	reply := read()
	if reply["ok"] != false || reply["code"] != protocol.CodeForbidden {
		t.Fatalf("input from a viewer = %v, want forbidden", reply)
	}

	// A watch is class read, so a viewer may have one: that is the whole point
	// of the credential.
	if err := cc.WriteText(`{"id":2,"op":"list"}`); err != nil {
		t.Fatal(err)
	}
	if reply := read(); reply["ok"] != true {
		t.Fatalf("list from a viewer = %v", reply)
	}
}

// ── SSE ─────────────────────────────────────────────────────────────────────

// TestRemoteSSEDeliversTheAggregateFirst, over a real listener and with a
// ticket rather than a header, which is what an EventSource can do.
func TestRemoteSSEDeliversTheAggregateFirst(t *testing.T) {
	m := startRemoteMagmux(t, "-e", "sh")

	_, body := m.req(t, "POST", "/v1/tickets", m.token, "")
	ticket, _ := body["ticket"].(string)
	if ticket == "" {
		t.Fatalf("no ticket: %v", body)
	}
	if strings.Contains(ticket, m.token) {
		t.Fatal("a ticket must not contain the token")
	}

	req, _ := http.NewRequest("GET", m.base+"/v1/events?ticket="+ticket, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	r := sse.NewReader(resp.Body)
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if ev.Name != "snapshot" {
		t.Fatalf("the first event must be the aggregate, got %q", ev.Name)
	}
	var agg map[string]any
	if err := json.Unmarshal(ev.Data, &agg); err != nil {
		t.Fatalf("aggregate %q: %v", ev.Data, err)
	}
	if panes, _ := agg["panes"].([]any); len(panes) < 2 {
		t.Fatalf("the aggregate must carry every pane, got %v", agg)
	}

	// Reusing the ticket is a 401, which EventSource treats as fatal rather
	// than looping on.
	resp2, body2 := m.req(t, "GET", "/v1/events?ticket="+ticket, "", "")
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reusing a ticket = %d, want 401: %v", resp2.StatusCode, body2)
	}
}

// TestFinalizeTornWriteSSE is the architecture's named test: a stalled
// /v1/events client must never see `event: results` after a partial `data:`
// line, and a live one must get results, shutdown and the end of the body.
//
// The stall is built rather than simulated: the client stops reading while a
// pane paints continuously at 30 fps, so the kernel buffers fill and magmux's
// writer blocks inside Write. Finalize then cuts that write short, and — because
// an SSE sink can never honestly report "no bytes left the process" — the write
// is treated as TORN and the subscriber is closed without finals.
func TestFinalizeTornWriteSSE(t *testing.T) {
	// A big synthetic screen, so one frame is tens of kilobytes rather than
	// one or two. Headless geometry comes from COLUMNS/LINES, which the child
	// inherits, and the stall has to outrun the kernel's socket buffers — a
	// stalled 80x24 stream fits entirely in them and never blocks a writer.
	t.Setenv("COLUMNS", "300")
	t.Setenv("LINES", "90")
	m := startRemoteMagmux(t,
		"-e", "while :; do printf '%s\\n' $RANDOM$RANDOM$RANDOM$RANDOM$RANDOM$RANDOM; done")

	// A LIVE client, kept drained throughout, is the control: whatever the
	// stalled one does, this one must get the full, ordered teardown.
	liveReq, _ := http.NewRequest("GET", m.base+"/v1/events", nil)
	liveReq.Header.Set("Authorization", "Bearer "+m.token)
	liveResp, err := http.DefaultClient.Do(liveReq)
	if err != nil {
		t.Fatal(err)
	}
	defer liveResp.Body.Close()
	liveEvents := make(chan string, 256)
	liveDone := make(chan struct{})
	go func() {
		defer close(liveDone)
		r := sse.NewReader(liveResp.Body)
		for {
			ev, err := r.Next()
			if err != nil {
				return
			}
			select {
			case liveEvents <- ev.Name:
			default:
			}
		}
	}()

	// The STALLED client: a raw socket with a deliberately tiny receive buffer,
	// which asks to watch every pane and then stops reading.
	d := net.Dialer{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048)
			})
		},
	}
	stalled, err := d.Dial("tcp", m.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	fmt.Fprintf(stalled, "GET /v1/events?watch=all&fps=30 HTTP/1.1\r\nHost: %s\r\n"+
		"Authorization: Bearer %s\r\nAccept: text/event-stream\r\n\r\n", m.addr, m.token)

	// Read just the response head and the first event, then stop. Everything
	// magmux writes from here on piles up in the kernel.
	br := bufio.NewReader(stalled)
	_ = stalled.SetReadDeadline(time.Now().Add(10 * time.Second))
	head, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("stalled client: %v", err)
	}
	if head.StatusCode != http.StatusOK {
		t.Fatalf("stalled client: status %d", head.StatusCode)
	}

	// Let the buffers fill, and then keep filling. The stall has to outlast the
	// KERNEL, not the client: a 2 KB receive buffer does not mean 2 KB in
	// flight, because the sender's own send buffer autotunes — measured at
	// roughly 460 KB on loopback here. Three seconds buffered cleanly and the
	// writer was never blocked, so the finals went out normally and the test
	// passed while proving nothing. Eight seconds is what puts the writer
	// INSIDE a blocked write when Finalize arrives, which is the only state in
	// which the torn-write rule has anything to do.
	time.Sleep(8 * time.Second)

	// Wait for the live client to have seen something, so the two are
	// genuinely concurrent.
	select {
	case <-liveEvents:
	case <-time.After(10 * time.Second):
		t.Fatal("the live SSE client never received its aggregate")
	}

	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	// Stay stalled ACROSS the cut. This is the part that makes the test about
	// the torn-write rule rather than about the scheduler: Finalize runs at
	// roughly SIGTERM+0.5s and cuts the in-flight write 500ms later, so the
	// reader must not start draining before then — draining would unblock the
	// writer and the message would complete normally.
	time.Sleep(1500 * time.Millisecond)

	// Now read whatever is there until the body ends.
	_ = stalled.SetReadDeadline(time.Now().Add(15 * time.Second))
	rest, _ := io.ReadAll(br)
	body := string(rest)

	// THE assertion. `event: results` may not appear after a partial line: a
	// final spliced into a half-written data line is one corrupt message where
	// a client expected two good ones.
	if i := strings.Index(body, "event: results"); i >= 0 {
		before := body[:i]
		if !strings.HasSuffix(before, "\n") {
			t.Fatalf("`event: results` followed a partial line; the torn-write rule did not hold.\n"+
				"tail before results: %q", tail(before, 200))
		}
		for _, line := range strings.Split(before, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			if !json.Valid([]byte(strings.TrimPrefix(line, "data: "))) {
				t.Fatalf("a truncated data line precedes `event: results`: %q", tail(line, 200))
			}
		}
		t.Logf("the stalled client drained in time and got the finals cleanly (%d bytes)", len(body))
	} else {
		t.Logf("the stalled client was cut without finals, as the torn-write rule requires (%d bytes)", len(body))
	}
	// Either way the body must END: a subscriber is never left hanging.
	if _, err := br.Read(make([]byte, 1)); err == nil {
		t.Error("the stalled client's body never ended")
	}

	// The live client gets the whole teardown, in order.
	//
	// liveDone must NOT end the collection on its own: the reader goroutine
	// closes it the moment the body ends, and select would then be free to take
	// that branch over events already sitting in the channel. Draining after it
	// fires is the difference between this test passing because the ordering
	// held and passing because the scheduler was kind.
	var seen []string
	timeout := time.After(15 * time.Second)
	ended := false
collect:
	for {
		select {
		case name := <-liveEvents:
			seen = append(seen, name)
			if name == "shutdown" {
				break collect
			}
		case <-liveDone:
			ended = true
			for {
				select {
				case name := <-liveEvents:
					seen = append(seen, name)
				default:
					break collect
				}
			}
		case <-timeout:
			break collect
		}
	}
	if len(seen) < 2 || seen[len(seen)-1] != "shutdown" || seen[len(seen)-2] != "results" {
		t.Fatalf("the live SSE client must end with results then shutdown, got %v", seen)
	}
	if !ended {
		select {
		case <-liveDone:
		case <-time.After(5 * time.Second):
			t.Error("the live client's body never ended after shutdown")
		}
	}

	if code := m.wait(20 * time.Second); code != 0 {
		t.Logf("magmux exited %d (SIGTERM)", code)
	}
	m.requireSilentStdout()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ── the invariants --listen must not break ──────────────────────────────────

// TestHeadlessWithListenStillWritesNothingToStdout. The zero-stdout rule has no
// exceptions, and --listen adds two new things that want to print: the startup
// line, and net/http's own error log.
func TestHeadlessWithListenStillWritesNothingToStdout(t *testing.T) {
	m := startRemoteMagmux(t, "-w", "-e", "echo hi", "-e", "echo there")

	// Provoke net/http into logging: a request that is not HTTP at all.
	conn, err := net.DialTimeout("tcp", m.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("this is not a request\r\n\r\n"))
	conn.Close()

	if code := m.wait(30 * time.Second); code != 0 {
		t.Fatalf("exit code %d\nstderr: %s", code, m.stderr.String())
	}
	m.requireSilentStdout()

	// And the startup line went to stderr, where it belongs, without the token
	// in it.
	errOut := m.stderr.String()
	// net/http's own diagnostics must not be there either. magmux may be
	// holding a raw-mode terminal, and a "http: ..." line printed into it
	// corrupts the frame with no way to repaint; ErrorLog is routed to dbgFile
	// or discarded for exactly that reason.
	if strings.Contains(errOut, "http: ") {
		t.Errorf("net/http's ErrorLog reached stderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "listening on") {
		t.Errorf("magmux must say where it is listening: %q", errOut)
	}
	if strings.Contains(errOut, m.token) {
		t.Fatal("the token must never be printed; the file path is printed instead")
	}
	if !strings.Contains(errOut, ".token") {
		t.Errorf("magmux must name the token file: %q", errOut)
	}
}

// TestGeneratedTokenFileIsRemovedAtExit, and the socket's own sweep cleans up
// after a magmux that never got to.
func TestGeneratedTokenFileIsRemovedAtExit(t *testing.T) {
	m := startRemoteMagmux(t, "-w", "-e", "true")
	path := filepath.Join(m.dir, fmt.Sprintf("magmux-%d.token", m.cmd.Process.Pid))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the token file must exist while magmux runs: %v", err)
	}
	if code := m.wait(30 * time.Second); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, m.stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the generated token file outlived magmux: %v", err)
	}
}

// TestListenWithNoTokenSourceIsStillProtected: the default is not "open".
func TestListenWithNoTokenSourceIsStillProtected(t *testing.T) {
	m := startRemoteMagmux(t, "-e", "sh")
	resp, body := m.req(t, "GET", "/v1/panes", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a magmux that generated its own token must still demand it: %d %v", resp.StatusCode, body)
	}
	if resp, _ := m.req(t, "GET", "/v1/panes", m.token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the generated token must work: %d", resp.StatusCode)
	}
}

// TestBadTLSHandshakeWritesNothingToTheTerminal is the raw-mode rule at the process
// boundary, which is the only place it can really be checked: net/http reports
// a failed handshake through http.Server.ErrorLog, and a Server built with a
// nil ErrorLog writes to log.Default(), which is stderr. magmux may be holding
// an alternate screen; a line printed into it corrupts the frame and there is
// no repaint that can fix it.
func TestBadTLSHandshakeWritesNothingToTheTerminal(t *testing.T) {
	dir := sockTestDir(t)
	certFile, keyFile := writeTestCertPair(t, dir)

	h := startHeadlessMagmux(t, "--headless", "--listen", "127.0.0.1:0",
		"--tls-cert", certFile, "--tls-key", keyFile, "-e", "sh")

	addr := ""
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && addr == "" {
		if m := listenLine.FindStringSubmatch(h.stderr.String()); m != nil {
			addr = strings.TrimPrefix(m[1], "https://")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("no listener announced\nstderr: %s", h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "https://") {
		t.Errorf("a TLS bind must announce itself as https:\n%s", h.stderr.String())
	}
	// A TLS bind is not insecure, so it must NOT warn even though the address
	// could be any interface.
	if strings.Contains(h.stderr.String(), "WARNING") {
		t.Errorf("a TLS bind must not warn:\n%s", h.stderr.String())
	}
	before := h.stderr.String()

	// Three ways to fail a handshake: plain HTTP, bytes that are not a TLS
	// record at all, and a truncated ClientHello.
	for _, junk := range []string{
		"GET /v1/panes HTTP/1.1\r\nHost: x\r\n\r\n",
		"\xde\xad\xbe\xef",
		"\x16\x03\x01\x00\x05\x01\x00",
	} {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte(junk))
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = c.Read(make([]byte, 64))
		c.Close()
	}
	time.Sleep(500 * time.Millisecond)

	after := h.stderr.String()
	if after != before {
		t.Fatalf("a failed TLS handshake reached stderr:\n%q", strings.TrimPrefix(after, before))
	}
	h.requireSilentStdout()
}

// writeTestCertPair mints a throwaway self-signed certificate for 127.0.0.1.
// Generated rather than committed: a checked-in key is a key, and a fixture
// certificate with a fixed expiry is a test that starts failing on a date
// nobody wrote down.
func writeTestCertPair(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "magmux-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// TestEnvTokenIsUsedAndNeverWritten, over the real binary.
//
// The unit test proves Resolve does not write it; this proves the whole
// process does not, which is the claim that matters — a token in the
// environment is the operator's to store, and magmux writing a copy into /tmp
// would quietly create a second place it can leak from.
func TestEnvTokenIsUsedAndNeverWritten(t *testing.T) {
	tok, _ := auth.Generate()
	t.Setenv("MAGMUX_TOKEN", tok)

	h := startHeadlessMagmux(t, "--headless", "--listen", "127.0.0.1:0", "-e", "sh")
	base := ""
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && base == "" {
		if m := listenLine.FindStringSubmatch(h.stderr.String()); m != nil {
			base = m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	if base == "" {
		t.Fatalf("no listener announced\nstderr: %s", h.stderr.String())
	}

	// No token file anywhere in the socket directory.
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".token") {
			b, _ := os.ReadFile(filepath.Join(h.dir, e.Name()))
			t.Fatalf("MAGMUX_TOKEN was written to %s (%q)", e.Name(), b)
		}
	}
	// And magmux does not offer to point anyone at one.
	if strings.Contains(h.stderr.String(), ".token") {
		t.Errorf("magmux named a token file it does not own:\n%s", h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), tok) {
		t.Fatal("the token must never be printed")
	}

	// The env token is what authenticates.
	req, _ := http.NewRequest("GET", base+"/v1/panes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MAGMUX_TOKEN did not authenticate: %d", resp.StatusCode)
	}
}

// TestNonLoopbackWithoutTLSWarnsBeforeInit is S3. The warning is the whole
// mitigation for a bind magmux cannot refuse — a Tailscale interface is a
// legitimate place to listen and magmux cannot tell one from a café LAN.
func TestNonLoopbackWithoutTLSWarnsBeforeInit(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless", "--listen", "0.0.0.0:0", "-w", "-e", "true")
	h.wait(30 * time.Second)
	out := h.stderr.String()
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "clear text") {
		t.Fatalf("a non-loopback bind without TLS must warn loudly:\n%s", out)
	}
	if !strings.Contains(out, "--tls-cert") {
		t.Errorf("the warning must say what to do instead:\n%s", out)
	}
	h.requireSilentStdout()

	// And a loopback bind does NOT warn: an alarm that fires on the ordinary
	// case is an alarm nobody reads.
	q := startRemoteMagmux(t, "-w", "-e", "true")
	q.wait(30 * time.Second)
	if strings.Contains(q.stderr.String(), "WARNING") {
		t.Errorf("a loopback bind must not warn:\n%s", q.stderr.String())
	}
}

// TestBadRemoteConfigurationExitsBeforeInit: every pre-init() failure is a
// plain line on stderr and an exit 1, while magmux still owns a normal
// terminal.
func TestBadRemoteConfigurationExitsBeforeInit(t *testing.T) {
	dir := sockTestDir(t)
	loose := filepath.Join(dir, "loose.token")
	tok, _ := auth.Generate()
	if err := os.WriteFile(loose, []byte(tok), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a token file anyone can read", []string{"--token-file", loose}, "0600"},
		{"a token file that does not exist", []string{"--token-file", filepath.Join(dir, "nope")}, "token file"},
		{"--tls-cert without --tls-key", []string{"--tls-cert", "/nonexistent.pem"}, "together"},
		{"a TLS pair that cannot be loaded", []string{"--tls-cert", "/no.pem", "--tls-key", "/no.key"}, "tls certificate"},
		{"a port already in use", nil, "listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--headless", "--listen", "127.0.0.1:0", "-w", "-e", "true"}, tc.args...)
			if tc.name == "a port already in use" {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				args = []string{"--headless", "--listen", ln.Addr().String(), "-w", "-e", "true"}
			}
			h := startHeadlessMagmux(t, args...)
			code := h.wait(20 * time.Second)
			if code == 0 {
				t.Fatalf("magmux started anyway\nstderr: %s", h.stderr.String())
			}
			if !strings.Contains(h.stderr.String(), tc.want) {
				t.Errorf("stderr must say why, want %q:\n%s", tc.want, h.stderr.String())
			}
			h.requireSilentStdout()
			// No token file is left behind by a start that failed.
			entries, _ := os.ReadDir(h.dir)
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".token") && strings.Contains(e.Name(), fmt.Sprint(h.cmd.Process.Pid)) {
					t.Errorf("a failed start left %s behind", e.Name())
				}
			}
		})
	}
}

// TestPaneEnvCarriesNoSecrets sits beside TestChildIsToldTheResolvedTheme: a
// pane is a shell, and the remote-control token is remote code execution on
// every pane in the session. A child that could read it could drive its
// siblings.
func TestPaneEnvCarriesNoSecrets(t *testing.T) {
	for _, name := range paneSecrets {
		t.Setenv(name, "SHOULD-NOT-BE-INHERITED-"+name)
	}
	t.Setenv("MAGMUX_SOCK", "/tmp/magmux-test.sock")
	t.Setenv("MAGMUX_NOT_A_SECRET", "keep-me")

	env := paneEnviron()
	for _, kv := range env {
		for _, secret := range paneSecrets {
			if strings.HasPrefix(kv, secret+"=") {
				t.Errorf("a pane inherited %s", secret)
			}
		}
		if strings.Contains(kv, "SHOULD-NOT-BE-INHERITED") {
			t.Errorf("a secret's VALUE survived under another name: %q", kv)
		}
	}

	// The denylist is a denylist, not an allowlist: a pane is a login shell and
	// its environment is the user's.
	var sawOther, sawSock bool
	for _, kv := range env {
		if kv == "MAGMUX_NOT_A_SECRET=keep-me" {
			sawOther = true
		}
		if strings.HasPrefix(kv, "MAGMUX_SOCK=") {
			sawSock = true
		}
	}
	if !sawOther {
		t.Error("an ordinary variable must still be inherited")
	}
	if !sawSock {
		t.Error("MAGMUX_SOCK is how a child talks back to magmux and must NOT be filtered")
	}
}
