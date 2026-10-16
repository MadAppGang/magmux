package mux

// The Phase 5 harness: a scripted InputGate (fakeGate), a real Claude
// controller whose screen is swapped at chosen moments (scriptedClaude), a
// child-side byte log, and the helpers that run one send and wait for it.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── the child's side of the PTY ─────────────────────────────────────────────

// byteLog drains the child's end of a pane's pipe for the whole test, so a
// long send never fills the pipe, and keeps everything it read.
type byteLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func logChild(t *testing.T, child *os.File) *byteLog {
	t.Helper()
	l := &byteLog{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := child.Read(b)
			if n > 0 {
				l.mu.Lock()
				l.buf.Write(b[:n])
				l.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return l
}

func (l *byteLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// settle gives the drain goroutine a moment to read what is already written.
func (l *byteLog) settle() string {
	time.Sleep(30 * time.Millisecond)
	return l.String()
}

func (l *byteLog) waitFor(t *testing.T, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s := l.String(); strings.Contains(s, want) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the child never received %q; it got %q", want, l.String())
	return ""
}

// ── a scripted gate ─────────────────────────────────────────────────────────

// fakeGate is an InputGate (and a ToolController) whose answers are scripted
// per call. A nil script answers ready / verified exact / taken.
type fakeGate struct {
	mu       sync.Mutex
	readyFn  func(q ReadinessQuery, n int) InputReadiness
	checkFn  func(want BoxExpect, n int) BoxCheck
	acceptFn func(b AcceptBaseline, n int) AcceptVerdict
	win      time.Duration // the acceptance window (0 = the real one)

	nReady, nCheck, nAccept int
	queries                 []ReadinessQuery
	expects                 []BoxExpect
	events                  []string // "ready", "check", "accept", "notify", in order
	log                     *byteLog // when set, every event records the bytes seen so far
	seen                    []string // log.String() at each event
	notified                atomic.Int32
}

var (
	_ InputGate     = (*fakeGate)(nil)
	_ InputNotifier = (*fakeGate)(nil)
)

func (f *fakeGate) Name() string                   { return claudeCodeName }
func (f *fakeGate) Start(context.Context) error    { return nil }
func (f *fakeGate) Poll() (Snapshot, error)        { return Snapshot{}, nil }
func (f *fakeGate) Stop() error                    { return nil }
func (f *fakeGate) acceptWindowFor() time.Duration { return f.win }

func (f *fakeGate) note(ev string) {
	f.events = append(f.events, ev)
	if f.log != nil {
		f.seen = append(f.seen, f.log.settleQuick())
	}
}

func (f *fakeGate) NotifyInput() {
	f.mu.Lock()
	f.note("notify")
	f.mu.Unlock()
	f.notified.Add(1)
}

func (f *fakeGate) InputReadiness(q ReadinessQuery) InputReadiness {
	f.mu.Lock()
	n := f.nReady
	f.nReady++
	f.queries = append(f.queries, q)
	f.note("ready")
	fn := f.readyFn
	f.mu.Unlock()
	if fn == nil {
		return readyNow()
	}
	r := fn(q, n)
	r.Gated = true
	return r
}

func (f *fakeGate) CheckBox(want BoxExpect) BoxCheck {
	f.mu.Lock()
	n := f.nCheck
	f.nCheck++
	f.expects = append(f.expects, want)
	f.note("check")
	fn := f.checkFn
	f.mu.Unlock()
	if fn == nil {
		return verifiedBox(verifiedExact)
	}
	return fn(want, n)
}

func (f *fakeGate) InputAccepted(b AcceptBaseline) AcceptVerdict {
	f.mu.Lock()
	n := f.nAccept
	f.nAccept++
	f.note("accept")
	fn := f.acceptFn
	f.mu.Unlock()
	if fn == nil {
		return AcceptVerdict{State: AcceptTaken, Evidence: evidenceHistory}
	}
	return fn(b, n)
}

func (f *fakeGate) counts() (ready, check, accept int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nReady, f.nCheck, f.nAccept
}

func (f *fakeGate) eventLog() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...), append([]string(nil), f.seen...)
}

func (f *fakeGate) lastExpect() BoxExpect {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.expects) == 0 {
		return BoxExpect{}
	}
	return f.expects[len(f.expects)-1]
}

// settleQuick is String without the sleep, for use under fakeGate.mu.
func (l *byteLog) settleQuick() string { return l.String() }

func readyNow() InputReadiness { return InputReadiness{Gated: true, Ready: true} }

func notReady(reason string) InputReadiness {
	return InputReadiness{Gated: true, Reason: reason, Detail: "scripted"}
}

func dialogUp(kind string, dismissable bool) InputReadiness {
	r := notReady("dialog:" + kind)
	r.Dismissable = dismissable
	return r
}

func verifiedBox(how string) BoxCheck {
	return BoxCheck{Readiness: readyNow(), Verdict: BoxVerified, Verified: how,
		Baseline: AcceptBaseline{At: time.Now(), VerifiedKey: "k", SentKey: "k"}}
}

func checkNotReady(r InputReadiness) BoxCheck { return BoxCheck{Readiness: r} }

// ── panes ───────────────────────────────────────────────────────────────────

// gatedMux is a magmux with one GATED pane whose controller is g, and the log
// of what its child received.
func gatedMux(t *testing.T, g ToolController) (*Magmux, *Pane, *byteLog) {
	t.Helper()
	m := &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel()}
	p := newScrollPane(20, 60)
	child := attachPipe(t, p)
	p.gateBasis = gateDeclared
	p.controller = g
	m.root, m.focused = p, p
	m.allPanes = []*Pane{p}
	m.stampPaneIDs()
	l := logChild(t, child)
	if fg, ok := g.(*fakeGate); ok {
		fg.log = l
	}
	return m, p, l
}

// gateSockPane declares p gated with controller g on a magmux that is already
// serving (sockMux). In production both fields are written before
// publication; here the server goroutines already exist, so the write is
// ordered under treeMu, which every reader of the pane table takes.
func gateSockPane(m *Magmux, p *Pane, g ToolController) {
	m.treeMu.Lock()
	p.gateBasis, p.controller = gateDeclared, g
	m.treeMu.Unlock()
}

// attachPipe gives a pane a pipe for a PTY; returns the child end.
func attachPipe(t *testing.T, p *Pane) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	p.ptmx = w
	return r
}

// sendOutcome is one finished send.
type sendOutcome struct {
	rep     sendReport
	err     error
	elapsed time.Duration
}

// startSend admits r and returns a channel that yields its outcome. An admit
// refusal is yielded at once.
func startSend(t *testing.T, m *Magmux, ctx context.Context, r sendRequest) <-chan sendOutcome {
	t.Helper()
	out := make(chan sendOutcome, 1)
	start := time.Now()
	if err := m.sendRequestVia(ctx, nil, r, func(rep sendReport, err error) {
		out <- sendOutcome{rep: rep, err: err, elapsed: time.Since(start)}
	}); err != nil {
		out <- sendOutcome{err: err, elapsed: time.Since(start)}
	}
	return out
}

// runSend runs one send to completion.
func runSend(t *testing.T, m *Magmux, r sendRequest) sendOutcome {
	t.Helper()
	return awaitSend(t, startSend(t, m, context.Background(), r), 60*time.Second)
}

func awaitSend(t *testing.T, ch <-chan sendOutcome, within time.Duration) sendOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(within):
		t.Fatalf("the send never finished within %v", within)
	}
	return sendOutcome{}
}

// wantErr checks a send's error by code, reason and typed flag.
func wantErr(t *testing.T, o sendOutcome, code, reason string, typed bool) {
	t.Helper()
	if got := verbErrCode(o.err); got != code {
		t.Fatalf("code = %q (%v), want %q", got, o.err, code)
	}
	var e *sockErr
	if ok := asSockErr(o.err, &e); !ok {
		t.Fatalf("error %T is not a protocol.Error", o.err)
	}
	if e.Reason != reason || e.Typed != typed {
		t.Fatalf("reason=%q typed=%v (%v), want %q %v", e.Reason, e.Typed, o.err, reason, typed)
	}
}

func asSockErr(err error, out **sockErr) bool {
	e, ok := err.(*sockErr)
	if ok {
		*out = e
	}
	return ok
}

// lastAck is the panel's ack text for the newest OUT row.
func lastAck(m *Magmux) string {
	sigs := panelSignals(m.control)
	for i := len(sigs) - 1; i >= 0; i-- {
		if sigs[i].ok != nil {
			return sigs[i].ackText
		}
	}
	return ""
}

func outRows(m *Magmux) int { return len(panelSignals(m.control)) }

// ── a real Claude controller over scripted screens ──────────────────────────

// scriptedClaude is the REAL ClaudeCodeController's gate, with hooks that
// swap the pane's screen before a box check or an acceptance read — as if
// Claude had drawn the next frame. The parse, the verify and the evidence
// rules are all the controller's own.
type scriptedClaude struct {
	*ClaudeCodeController
	mu           sync.Mutex
	beforeReady  func(n int)
	beforeCheck  func(n int)
	beforeAccept func(n int)
	nReady       int
	nCheck       int
	nAccept      int
	win          time.Duration
}

func (s *scriptedClaude) acceptWindowFor() time.Duration { return s.win }

func (s *scriptedClaude) InputReadiness(q ReadinessQuery) InputReadiness {
	s.mu.Lock()
	n := s.nReady
	s.nReady++
	fn := s.beforeReady
	s.mu.Unlock()
	if fn != nil {
		fn(n)
	}
	return s.ClaudeCodeController.InputReadiness(q)
}

func (s *scriptedClaude) CheckBox(w BoxExpect) BoxCheck {
	s.mu.Lock()
	n := s.nCheck
	s.nCheck++
	fn := s.beforeCheck
	s.mu.Unlock()
	if fn != nil {
		fn(n)
	}
	return s.ClaudeCodeController.CheckBox(w)
}

func (s *scriptedClaude) InputAccepted(b AcceptBaseline) AcceptVerdict {
	s.mu.Lock()
	n := s.nAccept
	s.nAccept++
	fn := s.beforeAccept
	s.mu.Unlock()
	if fn != nil {
		fn(n)
	}
	return s.ClaudeCodeController.InputAccepted(b)
}

// claudeMux puts a real, settled Claude controller over p (see gatedClaudeOn)
// inside a scriptedClaude, with a pipe for a PTY.
func claudeMux(t *testing.T, p *Pane) (*Magmux, *scriptedClaude, *byteLog) {
	t.Helper()
	c, _ := gatedClaudeOn(p)
	s := &scriptedClaude{ClaudeCodeController: c, win: 300 * time.Millisecond}
	p.controller = s
	child := attachPipe(t, p)
	m := &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel()}
	m.root, m.focused = p, p
	m.allPanes = []*Pane{p}
	m.stampPaneIDs()
	return m, s, logChild(t, child)
}

// setScreen replaces p's screen with src's.
func setScreen(p, src *Pane) {
	src.mu.Lock()
	sc, ps := src.screen, src.primaryScreen
	src.mu.Unlock()
	p.mu.Lock()
	p.screen, p.primaryScreen = sc, ps
	p.mu.Unlock()
}

// claudeLines draws a synthetic Claude screen: conversation lines, then the
// box (rule, ❯ row, rule) and a footer. An empty box shows the given faint
// placeholder. Rows are padded to rows, so the box sits at the bottom.
func claudeLines(rows, cols int, conv []string, box, placeholder string) *Pane {
	rule := strings.Repeat("─", cols)
	// Claude wraps the box at the width, continuation rows indented by two.
	var boxRows []string
	if box == "" {
		boxRows = []string{"❯ \x1b[2m" + placeholder + "\x1b[22m"}
	}
	for rs := []rune(box); len(rs) > 0; {
		n := min(len(rs), cols-2)
		lead := "  "
		if len(boxRows) == 0 {
			lead = "❯ "
		}
		boxRows = append(boxRows, lead+string(rs[:n]))
		rs = rs[n:]
	}
	tail := append(append([]string{rule}, boxRows...), rule, "  ⏵⏵ auto mode on")
	lines := append([]string(nil), conv...)
	for len(lines)+len(tail) < rows {
		lines = append(lines, "")
	}
	lines = append(lines, tail...)
	p := newScrollPane(rows, cols)
	feed(p, "\x1b[H\x1b[2J"+strings.Join(lines, "\r\n"))
	return p
}

// ── the socket ──────────────────────────────────────────────────────────────

// sockReader reads one connection's lines and hands back replies by id.
type sockReader struct {
	t  *testing.T
	br *bufio.Reader
	c  net.Conn
}

func newSockReader(t *testing.T, c net.Conn) *sockReader {
	return &sockReader{t: t, br: bufio.NewReader(c), c: c}
}

// reply waits for the reply with this id; every other line is skipped.
func (s *sockReader) reply(id string, within time.Duration) (map[string]any, string) {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		_ = s.c.SetReadDeadline(deadline)
		line, err := s.br.ReadString('\n')
		if err != nil {
			s.t.Fatalf("socket read while waiting for reply %s: %v", id, err)
		}
		var msg map[string]any
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg["type"] == "reply" && fmt.Sprint(msg["id"]) == id {
			return msg, line
		}
	}
	s.t.Fatalf("no reply %s within %v", id, within)
	return nil, ""
}
