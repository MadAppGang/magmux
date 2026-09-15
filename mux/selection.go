package mux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ── Selection color config ────────────────────────────────────────────────────
// Override with MAGMUX_SEL_FG / MAGMUX_SEL_BG env vars (256-color index)
var (
	selFg = 0   // black text
	selBg = 220 // yellow background (256-color)
)

// ── Selection state (matches MTM's sel_* globals) ─────────────────────────────

type Selection struct {
	active bool
	pane   *Pane
	sy, sx int // start (pane-relative)
	ey, ex int // end (pane-relative)
}

// sel is package-level state, so it is treeMu-guarded like the rest of the
// layout: sel.pane is a *Pane that close_pane can detach at any moment, and
// the renderer reads the whole struct on every frame.
var sel Selection

// selClear drops the current selection. Caller holds treeMu.Lock.
func (m *Magmux) selClear() {
	sel.active = false
	sel.pane = nil
}

// selTextLocked extracts the selected text and clears the selection. It does
// NO I/O: putting the text on the clipboard runs pbcopy, and rule 1 forbids
// holding treeMu across that. Returns "" when there is nothing to copy.
//
// Caller holds treeMu.Lock.
func (m *Magmux) selTextLocked() string {
	if sel.pane == nil {
		return ""
	}
	s := sel.pane.screen

	// Normalize start/end
	sy, sx, ey, ex := sel.sy, sel.sx, sel.ey, sel.ex
	if sy > ey || (sy == ey && sx > ex) {
		sy, sx, ey, ex = ey, ex, sy, sx
	}

	// Extract text line by line from the screen buffer. A selection is up to
	// three segments — the anchor row from sx to the right edge, whole rows
	// between, and the cursor row from the left edge to ex — which is why this
	// is three rowsText calls and not one. rowsText clamps rows past s.rows,
	// so a selection left over from before a shrink drops them exactly as the
	// old `r < s.rows` loop guard did.
	var lines []string
	sel.pane.mu.Lock()
	if sy == ey {
		lines = s.rowsText(sy, sy+1, sx, ex)
	} else {
		lines = append(lines, s.rowsText(sy, sy+1, sx, s.cols-1)...)
		lines = append(lines, s.rowsText(sy+1, ey, 0, s.cols-1)...)
		lines = append(lines, s.rowsText(ey, ey+1, 0, ex)...)
	}
	sel.pane.mu.Unlock()

	content := strings.Join(lines, "\n")
	if content == "" {
		return ""
	}

	// Deselect after copy
	sel.pane = nil
	sel.active = false
	return content
}

// putClipboard pushes text to the terminal's clipboard. Blocking (it execs
// pbcopy), so it must run with no lock held.
func putClipboard(content string) {
	if content == "" {
		return
	}
	// Method 1: OSC 52 clipboard escape (works over SSH)
	encoded := encodeBase64(content)
	os.Stdout.WriteString(fmt.Sprintf("\x1b]52;c;%s\x07", encoded))

	// Method 2: pbcopy fallback (local macOS)
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(content)
	cmd.Run()
}

func encodeBase64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var buf strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var b0, b1, b2 byte
		b0 = data[i]
		if i+1 < len(data) {
			b1 = data[i+1]
		}
		if i+2 < len(data) {
			b2 = data[i+2]
		}
		buf.WriteByte(alphabet[b0>>2])
		buf.WriteByte(alphabet[((b0&3)<<4)|(b1>>4)])
		if i+1 < len(data) {
			buf.WriteByte(alphabet[((b1&0xf)<<2)|(b2>>6)])
		} else {
			buf.WriteByte('=')
		}
		if i+2 < len(data) {
			buf.WriteByte(alphabet[b2&0x3f])
		} else {
			buf.WriteByte('=')
		}
	}
	return buf.String()
}

// parseSGRMouse handles ESC [ < btn ; col ; row M/m
// Mouse events are consumed by magmux (never forwarded to children).
// Matches MTM behavior: click = focus, drag = selection, release = copy.
func (m *Magmux) parseSGRMouse(buf []byte) (int, bool) {
	// buf starts at ESC, buf[1]=='[', buf[2]=='<'
	end := 3
	for end < len(buf) {
		if buf[end] == 'M' || buf[end] == 'm' {
			break
		}
		if buf[end] < 0x20 || buf[end] > 0x7e {
			return end + 1, false
		}
		end++
	}
	if end >= len(buf) {
		return 0, false // incomplete
	}

	params := string(buf[3:end])
	press := buf[end] == 'M'

	var btn, col, row int
	parts := strings.Split(params, ";")
	if len(parts) >= 1 {
		fmt.Sscanf(parts[0], "%d", &btn)
	}
	if len(parts) >= 2 {
		fmt.Sscanf(parts[1], "%d", &col)
	}
	if len(parts) >= 3 {
		fmt.Sscanf(parts[2], "%d", &row)
	}

	row0 := row - 1 // 0-indexed
	col0 := col - 1
	termChar := buf[end]

	// All of the state below (m.focused, sel, pane geometry) is treeMu's, so
	// the whole decision runs under one write lock and the two blocking
	// consequences — forwarding to a PTY, and putting text on the clipboard —
	// are deferred until after it is released.
	var (
		scrollPanel int    // 0 = no, else lines to scroll
		forwardTo   *Pane  // alt-screen mouse forwarding target
		forward     string // the sequence to forward
		copyText    string
	)

	m.treeMu.Lock()

	// Always: left click press switches focus (even in alt mode)
	if press && btn == 0 {
		if target := m.findPaneAtLocked(row0, col0); target != nil {
			m.focused = target
			m.control.setFocused(target.id)
		}
	}

	// Wheel over the control panel scrolls its exchange, whichever pane has
	// focus. Checked before the alt-screen forward below: the panel has no
	// PTY, so forwarding would drop the event entirely.
	if btn == 64 || btn == 65 {
		if target := m.findPaneAtLocked(row0, col0); target != nil && target.isControl {
			if btn == 64 {
				scrollPanel = 3
			} else {
				scrollPanel = -3
			}
		}
	}

	if scrollPanel == 0 {
		// If focused pane is in alternate screen (vim, htop, Claude Code,
		// OpenCode), forward ALL mouse events to it — like tmux does.
		if f := m.focused; f != nil && f.altMode {
			localRow := maxInt(1, row0-f.y+1)
			localCol := maxInt(1, col0-f.x+1)
			forwardTo = f
			forward = fmt.Sprintf("\x1b[<%d;%d;%d%c", btn, localCol, localRow, termChar)
		} else {
			// Normal mode (bash, etc.): handle mouse ourselves for selection
			switch {
			case btn == 64 || btn == 65: // wheel up / down
				// Scrolling a non-alt pane with the wheel is what every terminal
				// does and it cannot collide with anything: the alt-screen
				// branch above already claimed the wheel for TUIs, and this
				// branch is only reached when the focused pane is not one. It
				// targets the pane UNDER THE POINTER rather than the focused
				// one, like the panel's wheel handling directly above.
				target := m.findPaneAtLocked(row0, col0)
				if target == nil {
					target = m.focused
				}
				if target != nil && !target.isControl && target.screen != nil {
					target.mu.Lock()
					if !target.altMode && (target.screen.sbLen > 0 || target.screen.sbOff > 0) {
						if btn == 64 {
							target.screen.scrollBackBy(3)
						} else {
							target.screen.scrollBackBy(-3)
						}
						target.dirty = true
					}
					target.mu.Unlock()
				}

			case press && btn == 0: // Left click → start selection
				m.selClear()
				if f := m.focused; f != nil {
					// Clamp, exactly as the drag and release branches below do.
					// findPaneAtLocked returns nil on a split border, so a click
					// there does NOT move focus — it stays on the pane it was
					// already on, one cell away. A border above or to the left of
					// that pane then yields -1 here, and renderSelection indexes
					// p.screen.cells with it on the very next frame: magmux
					// panics and takes every session inside it down.
					sel.pane = f
					sel.active = true
					sel.sy = clamp(row0-f.y, 0, maxInt(0, f.h-1))
					sel.sx = clamp(col0-f.x, 0, maxInt(0, f.w-1))
					sel.ey = sel.sy
					sel.ex = sel.sx
				}

			case press && btn == 32: // Drag
				if sel.active && sel.pane != nil {
					sel.ey = clamp(row0-sel.pane.y, 0, maxInt(0, sel.pane.h-1))
					sel.ex = clamp(col0-sel.pane.x, 0, maxInt(0, sel.pane.w-1))
				}

			case !press && btn == 0: // Release → copy
				if sel.active && sel.pane != nil {
					sel.ey = clamp(row0-sel.pane.y, 0, maxInt(0, sel.pane.h-1))
					sel.ex = clamp(col0-sel.pane.x, 0, maxInt(0, sel.pane.w-1))
					if sel.sy != sel.ey || sel.sx != sel.ex {
						copyText = m.selTextLocked()
					}
					sel.active = false
				}
			}
		}
	}

	m.treeMu.Unlock()

	if scrollPanel != 0 {
		m.control.scrollBy(scrollPanel)
		return end + 1, true
	}
	if forwardTo != nil {
		forwardTo.writePTY([]byte(forward))
		return end + 1, true
	}
	putClipboard(copyText)

	return end + 1, true
}
