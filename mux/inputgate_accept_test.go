package mux

// Phase 5: acceptance [A](2) — no ok without a NEW user-prompt entry or an
// equal transcript entry — and the deadline rules [E]: a caller's timeout
// never reaches a delivery, and every default wait is at least sendBudget.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/httpapi"
)

// ── [A](2) negatives, against the REAL controller ───────────────────────────

const (
	synthRows = 30
	synthCols = 100
)

// entryRows renders a user prompt as Claude prints it in history: `❯ ` and
// continuation rows indented by two.
func entryRows(text string) []string {
	var out []string
	for rs := []rune(text); len(rs) > 0; {
		n := min(len(rs), synthCols-2)
		lead := "  "
		if len(out) == 0 {
			lead = "❯ "
		}
		out = append(out, lead+string(rs[:n]))
		rs = rs[n:]
	}
	return out
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var runningTurn = []string{
	"⏺ Bash(sleep 25 && echo DONE-ONE)",
	"  ⎿  Running…",
	"",
	"✢ Boondoggling… (6s · ↓ 451 tokens)",
}

const (
	idlePlaceholder   = `Try "refactor mux.go"`
	queuedPlaceholder = "Press up to edit queued messages"
)

// TestLostMidTurnSendWithMatchingProseIsUnconfirmed: mid-turn, the box held
// the prompt at the verify; after the Enter the box shows the queued-message
// placeholder and a NEW assistant row begins with the prompt's own words — but
// no new `❯` entry appeared. That is a lost send, never an ok.
func TestLostMidTurnSendWithMatchingProseIsUnconfirmed(t *testing.T) {
	const prompt = "Summarise the build log in one line"
	conv := cat(entryRows("Run the Bash command: sleep 25 && echo DONE-ONE"), []string{""}, runningTurn)
	start := claudeLines(synthRows, synthCols, conv, "", idlePlaceholder)
	inBox := claudeLines(synthRows, synthCols, conv, prompt, "")
	after := claudeLines(synthRows, synthCols,
		cat(conv, []string{"", "⏺ Summarise the build log in one line: every step passed."}), "", queuedPlaceholder)
	m, s, log := claudeMux(t, start)
	s.beforeCheck = func(int) { setScreen(start, inBox) }
	s.beforeAccept = func(int) { setScreen(start, after) }
	o := runSend(t, m, sendRequest{text: prompt, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonUnconfirmed, true)
	if got := log.settle(); got != prompt+"\r" {
		t.Errorf("bytes %q: one text and one Enter (an empty box is not InBox, so no resend)", got)
	}
}

// TestSharedPrefixSecondSendNeedsItsOwnEntry: two sends share a 40+-rune
// prefix; the first is confirmed by its entry, the second is lost after its
// Enter and must not be confirmed by the first's.
func TestSharedPrefixSecondSendNeedsItsOwnEntry(t *testing.T) {
	const (
		p1 = "Refactor the parser module so every helper is documented and tested; begin with lexer.go"
		p2 = "Refactor the parser module so every helper is documented and tested; then move on to grid.go"
	)
	if len(p1) < 48 || p1[:60] != p2[:60] {
		t.Fatalf("the prompts must share a long prefix")
	}
	idle := cat(claudeBanner, []string{"⏺ Ready."}) // a short conversation: the banner is on screen
	s0 := claudeLines(synthRows, synthCols, idle, "", idlePlaceholder)
	box1 := claudeLines(synthRows, synthCols, idle, p1, "")
	conv1 := cat(idle, []string{""}, entryRows(p1))
	after1 := claudeLines(synthRows, synthCols, conv1, "", idlePlaceholder)
	box2 := claudeLines(synthRows, synthCols, conv1, p2, "")
	m, s, _ := claudeMux(t, s0)
	var second atomic.Bool
	s.beforeCheck = func(int) {
		if second.Load() {
			setScreen(s0, box2)
		} else {
			setScreen(s0, box1)
		}
	}
	s.beforeAccept = func(int) { setScreen(s0, after1) } // p2 never gets an entry
	o := runSend(t, m, sendRequest{text: p1, enter: true})
	if o.err != nil || o.rep.accept != acceptConfirmed || o.rep.evidence != evidenceHistory {
		t.Fatalf("first send: %+v %v, want confirmed by history", o.rep, o.err)
	}
	second.Store(true)
	o = runSend(t, m, sendRequest{text: p2, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonUnconfirmed, true)
}

// TestLostIdenticalResendDuringToolCallIsUnconfirmed (r3 H3): the prior entry
// for P is followed DIRECTLY by a running tool row whose ⏺ blinks. P is sent
// again mid-turn; the verify reads a blink-OFF frame and acceptance reads
// alternate off/on frames. The re-send is lost, so no new entry exists in
// either phase — the reply must be unconfirmed, never history.
func TestLostIdenticalResendDuringToolCallIsUnconfirmed(t *testing.T) {
	const prompt = "Run the Bash command: sleep 25 && echo DONE-ONE"
	off := cat(entryRows(prompt), []string{"  Bash(sleep 25 && echo DONE-ONE)", "  ⎿  Running…", "", "✢ Boondoggling…"})
	on := cat(entryRows(prompt), []string{"⏺ Bash(sleep 25 && echo DONE-ONE)", "  ⎿  Running…", "", "✢ Boondoggling…"})
	start := claudeLines(synthRows, synthCols, off, "", queuedPlaceholder)
	inBoxOff := claudeLines(synthRows, synthCols, off, prompt, "")
	lostOff := claudeLines(synthRows, synthCols, off, "", queuedPlaceholder)
	lostOn := claudeLines(synthRows, synthCols, on, "", queuedPlaceholder)
	for _, sc := range []*Pane{lostOff, lostOn} {
		v := sc.tailView(0)
		if got := parseClaudeScreen(v.Rows, v.Cols).Prompts; len(got) != 1 || got[0].Key != inputKey(prompt) {
			t.Fatalf("a blink frame does not parse to the one entry: %+v", got)
		}
	}
	m, s, _ := claudeMux(t, start)
	s.beforeCheck = func(int) { setScreen(start, inBoxOff) }
	s.beforeAccept = func(n int) {
		if n%2 == 0 {
			setScreen(start, lostOn)
		} else {
			setScreen(start, lostOff)
		}
	}
	o := runSend(t, m, sendRequest{text: prompt, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonUnconfirmed, true)
}

// ── the acceptance windows, scripted ────────────────────────────────────────

// acceptGate is a fakeGate with short windows and a scripted acceptance.
func acceptGate(fn func(b AcceptBaseline, n int) AcceptVerdict) *fakeGate {
	return &fakeGate{win: 200 * time.Millisecond, acceptFn: fn}
}

func TestAcceptNeverOkWithoutEvidence(t *testing.T) {
	for _, c := range []struct {
		name   string
		v      AcceptVerdict
		reason string
		enters int
	}{
		{"nothing", AcceptVerdict{State: AcceptNone}, protocol.ReasonUnconfirmed, 1},
		{"a dialog appeared", AcceptVerdict{State: AcceptNone, Dialog: "permission"}, protocol.ReasonUnconfirmed, 1},
		{"text stays in the box", AcceptVerdict{State: AcceptInBox}, protocol.ReasonStillInBox, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := acceptGate(func(AcceptBaseline, int) AcceptVerdict { return c.v })
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, sendRequest{text: "hi", enter: true})
			wantErr(t, o, protocol.CodeNotAccepted, c.reason, true)
			if n := strings.Count(log.settle(), "\r"); n != c.enters {
				t.Errorf("%d Enters, want %d", n, c.enters)
			}
		})
	}
}

// enterCount is a script helper: how many \r the child has seen.
func enterCount(l *byteLog) int { return strings.Count(l.String(), "\r") }

func TestAcceptResendsEnterOnce(t *testing.T) {
	var log *byteLog
	g := acceptGate(func(AcceptBaseline, int) AcceptVerdict {
		if enterCount(log) >= 2 {
			return AcceptVerdict{State: AcceptTaken, Evidence: evidenceTranscript}
		}
		return AcceptVerdict{State: AcceptInBox}
	})
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{text: "hi", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if o.rep.accept != acceptResent || o.rep.evidence != evidenceTranscript {
		t.Errorf("report %+v, want resent_enter / transcript", o.rep)
	}
	if n := strings.Count(log.settle(), "\r"); n != 2 {
		t.Errorf("%d Enters, want exactly 2", n)
	}
	if ack := lastAck(m); !strings.Contains(ack, "accepted after re-sent Enter") {
		t.Errorf("ack %q", ack)
	}
}

// TestResendIsGuarded: the re-sent Enter is a write, so it needs a ready,
// verified box; a dialog at that moment means no second \r.
func TestResendIsGuarded(t *testing.T) {
	g := acceptGate(func(AcceptBaseline, int) AcceptVerdict { return AcceptVerdict{State: AcceptInBox} })
	g.checkFn = func(_ BoxExpect, n int) BoxCheck {
		if n == 0 {
			return verifiedBox(verifiedExact)
		}
		return checkNotReady(dialogUp("permission", true))
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hi", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonStillInBox, true)
	if n := strings.Count(log.settle(), "\r"); n != 1 {
		t.Errorf("%d Enters; a dialog at the resend must withhold the second", n)
	}
	if !strings.Contains(o.err.Error(), "pressed once") {
		t.Errorf("message %q", o.err)
	}
}

func TestStillInBoxAfterResend(t *testing.T) {
	g := acceptGate(func(AcceptBaseline, int) AcceptVerdict { return AcceptVerdict{State: AcceptInBox} })
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hi", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonStillInBox, true)
	if n := strings.Count(log.settle(), "\r"); n != 2 {
		t.Errorf("%d Enters, want 2", n)
	}
	if !strings.Contains(o.err.Error(), "pressed twice") || !strings.Contains(o.err.Error(), "do not retype") {
		t.Errorf("message %q", o.err)
	}
	if ack := lastAck(m); !strings.Contains(ack, "not accepted: still in box") {
		t.Errorf("ack %q", ack)
	}
}

// TestLateInBoxGetsThirdWindow: nothing in window 1, the text still in the
// box at the end of window 2 — one guarded Enter, then window 3.
func TestLateInBoxGetsThirdWindow(t *testing.T) {
	var log *byteLog
	var first time.Time
	g := acceptGate(func(_ AcceptBaseline, n int) AcceptVerdict {
		if n == 0 {
			first = time.Now()
		}
		switch {
		case enterCount(log) >= 2:
			return AcceptVerdict{State: AcceptTaken, Evidence: evidenceHistory}
		case time.Since(first) < 250*time.Millisecond:
			return AcceptVerdict{State: AcceptNone}
		}
		return AcceptVerdict{State: AcceptInBox}
	})
	m, _, l := gatedMux(t, g)
	log = l
	o := runSend(t, m, sendRequest{text: "hi", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if o.rep.accept != acceptResent {
		t.Errorf("accept %q, want resent_enter", o.rep.accept)
	}
	if n := strings.Count(log.settle(), "\r"); n != 2 {
		t.Errorf("%d Enters, want 2", n)
	}
}

func TestAcceptLivenessPaneDead(t *testing.T) {
	var p *Pane
	g := acceptGate(func(_ AcceptBaseline, n int) AcceptVerdict {
		if n == 2 {
			p.mu.Lock()
			p.dead = true // /exit
			p.mu.Unlock()
		}
		return AcceptVerdict{State: AcceptNone}
	})
	m, pane, _ := gatedMux(t, g)
	p = pane
	o := runSend(t, m, sendRequest{text: "/exit", enter: true})
	wantErr(t, o, protocol.CodePaneDead, "", true)
}

func TestQuiesceDuringAcceptIsUnconfirmed(t *testing.T) {
	g := &fakeGate{acceptFn: func(AcceptBaseline, int) AcceptVerdict { return AcceptVerdict{State: AcceptNone} }}
	m, _, log := gatedMux(t, g)
	exec, release := m.bus().ExecContext(context.Background())
	defer release()
	ch := startSend(t, m, exec, sendRequest{text: "hi", enter: true})
	log.waitFor(t, "hi\r", 5*time.Second)
	go m.bus().Quiesce(500 * time.Millisecond)
	o := awaitSend(t, ch, 5*time.Second)
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonUnconfirmed, true)
	if !strings.Contains(o.err.Error(), "shutting down") {
		t.Errorf("message %q", o.err)
	}
}

// ── [E] deadlines are values; budgets bound every default wait ──────────────

// slowGate holds the gate for d, then passes everything.
func slowGate(d time.Duration) *fakeGate {
	start := time.Now()
	return &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness {
		if time.Since(start) < d {
			return notReady(protocol.ReasonStarting)
		}
		return readyNow()
	}}
}

// restServer serves m's hub over HTTP in-process (no listener flags, no
// process), so a fake gate can sit behind a real REST request.
func restServer(t *testing.T, m *Magmux) (base, token string) {
	t.Helper()
	tok, _ := auth.Generate()
	srv, err := httpapi.New(httpapi.Config{
		Addr: "127.0.0.1:0", Hub: m.bus(), Tokens: auth.NewStore(tok, "", nil),
		Tickets: auth.NewTickets(0), ErrorLog: log.New(io.Discard, "", 0),
		Aggregate: func() []byte { return []byte(`{"type":"snapshot","panes":[]}` + "\n") },
		Ready:     func(time.Duration) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(srv.Close)
	return "http://" + srv.Addr().String(), tok
}

func postSend(base, token string, args string, timeout time.Duration) error {
	req, _ := http.NewRequest("POST", base+"/v1/ops/send", strings.NewReader(args))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestCallerTimeoutNeverCancelsDelivery(t *testing.T) {
	t.Run("socket call", func(t *testing.T) {
		m, child, dial := sockMux(t)
		gateSockPane(m, m.allPanes[0], slowGate(800*time.Millisecond))
		log := logChild(t, child)
		conn := dial()
		rd := newSockReader(t, conn)
		writeLines(t, conn,
			map[string]any{"type": "call", "op": "send", "id": "a", "timeoutMs": 100, "args": map[string]any{"pane": 0, "text": "hello"}},
			map[string]any{"type": "call", "op": "send", "id": "b", "args": map[string]any{"pane": 0, "text": "next"}})
		ra, _ := rd.reply("a", 5*time.Second)
		if ra["code"] != protocol.CodeTimeout {
			t.Fatalf("reply a = %v, want the CALLER's timeout", ra)
		}
		rb, _ := rd.reply("b", 10*time.Second)
		if rb["ok"] != true {
			t.Fatalf("reply b = %v", rb)
		}
		if got := log.settle(); got != "hello\rnext\r" {
			t.Errorf("bytes %q: the timed-out delivery must finish, then the next on its lane", got)
		}
	})
	t.Run("REST", func(t *testing.T) {
		g := slowGate(800 * time.Millisecond)
		m, _, log := gatedMux(t, g)
		base, tok := restServer(t, m)
		if err := postSend(base, tok, `{"pane":0,"text":"hello"}`, 100*time.Millisecond); err == nil {
			t.Fatalf("the client was expected to give up")
		}
		log.waitFor(t, "hello\r", 5*time.Second)
	})
}

// TestRESTSendTimeoutDoesNotTruncate: a REST client that disconnects while a
// typed send is half-typed leaves the delivery to finish — the whole text and
// its Enter. Before ExecContext, the caller's ctx was the delivery's.
func TestRESTSendTimeoutDoesNotTruncate(t *testing.T) {
	text := strings.Repeat("abcdefgh", 60) // 15 typed writes
	g := &fakeGate{}
	m, _, log := gatedMux(t, g)
	base, tok := restServer(t, m)
	args, _ := json.Marshal(map[string]any{"pane": 0, "text": text, "typed": true})
	_ = postSend(base, tok, string(args), 40*time.Millisecond)
	got := log.waitFor(t, "\r", 5*time.Second)
	if got != text+"\r" {
		t.Errorf("the child got %d bytes, want the whole %d-byte text and the Enter", len(got), len(text)+1)
	}
}

// TestSendBudgetBoundsLongKeysList: a real deliver of 500 printable keys and
// a submit whose acceptance takes the late-InBox path with the REAL windows.
func TestSendBudgetBoundsLongKeysList(t *testing.T) {
	t.Parallel()
	keys := make([]string, 500)
	for i := range keys {
		keys[i] = string(rune('a' + i%26))
	}
	var log *byteLog
	var first time.Time
	g := &fakeGate{acceptFn: func(_ AcceptBaseline, n int) AcceptVerdict {
		if n == 0 {
			first = time.Now()
		}
		switch {
		case enterCount(log) >= 2:
			return AcceptVerdict{State: AcceptTaken, Evidence: evidenceHistory}
		case time.Since(first) < acceptWindow+100*time.Millisecond:
			return AcceptVerdict{State: AcceptNone}
		}
		return AcceptVerdict{State: AcceptInBox}
	}}
	m, _, l := gatedMux(t, g)
	log = l
	r := sendRequest{keys: keys, enter: true}
	o := runSend(t, m, r)
	if o.err != nil || o.rep.accept != acceptResent {
		t.Fatalf("send: %+v %v, want the late-InBox path", o.rep, o.err)
	}
	if b := sendBudget(r); o.elapsed >= b {
		t.Errorf("took %v, past sendBudget %v", o.elapsed, b)
	}
}

func TestSendBudgetBoundsLongTypedText(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for b.Len() < 4000 {
		b.WriteString("a line of typed instruction text\n")
	}
	g := &fakeGate{}
	m, _, _ := gatedMux(t, g)
	r := sendRequest{mode: sendTyped, text: b.String(), enter: true}
	o := runSend(t, m, r)
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if bud := sendBudget(r); o.elapsed >= bud {
		t.Errorf("took %v, past sendBudget %v", o.elapsed, bud)
	}
	if got, want := len(typedWrites(r.text)), protocol.TypedWriteCount(r.text); got != want {
		t.Errorf("typedWrites makes %d writes, protocol.TypedWriteCount says %d", got, want)
	}
}

// TestDefaultWaitUsesBudget: `call` with no timeoutMs (and REST and WS, which
// read the same Hub.DefaultWait) waits at least sendBudget for a gated send;
// an ungated or noGate send keeps the transport's own default; an explicit
// timeoutMs is the caller's.
func TestDefaultWaitUsesBudget(t *testing.T) {
	g := &fakeGate{}
	m, p, _ := gatedMux(t, g)
	long := json.RawMessage(`{"pane":0,"text":"x","readyTimeoutMs":60000}`)
	want := sendBudget(sendRequest{idx: 0, text: "x", enter: true, readyTimeoutMs: 60000})
	if want <= callTimeoutDefault {
		t.Fatalf("the case must need more than the default: budget %v", want)
	}
	if got := m.bus().DefaultWait("send", long, callTimeoutDefault); got != want {
		t.Errorf("DefaultWait = %v, want sendBudget %v", got, want)
	}
	if got := m.callWait("send", long, 0); got != want {
		t.Errorf("call's default wait = %v, want %v", got, want)
	}
	if got := m.callWait("send", long, 100); got != 100*time.Millisecond {
		t.Errorf("an explicit timeoutMs became %v", got)
	}
	if got := m.callWait("send", json.RawMessage(`{"pane":0,"text":"x","readyTimeoutMs":60000,"noGate":true}`), 0); got != callTimeoutDefault {
		t.Errorf("noGate send waits %v, want the default", got)
	}
	p.gateBasis = gateNone
	if got := m.callWait("send", long, 0); got != callTimeoutDefault {
		t.Errorf("ungated send waits %v, want the default", got)
	}
}
