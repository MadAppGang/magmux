package main

// Control panel — a wire tap on ONE exchange.
//
// The exchange is between **the controller** — the agent driving the work —
// and **the controlled agents**, the panes it drives. In `pilot:demo` the
// controller is the pi agent in pane 1, the controlled agent is Claude Code in
// pane 0, and this panel in pane 2 shows what passed between them. The panel
// is an instrument: it controls nothing, and it is deliberately read-only, so
// "who closed pane 2" is never a question it has to answer about itself.
//
// An MCP client is just another controller. It reaches magmux through
// `magmux mcp` translating tools/call into socket verbs instead of writing
// socket verbs directly, and that extra process hop is plumbing, not a new
// participant — every controller action still arrives here as a socket verb.
// There is therefore no third class of row and no self-reporting.
//
// It renders into a Pane that has no PTY. magmux paints it by writing raw
// ANSI through the pane's own VT parser, exactly as a child process would,
// so the panel inherits borders, scrollback, selection and dirty-flag
// rendering from the normal pane path and needs no renderer of its own.
//
// The two directions come from different places on purpose:
//
//	OUT  the controller's request, as it arrived on the socket (recordSend etc)
//	IN   magmux observed the pane's own turn finishing (pollControllers)
//
// The panel therefore cannot show a step as completed just because the
// controller believes it was — an IN row exists only if a controller actually
// saw the session come back to awaiting_input. The grep-able form of that
// invariant: `observed` and `ctrlStep.state` are written in exactly one
// function, recordObserved. A reply/ack never closes a turn.

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Palette ───────────────────────────────────────────────────────────────────
//
// Semantic, not decorative: one colour means one thing across the whole panel.
// Chrome (borders, timestamps, labels) recedes; data and state badges are
// saturated and carry the eye. The values themselves live in theme.go, because
// there are two sets of them and which one is in force is a startup decision —
// see `pal`.

const sgrReset = "\x1b[0m"
const sgrBold = "\x1b[1m"

// sgrBase is the panel's ground state: no attributes, body foreground, and the
// TERMINAL's own background. Everything that would otherwise emit a bare reset
// emits this instead, so a segment that finished with a colour cannot leak it
// into the rest of the row.
//
// It deliberately sets no background. The panel is a pane inside the user's
// terminal and the terminal's background is the user's to choose; painting one
// of ours put a grey slab inside a cream window, which is a different bug from
// the one being fixed. The reported bug was the FOREGROUND — Mocha's #CDD6F4
// body text on a light terminal — and that is what the two palettes cure.
//
// The one exception is badge(), which fills a small chip and writes pal.ink on
// top of it: it controls both halves, so it is legible whatever the terminal is.
func sgrBase() string { return sgrReset + fg(pal.text) }

// paint wraps s in a foreground colour and returns to the panel ground state.
func paint(c rgb, s string) string { return fg(c) + s + sgrBase() }

// badge renders a status chip: ink on a saturated background. Reads as a
// discrete state at a glance, which plain coloured text does not.
func badge(label string, c rgb) string {
	return bg(c) + fg(pal.ink) + sgrBold + " " + label + " " + sgrBase()
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
			b.WriteString(fg(pal.border) + "░")
		}
	}
	b.WriteString(sgrBase())
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
	b.WriteString(sgrBase())
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

// ctrlRoute is what the panel knows about one pane the controller drives.
// Created the first time the controller touches a pane; never deleted — a
// closed pane's history is exactly what you want after an agent closes
// something that failed.
type ctrlRoute struct {
	pane     int
	sent     int
	observed int
	steps    int // planned step count for this route, 0 if open-ended

	title string // short name for the table, from open_pane's label or cmd
	goal  string
	state string // last state magmux observed on this pane
	tool  string // last tool the session used

	stateAt  time.Time
	lastSent time.Time
	openedAt time.Time
	closedAt time.Time // non-zero once the controller closed this pane

	steplog []ctrlStep // this route's ledger, oldest first
	durs    []float64  // completed turn durations, for the row's spark
}

// closed reports whether the pane behind this route is gone. The route stays.
func (r ctrlRoute) closed() bool { return !r.closedAt.IsZero() }

// ctrlSignal is one entry in the interleaved stream. dir carries the same two
// directions the panel has always had; there is no third.
//
// The stream is interleaved rather than split into per-pane lanes because
// causality across panes IS the interleaving — "the agent read pane 0, then
// opened pane 2" is a fact about the run that three parallel columns destroy,
// and three columns in a 60-wide pane are unreadable anyway. The 1-9 filter
// gives a single lane on demand.
type ctrlSignal struct {
	seq  int
	at   time.Time
	pane int    // -1 = run-level (start/finish/note, controller connect/disconnect)
	dir  string // "out" | "in" | "note"
	verb string // "send" | "open_pane" | "capture" | "list" | observed state

	text  string
	tool  string
	state string
	dur   time.Duration

	// turn distinguishes an IN row that CLOSED a requested turn from one that
	// merely reports the session changing state mid-turn. Both are magmux's own
	// observation; only the first is a completion, and the panel must not let
	// them look alike.
	turn bool

	// ok/code/ackText are magmux's ack of this request, rendered as an indented
	// continuation of the OUT row rather than as an entry of its own: a reply
	// answers the request it belongs to, so it is the same exchange. nil ok
	// means the request has not been answered yet. Recording an ack never
	// touches `observed` or `ctrlStep.state`.
	ok      *bool
	code    string
	ackText string
}

// ControlPanel holds the routes and the live status of a controlled run. Safe
// for concurrent use: the socket goroutine appends OUT rows while the render
// loop appends IN rows and reads for painting.
//
// ONE mutex, deliberately. A table where route 0's counters are from one
// instant and route 1's from another is worse than a coarser lock.
type ControlPanel struct {
	mu sync.Mutex

	// pane is the virtual pane we paint into; nil if not enabled.
	//
	// It is PANEL state, not layout state, so it lives under cp.mu like every
	// other field here. It used to be written under treeMu (by ClosePane) and
	// read with no lock at all, which was sound only for as long as the single
	// reader happened to be render(), which happened to hold treeMu.RLock —
	// three coincidences, none of them stated anywhere, and any future caller of
	// enabled() from a socket goroutine broke all three at once. Reach it
	// through attach / detach / paneRef and the question does not arise.
	pane *Pane

	goal      string
	client    string // controller identity for the header ("claude-code/2.1")
	model     string
	steps     int // total steps the controller planned, 0 if open-ended
	sent      int // run totals; per-route counters live on the route
	observed  int
	startedAt time.Time
	finished  bool
	summary   string

	state    string    // newest observed state across every route
	stateAt  time.Time // when that state was observed
	lastSent time.Time // when the last instruction went out

	routes     map[int]*ctrlRoute
	routeOrder []int // pane ids, ascending — the table is for comparison
	signals    []ctrlSignal
	seq        int

	// filter is the pane index the panel is showing alone, or -1 for every
	// route. It is a PANE INDEX and not an ordinal into routeOrder: a second
	// numbering beside the pane ids would be a bug factory.
	filter int

	// focused is magmux's own focused pane, so the route table can mark which
	// row the user is looking at.
	focused int

	note    string // latest panel-level annotation (attach / finish / fail)
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

const (
	ctrlMaxSteps   = 64  // ledger rows per route
	ctrlMaxSignals = 400 // entries in the interleaved stream
	ctrlMaxRoutes  = 32  // panes one controller may be tracked against
)

// ctrlMaxScroll bounds the scroll offset so a held PageUp cannot run away
// while the real limit (the wrapped line count) is only known at paint time.
const ctrlMaxScroll = 100000

func newControlPanel() *ControlPanel {
	return &ControlPanel{
		routes:    map[int]*ctrlRoute{},
		filter:    -1,
		focused:   -1,
		startedAt: time.Now(),
		state:     "idle",
	}
}

// attach binds the panel to the pane it paints into.
func (cp *ControlPanel) attach(p *Pane) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.pane = p
	cp.mu.Unlock()
}

// detach unbinds the panel if — and only if — p is the pane it is bound to.
// Called by ClosePane, where the pane the panel points at may be some other
// pane entirely, and unbinding then would blank a panel that is still on screen.
func (cp *ControlPanel) detach(p *Pane) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	if cp.pane == p {
		cp.pane = nil
	}
	cp.mu.Unlock()
}

// paneRef is the panel's pane, or nil when it has none.
func (cp *ControlPanel) paneRef() *Pane {
	if cp == nil {
		return nil
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.pane
}

func (cp *ControlPanel) enabled() bool { return cp.paneRef() != nil }

// routeLocked returns the route for a pane, creating it on first touch.
//
// Returns nil once ctrlMaxRoutes is reached. The signal still reaches the
// stream carrying its pane tag; what it loses is a table row, which is the
// right thing to lose, because an unbounded table pushes everything else off
// the panel. Caller holds cp.mu.
func (cp *ControlPanel) routeLocked(pane int) *ctrlRoute {
	if pane < 0 {
		return nil
	}
	if r, ok := cp.routes[pane]; ok {
		return r
	}
	if len(cp.routes) >= ctrlMaxRoutes {
		return nil
	}
	r := &ctrlRoute{pane: pane, state: "idle", openedAt: time.Now(), steps: cp.steps}
	cp.routes[pane] = r
	cp.routeOrder = append(cp.routeOrder, pane)
	sort.Ints(cp.routeOrder)
	return r
}

// targetPane resolves a `send` that named no pane.
//
// One route means the single-session case every pilot is, so it keeps working
// untouched. Several open routes means guessing, and a guess here types the
// next instruction into the wrong Claude Code session — expensive, and
// invisible until someone reads the transcript. So it refuses, and says so in
// the stream: a controller that has forgotten to name its pane should find out
// from the panel, not from the damage.
func (cp *ControlPanel) targetPane() (int, error) {
	if cp == nil {
		return 0, nil
	}
	cp.mu.Lock()
	switch len(cp.routeOrder) {
	case 0:
		cp.mu.Unlock()
		return 0, nil
	case 1:
		pane := cp.routeOrder[0]
		cp.mu.Unlock()
		return pane, nil
	}
	open := fmt.Sprint(cp.routeOrder)
	cp.appendSignalLocked(ctrlSignal{
		pane: -1, dir: "note", verb: "send",
		text: "refused: no pane given and " + open + " are open",
	})
	cp.needsPaint = true
	cp.mu.Unlock()
	return -1, sockErrf(sockCodeBadRequest,
		"send needs a pane: this controller is driving %s", open)
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

// appendSignalLocked stamps and files one entry in the interleaved stream.
// Caller holds cp.mu; returns the seq an ack can be attached to.
func (cp *ControlPanel) appendSignalLocked(sig ctrlSignal) int {
	cp.seq++
	sig.seq = cp.seq
	if sig.at.IsZero() {
		sig.at = time.Now()
	}
	cp.signals = append(cp.signals, sig)
	if len(cp.signals) > ctrlMaxSignals {
		cp.signals = cp.signals[len(cp.signals)-ctrlMaxSignals:]
	}
	return sig.seq
}

// recordStart notes the controller announcing itself and the task it will
// drive. client is its identity for the header ("claude-code/2.1") and is the
// one field MCP adds — everything else is the same `pilot` event the pi pilot
// has always sent, which is what keeps the driver-agnostic promise.
//
// pane is paneUnspecified when the event named none. That is an IDENTITY
// announcement, not a run start: it writes the header (client, goal, model,
// steps) and nothing else. It opens no route, because a route means "a pane
// this controller has touched" and announcing yourself touches none; and it
// wipes no counters, because a controller that names itself mid-run has not
// started a second run. See recordStartIdentityLocked.
func (cp *ControlPanel) recordStart(pane int, goal, model, client string, steps int) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	if pane < 0 {
		cp.recordStartIdentityLocked(goal, model, client, steps)
		cp.mu.Unlock()
		cp.setNote(attachNote(client), false)
		return
	}
	cp.goal = goal
	cp.model = model
	cp.client = client
	cp.steps = steps
	cp.startedAt = time.Now()
	cp.finished = false
	cp.summary = ""
	// A new run starts from nothing, exactly as the old single-lane panel did.
	// This zeroing is the only write to `observed` outside recordObserved, and
	// it claims no turn: it discards the previous run's, it does not credit
	// this one with any.
	cp.routes = map[int]*ctrlRoute{}
	cp.routeOrder = nil
	cp.signals = nil
	cp.sent, cp.observed = 0, 0
	cp.filter = -1
	if r := cp.routeLocked(pane); r != nil {
		r.goal = goal
	}
	cp.mu.Unlock()
	cp.setNote(attachNote(client), false)
}

// attachNote names whoever just announced itself.
func attachNote(client string) string {
	if client != "" {
		return client + " attached"
	}
	return "pilot attached"
}

// recordStartIdentityLocked applies a `start` that named no pane: header state
// only. Deliberately NOT reset — routes, routeOrder, signals, sent, observed,
// filter, finished, summary. recordStart's zeroing means "a new run begins",
// and an identity announcement is not that; MCP fires one per attached session,
// which can land while a run is already in flight, and wiping the ledger there
// would delete the very traffic the panel exists to show. Caller holds cp.mu.
func (cp *ControlPanel) recordStartIdentityLocked(goal, model, client string, steps int) {
	cp.client = client
	// Only non-empty values are written. An identity event carries just
	// `client`, so assigning unconditionally would blank a goal or model an
	// earlier run start had put in the header.
	if goal != "" {
		cp.goal = goal
	}
	if model != "" {
		cp.model = model
	}
	if steps > 0 {
		cp.steps = steps
	}
	// The one clock it may set: a controller that announces itself before
	// touching anything starts the run there rather than at magmux's own
	// startup. It can never RESTART a clock — a run with a route or a signal
	// already in it is in progress, and its elapsed time is real.
	if len(cp.routes) == 0 && len(cp.signals) == 0 {
		cp.startedAt = time.Now()
	}
}

// recordRouteOpened names a pane the controller just created, so the table has
// something better than a bare index to compare rows by.
func (cp *ControlPanel) recordRouteOpened(pane int, title string) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	if r := cp.routeLocked(pane); r != nil {
		r.openedAt = time.Now()
		if title != "" {
			r.title = title
		}
	}
	cp.mu.Unlock()
	cp.markDirty()
}

// recordRouteClosed marks a route's pane gone. The route itself survives: the
// history of something an agent closed because it failed is exactly what you
// want to still be able to read.
func (cp *ControlPanel) recordRouteClosed(pane int) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	if r := cp.routeLocked(pane); r != nil {
		r.closedAt = time.Now()
		r.state = "gone"
		r.stateAt = r.closedAt
	}
	cp.mu.Unlock()
	cp.markDirty()
}

// recordRequest logs a controller request that is not an instruction —
// open_pane, close_pane, capture, list. pane is -1 for a run-level request.
// Returns the seq recordAck attaches magmux's reply to.
func (cp *ControlPanel) recordRequest(pane int, verb, text string) int {
	if cp == nil {
		return 0
	}
	cp.mu.Lock()
	cp.routeLocked(pane) // touching a pane is what opens its route
	seq := cp.appendSignalLocked(ctrlSignal{pane: pane, dir: "out", verb: verb, text: text})
	cp.mu.Unlock()
	cp.markDirty()
	return seq
}

// recordAck attaches magmux's reply to the OUT row it answers.
//
// It writes nothing but ok/code/ackText on that one signal — the request's own
// text is left alone, because the row still has to say what was asked for. An
// ack is magmux saying "I accepted the request", which is a fact about the
// request and not about the session, so it must never touch `observed` or a
// step's state: a controller that could close its own turns by being replied
// to is a controller that can fabricate a completion.
func (cp *ControlPanel) recordAck(seq int, ok bool, code, text string) {
	if cp == nil || seq == 0 {
		return
	}
	cp.mu.Lock()
	for i := len(cp.signals) - 1; i >= 0; i-- {
		if cp.signals[i].seq != seq {
			continue
		}
		cp.signals[i].ok = &ok
		cp.signals[i].code = code
		cp.signals[i].ackText = text
		break
	}
	cp.mu.Unlock()
	cp.markDirty()
}

// noteController records the controller arriving or going away. It needs no
// protocol: magmux sees the fd close itself. An operator staring at a frozen
// panel needs to know the controller went away rather than got slow.
func (cp *ControlPanel) noteController(here bool) {
	if cp == nil {
		return
	}
	text := "controller disconnected"
	if here {
		text = "controller connected"
	}
	cp.mu.Lock()
	cp.appendSignalLocked(ctrlSignal{pane: -1, dir: "note", verb: "controller", text: text})
	cp.mu.Unlock()
	cp.markDirty()
}

// recordSend logs an instruction the controller pushed into a session, and
// returns the seq of its OUT row so magmux's ack can be hung off it.
func (cp *ControlPanel) recordSend(pane int, label, text string) int {
	if cp == nil {
		return 0
	}
	cp.mu.Lock()
	now := time.Now()
	cp.sent++
	cp.lastSent = now
	cp.state = "working"
	cp.stateAt = now
	if r := cp.routeLocked(pane); r != nil {
		r.sent++
		r.lastSent = now
		r.state = "working"
		r.stateAt = now
		step := label
		if step == "" {
			if r.steps > 0 {
				step = fmt.Sprintf("step %d/%d", r.sent, r.steps)
			} else {
				step = fmt.Sprintf("step %d", r.sent)
			}
		}
		r.steplog = append(r.steplog, ctrlStep{n: r.sent, label: step, text: text, at: now})
		if len(r.steplog) > ctrlMaxSteps {
			r.steplog = r.steplog[len(r.steplog)-ctrlMaxSteps:]
		}
	}
	// The verb column shows the MCP tool name when the controller supplied one
	// via `label`, and the socket verb otherwise. That is the whole of MCP's
	// footprint on this panel: no new verb, no self-reporting.
	verb := label
	if verb == "" {
		verb = "send"
	}
	seq := cp.appendSignalLocked(ctrlSignal{
		pane: pane, dir: "out", verb: verb, text: text, at: now,
	})
	cp.mu.Unlock()
	cp.markDirty()
	return seq
}

// recordObserved logs a state magmux itself saw on a controlled pane.
//
// This is the ONLY writer of `observed` and of `ctrlStep.state`. It is
// deliberately not driven by the controller: the panel must be able to show a
// session disagreeing with what the controller thinks it asked for, and a
// reply must never be able to close a turn.
func (cp *ControlPanel) recordObserved(pane int, state, response, tool string) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	// A pane with NO route is not this controller's business at all. Four
	// Claude panes where the agent drives two must not have the other two
	// flood the stream, and the route is created by the controller touching
	// the pane, so "no route" is exactly "never touched".
	r := cp.routes[pane]
	if r == nil {
		cp.mu.Unlock()
		return
	}
	prevState := r.state
	r.state = state
	r.stateAt = time.Now()
	cp.state = state
	cp.stateAt = r.stateAt
	if tool != "" {
		r.tool = tool
	}

	// An IN row means "the turn we asked for finished". A session sitting at
	// awaiting_input because it just booted, or settling a second time after
	// a turn we already logged, has not completed anything the controller
	// asked for — counting those inflates `done` past the instructions that
	// caused them and makes the progress meter lie.
	//
	// The counters are THIS ROUTE's, which is strictly stronger than a global
	// pair: a global `sent > observed` would let route 0's boot-time idle
	// close route 1's outstanding step, which is the "done 1 against zero
	// instructions" bug resurrected at N panes.
	outstanding := r.sent > r.observed
	// Errors and permission blocks are always worth surfacing: they are
	// exactly the states where the controller needs to see the session
	// disagree.
	logIt := (state == "awaiting_input" && outstanding) ||
		state == "error" || state == "awaiting_permission"

	var turnDur time.Duration
	if logIt && outstanding {
		r.observed++
		cp.observed++
		// Close the newest open step — the one this turn answers.
		for i := len(r.steplog) - 1; i >= 0; i-- {
			if r.steplog[i].done() {
				break
			}
			r.steplog[i].state = state
			r.steplog[i].reply = response
			r.steplog[i].tool = tool
			if !r.steplog[i].at.IsZero() {
				r.steplog[i].dur = time.Since(r.steplog[i].at)
				turnDur = r.steplog[i].dur
				r.durs = append(r.durs, turnDur.Seconds())
				if len(r.durs) > ctrlMaxSteps {
					r.durs = r.durs[len(r.durs)-ctrlMaxSteps:]
				}
			}
			break
		}
	} else if state == "working" && len(r.steplog) > 0 {
		// Keep the in-flight row's tool current so the ledger shows what the
		// session is doing right now, not only what it ended on.
		if last := &r.steplog[len(r.steplog)-1]; !last.done() && tool != "" {
			last.tool = tool
		}
	}
	same := prevState == state
	if logIt || !same {
		cp.appendSignalLocked(ctrlSignal{
			pane: pane, dir: "in", verb: state, text: response,
			tool: tool, state: state, dur: turnDur, turn: logIt && outstanding,
			at: r.stateAt,
		})
	}
	cp.mu.Unlock()

	if logIt || !same {
		cp.markDirty()
	}
}

// setFocused tells the panel which pane magmux is focused on, so the route
// table can mark the row the user is looking at.
func (cp *ControlPanel) setFocused(pane int) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	moved := cp.focused != pane
	cp.focused = pane
	cp.needsPaint = cp.needsPaint || moved
	cp.mu.Unlock()
}

// setFilter narrows the panel to one PANE INDEX, or to every route with -1.
// Filtering follows the tail again: a view you just switched into should be
// showing the newest traffic, not wherever the previous view was scrolled to.
func (cp *ControlPanel) setFilter(pane int) bool {
	if cp == nil {
		return false
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if pane >= 0 {
		if _, ok := cp.routes[pane]; !ok {
			return false // nothing is routed there; leave the view alone
		}
	}
	cp.filter = pane
	cp.scroll = 0
	cp.needsPaint = true
	return true
}

// cycleFilter steps through the open routes, wrapping via "all".
func (cp *ControlPanel) cycleFilter(delta int) bool {
	if cp == nil {
		return false
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	n := len(cp.routeOrder)
	if n == 0 {
		return false
	}
	// Positions 0..n-1 are the routes; n is "all". One ring, so ] from the
	// last route lands on the overview rather than wrapping silently.
	at := n
	for i, pane := range cp.routeOrder {
		if pane == cp.filter {
			at = i
			break
		}
	}
	at = ((at+delta)%(n+1) + (n + 1)) % (n + 1)
	if at == n {
		cp.filter = -1
	} else {
		cp.filter = cp.routeOrder[at]
	}
	cp.scroll = 0
	cp.needsPaint = true
	return true
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
		return pal.success
	case "working":
		return pal.warn
	case "error", "failed", "awaiting_permission":
		return pal.fail
	case "starting":
		return pal.running
	default:
		return pal.subtle
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

// stateBadgeShort is the four-column form the route table falls back to in a
// narrow pane. Truncating stateBadge instead gives "AWAI…" / "PERM…", which
// costs the same width and reads as damage rather than as an abbreviation.
func stateBadgeShort(state string) string {
	switch state {
	case "awaiting_input":
		return "IDLE"
	case "awaiting_permission":
		return "PERM"
	case "working":
		return "WORK"
	case "starting":
		return "BOOT"
	case "finished":
		return "DONE"
	case "failed":
		return "FAIL"
	case "error":
		return "ERR"
	case "gone":
		return "GONE"
	default:
		return "—"
	}
}

// liveLocked reports whether anything is still in flight, which is what buys
// the once-a-second repaint that keeps the elapsed counters honest.
//
// It is evaluated PER ROUTE, for the same reason `outstanding` is. cp.state is
// the run-GLOBAL last-observed state, written unconditionally by recordObserved
// for whichever route moved last, so reading liveness off it means that with two
// driven panes the tick stops the moment the first one settles — while the other
// is still mid-turn, and with nothing to restart it, because recordObserved only
// marks the panel dirty on a change and a working pane changing tools is not
// one. routeRow's in-flight ‹elapsed and the header clock then freeze on screen
// for the pane that has not finished, which is exactly the "wedged, or just
// slow?" question the elapsed time exists to answer.
//
// A closed route is skipped: recordRouteClosed leaves its counters alone, so an
// instruction that was outstanding when its pane went away would otherwise tick
// forever. Caller holds cp.mu.
func (cp *ControlPanel) liveLocked() bool {
	for _, r := range cp.routes {
		if r == nil || r.closed() {
			continue
		}
		// `sent > observed` is included because it is the exact condition
		// routeRow paints its growing ‹elapsed under: a number still growing on
		// screen must not be a number that stopped being updated.
		if r.state == "working" || r.state == "starting" || r.sent > r.observed {
			return true
		}
	}
	// No routes at all: fall back to the run-level state, which is all a panel
	// a controller has announced itself to but not yet touched anything with
	// has to go on.
	return len(cp.routes) == 0 && (cp.state == "working" || cp.state == "starting")
}

// render paints the whole panel into its pane. Called from the render loop.
func (cp *ControlPanel) render() {
	p := cp.paneRef()
	if p == nil {
		return
	}
	p.mu.Lock()
	w, h := p.w, p.h
	p.mu.Unlock()
	if w < 8 || h < 3 {
		return
	}

	cp.mu.Lock()
	// Repaint on change; otherwise only tick while work is in flight, so an
	// idle panel is as cheap as any other idle pane.
	live := cp.liveLocked()
	if !cp.needsPaint && (!live || time.Since(cp.lastPaint) < time.Second) {
		cp.mu.Unlock()
		return
	}
	cp.needsPaint = false
	cp.lastPaint = time.Now()
	snap := cp.snapshotLocked()
	steps := snap.legacySteps
	cp.mu.Unlock()

	lines := cp.frame(snap, steps, w, h)
	data := paintFrame(lines, h)

	p.mu.Lock()
	p.vt.write(data)
	p.dirty = true // vt.write alone doesn't; the read loop normally does this
	p.mu.Unlock()
}

// paintFrame turns a frame's lines into the bytes the pane's VT parser sees.
//
// Erasing is ED's job and ED alone: "\x1b[2J" homes the cursor and clears every
// cell of the pane to the terminal's DEFAULT background, because magmux's
// parser has no background-colour-erase (clearLine writes Cell{Bg:
// defaultColor}). That is exactly what is wanted — the untouched parts of the
// panel show the user's terminal through, like any other pane.
//
// An earlier round padded every row to the full width so those cells would
// carry a background of the panel's own. That padding existed only to carry the
// colour: the clear already handles stale content, so with no background to
// impose there is nothing left for it to do, and it is gone.
//
// Each row still opens with sgrBase() so a line cannot inherit the previous
// line's trailing colour. The pane's width is no longer a parameter, because
// nothing here is width-sensitive once the padding is gone — frame() already
// guarantees no line exceeds it.
func paintFrame(lines []string, h int) []byte {
	base := sgrBase()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	for i := 0; i < h; i++ {
		var ln string
		if i < len(lines) {
			ln = lines[i]
		}
		b.WriteString(fmt.Sprintf("\x1b[%d;1H", i+1))
		b.WriteString(base)
		b.WriteString(ln)
	}
	return []byte(b.String())
}

// ctrlFrameState is the lock-free value copy of the panel fields a frame
// needs. Copying ControlPanel itself would copy its mutex.
type ctrlFrameState struct {
	goal      string
	target    int
	model     string
	client    string
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

	routes  []ctrlRoute
	signals []ctrlSignal
	filter  int
	focused int

	// legacySteps is the ledger the single-route fast path renders. Carried
	// here so render() does not have to decide which route it came from twice.
	legacySteps []ctrlStep
}

// snapshotLocked copies everything a frame reads. Caller holds cp.mu.
//
// The copy is deep — steplogs and durs are cloned — because the socket
// goroutine keeps appending to them the moment the lock is released, and a
// frame reading a slice that grows underneath it is a data race with a
// perfectly plausible-looking output.
func (cp *ControlPanel) snapshotLocked() ctrlFrameState {
	s := ctrlFrameState{
		goal:      cp.goal,
		target:    -1,
		model:     cp.model,
		client:    cp.client,
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
		filter:    cp.filter,
		focused:   cp.focused,
	}
	for _, pane := range cp.routeOrder {
		r := cp.routes[pane]
		if r == nil {
			continue
		}
		rc := *r
		rc.steplog = append([]ctrlStep(nil), r.steplog...)
		rc.durs = append([]float64(nil), r.durs...)
		s.routes = append(s.routes, rc)
	}
	s.signals = append([]ctrlSignal(nil), cp.signals...)
	// One route is the shape this panel was built for, so its numbers ARE the
	// run's numbers and the legacy header can read them straight off s.
	if len(s.routes) == 1 {
		applyRouteToState(&s, s.routes[0])
	}
	return s
}

// ── the status-bar digest ────────────────────────────────────────────────────

// ctrlDigest is the one-line version of the panel, for the status bar.
//
// It exists so the panel can be READ while it is hidden — the whole point of
// hiding it is that a session pane gets the columns back, and a run you can no
// longer see anything of is a worse trade than the border it saved. The
// counters keep the panel's provenance rule intact: `sent` is what the
// controller asked for and `observed` is what magmux itself saw the session do,
// and there is no third number invented here to reconcile them.
//
// Same lock-free contract as ctrlFrameState: a value copy, taken under cp.mu
// and read with the lock released, because the status bar is built inside
// renderLocked and holding cp.mu across rendering is how the documented
// treeMu -> cp.mu order turns into a stall.
type ctrlDigest struct {
	active   bool // a controller has touched this panel; nothing to show if not
	sent     int
	observed int

	pane    int // the route the digest is describing, -1 if none
	state   string
	tool    string
	stateAt time.Time

	sigDir  string // "out" | "in" | "note"
	sigVerb string
	sigText string
}

// digest takes cp.mu briefly and returns a value copy. Caller must NOT hold
// cp.mu; it MAY hold treeMu, which is the documented order.
func (cp *ControlPanel) digest() ctrlDigest {
	if cp == nil {
		return ctrlDigest{pane: -1}
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.digestLocked()
}

// digestLocked is the twin for callers already holding cp.mu.
func (cp *ControlPanel) digestLocked() ctrlDigest {
	d := ctrlDigest{
		sent:     cp.sent,
		observed: cp.observed,
		pane:     -1,
		active:   cp.sent > 0 || cp.observed > 0 || len(cp.signals) > 0,
	}
	if n := len(cp.signals); n > 0 {
		s := cp.signals[n-1]
		d.sigDir, d.sigVerb, d.sigText = s.dir, s.verb, s.text
	}
	// Which route to describe: the one the user is looking at if the controller
	// has touched it, otherwise whichever moved most recently. Picking "the
	// first route" instead would pin the digest to pane 0 forever on a run that
	// had long since moved on.
	if r := cp.routes[cp.focused]; r != nil {
		d.pane, d.state, d.tool, d.stateAt = r.pane, r.state, r.tool, r.stateAt
		return d
	}
	var newest *ctrlRoute
	for _, pane := range cp.routeOrder {
		r := cp.routes[pane]
		if r == nil {
			continue
		}
		if newest == nil || r.stateAt.After(newest.stateAt) {
			newest = r
		}
	}
	if newest != nil {
		d.pane, d.state, d.tool, d.stateAt = newest.pane, newest.state, newest.tool, newest.stateAt
	}
	return d
}

// applyRouteToState points the legacy (single-lane) fields at one route, so
// frameRoute can render any route without knowing routes exist.
func applyRouteToState(s *ctrlFrameState, r ctrlRoute) {
	s.target = r.pane
	s.sent = r.sent
	s.observed = r.observed
	// The state is the ONE field a finished run keeps for itself. recordFinish
	// says how the run ended by setting cp.state to finished/failed, and
	// snapshotLocked applies this to every single-route run — the pilot path —
	// so overwriting it with the route's last OBSERVED state painted a green
	// AWAITING header badge directly above the red FAILED footer, on exactly the
	// run whose outcome matters most. The session's last turn really did end
	// fine; the verdict on the run is not the session's to give.
	if !s.finished {
		s.state = r.state
	}
	s.legacySteps = r.steplog
	if r.steps > 0 {
		s.steps = r.steps
	}
	if r.goal != "" {
		s.goal = r.goal
	}
}

// frame picks the layout: one lane, or the routing table plus stream.
//
// The single-route fast path is load-bearing, not a convenience. One route and
// no MCP-level controller is exactly the pilot case this panel was built for,
// and a routing table with one row in it is strictly worse than the link
// header it would replace. `task pilot:demo` and test/ui/case3.ts must render
// byte-identically through here.
//
// A filter is the same thing arrived at deliberately: the user asked for one
// lane, and one lane is literally today's layout.
func (cp *ControlPanel) frame(s ctrlFrameState, steps []ctrlStep, w, h int) []string {
	if s.filter >= 0 {
		for _, r := range s.routes {
			if r.pane != s.filter {
				continue
			}
			rs := s
			applyRouteToState(&rs, r)
			return cp.frameRoute(rs, r.steplog, w, h)
		}
	}
	if len(s.routes) <= 1 && s.client == "" {
		return cp.frameRoute(s, steps, w, h)
	}
	return cp.frameRouted(s, w, h)
}

// frameRoute lays out ONE lane as an instrument panel:
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
func (cp *ControlPanel) frameRoute(s ctrlFrameState, steps []ctrlStep, w, h int) []string {
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
	title := paint(pal.accent, sgrBold+"CONTROL PLANE")
	out = append(out, pad+padBetween(title, paint(pal.subtle, formatDuration(since)), inner))
	out = append(out, rule(pal.border))

	linkColor := stateColor(s.state)
	link := paint(pal.accent, sgrBold+"PILOT") +
		paint(linkColor, " ══▶ ") +
		paint(pal.accent, sgrBold+"SESSION")
	out = append(out, pad+padBetween(link, badge(stateBadge(s.state), linkColor), inner))

	who := paint(pal.subtle, shortModel(s.model))
	where := paint(pal.subtle, "pane ")
	if s.target >= 0 {
		where += paint(pal.text, fmt.Sprint(s.target))
	} else {
		where += paint(pal.dead, "—")
	}
	out = append(out, pad+padBetween(who, where, inner))

	// Counters as discrete figures, not prose.
	inFlight := s.sent - s.observed
	counts := paint(pal.running, fmt.Sprint(s.sent)) + paint(pal.subtle, " sent") +
		paint(pal.border, "  │  ") +
		paint(pal.success, fmt.Sprint(s.observed)) + paint(pal.subtle, " done")
	if inFlight > 0 {
		counts += paint(pal.border, "  │  ") +
			paint(pal.warn, fmt.Sprint(inFlight)) + paint(pal.subtle, " in flight")
	}
	var right string
	if s.steps > 0 {
		frac := float64(s.observed) / float64(s.steps)
		right = meter(frac, minInt(maxInt(inner/3, 6), 18), pal.running, pal.success)
	}
	out = append(out, pad+padBetween(counts, right, inner))

	if s.goal != "" && inner > 20 {
		out = append(out, pad+paint(pal.subtle, "goal ")+
			paint(pal.text, oneLine(s.goal, inner-6)))
	}

	// ── ledger ────────────────────────────────────────────────────────────
	out = append(out, rule(pal.border))
	avail := h - len(out)
	if avail < 1 {
		return out
	}

	if len(steps) == 0 {
		out = append(out, pad+paint(pal.dead, "no instructions yet"))
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
	head := paint(pal.border, strings.Repeat("─", maxInt(inner-24, 1)))
	switch {
	case scroll > 0:
		head += paint(pal.warn, fmt.Sprintf("  ▲ %d back · End to follow", scroll))
	case hidden > 0:
		head += paint(pal.subtle, fmt.Sprintf("  ▲ %d earlier", hidden))
	default:
		head += paint(pal.border, strings.Repeat("─", 24))
	}
	out = append(out, pad+truncANSI(head, inner))

	end := len(convo) - scroll
	start := maxInt(0, end-view)
	out = append(out, convo[start:end]...)

	if s.note != "" && !s.finished {
		c := pal.subtle
		if s.noteBad {
			c = pal.fail
		}
		out = append(out, pad+paint(c, "• "+s.note))
	}
	out = append(out, footer...)
	return out
}

// ── the routing layout ────────────────────────────────────────────────────────
//
// Two instruments, answering two different questions:
//
//	route table   comparison — which pane is behind, which is blocked
//	stream        causality  — the order things actually happened in
//
// Reserve order generalizes the "footer first" rule the single-lane layout
// already has: header → finish footer → route table (capped at half the
// remaining height) → note → stream, which gets the rest and follows the tail.
func (cp *ControlPanel) frameRouted(s ctrlFrameState, w, h int) []string {
	inner := w - 2
	if inner < 6 {
		inner = w
	}
	pad := " "
	rule := func(c rgb) string { return pad + paint(c, strings.Repeat("─", maxInt(inner, 1))) }

	since := time.Since(s.startedAt)
	if since < 0 {
		since = 0 // a clock jump must not garble the header
	}

	// ── header ────────────────────────────────────────────────────────────
	arrow := stateColor(s.state)
	// A controller that has announced itself but touched nothing has no routes.
	// That is a real, reachable state (MCP announces its client on attach), and
	// the honest header for it says so rather than claiming "0 PANES" or
	// borrowing the legacy PILOT ══▶ SESSION link, which would name a session
	// that does not exist.
	subject := fmt.Sprintf("%d PANES", len(s.routes))
	switch len(s.routes) {
	case 0:
		subject = "NO PANES YET"
	case 1:
		subject = "1 PANE"
	}
	title := paint(pal.accent, sgrBold+"CONTROLLER") +
		paint(arrow, " ══▶ ") +
		paint(pal.accent, sgrBold+subject)
	// The elapsed time is the last thing to go: it is what tells an operator
	// whether a quiet panel is finished or wedged. The client id yields first.
	right := paint(pal.subtle, formatDuration(since))
	if who := shortClient(s.client, s.model); who != "" {
		withWho := paint(pal.debug, who) + "  " + right
		if visWidth(title)+visWidth(withWho)+1 <= inner {
			right = withWho
		}
	}
	out := []string{pad + padBetween(title, right, inner), rule(pal.border)}

	inFlight, blocked := 0, 0
	for _, r := range s.routes {
		if r.sent > r.observed {
			inFlight++
		}
		if r.state == "error" || r.state == "awaiting_permission" {
			blocked++
		}
	}
	counts := paint(pal.running, fmt.Sprint(s.sent)) + paint(pal.subtle, " sent") +
		paint(pal.border, " │ ") +
		paint(pal.success, fmt.Sprint(s.observed)) + paint(pal.subtle, " done")
	if inFlight > 0 {
		counts += paint(pal.border, " │ ") +
			paint(pal.warn, fmt.Sprint(inFlight)) + paint(pal.subtle, " in flight")
	}
	if blocked > 0 {
		counts += paint(pal.border, " │ ") +
			paint(pal.fail, fmt.Sprint(blocked)) + paint(pal.subtle, " blocked")
	}
	var bar string
	if s.sent > 0 {
		bar = meter(float64(s.observed)/float64(s.sent),
			minInt(maxInt(inner/4, 6), 14), pal.running, pal.success)
	}
	out = append(out, pad+padBetween(counts, bar, inner), rule(pal.border))
	if len(out) >= h {
		return out[:h]
	}

	// ── reserved: the closing statement, then the note ────────────────────
	var footer []string
	if s.finished {
		footer = finishFooter(s, pad, inner)
	}
	noteRows := 0
	if s.note != "" && !s.finished {
		noteRows = 1
	}
	closeOut := func(out []string) []string {
		out = append(out, footer...)
		if len(out) > h {
			out = out[:h]
		}
		return out
	}

	avail := h - len(out) - len(footer) - noteRows
	if avail < 1 {
		return closeOut(out)
	}

	// ── route table, or the strip it degrades to ──────────────────────────
	tableCap := maxInt(1, avail/2)
	var table []string
	switch {
	case len(s.routes) == 0:
		// No routes, no table — not even the strip, which would be a blank
		// line pretending to be a row. The header already says why.
	case h < 18 || len(s.routes) > tableCap:
		// Too short for rows: one strip line still answers "which pane is
		// blocked", which is the question the table exists for.
		table = []string{pad + truncANSI(routeStrip(s.routes, s.focused, inner), inner)}
	default:
		for _, r := range s.routes {
			table = append(table, pad+routeRow(r, s.focused, inner))
		}
		if len(table) > tableCap {
			shown := maxInt(1, tableCap-1)
			hidden := len(table) - shown
			table = append(table[:shown:shown],
				pad+paint(pal.subtle, fmt.Sprintf("  +%d more routes", hidden)))
		}
	}
	out = append(out, table...)

	avail = h - len(out) - len(footer) - noteRows - 1 // -1 for the stream rule
	if avail < 1 {
		return closeOut(out)
	}

	// ── stream ────────────────────────────────────────────────────────────
	stream := signalLines(s.signals, s.focused, pad, inner)
	maxScroll := maxInt(0, len(stream)-avail)
	scroll := minInt(s.scroll, maxScroll)
	if scroll != s.scroll {
		cp.clampScroll(scroll)
	}
	hidden := maxInt(0, len(stream)-avail-scroll)
	head := paint(pal.border, strings.Repeat("─", maxInt(inner-24, 1)))
	switch {
	case s.filter >= 0:
		head += paint(pal.accent, fmt.Sprintf("  ▸%d only · 0 for all", s.filter))
	case scroll > 0:
		head += paint(pal.warn, fmt.Sprintf("  ▲ %d back · End to follow", scroll))
	case hidden > 0:
		head += paint(pal.subtle, fmt.Sprintf("  ▲ %d earlier", hidden))
	default:
		head += paint(pal.border, strings.Repeat("─", 24))
	}
	out = append(out, pad+truncANSI(head, inner))

	end := len(stream) - scroll
	start := maxInt(0, end-avail)
	out = append(out, stream[start:end]...)

	if noteRows == 1 {
		c := pal.subtle
		if s.noteBad {
			c = pal.fail
		}
		out = append(out, pad+paint(c, "• "+s.note))
	}
	return closeOut(out)
}

// routeTag is the ▸N marker a row and a stream entry share. Accented when the
// pane is the one magmux is focused on, so the row you are looking at and the
// pane you are looking at are visibly the same thing.
func routeTag(pane, focused int) string {
	if pane < 0 {
		return paint(pal.border, padRight("··", 3))
	}
	c := pal.subtle
	glyph := " "
	if pane == focused {
		c, glyph = pal.accent, "▸"
	}
	return paint(c, padRight(glyph+fmt.Sprint(pane), 3))
}

// routeRow renders one comparison row:
//
//	▸0 api      ✓ AWAITING   4/4   12.1s ▂▅▃█  Bash
//
// Fixed columns, because the whole value of the table is that route N is
// comparable to route N+1 without reading either of them.
func routeRow(r ctrlRoute, focused, inner int) string {
	wide := inner >= 46
	c := stateColor(r.state)
	glyph := "◐"
	switch {
	case r.closed():
		c, glyph = pal.dead, "✗"
	case r.state == "awaiting_input":
		glyph = "✓"
	case r.state == "error":
		glyph = "✗"
	case r.state == "awaiting_permission":
		glyph = "⚠"
	case r.state == "idle" || r.state == "":
		c, glyph = pal.subtle, "·"
	}

	titleW, badgeW := 9, 11
	label := stateBadge(r.state)
	if !wide {
		titleW, badgeW = 7, 4
		label = stateBadgeShort(r.state)
	}
	if r.closed() {
		label = "GONE"
	}
	head := routeTag(r.pane, focused) + " " +
		paint(pal.text, padRight(oneLine(r.title, titleW), titleW)) + " " +
		paint(c, glyph+" "+padRight(oneLine(label, badgeW), badgeW)) + " " +
		paint(pal.subtle, padLeft(fmt.Sprintf("%d/%d", r.sent, r.observed), 5))

	// In flight has no duration yet, so it shows elapsed behind a ‹ marker: a
	// number that is still growing must not read like a finished one.
	dur := "  —  "
	switch {
	case r.sent > r.observed && !r.lastSent.IsZero():
		dur = "‹" + formatDuration(time.Since(r.lastSent))
	case len(r.durs) > 0:
		dur = formatDuration(time.Duration(r.durs[len(r.durs)-1] * float64(time.Second)))
	}
	tail := paint(pal.subtle, padLeft(dur, 6))
	if wide {
		tool := r.tool
		if tool == "" {
			tool = "—"
		}
		trend := r.durs
		if len(trend) > 8 {
			trend = trend[len(trend)-8:]
		}
		// Padded by rune count, not by len: spark() returns escapes, and
		// padRight measures runes, so it would count the SGR sequence as
		// visible columns.
		sp := spark(trend, c)
		if n := 8 - len(trend); n > 0 {
			sp += strings.Repeat(" ", n)
		}
		tail += " " + sp + "  " + paint(pal.debug, padRight(oneLine(tool, 8), 8))
	}
	return padBetween(head, tail, inner)
}

// routeStrip is the table's one-line degradation: every route's index and
// glyph, nothing else. Overflow is counted rather than dropped.
func routeStrip(routes []ctrlRoute, focused, inner int) string {
	var b strings.Builder
	shown := 0
	for _, r := range routes {
		c := stateColor(r.state)
		glyph := "◐"
		switch {
		case r.closed():
			c, glyph = pal.dead, "✗"
		case r.state == "awaiting_input":
			glyph = "✓"
		case r.state == "error":
			glyph = "✗"
		case r.state == "awaiting_permission":
			glyph = "⚠"
		case r.state == "idle" || r.state == "":
			c, glyph = pal.subtle, "·"
		}
		cell := routeTag(r.pane, focused) + paint(c, glyph+" ")
		if visWidth(b.String())+visWidth(cell) > inner-6 {
			break
		}
		b.WriteString(cell)
		shown++
	}
	if shown < len(routes) {
		b.WriteString(paint(pal.subtle, fmt.Sprintf("+%d more", len(routes)-shown)))
	}
	return b.String()
}

// signalLines renders the interleaved stream, oldest first:
//
//	04:31 ▸1 ▶ send_and_wait  make the tests green
//	         ⇦ ok             34 bytes + enter
//	04:39 ▸0 ◀ AWAITING       42 passed, 0 failed          8.1s
//
// The ⇦ line is an indented continuation of the ▶ it answers, never an entry
// of its own — a reply belongs to the request it answers, so it is the same
// exchange and must not be counted as a second one.
func signalLines(sigs []ctrlSignal, focused int, pad string, inner int) []string {
	showTime := inner >= 46
	verbW := 8
	switch {
	case inner >= 64:
		verbW = 14
	case inner >= 46:
		verbW = 10
	}
	// The ⇦ lands exactly under the ▶ it answers, and its status word under the
	// verb column, so an ack reads as a continuation of one row rather than as
	// a second row that happens to be indented.
	indent := 3
	if showTime {
		indent = 9
	}

	out := make([]string, 0, len(sigs))
	for _, sig := range sigs {
		glyph, gc, vc, verb := signalMark(sig)
		if verbW <= 8 && sig.dir == "in" && sig.turn {
			verb = stateBadgeShort(sig.state) // "PERM", not "PERMISS…"
		}
		var b strings.Builder
		if showTime {
			b.WriteString(paint(pal.subtle, sig.at.Format("15:04")) + " ")
		}
		b.WriteString(routeTag(sig.pane, focused))
		b.WriteString(paint(gc, glyph) + " ")
		b.WriteString(paint(vc, padRight(oneLine(verb, verbW), verbW)) + " ")

		body := paint(pal.text, oneLine(sig.text, maxInt(inner-visWidth(b.String()), 4)))
		if sig.dir == "note" {
			body = paint(pal.subtle, oneLine(sig.text, maxInt(inner-visWidth(b.String()), 4)))
		}
		if sig.turn && sig.dur > 0 {
			gap := inner - visWidth(b.String())
			body = padBetween(body, paint(pal.subtle, formatDuration(sig.dur)), maxInt(gap, 4))
		}
		out = append(out, pad+truncANSI(b.String()+body, inner))

		if sig.ok == nil {
			continue
		}
		word, ac := "ok", pal.success
		if !*sig.ok {
			word, ac = "err", pal.fail
			if sig.code != "" {
				word = oneLine(sig.code, verbW)
			}
		}
		out = append(out, pad+truncANSI(strings.Repeat(" ", indent)+
			paint(pal.border, "⇦ ")+paint(ac, padRight(word, verbW))+" "+
			paint(pal.debug, oneLine(sig.ackText, maxInt(inner-indent-verbW-3, 4))), inner))
	}
	return out
}

// signalMark picks the glyph and colours for one stream entry. The two
// directions get the two arrows they have always had; a state change that did
// NOT close a requested turn gets the state's own glyph instead of ◀, so a
// completion and a progress report can never be mistaken for each other.
func signalMark(sig ctrlSignal) (glyph string, gc, vc rgb, verb string) {
	switch sig.dir {
	case "out":
		return "▶", pal.running, pal.accent, sig.verb
	case "note":
		return "•", pal.subtle, pal.subtle, sig.verb
	}
	c := stateColor(sig.state)
	if sig.turn {
		return "◀", c, c, stateBadge(sig.state)
	}
	switch sig.state {
	case "working":
		return "◐", c, c, sig.state
	case "starting":
		return "◌", c, c, sig.state
	}
	return "·", c, c, sig.state
}

// shortClient is the controller's identity for the header: the client id it
// announced, falling back to the model it said it was running.
func shortClient(client, model string) string {
	if client != "" {
		return client
	}
	if model == "" {
		return ""
	}
	return shortModel(model)
}

// finishFooter is the run's closing statement: what happened, and what will
// happen to this window. A finished run must never look like a stalled one,
// and it must never vanish before it has been read — so the panel either
// states the key that closes it, or counts down visibly.
func finishFooter(s ctrlFrameState, pad string, inner int) []string {
	c, label := pal.success, "FINISHED"
	if s.noteBad || s.state == "failed" {
		c, label = pal.fail, "FAILED"
	}
	line := badge(label, c)
	if s.closeIn > 0 {
		secs := int(s.closeIn.Seconds() + 0.5)
		line += " " + paint(pal.warn, fmt.Sprintf("closing in %ds", secs)) +
			paint(pal.subtle, " · any key cancels")
	} else {
		line += " " + paint(pal.subtle, "press ") + paint(pal.text, "q") +
			paint(pal.subtle, " to close")
	}
	out := []string{pad + padBetween(line, "", inner)}
	if s.summary != "" {
		for _, ln := range wrapText(s.summary, inner-2, 3) {
			out = append(out, pad+paint(pal.subtle, "  "+ln))
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
		out = append(out, block("▶", pal.running, pal.text, instr, pad, inner)...)

		replyColor := pal.success
		reply := st.reply
		if reply == "" {
			if st.done() {
				reply = "(no text reported)"
			} else {
				reply = "working…"
			}
			replyColor = pal.dead
		}
		if st.bad() {
			replyColor = pal.fail
		}
		body := wrapText(reply, inner-2, ctrlMaxScroll)
		out = append(out, block("◀", pal.success, replyColor, body, pad, inner)...)
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
	glyph, c := "◐", pal.warn // in flight
	switch {
	case st.state == "awaiting_input":
		glyph, c = "✓", pal.success
	case st.state == "error":
		glyph, c = "✗", pal.fail
	case st.state == "awaiting_permission":
		glyph, c = "⚠", pal.fail
	case st.state == "gone":
		glyph, c = "✗", pal.dead
	}

	num := fmt.Sprintf("%2d ", st.n)
	head := paint(pal.subtle, num) + paint(c, glyph+" ")

	// Label column, fixed width so the bars line up.
	labelW := 12
	if inner < 40 {
		labelW = 8
	}
	head += paint(pal.text, padRight(oneLine(st.label, labelW), labelW)) + " "

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
	tail := " " + paint(pal.subtle, padLeft(durTxt, 6)) + " " + paint(pal.debug, oneLine(tool, 8))

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
	return head + meter(frac, barW, pal.running, c) + tail
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
			b.WriteString(paint(pal.subtle, "…"))
			break
		}
		b.WriteRune(r)
		n += cw
	}
	b.WriteString(sgrBase())
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
