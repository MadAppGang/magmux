package mux

// The view socket: --view-sock binds a second unix socket beside the control
// one, and every connection on it is a READ-ONLY caller — the same rule a
// --listen view token gets. These tests drive a real binary over both sockets:
// the viewer can read and watch, every steering request is refused
// `forbidden`/`read_only`, and the control socket is untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// viewSockOf is where --view-sock binds for a pid-named session.
func viewSockOf(h *headlessMagmux) string {
	return strings.TrimSuffix(h.sock, ".sock") + ".view"
}

// requireReadOnlyRefusal asserts one refusal is the documented one.
func requireReadOnlyRefusal(t *testing.T, what string, ev map[string]any) {
	t.Helper()
	if code := replyCode(t, ev); code != protocol.CodeForbidden {
		t.Fatalf("%s on the view socket: code %q, want %q (%v)", what, code, protocol.CodeForbidden, ev)
	}
	if ev["reason"] != protocol.ReasonReadOnly {
		t.Fatalf("%s on the view socket: reason %v, want %q (%v)", what, ev["reason"], protocol.ReasonReadOnly, ev)
	}
}

func TestViewSocketIsReadOnly(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless", "--no-status", "--view-sock", "-e", "sh")
	ctrlConn := dialASAP(t, h.sock, 10*time.Second)
	ctrl := &rpcConn{t: t, c: ctrlConn, sc: newLineScanner(ctrlConn)}
	viewConn := dialASAP(t, viewSockOf(h), 10*time.Second)
	view := &rpcConn{t: t, c: viewConn, sc: newLineScanner(viewConn)}
	ids := &rpcIDs{}

	// The viewer is told what it is, and the control client is told where the
	// view socket is.
	caps := replyOK(t, ids.ask(view, map[string]any{"type": "capabilities"}))
	if caps["readOnly"] != true {
		t.Fatalf("view capabilities.readOnly = %v, want true", caps["readOnly"])
	}
	if caps["viewSocket"] != viewSockOf(h) {
		t.Fatalf("view capabilities.viewSocket = %v, want %s", caps["viewSocket"], viewSockOf(h))
	}
	ccaps := replyOK(t, ids.ask(ctrl, map[string]any{"type": "capabilities"}))
	if ccaps["readOnly"] != false || ccaps["viewSocket"] != viewSockOf(h) {
		t.Fatalf("control capabilities readOnly=%v viewSocket=%v", ccaps["readOnly"], ccaps["viewSocket"])
	}

	// Reads work.
	replyOK(t, ids.ask(view, map[string]any{"type": "list"}))
	replyOK(t, ids.ask(view, map[string]any{"type": "ops"}))
	replyOK(t, ids.ask(view, map[string]any{"type": "capture", "pane": 0}))
	replyOK(t, ids.ask(view, map[string]any{"type": "call", "op": "list"}))
	// transcript is a read: on a pane with no controller it gets the same
	// answer it gets on the control socket, never forbidden.
	if code := replyCode(t, ids.ask(view, map[string]any{"type": "transcript", "pane": 0})); code != sockCodeNoController {
		t.Fatalf("transcript on the view socket: code %q, want %q", code, sockCodeNoController)
	}
	replyOK(t, ids.ask(view, map[string]any{"type": "watch", "pane": 0, "mode": "frames", "fps": 30}))

	// Every steering request is refused with the one documented answer.
	refused := []map[string]any{
		{"type": "send", "pane": 0, "text": "echo VIEWLEAK"},
		{"type": "resize", "rows": 30, "cols": 100},
		{"type": "close_pane", "pane": 0},
		{"type": "open_pane", "command": "sh"},
		{"type": "focus", "pane": 0},
		{"type": "tint", "pane": 0, "color": "red"},
		{"type": "status", "text": "x"},
		{"type": "overlay", "pane": 0, "text": "x"},
		{"type": "pilot", "action": "start"},
		{"type": "agent", "pane": 0, "event": "Stop"},
		{"type": "call", "op": "send", "args": map[string]any{"pane": 0, "text": "echo VIEWLEAK"}},
		{"type": "call", "op": "input", "args": map[string]any{"pane": 0, "data": "echo VIEWLEAK\r"}},
		{"type": "call", "op": "resize", "args": map[string]any{"rows": 30, "cols": 100}},
		{"type": "plugin.register", "name": "evil", "token": "x", "ops": []any{}},
	}
	for _, req := range refused {
		what := fmt.Sprint(req["type"], " ", req["op"])
		requireReadOnlyRefusal(t, what, ids.ask(view, req))
	}
	// A no-id request is refused in silence, like every no-id failure.
	view.send(map[string]any{"type": "send", "pane": 0, "text": "echo VIEWLEAK"})

	// The control client still drives the pane, and the viewer sees it.
	replyOK(t, ids.ask(ctrl, map[string]any{"type": "send", "pane": 0, "text": "echo CTRL$((40+2))"}))
	screen := ids.awaitCaptureLine(view, 0, "CTRL42", 10*time.Second)
	if strings.Contains(screen, "VIEWLEAK") {
		t.Fatalf("a refused viewer request reached the pane:\n%s", screen)
	}

	// The viewer's watch carries frames of the live screen. The capture polls
	// above read past every event that was not their reply, frames included,
	// and a pane that has gone quiet sends no more: so ask for a keyframe with
	// resync, which is itself a viewer verb, and read its reply in the same
	// loop because the keyframe may arrive on either side of it.
	resyncID := ids.next("resync")
	view.send(map[string]any{"type": "resync", "pane": 0, "id": resyncID})
	deadline := time.Now().Add(10 * time.Second)
	_ = view.c.SetReadDeadline(deadline)
	for {
		ev, raw, ok := view.next()
		if !ok || time.Now().After(deadline) {
			t.Fatal("the view socket's watch delivered no frame")
		}
		if ev["type"] == "reply" && fmt.Sprint(ev["id"]) == resyncID && ev["ok"] != true {
			t.Fatalf("resync on the view socket was refused: %s", raw)
		}
		if ev["type"] != protocol.EventFrame {
			continue
		}
		var f protocol.Frame
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			t.Fatalf("frame did not decode: %v", err)
		}
		if f.Pane == 0 {
			break
		}
	}

	// The control socket is unchanged: the resize the viewer was refused is
	// still the control client's to make.
	replyOK(t, ids.ask(ctrl, map[string]any{"type": "resize", "rows": 30, "cols": 100}))

	// Both sockets are removed on an ordinary exit.
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	h.wait(10 * time.Second)
	for _, p := range []string{h.sock, viewSockOf(h)} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after exit (err=%v)", p, err)
		}
	}
}

// Without the flag nothing extra is bound and nothing changes for a client.
func TestNoViewSocketWithoutTheFlag(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless", "--no-status", "-e", "sh")
	conn := dialASAP(t, h.sock, 10*time.Second)
	rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}
	ids := &rpcIDs{}
	caps := replyOK(t, ids.ask(rc, map[string]any{"type": "capabilities"}))
	if _, ok := caps["viewSocket"]; ok {
		t.Fatalf("capabilities names a view socket without --view-sock: %v", caps["viewSocket"])
	}
	if _, err := os.Lstat(viewSockOf(h)); !os.IsNotExist(err) {
		t.Fatalf("a view socket exists without --view-sock (err=%v)", err)
	}
}

// A view socket that cannot be bound stops magmux at startup, with its own
// stderr line, and takes the control socket with it.
func TestViewSocketBindFailureIsFatal(t *testing.T) {
	bin := magmuxBinForTest(t)
	dir := sockTestDir(t)
	// A non-empty DIRECTORY at the view path: the stale-file remove fails and
	// so does the bind.
	blocker := filepath.Join(dir, "magmux-vfail.view")
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := spawnHeadlessMagmux(t, bin, dir, nil,
		"--sock-dir", dir, "--id", "vfail", "--headless", "--view-sock", "-e", "sh")
	if code := h.wait(10 * time.Second); code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "magmux: view socket "+blocker) {
		t.Fatalf("stderr does not name the view socket: %q", h.stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(dir, "magmux-vfail.sock")); !os.IsNotExist(err) {
		t.Fatalf("the control socket outlived a failed start (err=%v)", err)
	}
}

// The viewer's rule is the view token's rule, decided by auth.Store: a BUILT-IN
// read op, or a plugin op --view-op granted, and nothing else — a plugin
// labelling its own op `read` does not reach a viewer.
func TestViewerGateFollowsTheViewTokenRule(t *testing.T) {
	m := &Magmux{viewGrants: auth.NewStore("", "", []string{"p.granted"})}
	if err := m.bus().Register("p",
		hub.Op{Spec: protocol.OpSpec{Name: "p.granted", Class: protocol.ClassControl}, Fn: nopOp},
		hub.Op{Spec: protocol.OpSpec{Name: "p.selfread", Class: protocol.ClassRead}, Fn: nopOp},
		hub.Op{Spec: protocol.OpSpec{Name: "p.other", Class: protocol.ClassControl}, Fn: nopOp},
	); err != nil {
		t.Fatal(err)
	}
	viewer := hub.Caller{Transport: "socket", ReadOnly: true}
	cases := []struct {
		msg   sockMsg
		ok    bool
		grant bool
	}{
		{sockMsg{Type: "list"}, true, false},
		{sockMsg{Type: "capture"}, true, false},
		{sockMsg{Type: "transcript"}, true, false},
		{sockMsg{Type: "watch"}, true, false},
		{sockMsg{Type: "unwatch"}, true, false},
		{sockMsg{Type: "resync"}, true, false},
		{sockMsg{Type: "call", Op: "capture"}, true, false},
		{sockMsg{Type: "call", Op: "p.granted"}, true, true},
		{sockMsg{Type: "call", Op: "p.selfread"}, false, false},
		{sockMsg{Type: "call", Op: "p.other"}, false, false},
		{sockMsg{Type: "send"}, false, false},
		{sockMsg{Type: "pilot"}, false, false},
		{sockMsg{Type: "agent"}, false, false},
	}
	for _, c := range cases {
		c.msg.caller = viewer
		grant, err := m.viewerGate(c.msg)
		if (err == nil) != c.ok || grant != c.grant {
			t.Errorf("%s %s: grant=%v err=%v, want ok=%v grant=%v", c.msg.Type, c.msg.Op, grant, err, c.ok, c.grant)
		}
		if err != nil && (protocol.CodeOf(err) != protocol.CodeForbidden) {
			t.Errorf("%s %s: code %s, want forbidden", c.msg.Type, c.msg.Op, protocol.CodeOf(err))
		}
	}
	// A control-socket caller is never gated.
	if _, err := m.viewerGate(sockMsg{Type: "send", caller: hub.Caller{Transport: "socket"}}); err != nil {
		t.Errorf("control caller gated: %v", err)
	}
}

func nopOp(context.Context, hub.Caller, json.RawMessage) (map[string]any, error) { return nil, nil }
