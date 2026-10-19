package mux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── normalizeBinding ────────────────────────────────────────────────────────

func TestNormalizeBinding(t *testing.T) {
	const id = "0b7c2a4e-5d1f-4c3b-9a8e-1f2d3c4b5a69"
	path := "/cfg/projects/-w/" + id + ".jsonl"
	for _, tc := range []struct {
		name, id, path    string
		wantID, wantPath  string
		wantErrContaining string
	}{
		{name: "none"},
		{name: "id only", id: id, wantID: id},
		{name: "path only fills the id", path: path, wantID: id, wantPath: path},
		{name: "both, agreeing", id: id, path: path, wantID: id, wantPath: path},
		{name: "both, disagreeing", id: "other", path: path, wantErrContaining: "is not session"},
		{name: "relative path", path: "projects/x/" + id + ".jsonl", wantErrContaining: "clean absolute"},
		{name: "unclean path", path: "/cfg/projects/../x/" + id + ".jsonl", wantErrContaining: "clean absolute"},
		{name: "not jsonl", path: "/cfg/projects/x/" + id, wantErrContaining: ".jsonl"},
		{name: "id with a slash", id: "../../etc/passwd", wantErrContaining: "only letters"},
		{name: "id too long", id: strings.Repeat("a", sessionIDMax+1), wantErrContaining: "1-128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotPath, err := normalizeBinding(tc.id, tc.path)
			if tc.wantErrContaining != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContaining) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErrContaining)
				}
				return
			}
			if err != nil || gotID != tc.wantID || gotPath != tc.wantPath {
				t.Fatalf("= %q, %q, %v; want %q, %q, nil", gotID, gotPath, err, tc.wantID, tc.wantPath)
			}
		})
	}
}

func TestApplyBindingFlags(t *testing.T) {
	const id = "abc"
	one := func() []PaneConfig { return []PaneConfig{{Cmd: "sh"}, {Control: true}} }
	cmds := one()
	if err := applyBindingFlags(cmds, id, "", true, true); err != nil {
		t.Fatal(err)
	}
	if cmds[0].SessionID != id || cmds[1].SessionID != "" {
		t.Fatalf("binding landed on the wrong panes: %+v", cmds)
	}
	if err := applyBindingFlags(one(), id, "", false, true); err == nil ||
		!strings.Contains(err.Error(), "need --controller claude-code") {
		t.Errorf("without --controller: err = %v", err)
	}
	if err := applyBindingFlags(one(), id, "", true, false); err == nil ||
		!strings.Contains(err.Error(), "exactly one -e pane") {
		t.Errorf("with several panes: err = %v", err)
	}
	if err := applyBindingFlags(one(), "", "", false, false); err != nil {
		t.Errorf("no binding must be no-op: %v", err)
	}
}

// ── discovery never runs for a declared pane ───────────────────────────────

// TestNamedSessionWaitsAndNeverFallsBack is the binding's core rule in
// process: with a busy foreign transcript the only candidate, a declared pane
// tails nothing; it locks onto exactly its own file once that appears.
func TestNamedSessionWaitsAndNeverFallsBack(t *testing.T) {
	config := t.TempDir()
	cwd := t.TempDir()
	proj := filepath.Join(config, "projects", encodeProjectDir(cwd))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := writeTranscript(t, proj, "parent.jsonl", "the parent session")

	for _, declared := range []string{"path", "id"} {
		t.Run(declared, func(t *testing.T) {
			id := "ours-" + declared
			ours := filepath.Join(proj, id+".jsonl")
			c := &ClaudeCodeController{
				projectsDir: filepath.Join(config, "projects"),
				projectDir:  proj,
				spawnedAt:   time.Now().Add(-time.Minute),
				bindID:      id,
			}
			if declared == "path" {
				c.bindPath = ours
			}
			_ = os.Chtimes(foreign, time.Now(), time.Now()) // still being written
			if got := c.findActiveSession(); got != "" {
				t.Fatalf("a declared pane discovered %q before its own transcript existed", got)
			}
			if path, gotID, mode, seen := c.Binding(); gotID != id || mode != bindNamed || seen ||
				(declared == "path" && path != ours) {
				t.Fatalf("Binding() = %q %q %q %v before the file exists", path, gotID, mode, seen)
			}
			writeTranscript(t, proj, id+".jsonl", "")
			if got := c.findActiveSession(); got != ours {
				t.Fatalf("found %q, want exactly %q", got, ours)
			}
		})
	}
}

// TestDeclaredSessionIsReservedFromSiblings: within one magmux, a discovering
// pane cannot take <id>.jsonl in the window before its declared owner claims
// it, and two panes cannot both claim one declared file.
func TestDeclaredSessionIsReservedFromSiblings(t *testing.T) {
	m := &Magmux{}
	owner, other := &Pane{}, &Pane{}
	if !m.declareSession("s1", owner) {
		t.Fatal("first declaration refused")
	}
	if m.declareSession("s1", other) {
		t.Fatal("a second pane declared the same session")
	}
	path := "/cfg/projects/x/s1.jsonl"
	if !m.isSessionClaimed(path, other) || m.claimSession(path, other) {
		t.Fatal("a sibling could take a declared session's transcript")
	}
	if !m.claimSession(path, owner) {
		t.Fatal("the declaring pane could not claim its own transcript")
	}
	m.releaseSessions(owner)
	if !m.declareSession("s1", other) {
		t.Fatal("releaseSessions left the declaration behind")
	}
}

// TestRegistryReadsTheDeclaredSession: the registry is searched by the
// declared id from the start, under the declared transcript's config dir,
// and a pid entry naming another session is not this pane's.
func TestRegistryReadsTheDeclaredSession(t *testing.T) {
	config := t.TempDir()
	sessions := filepath.Join(config, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(sessions, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1.json", `{"status":"waiting","waitingFor":"permission","sessionId":"parent"}`)
	write("2.json", `{"status":"idle","sessionId":"ours"}`)
	c := &ClaudeCodeController{
		bindID:   "ours",
		bindPath: filepath.Join(config, "projects", "-w", "ours.jsonl"), // not on disk yet
	}
	if e := c.registry(time.Now()); e.SessionID != "ours" || e.Status != "idle" {
		t.Fatalf("registry() = %+v, want the declared session's own entry", e)
	}
}

// ── end to end: the claudish shape, on the real binary ──────────────────────

// TestDeclaredPanesInOneCwdBindTheirOwnTranscripts is the claudish setup that
// broke the gate: a parent session and two team slots share one cwd, every
// slot is its own magmux running a wrapper (no prompt to match), the parent's
// transcript is being written and its registry entry says it is waiting on a
// permission dialog, and the slots are sent to before either child has
// written a transcript.
//
// Declared, each slot reports exactly its own path with bound:named and is
// never answered from the parent's registry. The undeclared control run
// shows what the declaration fixes: discovery locks onto the parent.
func TestDeclaredPanesInOneCwdBindTheirOwnTranscripts(t *testing.T) {
	fakeToolShell(t)
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	cwd := t.TempDir()
	proj := filepath.Join(config, "projects", encodeProjectDir(cwd))
	sessions := filepath.Join(config, "sessions")
	for _, d := range []string{proj, sessions} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	parent := writeTranscript(t, proj, "parent.jsonl", "the parent session")
	if err := os.WriteFile(filepath.Join(sessions, "1.json"),
		[]byte(`{"status":"waiting","waitingFor":"permission","sessionId":"parent"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The parent keeps writing for the whole test, so it stays the newest file.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f, err := os.OpenFile(parent, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				f.WriteString(`{"type":"assistant","message":{"content":"working"}}` + "\n")
			}
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })

	launch := "cd " + cwd + " && stty raw -echo && sleep 30" // a wrapper in raw mode, as Claude Code is; no prompt anywhere
	slot := func(t *testing.T, id string, declare bool) *fakePlugin {
		args := []string{"--controller", "claude-code"}
		if declare {
			args = append(args, "--session-id", id, "--transcript", filepath.Join(proj, id+".jsonl"))
		}
		m := startPluginMagmux(t, append(args, "-e", launch)...)
		return dialPlugin(t, m.sock)
	}
	sendNow := func(t *testing.T, f *fakePlugin) map[string]any {
		return f.request(map[string]any{"type": "send", "pane": 0, "text": "hello", "readyTimeoutMs": 1500})
	}

	t.Run("undeclared control: discovery takes the parent", func(t *testing.T) {
		f := slot(t, "", false)
		var e map[string]any
		if !waitFor(10*time.Second, func() bool {
			e = listPanesByID(t, f)[0]
			return e["transcriptPath"] == parent
		}) {
			t.Fatalf("the control never locked onto the parent, so it proves nothing: %v", e)
		}
		if e["bound"] != bindMtime {
			t.Errorf("bound = %v, want %q", e["bound"], bindMtime)
		}
		// The symptom claudish saw: a false busy from the PARENT's registry.
		// Unless the control reproduces it, the declared runs below prove nothing.
		if rep := sendNow(t, f); rep["reason"] != "dialog:permission" {
			t.Fatalf("control send = %v, want the parent's dialog:permission", rep)
		}
	})

	ids := []string{"0b7c2a4e-0000-4c3b-9a8e-000000000001", "0b7c2a4e-0000-4c3b-9a8e-000000000002"}
	slots := make([]*fakePlugin, len(ids))
	for i, id := range ids {
		slots[i] = slot(t, id, true)
	}
	check := func(t *testing.T, i int, wantSeen bool) map[string]any {
		t.Helper()
		e := listPanesByID(t, slots[i])[0]
		want := filepath.Join(proj, ids[i]+".jsonl")
		if e["sessionId"] != ids[i] || e["bound"] != bindNamed || e["transcriptPath"] != want ||
			e["transcriptSeen"] != wantSeen {
			t.Fatalf("slot %d list = sessionId %v bound %v transcriptPath %v transcriptSeen %v; "+
				"want %s named %s %v", i, e["sessionId"], e["bound"], e["transcriptPath"],
				e["transcriptSeen"], ids[i], want, wantSeen)
		}
		return e
	}

	// Sends before either child has written its transcript. Each runs past
	// several 250ms polls, which is when discovery used to lock on.
	for i := range slots {
		rep := sendNow(t, slots[i])
		if rep["reason"] != "starting" {
			t.Errorf("slot %d early send = %v, want busy starting (no box yet), never the parent's dialog", i, rep)
		}
		check(t, i, false)
		if b, _ := json.Marshal(awaitInput(t, slots[i], 0)["input"]); strings.Contains(string(b), "dialog") {
			t.Errorf("slot %d input view reads the parent's registry: %s", i, b)
		}
	}

	// Slot 0's child writes its transcript; slot 1's still has not.
	writeTranscript(t, proj, ids[0]+".jsonl", "")
	if !waitFor(10*time.Second, func() bool {
		return listPanesByID(t, slots[0])[0]["transcriptSeen"] == true
	}) {
		t.Fatal("slot 0 never saw its own transcript")
	}
	check(t, 0, true)
	check(t, 1, false)

	writeTranscript(t, proj, ids[1]+".jsonl", "")
	if !waitFor(10*time.Second, func() bool {
		return listPanesByID(t, slots[1])[0]["transcriptSeen"] == true
	}) {
		t.Fatal("slot 1 never saw its own transcript")
	}
	check(t, 0, true)
	check(t, 1, true)
}

// TestOpenPaneBinding: the open_pane form, its refusal without the Claude
// controller, and the echo.
func TestOpenPaneBinding(t *testing.T) {
	fakeToolShell(t)
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	m := startPluginMagmux(t, "-e", "sleep 30")
	f := dialPlugin(t, m.sock)

	rep := f.request(map[string]any{"type": "open_pane", "cmd": "sleep 30", "session_id": "s1"})
	if ok, _ := rep["ok"].(bool); ok || rep["code"] != "bad_request" {
		t.Fatalf("session_id without controller:claude-code = %v, want bad_request", rep)
	}
	rep = f.request(map[string]any{"type": "open_pane", "cmd": "sleep 30", "controller": "claude-code",
		"session_id": "s1", "transcript": "/x/projects/y/s2.jsonl"})
	if ok, _ := rep["ok"].(bool); ok || rep["code"] != "bad_request" {
		t.Fatalf("disagreeing session_id/transcript = %v, want bad_request", rep)
	}
	res := replyOK(t, f.request(map[string]any{"type": "open_pane", "cmd": "sleep 30",
		"controller": "claude-code", "session_id": "s1"}))
	id := int(res["pane"].(float64))
	e := listPanesByID(t, f)[id]
	if e["sessionId"] != "s1" || e["bound"] != bindNamed || e["transcriptSeen"] != false {
		t.Fatalf("open_pane binding not echoed: %v", e)
	}
	if _, ok := e["transcriptPath"]; ok {
		t.Errorf("an id-only binding reports a transcriptPath before the file exists: %v", e)
	}
}
