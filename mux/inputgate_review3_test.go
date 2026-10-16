package mux

// Review round 3 (gpt H1/H2/M3, internal M1/M2/M3, gemini L1, the real-Claude
// smoke's lost early send): history evidence is anchored on USER-PROMPT
// entries only, a subcommand is only the first argument, the turn is announced
// at the first byte, transcript freshness is the entry's own submit time, an
// emptied box is retyped once, and a bash-mode send's keys drop the `!`.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// ── 1 (gpt H1, internal M3): the entry frontier ────────────────────────────

// claudeBanner is the welcome banner Claude draws at the top of a session
// (s1-box-history): the top of the document.
var claudeBanner = []string{
	" ▐▛███▛█   Claude Code v2.1.301",
	"▝▜██████▀  Haiku 5.5 · Claude Max",
	" ▝▝   ▝▝   ~/scratch",
	"",
}

// shortFrame lays out a conversation that does not fill the 39-row pane the
// way s1 shows one: a blank row 0, the conversation from row 1 (the banner
// first), blank rows down to the box, the box at the bottom.
func shortFrame(conv, box []string) []styledRow {
	const height = 39
	tail := []string{frontierRule()}
	tail = append(tail, box...)
	tail = append(tail, frontierRule(), "  * Haiku | $0.00", "  ⏵⏵ auto mode on (shift+tab to cycle)")
	rows := []styledRow{{}}
	for _, s := range conv {
		rows = append(rows, styledRow{Text: s})
	}
	for len(rows)+len(tail) < height {
		rows = append(rows, styledRow{})
	}
	for _, s := range tail {
		rows = append(rows, styledRow{Text: s})
	}
	return rows
}

// TestRepeatedAssistantBlocksAreNoAnchor is gpt's counterexample: identical
// three-row assistant blocks S → `❯ retry` → S. The last S is mutated (a live
// row redraws) and NO new prompt appears. The old `❯ retry` must not count.
func TestRepeatedAssistantBlocksAreNoAnchor(t *testing.T) {
	S := []string{"⏺ Same reply, line one of three.", "  same reply, line two of three.", "  same reply, line three of three."}
	conv := append(append(append([]string{}, S...), "", "❯ retry", ""), S...)
	before := parseClaudeScreen(claudeFrame(conv, []string{"❯ retry"}), frontierCols)
	b := newAcceptBaseline(before, before.BoxText, "retry", 0, time.Now(), 0)

	mutated := append(append(append([]string{}, S...), "", "❯ retry", ""),
		"⏺ Same reply, line one of three, now redrawn.", "  a different second row.", "  and a third.")
	after := parseClaudeScreen(claudeFrame(mutated, []string{"❯ "}), frontierCols)
	if got := acceptVerdict(after, b, nil); got.State != AcceptNone {
		t.Errorf("a mutated assistant block re-anchored on an older copy and counted the old `❯ retry`: %+v", got)
	}
}

// TestQueuedSendConfirmedWhileLiveOutputChanges: a correct mid-turn queued
// send while the running tool's output row changes between the verify and the
// acceptance read. The live row is no anchor, so it cannot hide the evidence.
func TestQueuedSendConfirmedWhileLiveOutputChanges(t *testing.T) {
	head := []string{"❯ Run the Bash command: sleep 25 && echo DONE-ONE", "", "⏺ Bash(sleep 25 && echo DONE-ONE)"}
	verify := append(append([]string{}, head...), "  ⎿  Running… (3s)", "", "✻ Billowing… (6s · ↓ 102 tokens)")
	before := parseClaudeScreen(claudeFrame(verify, []string{"❯ QUEUED-SECOND say hi"}), frontierCols)
	b := newAcceptBaseline(before, before.BoxText, "QUEUED-SECOND say hi", 0, time.Now(), 0)

	taken := append(append([]string{}, head...), "  ⎿  Running in the background (9s)",
		"", "❯ QUEUED-SECOND say hi", "  ctrl+x ctrl+s to send now", "", "✻ Billowing… (9s · ↓ 140 tokens)")
	after := parseClaudeScreen(claudeFrame(taken, []string{"❯ "}), frontierCols)
	if got := acceptVerdict(after, b, nil); got.State != AcceptTaken || got.Evidence != evidenceHistory {
		t.Errorf("a queued send under a changed live row: %+v, want taken/history", got)
	}
}

// TestIdenticalEntriesWithoutTheTopAreAmbiguous: the visible entries are a run
// of identical prompts and the top of the conversation is not on screen. A
// later screen with one more of them could be a NEW entry, or an older one a
// shrinking box brought back: the baseline's end cannot be located, so the
// history gives no evidence.
func TestIdenticalEntriesWithoutTheTopAreAmbiguous(t *testing.T) {
	var older []string
	for range 30 {
		older = append(older, "⏺ filler reply row")
	}
	block := []string{"❯ continue", "", "⏺ ok", ""}
	conv := append(append(append([]string{}, older...), block...), block...)
	before := parseClaudeScreen(claudeFrame(conv, []string{"❯ continue"}), frontierCols)
	if len(before.Prompts) != 2 {
		t.Fatalf("setup: %d entries visible, want 2", len(before.Prompts))
	}
	b := newAcceptBaseline(before, before.BoxText, "continue", 0, time.Now(), 0)
	more := append(append([]string{}, conv...), "❯ continue", "")
	after := parseClaudeScreen(claudeFrame(more, []string{"❯ "}), frontierCols)
	if got := acceptVerdict(after, b, nil); got.State != AcceptNone {
		t.Errorf("an ambiguous alignment gave history evidence: %+v", got)
	}
}

// TestRepeatedPromptUnderTheBannerIsConfirmed: with the top of the
// conversation (the banner) on the verify screen, nothing older can appear
// above the visible entries, so a repeated prompt's new entry is placeable.
func TestRepeatedPromptUnderTheBannerIsConfirmed(t *testing.T) {
	conv := append(append([]string{}, claudeBanner...), "❯ continue", "", "⏺ ok", "")
	before := parseClaudeScreen(shortFrame(conv, []string{"❯ continue"}), frontierCols)
	b := newAcceptBaseline(before, before.BoxText, "continue", 0, time.Now(), 0)
	if got := acceptVerdict(parseClaudeScreen(shortFrame(conv, []string{"❯ "}), frontierCols), b, nil); got.State != AcceptNone {
		t.Errorf("lost: %+v, want none", got)
	}
	taken := append(append([]string{}, conv...), "❯ continue", "", "· Germinating… (0s)")
	if got := acceptVerdict(parseClaudeScreen(shortFrame(taken, []string{"❯ "}), frontierCols), b, nil); got.State != AcceptTaken {
		t.Errorf("taken: %+v, want taken/history", got)
	}
}

// ── 2 (gpt H2): a subcommand is only the FIRST argument ────────────────────

func TestSubcommandOnlyAsFirstArgument(t *testing.T) {
	for _, r := range []struct {
		line   string
		direct bool
	}{
		{"claude --name doctor", true},
		{"claude --name=doctor", true},
		{"claude --agent doctor", true},
		{"claude --append-system-prompt update", true},
		{"claude --settings x mcp list", true}, // after an option: direct (documented)
		{"claude --model opus mcp list", true},
		{"claude mcp list", false},
		{"claude doctor", false},
		{"claude update", false},
		{"claude --version", false},
		{"claude --name x --help", false},
		{"claude -p hi", false},
		{"claude fix the doctor script", true},
	} {
		if got := claudeLaunchOf(shellCfg("/bin/zsh", r.line)); got.direct != r.direct {
			t.Errorf("claudeLaunchOf(%q).direct = %v, want %v", r.line, got.direct, r.direct)
		}
	}
	for _, r := range []struct {
		args   []string
		direct bool
	}{
		{[]string{"--name", "doctor"}, true},
		{[]string{"doctor"}, false},
		{[]string{"mcp", "list"}, false},
	} {
		if got := claudeLaunchOf(PaneConfig{Cmd: "claude", Args: r.args}); got.direct != r.direct {
			t.Errorf("argv %q: direct = %v, want %v", r.args, got.direct, r.direct)
		}
	}
}

// ── 3 (internal M1, gpt M3): notify at the first byte, once ────────────────

// TestResumedFirstKeyNotifiesOnce: a send sent back to the gate by the first
// key's re-ask announces the turn exactly once, when the first byte goes in.
func TestResumedFirstKeyNotifiesOnce(t *testing.T) {
	var flipped atomic.Bool
	g := &fakeGate{readyFn: func(_ ReadinessQuery, n int) InputReadiness {
		if n == 2 && !flipped.Swap(true) {
			return notReady(protocol.ReasonTerminalNotReady)
		}
		return readyNow()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{keys: []string{"h", "i", "enter"}})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if n := g.notified.Load(); n != 1 {
		t.Errorf("NotifyInput called %d times, want exactly 1", n)
	}
	evs, seen := g.eventLog()
	for i, ev := range evs {
		if ev == "notify" {
			if seen[i] != "" || i == 0 || evs[i-1] != "ready" {
				t.Errorf("notify at event %d (bytes %q, previous %q): want right after the last readiness check, before any byte", i, seen[i], evs[i-1])
			}
		}
	}
	if got := log.settle(); got != "hi\r" {
		t.Errorf("bytes %q", got)
	}
}

// ── 4 (internal M2, research F11): transcript freshness by submit time ─────

func transcriptLine(e map[string]any) []byte {
	b, _ := json.Marshal(e)
	return b
}

func stamp(ts time.Time) string { return ts.UTC().Format("2006-01-02T15:04:05.000Z") }

// enqueueLine, removeLine and queuedAttachLine are the F11 shapes, verbatim
// in their keys.
func enqueueLine(content string, ts time.Time) []byte {
	return transcriptLine(map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": stamp(ts),
		"sessionId": "s", "content": content})
}

func removeLine(content, reason string, ts time.Time) []byte {
	return transcriptLine(map[string]any{"type": "queue-operation", "operation": "remove", "timestamp": stamp(ts),
		"sessionId": "s", "content": content, "reason": reason})
}

func queuedAttachLine(prompt string, ts time.Time) []byte {
	return transcriptLine(map[string]any{"type": "attachment", "timestamp": stamp(ts),
		"attachment": map[string]any{"type": "queued_command", "prompt": prompt, "commandMode": "prompt",
			"timestamp": stamp(ts), "humanTurn": true}})
}

// lostSendOn is a verified CheckBox for sentFrance on s1, after which the
// screen shows the send LOST (an empty box, no entry): only the transcript can
// confirm anything.
func lostSendOn(t *testing.T) (*ClaudeCodeController, AcceptBaseline) {
	t.Helper()
	p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
	g, _ := gatedClaudeOn(p)
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
	if chk.Verdict != BoxVerified {
		t.Fatalf("check: %+v", chk)
	}
	swapScreen(t, p, "s1-box-history", "empty-box-placeholder")
	return g, chk.Baseline
}

// TestQueuedSendConfirmedByEnqueue: a queued send's transcript evidence is
// its `queue-operation` enqueue, written at queue time (F11).
func TestQueuedSendConfirmedByEnqueue(t *testing.T) {
	g, b := lostSendOn(t)
	g.applyLine(enqueueLine(sentFrance, time.Now()))
	if got := g.InputAccepted(b); got.State != AcceptTaken || got.Evidence != evidenceTranscript {
		t.Errorf("a fresh enqueue of this text: %+v, want taken/transcript", got)
	}
}

// TestEarlierIdenticalQueuedSendIsNotEvidence: two identical sends; the first
// was queued BEFORE the second's verify, the second was lost. The first's
// late records — its remove, its attachment (which carries the ORIGINAL
// enqueue timestamp, F11), and a user entry written when it is dequeued — are
// all the first send's.
func TestEarlierIdenticalQueuedSendIsNotEvidence(t *testing.T) {
	t.Run("enqueued before, attachment after", func(t *testing.T) {
		first := time.Now().Add(-3 * time.Second)
		p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
		g, _ := gatedClaudeOn(p)
		g.applyLine(enqueueLine(sentFrance, first)) // the first send, before the verify
		chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
		swapScreen(t, p, "s1-box-history", "empty-box-placeholder")
		g.applyLine(removeLine(sentFrance, "absorbed_mid_turn", time.Now()))
		g.applyLine(queuedAttachLine(sentFrance, first))
		if got := g.InputAccepted(chk.Baseline); got.State != AcceptNone {
			t.Errorf("the earlier send's late attachment counted: %+v", got)
		}
	})
	t.Run("enqueue read late", func(t *testing.T) {
		g, b := lostSendOn(t)
		g.applyLine(enqueueLine(sentFrance, time.Now().Add(-3*time.Second)))
		if got := g.InputAccepted(b); got.State != AcceptNone {
			t.Errorf("an old enqueue consumed late counted: %+v", got)
		}
	})
	t.Run("dequeued as a user entry", func(t *testing.T) {
		first := time.Now().Add(-3 * time.Second)
		p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
		g, _ := gatedClaudeOn(p)
		g.applyLine(enqueueLine(sentFrance, first))
		chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
		swapScreen(t, p, "s1-box-history", "empty-box-placeholder")
		now := time.Now()
		g.applyLine(removeLine(sentFrance, "dequeued", now))
		g.applyLine(userLine(sentFrance, now)) // stamped at dequeue: still the first send
		if got := g.InputAccepted(chk.Baseline); got.State != AcceptNone {
			t.Errorf("the earlier send's dequeued user entry counted: %+v", got)
		}
	})
}

// TestAbsorbedQueuedPromptDoesNotMisdateTheNextSend: a queued prompt removed
// as absorbed_mid_turn (F11) never becomes a user entry, so it must not date
// the NEXT identical send's own, fresh entry as old.
func TestAbsorbedQueuedPromptDoesNotMisdateTheNextSend(t *testing.T) {
	first := time.Now().Add(-5 * time.Second)
	p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
	g, _ := gatedClaudeOn(p)
	g.applyLine(enqueueLine(sentFrance, first))
	g.applyLine(removeLine(sentFrance, "absorbed_mid_turn", first.Add(time.Second)))
	g.applyLine(queuedAttachLine(sentFrance, first))
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
	swapScreen(t, p, "s1-box-history", "empty-box-placeholder")
	g.applyLine(userLine(sentFrance, time.Now())) // this send, submitted at idle
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptTaken || got.Evidence != evidenceTranscript {
		t.Errorf("this send's own fresh entry: %+v, want taken/transcript", got)
	}
}

// ── 5 (the real-Claude smoke): an emptied box is retyped once ──────────────

func boxEmptyCheck() BoxCheck { return BoxCheck{Readiness: readyNow(), Verdict: BoxEmpty} }

func TestEmptiedBoxIsRetypedOnce(t *testing.T) {
	g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck {
		if n == 0 {
			return boxEmptyCheck() // the late startup reset swallowed the text
		}
		return verifiedBox(verifiedExact)
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hellohello\r" {
		t.Errorf("bytes %q, want the text twice and one Enter", got)
	}
	res := map[string]any{}
	o.rep.addTo(res)
	if res["retyped"] != true {
		t.Errorf("reply %v: want retyped:true", res)
	}
	if n := g.notified.Load(); n != 1 {
		t.Errorf("NotifyInput called %d times, want 1", n)
	}
}

func TestEmptiedBoxTwiceIsTextMismatch(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return boxEmptyCheck() }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if got := log.settle(); got != "hellohello" {
		t.Errorf("bytes %q, want the text twice and no Enter", got)
	}
}

func TestPartialBoxIsNeverRetyped(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		return BoxCheck{Readiness: readyNow(), Verdict: BoxMismatch, Detail: "partial"}
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text once", got)
	}
	res := map[string]any{}
	o.rep.addTo(res)
	if _, ok := res["retyped"]; ok {
		t.Errorf("reply %v carries retyped", res)
	}
}

// ── 6 (LOWs): a `cd` cannot change HOME; waitedMs counts a re-entered gate ─

func TestTildeSettingsAfterPlainCd(t *testing.T) {
	home, _ := rewriteFixtures(t) // s.json here
	t.Setenv("HOME", home)
	private := settingsDir(home)
	for _, r := range []struct {
		line string
		want editorOverride
	}{
		{"cd /tmp && claude --settings ~/s.json", overrideMergedFile},
		{"cd /tmp; cd / && claude --settings=~/s.json", overrideMergedFile},
		{"HOME=/x; claude --settings ~/s.json", overrideUnparsed},
		{"export HOME=/x && claude --settings ~/s.json", overrideUnparsed},
		{"env HOME=/x sh; claude --settings ~/s.json", overrideUnparsed},
		{"eval x; claude --settings ~/s.json", overrideUnparsed},
		{". ./profile; claude --settings ~/s.json", overrideUnparsed},
		{"source ./profile; claude --settings ~/s.json", overrideUnparsed},
		{"f() { :; }; claude --settings ~/s.json", overrideUnparsed},
		{"cd $DIR && claude --settings ~/s.json", overrideUnparsed},
	} {
		cfg := shellCfg("/bin/sh", r.line)
		rw := rewriteClaudeLaunch(cfg, claudeLaunchOf(cfg), gateDirect, private)
		if rw.file != "" {
			defer os.Remove(rw.file)
		}
		if rw.override != r.want {
			t.Errorf("%q: override %q (why %q), want %q", r.line, rw.override, rw.why, r.want)
			continue
		}
		if r.want == overrideMergedFile {
			if b, err := os.ReadFile(rw.file); err != nil || !strings.Contains(string(b), "SECRET") {
				t.Errorf("%q: copy %q holds %q, want the inherited HOME's file", r.line, rw.file, b)
			}
			if filepath.Dir(rw.file) == "" {
				t.Errorf("no copy")
			}
		}
	}
}

func TestWaitedCountsTheReenteredGate(t *testing.T) {
	const notReadyFor = 300 * time.Millisecond
	var until atomic.Int64
	g := &fakeGate{readyFn: func(_ ReadinessQuery, n int) InputReadiness {
		switch {
		case n <= 1:
			if n == 1 {
				until.Store(time.Now().Add(notReadyFor).UnixNano())
			}
			return readyNow()
		case time.Now().UnixNano() < until.Load():
			return notReady(protocol.ReasonTerminalNotReady)
		}
		return readyNow()
	}}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{keys: []string{"h", "i", "enter"}})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if o.rep.waited < notReadyFor-keyGap-50*time.Millisecond {
		t.Errorf("waited %v, want at least the ~%v spent back in the gate", o.rep.waited, notReadyFor-keyGap)
	}
}

// ── 7 (gemini L1): a bash-mode send's keys drop the mode-switch `!` ────────

// TestBashModeCollapsedPasteIsConfirmed: a multi-line bash-mode command
// collapses to a paste token in the box; its history entry is `! <command>`,
// the command WITHOUT the `!` that switched the mode.
func TestBashModeCollapsedPasteIsConfirmed(t *testing.T) {
	const cmd = "echo a\necho b\necho c\necho d"
	before := parseClaudeScreen(shortFrame(claudeBanner, []string{"! [Pasted text #1 +3 lines]"}), frontierCols)
	if !before.BoxShell || len(before.BoxPastes) != 1 {
		t.Fatalf("setup: %+v", before)
	}
	verdict, verified, paste, _ := judgeBox(before, BoxExpect{Exact: "!" + cmd, Pasted: true, TypedText: true})
	if verdict != BoxVerified || verified != verifiedPasteToken {
		t.Fatalf("judgeBox = %v %q", verdict, verified)
	}
	b := newAcceptBaseline(before, before.BoxText, sentOf(before, "!"+cmd), paste, time.Now(), 0)
	conv := append(append([]string{}, claudeBanner...), "! echo a", "  echo b", "  echo c", "  echo d", "  ⎿  a", "")
	after := parseClaudeScreen(shortFrame(conv, []string{"❯ "}), frontierCols)
	if got := acceptVerdict(after, b, nil); got.State != AcceptTaken || got.Evidence != evidenceHistory {
		t.Errorf("a collapsed bash-mode paste: %+v, want taken/history", got)
	}
}
