package mux

// `send`'s two delivery modes: typed, which a TUI must read as keystrokes, and
// paste, which it must read as one paste whatever the text holds.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestTypedWritesNeverSubmitAndStaySmall. Every newline spelling becomes one
// ctrl-j write of its own, no write carries more than typedChunkRunes runes, and
// the writes rebuild the text exactly with its newlines normalised.
func TestTypedWritesNeverSubmitAndStaySmall(t *testing.T) {
	long := strings.Repeat("abcdefghij", 10) // 100 runes, more than one chunk
	text := "first\r\nsecond\rthird\n" + long + "\nunicode ✓ é"
	writes := typedWrites(text)

	var rebuilt strings.Builder
	for i, w := range writes {
		s := string(w)
		if strings.Contains(s, "\r") {
			t.Fatalf("write %d holds a carriage return %q — that is Return, and it submits", i, s)
		}
		if strings.Contains(s, "\n") && s != typedNewline {
			t.Fatalf("write %d mixes a newline into text %q — a newline must be a keystroke of its own", i, s)
		}
		if n := len([]rune(s)); n > typedChunkRunes {
			t.Fatalf("write %d is %d runes, over the %d-rune chunk", i, n, typedChunkRunes)
		}
		rebuilt.WriteString(s)
	}
	want := "first\nsecond\nthird\n" + long + "\nunicode ✓ é"
	if rebuilt.String() != want {
		t.Errorf("the writes rebuild %q, want %q", rebuilt.String(), want)
	}
}

// TestTypedSendIsNeverBracketed. A pane in bracketed-paste mode is exactly where
// the old shape wrapped a multi-line text; typed must not.
func TestTypedSendIsNeverBracketed(t *testing.T) {
	m, p, child := inputMux(t)
	p.mu.Lock()
	p.bracketPaste = true
	p.mu.Unlock()

	done := make(chan error, 1)
	if err := m.sendToPaneVia(nil, nil, p.id, sendTyped, "one\ntwo", nil, false, "", func(err error) { done <- err }); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitDelivered(t, done)
	got := drain(t, child)
	if strings.Contains(got, "\x1b[200~") {
		t.Errorf("a typed send arrived bracketed: %q", got)
	}
	if got != "one\ntwo" {
		t.Errorf("the child saw %q, want %q", got, "one\ntwo")
	}
}

// TestPasteSendIsAlwaysBracketed: one line, and nothing at all — the two shapes
// the old send would have written bare or not at all.
func TestPasteSendIsAlwaysBracketed(t *testing.T) {
	for _, text := range []string{"/tmp/shot.png", ""} {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			m, p, child := inputMux(t)
			p.mu.Lock()
			p.bracketPaste = true
			p.mu.Unlock()

			done := make(chan error, 1)
			if err := m.sendToPaneVia(nil, nil, p.id, sendPaste, text, nil, false, "", func(err error) { done <- err }); err != nil {
				t.Fatalf("send: %v", err)
			}
			waitDelivered(t, done)
			if got, want := drain(t, child), "\x1b[200~"+text+"\x1b[201~"; got != want {
				t.Errorf("the child saw %q, want %q", got, want)
			}
		})
	}
}

// TestPasteSendRefusedWithoutBracketedPaste. Markers the program never asked for
// arrive as keystrokes, so the paste would silently not be one.
func TestPasteSendRefusedWithoutBracketedPaste(t *testing.T) {
	m, p, child := inputMux(t)
	err := m.sendToPaneVia(nil, nil, p.id, sendPaste, "x", nil, false, "", nil)
	if verbErrCode(err) != sockCodeBadRequest {
		t.Fatalf("err = %v, want a bad_request", err)
	}
	if got := drain(t, child); got != "" {
		t.Errorf("a refused paste still wrote %q", got)
	}
}

// TestSendVerbReadsTypedAndPaste: the socket fields reach the mode, and asking
// for both is refused before anything is admitted.
func TestSendVerbReadsTypedAndPaste(t *testing.T) {
	for _, tc := range []struct {
		typed, paste bool
		want         sendMode
		wantErr      bool
	}{
		{false, false, sendAuto, false},
		{true, false, sendTyped, false},
		{false, true, sendPaste, false},
		{true, true, sendAuto, true},
	} {
		got, err := sendModeOf(tc.typed, tc.paste)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("sendModeOf(%v, %v) = %q, %v; want %q, error %v", tc.typed, tc.paste, got, err, tc.want, tc.wantErr)
		}
	}

	var msg sockMsg
	if err := json.Unmarshal([]byte(`{"type":"send","text":"x","typed":true}`), &msg); err != nil || !msg.Typed || msg.Paste {
		t.Errorf("decoded %+v (err %v), want Typed only", msg, err)
	}
}

// waitDelivered blocks until a delivery reports, failing on an error or a hang.
func waitDelivered(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("delivery: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never finished")
	}
}
