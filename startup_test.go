package main

// Startup ordering: what the socket may say before the layout exists.
//
// magmux binds its socket BEFORE it builds any pane, and it has to — the path
// must be in MAGMUX_SOCK for the very first child. Everything served in that
// window is therefore served against an empty m.allPanes, and the damage is not
// limited to a confusing error: the first line handleSocketConn writes is the
// connect-time AGGREGATE snapshot, which is the one and only place a subscriber
// learns about panes it did not watch appear (per-pane snapshots fire on change
// only). A client handed an empty aggregate does not retry; it waits forever.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"
)

// dialASAP connects the instant the listener exists. The 50ms retry in
// rpcMagmux.dial is deliberately not used here: this test's whole subject is
// the window between bind and layout, which is tens of milliseconds wide, so a
// polite poll would step straight over the thing under test.
func dialASAP(t *testing.T, sock string, within time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			t.Cleanup(func() { conn.Close() })
			_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
			return conn
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("socket %s never appeared within %v", sock, within)
	return nil
}

// TestSocketVerbBeforeLayoutReady is the guard on that window.
//
// It connects as fast as the OS allows and sends a verb immediately, which puts
// both halves of the hazard on the same connection: the aggregate snapshot the
// client seeds from, and a verb dispatched against whatever layout existed at
// that moment. Both must describe the real panes.
//
// Run several times because the window is a race, not a state — a single
// attempt that happened to lose it would pass against the broken code and prove
// nothing. Against the unfixed binary this reported an empty aggregate (and
// open_pane answering `no_such_pane`, blaming the caller's index for magmux's
// own startup) within the first attempt or two.
func TestSocketVerbBeforeLayoutReady(t *testing.T) {
	const attempts = 6
	for i := 0; i < attempts; i++ {
		t.Run(fmt.Sprintf("race%d", i), func(t *testing.T) {
			mux := startRPCMagmux(t, "-e", `sh -c "sleep 4"`, "-e", `sh -c "sleep 4"`)

			conn := dialASAP(t, mux.sock, 20*time.Second)
			connectedAt := time.Now()
			req, _ := json.Marshal(map[string]any{"type": "list", "id": "boot"})
			if _, err := conn.Write(append(req, '\n')); err != nil {
				t.Fatalf("write list: %v", err)
			}

			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

			// First line, by contract, is the connect-time aggregate.
			if !sc.Scan() {
				t.Fatalf("connection closed before the aggregate snapshot: %v", sc.Err())
			}
			waited := time.Since(connectedAt)
			var agg map[string]any
			if err := json.Unmarshal(sc.Bytes(), &agg); err != nil {
				t.Fatalf("first line is not JSON (%q): %v", sc.Text(), err)
			}
			if agg["type"] != "snapshot" {
				t.Fatalf("first line is a %v event, want the connect-time snapshot: %s", agg["type"], sc.Text())
			}
			aggPanes, _ := agg["panes"].([]any)
			if len(aggPanes) == 0 {
				t.Fatalf("the connect-time aggregate listed no panes: %s\n"+
					"a subscriber seeds its entire pane map from this line and is only told about "+
					"panes again when they CHANGE, so an empty one strands it forever", sc.Text())
			}
			t.Logf("aggregate with %d panes arrived %v after connect", len(aggPanes), waited)

			// And the verb sent in the same breath.
			for {
				if !sc.Scan() {
					t.Fatalf("connection closed before the reply to the verb sent at connect time: %v", sc.Err())
				}
				var ev map[string]any
				if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
					continue
				}
				if ev["type"] != "reply" || fmt.Sprint(ev["id"]) != "boot" {
					continue
				}
				if ok, _ := ev["ok"].(bool); !ok {
					t.Fatalf("a verb sent at connect time failed: code=%v error=%v\n"+
						"magmux was still starting up; the socket must not serve verbs before the layout exists",
						ev["code"], ev["error"])
				}
				res, _ := ev["result"].(map[string]any)
				panes, _ := res["panes"].([]any)
				if len(panes) < 2 {
					t.Fatalf("list at connect time reported %d panes, want the 2 that were asked for: %v", len(panes), res)
				}
				return
			}
		})
	}
}

// TestSocketReadyWaitDoesNotOutliveMagmux pins the other half of the wait: it
// must never be able to hold a client past magmux's own exit. A wait that only
// watched layoutReady would park a connection for the full timeout after a
// startup failure, and — worse — would sit in front of the finalEvents replay
// that is how a client connecting during teardown still receives results.
func TestSocketReadyWaitDoesNotOutliveMagmux(t *testing.T) {
	m := &Magmux{quit: make(chan struct{}), layoutReady: make(chan struct{})}

	if m.layoutIsReady() {
		t.Fatal("a layout that was never built reports ready")
	}

	done := make(chan bool, 1)
	go func() { done <- m.waitLayoutReady(30 * time.Second) }()
	close(m.quit)
	select {
	case ready := <-done:
		if ready {
			t.Error("the wait reported a layout that does not exist")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitLayoutReady ignored m.quit: teardown would block behind a client that connected at startup")
	}

	// Nil means "no wait": the in-process tests build their layout before
	// anything can look at it, and must not be made to wait for a signal
	// nobody sends.
	var bare Magmux
	if !bare.layoutIsReady() || !bare.waitLayoutReady(time.Millisecond) {
		t.Error("a Magmux with no layoutReady channel must be treated as ready")
	}
}
