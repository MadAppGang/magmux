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

// sendToPane delivers a pilot instruction: optional text, then optional named
// keys, then optionally Enter. Runs on its own goroutine because of the
// inter-write delay — the socket reader must not block on a slow pane.
func (m *Magmux) sendToPane(idx int, text string, keys []string, enter bool, label string) {
	if idx < 0 || idx >= len(m.allPanes) {
		return
	}
	p := m.allPanes[idx]
	if p.isControl {
		return // the panel is not a session; nothing to type into
	}

	// Log before delivery so the panel shows the instruction even if the
	// pane rejects it — a send that vanished is exactly what you want to see.
	m.control.recordSend(idx, label, text)
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
		if text != "" {
			if !p.injectPTY(p.pasteWrap(text)) {
				return
			}
		}
		for _, k := range keys {
			b, ok := keyBytes(k)
			if !ok {
				if dbgFile != nil {
					fmt.Fprintf(dbgFile, "[pilot] unknown key %q\n", k)
				}
				continue
			}
			time.Sleep(20 * time.Millisecond)
			p.injectPTY(b)
		}
		if enter {
			// Let the TUI settle on the text before submitting it.
			time.Sleep(pilotSendDelay)
			p.injectPTY([]byte("\r"))
		}
	}()
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
	page := maxInt(1, m.focused.h-6)
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
func (m *Magmux) dispatchPilotMsg(msg sockMsg) {
	switch msg.Event {
	case "start", "":
		pane := m.parsePaneIndex(msg.Pane)
		if pane < 0 {
			pane = 0
		}
		m.control.recordStart(pane, msg.Goal, msg.Model, msg.Steps)
		m.broadcastEvent(map[string]any{
			"type": "control", "dir": "note", "event": "start",
			"pane": pane, "goal": msg.Goal, "model": msg.Model, "steps": msg.Steps,
		})
	case "finish", "fail":
		failed := msg.Event == "fail"
		m.control.recordFinish(msg.Summary, failed)
		// Arm the auto-close countdown, if one was configured. Deliberately
		// only on pilot finish: a run that ends by the session going idle is
		// not the same as the pilot declaring the task over.
		if m.autoCloseAfter > 0 {
			m.closeAt = time.Now().Add(m.autoCloseAfter)
		}
		m.broadcastEvent(map[string]any{
			"type": "control", "dir": "note", "event": msg.Event,
			"summary": msg.Summary, "failed": failed,
		})
	case "note":
		note := msg.Label
		if msg.Text != "" {
			note = strings.TrimSpace(note + " " + msg.Text)
		}
		m.control.setNote(note, false)
	}
}
