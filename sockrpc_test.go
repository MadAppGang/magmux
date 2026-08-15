package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ── harness ─────────────────────────────────────────────────────────────────

// rpcMagmux is a real magmux subprocess under a PTY: the only setup in which
// the socket, the panes and the emulator all behave the way they do in the
// field. Every assertion below is made against magmux's own output rather than
// against an in-process fake, because the property under test — who receives a
// line and who does not — exists only on real connections.
type rpcMagmux struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	sock   string
}

func startRPCMagmux(t *testing.T, args ...string) *rpcMagmux {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY/socket test requires darwin or linux")
	}
	bin := magmuxBinForTest(t)

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	// A freshly opened PTY reports 0x0, and magmux sizes its panes from the
	// terminal — without this every pane's screen is zero rows and every screen
	// assertion below passes vacuously.
	setWinSize(master, 24, 100)

	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	// Drain the master so magmux's writes never block.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		master.Close()
		slave.Close()
	})

	return &rpcMagmux{
		t:      t,
		cmd:    cmd,
		master: master,
		sock:   fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid),
	}
}

// quit sends Ctrl-G q, magmux's own quit chord, so the run ends through the
// normal teardown that broadcasts `results` — a signal would skip it.
func (r *rpcMagmux) quit() {
	r.t.Helper()
	if _, err := r.master.Write([]byte{0x07, 'q'}); err != nil {
		r.t.Fatalf("write quit chord to the PTY: %v", err)
	}
}

// rpcConn is one subscriber connection and the scanner that owns its stream.
// The scanner is created with the connection and never shared: two scanners on
// one fd would each swallow lines the other was waiting for.
type rpcConn struct {
	t  *testing.T
	c  net.Conn
	sc *bufio.Scanner
}

func (r *rpcMagmux) dial() *rpcConn {
	r.t.Helper()
	var (
		conn net.Conn
		err  error
	)
	for i := 0; i < 60; i++ {
		conn, err = net.Dial("unix", r.sock)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		r.t.Fatalf("could not connect to magmux socket %s: %v", r.sock, err)
	}
	r.t.Cleanup(func() { conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &rpcConn{t: r.t, c: conn, sc: sc}
}

func (rc *rpcConn) send(v map[string]any) {
	rc.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		rc.t.Fatalf("marshal %v: %v", v, err)
	}
	if _, err := rc.c.Write(append(b, '\n')); err != nil {
		rc.t.Fatalf("socket write: %v", err)
	}
}

// next returns the next event and the raw line it was decoded from, or ok=false
// at EOF. The raw line is what proves an id round-tripped verbatim rather than
// merely comparing equal after decoding.
func (rc *rpcConn) next() (ev map[string]any, raw string, ok bool) {
	for rc.sc.Scan() {
		raw = rc.sc.Text()
		ev = nil
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		return ev, raw, true
	}
	if err := rc.sc.Err(); err != nil {
		rc.t.Fatalf("socket read error before EOF: %v", err)
	}
	return nil, "", false
}

// awaitReply reads until the reply to id arrives, handing every other event to
// see (if non-nil) on the way past.
func (rc *rpcConn) awaitReply(id string, see func(map[string]any)) (map[string]any, string) {
	rc.t.Helper()
	for {
		ev, raw, ok := rc.next()
		if !ok {
			rc.t.Fatalf("stream ended before a reply to id %q arrived", id)
		}
		if ev["type"] == "reply" && fmt.Sprint(ev["id"]) == id {
			return ev, raw
		}
		if see != nil {
			see(ev)
		}
	}
}

// drain reads the whole stream to EOF. magmux closes each subscriber only after
// its final results/shutdown broadcasts, so reaching EOF here is itself part of
// what the ordering assertions check.
func (rc *rpcConn) drain() []map[string]any {
	var out []map[string]any
	for {
		ev, _, ok := rc.next()
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

func replyOK(t *testing.T, ev map[string]any) map[string]any {
	t.Helper()
	if ok, _ := ev["ok"].(bool); !ok {
		t.Fatalf("reply not ok: code=%v error=%v", ev["code"], ev["error"])
	}
	res, _ := ev["result"].(map[string]any)
	return res
}

// ── tests ───────────────────────────────────────────────────────────────────

// TestSocketReplyIsUnicast is the load-bearing compatibility test: it is the
// whole argument that adding request/response to the socket cannot disturb a
// client that predates it.
//
// A reply goes to the connection that asked, and to no other. If it were
// broadcast, every existing subscriber (madbench, pilot/magmux.ts) would start
// receiving a line it has no case for — a change to the stream they read, made
// by somebody else's request, with no way for them to opt out. Asserting that
// B's stream is clean *to EOF* is what makes this a proof rather than a hope:
// it covers the whole run, not just the moment the reply was sent.
func TestSocketReplyIsUnicast(t *testing.T) {
	mux := startRPCMagmux(t, "-w", "-e", `sh -c "sleep 1.5"`)
	a := mux.dial()
	b := mux.dial()

	// B is read to EOF on its own goroutine: it is the connection that must
	// receive nothing, so the only way to be sure is to read all of it.
	bDone := make(chan []map[string]any, 1)
	go func() { bDone <- b.drain() }()

	a.send(map[string]any{"type": "capabilities", "id": "1"})
	ev, _ := a.awaitReply("1", nil)
	res := replyOK(t, ev)

	if got := res["protocol"]; got != float64(sockProtocol) {
		t.Errorf("capabilities protocol = %v, want %d", got, sockProtocol)
	}
	if _, ok := res["scrollback"]; !ok {
		t.Error("capabilities omitted scrollback; a client cannot otherwise learn that capture is visible-screen-only")
	}
	verbs := fmt.Sprint(res["verbs"])
	for _, want := range []string{"capabilities", "list", "capture", "send"} {
		if !strings.Contains(verbs, want) {
			t.Errorf("capabilities verbs %v missing %q", verbs, want)
		}
	}

	// A numeric id must come back as a number, not as a string: the client
	// matching replies to requests compares it against what it sent.
	a.send(map[string]any{"type": "capabilities", "id": 7})
	_, raw := a.awaitReply("7", nil)
	if !strings.Contains(raw, `"id":7`) {
		t.Errorf("numeric id did not round-trip verbatim; reply line was %s", raw)
	}

	bEvents := <-bDone
	var sawResults bool
	for _, ev := range bEvents {
		if ev["type"] == "reply" {
			t.Errorf("connection B received a reply it never asked for: %v", ev)
		}
		if ev["type"] == "results" {
			sawResults = true
		}
	}
	if !sawResults {
		t.Fatal("connection B never received results, so it was not a live subscriber and the assertion above proved nothing")
	}
}

// TestSocketNoIDNoReply is the madbench / pilot regression guard. Those clients
// send verbs with no id, so the contract they were written against is: nothing
// ever comes back, unknown verbs are ignored rather than refused, and the
// stream still ends results → shutdown → EOF.
//
// The unknown verb matters twice over. It is how an older magmux is detected
// (silence to `capabilities` means "predates replies"), which only works if
// silence remains the answer for a client that did not ask for one.
func TestSocketNoIDNoReply(t *testing.T) {
	mux := startRPCMagmux(t, "-w", "-e", `sh -c "sleep 1.2"`)
	c := mux.dial()

	c.send(map[string]any{"type": "tint", "pane": 0, "color": "red"})
	c.send(map[string]any{"type": "send", "pane": 0, "text": "no-reply-please"})
	c.send(map[string]any{"type": "definitely-not-a-verb", "pane": 0})
	// A verb that fails: still silent, because silence is what was asked for.
	c.send(map[string]any{"type": "capture", "pane": 99})

	events := c.drain()

	resultsIdx, shutdownIdx := -1, -1
	for i, ev := range events {
		if ev["type"] == "reply" {
			t.Errorf("event %d is a reply to a message that carried no id: %v", i, ev)
		}
		switch ev["type"] {
		case "results":
			resultsIdx = i
		case "shutdown":
			shutdownIdx = i
		}
	}
	if resultsIdx < 0 {
		t.Fatalf("no results event before EOF; events=%v", events)
	}
	if shutdownIdx >= 0 && shutdownIdx < resultsIdx {
		t.Fatalf("shutdown (idx %d) arrived before results (idx %d)", shutdownIdx, resultsIdx)
	}
}

// TestCaptureReturnsRenderedScreen checks capture against the only ground truth
// there is: what magmux's own emulator painted for the pane. The pane prints a
// marker and then sleeps, so the marker is on screen and nowhere else — no
// transcript, no log, no claim by magmux about its own behaviour.
func TestCaptureReturnsRenderedScreen(t *testing.T) {
	mux := startRPCMagmux(t, "-e", `printf 'MARKER-7\n'; sleep 5`)
	c := mux.dial()

	// The child has to be scheduled, write, and have its output parsed before
	// the marker is on screen, so poll rather than sleeping a guessed interval.
	var res map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		id := fmt.Sprintf("cap-%d", attempt)
		c.send(map[string]any{"type": "capture", "id": id, "pane": 0, "lines": 40})
		ev, _ := c.awaitReply(id, nil)
		res = replyOK(t, ev)
		if text, _ := res["text"].(string); strings.Contains(text, "MARKER-7") {
			break
		}
		res = nil
		time.Sleep(200 * time.Millisecond)
	}
	if res == nil {
		t.Fatal("capture never returned the pane's own output within 5s")
	}

	if rows, _ := res["rows"].(float64); rows <= 0 {
		t.Errorf("capture reported %v rows; the pane screen was never sized", res["rows"])
	}
	if cols, _ := res["cols"].(float64); cols <= 0 {
		t.Errorf("capture reported %v cols", res["cols"])
	}
	if alt, _ := res["alt"].(bool); alt {
		t.Error("capture reported the alt screen for a pane that never entered it")
	}
	if _, ok := res["cursor"].(map[string]any); !ok {
		t.Errorf("capture omitted the cursor position: %v", res["cursor"])
	}
}

// TestSendReplyReportsDeadPane covers the failure `send` could not report until
// now. Delivery happens on its own goroutine, so a pane whose child had already
// exited swallowed the instruction there: no error, no log, and a driver left
// waiting for a turn that could never start.
func TestSendReplyReportsDeadPane(t *testing.T) {
	// No -w: the pane dies immediately and magmux stays up around its corpse,
	// which is exactly the state a driver can walk into.
	mux := startRPCMagmux(t, "-e", `exit 7`)
	c := mux.dial()

	// The pane may already be dead in the connect-time aggregate, or die in an
	// exit event just after; both are the same starting condition.
	deadline := time.Now().Add(10 * time.Second)
	dead := false
	for !dead && time.Now().Before(deadline) {
		ev, _, ok := c.next()
		if !ok {
			t.Fatal("stream ended before pane 0 died")
		}
		switch ev["type"] {
		case "exit":
			dead = true
		case "snapshot":
			panes, _ := ev["panes"].([]any)
			for _, p := range panes {
				pm, _ := p.(map[string]any)
				if d, _ := pm["dead"].(bool); d {
					dead = true
				}
			}
		}
	}
	if !dead {
		t.Fatal("pane 0 never reported dead; the fixture did not set up")
	}

	c.send(map[string]any{"type": "send", "id": "s1", "pane": 0, "text": "anyone home?"})
	ev, _ := c.awaitReply("s1", nil)
	if ok, _ := ev["ok"].(bool); ok {
		t.Fatalf("send to a dead pane reported success: %v", ev)
	}
	if ev["code"] != sockCodePaneDead {
		t.Errorf("send to a dead pane replied code %v, want %q (error: %v)", ev["code"], sockCodePaneDead, ev["error"])
	}
}

// TestListMatchesResults locks in the one-code-path invariant: `list` and the
// shutdown `results` are both buildPaneResults, so they cannot disagree about a
// pane. Two builders would drift, and the first symptom would be a driver that
// believes a pane is running while the run's own report says it finished —
// the same class of contradiction CLAUDE.md already forbids between snapshot
// and results.
//
// The session pane sleeps rather than exiting so its state is genuinely stable
// across the two reads; the control pane is present because its entry is the
// one with a hand-written shape.
func TestListMatchesResults(t *testing.T) {
	mux := startRPCMagmux(t, "-c", "-e", `sh -c "sleep 30"`)
	c := mux.dial()
	time.Sleep(400 * time.Millisecond)

	c.send(map[string]any{"type": "list", "id": "l1"})
	ev, _ := c.awaitReply("l1", nil)
	listed := paneStates(t, replyOK(t, ev)["panes"])

	mux.quit()

	var final map[string]string
	for _, ev := range c.drain() {
		if ev["type"] == "results" {
			final = paneStates(t, ev["panes"])
		}
	}
	if final == nil {
		t.Fatal("no results event before EOF")
	}

	if len(listed) != len(final) {
		t.Fatalf("list reported %d panes, results %d: %v vs %v", len(listed), len(final), listed, final)
	}
	for pane, state := range listed {
		got, ok := final[pane]
		if !ok {
			t.Errorf("pane %s appears in list but not in results", pane)
			continue
		}
		if got != state {
			t.Errorf("pane %s: list said state %q, results said %q — list and results must be the same code path",
				pane, state, got)
		}
	}
	if len(listed) < 2 {
		t.Fatalf("expected a session pane and the control panel, got %v", listed)
	}
}

// ── transcript ──────────────────────────────────────────────────────────────

// watcherOnly is a ToolController that does NOT implement TranscriptReader —
// the case the optional-interface design exists for. Every controller must be
// able to decline history without being forced to fake it.
type watcherOnly struct{}

func (watcherOnly) Name() string                  { return "watcher" }
func (watcherOnly) Start(_ context.Context) error { return nil }
func (watcherOnly) Poll() (Snapshot, error)       { return Snapshot{State: CtrlWorking}, nil }
func (watcherOnly) Stop() error                   { return nil }

// TestSockTranscriptFailsHonestly walks the three ways this verb has nothing to
// return. Each is a different code because each sends the caller somewhere
// different, and none of them may be an empty success: `{"turns":[]}` reads to
// an agent as "the session has said nothing", which is a claim about the
// SESSION when the truth in all three cases is a claim about magmux.
func TestSockTranscriptFailsHonestly(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	p := m.paneByID(0)

	// 1. No controller at all — a shell, a REPL, npm run dev.
	_, err := m.sockTranscript(sockMsg{Type: "transcript", Pane: 0.0})
	if got := verbErrCode(err); got != sockCodeNoController {
		t.Fatalf("a pane with no controller replied code %q, want %q (err: %v)",
			got, sockCodeNoController, err)
	}
	if err != nil && !strings.Contains(err.Error(), "capture") {
		t.Errorf("the refusal does not point at the screen instead: %v", err)
	}

	// 2. A controller that does not implement TranscriptReader.
	p.controller = watcherOnly{}
	_, err = m.sockTranscript(sockMsg{Type: "transcript", Pane: 0.0})
	if got := verbErrCode(err); got != sockCodeUnsupported {
		t.Fatalf("a controller without TranscriptReader replied code %q, want %q (err: %v)",
			got, sockCodeUnsupported, err)
	}

	// 3. The Claude controller, attached and healthy, that has not located the
	//    session file. This is the one that happens in the field: discovery
	//    lags at session start and can fail outright.
	p.controller = &ClaudeCodeController{}
	_, err = m.sockTranscript(sockMsg{Type: "transcript", Pane: 0.0})
	if got := verbErrCode(err); got != sockCodeNoTranscript {
		t.Fatalf("an undiscovered transcript replied code %q, want %q (err: %v)",
			got, sockCodeNoTranscript, err)
	}
	if err != nil && !strings.Contains(err.Error(), "NOT mean") {
		t.Errorf("the refusal does not distinguish itself from an empty session: %v", err)
	}

	// A missing pane still beats all of them to the answer.
	_, err = m.sockTranscript(sockMsg{Type: "transcript", Pane: 41.0})
	if got := verbErrCode(err); got != sockCodeNoSuchPane {
		t.Errorf("pane 41 replied code %q, want %q", got, sockCodeNoSuchPane)
	}
}

// TestSockTranscriptServesTurns covers the success path end to end through the
// verb: the turn count arrives in `lines`, and the reply carries the full text
// plus each tool's input and result.
func TestSockTranscriptServesTurns(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONL(t, dir, "s.jsonl",
		`{"type":"user","message":{"content":"ship it"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"shipped"},`+
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git push"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"main -> main"}]}}`,
	)
	m := newTestMux(t, ctrlPanes(1)...)
	m.paneByID(0).controller = controllerOn(path)

	res, err := m.sockTranscript(sockMsg{Type: "transcript", Pane: 0.0, Lines: 4})
	if err != nil {
		t.Fatalf("sockTranscript: %v", err)
	}
	if res["controller"] != "claude-code" {
		t.Errorf("controller = %v, want claude-code", res["controller"])
	}
	if res["requested"] != 4 {
		t.Errorf("requested = %v, want the 4 turns asked for in `lines`", res["requested"])
	}
	turns, _ := res["turns"].([]any)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %v", len(turns), turns)
	}
	assistant, _ := turns[1].(map[string]any)
	if assistant["role"] != TurnAssistant || assistant["text"] != "shipped" {
		t.Fatalf("assistant turn = %v", assistant)
	}
	tools, _ := assistant["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("assistant turn carries %d tools, want 1", len(tools))
	}
	call, _ := tools[0].(map[string]any)
	if call["name"] != "Bash" {
		t.Errorf("tool name = %v, want Bash", call["name"])
	}
	if in, _ := call["input"].(string); !strings.Contains(in, "git push") {
		t.Errorf("tool input = %q, want the command", in)
	}
	if call["result"] != "main -> main" {
		t.Errorf("tool result = %v, want what the tool returned", call["result"])
	}

	// An omitted count is the default, not zero turns.
	res, err = m.sockTranscript(sockMsg{Type: "transcript", Pane: 0.0})
	if err != nil {
		t.Fatalf("sockTranscript with no count: %v", err)
	}
	if res["requested"] != defaultTranscriptTurns {
		t.Errorf("requested = %v, want the default %d", res["requested"], defaultTranscriptTurns)
	}
}

// TestTranscriptVerbAnswersOnlyWhenAsked runs the verb over a real socket. It
// is the routing proof: `transcript` reaches the dispatcher at all, an id draws
// exactly one reply, and a message without one stays silent — the contract every
// client written before replies existed depends on, which a new verb must not
// quietly break.
func TestTranscriptVerbAnswersOnlyWhenAsked(t *testing.T) {
	mux := startRPCMagmux(t, "-e", `sh -c "sleep 5"`)
	c := mux.dial()

	c.send(map[string]any{"type": "capabilities", "id": "caps"})
	ev, _ := c.awaitReply("caps", nil)
	if verbs := fmt.Sprint(replyOK(t, ev)["verbs"]); !strings.Contains(verbs, "transcript") {
		t.Errorf("capabilities does not advertise transcript, so a client that feature-detects "+
			"cannot find it: %v", verbs)
	}

	// No id: silent, like every other verb. Proven by asking a second question
	// afterwards — the first reply to arrive must be that second question's.
	c.send(map[string]any{"type": "transcript", "pane": 0})
	c.send(map[string]any{"type": "capabilities", "id": "after"})
	for {
		ev, _, ok := c.next()
		if !ok {
			t.Fatal("stream ended before the reply to `after`")
		}
		if ev["type"] != "reply" {
			continue
		}
		if fmt.Sprint(ev["id"]) != "after" {
			t.Fatalf("a transcript request carrying no id drew a reply: %v", ev)
		}
		break
	}

	// With an id: exactly one reply, and an honest refusal — `sh -c sleep` has
	// no controller, so there is no record of turns to read.
	c.send(map[string]any{"type": "transcript", "id": "t1", "pane": 0})
	ev, _ = c.awaitReply("t1", nil)
	if ok, _ := ev["ok"].(bool); ok {
		t.Fatalf("a pane with no agent in it returned a transcript: %v", ev)
	}
	if ev["code"] != sockCodeNoController {
		t.Errorf("code = %v, want %q (error: %v)", ev["code"], sockCodeNoController, ev["error"])
	}
}

// paneStates reduces a panes array to pane index → state, so a mismatch names
// the pane that disagreed rather than dumping two blobs. The control panel
// keeps its own shape ({"state":"panel","control":true}) and is checked for it
// here, because test/ui/case3 asserts on that exact shape.
func paneStates(t *testing.T, v any) map[string]string {
	t.Helper()
	panes, ok := v.([]any)
	if !ok {
		t.Fatalf("panes is %T, want an array", v)
	}
	out := map[string]string{}
	for _, p := range panes {
		pm, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("pane entry is %T, want an object", p)
		}
		idx := fmt.Sprint(pm["pane"])
		state, _ := pm["state"].(string)
		if ctrl, _ := pm["control"].(bool); ctrl && state != "panel" {
			t.Errorf("control pane %s reported state %q, want %q", idx, state, "panel")
		}
		out[idx] = state
	}
	return out
}

// ── focus ───────────────────────────────────────────────────────────────────

// TestFocusRefusesAHiddenPane is the "parked keyboard" guard.
//
// The panel is hidden by default and buildPaneResults publishes its id to every
// client as state:"panel", hidden:true — so a socket client can read the id off
// `list` and focus it. A hidden pane is not in the tree and is never painted,
// and the panel has no PTY, so every later keystroke would be swallowed with
// nothing on screen to explain where it went. That is the exact state
// hidePanelLocked repairs and focusNext / resolveSplitTargetLocked skip; `focus`
// is the one door left open into it.
//
// The refusal is against HIDDEN, not against the panel: a panel that is on
// screen is a legitimate focus target (inputLoop routes keys to
// consumeControlKey, which scrolls it), and Ctrl-G o cycles onto it already.
func TestFocusRefusesAHiddenPane(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	panel := m.paneByID(1)

	// Focus works while the pane is on screen — this is the control case, and it
	// is what makes the failure below about hidden rather than about isControl.
	if _, err := m.sockFocus(sockMsg{Type: "focus", Pane: 1.0}); err != nil {
		t.Fatalf("focus refused a visible pane: %v", err)
	}

	m.treeMu.Lock()
	m.hidePanelLocked(panel)
	m.treeMu.Unlock()

	m.treeMu.RLock()
	before := m.focused
	m.treeMu.RUnlock()
	if before == panel {
		t.Fatal("hiding the pane left focus on it, so this test cannot tell a refusal from a no-op")
	}

	_, err := m.sockFocus(sockMsg{Type: "focus", Pane: 1.0})
	if err == nil {
		t.Fatal("focus accepted a hidden pane: every keystroke now goes to a pane that is not in the tree, is never painted and has no PTY")
	}
	if got := verbErrCode(err); got != sockCodePaneHidden {
		t.Errorf("focus on a hidden pane replied code %q, want %q (err: %v)",
			got, sockCodePaneHidden, err)
	}

	m.treeMu.RLock()
	after := m.focused
	m.treeMu.RUnlock()
	if after == panel {
		t.Fatal("the refusal still moved focus onto the hidden pane")
	}
	if after != before {
		t.Errorf("a refused focus moved the keyboard anyway: %v -> %v", before, after)
	}
}

// ── routes ──────────────────────────────────────────────────────────────────

// TestBadPaneIDsOpenNoRoute — a request magmux refuses must not leave a route
// behind.
//
// recordRequest opens a route for any pane >= 0, so recording before
// paneForMsg validates manufactures a permanent route to a pane that does not
// exist. Two consequences, and the second is the one that bites: 32 of them
// exhaust ctrlMaxRoutes so real panes stop getting table rows, and ONE of them
// is enough to make targetPane see two routes where there is one — after which
// a pilot that had always sent pane-less instructions is refused with "send
// needs a pane" for the rest of the run, because somebody typo'd a pane id once.
func TestBadPaneIDsOpenNoRoute(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)

	// One real route, the way a controller opens one: by touching a pane.
	if _, err := m.sockCapture(sockMsg{Type: "capture", Pane: 0.0}); err != nil {
		t.Fatalf("capture of a live pane failed: %v", err)
	}

	// Now the typos. Every one of these is refused, so none of them is a pane
	// this controller has touched.
	if _, err := m.sockCapture(sockMsg{Type: "capture", Pane: 99.0}); err == nil {
		t.Fatal("capture of pane 99 succeeded")
	}
	if _, err := m.sockTranscript(sockMsg{Type: "transcript", Pane: 98.0}); err == nil {
		t.Fatal("transcript of pane 98 succeeded")
	}
	if _, err := m.sockClosePane(sockMsg{Type: "close_pane", Pane: 97.0}); err == nil {
		t.Fatal("close_pane of pane 97 succeeded")
	}

	m.control.mu.Lock()
	order := append([]int(nil), m.control.routeOrder...)
	m.control.mu.Unlock()
	if len(order) != 1 || order[0] != 0 {
		t.Errorf("routes after three refused requests = %v, want [0]: a refused request "+
			"names no pane the controller has touched", order)
	}

	// The user-visible consequence, asserted directly: a pilot driving one
	// session must still be able to send without naming it.
	pane, err := m.control.targetPane()
	if err != nil {
		t.Fatalf("a pane-less send was refused after a typo'd pane id: %v", err)
	}
	if pane != 0 {
		t.Errorf("targetPane() = %d, want 0", pane)
	}

	// The mistake is still visible in the panel — it is a controller request
	// that arrived on the socket, so it belongs in the stream. What it must not
	// have is a route.
	m.control.mu.Lock()
	defer m.control.mu.Unlock()
	refused := 0
	for _, sig := range m.control.signals {
		if sig.ok != nil && !*sig.ok && sig.code == sockCodeNoSuchPane {
			refused++
		}
	}
	if refused != 3 {
		t.Errorf("%d refusals reached the stream, want 3: a request the controller got "+
			"wrong is exactly what the panel exists to show", refused)
	}
}
