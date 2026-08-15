package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// samplePanel builds a panel mid-run: a pilot part-way through a five-step
// task on ONE route, with one turn still in flight. This is the shape the
// single-lane layout exists for, and the shape pilot:demo produces.
func samplePanel() *ControlPanel {
	cp := newControlPanel()
	// Relative to now, not a fixed date: the panel renders live durations, so
	// a frozen base makes every elapsed figure read as "24h 20m" and the
	// screenshot stops representing what the panel actually looks like.
	base := time.Now().Add(-160 * time.Second)
	cp.startedAt = base
	cp.model = "anthropic/claude-sonnet-5"
	cp.goal = "get the test suite green, then clear every go vet warning"
	cp.steps = 5
	cp.sent = 4
	cp.observed = 3
	cp.state = "working"
	cp.note = "pilot attached"
	r := cp.routeLocked(0)
	r.steps = 5
	r.sent = 4
	r.observed = 3
	r.state = "working"
	r.tool = "Read"
	r.durs = []float64{32, 74, 21}
	r.steplog = []ctrlStep{
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

// steplog is the sample's single route's ledger, which is what the existing
// single-lane tests are written against.
func (cp *ControlPanel) steplogOf(pane int) []ctrlStep {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if r := cp.routes[pane]; r != nil {
		return r.steplog
	}
	return nil
}

// sampleFrameState mirrors what render() snapshots. It goes through
// snapshotLocked itself, so a field added to ctrlFrameState and forgotten here
// cannot silently render as its zero value in every test at once.
func sampleFrameState(cp *ControlPanel) ctrlFrameState {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.snapshotLocked()
}

// routedPanel builds a controller driving n panes, with an interleaved stream.
func routedPanel(n int) *ControlPanel {
	cp := newControlPanel()
	cp.recordStart(0, "ship the routing panel", "anthropic/claude-opus-5", "claude-code/2.1", 0)
	cp.focused = 0
	names := []string{"api", "web", "infra", "scratch", "docs", "bench", "ui", "db"}
	for i := 0; i < n; i++ {
		title := ""
		if i < len(names) {
			title = names[i]
		}
		cp.recordRouteOpened(i, title)
		seq := cp.recordSend(i, "send_and_wait", "make the tests green in "+title)
		cp.recordAck(seq, true, "", "34 bytes + enter")
		if i%2 == 0 {
			cp.recordObserved(i, "awaiting_input", "42 passed, 0 failed", "Bash")
		} else {
			cp.recordObserved(i, "awaiting_permission", "Bash(rm -rf build) needs approval", "Bash")
		}
	}
	return cp
}

// dumpRoutedPanel builds a routed run worth looking at: several turns per
// route, an open_pane with its ack, a permission block and a closed pane, so
// every row class the layout can produce is on screen at once.
func dumpRoutedPanel(n int) *ControlPanel {
	cp := newControlPanel()
	cp.recordStart(0, "port the ledger onto routes and keep case3 green",
		"anthropic/claude-opus-5", "claude-code/2.1", 0)
	cp.startedAt = time.Now().Add(-242 * time.Second)
	cp.focused = 0
	names := []string{"api", "web", "infra", "scratch", "docs", "bench", "ui", "db"}
	tools := []string{"Bash", "Edit", "Read", "Grep"}

	seq := cp.recordRequest(-1, "open_pane", "claude --dangerously-skip-permissions")
	cp.recordAck(seq, true, "", "pane 2 · claude")

	for i := 0; i < n; i++ {
		title := names[i%len(names)]
		cp.recordRouteOpened(i, title)
		for turn := 0; turn < 2+i%3; turn++ {
			s := cp.recordSend(i, "send_and_wait",
				fmt.Sprintf("%s: make the tests green, then report what changed", title))
			cp.recordAck(s, true, "", "34 bytes + enter")
			cp.recordObserved(i, "working", "", tools[turn%len(tools)])
			switch {
			case i == 2 && turn == 1:
				cp.recordObserved(i, "awaiting_permission",
					"Bash(rm -rf build) needs approval", "Bash")
			default:
				cp.recordObserved(i, "awaiting_input",
					"42 passed, 0 failed", tools[turn%len(tools)])
			}
		}
	}
	if n >= 4 {
		s := cp.recordRequest(3, "close_pane", "pane 3")
		cp.recordAck(s, true, "", "pane 3 closed")
		cp.recordRouteClosed(3)
	}
	s := cp.recordRequest(1, "capture", "read the screen")
	cp.recordAck(s, false, "pane_dead", "pane 1 rejected the read")
	cp.recordSend(0, "send_and_wait", "now run go vet ./... and list every warning")
	cp.recordObserved(0, "working", "", "Bash")

	// Everything above happened in one instant, so every turn reads as 0ms and
	// the spark is flat. Back-date the timings so the dump shows what a real
	// run looks like — the point of the dump is to judge the visual.
	fake := []float64{12.1, 41.7, 8.4, 96.2, 3.9, 61.5, 18.8, 7.2}
	base := time.Now().Add(-242 * time.Second)
	for i, pane := range cp.routeOrder {
		r := cp.routes[pane]
		r.durs = r.durs[:0]
		for j := range r.steplog {
			at := base.Add(time.Duration(20*(i+j)) * time.Second)
			r.steplog[j].at = at
			if r.steplog[j].done() {
				d := time.Duration(fake[(i+j)%len(fake)] * float64(time.Second))
				r.steplog[j].dur = d
				r.durs = append(r.durs, d.Seconds())
			}
		}
		r.lastSent = base.Add(time.Duration(20*(i+len(r.steplog))) * time.Second)
	}
	for i := range cp.signals {
		cp.signals[i].at = base.Add(time.Duration(9*i) * time.Second)
		if cp.signals[i].turn {
			cp.signals[i].dur = time.Duration(fake[i%len(fake)] * float64(time.Second))
		}
	}
	return cp
}

// TestControlPanelFrameShape locks the panel's structural invariants: it fits
// its pane exactly, never exceeds the width, and leads with the link header.
func TestControlPanelFrameShape(t *testing.T) {
	cp := samplePanel()
	for _, size := range [][2]int{{46, 24}, {80, 20}, {120, 30}} {
		w, h := size[0], size[1]
		lines := cp.frame(sampleFrameState(cp), cp.steplogOf(0), w, h)
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
	lines := cp.frame(sampleFrameState(cp), cp.steplogOf(0), 80, 24)

	rows := 0
	for _, ln := range lines {
		for _, st := range cp.routes[0].steplog {
			if strings.Contains(ln, st.label) && strings.Contains(ln, "─") == false {
				rows++
				break
			}
		}
	}
	if rows < len(cp.routes[0].steplog) {
		t.Errorf("only %d of %d steps got a ledger row", rows, len(cp.routes[0].steplog))
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
	cp.routes[0].steplog[len(cp.routes[0].steplog)-1].state = "awaiting_input"
	cp.routes[0].steplog[len(cp.routes[0].steplog)-1].reply = strings.Repeat(
		"The vet run surfaced four findings across three files, each of them a "+
			"shadowed variable in an error branch. ", 12)

	const w, h = 80, 40
	lines := cp.frame(sampleFrameState(cp), cp.steplogOf(0), w, h)

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
	for i := range cp.routes[0].steplog {
		cp.routes[0].steplog[i].state = "awaiting_input"
		cp.routes[0].steplog[i].text = "instrmarker" + string(rune('A'+i)) + " do the thing"
		cp.routes[0].steplog[i].reply = "replymarker" + string(rune('A'+i)) + " it is done"
	}

	convo := cp.exchangeLines(cp.steplogOf(0), " ", 70)
	joined := strings.Join(convo, "\n")
	for i, st := range cp.routes[0].steplog {
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
	for i := range cp.routes[0].steplog {
		cp.routes[0].steplog[i].state = "awaiting_input"
		cp.routes[0].steplog[i].reply = strings.Repeat("a long observed reply. ", 20)
	}

	const w, h = 70, 24
	cp.scrollBy(100000) // slam against the top
	lines := cp.frame(sampleFrameState(cp), cp.steplogOf(0), w, h)
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
	lines := cp.frame(s, cp.steplogOf(0), 80, 24)
	last := strings.Join(lines, "\n")
	if !strings.Contains(last, "FINISHED") {
		t.Error("finished run does not show a FINISHED badge")
	}
	if !strings.Contains(last, "to close") {
		t.Error("finished run does not say how to close it")
	}

	s.closeIn = 12 * time.Second
	lines = cp.frame(s, cp.steplogOf(0), 80, 24)
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
	cp.recordStart(0, "do a thing", "gpt-5", "", 2)
	cp.recordSend(0, "", "first instruction")

	cp.mu.Lock()
	sent, observed, open := cp.sent, cp.observed, cp.routes[0].steplog[0].done()
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
	stillOpen := !cp.routes[0].steplog[0].done()
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
	st := cp.routes[0].steplog[0]
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
	cp.recordStart(0, "do a thing", "", "", 3)

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

// TestControlPanelIgnoresOtherPanes guards against a pane the controller has
// never touched bleeding into the panel. A magmux running four Claude sessions
// where an agent drives two must not have the other two flood the stream.
func TestControlPanelIgnoresOtherPanes(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(1, "drive pane 1", "", "", 0)
	cp.recordSend(1, "step 1", "do it")
	cp.recordObserved(0, "awaiting_input", "from the wrong pane", "")

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.routes[1].steplog[0].done() {
		t.Fatal("an observation from pane 0 closed a step targeting pane 1")
	}
	if _, ok := cp.routes[0]; ok {
		t.Error("an untouched pane opened a route just by being observed")
	}
	for _, sig := range cp.signals {
		if sig.pane == 0 {
			t.Errorf("an untouched pane reached the stream: %+v", sig)
		}
	}
}

// TestTruncANSIKeepsColorIntact checks that cutting a styled line to width
// never leaves a dangling escape — a half-written SGR would recolour the rest
// of the pane.
func TestTruncANSIKeepsColorIntact(t *testing.T) {
	s := paint(pal.success, "0123456789") + paint(pal.fail, "abcdefghij")
	got := truncANSI(s, 8)
	if visWidth(got) > 8 {
		t.Errorf("truncated to %d visible columns, want <= 8", visWidth(got))
	}
	// The cut ends at the panel's ground state, not at a bare reset: a bare
	// reset would hand the rest of the row back to the terminal's background.
	if !strings.HasSuffix(got, sgrBase()) {
		t.Errorf("truncated string does not end at the panel base: %q", got)
	}
}

// ── routing ───────────────────────────────────────────────────────────────────

// TestControlPanelAcksNeverCompleteTurns is the provenance test.
//
// magmux replying "ok, I wrote your bytes" says nothing whatsoever about the
// session. If an ack could close a turn, a controller would be able to
// fabricate a completion by being answered, and the panel would stop being
// able to show the session disagreeing with it — which is the only reason the
// panel is worth looking at.
func TestControlPanelAcksNeverCompleteTurns(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(0, "drive pane 0", "", "", 0)
	seq := cp.recordSend(0, "send_and_wait", "make the tests green")
	cp.recordAck(seq, true, "", "34 bytes + enter")

	cp.mu.Lock()
	r := cp.routes[0]
	if r.observed != 0 {
		t.Errorf("an ack moved observed to %d; only recordObserved may write it", r.observed)
	}
	if r.steplog[0].done() {
		t.Error("an ack closed the step it was answering")
	}
	acked := false
	for _, sig := range cp.signals {
		if sig.seq == seq && sig.ok != nil && *sig.ok {
			acked = true
			if sig.text != "make the tests green" {
				t.Errorf("the ack overwrote the request text: %q", sig.text)
			}
		}
	}
	cp.mu.Unlock()
	if !acked {
		t.Fatal("the ack was not recorded on the OUT row it answers")
	}
	// The ack must also not have become a row of its own.
	cp.mu.Lock()
	outs := 0
	for _, sig := range cp.signals {
		if sig.dir == "out" {
			outs++
		}
	}
	cp.mu.Unlock()
	if outs != 1 {
		t.Errorf("%d OUT rows for one send: the reply became an entry of its own", outs)
	}

	cp.recordObserved(0, "awaiting_input", "42 passed", "Bash")
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.routes[0].observed != 1 {
		t.Errorf("observed=%d after a real observation, want 1", cp.routes[0].observed)
	}
	if !cp.routes[0].steplog[0].done() {
		t.Error("recordObserved did not close the step")
	}
}

// TestControlPanelRoutesAreIndependent — one controller, N sessions. An
// observation on one route must not touch another's ledger.
func TestControlPanelRoutesAreIndependent(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(0, "drive two panes", "", "claude-code/2.1", 0)
	cp.recordSend(0, "", "instruction for pane 0")
	cp.recordSend(1, "", "instruction for pane 1")
	cp.recordObserved(1, "awaiting_input", "pane 1 is done", "Bash")

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if got := cp.routes[1].observed; got != 1 {
		t.Errorf("route 1 observed=%d, want 1", got)
	}
	if !cp.routes[1].steplog[0].done() {
		t.Error("route 1's step did not close on its own observation")
	}
	if got := cp.routes[0].observed; got != 0 {
		t.Errorf("route 0 observed=%d: another route's turn closed it", got)
	}
	if cp.routes[0].steplog[0].done() {
		t.Fatal("an observation on pane 1 closed pane 0's outstanding step")
	}
}

// TestControlPanelCountsOnlyRequestedTurnsPerRoute is the N-pane form of the
// "done 1 against zero instructions" bug. A global sent > observed pair would
// let route 0's outstanding send be closed by route 1 merely booting.
func TestControlPanelCountsOnlyRequestedTurnsPerRoute(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(0, "drive two panes", "", "claude-code/2.1", 0)
	cp.recordRouteOpened(1, "web")
	cp.recordSend(0, "", "do the thing on pane 0")

	// Pane 1 reaches awaiting_input as soon as it boots. Nothing was asked of
	// it, and route 0's outstanding send is not its to answer.
	cp.recordObserved(1, "awaiting_input", "ready", "")

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if got := cp.routes[1].observed; got != 0 {
		t.Errorf("route 1 observed=%d for a boot-time idle it was never sent, want 0", got)
	}
	if got := cp.routes[0].observed; got != 0 {
		t.Errorf("route 0 observed=%d: route 1's idle closed route 0's step", got)
	}
	if cp.observed != 0 {
		t.Errorf("run total observed=%d, want 0", cp.observed)
	}
}

// TestControlPanelSingleRouteRendersLegacyLayout is the pilot:demo / case3
// guard. One route and no MCP-level controller must render exactly as before —
// a routing table with one row in it is strictly worse than the link header it
// would replace.
func TestControlPanelSingleRouteRendersLegacyLayout(t *testing.T) {
	cp := samplePanel()
	joined := strings.Join(cp.frame(sampleFrameState(cp), cp.steplogOf(0), 80, 30), "\n")
	if !strings.Contains(joined, "PILOT") || !strings.Contains(joined, "SESSION") {
		t.Error("the single-route panel lost the PILOT ══▶ SESSION link header")
	}
	if strings.Contains(joined, "CONTROLLER") {
		t.Error("the routed header appeared for a single-route pilot run")
	}
	for _, st := range cp.steplogOf(0) {
		if !strings.Contains(joined, st.label) {
			t.Errorf("step %q lost its ledger row", st.label)
		}
	}
}

// TestControlPanelRouteTableFitsAndScales — the table must fit its pane at
// every size the grid produces, and no open route may vanish silently.
func TestControlPanelRouteTableFitsAndScales(t *testing.T) {
	for _, n := range []int{1, 2, 3, 7} {
		cp := routedPanel(n)
		for _, size := range [][2]int{{46, 22}, {80, 24}, {120, 40}} {
			w, h := size[0], size[1]
			lines := cp.frame(sampleFrameState(cp), nil, w, h)
			if len(lines) > h {
				t.Errorf("%d routes at %dx%d: %d lines for %d rows", n, w, h, len(lines), h)
			}
			for i, ln := range lines {
				if got := visWidth(ln); got > w {
					t.Errorf("%d routes at %dx%d: line %d is %d columns: %q", n, w, h, i, got, ln)
				}
			}
			// The table region is the header plus at most one line per route
			// (plus its overflow line). Looking only there matters: every route
			// also has stream entries carrying the same tag, so searching the
			// whole frame would pass even with no table at all.
			table := strings.Join(lines[:minInt(len(lines), 4+n+1)], "\n")
			overflow := strings.Contains(table, "more")
			for i := 0; i < n; i++ {
				tag := " " + strconv.Itoa(i) + " "
				if i == cp.focused {
					tag = "▸" + strconv.Itoa(i)
				}
				if !strings.Contains(table, tag) && !overflow {
					t.Errorf("%d routes at %dx%d: route %d has neither a row nor an overflow count:\n%s",
						n, w, h, i, table)
				}
			}
		}
	}
}

// TestControlPanelDegradesToStrip — a short pane still has to answer "which
// pane is blocked", so the table collapses rather than disappearing.
func TestControlPanelDegradesToStrip(t *testing.T) {
	cp := routedPanel(5)
	const w, h = 80, 12
	lines := cp.frame(sampleFrameState(cp), nil, w, h)
	if len(lines) > h {
		t.Fatalf("strip frame produced %d lines for %d rows", len(lines), h)
	}
	rows := 0
	for _, ln := range lines {
		if strings.Contains(ln, "AWAITING") || strings.Contains(ln, "WORKING") {
			rows++
		}
	}
	if rows > 1 {
		t.Errorf("%d full table rows at h=%d; the table should have collapsed", rows, h)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "▸0") {
		t.Error("the strip does not name the focused route")
	}
}

// TestControlPanelFilterShowsOneRoute — 1-9 narrows to one PANE INDEX, and
// filtering follows the tail again rather than keeping a stale scroll offset.
func TestControlPanelFilterShowsOneRoute(t *testing.T) {
	cp := routedPanel(3)
	cp.scrollBy(40)
	if !cp.setFilter(1) {
		t.Fatal("setFilter(1) refused a route that exists")
	}
	cp.mu.Lock()
	if cp.scroll != 0 {
		t.Errorf("filtering left scroll at %d; it must follow the tail", cp.scroll)
	}
	cp.mu.Unlock()

	joined := strings.Join(cp.frame(sampleFrameState(cp), nil, 80, 30), "\n")
	if !strings.Contains(joined, "make the tests green in web") {
		t.Error("the filtered view does not show route 1's own traffic")
	}
	if strings.Contains(joined, "make the tests green in api") {
		t.Error("route 0's traffic survived a filter to route 1")
	}
	if cp.setFilter(7) {
		t.Error("setFilter accepted a pane with no route")
	}
}

// TestControlPanelFinishFooterSurvivesManyRoutes — the closing statement is
// reserved before the table, so a wide run can never push it off the bottom.
func TestControlPanelFinishFooterSurvivesManyRoutes(t *testing.T) {
	cp := routedPanel(8)
	cp.recordFinish("every route reported green", false)
	lines := cp.frame(sampleFrameState(cp), nil, 80, 20)
	if len(lines) > 20 {
		t.Fatalf("frame produced %d lines for 20 rows", len(lines))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "FINISHED") {
		t.Error("8 routes pushed the closing statement off the panel")
	}
	if !strings.Contains(joined, "to close") {
		t.Error("the finished run no longer says how to close it")
	}
}

// TestControlPanelStreamIsChronological — the stream is the causality
// instrument, so entries appear in the order they happened, each carrying the
// route it belongs to. Ordering by pane instead would destroy the one fact the
// table cannot show.
func TestControlPanelStreamIsChronological(t *testing.T) {
	cp := routedPanel(3)
	cp.mu.Lock()
	sigs := append([]ctrlSignal(nil), cp.signals...)
	cp.mu.Unlock()
	if len(sigs) < 6 {
		t.Fatalf("only %d signals recorded for three routes", len(sigs))
	}
	for i := 1; i < len(sigs); i++ {
		if sigs[i].seq <= sigs[i-1].seq {
			t.Fatalf("signal %d is out of order: seq %d after %d", i, sigs[i].seq, sigs[i-1].seq)
		}
	}

	// Interleaving is the point: a pane's OUT must be followed by that same
	// pane's IN before the next pane's OUT, exactly as it happened.
	var order []int
	for _, sig := range sigs {
		if sig.pane >= 0 {
			order = append(order, sig.pane)
		}
	}
	want := []int{0, 0, 1, 1, 2, 2}
	if len(order) != len(want) {
		t.Fatalf("route tags %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("route tags %v, want %v", order, want)
		}
	}
}

// ── run-level identity vs. a run start ────────────────────────────────────────

// pilotMsg decodes a pilot event off the wire, so these tests exercise the
// distinction that actually matters — a `pane` field that is ABSENT versus one
// that is present and 0 — rather than a Go zero value that cannot tell them
// apart.
func pilotMsg(t *testing.T, raw string) sockMsg {
	t.Helper()
	var msg sockMsg
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return msg
}

// pilotHost is a Magmux with nothing but a panel: dispatchPilotMsg's start arm
// touches the panel and broadcastEvent, and broadcastEvent with no subscribers
// writes nowhere. No PTY, no socket, no goroutines.
func pilotHost() *Magmux { return &Magmux{control: newControlPanel()} }

// TestPilotStartWithoutPaneOpensNoRoute — a `start` that names no pane
// identifies the CONTROLLER, not a route, and a route means "a pane this
// controller has touched". Mapping the absent field to 0 (as it once did) put a
// pane nothing had driven into the ledger at 0/0, and a controller that then
// drove panes 1 and 2 looked as though it had dropped an instruction to pane 0.
func TestPilotStartWithoutPaneOpensNoRoute(t *testing.T) {
	m := pilotHost()
	if err := m.dispatchPilotMsg(pilotMsg(t,
		`{"type":"pilot","event":"start","client":"claude-code/2.1"}`)); err != nil {
		t.Fatalf("identity start rejected: %v", err)
	}

	cp := m.control
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.routes) != 0 || len(cp.routeOrder) != 0 {
		t.Errorf("an identity announcement opened routes %v; it touched no pane", cp.routeOrder)
	}
	if cp.client != "claude-code/2.1" {
		t.Errorf("client = %q, want claude-code/2.1 — the header is what the event is for", cp.client)
	}
	if !strings.Contains(cp.note, "claude-code/2.1") {
		t.Errorf("note = %q, want it to name the controller that attached", cp.note)
	}
}

// TestPilotStartWithPaneStillOpensRoute is the pilot:demo / case3 guard: the pi
// pilot announces `pane:0` and must still get route 0 and the legacy layout.
func TestPilotStartWithPaneStillOpensRoute(t *testing.T) {
	m := pilotHost()
	if err := m.dispatchPilotMsg(pilotMsg(t, `{"type":"pilot","event":"start","pane":0,`+
		`"goal":"get the suite green","steps":5,"model":"anthropic/claude-sonnet-5"}`)); err != nil {
		t.Fatalf("pi-pilot start rejected: %v", err)
	}

	cp := m.control
	cp.mu.Lock()
	r := cp.routes[0]
	if r == nil {
		t.Fatalf("a start naming pane 0 opened no route (routes %v)", cp.routeOrder)
	}
	if r.goal != "get the suite green" {
		t.Errorf("route goal = %q, want the announced goal", r.goal)
	}
	cp.mu.Unlock()

	joined := strings.Join(cp.frame(sampleFrameState(cp), cp.steplogOf(0), 80, 30), "\n")
	if !strings.Contains(joined, "PILOT") || !strings.Contains(joined, "SESSION") {
		t.Error("a single-route pilot run lost the legacy PILOT ══▶ SESSION layout")
	}

	// A pane that WAS given and is not an index is still refused rather than
	// rounded down: the announced pane is where every later pane-less send goes.
	if err := m.dispatchPilotMsg(pilotMsg(t,
		`{"type":"pilot","event":"start","pane":"api"}`)); err == nil {
		t.Error("a start naming a label was accepted; it must be refused, not rounded to 0")
	}
}

// TestIdentityStartDoesNotWipeRunCounters pins the second half of the rule.
// recordStart's zeroing means "a NEW RUN begins"; an identity announcement is
// not that. MCP fires one per attached session, which can land while a run is
// already in flight, and wiping the ledger there deletes exactly the traffic
// the panel exists to show.
func TestIdentityStartDoesNotWipeRunCounters(t *testing.T) {
	m := pilotHost()
	cp := m.control
	cp.recordStart(1, "drive pane 1", "gpt-5", "", 3)
	seq := cp.recordSend(1, "send_and_wait", "make the tests green")
	cp.recordAck(seq, true, "", "34 bytes + enter")
	cp.recordObserved(1, "awaiting_input", "42 passed, 0 failed", "Bash")
	cp.mu.Lock()
	wantSignals, wantSent, wantObserved := len(cp.signals), cp.sent, cp.observed
	wantStart := cp.startedAt
	cp.mu.Unlock()

	if err := m.dispatchPilotMsg(pilotMsg(t,
		`{"type":"pilot","event":"start","client":"claude-code/2.1"}`)); err != nil {
		t.Fatalf("identity start rejected: %v", err)
	}

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if r := cp.routes[1]; r == nil || r.sent != 1 || r.observed != 1 {
		t.Errorf("the run's route was wiped by an identity announcement: %+v", r)
	}
	if cp.sent != wantSent || cp.observed != wantObserved {
		t.Errorf("counters became %d/%d, want %d/%d", cp.sent, cp.observed, wantSent, wantObserved)
	}
	if len(cp.signals) != wantSignals {
		t.Errorf("stream became %d signals, want the run's %d", len(cp.signals), wantSignals)
	}
	if !cp.startedAt.Equal(wantStart) {
		t.Error("an identity announcement restarted the clock on a run already in progress")
	}
	// What it MAY change: the header. Nothing else.
	if cp.client != "claude-code/2.1" {
		t.Errorf("client = %q, want the announced identity", cp.client)
	}
	if cp.goal != "drive pane 1" || cp.model != "gpt-5" || cp.steps != 3 {
		t.Errorf("the header lost the run's own goal/model/steps: %q %q %d",
			cp.goal, cp.model, cp.steps)
	}
}

// TestControlPanelKnownClientNoRoutesRendersSanely — the state this fix
// creates: a controller has said who it is and driven nothing yet. Zero routes
// with a non-empty client takes the routed layout, so it must fit its pane and
// say who is connected, and must NOT claim a PILOT ══▶ SESSION link to a
// session that does not exist.
func TestControlPanelKnownClientNoRoutesRendersSanely(t *testing.T) {
	m := pilotHost()
	if err := m.dispatchPilotMsg(pilotMsg(t,
		`{"type":"pilot","event":"start","client":"claude-code/2.1"}`)); err != nil {
		t.Fatalf("identity start rejected: %v", err)
	}
	cp := m.control

	for _, size := range [][2]int{{46, 12}, {46, 22}, {80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		lines := cp.frame(sampleFrameState(cp), nil, w, h)
		if len(lines) > h {
			t.Errorf("%dx%d: %d lines for %d rows", w, h, len(lines), h)
		}
		if len(lines) == 0 {
			t.Fatalf("%dx%d: an attached controller rendered nothing at all", w, h)
		}
		for i, ln := range lines {
			if got := visWidth(ln); got > w {
				t.Errorf("%dx%d: line %d is %d columns: %q", w, h, i, got, ln)
			}
		}
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "SESSION") {
			t.Errorf("%dx%d: the panel named a session it has no route to:\n%s", w, h, joined)
		}
		if !strings.Contains(joined, "CONTROLLER") {
			t.Errorf("%dx%d: the routed header is missing:\n%s", w, h, joined)
		}
		if w >= 80 && !strings.Contains(joined, "claude-code/2.1") {
			t.Errorf("%dx%d: the frame does not say who is connected:\n%s", w, h, joined)
		}
	}
}

// ── palettes ─────────────────────────────────────────────────────────────────

// TestControlPanelImposesNoBackground is the correction to the previous round.
//
// The reported bug was the panel's FOREGROUND: every colour was Catppuccin
// Mocha, so on a light terminal #CDD6F4 body text sat on cream at 1.31:1. The
// round that fixed it also made the panel fill every cell with pal.base, which
// put a grey slab inside a cream terminal — a colour magmux has no business
// choosing. A multiplexer blends into the terminal it runs in.
//
// So: the ONLY background the panel may set is a badge's chip, which sets its
// own foreground in the same breath and is therefore legible anywhere. The
// grep-able form of that rule is "no 48;2; in the frame except at the start of
// a badge", and it is asserted here on the real bytes and again on the cells
// they produce after the real VT parser has run.
func TestControlPanelImposesNoBackground(t *testing.T) {
	// badgeOpen is exactly what badge() emits before its label: a truecolor
	// background, pal.ink on top of it, bold. Any other 48;2; is the bug.
	badgeOpen := func(s string) bool {
		i := strings.Index(s, "m")
		if i < 0 {
			return false
		}
		return strings.HasPrefix(s[i+1:], fg(pal.ink)+sgrBold)
	}

	for _, kind := range []themeKind{themeDark, themeLight} {
		t.Run(kind.String(), func(t *testing.T) {
			defer useTheme(kind)()

			const w, h = 84, 26
			// Every colour a badge is ever filled with. A cell background
			// outside this set is a background the panel invented.
			allowed := map[rgb]bool{
				pal.success: true, pal.running: true, pal.warn: true,
				pal.fail: true, pal.accent: true, pal.subtle: true, pal.dead: true,
			}

			for name, cp := range map[string]*ControlPanel{
				"single route": samplePanel(),
				"routed":       dumpRoutedPanel(4),
			} {
				p := newControlPane(0, 0, h, w, "control")
				cp.attach(p)
				cp.markDirty()
				cp.render()

				lines := cp.frame(sampleFrameState(cp), cp.steplogOf(0), w, h)
				data := string(paintFrame(lines, h))
				for at := 0; ; {
					i := strings.Index(data[at:], "\x1b[48;2;")
					if i < 0 {
						break
					}
					at += i
					if !badgeOpen(data[at:]) {
						t.Fatalf("%s: the panel set a background at byte %d that is not a "+
							"badge chip (%q…) — the terminal's background is the user's, "+
							"and a slab of ours inside it is the bug this replaced",
							name, at, data[at:min(at+40, len(data))])
					}
					at += len("\x1b[48;2;")
				}

				p.mu.Lock()
				for row := 0; row < h; row++ {
					for col := 0; col < w; col++ {
						c := p.screen.cells[row][col]
						if !c.Bg.True {
							continue // the terminal's own background: correct
						}
						if !allowed[rgb{c.Bg.R, c.Bg.G, c.Bg.B}] {
							p.mu.Unlock()
							t.Fatalf("%s: cell %d,%d carries background %+v, which is not a "+
								"badge fill — the panel painted a background of its own",
								name, row, col, c.Bg)
						}
					}
				}
				p.mu.Unlock()
			}
		})
	}
}

// TestControlPanelRendersInBothPalettes runs the layout invariants — fits the
// pane, never overruns the width — against both palettes, and checks that the
// two actually differ. Escapes are wider in bytes than the text they colour,
// so a palette swap is exactly the kind of change that can push a line over
// the width if anything measures bytes instead of columns.
func TestControlPanelRendersInBothPalettes(t *testing.T) {
	for _, kind := range []themeKind{themeDark, themeLight} {
		t.Run(kind.String(), func(t *testing.T) {
			defer useTheme(kind)()
			for _, cp := range []*ControlPanel{samplePanel(), routedPanel(4)} {
				for _, size := range [][2]int{{46, 24}, {80, 20}, {120, 30}} {
					w, h := size[0], size[1]
					lines := cp.frame(sampleFrameState(cp), cp.steplogOf(0), w, h)
					if len(lines) > h {
						t.Errorf("%dx%d: %d lines for %d rows", w, h, len(lines), h)
					}
					for i, ln := range lines {
						if got := visWidth(ln); got > w {
							t.Errorf("%dx%d: line %d is %d columns: %q", w, h, i, got, ln)
						}
					}
					// Every row of the pane is addressed, including the ones
					// the frame did not fill: the cursor is homed on each, so
					// a short frame cannot leave a previous frame behind.
					data := string(paintFrame(lines, h))
					for i := 1; i <= h; i++ {
						if !strings.Contains(data, fmt.Sprintf("\x1b[%d;1H", i)) {
							t.Errorf("%dx%d: row %d was never painted", w, h, i)
						}
					}
					// And no bare reset is ever left standing: every one is
					// immediately followed by the panel's ground state, so a
					// colour cannot run past the token that set it.
					for at := 0; ; {
						i := strings.Index(data[at:], sgrReset)
						if i < 0 {
							break
						}
						at += i
						if !strings.HasPrefix(data[at:], sgrBase()) {
							t.Fatalf("%dx%d: a bare reset at byte %d leaves no foreground "+
								"in force for the rest of the row", w, h, at)
						}
						at += len(sgrReset)
					}
				}
			}
		})
	}

	// The palettes have to actually differ, or the whole mechanism is inert.
	cp := samplePanel()
	defer useTheme(themeDark)()
	dark := strings.Join(cp.frame(sampleFrameState(cp), cp.steplogOf(0), 80, 24), "\n")
	setTheme(themeLight)
	light := strings.Join(cp.frame(sampleFrameState(cp), cp.steplogOf(0), 80, 24), "\n")
	if dark == light {
		t.Error("the light and dark frames are byte-identical; the palette is not being applied")
	}
	if visWidth(dark) != visWidth(light) {
		t.Error("the two palettes produce different visible widths; a colour changed the layout")
	}
}

// TestControlPanelTicksWhileAnyRouteWorks — liveness is PER ROUTE.
//
// cp.state is the run-global last-observed state, written by whichever route
// moved most recently. Deciding the once-a-second repaint from it means that
// with two driven panes, the moment route 1 settles to awaiting_input the tick
// stops while route 0 is still mid-turn — and nothing restarts it, because
// recordObserved only marks the panel dirty on a change and a working pane
// changing tools is not one. routeRow's in-flight ‹elapsed and the header clock
// then sit frozen on screen for the pane the operator is actually waiting on,
// which is precisely the "wedged, or just slow?" question they exist to answer.
func TestControlPanelTicksWhileAnyRouteWorks(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(0, "drive two sessions", "", "claude-code/2.1", 0)
	cp.recordSend(0, "step 1", "work on pane 0")
	cp.recordSend(1, "step 1", "work on pane 1")
	// Route 1 finishes; route 0 is still mid-turn. This is the write that leaves
	// cp.state saying "awaiting_input" for the whole run.
	cp.recordObserved(1, "awaiting_input", "42 passed, 0 failed", "Bash")

	p := newControlPane(0, 0, 24, 80, "control")
	cp.attach(p)
	cp.markDirty()
	cp.render() // the paint those records earned

	// repainted reports whether an aged panel paints again with nothing new to
	// say — i.e. whether the elapsed counters are still moving.
	repainted := func() bool {
		cp.mu.Lock()
		cp.lastPaint = time.Now().Add(-2 * time.Second)
		cp.mu.Unlock()
		p.mu.Lock()
		p.dirty = false
		p.mu.Unlock()
		cp.render()
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.dirty
	}

	if !repainted() {
		t.Error("the panel stopped ticking while route 0's turn was still in flight: " +
			"its ‹elapsed and the header clock are frozen on the pane that has not finished")
	}

	// The converse half, which is what stops the fix being "always tick": once
	// every route has settled the panel is idle, and an idle pane costs nothing.
	cp.recordObserved(0, "awaiting_input", "done too", "Bash")
	cp.render() // consume the dirty flag that observation set
	if repainted() {
		t.Error("the panel keeps repainting once every route has settled; an idle pane must cost nothing")
	}
}

// TestControlPanelFinishedOutcomeWinsInTheHeader — the header may not
// contradict the footer.
//
// recordFinish states the run's outcome by setting cp.state to
// finished/failed, but applyRouteToState overwrites s.state with the route's
// last OBSERVED state, and snapshotLocked applies it on every single-route run —
// which is the pilot path. The session's last turn ended fine, so after a
// `pilot fail` the header painted a green AWAITING directly above a red FAILED
// footer, on exactly the run whose outcome matters most.
func TestControlPanelFinishedOutcomeWinsInTheHeader(t *testing.T) {
	cp := samplePanel()
	// The last turn completed normally: the failure is the RUN's verdict, not
	// this turn's, which is what makes the two disagree.
	cp.recordObserved(0, "awaiting_input", "go vet is clean", "Bash")
	cp.recordFinish("could not reproduce the reported bug", true)

	lines := cp.frame(sampleFrameState(cp), cp.steplogOf(0), 80, 30)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "FAILED") {
		t.Fatal("a failed run does not say FAILED anywhere")
	}
	header := strings.Join(lines[:minInt(len(lines), 4)], "\n")
	if strings.Contains(header, "AWAITING") {
		t.Errorf("the header badge reports the last turn's state, contradicting the FAILED footer:\n%s", header)
	}
	if !strings.Contains(header, stateBadge("failed")) {
		t.Errorf("the header does not carry the run's own outcome:\n%s", header)
	}

	// A finished run is the same rule with the other verdict.
	ok := samplePanel()
	ok.recordObserved(0, "awaiting_input", "all green", "Bash")
	ok.recordFinish("done", false)
	okHeader := strings.Join(ok.frame(sampleFrameState(ok), ok.steplogOf(0), 80, 30)[:4], "\n")
	if !strings.Contains(okHeader, stateBadge("finished")) {
		t.Errorf("a finished run does not carry its outcome in the header:\n%s", okHeader)
	}
}

// TestControlPanelDump prints a full frame with escapes so the panel can be
// screenshotted and judged visually. The renderer cannot be reviewed from
// source; colour and alignment have to be seen.
//
// MAGMUX_THEME=light selects the light palette, which is the only way to look
// at it without a light terminal to hand.
//
//	MAGMUX_PANEL_DUMP=1 go test -run TestControlPanelDump -v
//	MAGMUX_PANEL_DUMP=1 MAGMUX_PANEL_ROUTES=3 go test -run TestControlPanelDump -v
func TestControlPanelDump(t *testing.T) {
	if os.Getenv("MAGMUX_PANEL_DUMP") == "" {
		t.Skip("set MAGMUX_PANEL_DUMP=1 to print a frame for screenshotting")
	}
	if themeSetting("", os.Getenv("MAGMUX_THEME")) == "light" {
		defer useTheme(themeLight)()
	}
	// No padding and no background: the panel is judged on the terminal it is
	// dumped into, which is exactly how it is judged in use. Dumping it on a
	// background of its own would misrepresent the thing this exists to show.
	dump := func(lines []string, w int) {
		for _, ln := range lines {
			os.Stdout.WriteString(sgrBase() + ln + sgrReset + "\n")
		}
		os.Stdout.WriteString("\n")
	}
	if n := os.Getenv("MAGMUX_PANEL_ROUTES"); n != "" {
		routes, err := strconv.Atoi(n)
		if err != nil || routes < 1 {
			t.Fatalf("MAGMUX_PANEL_ROUTES=%q is not a positive count", n)
		}
		cp := dumpRoutedPanel(routes)
		for _, size := range [][2]int{{46, 22}, {84, 26}} {
			dump(cp.frame(sampleFrameState(cp), nil, size[0], size[1]), size[0])
		}
		return
	}
	cp := samplePanel()
	cp.routes[0].steplog[len(cp.routes[0].steplog)-1].state = "awaiting_input"
	cp.routes[0].steplog[len(cp.routes[0].steplog)-1].reply =
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
		dump(cp.frame(sampleFrameState(cp), cp.steplogOf(0), size[0], size[1]), size[0])
	}
}
