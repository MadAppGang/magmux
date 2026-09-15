package client

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ── the two-phase wait ──────────────────────────────────────────────────────

// TestRunInstructionTwoPhase is the reason phase one exists. Without it,
// "wait for awaiting_input" returns instantly with the PREVIOUS turn's answer,
// because awaiting_input is exactly the state we send in.
func TestRunInstructionTwoPhase(t *testing.T) {
	// (a) a normal turn: settled -> working -> settled.
	t.Run("normal transition is not stalled", func(t *testing.T) {
		st := NewSessionState()
		st.SeedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input"}})

		send := func() error {
			go func() {
				time.Sleep(10 * time.Millisecond)
				st.ApplyPane(map[string]any{"pane": 0, "state": "working"})
				time.Sleep(10 * time.Millisecond)
				st.ApplyPane(map[string]any{"pane": 0, "state": "awaiting_input",
					"response": "42 passed", "tool": "Bash"})
			}()
			return nil
		}

		r, err := RunInstruction(context.Background(), st, 0, send, 2*time.Second, 2*time.Second)
		if err != nil {
			t.Fatalf("RunInstruction: %v", err)
		}
		if r.Stalled {
			t.Errorf("turn reported stalled: %+v", r)
		}
		if r.State != "awaiting_input" {
			t.Errorf("state = %q, want awaiting_input", r.State)
		}
		if r.Response != "42 passed" {
			t.Errorf("response = %q, want the NEW turn's response", r.Response)
		}
		if r.Tool != "Bash" {
			t.Errorf("tool = %q, want Bash", r.Tool)
		}
	})

	// (b) the instruction was dropped: nothing moved at all. This must be
	// reported as stalled, never as an empty success — "the instruction never
	// arrived" and "there was nothing to do" need different responses.
	t.Run("no transition and no response change is stalled", func(t *testing.T) {
		st := NewSessionState()
		st.SeedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input",
			"response": "previous answer"}})

		r, err := RunInstruction(context.Background(), st, 0,
			func() error { return nil }, 60*time.Millisecond, time.Second)
		if err != nil {
			t.Fatalf("RunInstruction: %v", err)
		}
		if !r.Stalled {
			t.Errorf("a dropped instruction was not reported as stalled: %+v", r)
		}
		if r.State != "stalled" {
			t.Errorf("state = %q, want stalled", r.State)
		}
		if r.Response != "" {
			t.Errorf("a stalled turn must not carry the previous turn's response, got %q",
				r.Response)
		}
	})

	// (c) the escape hatch (magmux.ts:202): the turn ran and finished inside a
	// single 250ms controller poll, so we never sampled a non-settled state,
	// but the response text changed — that is a real turn.
	t.Run("response change without a visible transition is a real turn", func(t *testing.T) {
		st := NewSessionState()
		st.SeedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input",
			"response": "previous answer"}})

		send := func() error {
			go func() {
				time.Sleep(10 * time.Millisecond)
				// Never leaves awaiting_input, but answers.
				st.ApplyPane(map[string]any{"pane": 0, "state": "awaiting_input",
					"response": "new answer"})
			}()
			return nil
		}

		r, err := RunInstruction(context.Background(), st, 0, send, 300*time.Millisecond, time.Second)
		if err != nil {
			t.Fatalf("RunInstruction: %v", err)
		}
		if r.Stalled {
			t.Errorf("escape hatch did not fire; turn reported stalled: %+v", r)
		}
		if r.Response != "new answer" {
			t.Errorf("response = %q, want \"new answer\"", r.Response)
		}
	})
}

// TestPerPaneSnapshotClearsTheResponse pins the one place the MCP client
// deliberately stops being a port of pilot/magmux.ts:145.
//
// magmux clears LastResponse at the START of every turn
// (controller_claude.go), and pollControllers always emits the `response` key
// on a per-pane snapshot — so `"response":""` there means "this turn has said
// nothing yet", not "unchanged". The aggregate (buildPaneResults) omits the key
// entirely when empty, so there an absent key really does mean unchanged.
// Treating both the same is what handed send_and_wait the previous turn's
// answer for any turn that was pure tool calls.
func TestPerPaneSnapshotClearsTheResponseButAggregateDoesNot(t *testing.T) {
	st := NewSessionState()
	st.SeedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input",
		"response": "Added the parser."}})

	// The aggregate omits `response` when it is empty, so an absent key must
	// leave the last answer standing — that is what stops a late attach or a
	// `list` reply wiping everything we know.
	st.SeedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input"}})
	if got := st.response(0); got != "Added the parser." {
		t.Errorf("an aggregate that omits response cleared it: %q", got)
	}

	// A per-pane snapshot carries the key on every poll, so an explicit empty
	// value is magmux telling us the turn has produced no text.
	st.ApplyPane(map[string]any{"pane": 0, "state": "working", "response": "", "tool": "Edit"})
	if got := st.response(0); got != "" {
		t.Errorf("an explicit empty response on a per-pane snapshot was discarded: %q — "+
			"send_and_wait would report the previous turn's answer", got)
	}

	// An omitted key on a per-pane snapshot is still "unchanged": only the keys
	// magmux actually sends may overwrite what we know.
	st.ApplyPane(map[string]any{"pane": 0, "state": "awaiting_input", "response": "done"})
	st.ApplyPane(map[string]any{"pane": 0, "state": "awaiting_input"})
	if got := st.response(0); got != "done" {
		t.Errorf("a per-pane snapshot with no response key cleared it: %q", got)
	}
}

func TestRunInstructionReportsSendFailure(t *testing.T) {
	st := NewSessionState()
	st.SeedAggregate([]any{map[string]any{"pane": 0, "state": "awaiting_input"}})
	want := errors.New("no such pane")
	if _, err := RunInstruction(context.Background(), st, 0,
		func() error { return want }, time.Second, time.Second); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the send error", err)
	}
}

// ── ingest ──────────────────────────────────────────────────────────────────

// TestIngestSeedsFromAggregateSnapshot covers the rule that stops a
// late-attaching agent waiting forever: magmux pushes per-pane snapshots on
// CHANGE only, so a session already sitting at awaiting_input emits nothing
// further and the connect-time aggregate is the only state we will ever see.
func TestIngestSeedsFromAggregateSnapshot(t *testing.T) {
	s := &Session{state: NewSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}

	s.ingest([]byte(`{"type":"snapshot","panes":[
	  {"pane":0,"state":"running","controller":"claude-code","model":"opus"},
	  {"pane":1,"state":"awaiting_input","response":"done"},
	  {"pane":2,"state":"completed","dead":true,"exitCode":0},
	  {"pane":3,"state":"failed","dead":true,"exitCode":2},
	  {"pane":4,"state":"panel","control":true}
	]}`))

	want := map[int]string{0: "working", 1: "awaiting_input", 2: "gone", 3: "gone", 4: "panel"}
	for idx, state := range want {
		if got := s.state.PaneState(idx); got != state {
			t.Errorf("pane %d state = %q, want %q (aggregate vocabulary must be translated)",
				idx, got, state)
		}
	}
	if p, _ := s.state.pane(4); !p.Control {
		t.Error("pane 4 is the control panel and must be marked as such")
	}
	if p, _ := s.state.pane(3); p.ExitCode != 2 || !p.Dead {
		t.Errorf("pane 3 = %+v, want dead with exit code 2", p)
	}
	if got := s.state.response(1); got != "done" {
		t.Errorf("pane 1 response = %q, want done", got)
	}

	// A per-pane snapshot (singular `pane`, no `panes`) is the only event that
	// tracks a turn, and its state names pass through untranslated.
	s.ingest([]byte(`{"type":"snapshot","pane":1,"state":"working","tool":"Bash"}`))
	if got := s.state.PaneState(1); got != "working" {
		t.Errorf("pane 1 state = %q after a live snapshot, want working", got)
	}
	if got := s.state.tool(1); got != "Bash" {
		t.Errorf("pane 1 tool = %q, want Bash", got)
	}
	if got := s.state.response(1); got != "done" {
		t.Errorf("an omitted response means unchanged, got %q", got)
	}

	s.ingest([]byte(`{"type":"exit","pane":0,"exitCode":3}`))
	if got := s.state.PaneState(0); got != "gone" {
		t.Errorf("pane 0 state = %q after exit, want gone", got)
	}

	// A reply must never touch pane state — a controller cannot be allowed to
	// fabricate an observation about a session.
	s.ingest([]byte(`{"type":"reply","id":"1","ok":true,"result":{"pane":1,"state":"nonsense"}}`))
	if got := s.state.PaneState(1); got != "working" {
		t.Errorf("a reply changed pane state to %q", got)
	}
	// Unknown event types are ignored rather than fatal.
	s.ingest([]byte(`{"type":"control","dir":"out","pane":1}`))
	s.ingest([]byte(`not json at all`))
	if got := s.state.PaneState(1); got != "working" {
		t.Errorf("pane 1 state = %q after junk, want working", got)
	}

	// results/shutdown end the session and wake every waiter, so nothing sits
	// on a fifteen-minute timeout after magmux has gone.
	done := make(chan bool, 1)
	go func() {
		done <- s.state.wait(context.Background(),
			func() bool { return s.state.PaneState(1) == "awaiting_input" }, time.Minute)
	}()
	time.Sleep(20 * time.Millisecond)
	s.ingest([]byte(`{"type":"shutdown"}`))
	select {
	case ok := <-done:
		if ok {
			t.Error("wait returned true although the predicate never held")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after shutdown — waiters must be woken when magmux goes")
	}
}

func TestReplyRoutingMatchesNumericAndStringIDs(t *testing.T) {
	s := &Session{state: NewSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}
	ch := make(chan mcpReply, 1)
	s.pending["7"] = ch

	s.ingest([]byte(`{"type":"reply","id":7,"ok":true,"result":{"pane":2}}`))
	select {
	case r := <-ch:
		if !r.OK {
			t.Errorf("reply not ok: %+v", r)
		}
	default:
		t.Fatal("a numeric id did not route to the pending request registered as \"7\"")
	}

	s.pending["8"] = make(chan mcpReply, 1)
	s.ingest([]byte(`{"type":"reply","id":"8","ok":false,"code":"no_such_pane","error":"pane 4 of 3"}`))
	select {
	case r := <-s.pending["8"]:
		if r.OK || r.Code != "no_such_pane" {
			t.Errorf("reply = %+v, want the failure with its code", r)
		}
	default:
		t.Fatal("a string id did not route")
	}
}

// TestALateReplyProvesTheReplyProtocol covers the cheapest recovery there is:
// a reply that arrived just after its waiter gave up is still proof that the
// plumbing exists, and proof outranks any amount of silence.
func TestALateReplyProvesTheReplyProtocol(t *testing.T) {
	sess := &Session{ID: "late", state: NewSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}
	sess.capState.Store(capsSilent)
	sess.probedAt = time.Now() // a fresh verdict: IsLegacy answers without probing
	ctx := context.Background()
	if !sess.IsLegacy(ctx) {
		t.Fatal("setup: the session should start out on the silent verdict")
	}

	// Nobody is waiting on id 99 any more — the request timed out — and it must
	// still count.
	sess.ingest([]byte(`{"type":"reply","id":"99","ok":false,"code":"no_such_pane","error":"nope"}`))
	if sess.IsLegacy(ctx) {
		t.Error("magmux replied and the session is still refused as legacy")
	}
}

func TestSendAndWaitRefusesTwoConcurrentTurnsOnOnePane(t *testing.T) {
	sess := &Session{ID: "x", state: NewSessionState(), pending: map[string]chan mcpReply{},
		closed: make(chan struct{}), inFlight: map[int]bool{}}
	if !sess.BeginTurn(2) {
		t.Fatal("first turn was refused")
	}
	if sess.BeginTurn(2) {
		t.Error("two concurrent turns on one pane were allowed — the second would report " +
			"the first one's answer")
	}
	if !sess.BeginTurn(3) {
		t.Error("a turn on another pane was refused")
	}
	sess.EndTurn(2)
	if !sess.BeginTurn(2) {
		t.Error("the pane was not released")
	}
}
