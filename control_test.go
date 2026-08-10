package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// samplePanel builds a panel mid-run: a pilot part-way through a five-step
// task, with one turn still in flight.
func samplePanel() *ControlPanel {
	cp := newControlPanel()
	// Relative to now, not a fixed date: the panel renders live durations, so
	// a frozen base makes every elapsed figure read as "24h 20m" and the
	// screenshot stops representing what the panel actually looks like.
	base := time.Now().Add(-160 * time.Second)
	cp.startedAt = base
	cp.target = 0
	cp.model = "anthropic/claude-sonnet-5"
	cp.goal = "get the test suite green, then clear every go vet warning"
	cp.steps = 5
	cp.sent = 4
	cp.observed = 3
	cp.state = "working"
	cp.note = "pilot attached"
	cp.steplog = []ctrlStep{
		{n: 1, label: "step 1/5", at: base.Add(2 * time.Second), dur: 32 * time.Second,
			tool: "Bash", state: "awaiting_input",
			text:  "run the full test suite and report which tests fail",
			reply: "3 of 42 tests fail, all in TestSocketSubscriberContract"},
		{n: 2, label: "step 2/5", at: base.Add(35 * time.Second), dur: 74 * time.Second,
			tool: "Edit", state: "awaiting_input",
			text:  "fix TestSocketSubscriberContract without weakening the assertion",
			reply: "fixed: the subscriber was registered after the shutdown broadcast, so results raced the close"},
		{n: 3, label: "verify", at: base.Add(110 * time.Second), dur: 21 * time.Second,
			tool: "Bash", state: "awaiting_input",
			text: "re-run the suite", reply: "42 passed, 0 failed"},
		{n: 4, label: "step 3/5", at: base.Add(132 * time.Second),
			tool: "Read", text: "now run go vet ./... and list every warning"},
	}
	return cp
}

// sampleFrameState mirrors what render() snapshots. Keep it in step with that
// struct: a field missing here silently renders as its zero value and a test
// asserting on it fails for a reason that has nothing to do with the panel.
func sampleFrameState(cp *ControlPanel) ctrlFrameState {
	return ctrlFrameState{
		goal: cp.goal, target: cp.target, model: cp.model, steps: cp.steps,
		sent: cp.sent, observed: cp.observed, startedAt: cp.startedAt,
		state: cp.state, note: cp.note, noteBad: cp.noteBad,
		scroll: cp.scroll, finished: cp.finished, summary: cp.summary,
		closeIn: cp.closeIn,
	}
}

// TestControlPanelFrameShape locks the panel's structural invariants: it fits
// its pane exactly, never exceeds the width, and leads with the link header.
func TestControlPanelFrameShape(t *testing.T) {
	cp := samplePanel()
	for _, size := range [][2]int{{46, 24}, {80, 20}, {120, 30}} {
		w, h := size[0], size[1]
		lines := cp.frame(sampleFrameState(cp), cp.steplog, w, h)
		if len(lines) > h {
			t.Errorf("%dx%d: frame produced %d lines for %d rows", w, h, len(lines), h)
		}
		for i, ln := range lines {
			if got := visWidth(ln); got > w {
				t.Errorf("%dx%d: line %d is %d columns wide: %q", w, h, i, got, ln)
			}
		}
		if !strings.Contains(lines[0], "CONTROL PLANE") {
			t.Errorf("%dx%d: header is not first: %q", w, h, lines[0])
		}
	}
}

// TestControlPanelLedgerIsTabular is the point of the redesign. The pilot's
// own pane is already a chronological log of the same run; if this panel is
// also a log, the two are indistinguishable and the control plane earns
// nothing. Every step must occupy exactly one row.
func TestControlPanelLedgerIsTabular(t *testing.T) {
	cp := samplePanel()
	lines := cp.frame(sampleFrameState(cp), cp.steplog, 80, 24)

	rows := 0
	for _, ln := range lines {
		for _, st := range cp.steplog {
			if strings.Contains(ln, st.label) && strings.Contains(ln, "─") == false {
				rows++
				break
			}
		}
	}
	if rows < len(cp.steplog) {
		t.Errorf("only %d of %d steps got a ledger row", rows, len(cp.steplog))
	}

	// No instruction body may be wrapped across lines: a step's text appears
	// at most once, and only in the single-line detail row.
	full := "run the full test suite and report which tests fail"
	count := 0
	for _, ln := range lines {
		if strings.Contains(ln, full) {
			count++
		}
	}
	if count > 1 {
		t.Errorf("instruction text appears on %d lines; the ledger must not wrap prose", count)
	}
}

// TestControlPanelFillsPaneWithTheExchange checks that the detail block grows
// into the space below the ledger instead of staying a fixed stub.
//
// It used to reserve exactly three rows and truncate both the instruction and
// the reply to one line each, so a tall pane sat mostly empty while the thing
// you actually want to read was cut off after 40 characters.
func TestControlPanelFillsPaneWithTheExchange(t *testing.T) {
	cp := samplePanel()
	// A long reply, the normal case: Claude Code answers in paragraphs.
	cp.steplog[len(cp.steplog)-1].state = "awaiting_input"
	cp.steplog[len(cp.steplog)-1].reply = strings.Repeat(
		"The vet run surfaced four findings across three files, each of them a "+
			"shadowed variable in an error branch. ", 12)

	const w, h = 80, 40
	lines := cp.frame(sampleFrameState(cp), cp.steplog, w, h)

	// The pane should be substantially filled, not a third full.
	if len(lines) < h*3/4 {
		t.Errorf("frame used %d of %d rows; the exchange should fill the pane", len(lines), h)
	}

	// The reply must span several rows rather than being cut to one.
	replyRows := 0
	for _, ln := range lines {
		if strings.Contains(ln, "shadowed variable") {
			replyRows++
		}
	}
	if replyRows < 3 {
		t.Errorf("reply occupies %d rows in a %d-row pane; it should wrap", replyRows, h)
	}

	// And it must still respect the pane: no overflow.
	if len(lines) > h {
		t.Errorf("frame produced %d lines for %d rows", len(lines), h)
	}
	for i, ln := range lines {
		if visWidth(ln) > w {
			t.Errorf("line %d is %d columns wide, pane is %d", i, visWidth(ln), w)
		}
	}
}

// TestControlPanelShowsEveryExchange checks that the panel holds the whole
// conversation, not just the newest pair — every instruction sent and every
// reply observed must be reachable by scrolling.
func TestControlPanelShowsEveryExchange(t *testing.T) {
	cp := samplePanel()
	// Distinct single-word markers: the exchange wraps, so a multi-word
	// needle can straddle a line break and fail for the wrong reason.
	for i := range cp.steplog {
		cp.steplog[i].state = "awaiting_input"
		cp.steplog[i].text = "instrmarker" + string(rune('A'+i)) + " do the thing"
		cp.steplog[i].reply = "replymarker" + string(rune('A'+i)) + " it is done"
	}

	convo := cp.exchangeLines(cp.steplog, " ", 70)
	joined := strings.Join(convo, "\n")
	for i, st := range cp.steplog {
		if !strings.Contains(joined, "replymarker"+string(rune('A'+i))) {
			t.Errorf("step %d's reply is missing from the exchange", st.n)
		}
		if !strings.Contains(joined, "instrmarker"+string(rune('A'+i))) {
			t.Errorf("step %d's instruction is missing from the exchange", st.n)
		}
	}
}

// TestControlPanelScrollClampsToContent guards the two ways a scroll offset
// goes wrong: running past the top (leaving a blank view that never recovers)
// and refusing to return to the live tail.
func TestControlPanelScrollClampsToContent(t *testing.T) {
	cp := samplePanel()
	for i := range cp.steplog {
		cp.steplog[i].state = "awaiting_input"
		cp.steplog[i].reply = strings.Repeat("a long observed reply. ", 20)
	}

	const w, h = 70, 24
	cp.scrollBy(100000) // slam against the top
	lines := cp.frame(sampleFrameState(cp), cp.steplog, w, h)
	if len(lines) > h {
		t.Fatalf("scrolled frame produced %d lines for %d rows", len(lines), h)
	}
	cp.mu.Lock()
	clamped := cp.scroll
	cp.mu.Unlock()
	if clamped >= 100000 {
		t.Errorf("scroll was not clamped to the content: %d", clamped)
	}

	cp.scrollToBottom()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.scroll != 0 {
		t.Errorf("scrollToBottom left offset at %d, want 0", cp.scroll)
	}
}

// TestControlPanelFinishFooter checks that a finished run always states how
// it ends — either the key that closes it, or a visible countdown. A run that
// silently vanishes, or that looks identical to a stalled one, is the failure
// this guards.
func TestControlPanelFinishFooter(t *testing.T) {
	cp := samplePanel()
	cp.finished = true
	cp.state = "finished"
	cp.summary = "built the tool and verified its output"

	s := sampleFrameState(cp)
	lines := cp.frame(s, cp.steplog, 80, 24)
	last := strings.Join(lines, "\n")
	if !strings.Contains(last, "FINISHED") {
		t.Error("finished run does not show a FINISHED badge")
	}
	if !strings.Contains(last, "to close") {
		t.Error("finished run does not say how to close it")
	}

	s.closeIn = 12 * time.Second
	lines = cp.frame(s, cp.steplog, 80, 24)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "closing in 12s") {
		t.Error("armed countdown is not shown")
	}
	if !strings.Contains(joined, "any key cancels") {
		t.Error("countdown does not say it can be cancelled")
	}
}

// TestControlPanelSeparatesDirections is the invariant that makes the panel
// trustworthy: steps come from the pilot, outcomes only from what magmux
// observed. A send must never be able to fabricate a completion.
func TestControlPanelSeparatesDirections(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(0, "do a thing", "gpt-5", 2)
	cp.recordSend(0, "", "first instruction")

	cp.mu.Lock()
	sent, observed, open := cp.sent, cp.observed, cp.steplog[0].done()
	cp.mu.Unlock()
	if sent != 1 || observed != 0 {
		t.Fatalf("after a send: sent=%d observed=%d, want 1/0", sent, observed)
	}
	if open {
		t.Error("step is marked done before any turn was observed")
	}

	// "working" churn must not close the step — it belongs in the live row.
	cp.recordObserved(0, "working", "", "Bash")
	cp.mu.Lock()
	stillOpen := !cp.steplog[0].done()
	cp.mu.Unlock()
	if !stillOpen {
		t.Error("a working snapshot closed the step")
	}

	cp.recordObserved(0, "awaiting_input", "done that", "Bash")
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.observed != 1 {
		t.Errorf("observed=%d after one completed turn, want 1", cp.observed)
	}
	st := cp.steplog[0]
	if !st.done() || st.reply != "done that" || st.tool != "Bash" {
		t.Errorf("step not closed with the observed result: %+v", st)
	}
	if st.dur <= 0 {
		t.Error("closed step has no turn duration")
	}
}

// TestControlPanelCountsOnlyRequestedTurns is a regression test for a panel
// that claimed work nobody asked for. A Claude Code pane reaches
// awaiting_input as soon as it boots, well before the pilot sends anything;
// counting that showed "done 1" against zero instructions.
func TestControlPanelCountsOnlyRequestedTurns(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(0, "do a thing", "", 3)

	cp.recordObserved(0, "awaiting_input", "ready", "")
	cp.mu.Lock()
	observed := cp.observed
	cp.mu.Unlock()
	if observed != 0 {
		t.Errorf("observed=%d after a boot-time idle with no instruction, want 0", observed)
	}

	cp.recordSend(0, "step 1/3", "do the first thing")
	cp.recordObserved(0, "awaiting_input", "did it", "Bash")
	cp.recordObserved(0, "awaiting_input", "still idle", "")

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.observed != 1 {
		t.Errorf("observed=%d after one instruction and a repeat idle, want 1", cp.observed)
	}
	if cp.observed > cp.sent {
		t.Errorf("observed (%d) exceeds sent (%d): claiming turns nobody requested",
			cp.observed, cp.sent)
	}
}

// TestControlPanelIgnoresOtherPanes guards against a second controlled pane
// bleeding into a panel bound to a different one.
func TestControlPanelIgnoresOtherPanes(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(1, "drive pane 1", "", 0)
	cp.recordSend(1, "step 1", "do it")
	cp.recordObserved(0, "awaiting_input", "from the wrong pane", "")

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.steplog[0].done() {
		t.Fatal("an observation from pane 0 closed a step targeting pane 1")
	}
}

// TestTruncANSIKeepsColorIntact checks that cutting a styled line to width
// never leaves a dangling escape — a half-written SGR would recolour the rest
// of the pane.
func TestTruncANSIKeepsColorIntact(t *testing.T) {
	s := paint(colSuccess, "0123456789") + paint(colError, "abcdefghij")
	got := truncANSI(s, 8)
	if visWidth(got) > 8 {
		t.Errorf("truncated to %d visible columns, want <= 8", visWidth(got))
	}
	if !strings.HasSuffix(got, sgrReset) {
		t.Errorf("truncated string does not end reset: %q", got)
	}
}

// TestControlPanelDump prints a full frame with escapes so the panel can be
// screenshotted and judged visually. The renderer cannot be reviewed from
// source; colour and alignment have to be seen.
//
//	MAGMUX_PANEL_DUMP=1 go test -run TestControlPanelDump -v
func TestControlPanelDump(t *testing.T) {
	if os.Getenv("MAGMUX_PANEL_DUMP") == "" {
		t.Skip("set MAGMUX_PANEL_DUMP=1 to print a frame for screenshotting")
	}
	cp := samplePanel()
	cp.steplog[len(cp.steplog)-1].state = "awaiting_input"
	cp.steplog[len(cp.steplog)-1].reply =
		"go vet reported 4 findings across 3 files. All four are shadowed variables " +
			"inside error branches — the inner err is assigned but never checked, so a " +
			"failure from the inner call is silently dropped while the outer err still " +
			"reads nil. I have not changed anything yet; tell me whether to fix them in " +
			"place or open an issue first."
	if os.Getenv("MAGMUX_PANEL_FINISHED") != "" {
		cp.finished = true
		cp.state = "finished"
		cp.observed = cp.sent
		cp.summary = "test suite green and every go vet warning cleared"
	}
	for _, size := range [][2]int{{46, 22}, {84, 26}} {
		for _, ln := range cp.frame(sampleFrameState(cp), cp.steplog, size[0], size[1]) {
			os.Stdout.WriteString(ln + sgrReset + "\n")
		}
		os.Stdout.WriteString("\n")
	}
}
