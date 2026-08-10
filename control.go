package main

// Control panel — the view of a *controlled* session.
//
// A controlled session is one an external AI agent (the "pilot") is steering
// through a multi-step task: the pilot sends an instruction, the tool works,
// magmux observes the turn finish, the pilot reads the result and sends the
// next instruction. The control panel is where that traffic is visible.
//
// It renders into a Pane that has no PTY. magmux paints it by writing raw
// ANSI through the pane's own VT parser, exactly as a child process would,
// so the panel inherits borders, scrollback, selection and dirty-flag
// rendering from the normal pane path and needs no renderer of its own.
//
// The two directions come from different places on purpose:
//
//	OUT  the pilot told us what it sent (dispatchSocketMsg, "send")
//	IN   magmux observed the pane's own turn finishing (pollControllers)
//
// The panel therefore cannot show a step as completed just because the pilot
// believes it was — an IN row exists only if a controller actually saw the
// session come back to awaiting_input.

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ── Palette ───────────────────────────────────────────────────────────────────
//
// Semantic, not decorative: one colour means one thing across the whole panel.
// Chrome (borders, timestamps, labels) is low-contrast and recedes; data and
// state badges are saturated and carry the eye.

type rgb struct{ r, g, b uint8 }

var (
	colSuccess = rgb{0x2E, 0xCC, 0x71} // turn completed / session idle
	colRunning = rgb{0x34, 0x98, 0xDB} // pilot instruction in flight
	colWarn    = rgb{0xFF, 0xB4, 0x54} // tool working
	colError   = rgb{0xFF, 0x6B, 0x6B} // error / permission block
	colAccent  = rgb{0x89, 0xB4, 0xFA} // titles, focus
	colText    = rgb{0xCD, 0xD6, 0xF4} // body text
	colSubtle  = rgb{0x6C, 0x70, 0x86} // labels, timestamps
	colBorder  = rgb{0x45, 0x47, 0x5A} // rules
	colInk     = rgb{0x11, 0x11, 0x1B} // text on a bright badge
	colDead    = rgb{0x6C, 0x70, 0x86} // absent / not applicable
	colDebug   = rgb{0x8A, 0x8F, 0xA0} // secondary data (tool names)
)

func fg(c rgb) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b) }
func bg(c rgb) string { return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.r, c.g, c.b) }

const sgrReset = "\x1b[0m"
const sgrBold = "\x1b[1m"

// paint wraps s in a foreground colour and resets after.
func paint(c rgb, s string) string { return fg(c) + s + sgrReset }

// badge renders a status chip: dark ink on a saturated background. Reads as a
// discrete state at a glance, which plain coloured text does not.
func badge(label string, c rgb) string {
	return bg(c) + fg(colInk) + sgrBold + " " + label + " " + sgrReset
}

// blend interpolates n colours from `from` to `to`. One colour per cell makes
// a meter read as a continuous ramp instead of four chunky steps.
func blend(n int, from, to rgb) []rgb {
	if n <= 1 {
		return []rgb{to}
	}
	out := make([]rgb, n)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(n-1)
		out[i] = rgb{
			r: uint8(float64(from.r) + (float64(to.r)-float64(from.r))*t),
			g: uint8(float64(from.g) + (float64(to.g)-float64(from.g))*t),
			b: uint8(float64(from.b) + (float64(to.b)-float64(from.b))*t),
		}
	}
	return out
}

// meter renders a gradient progress bar. The fill ratio and the colour both
// encode magnitude, so it survives greyscale and colour-blind viewing.
func meter(frac float64, width int, from, to rgb) string {
	if width <= 0 {
		return ""
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	cols := blend(width, from, to)
	filled := int(frac * float64(width))
	var b strings.Builder
	for i := 0; i < width; i++ {
		if i < filled {
			b.WriteString(fg(cols[i]) + "█")
		} else {
			b.WriteString(fg(colBorder) + "░")
		}
	}
	b.WriteString(sgrReset)
	return b.String()
}

var sparkRunes = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// spark renders a one-line trend. Used for per-turn durations, so a session
// that is getting progressively slower is visible without reading numbers.
func spark(values []float64, c rgb) string {
	if len(values) == 0 {
		return ""
	}
	max := values[0]
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		max = 1
	}
	var b strings.Builder
	b.WriteString(fg(c))
	for _, v := range values {
		idx := int(v / max * float64(len(sparkRunes)-1))
		if idx < 0 {
			idx = 0
		}
		if idx > len(sparkRunes)-1 {
			idx = len(sparkRunes) - 1
		}
		b.WriteRune(sparkRunes[idx])
	}
	b.WriteString(sgrReset)
	return b.String()
}

// ── Model ─────────────────────────────────────────────────────────────────────

// ctrlStep is one instruction and the turn it produced — the two directions
// joined into a single record.
//
// The panel is deliberately NOT a chronological log. The pilot's own pane is
// already a log, and two scrolling logs side by side are impossible to tell
// apart at a glance. This is the instrument panel for the same run: a fixed
// ledger of steps with durations, tools and outcomes, where the eye compares
// rows instead of reading paragraphs.
type ctrlStep struct {
	n     int           // 1-based step number
	label string        // "step 2/5" — the pilot's own tag
	text  string        // the instruction sent
	at    time.Time     // when it went out
	dur   time.Duration // how long the turn took; zero while in flight
	tool  string        // last tool the session used during the turn
	state string        // observed terminal state, "" while in flight
	reply string        // what the session reported back
}

// done reports whether the turn this step asked for has settled.
func (s ctrlStep) done() bool { return s.state != "" }

// bad reports whether the step ended in a state the pilot must react to.
func (s ctrlStep) bad() bool {
	return s.state == "error" || s.state == "awaiting_permission"
}

// ControlPanel holds the command log and the live status of a controlled
// session. Safe for concurrent use: the socket goroutine appends OUT rows
// while the render loop appends IN rows and reads for painting.
type ControlPanel struct {
	mu sync.Mutex

	pane *Pane // the virtual pane we paint into; nil if not enabled

	goal      string
	target    int // pane index being controlled; -1 until the pilot announces
	model     string
	steps     int // total steps the pilot planned, 0 if open-ended
	sent      int
	observed  int
	startedAt time.Time
	finished  bool
	summary   string

	state    string    // last observed state of the target pane
	stateAt  time.Time // when that state was observed
	lastSent time.Time // when the last instruction went out

	steplog []ctrlStep // the ledger, oldest first
	note    string     // latest panel-level annotation (attach / finish / fail)
	noteBad bool

	// scroll is how many lines the exchange view is lifted off the bottom.
	// Zero means "follow the newest", which is what a live panel should do
	// until the user deliberately looks back.
	scroll int

	// closeIn is the remaining auto-close countdown, refreshed by the render
	// loop. Zero means the run waits for an explicit keypress instead.
	closeIn time.Duration

	// needsPaint gates repainting. magmux's whole rendering model is that an
	// idle pane costs nothing, so the panel must not redraw every frame just
	// because it owns a pane — it repaints on change, plus once a second
	// while a turn is in flight to keep the elapsed counters honest.
	needsPaint bool
	lastPaint  time.Time
}

const ctrlMaxSteps = 200

// ctrlMaxScroll bounds the scroll offset so a held PageUp cannot run away
// while the real limit (the wrapped line count) is only known at paint time.
const ctrlMaxScroll = 100000

func newControlPanel() *ControlPanel {
	return &ControlPanel{target: -1, startedAt: time.Now(), state: "idle"}
}

func (cp *ControlPanel) enabled() bool { return cp != nil && cp.pane != nil }

// targetPane is the pane index the pilot announced it is driving, or 0 if it
// never announced one.
func (cp *ControlPanel) targetPane() int {
	if cp == nil {
		return 0
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.target < 0 {
		return 0
	}
	return cp.target
}

// scrollBy moves the exchange view. Positive scrolls back through history,
// negative returns towards the newest. Returns true if anything moved, so the
// caller can tell a consumed key from an ignored one.
func (cp *ControlPanel) scrollBy(lines int) bool {
	if cp == nil {
		return false
	}
	cp.mu.Lock()
	prev := cp.scroll
	cp.scroll += lines
	if cp.scroll < 0 {
		cp.scroll = 0
	}
	// The upper bound depends on the wrapped line count, which only the
	// renderer knows. It clamps on the next paint; here we just avoid
	// unbounded growth from someone leaning on PageUp.
	if cp.scroll > ctrlMaxScroll {
		cp.scroll = ctrlMaxScroll
	}
	moved := cp.scroll != prev
	cp.needsPaint = moved
	cp.mu.Unlock()
	return moved
}

// setCloseIn publishes the remaining auto-close countdown for the footer.
// Repaints only when the displayed second changes, so a running countdown
// costs one repaint per second rather than one per frame.
func (cp *ControlPanel) setCloseIn(d time.Duration) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	if int(d.Seconds()) != int(cp.closeIn.Seconds()) {
		cp.needsPaint = true
	}
	cp.closeIn = d
	cp.mu.Unlock()
}

// cancelClose stops an armed countdown — any keypress does this, so a run
// cannot close while someone is reading it.
func (cp *ControlPanel) cancelClose() {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	if cp.closeIn != 0 {
		cp.closeIn = 0
		cp.needsPaint = true
	}
	cp.mu.Unlock()
}

// scrollToBottom resumes following the newest exchange.
func (cp *ControlPanel) scrollToBottom() {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	moved := cp.scroll != 0
	cp.scroll = 0
	cp.needsPaint = cp.needsPaint || moved
	cp.mu.Unlock()
}

// setNote records a panel-level annotation (pilot attached, finished, failed).
func (cp *ControlPanel) setNote(text string, bad bool) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.note = text
	cp.noteBad = bad
	cp.mu.Unlock()
	cp.markDirty()
}

func (cp *ControlPanel) markDirty() {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.needsPaint = true
	cp.mu.Unlock()
}

// recordStart notes the pilot announcing itself and the task it will drive.
func (cp *ControlPanel) recordStart(pane int, goal, model string, steps int) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.target = pane
	cp.goal = goal
	cp.model = model
	cp.steps = steps
	cp.startedAt = time.Now()
	cp.finished = false
	cp.summary = ""
	cp.steplog = nil
	cp.mu.Unlock()
	cp.setNote("pilot attached", false)
}

// recordSend logs an instruction the pilot pushed into the session.
func (cp *ControlPanel) recordSend(pane int, label, text string) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.sent++
	cp.lastSent = time.Now()
	cp.state = "working"
	cp.stateAt = cp.lastSent
	if cp.target < 0 {
		cp.target = pane
	}
	if label == "" {
		if cp.steps > 0 {
			label = fmt.Sprintf("step %d/%d", cp.sent, cp.steps)
		} else {
			label = fmt.Sprintf("step %d", cp.sent)
		}
	}
	cp.steplog = append(cp.steplog, ctrlStep{
		n: cp.sent, label: label, text: text, at: cp.lastSent,
	})
	if len(cp.steplog) > ctrlMaxSteps {
		cp.steplog = cp.steplog[len(cp.steplog)-ctrlMaxSteps:]
	}
	cp.mu.Unlock()
	cp.markDirty()
}

// recordObserved logs a state magmux itself saw on the controlled pane. This
// is deliberately not driven by the pilot: the panel must be able to show the
// session disagreeing with what the pilot thinks it asked for.
func (cp *ControlPanel) recordObserved(pane int, state, response, tool string) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	if cp.target >= 0 && cp.target != pane {
		cp.mu.Unlock()
		return
	}
	prevState := cp.state
	cp.state = state
	cp.stateAt = time.Now()

	// An IN row means "the turn we asked for finished". A session sitting at
	// awaiting_input because it just booted, or settling a second time after
	// a turn we already logged, has not completed anything the pilot asked
	// for — counting those inflates `done` past the instructions that caused
	// them and makes the progress meter lie.
	outstanding := cp.sent > cp.observed
	// Errors and permission blocks are always worth surfacing: they are
	// exactly the states where the pilot needs to see the session disagree.
	logIt := (state == "awaiting_input" && outstanding) ||
		state == "error" || state == "awaiting_permission"

	if logIt && outstanding {
		cp.observed++
		// Close the newest open step — the one this turn answers.
		for i := len(cp.steplog) - 1; i >= 0; i-- {
			if cp.steplog[i].done() {
				break
			}
			cp.steplog[i].state = state
			cp.steplog[i].reply = response
			cp.steplog[i].tool = tool
			if !cp.steplog[i].at.IsZero() {
				cp.steplog[i].dur = time.Since(cp.steplog[i].at)
			}
			break
		}
	} else if state == "working" && len(cp.steplog) > 0 {
		// Keep the in-flight row's tool current so the ledger shows what the
		// session is doing right now, not only what it ended on.
		if last := &cp.steplog[len(cp.steplog)-1]; !last.done() && tool != "" {
			last.tool = tool
		}
	}
	same := prevState == state
	cp.mu.Unlock()

	if logIt || !same {
		cp.markDirty()
	}
}

// recordFinish notes the pilot declaring the task complete.
func (cp *ControlPanel) recordFinish(summary string, failed bool) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.finished = true
	cp.summary = summary
	cp.state = "finished"
	if failed {
		cp.state = "failed"
	}
	cp.mu.Unlock()
	label := "finished"
	if failed {
		label = "failed"
	}
	cp.setNote(label, failed)
}

// ── Rendering ─────────────────────────────────────────────────────────────────

// stateColor maps an observed session state to its fixed palette entry.
func stateColor(state string) rgb {
	switch state {
	case "awaiting_input", "finished":
		return colSuccess
	case "working":
		return colWarn
	case "error", "failed", "awaiting_permission":
		return colError
	case "starting":
		return colRunning
	default:
		return colSubtle
	}
}

// stateBadge shortens a state to a chip label that fits a narrow pane.
func stateBadge(state string) string {
	switch state {
	case "awaiting_input":
		return "AWAITING"
	case "awaiting_permission":
		return "PERMISSION"
	case "working":
		return "WORKING"
	case "starting":
		return "STARTING"
	case "finished":
		return "DONE"
	case "failed":
		return "FAILED"
	case "error":
		return "ERROR"
	case "gone":
		return "GONE"
	default:
		return "IDLE"
	}
}

// render paints the whole panel into its pane. Called from the render loop.
func (cp *ControlPanel) render() {
	if !cp.enabled() {
		return
	}
	p := cp.pane
	p.mu.Lock()
	w, h := p.w, p.h
	p.mu.Unlock()
	if w < 8 || h < 3 {
		return
	}

	cp.mu.Lock()
	// Repaint on change; otherwise only tick while work is in flight, so an
	// idle panel is as cheap as any other idle pane.
	live := cp.state == "working" || cp.state == "starting"
	if !cp.needsPaint && (!live || time.Since(cp.lastPaint) < time.Second) {
		cp.mu.Unlock()
		return
	}
	cp.needsPaint = false
	cp.lastPaint = time.Now()
	snap := ctrlFrameState{
		goal:      cp.goal,
		target:    cp.target,
		model:     cp.model,
		steps:     cp.steps,
		sent:      cp.sent,
		observed:  cp.observed,
		startedAt: cp.startedAt,
		state:     cp.state,
		note:      cp.note,
		noteBad:   cp.noteBad,
		scroll:    cp.scroll,
		finished:  cp.finished,
		summary:   cp.summary,
		closeIn:   cp.closeIn,
	}
	steps := make([]ctrlStep, len(cp.steplog))
	copy(steps, cp.steplog)
	cp.mu.Unlock()

	lines := cp.frame(snap, steps, w, h)

	// Repaint the whole pane: home the cursor, clear, then write the frame.
	// Cheap — this only runs when something actually changed.
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	for i, ln := range lines {
		if i >= h {
			break
		}
		b.WriteString(fmt.Sprintf("\x1b[%d;1H", i+1))
		b.WriteString(ln)
		b.WriteString(sgrReset)
	}
	data := []byte(b.String())

	p.mu.Lock()
	p.vt.write(data)
	p.dirty = true // vt.write alone doesn't; the read loop normally does this
	p.mu.Unlock()
}

// ctrlFrameState is the lock-free value copy of the panel fields a frame
// needs. Copying ControlPanel itself would copy its mutex.
type ctrlFrameState struct {
	goal      string
	target    int
	model     string
	steps     int
	sent      int
	observed  int
	startedAt time.Time
	state     string
	note      string
	noteBad   bool
	scroll    int
	finished  bool
	summary   string
	closeIn   time.Duration // >0 when an auto-close countdown is running
}

// frame lays out the control plane as an instrument panel:
//
//	link      who is driving what, and the live state
//	ledger    one fixed row per step: duration bar, tool, outcome
//	detail    the newest instruction and reply, one line each
//
// The deliberate choice is that the ledger is a TABLE, not a transcript. The
// pilot's own pane is already a scrolling log of the same run, and two logs
// side by side are indistinguishable at a glance — which defeats the point of
// having a control plane at all. Aligned columns and duration bars give the
// eye something it cannot get from the pilot pane: comparison across steps.
func (cp *ControlPanel) frame(s ctrlFrameState, steps []ctrlStep, w, h int) []string {
	var out []string
	inner := w - 2 // one column of padding either side
	if inner < 6 {
		inner = w
	}
	pad := " "
	rule := func(c rgb) string { return pad + paint(c, strings.Repeat("─", maxInt(inner, 1))) }

	since := time.Since(s.startedAt)
	if since < 0 {
		since = 0 // a clock jump must not garble the header
	}

	// ── link ──────────────────────────────────────────────────────────────
	// PILOT ══▶ SESSION reads as a control plane at a glance; a timestamped
	// list reads as a log. The arrow is the identity of this pane.
	title := paint(colAccent, sgrBold+"CONTROL PLANE")
	out = append(out, pad+padBetween(title, paint(colSubtle, formatDuration(since)), inner))
	out = append(out, rule(colBorder))

	linkColor := stateColor(s.state)
	link := paint(colAccent, sgrBold+"PILOT") +
		paint(linkColor, " ══▶ ") +
		paint(colAccent, sgrBold+"SESSION")
	out = append(out, pad+padBetween(link, badge(stateBadge(s.state), linkColor), inner))

	who := paint(colSubtle, shortModel(s.model))
	where := paint(colSubtle, "pane ")
	if s.target >= 0 {
		where += paint(colText, fmt.Sprint(s.target))
	} else {
		where += paint(colDead, "—")
	}
	out = append(out, pad+padBetween(who, where, inner))

	// Counters as discrete figures, not prose.
	inFlight := s.sent - s.observed
	counts := paint(colRunning, fmt.Sprint(s.sent)) + paint(colSubtle, " sent") +
		paint(colBorder, "  │  ") +
		paint(colSuccess, fmt.Sprint(s.observed)) + paint(colSubtle, " done")
	if inFlight > 0 {
		counts += paint(colBorder, "  │  ") +
			paint(colWarn, fmt.Sprint(inFlight)) + paint(colSubtle, " in flight")
	}
	var right string
	if s.steps > 0 {
		frac := float64(s.observed) / float64(s.steps)
		right = meter(frac, minInt(maxInt(inner/3, 6), 18), colRunning, colSuccess)
	}
	out = append(out, pad+padBetween(counts, right, inner))

	if s.goal != "" && inner > 20 {
		out = append(out, pad+paint(colSubtle, "goal ")+
			paint(colText, oneLine(s.goal, inner-6)))
	}

	// ── ledger ────────────────────────────────────────────────────────────
	out = append(out, rule(colBorder))
	avail := h - len(out)
	if avail < 1 {
		return out
	}

	if len(steps) == 0 {
		out = append(out, pad+paint(colDead, "no instructions yet"))
		return out
	}

	// Split the remaining space. The ledger is the summary; the detail block
	// below it is the actual exchange, and that is what the empty half of the
	// pane should be filled with. Capping the ledger at half keeps a long run
	// from squeezing the exchange down to nothing, and vice versa.
	ledgerRows := minInt(len(steps), maxInt(3, avail/2))

	// Bars are scaled against the slowest turn so the column is a comparison,
	// not an absolute timeline.
	var maxDur time.Duration
	for _, st := range steps {
		if st.dur > maxDur {
			maxDur = st.dur
		}
	}
	visible := steps
	if len(visible) > ledgerRows {
		visible = visible[len(visible)-ledgerRows:]
	}
	for _, st := range visible {
		out = append(out, pad+cp.stepRow(st, maxDur, inner))
	}

	// ── exchange: every instruction and reply, scrollable ─────────────────
	//
	// The full conversation lives here, not just the newest pair. It is the
	// record of what was actually asked and answered, so it is rendered in
	// full and the view scrolls rather than the text being truncated.
	// The finish footer is built first and its height reserved, so the
	// closing statement can never be pushed off the bottom by a long reply.
	var footer []string
	if s.finished {
		footer = finishFooter(s, pad, inner)
	}
	noteRows := 0
	if s.note != "" && !s.finished {
		noteRows = 1 // the footer supersedes the note once the run has ended
	}
	convo := cp.exchangeLines(steps, pad, inner)
	view := h - len(out) - 1 - noteRows - len(footer) // -1 for the rule
	if view < 1 {
		return out
	}

	// Clamp the scroll offset now that the real line count is known, and
	// write it back so a scroll past the top does not leave the view stuck.
	maxScroll := maxInt(0, len(convo)-view)
	scroll := minInt(s.scroll, maxScroll)
	if scroll != s.scroll {
		cp.clampScroll(scroll)
	}

	hidden := maxInt(0, len(convo)-view-scroll)
	head := paint(colBorder, strings.Repeat("─", maxInt(inner-24, 1)))
	switch {
	case scroll > 0:
		head += paint(colWarn, fmt.Sprintf("  ▲ %d back · End to follow", scroll))
	case hidden > 0:
		head += paint(colSubtle, fmt.Sprintf("  ▲ %d earlier", hidden))
	default:
		head += paint(colBorder, strings.Repeat("─", 24))
	}
	out = append(out, pad+truncANSI(head, inner))

	end := len(convo) - scroll
	start := maxInt(0, end-view)
	out = append(out, convo[start:end]...)

	if s.note != "" && !s.finished {
		c := colSubtle
		if s.noteBad {
			c = colError
		}
		out = append(out, pad+paint(c, "• "+s.note))
	}
	out = append(out, footer...)
	return out
}

// finishFooter is the run's closing statement: what happened, and what will
// happen to this window. A finished run must never look like a stalled one,
// and it must never vanish before it has been read — so the panel either
// states the key that closes it, or counts down visibly.
func finishFooter(s ctrlFrameState, pad string, inner int) []string {
	c, label := colSuccess, "FINISHED"
	if s.noteBad || s.state == "failed" {
		c, label = colError, "FAILED"
	}
	line := badge(label, c)
	if s.closeIn > 0 {
		secs := int(s.closeIn.Seconds() + 0.5)
		line += " " + paint(colWarn, fmt.Sprintf("closing in %ds", secs)) +
			paint(colSubtle, " · any key cancels")
	} else {
		line += " " + paint(colSubtle, "press ") + paint(colText, "q") +
			paint(colSubtle, " to close")
	}
	out := []string{pad + padBetween(line, "", inner)}
	if s.summary != "" {
		for _, ln := range wrapText(s.summary, inner-2, 3) {
			out = append(out, pad+paint(colSubtle, "  "+ln))
		}
	}
	return out
}

// clampScroll pins the offset to a bound the renderer just computed.
func (cp *ControlPanel) clampScroll(v int) {
	cp.mu.Lock()
	cp.scroll = v
	cp.mu.Unlock()
}

// exchangeLines renders the whole conversation, oldest first: every
// instruction the pilot sent and every reply magmux observed, wrapped to the
// pane. Nothing is truncated — the view scrolls instead.
func (cp *ControlPanel) exchangeLines(steps []ctrlStep, pad string, inner int) []string {
	var out []string
	for i, st := range steps {
		if i > 0 {
			out = append(out, "")
		}
		instr := wrapText(st.text, inner-2, ctrlMaxScroll)
		out = append(out, block("▶", colRunning, colText, instr, pad, inner)...)

		replyColor := colSuccess
		reply := st.reply
		if reply == "" {
			if st.done() {
				reply = "(no text reported)"
			} else {
				reply = "working…"
			}
			replyColor = colDead
		}
		if st.bad() {
			replyColor = colError
		}
		body := wrapText(reply, inner-2, ctrlMaxScroll)
		out = append(out, block("◀", colSuccess, replyColor, body, pad, inner)...)
	}
	return out
}

// block renders a marked, wrapped paragraph: the glyph leads the first line
// and continuations are indented to align under the text.
func block(glyph string, glyphColor, textColor rgb, lines []string, pad string, inner int) []string {
	out := make([]string, 0, len(lines))
	for i, ln := range lines {
		prefix := paint(glyphColor, glyph+" ")
		if i > 0 {
			prefix = "  "
		}
		out = append(out, pad+truncANSI(prefix+paint(textColor, ln), inner))
	}
	return out
}

// stepRow renders one ledger line:
//
//	3 ✓ verify 1b      ███████░░░░  9.0s  Bash
//
// Fixed columns on purpose — the value of the ledger is that row N is
// comparable to row N+1 without reading either of them.
func (cp *ControlPanel) stepRow(st ctrlStep, maxDur time.Duration, inner int) string {
	glyph, c := "◐", colWarn // in flight
	switch {
	case st.state == "awaiting_input":
		glyph, c = "✓", colSuccess
	case st.state == "error":
		glyph, c = "✗", colError
	case st.state == "awaiting_permission":
		glyph, c = "⚠", colError
	case st.state == "gone":
		glyph, c = "✗", colDead
	}

	num := fmt.Sprintf("%2d ", st.n)
	head := paint(colSubtle, num) + paint(c, glyph+" ")

	// Label column, fixed width so the bars line up.
	labelW := 12
	if inner < 40 {
		labelW = 8
	}
	head += paint(colText, padRight(oneLine(st.label, labelW), labelW)) + " "

	used := visWidth(head)
	// Duration text and tool sit right of the bar; give the bar what is left.
	durTxt := "  —  "
	if st.dur > 0 {
		durTxt = formatDuration(st.dur)
	} else if !st.at.IsZero() {
		durTxt = formatDuration(time.Since(st.at))
	}
	tool := st.tool
	if tool == "" {
		tool = "—"
	}
	tail := " " + paint(colSubtle, padLeft(durTxt, 6)) + " " + paint(colDebug, oneLine(tool, 8))

	barW := inner - used - visWidth(tail)
	if barW < 3 {
		return truncANSI(head+tail, inner)
	}
	var frac float64
	switch {
	case maxDur > 0 && st.dur > 0:
		frac = float64(st.dur) / float64(maxDur)
	case !st.done():
		frac = 0.15 // in flight: a stub, not a claim about length
	}
	return head + meter(frac, barW, colRunning, c) + tail
}

// shortModel trims a provider-qualified model id to something that fits a
// header without dominating it.
func shortModel(m string) string {
	if m == "" {
		return "no pilot attached"
	}
	if i := strings.LastIndexByte(m, '/'); i >= 0 && i+1 < len(m) {
		return m[i+1:]
	}
	return m
}

// oneLine collapses whitespace and truncates to a single row. The ledger is a
// table; nothing in it may wrap, or the columns stop lining up.
func oneLine(s string, w int) string {
	flat := strings.Join(strings.Fields(s), " ")
	if w < 2 {
		return ""
	}
	if len([]rune(flat)) <= w {
		return flat
	}
	r := []rune(flat)
	return string(r[:w-1]) + "…"
}

func padRight(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func padLeft(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── text helpers ──────────────────────────────────────────────────────────────

// padBetween puts `left` and `right` at opposite ends of a width-`w` field,
// measuring visible width only (escape sequences take no columns).
func padBetween(left, right string, w int) string {
	gap := w - visWidth(left) - visWidth(right)
	if gap < 1 {
		return truncANSI(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

// visWidth is the rendered column count of a string containing SGR escapes.
func visWidth(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		n += runeWidth(r)
	}
	return n
}

// truncANSI cuts a string to `w` visible columns, keeping escape sequences
// intact so colour never bleeds past the cut.
func truncANSI(s string, w int) string {
	if visWidth(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			b.WriteRune(r)
			inEsc = true
			continue
		}
		cw := runeWidth(r)
		if n+cw > w-1 {
			b.WriteString(paint(colSubtle, "…"))
			break
		}
		b.WriteRune(r)
		n += cw
	}
	b.WriteString(sgrReset)
	return b.String()
}

// wrapText greedily wraps plain (unescaped) text to `w` columns, stopping
// after maxLines and marking the cut with an ellipsis.
//
// The caller passes the budget rather than the function assuming one: how much
// of a reply is worth showing depends entirely on how much pane is left below
// the ledger, which only the layout knows.
func wrapText(s string, w, maxLines int) []string {
	if w < 4 {
		w = 4
	}
	if maxLines < 1 {
		maxLines = 1
	}
	// Collapse whitespace first: a reply full of blank lines and markdown
	// tables would otherwise spend the whole budget on empty rows.
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := ""
	for _, word := range words {
		switch {
		case cur == "":
			cur = word
		case len(cur)+1+len(word) <= w:
			cur += " " + word
		default:
			lines = append(lines, cur)
			cur = word
		}
		// A single word longer than the line gets hard-split.
		for len(cur) > w {
			lines = append(lines, cur[:w])
			cur = cur[w:]
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = oneLine(lines[maxLines-1], w-1) + "…"
	}
	return lines
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
