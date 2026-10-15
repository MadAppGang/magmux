package mux

// The `resize` op end to end: a real binary, a real socket, a real PTY, and a
// real child that has to SEE the new size. The in-process tests in
// resize_test.go prove the decision; these prove the decision reaches the
// places a client cares about — the child's tty, the watch stream, the
// capabilities reply, HTTP — and that the human's window cannot undo it.
//
// No -w anywhere and no answer read out of an `exit` event (CLAUDE.md: `exit`
// has no delivery guarantee). Teardown is the harnesses' SIGTERM cleanup. Every
// wait is a poll against a deadline, and the one negative claim ("no new
// keyframe") is closed by a positive fence.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/pty"
)

// rpcIDs makes every request id in a test distinct, so a late reply can never
// be mistaken for the one being waited on.
type rpcIDs struct{ n int }

func (ids *rpcIDs) next(prefix string) string {
	ids.n++
	return fmt.Sprintf("%s%d", prefix, ids.n)
}

// ask sends one request and returns its reply event.
func (ids *rpcIDs) ask(rc *rpcConn, v map[string]any) map[string]any {
	rc.t.Helper()
	id := ids.next(fmt.Sprint(v["type"]))
	v["id"] = id
	rc.send(v)
	ev, _ := rc.awaitReply(id, nil)
	return ev
}

// replyCode asserts a refusal and returns its code.
func replyCode(t *testing.T, ev map[string]any) string {
	t.Helper()
	if ok, _ := ev["ok"].(bool); ok {
		t.Fatalf("expected a refusal, got ok: %v", ev)
	}
	code, _ := ev["code"].(string)
	return code
}

// wireResult checks the four contract fields of a decoded reply.
func wireResult(t *testing.T, what string, res map[string]any, rows, cols int, changed, manual bool) {
	t.Helper()
	got := fmt.Sprintf("%v %v %v %v", res["rows"], res["cols"], res["changed"], res["manual"])
	want := fmt.Sprintf("%d %d %v %v", rows, cols, changed, manual)
	if got != want {
		t.Fatalf("%s: rows/cols/changed/manual = %s, want %s (%v)", what, got, want, res)
	}
}

// capsSize reads capabilities' rows/cols.
func (ids *rpcIDs) capsSize(rc *rpcConn) (int, int) {
	rc.t.Helper()
	res := replyOK(rc.t, ids.ask(rc, map[string]any{"type": "capabilities"}))
	r, _ := res["rows"].(float64)
	c, _ := res["cols"].(float64)
	return int(r), int(c)
}

// awaitCaptureLine polls `capture` until some row of pane's screen is exactly
// want (trimmed), and returns the whole screen it saw.
func (ids *rpcIDs) awaitCaptureLine(rc *rpcConn, pane int, want string, within time.Duration) string {
	rc.t.Helper()
	var text string
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		res := replyOK(rc.t, ids.ask(rc, map[string]any{"type": "capture", "pane": pane}))
		text, _ = res["text"].(string)
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == want {
				return text
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	rc.t.Fatalf("pane %d never showed a line %q within %v; last screen:\n%s", pane, want, within, text)
	return ""
}

// TestResizeReachesTheChild is criterion 1, in claudish's own shape: headless,
// --no-status, one -e, COLUMNS=160 LINES=50. The child traps WINCH and prints
// `stty size`, so a "40 100" on its screen is the child itself reporting a
// real SIGWINCH and the tty's new size — not magmux's opinion of it.
func TestResizeReachesTheChild(t *testing.T) {
	t.Setenv("COLUMNS", "160")
	t.Setenv("LINES", "50")
	script := filepath.Join(t.TempDir(), "winch.sh")
	body := "trap 'stty size' WINCH\nstty size\nwhile :; do sleep 0.1; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	h := startHeadlessMagmux(t, "--headless", "--no-status", "-e", "sh "+script)
	conn := dialASAP(t, h.sock, 10*time.Second)
	rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}
	ids := &rpcIDs{}

	ids.awaitCaptureLine(rc, 0, "50 160", 15*time.Second)

	res := replyOK(t, ids.ask(rc, map[string]any{"type": "resize", "pane": 0, "rows": 40, "cols": 100}))
	wireResult(t, "resize", res, 40, 100, true, true)

	if r, c := ids.capsSize(rc); r != 40 || c != 100 {
		t.Fatalf("capabilities reports %dx%d after the resize, want 40x100", r, c)
	}
	var pane0 map[string]any
	for _, p := range listPanes(t, rc, ids.next("list")) {
		if fmt.Sprint(p["pane"]) == "0" {
			pane0 = p
		}
	}
	if fmt.Sprint(pane0["rows"], "x", pane0["cols"]) != "40x100" {
		t.Fatalf("list reports pane 0 as %vx%v, want 40x100: %v", pane0["rows"], pane0["cols"], pane0)
	}
	before := ids.awaitCaptureLine(rc, 0, "40 100", 10*time.Second)

	// Criterion 5's spot check on the real binary: refusals answer with their
	// code and leave the size and the screen alone.
	if code := replyCode(t, ids.ask(rc, map[string]any{"type": "resize", "pane": 0, "rows": 2, "cols": 100})); code != protocol.CodeTooSmall {
		t.Fatalf("rows:2 refused %q, want too_small", code)
	}
	if code := replyCode(t, ids.ask(rc, map[string]any{"type": "resize", "rows": "x", "cols": 100})); code != protocol.CodeBadRequest {
		t.Fatalf(`"rows":"x" refused %q, want bad_request`, code)
	}
	if code := replyCode(t, ids.ask(rc, map[string]any{"type": "resize", "rows": 10001, "cols": 100})); code != protocol.CodeTooLarge {
		t.Fatalf("rows:10001 refused %q, want too_large", code)
	}
	if r, c := ids.capsSize(rc); r != 40 || c != 100 {
		t.Fatalf("a refused resize moved the screen to %dx%d", r, c)
	}
	after := replyOK(t, ids.ask(rc, map[string]any{"type": "capture", "pane": 0}))
	if after["text"] != before || fmt.Sprint(after["rows"], "x", after["cols"]) != "40x100" {
		t.Fatalf("a refused resize changed pane 0:\n--- before\n%s\n--- after (%vx%v)\n%s",
			before, after["rows"], after["cols"], after["text"])
	}

	// Criterion 6: advertised, as control, and the protocol did not move.
	caps := replyOK(t, ids.ask(rc, map[string]any{"type": "capabilities"}))
	if fmt.Sprint(caps["protocol"]) != "1" {
		t.Fatalf("protocol is %v, want 1", caps["protocol"])
	}
	verbs, _ := caps["verbs"].([]any)
	if !strings.Contains(fmt.Sprint(verbs), "resize") {
		t.Fatalf("capabilities.verbs %v has no resize", verbs)
	}
	ops, _ := replyOK(t, ids.ask(rc, map[string]any{"type": "ops"}))["ops"].([]any)
	found := false
	for _, o := range ops {
		op, _ := o.(map[string]any)
		if op["name"] == "resize" {
			found = true
			if op["class"] != string(protocol.ClassControl) {
				t.Fatalf("ops lists resize as class %v, want control", op["class"])
			}
		}
	}
	if !found {
		t.Fatalf("ops does not list resize: %v", ops)
	}
}

// TestResizeKeyframeOverWatch is criteria 2 and 3 on the watch stream: a
// resize reaches a frames-mode watcher as a KEYFRAME at the new geometry, and
// a same-size resize produces none.
//
// "None" is closed by a fence: after the no-op, type a marker into the pane and
// read until a frame carries it. The framer delivers one pane's frames in
// order, so a keyframe caused by the no-op would have to arrive before it.
func TestResizeKeyframeOverWatch(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	t.Setenv("LINES", "40")
	h := startHeadlessMagmux(t, "--headless", "--no-status", "-e", "sh")
	conn := dialASAP(t, h.sock, 10*time.Second)
	rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}
	ids := &rpcIDs{}

	replyOK(t, ids.ask(rc, map[string]any{"type": "watch", "pane": 0, "mode": "frames", "fps": 30}))

	// frame decodes ev as a frame for pane 0, or reports false.
	frame := func(ev map[string]any, raw string) (protocol.Frame, bool) {
		var f protocol.Frame
		if ev["type"] != protocol.EventFrame {
			return f, false
		}
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			t.Fatalf("a frame did not decode: %v\n%s", err, raw)
		}
		return f, f.Pane == 0
	}
	// On failure, say which step stalled and what the stream carried last: a
	// bare read timeout names neither.
	var step string
	var seen []string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("stalled waiting for %s; last %d events:", step, len(seen))
			for _, s := range seen {
				t.Logf("  %s", s)
			}
		}
	})
	// readUntil hands every event to fn until fn says stop.
	readUntil := func(what string, within time.Duration, fn func(ev map[string]any, raw string) bool) {
		t.Helper()
		step = what
		deadline := time.Now().Add(within)
		_ = rc.c.SetReadDeadline(deadline)
		for time.Now().Before(deadline) {
			ev, raw, ok := rc.next()
			short := raw
			if len(short) > 300 {
				short = short[:300] + "…"
			}
			if seen = append(seen, short); len(seen) > 8 {
				seen = seen[1:]
			}
			if !ok {
				t.Fatalf("the stream ended waiting for %s", what)
			}
			if fn(ev, raw) {
				return
			}
		}
		t.Fatalf("no %s within %v", what, within)
	}

	readUntil("the first keyframe", 10*time.Second, func(ev map[string]any, raw string) bool {
		f, ok := frame(ev, raw)
		return ok && f.Key
	})

	// A real resize: a reply saying changed, and a KEYFRAME at 30x90. A delta
	// at the new geometry would be a client applying rows to a screen of the
	// wrong shape, so the first 30x90 frame must be the key.
	rc.send(map[string]any{"type": "resize", "rows": 30, "cols": 90, "id": "rz1"})
	var gotReply, gotKey bool
	readUntil("the resize reply and its keyframe", 10*time.Second, func(ev map[string]any, raw string) bool {
		if ev["type"] == protocol.EventReply && fmt.Sprint(ev["id"]) == "rz1" {
			wireResult(t, "resize", replyOK(t, ev), 30, 90, true, true)
			gotReply = true
		}
		if f, ok := frame(ev, raw); ok && f.Rows == 30 && f.Cols == 90 && !gotKey {
			if !f.Key {
				t.Fatalf("the first frame at 30x90 is a delta, not a keyframe: %s", raw)
			}
			gotKey = true
		}
		return gotReply && gotKey
	})

	// The same size again: changed:false, and from here to the fence there
	// must be no keyframe at all.
	keysAfter := 0
	countKeys := func(ev map[string]any, raw string) {
		if f, ok := frame(ev, raw); ok && f.Key {
			keysAfter++
		}
	}
	rc.send(map[string]any{"type": "resize", "rows": 30, "cols": 90, "id": "rz2"})
	readUntil("the same-size reply", 10*time.Second, func(ev map[string]any, raw string) bool {
		countKeys(ev, raw)
		if ev["type"] == protocol.EventReply && fmt.Sprint(ev["id"]) == "rz2" {
			wireResult(t, "same size", replyOK(t, ev), 30, 90, false, true)
			return true
		}
		return false
	})
	rc.send(map[string]any{"type": "call", "op": "input", "id": "fence",
		"args": map[string]any{"pane": 0, "text": "echo FENCE_RZ\r"}})
	readUntil("the fence", 10*time.Second, func(ev map[string]any, raw string) bool {
		countKeys(ev, raw)
		f, ok := frame(ev, raw)
		if !ok {
			return false
		}
		for _, l := range f.Lines {
			if strings.TrimSpace(l.T) == "FENCE_RZ" {
				return true
			}
		}
		return false
	})
	if keysAfter != 0 {
		t.Fatalf("a same-size resize produced %d keyframe(s); it must touch nothing", keysAfter)
	}
}

// TestResizeSurvivesSIGWINCH is criterion 4 on a real terminal: the human's
// window changing size does not undo a client's size. A smaller window clips
// it while it cannot be shown; a larger one gives it back; {"auto":true}
// returns to the terminal and says manual:false.
func TestResizeSurvivesSIGWINCH(t *testing.T) {
	mux := startRPCMagmux(t, "-e", "sh") // a 24x100 terminal, status row on
	rc := mux.dial()
	ids := &rpcIDs{}

	awaitSize := func(what string, done func(r, c int) bool) (int, int) {
		t.Helper()
		var r, c int
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if r, c = ids.capsSize(rc); done(r, c) {
				return r, c
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: capabilities still says %dx%d after 10s", what, r, c)
		return r, c
	}
	awaitSize("the boot size", func(r, c int) bool { return r == 24 && c == 100 })

	res := replyOK(t, ids.ask(rc, map[string]any{"type": "resize", "rows": 20, "cols": 80}))
	wireResult(t, "resize", res, 20, 80, true, true)

	// Interactive: bigger than the terminal has nowhere to be painted.
	if code := replyCode(t, ids.ask(rc, map[string]any{"type": "resize", "rows": 25, "cols": 100})); code != protocol.CodeTooLarge {
		t.Fatalf("25x100 on a 24x100 terminal refused %q, want too_large", code)
	}

	// A smaller window clips the manual size. Seeing 18x70 is also the proof
	// that magmux processed the SIGWINCH at all.
	pty.SetWinSize(mux.master, 18, 70)
	awaitSize("the clip", func(r, c int) bool { return r == 18 && c == 70 })

	// A larger window gives it back — 20x80, not the window's 30x120, which is
	// what an undone manual mode would read.
	pty.SetWinSize(mux.master, 30, 120)
	if r, c := awaitSize("the window growing", func(r, c int) bool { return r != 18 || c != 70 }); r != 20 || c != 80 {
		t.Fatalf("after the window grew to 30x120 the screen is %dx%d; the manual 20x80 should be back", r, c)
	}

	res = replyOK(t, ids.ask(rc, map[string]any{"type": "resize", "auto": true}))
	wireResult(t, "auto", res, 30, 120, true, false)
	if r, c := ids.capsSize(rc); r != 30 || c != 120 {
		t.Fatalf("after auto the screen is %dx%d, want the terminal's 30x120", r, c)
	}
}

// TestResizeOpOverHTTP: the hub op reaches HTTP with the one status map, and
// the view token cannot change everybody's screen.
func TestResizeOpOverHTTP(t *testing.T) {
	dir := sockTestDir(t)
	viewTok, _ := auth.Generate()
	viewPath := filepath.Join(dir, "view.token")
	if err := os.WriteFile(viewPath, []byte(viewTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := startRemoteMagmux(t, "--view-token-file", viewPath, "-e", "sh")

	resp, body := m.req(t, "POST", "/v1/ops/resize", viewTok, `{"rows":30,"cols":90}`)
	if resp.StatusCode != http.StatusForbidden || body["code"] != protocol.CodeForbidden {
		t.Fatalf("resize with the view token = %d %v, want 403 forbidden", resp.StatusCode, body)
	}

	resp, body = m.req(t, "POST", "/v1/ops/resize", m.token, `{"rows":30,"cols":90}`)
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("resize with the session token = %d %v, want 200", resp.StatusCode, body)
	}
	res, _ := body["result"].(map[string]any)
	wireResult(t, "http", res, 30, 90, true, true)

	for _, tc := range []struct {
		args   string
		status int
		code   string
	}{
		{`{"rows":1,"cols":1}`, http.StatusBadRequest, protocol.CodeTooSmall},
		{`{"rows":10001,"cols":80}`, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge},
		{`{"rows":"30","cols":90}`, http.StatusBadRequest, protocol.CodeBadRequest},
		{`{"pane":99,"rows":30,"cols":90}`, http.StatusNotFound, protocol.CodeNoSuchPane},
	} {
		resp, body := m.req(t, "POST", "/v1/ops/resize", m.token, tc.args)
		if resp.StatusCode != tc.status || body["code"] != tc.code {
			t.Errorf("%s = %d %v, want %d %s", tc.args, resp.StatusCode, body["code"], tc.status, tc.code)
		}
	}
}
