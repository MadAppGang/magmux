package mux

// A pane observed by a PLUGIN, and the idle reconciliation both kinds of
// controller share.
//
// magmux follows Claude Code by tailing its transcript. It cannot do that for
// anything else, and it should not try: a plugin that knows how to watch its
// own tool — a build runner, a test loop, an agent magmux has never heard of —
// can simply report what it sees. That is `controller.snapshot`, and this file
// is what receives it.
//
// The rule that makes a plugin-observed pane indistinguishable from a
// magmux-observed one, everywhere it matters, is that BOTH go through
// mergeTerminalIdle. Pane idleness has two independent sources — what the
// terminal saw (OSC 9, a bracketed-paste cycle, a title change, a text-idle
// timeout) and what the controller knows — and neither is complete. Reconciling
// them in one place is what stops the live `snapshot` event and the shutdown
// `results` event disagreeing about the same pane, which is the one thing a
// driver cannot recover from.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// pluginControllerPrefix is how a plugin-observed pane names its controller,
// everywhere a controller is named: `list`, `results`, the live snapshot, the
// panel. The prefix is not decoration — it is the provenance. A reader can tell
// "magmux watched this itself" from "a plugin said so", which is exactly the
// distinction the control panel exists to keep.
const pluginControllerPrefix = "plugin:"

// pluginController is a pane whose state is reported by a plugin.
//
// It holds no transcript, no file watcher and no heuristics of its own: the
// plugin is the observer. What it adds is everything a ToolController must do
// to be interchangeable with the built-in one — the idle merge, the
// injected-input demotion, and a snapshot that is safe to read from the render
// goroutine while a socket goroutine is writing it.
type pluginController struct {
	// plugin is the registered name, without the prefix. Written at
	// construction and never again.
	plugin string
	pane   *Pane

	// injected is set by NotifyInput from a `send` or a submitting `input`, and
	// consumed by the next Poll. Atomic because the two run on different
	// goroutines and neither should have to take the lock the other holds.
	injected atomic.Bool

	// mu guards everything below. It is a LEAF: Poll reads the pane's idle
	// state under p.mu, RELEASES it, and only then takes this — so the order is
	// p.mu then pc.mu and never the reverse.
	mu   sync.Mutex
	snap Snapshot
	// lastPushAt is when the plugin last told us something. It plays exactly
	// the part ClaudeCodeController.lastApplyAt plays: the moment of the
	// controller's own freshest evidence, against which a terminal idle signal
	// is judged older or newer.
	lastPushAt time.Time
	// gone is set when the plugin's process or connection went away. The
	// controller stays attached — p.controller is write-once — and simply stops
	// being told anything, which degrades the pane to terminal-only
	// observation rather than freezing its last reported state forever.
	gone bool
}

// newPluginController builds one for a pane a plugin claimed with
// open_pane {controller:"self"}.
//
// The initial state is `starting` rather than `unknown` because the plugin has
// by definition just opened the pane: something IS starting, and a pane that
// reported `unknown` until the first push would show as a session magmux had no
// opinion about.
func newPluginController(plugin string, p *Pane) *pluginController {
	return &pluginController{
		plugin: plugin,
		pane:   p,
		snap:   Snapshot{State: CtrlStarting, StartedAt: time.Now()},
	}
}

// Name is `plugin:<name>`. See pluginControllerPrefix.
func (c *pluginController) Name() string { return pluginControllerPrefix + c.plugin }

// Start has nothing to do: the observer is another process, and it is already
// running. Idempotent, as the interface requires.
func (c *pluginController) Start(context.Context) error { return nil }

func (c *pluginController) Stop() error { return nil }

// Poll returns the last snapshot the plugin pushed, reconciled with what the
// terminal itself saw.
//
// The order inside is the lock discipline stated on the struct: the pane's idle
// state is read first, under p.mu alone, and pc.mu is taken only after it is
// released.
func (c *pluginController) Poll() (Snapshot, error) {
	idle := paneTerminalIdle(c.pane)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.injected.Swap(false) && demoteSettled(&c.snap) && dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/%s] pane=%d input injected → working\n", c.Name(), paneID(c.pane))
	}
	mergeTerminalIdle(&c.snap, idle, c.lastPushAt, c.Name())
	return c.snap, nil
}

// NotifyInput records that something typed into this pane from outside — a
// pilot's `send`, or an `input` that submitted.
//
// It exists for exactly the reason the built-in controller's does: controller
// state is ONE-WAY. mergeTerminalIdle promotes a pane to awaiting_input and
// then refuses to touch it, and only the controller's own next observation
// moves it back. A plugin that is slow to push — or that is watching for a
// pattern that has not appeared yet — would otherwise leave a driver waiting
// forever for a turn that had already begun.
//
// Deliberately not sticky: if the tool ignores the instruction, the idle
// heuristics settle the pane again, so a dropped instruction surfaces as a
// suspiciously fast empty turn rather than a permanent "working".
func (c *pluginController) NotifyInput() { c.injected.Store(true) }

// push applies one controller.snapshot from the plugin.
//
// A push is the controller's own evidence, so it stamps lastPushAt: from this
// moment a terminal idle signal older than the push cannot override it, which
// is the same ordering rule the transcript-tailing controller uses.
func (c *pluginController) push(snap protocol.ControllerSnapshot) error {
	state, ok := controllerStateOf(snap.State)
	if !ok {
		return sockErrf(sockCodeBadRequest,
			"controller.snapshot: %q is not a state (want starting, working, awaiting_input, "+
				"awaiting_permission, error or gone)", snap.State)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return sockErrf(sockCodePluginGone,
			"plugin %q is no longer connected, so it can no longer report on this pane", c.plugin)
	}
	prev := c.snap.State
	c.snap.State = state
	c.snap.LastResponse = snap.Response
	c.snap.LastTool = snap.Tool
	if snap.Prompt != "" {
		c.snap.LastUserPrompt = snap.Prompt
	}
	if snap.Model != "" {
		c.snap.Model = snap.Model
	}
	if snap.Project != "" {
		c.snap.Project = snap.Project
	}
	if snap.Error != "" {
		c.snap.Error = fmt.Errorf("%s", snap.Error)
	} else {
		c.snap.Error = nil
	}
	// The two clocks are magmux's, not the plugin's: a turn that began is one
	// this process saw begin. StartedAt moves only on a transition INTO working,
	// so a plugin that re-pushes `working` every second does not keep restarting
	// its own turn.
	now := time.Now()
	switch {
	case state == CtrlWorking && prev != CtrlWorking:
		c.snap.StartedAt = now
		c.snap.CompletedAt = time.Time{}
	case settledState(state) && !settledState(prev):
		c.snap.CompletedAt = now
	}
	c.lastPushAt = now
	// A push is fresher evidence than any injected input that preceded it, and
	// the plugin has just said what the state IS. Clearing the flag here stops
	// the next Poll demoting the answer the plugin just gave.
	c.injected.Store(false)
	return nil
}

// markGone records that the plugin behind this controller has died.
func (c *pluginController) markGone() {
	c.mu.Lock()
	c.gone = true
	c.mu.Unlock()
}

// settledState is the set mergeTerminalIdle will not touch: a turn that has
// ended, one way or another.
func settledState(s ControllerState) bool {
	switch s {
	case CtrlAwaitingInput, CtrlAwaitingPermission, CtrlError, CtrlGone:
		return true
	}
	return false
}

// controllerStateOf parses the wire spelling of a state. It refuses an unknown
// one rather than mapping it to CtrlUnknown, because a typo that silently
// became "unknown" would show a driver a pane magmux has no opinion about and
// give the plugin author no clue why.
func controllerStateOf(s string) (ControllerState, bool) {
	switch s {
	case "starting":
		return CtrlStarting, true
	case "working":
		return CtrlWorking, true
	case "awaiting_input":
		return CtrlAwaitingInput, true
	case "awaiting_permission":
		return CtrlAwaitingPermission, true
	case "error":
		return CtrlError, true
	case "gone":
		return CtrlGone, true
	case "unknown":
		return CtrlUnknown, true
	}
	return CtrlUnknown, false
}

// ── the shared idle merge ───────────────────────────────────────────────────

// terminalIdle is what the PANE itself concluded about its child, as opposed to
// what a controller knows about the tool inside it.
type terminalIdle struct {
	ready  bool
	signal string
	at     time.Time
}

// paneTerminalIdle reads that verdict. Caller must NOT hold p.mu, and must not
// hold any controller lock either: this takes p.mu, and every controller lock
// is below it.
func paneTerminalIdle(p *Pane) terminalIdle {
	if p == nil {
		return terminalIdle{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return terminalIdle{ready: p.inputReady, signal: p.inputSignal, at: p.inputReadyAt}
}

// mergeTerminalIdle promotes a snapshot to CtrlAwaitingInput when the pane
// itself has seen its child go idle at a prompt, and the controller has said
// nothing more recent.
//
// It is the ONE copy of this rule, used by every controller, and that is the
// point rather than a tidiness. A controller learns a turn ended from the
// tool's own record — Claude Code's transcript, a plugin's own watcher — and
// neither is reliable on its own: Claude Code only announces "turn finished"
// through a stop_hook_summary entry that exists solely when the user has a Stop
// hook configured, and a plugin may be watching for a pattern that never
// appears. The pane's own idle detection already knows better, and
// buildPaneResults has always reported from it. Promoting here is what stops
// the live `snapshot` event and the final `results` event contradicting each
// other about the same pane.
//
// Promotion is ONE-WAY and ORDERED:
//
//   - one-way, because a settled snapshot is left alone. Only the controller's
//     own next observation moves a pane off awaiting_input, which is what
//     NotifyInput exists to unstick when that observation is late;
//   - ordered, because it applies only when the terminal went idle AFTER the
//     controller's freshest evidence. A controller that has just been told
//     something is authoritative, so the state cannot flap.
//
// It takes NO lock: the caller holds its own controller lock and has already
// read the pane's state through paneTerminalIdle. Reports whether it promoted.
func mergeTerminalIdle(snap *Snapshot, idle terminalIdle, lastEvidenceAt time.Time, who string) bool {
	if settledState(snap.State) {
		return false // already settled; nothing to promote
	}
	// "ctrl" and "perm" are set BY a controller snapshot — reading them back
	// would be a feedback loop, not evidence.
	if !idle.ready || idle.signal == "ctrl" || idle.signal == "perm" {
		return false
	}
	// The controller moved at or after the terminal went idle, so the
	// controller is the fresher signal. Leave the state alone.
	if !idle.at.After(lastEvidenceAt) {
		return false
	}
	snap.State = CtrlAwaitingInput
	if snap.CompletedAt.IsZero() {
		snap.CompletedAt = idle.at
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/%s] terminal idle (%s) → awaiting_input\n", who, idle.signal)
	}
	return true
}

// demoteSettled moves a settled snapshot back to working, for a controller that
// has just learned an instruction was injected into its pane.
//
// Clearing LastResponse is the load-bearing half. pollControllers emits
// `response` on every snapshot, so an answer left over from the previous turn
// would be broadcast as if the NEW turn had said it — and a tool-only turn
// would then settle still carrying it, which is precisely the stale answer a
// two-phase wait promises cannot happen.
//
// CtrlGone is deliberately NOT demoted, though it is otherwise a settled
// state: a tool whose process has exited cannot start a turn, and reporting it
// as working because somebody typed at it would be magmux inventing a session.
//
// Reports whether it demoted anything.
func demoteSettled(snap *Snapshot) bool {
	switch snap.State {
	case CtrlAwaitingInput, CtrlAwaitingPermission, CtrlError:
	default:
		return false
	}
	snap.State = CtrlWorking
	snap.StartedAt = time.Now()
	snap.CompletedAt = time.Time{}
	snap.LastTool = ""
	snap.LastResponse = ""
	return true
}

// paneID is p.id with a nil guard, for debug lines. p.id is write-once before
// the pane is reachable, so it needs no lock.
func paneID(p *Pane) int {
	if p == nil {
		return -1
	}
	return p.id
}
