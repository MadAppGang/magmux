package mux

// ADR-X5: in a magmux started with MAGMUX_AGENTS=1 the control panel is
// on-box only, for the whole life of the process.
//
// The panel's attention view lists every Claude Code session on the machine,
// and the `agents` document it is drawn from is socket-only. Every pane read of
// the panel — watch, watch_all, capture, transcript, and any ClassRead op added
// later that takes a pane — is therefore refused to a caller whose transport is
// not the local socket, with panel_private.
//
// Nothing here reads ~/.claude, probes a pid or speaks to a real magmux.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/protocol"
)

// offBoxTransports is every transport that is not the local socket, plus the
// zero Caller, which must fail closed.
var offBoxTransports = []string{"http", "ws", "sse", "firebase", ""}

// privatePanelMux builds a magmux with one session pane (id 0) and the control
// panel (id 1), and the panel's privacy as asked. It is a struct literal, as
// every test magmux is, so panelPrivate is set exactly the way Main sets it: on
// the literal, before anything that could serve a read exists.
func privatePanelMux(t *testing.T, private bool) (m *Magmux, session, panel *Pane) {
	t.Helper()
	m = &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel(),
		panelPrivate: private}
	session = newScrollPane(20, 60)
	panel = newControlPane(0, 61, 20, 59, "panel")
	m.root = session
	m.focused = session
	m.allPanes = []*Pane{session, panel}
	m.stampPaneIDs()
	m.control.attach(panel)
	m.panel = panel
	return m, session, panel
}

// callOp runs one op through the hub as caller c and returns its error code
// ("" on success).
func callOp(t *testing.T, m *Magmux, transport, op string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = m.bus().Call(ctx, hub.Caller{Transport: transport, Conn: "x5"}, op, raw)
	if err == nil {
		return ""
	}
	return protocol.CodeOf(err)
}

// watchCode watches one pane from a fresh Sub on transport, and returns the
// refusal's code ("" when the watch was accepted).
func watchCode(t *testing.T, m *Magmux, transport string, pane int) string {
	t.Helper()
	m.streamer()
	sub := m.bus().Session(hub.Caller{Transport: transport, Conn: "x5"}, &capSink{}, nil)
	sub.Start(nil)
	defer sub.Close("test over")
	_, err := sub.Watch(pane, protocol.WatchFrames, 10)
	if err == nil {
		return ""
	}
	return protocol.CodeOf(err)
}

// panelReadsAreRefusedOffBox asserts the whole rule once: capture and
// transcript ops and a watch of the panel are refused to every off-box caller,
// and a socket caller is not refused for privacy.
func panelReadsAreRefusedOffBox(t *testing.T, m *Magmux, panelID int, when string) {
	t.Helper()
	for _, tr := range offBoxTransports {
		for _, op := range []string{"capture", "transcript"} {
			if code := callOp(t, m, tr, op, map[string]any{"pane": panelID}); code != protocol.CodePanelPrivate {
				t.Errorf("%s: %s of the panel by transport %q answered %q, want %q",
					when, op, tr, code, protocol.CodePanelPrivate)
			}
		}
		if code := watchCode(t, m, tr, panelID); code != protocol.CodePanelPrivate {
			t.Errorf("%s: a watch of the panel by transport %q answered %q, want %q",
				when, tr, code, protocol.CodePanelPrivate)
		}
	}
	if code := callOp(t, m, "socket", "capture", map[string]any{"pane": panelID}); code != "" {
		t.Errorf("%s: a socket capture of the panel answered %q; the socket is on-box", when, code)
	}
	if code := watchCode(t, m, "socket", panelID); code != "" {
		t.Errorf("%s: a socket watch of the panel answered %q; the socket is on-box", when, code)
	}
}

// TestThePanelIsPrivateForTheLifetimeOfAnObservingMagmux: the privacy is a fact
// about the PROCESS, not about the pipeline. It holds before the first
// document, while observation runs, after it stops, and when it never started
// at all — which is the owner's AQ2 wording taken literally.
func TestThePanelIsPrivateForTheLifetimeOfAnObservingMagmux(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	t.Run("an observing magmux", func(t *testing.T) {
		m, session, panel := privatePanelMux(t, true)

		panelReadsAreRefusedOffBox(t, m, panel.id, "before observation starts")

		dir := t.TempDir()
		body := `{"sessionId":"id-a","pid":4242,"name":"a","cwd":"/tmp/p","status":"busy",` +
			`"statusUpdatedAt":1789286000000,"updatedAt":1789286000000}`
		if err := os.WriteFile(filepath.Join(dir, "4242.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("writing the record: %v", err)
		}
		if err := m.startObservation(dir); err != nil {
			t.Fatalf("startObservation: %v", err)
		}
		panelReadsAreRefusedOffBox(t, m, panel.id, "while observing")

		m.stopObservation()
		panelReadsAreRefusedOffBox(t, m, panel.id, "after observation stopped")

		// A session pane is NOT the panel and is never refused for privacy.
		for _, tr := range offBoxTransports[:4] {
			if code := callOp(t, m, tr, "capture", map[string]any{"pane": session.id}); code != "" {
				t.Errorf("a %s capture of a session pane answered %q; only the panel is private", tr, code)
			}
		}
	})

	t.Run("observation that failed to start", func(t *testing.T) {
		m, _, panel := privatePanelMux(t, true)
		if err := m.startObservation(filepath.Join(t.TempDir(), "not-there")); err == nil {
			t.Fatal("observation started against a registry that does not exist")
		}
		panelReadsAreRefusedOffBox(t, m, panel.id, "after a failed start")
	})

	t.Run("the zero value is today's public panel", func(t *testing.T) {
		m, _, panel := privatePanelMux(t, false)
		for _, tr := range offBoxTransports {
			if code := callOp(t, m, tr, "capture", map[string]any{"pane": panel.id}); code != "" {
				t.Errorf("a %q capture of the panel on a non-observing magmux answered %q", tr, code)
			}
			if code := watchCode(t, m, tr, panel.id); code != "" {
				t.Errorf("a %q watch of the panel on a non-observing magmux answered %q", tr, code)
			}
		}
	})
}

// TestObservationRefusesToStartOnAPublicPanel: "observation running" implies
// "panel private" by construction, for every caller including a test literal.
// The refusal comes THIRD, so the two errors that come before it are still the
// ones their callers get.
func TestObservationRefusesToStartOnAPublicPanel(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	dir := t.TempDir()

	err := m.startObservation(dir)
	if !errors.Is(err, errPanelPublic) {
		t.Fatalf("startObservation on a public panel = %v, want errPanelPublic", err)
	}
	if m.obs != nil {
		t.Fatal("a refused start left a pipeline behind")
	}

	// Second: a missing registry is reported as such, not as a public panel.
	err = m.startObservation(filepath.Join(dir, "not-there"))
	if err == nil || errors.Is(err, errPanelPublic) {
		t.Fatalf("a missing registry on a public panel = %v, want the registry error", err)
	}

	// First: "already running" wins over both. A non-nil pipeline pointer is
	// enough, because the check returns before anything reads it.
	m.obs = &pipeline.Pipeline{}
	err = m.startObservation(dir)
	m.obs = nil
	if err == nil || errors.Is(err, errPanelPublic) {
		t.Fatalf("a second start on a public panel = %v, want \"already running\"", err)
	}
}

// TestASocketVerbCaptureOfThePanelIsOnBox drives the REAL socket adapter: a
// unix socket served by handleSocketConn, whose reader sets msg.caller once,
// with callerFor. A capture of the panel over it is on-box and is served; the
// same request through the hub as an HTTP caller is refused. And the caller the
// adapter builds (callerFor) carries the connection's self-declared Client, and
// is the only writer of msg.caller on the socket path (review 3 Issue 19).
func TestASocketVerbCaptureOfThePanelIsOnBox(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()
	m, _, panel := privatePanelMux(t, true)

	// A probe op, registered for this test only, that reports the Caller the
	// hub hands it. `call` over the socket reaches it with msg.caller.
	got := make(chan hub.Caller, 1)
	if err := m.bus().Register("x5-test", hub.Op{
		Spec: protocol.OpSpec{Name: "x5.whoami", Class: protocol.ClassRead, Source: "x5-test",
			Schema: objectSchema(nil)},
		Fn: func(_ context.Context, c hub.Caller, _ json.RawMessage) (map[string]any, error) {
			select {
			case got <- c:
			default:
			}
			return map[string]any{}, nil
		},
	}); err != nil {
		t.Fatalf("registering the probe op: %v", err)
	}

	path := filepath.Join(sockTestDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go m.handleSocketConn(conn)
		}
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	r := newSockReader(t, conn)

	writeLines(t, conn, map[string]any{"type": "capture", "pane": panel.id, "id": 1, "client": "x5-probe"})
	reply, line := r.reply("1", 10*time.Second)
	if reply["ok"] != true {
		t.Fatalf("a socket capture of the panel was refused on an observing magmux: %s", line)
	}

	writeLines(t, conn, map[string]any{"type": "call", "op": "capture", "args": map[string]any{"pane": panel.id},
		"id": 2, "client": "x5-probe"})
	if reply, line := r.reply("2", 10*time.Second); reply["ok"] != true {
		t.Fatalf("a socket `call capture` of the panel was refused: %s", line)
	}

	writeLines(t, conn, map[string]any{"type": "call", "op": "x5.whoami", "id": 3, "client": "x5-probe"})
	if reply, line := r.reply("3", 10*time.Second); reply["ok"] != true {
		t.Fatalf("the probe op failed: %s", line)
	}
	select {
	case c := <-got:
		// `call` runs on the connection's Sub (subCall -> Sub.Call), which
		// passes the Sub's own Caller: Transport "socket" and the connection
		// label, and no per-message Client. That is main's behaviour and X5
		// depends on Transport alone; Client is a panel label nobody may
		// authorise on.
		if c.Transport != "socket" {
			t.Errorf("a socket `call` reached the hub as %+v, want Transport \"socket\"", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe op never ran")
	}

	// The one writer of msg.caller on the socket path is callerFor, and it
	// carries the message's self-declared Client beside the Sub's identity.
	sub := m.bus().Session(hub.Caller{Transport: "socket", Conn: "sock#x5"}, &capSink{}, nil)
	defer sub.Close("test over")
	if c := callerFor(sub, sockMsg{Client: "x5-probe"}); c.Transport != "socket" || c.Client != "x5-probe" ||
		c.Conn != "sock#x5" {
		t.Errorf("callerFor built %+v, want the Sub's socket identity plus Client \"x5-probe\"", c)
	}
	// And it IS the one writer: handleSocketMsg must not assign the field a
	// second time (review 3 Issue 19). Assignments to a sockMsg's `caller`
	// in non-test code: the socket adapter and the op adapter, nothing else.
	if sites := assignmentSites(t, "caller"); !sameSites(sites, map[string]string{
		"handleSocketConn": "sockconn.go", "opMsg": "ops_builtin.go",
	}) {
		t.Errorf("msg.caller is assigned at %v, want exactly handleSocketConn (sockconn.go) and opMsg "+
			"(ops_builtin.go)", sites)
	}

	if code := callOp(t, m, "http", "capture", map[string]any{"pane": panel.id}); code != protocol.CodePanelPrivate {
		t.Errorf("the same capture as an HTTP caller answered %q, want %q", code, protocol.CodePanelPrivate)
	}
}

// TestNoReadOpServesThePanelOffBoxWhileObserving is table-driven AND complete:
// every registered ClassRead op that takes a pane must have a case here, so an
// op added later without one fails rather than quietly serving the panel.
// watch and watch_all are not ops — they are the hub's Watcher — and have their
// own cases.
func TestNoReadOpServesThePanelOffBoxWhileObserving(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()
	m, session, panel := privatePanelMux(t, true)

	cases := map[string]map[string]any{
		"capture":    {"pane": panel.id},
		"transcript": {"pane": panel.id},
	}

	// Completeness: the registered ClassRead ops with a `pane` argument.
	specs, _ := m.bus().Ops()
	var missing []string
	for _, s := range specs {
		if s.Class != protocol.ClassRead {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(s.Schema, &schema); err != nil {
			t.Fatalf("op %s has a schema that is not an object: %v", s.Name, err)
		}
		if _, takesPane := schema.Properties["pane"]; !takesPane {
			continue
		}
		if _, ok := cases[s.Name]; !ok {
			missing = append(missing, s.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("ClassRead ops that take a pane and have no case here: %v. Add each, and make sure "+
			"it refuses the panel off-box (refusePanelOffBox).", missing)
	}

	for op, args := range cases {
		for _, tr := range offBoxTransports {
			t.Run(fmt.Sprintf("%s/%q", op, tr), func(t *testing.T) {
				if code := callOp(t, m, tr, op, args); code != protocol.CodePanelPrivate {
					t.Errorf("%s of the panel by %q answered %q, want %q", op, tr, code, protocol.CodePanelPrivate)
				}
			})
		}
	}

	for _, tr := range offBoxTransports {
		t.Run(fmt.Sprintf("watch/%q", tr), func(t *testing.T) {
			if code := watchCode(t, m, tr, panel.id); code != protocol.CodePanelPrivate {
				t.Errorf("a watch of the panel by %q answered %q, want %q", tr, code, protocol.CodePanelPrivate)
			}
		})
	}

	// watch_all: the panel is SKIPPED, by construction, and the session pane
	// is still streamed.
	t.Run("watch_all skips the panel", func(t *testing.T) {
		sub := m.bus().Session(hub.Caller{Transport: "ws", Conn: "x5"}, &capSink{}, nil)
		sub.Start(nil)
		defer sub.Close("test over")
		if err := sub.WatchAll(10); err != nil {
			t.Fatalf("WatchAll: %v", err)
		}
		st := m.streamer()
		st.mu.Lock()
		panelFramer, sessionFramer := st.framers[panel.id], st.framers[session.id]
		st.mu.Unlock()
		if panelFramer != nil {
			t.Error("watch_all from an off-box caller started a framer on the panel")
		}
		if sessionFramer == nil {
			t.Error("watch_all from an off-box caller did not stream the session pane")
		}
	})
}

// sameSites reports whether the assignment sites are exactly want, function
// name to file base name.
func sameSites(sites []stampSite, want map[string]string) bool {
	if len(sites) != len(want) {
		return false
	}
	for _, s := range sites {
		if file, ok := want[s.fn]; !ok || filepath.Base(s.file) != file {
			return false
		}
	}
	return true
}

// TestTheAgentsVerbIsSocketOnly is ADR-X4: the document lists every Claude
// Code session on the machine, so it is a socket verb and NOT a hub op — a hub
// op is readable by a view-token viewer over HTTP, WebSocket or the Firebase
// mirror. Checked at both levels: no op of that name is registered, and the
// dispatch refuses the verb to any caller that did not arrive on the socket.
func TestTheAgentsVerbIsSocketOnly(t *testing.T) {
	m, _, _ := privatePanelMux(t, true)

	specs, _ := m.bus().Ops()
	for _, s := range specs {
		if s.Name == "agents" {
			t.Fatalf("`agents` is registered as a hub op (%+v); it must stay socket-only", s)
		}
	}
	if code := callOp(t, m, "http", "agents", nil); code != protocol.CodeUnknownVerb {
		t.Errorf("`agents` called as an op answered %q, want %q", code, protocol.CodeUnknownVerb)
	}

	for _, tr := range offBoxTransports {
		_, err := m.dispatchSocketVerbExt(sockMsg{Type: "agents", caller: hub.Caller{Transport: tr}}, nil)
		if code := protocol.CodeOf(err); code != protocol.CodeUnknownVerb {
			t.Errorf("the `agents` verb from transport %q answered %q, want %q", tr, code, protocol.CodeUnknownVerb)
		}
	}
	// On the socket it is answered — here "off", because nothing started the
	// pipeline, which is the remedy-specific refusal the verb is meant to give.
	_, err := m.dispatchSocketVerbExt(sockMsg{Type: "agents", caller: hub.Caller{Transport: "socket"}}, nil)
	if code := protocol.CodeOf(err); code != protocol.CodeObsOff {
		t.Errorf("the `agents` verb on the socket answered %q, want %q", code, protocol.CodeObsOff)
	}
}
