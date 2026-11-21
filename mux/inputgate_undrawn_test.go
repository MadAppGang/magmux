package mux

// A box that LOOKS empty is not proof that Claude Code's input buffer is
// empty: under load Claude can have read the first copy of a send and not yet
// drawn it. The one retype then lands BEHIND that copy in the PTY stream, and
// the Enter licensed by the late frame of copy 1 is ordered after copy 2, so
// whatever the frame showed, Claude submits everything that came before it.
// The only defence is in the byte stream itself: something between the copies
// that leaves at most one copy in the buffer at the Enter.

import (
	"strings"
	"testing"
)

// claudeInputModel replays the bytes a pane's child received through a
// sequential model of Claude Code's input box and returns what was submitted.
// A printable rune appends; ctrl-u (0x15) deletes the last wrapped row at
// width cols-4 (one row per key, nothing on an empty buffer); a carriage
// return submits the buffer and empties it. Every other control byte is
// ignored.
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
// every read, so the delivery retypes it. The first copy was READ but not
// drawn: once the second copy is in, the box draws exactly one copy (copy 1,
// late) and the Enter goes in while copy 2 is still queued. Claude must
// submit the text exactly once.
//
// REGRESSION: a retype typed onto text Claude had read but not drawn submitted it twice.
func TestReadButUndrawnTextIsSubmittedOnce(t *testing.T) {
	text := "Please review the authentication configuration in the launcher, " +
		"check the directory key handling, and report every place it is read now."
	if n := len([]rune(text)); n < 130 || n > 160 || strings.ContainsAny(text, "\n\r") || strings.HasPrefix(text, "!") {
		t.Fatalf("test text is %d runes; want ~140, single-line, not bash mode", n)
	}

	// The model itself: one ctrl-u between the copies clears one wrapped row
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
		cause string
		empty func() BoxCheck
	}{
		{"never_drawn", retypeNeverDrawn, boxEmptyCheck},
		{"terminal_reset", retypeTerminalReset, lostBoxCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGate{}
			g.checkFn = func(BoxExpect, int) BoxCheck {
				if strings.Count(g.log.String(), text) < 2 {
					return tc.empty()
				}
				// Copy 1 drawn late; whatever followed it is still queued.
				return verifiedBox(verifiedExact)
			}
			m, p, log := gatedMux(t, g)
			p.mu.Lock()
			cols := p.screen.cols
			p.mu.Unlock()
			if rows := (len([]rune(text)) + cols - 5) / (cols - 4); rows < 3 {
				t.Fatalf("text wraps over %d rows at %d columns; want at least 3", rows, cols)
			}

			o := runSend(t, m, sendRequest{text: text, enter: true})
			stream := log.settle()
			if o.err != nil {
				t.Fatalf("send: %v (bytes %q)", o.err, stream)
			}
			if res := replyOf(o); res["retyped"] != true || res["retypeCause"] != tc.cause {
				t.Fatalf("reply %v: want retyped:true, retypeCause:%q (the retype path is what this test drives)", res, tc.cause)
			}

			submits := claudeInputModel(stream, cols)
			if len(submits) != 1 || submits[0] != text {
				t.Errorf("Claude would submit %d prompt(s) %q, want exactly [%q]\nbytes written to the pty: %q",
					len(submits), submits, text, stream)
			}
		})
	}
}
