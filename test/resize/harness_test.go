package resize_test

// Black-box harness: a real magmux binary, started headless, driven over its
// unix socket with newline-delimited JSON. Nothing here knows how magmux is
// built; it only speaks the wire contract.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

const (
	startCols = 160
	startRows = 50

	// sizeChild prints `stty size` once at start and again on every SIGWINCH.
	sizeChild = `trap "stty size" WINCH; stty size; while :; do sleep 0.1; done`

	replyTimeout = 10 * time.Second
	readyTimeout = 15 * time.Second
	// quietWindow is how long an ABSENCE check waits (no SIGWINCH line, no
	// keyframe). The child's loop wakes every 100ms, so a delivered SIGWINCH
	// would print well inside it.
	quietWindow = 1500 * time.Millisecond
)

// ── process ─────────────────────────────────────────────────────────────────

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type magmux struct {
	id     string
	sock   string
	cmd    *exec.Cmd
	stderr *lockedBuf
}

func binPath(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("MAGMUX_BIN"); bin != "" {
		return bin
	}
	if builtBin == "" {
		t.Fatalf("no magmux binary: MAGMUX_BIN is unset and TestMain could not build one: %v", buildErr)
	}
	return builtBin
}

func shortID(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "rz" + hex.EncodeToString(b[:])
}

// startMagmux runs `COLUMNS=160 LINES=50 magmux --headless --no-status --id X
// [extra...] -e c1 [-e c2...]` and returns once the socket accepts.
func startMagmux(t *testing.T, extra []string, cmds ...string) *magmux {
	t.Helper()
	bin := binPath(t)
	id := shortID(t)
	args := []string{"--headless", "--no-status", "--id", id}
	args = append(args, extra...)
	for _, c := range cmds {
		args = append(args, "-e", c)
	}
	cmd := exec.Command(bin, args...)
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "COLUMNS=") || strings.HasPrefix(kv, "LINES=") ||
			strings.HasPrefix(kv, "MAGMUX_SOCK_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, fmt.Sprintf("COLUMNS=%d", startCols), fmt.Sprintf("LINES=%d", startRows))
	cmd.Env = env
	stderr := &lockedBuf{}
	cmd.Stderr = stderr
	cmd.Stdout = &lockedBuf{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	m := &magmux{id: id, sock: "/tmp/magmux-" + id + ".sock", cmd: cmd, stderr: stderr}

	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		_ = os.Remove(m.sock)
		if t.Failed() {
			t.Logf("magmux %s stderr:\n%s", id, stderr.String())
		}
	})

	deadline := time.Now().Add(readyTimeout)
	for {
		c, err := net.DialTimeout("unix", m.sock, time.Second)
		if err == nil {
			_ = c.Close()
			return m
		}
		select {
		case <-exited:
			t.Fatalf("magmux exited before its socket appeared; stderr:\n%s", stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s did not accept within %v: %v", m.sock, readyTimeout, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ── connection ──────────────────────────────────────────────────────────────

type reply struct {
	Type   string          `json:"type"`
	ID     json.RawMessage `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Code   string          `json:"code"`
	Error  string          `json:"error"`
	raw    string
}

func (r reply) resultMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Result, &m); err != nil {
		t.Fatalf("reply result is not an object: %v; line: %s", err, r.raw)
	}
	return m
}

type conn struct {
	t      *testing.T
	c      net.Conn
	mu     sync.Mutex
	next   int
	wait   map[string]chan reply
	frames chan protocol.Frame
}

func dial(t *testing.T, m *magmux) *conn {
	t.Helper()
	c, err := net.DialTimeout("unix", m.sock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", m.sock, err)
	}
	cn := &conn{t: t, c: c, wait: map[string]chan reply{}, frames: make(chan protocol.Frame, 4096)}
	t.Cleanup(func() { _ = c.Close() })
	go cn.readLoop()
	return cn
}

func (cn *conn) readLoop() {
	br := bufio.NewReaderSize(cn.c, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			cn.dispatch(line)
		}
		if err != nil {
			return
		}
	}
}

func (cn *conn) dispatch(line []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &head) != nil {
		return
	}
	switch head.Type {
	case protocol.EventReply:
		var r reply
		if json.Unmarshal(line, &r) != nil {
			return
		}
		r.raw = string(line)
		var id string
		if json.Unmarshal(r.ID, &id) != nil {
			return
		}
		cn.mu.Lock()
		ch := cn.wait[id]
		delete(cn.wait, id)
		cn.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	case protocol.EventFrame:
		var f protocol.Frame
		if json.Unmarshal(line, &f) != nil {
			return
		}
		select {
		case cn.frames <- f:
		default: // a test that is not reading frames must not wedge the reader
		}
	}
}

// sendLine writes a raw JSON object with "id" spliced in, so a test can put
// exact bytes on the wire (40.0, "40", true) that a Go map would normalise.
func (cn *conn) sendLine(body string) reply {
	cn.t.Helper()
	cn.mu.Lock()
	cn.next++
	id := fmt.Sprintf("q%d", cn.next)
	ch := make(chan reply, 1)
	cn.wait[id] = ch
	cn.mu.Unlock()

	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "{") || !strings.HasSuffix(body, "}") {
		cn.t.Fatalf("sendLine wants a JSON object, got %q", body)
	}
	inner := strings.TrimSpace(body[1 : len(body)-1])
	line := fmt.Sprintf(`{"id":%q`, id)
	if inner != "" {
		line += "," + inner
	}
	line += "}\n"
	_ = cn.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := cn.c.Write([]byte(line)); err != nil {
		cn.t.Fatalf("write %s: %v", line, err)
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(replyTimeout):
		cn.t.Fatalf("no reply within %v to %s", replyTimeout, strings.TrimSpace(line))
		return reply{}
	}
}

func (cn *conn) request(msg map[string]any) reply {
	cn.t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		cn.t.Fatalf("marshal: %v", err)
	}
	return cn.sendLine(string(b))
}

func (cn *conn) mustOK(msg map[string]any) reply {
	cn.t.Helper()
	r := cn.request(msg)
	if !r.OK {
		cn.t.Fatalf("%v: want ok, got code=%q error=%q", msg, r.Code, r.Error)
	}
	return r
}

// ── typed views over the wire ───────────────────────────────────────────────

type resizeResult struct {
	Rows    *int  `json:"rows"`
	Cols    *int  `json:"cols"`
	Changed *bool `json:"changed"`
	Manual  *bool `json:"manual"`
}

// resizeOK sends a resize and requires success with ALL four result fields.
func (cn *conn) resizeOK(msg map[string]any) resizeResult {
	cn.t.Helper()
	msg["type"] = "resize"
	r := cn.mustOK(msg)
	return decodeResize(cn.t, r)
}

func decodeResize(t *testing.T, r reply) resizeResult {
	t.Helper()
	var res resizeResult
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatalf("resize result does not decode: %v; line: %s", err, r.raw)
	}
	if res.Rows == nil || res.Cols == nil || res.Changed == nil || res.Manual == nil {
		t.Fatalf("resize result must carry rows, cols, changed and manual; line: %s", r.raw)
	}
	return res
}

func assertResize(t *testing.T, got resizeResult, rows, cols int, changed, manual bool) {
	t.Helper()
	if *got.Rows != rows || *got.Cols != cols || *got.Changed != changed || *got.Manual != manual {
		t.Fatalf("resize result = {rows:%d cols:%d changed:%v manual:%v}; want {rows:%d cols:%d changed:%v manual:%v}",
			*got.Rows, *got.Cols, *got.Changed, *got.Manual, rows, cols, changed, manual)
	}
}

type caps struct {
	Rows     int      `json:"rows"`
	Cols     int      `json:"cols"`
	Verbs    []string `json:"verbs"`
	Protocol int      `json:"protocol"`
}

func (cn *conn) capabilities() caps {
	cn.t.Helper()
	r := cn.mustOK(map[string]any{"type": "capabilities"})
	var c caps
	if err := json.Unmarshal(r.Result, &c); err != nil {
		cn.t.Fatalf("capabilities result: %v; line: %s", err, r.raw)
	}
	return c
}

func (cn *conn) capsSize() (int, int) {
	cn.t.Helper()
	c := cn.capabilities()
	return c.Rows, c.Cols
}

type paneRow struct {
	Pane    int    `json:"pane"`
	State   string `json:"state"`
	Control bool   `json:"control"`
	Rows    int    `json:"rows"`
	Cols    int    `json:"cols"`
}

func (p paneRow) isControl() bool { return p.Control || p.State == "panel" }

func (cn *conn) list() []paneRow {
	cn.t.Helper()
	r := cn.mustOK(map[string]any{"type": "list"})
	var res struct {
		Panes []paneRow `json:"panes"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		cn.t.Fatalf("list result: %v; line: %s", err, r.raw)
	}
	return res.Panes
}

func (cn *conn) capture(pane int) string {
	cn.t.Helper()
	r := cn.mustOK(map[string]any{"type": "capture", "pane": pane})
	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		cn.t.Fatalf("capture result: %v; line: %s", err, r.raw)
	}
	return res.Text
}

// paneSize is the geometry magmux reports for one pane, read from the `watch`
// reply (protocol.WatchInfo), the documented place a client learns a pane's
// size before its first frame. The watch is dropped again immediately.
func (cn *conn) paneSize(pane int) (int, int) {
	cn.t.Helper()
	r := cn.mustOK(map[string]any{"type": "watch", "pane": pane, "mode": string(protocol.WatchNotify)})
	var info protocol.WatchInfo
	if err := json.Unmarshal(r.Result, &info); err != nil {
		cn.t.Fatalf("watch result: %v; line: %s", err, r.raw)
	}
	cn.mustOK(map[string]any{"type": "unwatch", "pane": pane})
	return info.Rows, info.Cols
}

// ── the child's own view of its size ────────────────────────────────────────

var sizeLine = regexp.MustCompile(`(?m)^\s*(\d+) (\d+)\s*$`)

// sizeLines returns every "ROWS COLS" line the child has printed, in order.
func sizeLines(text string) []string {
	var out []string
	for _, m := range sizeLine.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1]+" "+m[2])
	}
	return out
}

// waitChildSees polls capture until the LAST size line the child printed is
// "rows cols".
func (cn *conn) waitChildSees(pane, rows, cols int) {
	cn.t.Helper()
	want := fmt.Sprintf("%d %d", rows, cols)
	deadline := time.Now().Add(readyTimeout)
	var text string
	for time.Now().Before(deadline) {
		r := cn.request(map[string]any{"type": "capture", "pane": pane})
		if r.OK {
			var res struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(r.Result, &res)
			text = res.Text
			if l := sizeLines(text); len(l) > 0 && l[len(l)-1] == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	cn.t.Fatalf("child in pane %d never printed %q (stty size); last capture:\n%s", pane, want, text)
}

// ── frames ──────────────────────────────────────────────────────────────────

func (cn *conn) drainFrames() {
	for {
		select {
		case <-cn.frames:
		default:
			return
		}
	}
}

// waitKeyframe waits for a key:true frame of pane at rows x cols.
func (cn *conn) waitKeyframe(pane, rows, cols int, within time.Duration) protocol.Frame {
	cn.t.Helper()
	timeout := time.After(within)
	var seen []string
	for {
		select {
		case f := <-cn.frames:
			seen = append(seen, fmt.Sprintf("{pane:%d key:%v %dx%d}", f.Pane, f.Key, f.Rows, f.Cols))
			if f.Pane == pane && f.Key && f.Rows == rows && f.Cols == cols {
				return f
			}
		case <-timeout:
			cn.t.Fatalf("no keyframe for pane %d at %dx%d within %v; frames seen: %v", pane, rows, cols, within, seen)
			return protocol.Frame{}
		}
	}
}

// noKeyframeFor fails if a key:true frame for pane arrives within d.
func (cn *conn) noKeyframeFor(pane int, d time.Duration) {
	cn.t.Helper()
	timeout := time.After(d)
	for {
		select {
		case f := <-cn.frames:
			if f.Pane == pane && f.Key {
				cn.t.Fatalf("got a keyframe (%dx%d, seq %d) where none is allowed", f.Rows, f.Cols, f.Seq)
			}
		case <-timeout:
			return
		}
	}
}

// ── geometry snapshots for the byte-identical rule ──────────────────────────

type worldState struct {
	capsRows, capsCols int
	panes              string // every list row's pane/rows/cols, in order
	text               map[int]string
}

func (cn *conn) world() worldState {
	cn.t.Helper()
	w := worldState{text: map[int]string{}}
	w.capsRows, w.capsCols = cn.capsSize()
	var b strings.Builder
	for _, p := range cn.list() {
		fmt.Fprintf(&b, "[%d %dx%d control=%v] ", p.Pane, p.Rows, p.Cols, p.isControl())
		if !p.isControl() && p.State != "closed" {
			w.text[p.Pane] = cn.capture(p.Pane)
		}
	}
	w.panes = b.String()
	return w
}

func assertSameWorld(t *testing.T, label string, before, after worldState) {
	t.Helper()
	if before.capsRows != after.capsRows || before.capsCols != after.capsCols {
		t.Errorf("%s: capabilities size changed %dx%d -> %dx%d", label,
			before.capsRows, before.capsCols, after.capsRows, after.capsCols)
	}
	if before.panes != after.panes {
		t.Errorf("%s: pane geometry changed\n before: %s\n after:  %s", label, before.panes, after.panes)
	}
	for id, txt := range before.text {
		if after.text[id] != txt {
			t.Errorf("%s: pane %d screen contents changed\n before:\n%s\n after:\n%s", label, id, txt, after.text[id])
		}
	}
}

var jsonUnmarshal = json.Unmarshal
