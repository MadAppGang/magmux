package mux

// Streaming and typing over a REAL socket, against a real magmux with a real
// shell in a real PTY.
//
// The in-process tests prove the framer and the slot. These prove the thing a
// remote client actually does: watch a pane, type into it, and see the result
// appear on its own screen — across a process boundary, through the op
// registry, the lane, the PTY, the VT parser, the diff and the encoder. Nothing
// smaller than this covers the wiring between them.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// sessionPane is the id of the first pane that is a SESSION rather than
// magmux's own control panel. Panel ids are not stable — every session has a
// panel now, hidden unless -c asked otherwise — and hardcoding 0 would make
// these tests depend on where it landed.
func (r *rpcMagmux) sessionPane(c *rpcConn) int {
	r.t.Helper()
	c.send(map[string]any{"type": "list", "id": "lp"})
	ev, _ := c.awaitReply("lp", nil)
	panes, _ := replyOK(r.t, ev)["panes"].([]any)
	for _, p := range panes {
		pane, _ := p.(map[string]any)
		if state, _ := pane["state"].(string); state == "panel" {
			continue
		}
		if idx, ok := pane["pane"].(float64); ok {
			return int(idx)
		}
	}
	r.t.Fatalf("no session pane in %v", panes)
	return -1
}

// TestWatchAndInputOverTheSocket is the end-to-end case for the whole feature:
// a client watches a shell pane, types a command into it with `call
// {op:"input"}`, and the output comes back as a FRAME on the same connection.
//
// The deadline is the claim. A remote terminal that shows a keystroke's effect
// half a second later is not a terminal, and the whole wake-driven design
// exists so that the delay is one frame interval and not one poll interval.
func TestWatchAndInputOverTheSocket(t *testing.T) {
	mux := startRPCMagmux(t, "-e", "sh")
	c := mux.dial()
	pane := mux.sessionPane(c)

	// Watch first, and wait for the keyframe: a client sizes itself from the
	// watch reply and clears its screen for the keyframe, and only then is it
	// ready to be typed into.
	c.send(map[string]any{"type": "watch", "pane": pane, "fps": 30, "id": "w"})
	ev, _ := c.awaitReply("w", nil)
	res := replyOK(t, ev)
	if fmt.Sprint(res["pane"]) != fmt.Sprint(pane) {
		t.Fatalf("watch reply names pane %v, asked for %d", res["pane"], pane)
	}
	if rows, _ := res["rows"].(float64); rows <= 0 {
		t.Fatalf("watch reply has no geometry: %v", res)
	}

	var sawKey bool
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !sawKey {
		ev, raw, ok := c.next()
		if !ok {
			t.Fatal("the stream ended before the first keyframe")
		}
		if ev["type"] != protocol.EventFrame {
			continue
		}
		var f protocol.Frame
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			t.Fatalf("a frame did not decode as protocol.Frame: %v\n%s", err, raw)
		}
		if f.Pane == pane && f.Key {
			sawKey = true
		}
	}
	if !sawKey {
		t.Fatal("no keyframe for the watched pane")
	}
	// Let the shell finish printing its prompt, so the frame we measure is the
	// one our own input produced.
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	c.send(map[string]any{"type": "call", "op": "input", "id": "i",
		"args": map[string]any{"pane": pane, "text": "echo MAGMUX_RC_OK\r"}})

	var (
		replyAt  time.Duration
		frameAt  time.Duration
		bytes    any
		gotReply bool
	)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ev, raw, ok := c.next()
		if !ok {
			t.Fatal("the stream ended before the echo came back")
		}
		if ev["type"] == protocol.EventReply && fmt.Sprint(ev["id"]) == "i" {
			replyAt = time.Since(started)
			bytes = replyOK(t, ev)["bytes"]
			gotReply = true
			continue
		}
		if ev["type"] != protocol.EventFrame {
			continue
		}
		var f protocol.Frame
		if err := json.Unmarshal([]byte(raw), &f); err != nil || f.Pane != pane {
			continue
		}
		for _, l := range f.Lines {
			// The command line itself is echoed by the terminal; the OUTPUT is
			// the line that proves the shell ran it. Both carry the text, so
			// match on the line that is the text alone.
			if strings.TrimSpace(l.T) == "MAGMUX_RC_OK" {
				frameAt = time.Since(started)
			}
		}
		if frameAt > 0 {
			break
		}
	}
	if !gotReply {
		t.Error("no reply to the input call")
	} else if fmt.Sprint(bytes) != "18" {
		t.Errorf("input reported %v bytes, want 18 (the text plus its CR)", bytes)
	}
	if frameAt == 0 {
		t.Fatal("no frame carrying MAGMUX_RC_OK arrived within 5s")
	}
	t.Logf("input -> reply %v; input -> frame carrying MAGMUX_RC_OK %v", replyAt, frameAt)
	if frameAt > time.Second {
		t.Errorf("the echo took %v to arrive as a frame; a remote terminal must be inside a second", frameAt)
	}
}

// TestConcurrentCallSendIsLaneOrdered closes P2's open item.
//
// The direct `send` verb was already delivered on the connection's lane while
// `call {op:"send"}` still spawned a goroutine per send, so two identical
// requests by two different routes had different ordering guarantees — and the
// `call` one could type the second instruction into the middle of the first.
// Both now go through Sub.Call and take the same lane.
//
// The four calls go out back to back with nothing awaited in between, which is
// what "concurrent" means for a client: they are all in flight at once. Each is
// a shell command, so the shell's own output is the proof — interleaved
// delivery produces a command line that is not any of the four.
func TestConcurrentCallSendIsLaneOrdered(t *testing.T) {
	mux := startRPCMagmux(t, "-e", "sh")
	c := mux.dial()
	pane := mux.sessionPane(c)
	time.Sleep(500 * time.Millisecond) // the shell's first prompt

	const n = 4
	for i := 1; i <= n; i++ {
		c.send(map[string]any{"type": "call", "op": "send", "id": fmt.Sprintf("s%d", i),
			"args": map[string]any{"pane": pane, "text": fmt.Sprintf("echo MAGMUX_ORDER_%d", i)}})
	}
	for i := 1; i <= n; i++ {
		ev, _ := c.awaitReply(fmt.Sprintf("s%d", i), nil)
		replyOK(t, ev)
	}

	// Read the pane back and check the four outputs are there, in order. A
	// send typed into the middle of another produces a command line that is
	// none of the four, so a missing marker is an interleave.
	var text string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.send(map[string]any{"type": "capture", "pane": pane, "id": "cap"})
		ev, _ := c.awaitReply("cap", nil)
		text, _ = replyOK(t, ev)["text"].(string)
		if strings.Count(text, "MAGMUX_ORDER_") >= 2*n {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	at := make([]int, 0, n)
	for i := 1; i <= n; i++ {
		marker := fmt.Sprintf("MAGMUX_ORDER_%d", i)
		// The LAST occurrence is the output line; the first is the echo of the
		// command. Either ordering works as long as the same one is used for
		// every marker.
		idx := strings.LastIndex(text, marker)
		if idx < 0 {
			t.Fatalf("MAGMUX_ORDER_%d never reached the shell; the sends interleaved.\nscreen:\n%s", i, text)
		}
		at = append(at, idx)
	}
	for i := 1; i < len(at); i++ {
		if at[i] < at[i-1] {
			t.Fatalf("send %d landed before send %d; the lane did not order them.\nscreen:\n%s", i+1, i, text)
		}
	}
}
