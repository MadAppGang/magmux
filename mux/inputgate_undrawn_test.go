package mux

// A box that LOOKS empty is not proof that Claude Code's input buffer is
// empty: under load Claude can have read the first copy of a send and not yet
// drawn it, or not have read it at all. Anything written after it lands
// BEHIND that copy in the PTY stream, so a second copy would be submitted
// with the first. The delivery therefore types a send's text once, and an
// emptied box leaves exactly one copy in the buffer.

import (
	"strings"
	"testing"
)

// claudeInputModel replays the bytes a pane's child received through a
// sequential model of Claude Code's input box and returns what was submitted.
// A printable rune appends; ctrl-u (0x15) deletes the last wrapped row at
// width cols-4 (one row per key, nothing on an empty buffer); a carriage
// return submits the buffer and empties it. Every other control byte is
// ignored. It models Claude reading each byte as it arrives; a reader that
// stalls and then reads a large backlog at once is stalledReaderModel.
func claudeInputModel(stream string, cols int) []string {
	width := max(cols-4, 1)
	var buf []rune
	var submits []string
	for _, r := range stream {
		switch {
		case r == '\r':
			submits = append(submits, string(buf))
			buf = buf[:0]
		case r == 0x15:
			if len(buf) == 0 {
				continue
			}
			last := len(buf) % width
			if last == 0 {
				last = width
			}
			buf = buf[:len(buf)-last]
		case r < 0x20 || r == 0x7f:
			// not modelled
		default:
			buf = append(buf, r)
		}
	}
	return submits
}

// TestReadButUndrawnTextIsSubmittedOnce: a single-line text long enough to
// wrap over three rows of a 60-column pane is typed; the box stays empty on
// every read. The first copy was READ but not drawn, so it is still in
// Claude's buffer. The delivery must leave exactly that one copy: no second
// copy, no clear and no Enter of its own, so the next Enter — whoever presses
// it — submits the text exactly once.
//
// REGRESSION: a retype typed onto text Claude had read but not drawn submitted it twice.
func TestReadButUndrawnTextIsSubmittedOnce(t *testing.T) {
	text := "Please review the authentication configuration in the launcher, " +
		"check the directory key handling, and report every place it is read now."
	if n := len([]rune(text)); n < 130 || n > 160 || strings.ContainsAny(text, "\n\r") || strings.HasPrefix(text, "!") {
		t.Fatalf("test text is %d runes; want ~140, single-line, not bash mode", n)
	}

	// The model itself: one ctrl-u between two copies clears one wrapped row
	// and leaves the rest of copy 1 in front of copy 2; one per row clears it.
	t.Run("model", func(t *testing.T) {
		const cols = 60
		rows := (len([]rune(text)) + cols - 5) / (cols - 4)
		one := claudeInputModel(text+"\x15"+text+"\r", cols)
		if len(one) == 1 && one[0] == text {
			t.Errorf("one ctrl-u cleared a %d-row copy; the model does not pin the row count", rows)
		}
		enough := claudeInputModel(text+strings.Repeat("\x15", rows)+text+"\r", cols)
		if len(enough) != 1 || enough[0] != text {
			t.Errorf("%d ctrl-u (one per row) left %q, want [%q]", rows, enough, text)
		}
	})

	for _, tc := range []struct {
		name  string
		word  string
		empty func() BoxCheck
	}{
		{"never_drawn", "never drawn", boxEmptyCheck},
		{"terminal_reset", "reset", lostBoxCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return tc.empty() }}
			m, p, log := gatedMux(t, g)
			p.mu.Lock()
			cols := p.screen.cols
			p.mu.Unlock()
			if rows := (len([]rune(text)) + cols - 5) / (cols - 4); rows < 3 {
				t.Fatalf("text wraps over %d rows at %d columns; want at least 3", rows, cols)
			}

			o := runSend(t, m, sendRequest{text: text, enter: true})
			stream := log.settle()
			wantNotRetyped(t, o, tc.word)
			if strings.Contains(stream, "\r") {
				t.Errorf("the delivery pressed Enter for a box that never verified\nbytes: %q", stream)
			}

			// A later Enter submits what the buffer holds.
			submits := claudeInputModel(stream+"\r", cols)
			if len(submits) != 1 || submits[0] != text {
				t.Errorf("Claude would submit %d prompt(s) %q, want exactly [%q]\nbytes written to the pty: %q",
					len(submits), submits, text, stream)
			}
		})
	}
}
