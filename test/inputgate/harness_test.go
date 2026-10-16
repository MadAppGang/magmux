package inputgate

// Black-box harness: a real magmux binary, started headless, driven over its
// unix socket with newline-delimited JSON, with the fake Claude child as the
// pane. Nothing here knows how magmux is built; it speaks only the wire
// contract (contract/architecture-contracts.md §5) and reads the fake child's
// own log of what reached its terminal.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	cols = 100
	rows = 40

	// replyTimeout bounds one request. It exceeds the largest readiness
	// deadline a test asks for (default 10s) plus the acceptance window.
	replyTimeout = 45 * time.Second
	startTimeout = 15 * time.Second
	pollEvery    = 50 * time.Millisecond
)

// Contract §5.4 vocabulary.
const (
	codeBusy        = "busy"
	codeNotAccepted = "not_accepted"
	codeBadRequest  = "bad_request"
	codePaneDead    = "pane_dead"
)

// gateResultKeys are the keys an UNGATED send reply must never carry (§5.4:
// "keys added only when gated; an ungated reply is byte-identical to today").
var gateResultKeys = []string{"waitedMs", "dismissed", "verified", "accept", "evidence"}

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

func requireBins(t *testing.T) {
	t.Helper()
	if builtBin == "" || agentBin == "" || directBin == "" {
		t.Fatalf("binaries not built: %v", buildErr)
	}
}

func shortID(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "ig" + hex.EncodeToString(b[:])
}

// tmpDir is a short directory under /tmp (unix socket paths are limited to
// ~104 bytes, and see main_test.go for why never under TMPDIR).
func tmpDir(t *testing.T, prefix string) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// fake configures one fake Claude child. Its settings travel in magmux's
// ENVIRONMENT, which every pane inherits, so the pane command is a bare path
// and nothing the harness adds to it can change how magmux classifies it.
type fake map[string]string

type magmux struct {
	id      string
	sock    string
	logPath string
	cmd     *exec.Cmd
	stderr  *lockedBuf
}

type opts struct {
	args []string // extra flags, in order, placed BEFORE the -e list
	tail []string // flags placed AFTER the -e list
	cmds []string // one -e per entry
	env  fake
}

func baseEnv(t *testing.T, home string, f fake) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+len(f)+6)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "COLUMNS", k == "LINES", k == "HOME", k == "CLAUDE_CONFIG_DIR",
			k == "MAGMUX_SOCK_DIR", strings.HasPrefix(k, "FAKE_"),
			k == "CLAUDE_CODE_CHILD_SESSION":
			continue
		}
		env = append(env, kv)
	}
	// HOME is isolated so the Claude controller can never find the real
	// user's transcripts or session registry and mistake them for evidence.
	env = append(env,
		fmt.Sprintf("COLUMNS=%d", cols), fmt.Sprintf("LINES=%d", rows),
		"HOME="+home, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"))
	for k, v := range f {
		env = append(env, k+"="+v)
	}
	return env
}

// start runs `magmux --headless --no-status --id X --sock-dir D [args] -e …
// [tail]` with the fake's log wired in, and returns once the socket accepts.
func start(t *testing.T, o opts) *magmux {
	t.Helper()
	requireBins(t)
	id := shortID(t)
	sockDir := tmpDir(t, "igs")
	home := tmpDir(t, "igh")
	logPath := filepath.Join(tmpDir(t, "igl"), "fake.log")

	f := fake{"FAKE_LOG": logPath}
	for k, v := range o.env {
		f[k] = v
	}
	args := []string{"--headless", "--no-status", "--id", id, "--sock-dir", sockDir}
	args = append(args, o.args...)
	for _, c := range o.cmds {
		args = append(args, "-e", c)
	}
	args = append(args, o.tail...)

	cmd := exec.Command(builtBin, args...)
	cmd.Env = baseEnv(t, home, f)
	stderr := &lockedBuf{}
	cmd.Stderr = stderr
	cmd.Stdout = &lockedBuf{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	m := &magmux{id: id, logPath: logPath, cmd: cmd, stderr: stderr}

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
		if t.Failed() {
			t.Logf("magmux %s args %q\nstderr:\n%s", id, args, stderr.String())
			t.Logf("fake child log:\n%s", rawLog(logPath))
		}
	})

	deadline := time.Now().Add(startTimeout)
	for {
		socks, _ := filepath.Glob(filepath.Join(sockDir, "*.sock"))
		for _, s := range socks {
			if c, err := net.DialTimeout("unix", s, time.Second); err == nil {
				_ = c.Close()
				m.sock = s
				return m
			}
		}
		select {
		case <-exited:
			t.Fatalf("magmux exited before its socket appeared; stderr:\n%s", stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no socket in %s accepted within %v", sockDir, startTimeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// declared starts one fake pane declared with --controller claude-code.
func declared(t *testing.T, env fake) (*magmux, *conn, int) {
	t.Helper()
	m := start(t, opts{args: []string{"--controller", "claude-code"}, cmds: []string{agentBin}, env: env})
	cn := dial(t, m)
	return m, cn, cn.sessionPane().Pane
}

// ── connection ──────────────────────────────────────────────────────────────

type reply struct {
	Type   string          `json:"type"`
	ID     json.RawMessage `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Code   string          `json:"code"`
	Reason string          `json:"reason"`
	Typed  *bool           `json:"typed"`
	Error  string          `json:"error"`
	raw    string
}

func (r reply) String() string { return strings.TrimSpace(r.raw) }

func (r reply) result(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Result, &m); err != nil {
		t.Fatalf("reply result is not an object: %v; line: %s", err, r.raw)
	}
	return m
}

// topKeys is the set of keys on the reply object itself.
func (r reply) topKeys(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.raw), &m); err != nil {
		t.Fatalf("reply is not an object: %v; line: %s", err, r.raw)
	}
	return m
}

func (r reply) typedTrue() bool { return r.Typed != nil && *r.Typed }

type conn struct {
	t    *testing.T
	c    net.Conn
	wmu  sync.Mutex
	mu   sync.Mutex
	next int
	wait map[string]chan reply
}

func dial(t *testing.T, m *magmux) *conn {
	t.Helper()
	c, err := net.DialTimeout("unix", m.sock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", m.sock, err)
	}
	cn := &conn{t: t, c: c, wait: map[string]chan reply{}}
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
	var r reply
	if json.Unmarshal(line, &r) != nil || r.Type != "reply" {
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
}

// async writes msg (with a fresh string id) and returns the channel its reply
// will arrive on. Safe to call from the test goroutine only (it may Fatal).
func (cn *conn) async(msg map[string]any) <-chan reply {
	cn.t.Helper()
	cn.mu.Lock()
	cn.next++
	id := fmt.Sprintf("q%d", cn.next)
	ch := make(chan reply, 1)
	cn.wait[id] = ch
	cn.mu.Unlock()

	body := map[string]any{"id": id}
	for k, v := range msg {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		cn.t.Fatalf("marshal: %v", err)
	}
	b = append(b, '\n')
	cn.wmu.Lock()
	_ = cn.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = cn.c.Write(b)
	cn.wmu.Unlock()
	if err != nil {
		cn.t.Fatalf("write %s: %v", b, err)
	}
	return ch
}

func (cn *conn) await(ch <-chan reply, what any) reply {
	cn.t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(replyTimeout):
		cn.t.Fatalf("no reply within %v to %v", replyTimeout, what)
		return reply{}
	}
}

func (cn *conn) request(msg map[string]any) reply {
	cn.t.Helper()
	return cn.await(cn.async(msg), msg)
}

func (cn *conn) mustOK(msg map[string]any) reply {
	cn.t.Helper()
	r := cn.request(msg)
	if !r.OK {
		cn.t.Fatalf("%v: want ok, got %s", msg, r)
	}
	return r
}

// send issues a `send` to pane with the given fields.
func (cn *conn) send(pane int, fields map[string]any) reply {
	cn.t.Helper()
	msg := map[string]any{"type": "send", "pane": pane}
	for k, v := range fields {
		msg[k] = v
	}
	return cn.request(msg)
}

// ── list ────────────────────────────────────────────────────────────────────

type inputView struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

type paneRow struct {
	Pane           int        `json:"pane"`
	State          string     `json:"state"`
	Control        bool       `json:"control"`
	Controller     string     `json:"controller"`
	GateBasis      string     `json:"gateBasis"`
	EditorOverride string     `json:"editorOverride"`
	Input          *inputView `json:"input"`
	raw            map[string]json.RawMessage
}

func (p paneRow) isControl() bool { return p.Control || p.State == "panel" }

func (cn *conn) list() []paneRow {
	cn.t.Helper()
	r := cn.mustOK(map[string]any{"type": "list"})
	var res struct {
		Panes []json.RawMessage `json:"panes"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		cn.t.Fatalf("list result: %v; line: %s", err, r.raw)
	}
	out := make([]paneRow, 0, len(res.Panes))
	for _, raw := range res.Panes {
		var p paneRow
		if err := json.Unmarshal(raw, &p); err != nil {
			cn.t.Fatalf("list row %s: %v", raw, err)
		}
		_ = json.Unmarshal(raw, &p.raw)
		out = append(out, p)
	}
	return out
}

// sessionPane is the first live, non-panel pane (there is one per test).
func (cn *conn) sessionPane() paneRow {
	cn.t.Helper()
	deadline := time.Now().Add(startTimeout)
	for {
		for _, p := range cn.list() {
			if !p.isControl() && p.State != "closed" {
				return p
			}
		}
		if time.Now().After(deadline) {
			cn.t.Fatalf("no session pane listed within %v", startTimeout)
		}
		time.Sleep(pollEvery)
	}
}

func (cn *conn) row(pane int) paneRow {
	cn.t.Helper()
	for _, p := range cn.list() {
		if p.Pane == pane {
			return p
		}
	}
	cn.t.Fatalf("pane %d not in list", pane)
	return paneRow{}
}

// waitRow polls list until pred holds for pane's row.
func (cn *conn) waitRow(pane int, within time.Duration, what string, pred func(paneRow) bool) paneRow {
	cn.t.Helper()
	deadline := time.Now().Add(within)
	var last paneRow
	for {
		last = cn.row(pane)
		if pred(last) {
			return last
		}
		if time.Now().After(deadline) {
			cn.t.Fatalf("pane %d never reached %q within %v; last row: %v", pane, what, within, last.raw)
		}
		time.Sleep(pollEvery)
	}
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

// ── the fake child's log ────────────────────────────────────────────────────

type event struct {
	T       int64    `json:"t"`
	Ev      string   `json:"ev"`
	Data    string   `json:"data"`
	Dropped bool     `json:"dropped"`
	Text    string   `json:"text"`
	Dialog  string   `json:"dialog"`
	Key     string   `json:"key"`
	Why     string   `json:"why"`
	Args    []string `json:"args"`
}

func rawLog(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func readLog(t *testing.T, path string) []event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake log: %v", err)
	}
	var out []event
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e event
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (m *magmux) events(t *testing.T) []event { return readLog(t, m.logPath) }

// waitEvent polls the fake's log until an event satisfies pred.
func (m *magmux) waitEvent(t *testing.T, within time.Duration, what string, pred func(event) bool) event {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, e := range m.events(t) {
			if pred(e) {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake child never logged %s within %v; log:\n%s", what, within, rawLog(m.logPath))
		}
		time.Sleep(pollEvery)
	}
}

// waitBooted waits until the fake has finished its boot reset (or shown its
// box/dialog when it has no reset), so a negative test is refused for the
// reason under test and not for "still starting".
func (m *magmux) waitBooted(t *testing.T) {
	t.Helper()
	m.waitEvent(t, startTimeout, "boot reset end", func(e event) bool {
		return e.Ev == "reset-end" && e.Why == "boot"
	})
}

func ofKind(evs []event, kind string) []event {
	var out []event
	for _, e := range evs {
		if e.Ev == kind {
			out = append(out, e)
		}
	}
	return out
}

// received is every byte that reached the child's terminal, dropped or not.
func received(evs []event) string {
	var b strings.Builder
	for _, e := range ofKind(evs, "in") {
		b.WriteString(e.Data)
	}
	return b.String()
}

func submits(evs []event) []string {
	var out []string
	for _, e := range ofKind(evs, "submit") {
		out = append(out, e.Text)
	}
	return out
}

func countEnter(s string) int { return strings.Count(s, "\r") + strings.Count(s, "\n") }

// ── assertions ──────────────────────────────────────────────────────────────

func wantBusy(t *testing.T, r reply, reasons ...string) {
	t.Helper()
	if r.OK || r.Code != codeBusy {
		t.Fatalf("want error code %q (reason in %v), got %s", codeBusy, reasons, r)
	}
	if r.Reason == "" {
		t.Fatalf("busy reply carries no reason: %s", r)
	}
	if len(reasons) > 0 && !slices.Contains(reasons, r.Reason) {
		t.Fatalf("busy reason = %q; want one of %v; reply: %s", r.Reason, reasons, r)
	}
	if r.typedTrue() {
		t.Errorf("a gate refusal must not claim typed:true; reply: %s", r)
	}
}

func wantNothingReceived(t *testing.T, m *magmux) {
	t.Helper()
	if got := received(m.events(t)); got != "" {
		t.Fatalf("the child received %q; want nothing written to the pane", got)
	}
}

func wantGatedOK(t *testing.T, r reply) map[string]any {
	t.Helper()
	if !r.OK {
		t.Fatalf("want ok, got %s", r)
	}
	res := r.result(t)
	if _, ok := res["waitedMs"].(float64); !ok {
		t.Errorf("gated ok reply lacks numeric waitedMs: %s", r)
	}
	return res
}

func wantField(t *testing.T, res map[string]any, key string, want any, r reply) {
	t.Helper()
	if got := res[key]; got != want {
		t.Errorf("result.%s = %v; want %v; reply: %s", key, got, want, r)
	}
}

// runCLI runs magmux with args to completion (bounded) and returns its exit
// code and stderr.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	requireBins(t)
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, builtBin, args...)
	cmd.Env = baseEnv(t, tmpDir(t, "igh"), nil)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("magmux %q did not exit within %v (it should refuse at startup); stderr:\n%s", args, startTimeout, stderr.String())
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run magmux: %v", err)
	}
	return code, stderr.String()
}

// settingsArgs extracts every --settings value from argv, in both the
// `--settings V` and `--settings=V` forms.
func settingsArgs(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--settings" && i+1 < len(argv) {
			out = append(out, argv[i+1])
			i++
		} else if v, ok := strings.CutPrefix(a, "--settings="); ok {
			out = append(out, v)
		}
	}
	return out
}

// settingsObject decodes one --settings value: inline JSON, or a file path.
func settingsObject(t *testing.T, v string) map[string]any {
	t.Helper()
	b := []byte(v)
	if s := strings.TrimSpace(v); !strings.HasPrefix(s, "{") {
		fb, err := os.ReadFile(v)
		if err != nil {
			t.Fatalf("--settings %q is neither JSON nor a readable file: %v", v, err)
		}
		b = fb
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("--settings value %q is not a JSON object: %v", b, err)
	}
	return m
}
