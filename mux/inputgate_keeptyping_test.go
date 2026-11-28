package mux

// A gated typed send writes its text in 32-rune chunks with a readiness check
// before every chunk after the first. One not-ready answer there (a box-less
// frame, not a dialog) must not end the typing: the delivery re-waits for a
// box that holds exactly what was written, then types the rest. When it
// cannot resume, the reply says how far it got and why.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// childBox models the input box as exactly the bytes the child has read: a
// check verifies when want.Exact equals them, and otherwise reports the box
// as holding what the child read. It waits briefly for the drain goroutine,
// so a check made right after a write sees that write.
func childBox(log **byteLog, want BoxExpect) BoxCheck {
	deadline := time.Now().Add(150 * time.Millisecond)
	for {
		got := (*log).String()
		if got == want.Exact {
			return verifiedBox(verifiedExact)
		}
		if !time.Now().Before(deadline) {
			return boxMismatchCheck(got, "the box holds a prefix of what was typed", false)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wireReply serialises a send's outcome as the socket does: the result map
// the send op builds (completed by sendReport.addTo) on success, the error's
// own fields on failure.
func wireReply(t *testing.T, m *Magmux, o sendOutcome) map[string]any {
	t.Helper()
	var result map[string]any
	if o.err == nil {
		result = map[string]any{}
		o.rep.addTo(result)
	}
	var got map[string]any
	if err := json.Unmarshal(m.replyBytes(json.RawMessage(`1`), result, o.err), &got); err != nil {
		t.Fatalf("reply: %v", err)
	}
	return got
}

// wireInt reads an integer key from a decoded reply map; ok is false when the
// key is absent or not a number.
func wireInt(fields map[string]any, key string) (int, bool) {
	f, ok := fields[key].(float64)
	return int(f), ok
}

func TestOneNotReadyBetweenWritesKeepsTyping(t *testing.T) {
	// REGRESSION: a gated typed send stopped typing partway, silently, on one not-ready answer between writes.
	text := strings.Repeat("x", 100) // four typed writes: 32 + 32 + 32 + 4
	writesTotal := len(typedWrites(text))
	if writesTotal != 4 {
		t.Fatalf("typedWrites(100 runes) = %d writes, the test assumes 4", writesTotal)
	}
	var log *byteLog
	g := &fakeGate{
		readyFn: func(_ ReadinessQuery, n int) InputReadiness {
			// 0 gate, 1 post-token, 2 before write 2, 3 before write 3: one
			// box-less frame after two writes reached the child.
			if n == 3 {
				return notReady(protocol.ReasonNoInputBox)
			}
			return readyNow()
		},
		checkFn: func(want BoxExpect, _ int) BoxCheck { return childBox(&log, want) },
	}
	m, _, l := gatedMux(t, g)
	log = l

	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})

	if got := log.settle(); got != text+"\r" {
		t.Errorf("the child received %d bytes %q; want the whole %d-rune text exactly once, then one Enter",
			len(got), got, len(text))
	}
	if o.err != nil {
		t.Fatalf("send: %v; one not-ready answer between writes must not end the typing", o.err)
	}
	if o.rep.verified != verifiedExact {
		t.Errorf("verified = %q, want %q", o.rep.verified, verifiedExact)
	}
	reply := wireReply(t, m, o)
	res, _ := reply["result"].(map[string]any)
	if res == nil {
		t.Fatalf("reply %v has no result", reply)
	}
	if res["verified"] != verifiedExact {
		t.Errorf("result verified = %v, want %q", res["verified"], verifiedExact)
	}
	w, okW := wireInt(res, "writes")
	wt, okT := wireInt(res, "writesTotal")
	if !okW || !okT || w != writesTotal || wt != writesTotal {
		t.Errorf("result writes=%v writesTotal=%v, want both %d: %v", res["writes"], res["writesTotal"], writesTotal, res)
	}
	if _, ok := res["typingStopped"]; ok {
		t.Errorf("an ok result carries typingStopped: %v", res)
	}
}

func TestTypingStoppedAtTheDeadlineIsReported(t *testing.T) {
	// REGRESSION: a gated typed send stopped typing partway, silently, on one not-ready answer between writes.
	text := strings.Repeat("x", 100) // four typed writes
	writesTotal := len(typedWrites(text))
	var log *byteLog
	g := &fakeGate{
		readyFn: func(_ ReadinessQuery, n int) InputReadiness {
			// From the check before write 3 onward the box never comes back.
			if n >= 3 {
				return notReady(protocol.ReasonNoInputBox)
			}
			return readyNow()
		},
		checkFn: func(want BoxExpect, _ int) BoxCheck {
			// A check of a PREFIX (a resume) sees the same box-less screen
			// the readiness check saw. The final verify of the whole text
			// reads the box as the child has it.
			if fromWrite3(text, want) {
				return checkNotReady(notReady(protocol.ReasonNoInputBox))
			}
			return childBox(&log, want)
		},
	}
	m, _, l := gatedMux(t, g)
	log = l

	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true, readyTimeoutMs: 600})

	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if got := log.settle(); strings.Contains(got, "\r") {
		t.Errorf("an Enter was written after typing stopped: %q", got)
	} else if got != text[:64] {
		t.Errorf("the child received %d bytes, want the first two writes (64)", len(got))
	}
	if !strings.Contains(o.err.Error(), "typing stopped") {
		t.Errorf("the error does not say typing stopped: %v", o.err)
	}
	reply := wireReply(t, m, o)
	w, okW := wireInt(reply, "writes")
	wt, okT := wireInt(reply, "writesTotal")
	if !okW || !okT || w != 2 || wt != writesTotal {
		t.Errorf("reply writes=%v writesTotal=%v, want 2 of %d: %v", reply["writes"], reply["writesTotal"], writesTotal, reply)
	}
	if reply["typingStopped"] != "deadline" {
		t.Errorf("reply typingStopped = %v, want \"deadline\": %v", reply["typingStopped"], reply)
	}
}
