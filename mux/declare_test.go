package mux

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/sockdir"
)

// ── fakes ───────────────────────────────────────────────────────────────────

// fakeToolShell points $SHELL at a test shell named `sh` whose PATH starts
// with fake `claude` and `claudish` executables, and returns the directory.
//
// The fakes are not a convenience. A pane command spelled `claude 'p'` runs
// whatever `claude` the login shell finds, and on a developer's machine that
// is the real one: a real session, real tokens, real side effects. The test
// shell also drops `-l`, because a login profile (macOS's path_helper above
// all) reorders PATH and would put the real binaries back in front.
//
// Each fake writes its argv, one per line, to <dir>/<name>.argv and sleeps, so
// a test can prove what the pane actually ran.
func fakeToolShell(t *testing.T) string {
	t.Helper()
	dir := sockTestDir(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"claude", "claudish"} {
		write(tool, fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexec sleep 30\n",
			filepath.Join(dir, tool+".argv")))
	}
	write("sh", fmt.Sprintf("#!/bin/sh\nPATH=%q:\"$PATH\"; export PATH\n[ \"$1\" = -l ] && shift\nexec /bin/sh \"$@\"\n", dir))
	t.Setenv("SHELL", filepath.Join(dir, "sh"))
	return dir
}

// readArgv returns what a fake tool was run with, waiting for it to start.
func readArgv(t *testing.T, dir, tool string) []string {
	t.Helper()
	path := filepath.Join(dir, tool+".argv")
	var b []byte
	if !waitFor(15*time.Second, func() bool {
		var err error
		b, err = os.ReadFile(path)
		return err == nil && len(b) > 0
	}) {
		t.Fatalf("the fake %s never ran (no %s)", tool, path)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// listPanes asks for `list` and returns its panes keyed by id.
func listPanesByID(t *testing.T, f *fakePlugin) map[int]map[string]any {
	t.Helper()
	res := replyOK(t, f.request(map[string]any{"type": "list"}))
	out := map[int]map[string]any{}
	raw, _ := res["panes"].([]any)
	for _, r := range raw {
		e := r.(map[string]any)
		out[int(e["pane"].(float64))] = e
	}
	return out
}

// awaitInput polls `list` until pane id publishes its `input` view, which
// only a GATED pane ever does (Poll, ~4 Hz).
func awaitInput(t *testing.T, f *fakePlugin, id int) map[string]any {
	t.Helper()
	var e map[string]any
	if !waitFor(10*time.Second, func() bool {
		e = listPanesByID(t, f)[id]
		_, ok := e["input"]
		return ok
	}) {
		t.Fatalf("pane %d never published `input`; a gated pane must: %v", id, e)
	}
	return e
}

// ── the predicate ───────────────────────────────────────────────────────────

// TestOnePredicate runs the whole decision — newPane's pre-spawn half, then
// the attach order — over a command table, and asks inputGateOf, the ONE
// predicate, about each result.
//
// Nothing is spawned: preparePane is newPane without the fork, and p.cmd is an
// UNSTARTED exec.Cmd, which is all the loose factory reads.
func TestOnePredicate(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // Start's transcript half resolves ~/.claude
	rows := []struct {
		name       string
		cfg        PaneConfig
		controller string // the attached controller's Name(), "" for none
		basis      gateBasis
		gated      bool
	}{
		{"declared wrapper", PaneConfig{Cmd: "/bin/sh", Args: []string{"-l", "-c", "claudish -i --model m -y"}, Controller: claudeCodeName},
			"claude-code", gateDeclared, true},
		{"declared plain shell", PaneConfig{Cmd: "/bin/sh", Args: []string{"-l", "-c", "sleep 30"}, Controller: claudeCodeName},
			"claude-code", gateDeclared, true},
		{"declared direct", PaneConfig{Cmd: "/bin/sh", Args: []string{"-l", "-c", "claude 'p'"}, Controller: claudeCodeName},
			"claude-code", gateDeclared, true},
		{"declared print mode", PaneConfig{Cmd: "/bin/sh", Args: []string{"-l", "-c", "claude -p 'x'"}, Controller: claudeCodeName},
			"claude-code", gateDeclared, true},
		{"direct", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "claude 'p'"}},
			"claude-code", gateDirect, true},
		{"direct, missed by the loose factory", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "claude>log"}},
			"claude-code", gateDirect, true},
		{"direct argv", PaneConfig{Cmd: "/usr/local/bin/claude"},
			"claude-code", gateDirect, true},
		{"print mode", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "claude -p 'x'"}},
			"claude-code", gateNone, false},
		{"loose only: grep", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "grep claude x"}},
			"claude-code", gateNone, false},
		// Not even the loose factory matches this one (`claude'` ends in a
		// quote), and direct detection must not either.
		{"nested shell", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "bash -c 'claude'"}},
			"", gateNone, false},
		{"loose only: bash -c with a prompt", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "bash -c 'claude p'"}},
			"claude-code", gateNone, false},
		{"loose only: two invocations", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "claude; claude"}},
			"claude-code", gateNone, false},
		{"fish", PaneConfig{Cmd: "/opt/homebrew/bin/fish", Args: []string{"-l", "-c", "claude 'p'"}},
			"claude-code", gateNone, false},
		{"undeclared wrapper", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "claudish -i --model m -y"}},
			"", gateNone, false},
		{"a shell", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l"}},
			"", gateNone, false},
		{"plugin", PaneConfig{Cmd: "/bin/zsh", Args: []string{"-l", "-c", "claude 'p'"}, Controller: pluginControllerPrefix + "ticket"},
			"plugin:ticket", gateNone, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			m := &Magmux{ctx: context.Background(), controllerFactories: []ControllerFactory{claudeCodeFactory}}
			p := preparePane(0, 0, 24, 80, r.cfg)
			p.cmd = exec.Command(r.cfg.Cmd, r.cfg.Args...)
			m.attachPaneController(p, r.cfg.Controller)
			t.Cleanup(func() { m.stopController(p) })

			name := ""
			if p.controller != nil {
				name = p.controller.Name()
			}
			if name != r.controller {
				t.Errorf("controller = %q, want %q", name, r.controller)
			}
			if p.gateBasis != r.basis {
				t.Errorf("gateBasis = %q, want %q", p.gateBasis, r.basis)
			}
			if got := inputGateOf(p) != nil; got != r.gated {
				t.Errorf("inputGateOf = %v, want %v", got, r.gated)
			}
			// direct ⇒ a Claude controller, always: the predicate's two halves
			// cannot disagree for a pane magmux itself recognised.
			if p.gateBasis != gateNone && inputGateOf(p) == nil {
				t.Errorf("gateBasis %q with no InputGate: the gate would silently not exist", p.gateBasis)
			}
		})
	}
}

// TestGateDecision pins editorOverride's Phase 4 values: a declared pane
// whose command is not a direct invocation is `delegated` (the wrapper owns
// the override); nothing else is decided yet.
func TestGateDecision(t *testing.T) {
	direct := claudeLaunchOf(shellCfg("/bin/sh", "claude"))
	wrapper := claudeLaunchOf(shellCfg("/bin/sh", "claudish -i"))
	rows := []struct {
		controller string
		launch     launchInfo
		basis      gateBasis
		override   editorOverride
	}{
		{"", wrapper, gateNone, overrideNone},
		{"", direct, gateDirect, overrideNone},
		{claudeCodeName, wrapper, gateDeclared, overrideDelegated},
		{claudeCodeName, direct, gateDeclared, overrideNone},
		{"plugin:ticket", direct, gateNone, overrideNone},
	}
	for _, r := range rows {
		b, o := gateDecision(r.controller, r.launch)
		if b != r.basis || o != r.override {
			t.Errorf("gateDecision(%q, direct=%v) = (%q, %q), want (%q, %q)",
				r.controller, r.launch.direct, b, o, r.basis, r.override)
		}
	}
}

// ── --controller ────────────────────────────────────────────────────────────

// TestControllerFlag: the flag applies to every -e and -g pane and never to
// the panel; an unknown value and a flag with no pane to apply to both exit 1
// with a message, because a flag that silently does nothing is the failure
// this repo keeps refusing.
func TestControllerFlag(t *testing.T) {
	t.Run("applyControllerFlag", func(t *testing.T) {
		cmds := []PaneConfig{{Cmd: "sh"}, {Cmd: "sh"}, {Control: true}}
		if err := applyControllerFlag(cmds, claudeCodeName, true); err != nil {
			t.Fatal(err)
		}
		if cmds[0].Controller != claudeCodeName || cmds[1].Controller != claudeCodeName {
			t.Errorf("a session pane was not declared: %+v", cmds)
		}
		if cmds[2].Controller != "" {
			t.Errorf("the panel was declared a Claude pane: %+v", cmds[2])
		}
		err := applyControllerFlag([]PaneConfig{{Cmd: "sh"}}, "codex", true)
		if err == nil || err.Error() != `unknown controller "codex" (known: claude-code)` {
			t.Errorf("unknown value: err = %v", err)
		}
		err = applyControllerFlag([]PaneConfig{{Control: true}}, claudeCodeName, false)
		if err == nil || err.Error() != "--controller applies to -e/-g panes and there are none" {
			t.Errorf("no panes: err = %v", err)
		}
	})

	exitWith := func(t *testing.T, wantMsg string, args ...string) {
		t.Helper()
		h := startHeadlessMagmux(t, args...)
		if code := h.wait(20 * time.Second); code != 1 {
			t.Fatalf("exit code %d, want 1\nstderr: %s", code, h.stderr.String())
		}
		if got := h.stderr.String(); !strings.Contains(got, wantMsg) {
			t.Errorf("stderr = %q, want it to contain %q", got, wantMsg)
		}
		h.requireSilentStdout()
	}
	t.Run("unknown value exits 1", func(t *testing.T) {
		exitWith(t, `magmux: unknown controller "codex" (known: claude-code)`,
			"--headless", "--controller", "codex", "-e", "sleep 30")
	})
	t.Run("no value exits 1", func(t *testing.T) {
		exitWith(t, `magmux: unknown controller "" (known: claude-code)`,
			"--headless", "-e", "sleep 30", "--controller")
	})
	t.Run("no -e/-g exits 1", func(t *testing.T) {
		exitWith(t, "magmux: --controller applies to -e/-g panes and there are none",
			"--headless", "--controller", "claude-code")
	})
	t.Run("-c alone exits 1", func(t *testing.T) {
		exitWith(t, "magmux: --controller applies to -e/-g panes and there are none",
			"--headless", "-c", "--controller", "claude-code")
	})

	t.Run("applies to every -e pane, not the panel; position-independent; last wins", func(t *testing.T) {
		fakeToolShell(t)
		m := startPluginMagmux(t, "-c", "-e", "sleep 30", "--controller", "codex",
			"-e", "sleep 31", "--controller", "claude-code")
		f := dialPlugin(t, m.sock)
		panes := listPanesByID(t, f)
		for _, id := range []int{0, 1} {
			e := panes[id]
			if e["gateBasis"] != "declared" || e["controller"] != "claude-code" {
				t.Errorf("-e pane %d: gateBasis=%v controller=%v, want declared/claude-code: %v",
					id, e["gateBasis"], e["controller"], e)
			}
		}
		panel := panes[2]
		if panel["state"] != "panel" {
			t.Fatalf("pane 2 is not the panel: %v", panel)
		}
		if _, ok := panel["gateBasis"]; ok {
			t.Errorf("the panel carries a gateBasis: %v", panel)
		}
		if _, ok := panel["controller"]; ok {
			t.Errorf("the panel has a controller: %v", panel)
		}
	})

	t.Run("applies to every -g pane", func(t *testing.T) {
		fakeToolShell(t)
		grid := filepath.Join(t.TempDir(), "grid.txt")
		if err := os.WriteFile(grid, []byte("# two\nsleep 30\nsleep 31\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := startPluginMagmux(t, "--controller", "claude-code", "-g", grid)
		f := dialPlugin(t, m.sock)
		panes := listPanesByID(t, f)
		for _, id := range []int{0, 1} {
			if e := panes[id]; e["gateBasis"] != "declared" || e["controller"] != "claude-code" {
				t.Errorf("-g pane %d is not declared: %v", id, e)
			}
		}
	})

	t.Run("without the flag nothing is declared", func(t *testing.T) {
		fakeToolShell(t)
		m := startPluginMagmux(t, "-e", "sleep 30")
		f := dialPlugin(t, m.sock)
		e := listPanesByID(t, f)[0]
		for _, k := range []string{"gateBasis", "editorOverride", "input", "controller"} {
			if _, ok := e[k]; ok {
				t.Errorf("an undeclared shell pane carries %q: %v", k, e)
			}
		}
	})
}

// TestClaudishShapeIsGated is claudish's exact launch (research F8): one
// headless magmux per pane, argv, with --controller before -e. The pane's
// command is a claudish wrapper that no substring test recognises, so the
// declaration is the only way in, and it must yield a gated Claude pane whose
// command reached the wrapper byte for byte (delegated: the wrapper owns the
// editor-mode override).
func TestClaudishShapeIsGated(t *testing.T) {
	fake := fakeToolShell(t)
	const id = "claudish-pane-1"
	// startHeadlessMagmux prepends --sock-dir, which is the rest of the shape.
	h := startHeadlessMagmux(t, "--headless", "--no-status", "--id", id,
		"--controller", "claude-code", "-e", "claudish -i --model m -y")
	m := &pluginMagmux{headlessMagmux: h}
	m.sock = filepath.Join(h.dir, "magmux-"+id+".sock")
	m.waitSock(t)
	f := dialPlugin(t, m.sock)

	e := awaitInput(t, f, 0)
	if e["controller"] != "claude-code" {
		t.Errorf("controller = %v, want claude-code", e["controller"])
	}
	if e["gateBasis"] != "declared" {
		t.Errorf("gateBasis = %v, want declared", e["gateBasis"])
	}
	if e["editorOverride"] != "delegated" {
		t.Errorf("editorOverride = %v, want delegated", e["editorOverride"])
	}
	if in, _ := e["input"].(map[string]any); in == nil || in["ready"] != false {
		// A fake wrapper that never draws a box is never ready.
		t.Errorf("input = %v, want a not-ready view", e["input"])
	}
	if got, want := readArgv(t, fake, "claudish"), []string{"-i", "--model", "m", "-y"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("the wrapper ran with %q, want %q byte for byte", got, want)
	}
	if !strings.HasSuffix(fmt.Sprint(e["cmd"]), "-l -c claudish -i --model m -y") {
		t.Errorf("cmd = %v; a declared wrapper's command is not rewritten", e["cmd"])
	}
}

// ── open_pane controller:"claude-code" ──────────────────────────────────────

// TestOpenPaneDeclaredController: "claude-code" names a TOOL and grants no
// ownership, so any connection may use it — on the socket verb and through
// the op, which is what HTTP and WS reach. Every value but "", "self" and
// "claude-code" stays bad_request.
func TestOpenPaneDeclaredController(t *testing.T) {
	fakeToolShell(t)
	m := startPluginMagmux(t, "-e", "sleep 60")
	f := dialPlugin(t, m.sock) // NOT registered: claude-code needs no identity

	verb := replyOK(t, f.request(map[string]any{
		"type": "open_pane", "cmd": "sleep 30", "label": "v", "controller": "claude-code",
	}))
	call := replyOK(t, f.request(map[string]any{
		"type": "call", "op": "open_pane",
		"args": map[string]any{"cmd": "sleep 31", "label": "o", "controller": "claude-code"},
	}))
	callID := func(res map[string]any) int {
		if r, ok := res["result"].(map[string]any); ok {
			return int(r["pane"].(float64))
		}
		return int(res["pane"].(float64))
	}
	panes := listPanesByID(t, f)
	for _, id := range []int{int(verb["pane"].(float64)), callID(call)} {
		e := panes[id]
		if e["gateBasis"] != "declared" || e["controller"] != "claude-code" || e["editorOverride"] != "delegated" {
			t.Errorf("pane %d: gateBasis=%v controller=%v editorOverride=%v, want declared/claude-code/delegated",
				id, e["gateBasis"], e["controller"], e["editorOverride"])
		}
	}

	for _, bad := range []string{"claude", "plugin:ticket", "codex", "Claude-Code"} {
		replyFailed(t, f.request(map[string]any{
			"type": "open_pane", "cmd": "sleep 30", "controller": bad,
		}), protocol.CodeBadRequest)
	}
	// "self" is still a plugin's word, and this connection is not one.
	replyFailed(t, f.request(map[string]any{
		"type": "open_pane", "cmd": "sleep 30", "controller": "self",
	}), protocol.CodeForbidden)

	// The op schema says so, for HTTP and WS clients that read it.
	ops := replyOK(t, f.request(map[string]any{"type": "ops"}))
	raw, _ := json.Marshal(ops["ops"])
	var specs []protocol.OpSpec
	if err := json.Unmarshal(raw, &specs); err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Name != "open_pane" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(s.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		if got := schema.Properties["controller"].Enum; len(got) != 1 || got[0] != "claude-code" {
			t.Errorf("open_pane's schema controller enum = %v, want [claude-code]", got)
		}
		return
	}
	t.Fatal("no open_pane in ops")
}

// TestOpenPaneControllerReachesNewPane (gemini H1): the declaration has ONE
// field, PaneConfig.Controller, so what sockOpenPane resolved is what newPane
// decides on. A plugin's claimed pane running a direct `claude` must reach it
// as plugin:<name> — not gated, not a Claude controller — and a claude-code
// declaration over a wrapper must reach it as declared + delegated.
func TestOpenPaneControllerReachesNewPane(t *testing.T) {
	fake := fakeToolShell(t)
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))

	plug := int(replyOK(t, p.request(map[string]any{
		"type": "open_pane", "cmd": "claude 'p'", "controller": "self",
	}))["pane"].(float64))
	decl := int(replyOK(t, p.request(map[string]any{
		"type": "open_pane", "cmd": "claudish -i --model m -y", "controller": "claude-code",
	}))["pane"].(float64))

	panes := listPanesByID(t, p)
	pe := panes[plug]
	if pe["controller"] != "plugin:ticket" {
		t.Errorf("plugin pane controller = %v, want plugin:ticket", pe["controller"])
	}
	for _, k := range []string{"gateBasis", "editorOverride", "input"} {
		if _, ok := pe[k]; ok {
			t.Errorf("the plugin's direct-claude pane carries %q=%v; it must never be gated", k, pe[k])
		}
	}
	de := panes[decl]
	if de["controller"] != "claude-code" || de["gateBasis"] != "declared" || de["editorOverride"] != "delegated" {
		t.Errorf("declared pane: controller=%v gateBasis=%v editorOverride=%v, want claude-code/declared/delegated",
			de["controller"], de["gateBasis"], de["editorOverride"])
	}
	// No rewrite on the plugin pane: its claude got exactly the prompt.
	if got := readArgv(t, fake, "claude"); len(got) != 1 || got[0] != "p" {
		t.Errorf("the plugin pane's claude ran with %q, want [p]", got)
	}
}

// ── PaneConfig.SockDir ──────────────────────────────────────────────────────

// TestSettingsDirFollowsPaneSockDir (gemini M7): the private settings
// directory is derived from the pane's OWN sockdir, which every builder and
// OpenPane fill from m.socketDir() — so a struct-literal Magmux with its own
// sockDir writes (and, in Phase 6, sweeps) the same directory, and never the
// package global.
func TestSettingsDirFollowsPaneSockDir(t *testing.T) {
	if got, want := settingsDir("/x/y"), filepath.Join("/x/y", fmt.Sprintf("magmux-settings-%d", os.Geteuid())); got != want {
		t.Errorf("settingsDir = %q, want %q", got, want)
	}
	if got, want := settingsDir(""), settingsDir(sockdir.Dir); got != want {
		t.Errorf("settingsDir(\"\") = %q, want the package default %q", got, want)
	}

	dir := sockTestDir(t)
	want := settingsDir(dir)
	if want == settingsDir(sockdir.Dir) {
		t.Fatalf("the test dir %q is the package default; the test proves nothing", dir)
	}
	cfg := PaneConfig{Cmd: "/bin/sh", Args: []string{"-c", "sleep 30"}}
	reap := func(m *Magmux) {
		for _, p := range m.allPanes {
			if p != nil {
				m.reapPane(p, true)
				if p.cmd != nil {
					_, _ = p.cmd.Process.Wait()
				}
			}
		}
	}

	t.Run("buildGrid", func(t *testing.T) {
		m := &Magmux{rows: 24, cols: 80, sockDir: dir, quit: make(chan struct{}), control: newControlPanel()}
		if err := m.buildGrid([]PaneConfig{cfg, cfg}); err != nil {
			t.Fatal(err)
		}
		defer reap(m)
		for _, p := range m.allPanes {
			if p.settingsDir != want {
				t.Errorf("buildGrid pane settingsDir = %q, want %q", p.settingsDir, want)
			}
		}
	})
	t.Run("buildLayout", func(t *testing.T) {
		m := &Magmux{rows: 24, cols: 80, sockDir: dir, quit: make(chan struct{}), control: newControlPanel()}
		if err := m.buildLayout([]PaneConfig{cfg, cfg, cfg}); err != nil {
			t.Fatal(err)
		}
		defer reap(m)
		for _, p := range m.allPanes {
			if p.settingsDir != want {
				t.Errorf("buildLayout pane settingsDir = %q, want %q", p.settingsDir, want)
			}
		}
	})
	t.Run("OpenPane", func(t *testing.T) {
		m := &Magmux{rows: 24, cols: 120, sockDir: dir, quit: make(chan struct{}), control: newControlPanel()}
		if err := m.buildGrid([]PaneConfig{cfg}); err != nil {
			t.Fatal(err)
		}
		defer reap(m)
		id, err := m.OpenPane(OpenPaneRequest{PaneConfig: cfg, Target: targetLargest})
		if err != nil {
			t.Fatal(err)
		}
		if got := m.paneByID(id).settingsDir; got != want {
			t.Errorf("OpenPane pane settingsDir = %q, want %q", got, want)
		}
	})
}

// ── Stop ────────────────────────────────────────────────────────────────────

// stopCounter is a controller that counts Stop calls.
type stopCounter struct{ stops int }

func (s *stopCounter) Name() string                { return "stop-counter" }
func (s *stopCounter) Start(context.Context) error { return nil }
func (s *stopCounter) Poll() (Snapshot, error)     { return Snapshot{State: CtrlStarting}, nil }
func (s *stopCounter) Stop() error                 { s.stops++; return nil }

// TestCloseAndUnwindStopTheController: ClosePane and unwindPane both stop the
// pane's controller, which for a Claude pane is what ends its termios watch
// before the master closes under it.
func TestCloseAndUnwindStopTheController(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	sc := &stopCounter{}
	m.paneByID(1).controller = sc
	if err := m.ClosePane(1, false); err != nil {
		t.Fatal(err)
	}
	if sc.stops != 1 {
		t.Errorf("ClosePane stopped the controller %d times, want 1", sc.stops)
	}

	uc := &stopCounter{}
	m.unwindPane(&Pane{controller: uc})
	if uc.stops != 1 {
		t.Errorf("unwindPane stopped the controller %d times, want 1", uc.stops)
	}
}

// TestGatedStartCannotFail is the Start split: os.UserHomeDir is Start's only
// error, and for a GATED pane it leaves transcript discovery dormant instead
// of failing — attachController drops a controller whose Start fails, and
// with it would go the gate. An observed-only pane fails exactly as before.
func TestGatedStartCannotFail(t *testing.T) {
	t.Setenv("HOME", "") // os.UserHomeDir errors on an empty $HOME
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("os.UserHomeDir does not fail on this platform with HOME unset")
	}
	m := &Magmux{ctx: context.Background(), controllerFactories: []ControllerFactory{claudeCodeFactory}}

	gated := preparePane(0, 0, 24, 80, shellCfg("/bin/sh", "claudish -i"))
	gated.gateBasis = gateDeclared
	m.attachController(gated)
	if inputGateOf(gated) == nil {
		t.Fatal("a gated pane lost its controller (and its gate) to a Start error")
	}
	if c := gated.controller.(*ClaudeCodeController); c.projectDir != "" {
		t.Errorf("transcript discovery is not dormant: projectDir = %q", c.projectDir)
	}

	loose := preparePane(0, 0, 24, 80, shellCfg("/bin/sh", "grep claude x"))
	loose.cmd = exec.Command("/bin/sh", "-l", "-c", "grep claude x")
	m.attachController(loose)
	if loose.controller != nil {
		t.Errorf("an observed-only pane kept a controller whose Start failed: %v", loose.controller.Name())
	}
}
