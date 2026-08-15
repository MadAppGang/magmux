package main

// Pilot wiring — the inbound half of a controlled session.
//
// magmux's socket has always been able to *describe* a pane (tint, overlay,
// status, agent hook events) but never to *drive* one. A controlled session
// needs both halves: an external agent reads the session's state off the
// socket and must be able to push the next instruction back in.
//
// That is what `send` is. The subtlety is that the state a pilot wants to
// act on — the pane has finished its turn and is waiting — is exactly the
// state `writePTY` refuses to write to, because in grid mode an idle pane is
// considered "done" and user keystrokes are suppressed so a finished grid
// can be dismissed with `q`. So a pilot send cannot go through writePTY; it
// takes the inject path below, which clears the idle state the way a real
// keystroke would and then writes regardless.

import (
	"fmt"
	"strings"
	"time"
)

// pilotSendDelay is the pause between writing an instruction's text and
// writing the Enter that submits it. Interactive TUIs (Claude Code among
// them) debounce and re-render their input box; submitting in the same write
// as the text can land the Return before the box has taken the text, which
// silently drops the instruction.
const pilotSendDelay = 150 * time.Millisecond

// namedKeys maps the key names accepted by `send` to the bytes a terminal
// would produce. Cursor keys use the normal (non-application) CSI forms —
// magmux ignores DECCKM, so panes see these regardless of mode.
var namedKeys = map[string][]byte{
	"enter":     []byte("\r"),
	"return":    []byte("\r"),
	"tab":       []byte("\t"),
	"escape":    []byte("\x1b"),
	"esc":       []byte("\x1b"),
	"space":     []byte(" "),
	"backspace": []byte("\x7f"),
	"delete":    []byte("\x1b[3~"),
	"up":        []byte("\x1b[A"),
	"down":      []byte("\x1b[B"),
	"right":     []byte("\x1b[C"),
	"left":      []byte("\x1b[D"),
	"home":      []byte("\x1b[H"),
	"end":       []byte("\x1b[F"),
	"pageup":    []byte("\x1b[5~"),
	"pagedown":  []byte("\x1b[6~"),
	"ctrl-a":    {0x01},
	"ctrl-b":    {0x02},
	"ctrl-c":    {0x03},
	"ctrl-d":    {0x04},
	"ctrl-e":    {0x05},
	"ctrl-k":    {0x0b},
	"ctrl-l":    {0x0c},
	"ctrl-n":    {0x0e},
	"ctrl-p":    {0x10},
	"ctrl-r":    {0x12},
	"ctrl-u":    {0x15},
	"ctrl-w":    {0x17},
}

// keyBytes resolves a named key, or a single literal character.
func keyBytes(name string) ([]byte, bool) {
	k := strings.ToLower(strings.TrimSpace(name))
	if b, ok := namedKeys[k]; ok {
		return b, true
	}
	// A bare single character is taken literally, so a pilot can answer a
	// numbered menu with {"keys":["2"]} without a special case.
	if r := []rune(name); len(r) == 1 {
		return []byte(name), true
	}
	return nil, false
}

// injectPTY writes to the pane's PTY on behalf of an external controller.
//
// Unlike writePTY it does not refuse an idle pane — steering an idle session
// is the entire point — but it clears the same completion state a real
// keystroke would, so the pane stops reading as "done" the moment work is
// pushed into it. Returns false if the pane cannot accept input at all.
//
// Caller must NOT hold p.mu.
func (p *Pane) injectPTY(data []byte) bool {
	p.mu.Lock()
	if p.ptmx == nil || p.dead {
		p.mu.Unlock()
		return false
	}
	if p.inputReady {
		// Mirror writePTY: an instruction restarts the turn, so the pane is
		// no longer awaiting input and its completion chrome must go. Reset
		// hadTextOutput too, or the text-idle heuristic can immediately
		// re-fire on the pre-existing output and call the pane done again.
		p.inputReady = false
		p.inputSignal = ""
		p.tint = ""
		p.overlayText = ""
		p.overlayStyle = ""
		p.hadTextOutput = false
		p.lastTextAt = time.Now()
		p.titleIdleAt = time.Time{}
	}
	p.dirty = true
	ptmx := p.ptmx
	p.mu.Unlock()

	_, err := ptmx.Write(data)
	if err != nil && dbgFile != nil {
		fmt.Fprintf(dbgFile, "[pilot] inject write error: %v\n", err)
	}
	return err == nil
}

// pasteWrap wraps multi-line text in bracketed-paste markers when the pane
// has requested that mode. Without this, the first newline of a multi-line
// instruction submits a half-written prompt.
func (p *Pane) pasteWrap(text string) []byte {
	p.mu.Lock()
	bracketed := p.bracketPaste
	p.mu.Unlock()
	if bracketed && strings.Contains(text, "\n") {
		return []byte("\x1b[200~" + text + "\x1b[201~")
	}
	return []byte(text)
}

// sendToPane delivers a driver instruction: optional text, then optional named
// keys, then optionally Enter. The writes run on their own goroutine because of
// the inter-write delay — the socket reader must not block on a slow pane.
//
// done, if non-nil, is called on that delivery goroutine once every write has
// been attempted: nil on success, a *sockErr otherwise. It is the only way a
// caller can learn that the bytes never reached the PTY, a failure that until
// now lived and died inside this goroutine. Note what it does and does not
// claim: the bytes were written, not that the TUI on the far end accepted them
// — an app that is not ready for a paste can still drop it, which is what
// pilotSendDelay exists to make unlikely.
//
// Synchronous validation errors (a bad index, the control pane) are returned
// directly and done is NOT called, so a caller waiting on a reply gets exactly
// one answer by exactly one route.
//
// Caller must NOT hold p.mu.
func (m *Magmux) sendToPane(idx int, text string, keys []string, enter bool, label string, done func(error)) error {
	// Resolved through the identity table, never by raw index: after a
	// close_pane the ids are sparse, and a bounds check alone would happily
	// type an instruction into a pane that is no longer on screen.
	p := m.paneByID(idx)
	if p == nil {
		return sockErrf(sockCodeNoSuchPane, "no pane %d (it may have been closed)", idx)
	}
	if p.isControl {
		// the panel is not a session; nothing to type into
		return sockErrf(sockCodePaneIsControl, "pane %d is the control panel and has no session to type into", idx)
	}

	// Log before delivery so the panel shows the instruction even if the
	// pane rejects it — a send that vanished is exactly what you want to see.
	// The seq is how magmux's own ack finds this OUT row again; the ack is a
	// continuation of the request, never a row of its own, and never closes a
	// turn.
	seq := m.control.recordSend(idx, label, text)
	ev := map[string]any{
		"type":  "control",
		"dir":   "out",
		"pane":  idx,
		"label": label,
		"text":  text,
		"at":    time.Now().UTC().Format(time.RFC3339),
	}
	if len(keys) > 0 {
		ev["keys"] = keys // omitted rather than null when unused
	}
	m.broadcastEvent(ev)

	// Tell the pane's controller a new turn is starting. Without this a
	// controller whose transcript is missing or lagging stays settled on the
	// previous turn's awaiting_input, and a pilot waiting for the turn to
	// begin waits forever. See InputNotifier.
	if n, ok := p.controller.(InputNotifier); ok && p.controller != nil {
		n.NotifyInput()
	}

	go func() {
		// A rejected write means the pane cannot take input at all — its child
		// exited or its PTY is closed — so the instruction is gone and there is
		// no point pressing on. A bad key name is the caller's mistake in one
		// keystroke: the rest is still delivered, as it always was, and the
		// first such error is what the reply reports.
		dead := func(what string) error {
			return sockErrf(sockCodePaneDead,
				"pane %d rejected the %s: its child has exited or its PTY is closed", idx, what)
		}
		var firstErr error
		finish := func(err error) {
			// The panel learns the outcome whether or not anyone asked for a
			// reply: a fire-and-forget send that never reached the PTY is
			// exactly the failure an operator needs to see.
			m.control.recordAck(seq, err == nil, verbErrCode(err), ackText(text, keys, enter, err))
			if done != nil {
				done(err)
			}
		}

		if text != "" {
			if !p.injectPTY(p.pasteWrap(text)) {
				finish(dead("text"))
				return
			}
		}
		for _, k := range keys {
			b, ok := keyBytes(k)
			if !ok {
				if dbgFile != nil {
					fmt.Fprintf(dbgFile, "[pilot] unknown key %q\n", k)
				}
				if firstErr == nil {
					firstErr = sockErrf(sockCodeBadRequest, "unknown key %q", k)
				}
				continue
			}
			time.Sleep(20 * time.Millisecond)
			if !p.injectPTY(b) && firstErr == nil {
				firstErr = dead("key " + k)
			}
		}
		if enter {
			// Let the TUI settle on the text before submitting it.
			time.Sleep(pilotSendDelay)
			if !p.injectPTY([]byte("\r")) && firstErr == nil {
				firstErr = dead("enter")
			}
		}
		finish(firstErr)
	}()
	return nil
}

// ackText summarises what magmux did with a send, for the ⇦ continuation on
// the panel's OUT row. It describes the delivery, not the session's reaction:
// "the bytes reached the PTY" is all an ack can honestly claim.
func ackText(text string, keys []string, enter bool, err error) string {
	if err != nil {
		return err.Error()
	}
	var parts []string
	if n := len(text); n > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes", n))
	}
	if n := len(keys); n > 0 {
		parts = append(parts, fmt.Sprintf("%d keys", n))
	}
	if enter {
		parts = append(parts, "enter")
	}
	if len(parts) == 0 {
		return "nothing to send"
	}
	return strings.Join(parts, " + ")
}

// consumeControlKey handles a keystroke aimed at the control panel, which has
// no PTY to write to. Returns how many bytes it consumed, or 0 to let the
// normal path have them.
//
// Scrolling matters here because the panel holds the full exchange: every
// instruction and every reply, which is far more than fits on screen by the
// third or fourth step.
func (m *Magmux) consumeControlKey(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	f := m.focusedPane()
	if f == nil {
		return 0
	}
	m.treeMu.RLock()
	page := maxInt(1, f.h-6)
	m.treeMu.RUnlock()
	switch buf[0] {
	case 'k':
		m.control.scrollBy(1)
		return 1
	case 'j':
		m.control.scrollBy(-1)
		return 1
	case 'g':
		m.control.scrollBy(ctrlMaxScroll)
		return 1
	case 'G':
		m.control.scrollToBottom()
		return 1
	case ' ':
		m.control.scrollBy(-page)
		return 1
	case '[':
		m.control.cycleFilter(-1)
		return 1
	case ']':
		m.control.cycleFilter(1)
		return 1
	}
	// Digits filter the panel to one PANE INDEX — deliberately the pane's own
	// number and not the Nth route, because a second numbering beside the ids
	// every other verb uses would be a bug factory. They cost nothing to claim:
	// this handler only runs when the focused pane isControl, and that pane has
	// no PTY, so a digit is silently dropped today.
	if buf[0] >= '0' && buf[0] <= '9' {
		if buf[0] == '0' {
			m.control.setFilter(-1)
		} else {
			m.control.setFilter(int(buf[0] - '0'))
		}
		return 1
	}
	if buf[0] != 0x1b || len(buf) < 3 || buf[1] != '[' {
		return 0
	}
	switch buf[2] {
	case 'A': // up
		m.control.scrollBy(1)
		return 3
	case 'B': // down
		m.control.scrollBy(-1)
		return 3
	case 'H': // home
		m.control.scrollBy(ctrlMaxScroll)
		return 3
	case 'F': // end
		m.control.scrollToBottom()
		return 3
	}
	// PageUp/PageDown arrive as CSI 5~ / CSI 6~
	if len(buf) >= 4 && buf[3] == '~' {
		switch buf[2] {
		case '5':
			m.control.scrollBy(page)
			return 4
		case '6':
			m.control.scrollBy(-page)
			return 4
		}
	}
	return 0
}

// dispatchPilotMsg handles the socket verbs a pilot uses to announce itself
// and to close out a run. Kept separate from dispatchSocketMsg's display
// verbs because these describe the *session*, not a pane's appearance.
func (m *Magmux) dispatchPilotMsg(msg sockMsg) error {
	switch msg.Event {
	case "start", "":
		// A named pane and an absent one mean genuinely different things, and
		// collapsing the second into pane 0 was a bug.
		//
		// NAMED (`"pane":0`, the pi pilot's shape) is a run start against that
		// pane: the route opens here, and that pane is what every later
		// pane-less `send` is delivered to. A pane that was *given* and is not
		// an index ("*", "api") is refused rather than rounded down — silently
		// retargeting the announced pane mis-routes the entire run.
		//
		// ABSENT identifies the CONTROLLER, not a route: MCP announces its
		// `client` once per attached session, before it has touched anything.
		// A route is created the first time the controller touches a pane, and
		// an identity announcement touches none — so mapping absent to 0 put a
		// pane nothing had driven into the ledger at 0/0, and made a controller
		// that then drove panes 1 and 2 look as though an instruction to pane 0
		// had been dropped. recordStart takes paneUnspecified verbatim and
		// writes header state only.
		pane := m.parsePaneIndex(msg.Pane)
		if pane != paneUnspecified && pane < 0 {
			return sockErrf(sockCodeBadRequest, "pane is not an index")
		}
		// `client` is the ONE field MCP adds to this protocol: an identity for
		// the header ("claude-code/2.1"). Everything else is the same event the
		// pi pilot has always sent, which is what keeps the promise that
		// anything speaking these verbs fills the panel in.
		m.control.recordStart(pane, msg.Goal, msg.Model, msg.Client, msg.Steps)
		ev := map[string]any{
			"type": "control", "dir": "note", "event": "start",
			"goal": msg.Goal, "model": msg.Model, "steps": msg.Steps,
		}
		if pane != paneUnspecified {
			ev["pane"] = pane // omitted, not -3: the event named no route
		}
		if msg.Client != "" {
			ev["client"] = msg.Client // omitted rather than empty for a pilot
		}
		m.broadcastEvent(ev)
		return nil
	case "finish", "fail":
		failed := msg.Event == "fail"
		m.control.recordFinish(msg.Summary, failed)
		// Arm the auto-close countdown, if one was configured. Deliberately
		// only on pilot finish: a run that ends by the session going idle is
		// not the same as the pilot declaring the task over.
		if m.autoCloseAfter > 0 {
			m.treeMu.Lock()
			m.closeAt = time.Now().Add(m.autoCloseAfter)
			m.treeMu.Unlock()
		}
		m.broadcastEvent(map[string]any{
			"type": "control", "dir": "note", "event": msg.Event,
			"summary": msg.Summary, "failed": failed,
		})
		return nil
	case "note":
		note := msg.Label
		if msg.Text != "" {
			note = strings.TrimSpace(note + " " + msg.Text)
		}
		m.control.setNote(note, false)
		return nil
	}
	return sockErrf(sockCodeBadRequest, "unknown pilot event %q", msg.Event)
}
