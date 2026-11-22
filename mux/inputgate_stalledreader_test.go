package mux

// A box that stays empty after typing does not prove the text was lost. Under
// a concurrent boot Claude Code can draw its box (so the gate opens) and then
// not read stdin at all for many seconds: the first copy is still UNREAD in
// the pty. Anything magmux writes after it — a clear, a second copy — queues
// behind it, and when Claude resumes it reads the whole backlog in one large
// read. Inside a read of 64 bytes or more it ignores control bytes (ctrl-u
// among them), so the clear does nothing and the copies land joined. magmux
// therefore never types a second copy on its own.

import (
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/protocol"
)

// stalledReaderChunk is the read size from which Claude Code ignores the
// control bytes inside a read.
const stalledReaderChunk = 64

// stalledReaderModel replays stream as Claude Code reads it after a stall:
// it has drawn the box and read nothing of the send, then reads the whole
// backlog in one chunk. A chunk of stalledReaderChunk bytes or more loses
// every byte below 0x20 except tab, LF and CR. The result goes through the
// sequential claudeInputModel, and what is left in the box is returned.
func stalledReaderModel(stream string, cols int) string {
	chunk := stream
	if len(chunk) >= stalledReaderChunk {
		var b strings.Builder
		for i := 0; i < len(chunk); i++ {
			c := chunk[i]
			if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
				continue
			}
			b.WriteByte(c)
		}
		chunk = b.String()
	}
	submits := claudeInputModel(chunk+"\r", cols)
	return submits[len(submits)-1]
}

// TestStalledReaderNeverGetsASecondCopy: the box is drawn and stays empty on
// every read for the whole send, because Claude is not reading stdin. The
// send must type the text once, write no clear and no Enter, and fail
// not_accepted / text_mismatch saying the box stayed empty and nothing was
// retyped.
//
// REGRESSION: a gated send's auto-retype stacked prompt copies behind text Claude Code had not read yet.
func TestStalledReaderNeverGetsASecondCopy(t *testing.T) {
	text := "Summarise the open review comments on the launcher branch"
	if n := len([]rune(text)); n < 40 || n > 60 || strings.ContainsAny(text, "\n\r") || strings.HasPrefix(text, "!") {
		t.Fatalf("test text is %d runes; want 40-60, single-line, not bash mode", n)
	}

	// The model itself: a short backlog is read with its control bytes, so a
	// clear between two copies works; a backlog of 64 bytes or more loses
	// them, and two copies land joined.
	t.Run("model", func(t *testing.T) {
		const cols = 60
		if got := stalledReaderModel("hi\x15hi", cols); got != "hi" {
			t.Errorf("short backlog left %q, want %q (the clear is read)", got, "hi")
		}
		clear := strings.Repeat("\x15", retypeClearKeys(text, retypeClearCols))
		long := text + clear + text
		if len(long) < stalledReaderChunk {
			t.Fatalf("copy+clear+copy is %d bytes; want at least %d", len(long), stalledReaderChunk)
		}
		if got := stalledReaderModel(long, cols); got != text+text {
			t.Errorf("long backlog left %q, want the two copies joined %q", got, text+text)
		}
		if got := stalledReaderModel(text, cols); got != text {
			t.Errorf("one copy left %q, want %q", got, text)
		}
	})

	for _, tc := range []struct {
		name   string
		empty  func() BoxCheck
		detail []string
	}{
		{"never_drawn", boxEmptyCheck, []string{"stayed empty", "not retyped", "never drawn"}},
		{"terminal_reset", lostBoxCheck, []string{"stayed empty", "not retyped", "reset"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The stall: Claude reads nothing, so the box is empty on every
			// read for the whole send, whatever was written.
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return tc.empty() }}
			m, p, log := gatedMux(t, g)
			p.mu.Lock()
			cols := p.screen.cols
			p.mu.Unlock()

			o := runSend(t, m, sendRequest{text: text, enter: true})
			stream := log.settle()

			if n := strings.Count(stream, text); n != 1 {
				t.Errorf("the pty got the text %d times, want exactly once\nbytes: %q", n, stream)
			}
			if strings.ContainsRune(stream, 0x15) {
				t.Errorf("the pty got a ctrl-u clear; a stalled reader ignores it inside a large read\nbytes: %q", stream)
			}
			if strings.Contains(stream, "\r") {
				t.Errorf("the pty got an Enter for a box that never verified\nbytes: %q", stream)
			}
			if box := stalledReaderModel(stream, cols); box != text {
				t.Errorf("after the stall Claude's box holds %q, want one copy %q\nbytes: %q", box, text, stream)
			}
			if res := replyOf(o); res["retyped"] == true {
				t.Errorf("reply %v: want no retyped:true", res)
			}

			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			for _, w := range tc.detail {
				if !strings.Contains(o.err.Error(), w) {
					t.Errorf("error %q does not contain %q", o.err, w)
				}
			}
		})
	}
}
