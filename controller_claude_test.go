package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// paneWithCmd builds a minimal Pane carrying the given command line, as
// magmux constructs it for gridfile / `-e` panes (`zsh -l -c "<cmdline>"`).
func paneWithCmd(cmdline string) *Pane {
	return &Pane{cmd: exec.Command("zsh", "-l", "-c", cmdline)}
}

func TestEncodeProjectDir(t *testing.T) {
	// Expectations are taken from real ~/.claude/projects directory names
	// cross-checked against the `cwd` recorded inside their transcripts.
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{
			name: "plain path",
			cwd:  "/Users/jack/mag/magmux",
			want: "-Users-jack-mag-magmux",
		},
		{
			name: "dotted path collapses .claude to -claude",
			cwd:  "/Users/jack/dev/circl/circlmini/.claude/worktrees/incidenc-agent",
			want: "-Users-jack-dev-circl-circlmini--claude-worktrees-incidenc-agent",
		},
		{
			name: "underscore becomes dash",
			cwd:  "/Users/jack/mag/babble_ml",
			want: "-Users-jack-mag-babble-ml",
		},
		{
			name: "space becomes dash",
			cwd:  "/Users/jack/Documents/Claude/Projects/Models bioses",
			want: "-Users-jack-Documents-Claude-Projects-Models-bioses",
		},
		{
			name: "underscore inside a worktree name",
			cwd:  "/Users/jack/mag/meroku/.claude/worktrees/new_ui",
			want: "-Users-jack-mag-meroku--claude-worktrees-new-ui",
		},
		{
			name: "digits and existing dashes are preserved",
			cwd:  "/Users/jack/mag/passflow/passflow/.claude/worktrees/2fa",
			want: "-Users-jack-mag-passflow-passflow--claude-worktrees-2fa",
		},
		{
			name: "empty",
			cwd:  "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encodeProjectDir(tt.cwd); got != tt.want {
				t.Errorf("encodeProjectDir(%q)\n got: %q\nwant: %q", tt.cwd, got, tt.want)
			}
		})
	}
}

// TestEncodeProjectDirNoLiteralDots guards the specific regression: a dotted
// cwd (every .claude/worktrees/* git worktree) must not produce a directory
// name containing a literal dot, because Claude Code never writes one.
func TestEncodeProjectDirNoLiteralDots(t *testing.T) {
	got := encodeProjectDir("/Users/jack/mag/magmux/.claude/worktrees/finish-1")
	for _, r := range got {
		if r != '-' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			t.Fatalf("encoded name %q contains non-alphanumeric, non-dash rune %q", got, r)
		}
	}
}

func TestExtractClaudePrompt(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"double quoted", `claude "say hi"`, "say hi"},
		{"single quoted", `cd /foo && claude 'list files'`, "list files"},
		{"with flags", `claude --dangerously-skip-permissions "reply with only the word hello"`, "reply with only the word hello"},
		{"unquoted with redirect", `claude do a thing > /tmp/out`, "do a thing"},
		{"interactive, no prompt", `claude`, ""},
		{"not claude at all", `vim main.go`, ""},
		// REGRESSION: a flag as the final token was returned as the prompt.
		// The flag-skipping loop looked for a following space to advance
		// past, found none, and fell out still holding the flag. A non-empty
		// wantPrompt then routes discovery down match-by-prompt, hunting for
		// a transcript whose first user message is "--dangerously-skip-
		// permissions". Nothing matches, the mtime fallback that interactive
		// panes depend on is never reached, and the controller never locks
		// on — so model/response/tool stay empty for the whole session while
		// the pane looks perfectly healthy. This is the exact command a
		// controlled session uses, so it broke every pilot run.
		{"trailing flag only", `claude --dangerously-skip-permissions`, ""},
		{"two trailing flags", `claude --verbose --dangerously-skip-permissions`, ""},
		{"flag with value, no prompt", `claude --model opus`, ""},
		{"flags then prompt still works", `claude --verbose -p "do it"`, "do it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractClaudePrompt(paneWithCmd(tt.cmdline)); got != tt.want {
				t.Errorf("extractClaudePrompt(%q)\n got: %q\nwant: %q", tt.cmdline, got, tt.want)
			}
		})
	}
}

func TestExtractClaudeCwd(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"cd prefix to a real dir", "cd " + dir + " && claude 'hi'", dir},
		{"quoted cd target", `cd "` + dir + `" && claude 'hi'`, dir},
		{"no cd prefix", `claude 'hi'`, ""},
		{"cd to a nonexistent dir", `cd /nope/does/not/exist && claude 'hi'`, ""},
		{"relative cd is not resolvable", `cd ../sibling && claude 'hi'`, ""},
		{"cd without && is not a prefix", `cd ` + dir, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractClaudeCwd(paneWithCmd(tt.cmdline)); got != tt.want {
				t.Errorf("extractClaudeCwd(%q)\n got: %q\nwant: %q", tt.cmdline, got, tt.want)
			}
		})
	}
}

// TestStartUsesCdTargetAsProjectDir covers the case where the pane's real cwd
// differs from magmux's own: `cd /foo && claude '...'` files its transcript
// under /foo, not under wherever magmux was launched.
func TestStartUsesCdTargetAsProjectDir(t *testing.T) {
	paneDir := t.TempDir()
	c := newClaudeCodeController(paneWithCmd("cd " + paneDir + " && claude 'hi'"))
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c.cwd != paneDir {
		t.Errorf("cwd = %q, want the cd target %q", c.cwd, paneDir)
	}
	if want := encodeProjectDir(paneDir); filepath.Base(c.projectDir) != want {
		t.Errorf("projectDir base = %q, want %q", filepath.Base(c.projectDir), want)
	}
}

// --- state machine -------------------------------------------------------

func TestApplyLineStateMachine(t *testing.T) {
	c := &ClaudeCodeController{snap: Snapshot{State: CtrlStarting}}

	c.applyLine([]byte(`{"type":"user","cwd":"/tmp/proj","timestamp":"2026-07-15T10:00:00.000Z","message":{"content":"say hi"}}`))
	if c.snap.State != CtrlWorking {
		t.Errorf("after user entry: state = %v, want working", c.snap.State)
	}
	if c.snap.LastUserPrompt != "say hi" {
		t.Errorf("LastUserPrompt = %q, want %q", c.snap.LastUserPrompt, "say hi")
	}
	if c.snap.Project != "proj" {
		t.Errorf("Project = %q, want %q", c.snap.Project, "proj")
	}

	c.applyLine([]byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":" hello "}]}}`))
	if c.snap.Model != "claude-opus-5" {
		t.Errorf("Model = %q", c.snap.Model)
	}
	if c.snap.LastTool != "Bash" {
		t.Errorf("LastTool = %q, want Bash", c.snap.LastTool)
	}
	if c.snap.LastResponse != "hello" {
		t.Errorf("LastResponse = %q, want %q (trimmed)", c.snap.LastResponse, "hello")
	}
	if c.snap.State != CtrlWorking {
		t.Errorf("assistant entry must not settle state, got %v", c.snap.State)
	}

	c.applyLine([]byte(`{"type":"system","subtype":"stop_hook_summary","timestamp":"2026-07-15T10:00:05.000Z"}`))
	if c.snap.State != CtrlAwaitingInput {
		t.Errorf("after stop_hook_summary: state = %v, want awaiting_input", c.snap.State)
	}
	if c.snap.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set by stop_hook_summary")
	}

	// Malformed lines must be ignored, not panic or reset state.
	c.applyLine([]byte(`not json`))
	c.applyLine([]byte(`{"type":`))
	if c.snap.State != CtrlAwaitingInput {
		t.Errorf("malformed lines changed state to %v", c.snap.State)
	}
}

// --- issue #2: snapshot must reach awaiting_input --------------------------

func TestApplyTerminalIdlePromotesWithoutStopHook(t *testing.T) {
	// The exact shape of issue #2: no stop_hook_summary ever arrives (or the
	// transcript was never found at all), but the pane's OSC notification
	// says the child is idle. The snapshot must agree with what results
	// reports rather than sitting at "starting" forever.
	p := &Pane{}
	p.inputReady = true
	p.inputSignal = "osc"
	p.inputReadyAt = time.Now()

	c := &ClaudeCodeController{pane: p, snap: Snapshot{State: CtrlStarting}}
	c.applyTerminalIdle()

	if c.snap.State != CtrlAwaitingInput {
		t.Fatalf("state = %v, want awaiting_input", c.snap.State)
	}
	if c.snap.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set from the terminal idle time")
	}
}

func TestApplyTerminalIdleSignalSources(t *testing.T) {
	// Every non-controller idle signal is admissible evidence; the two the
	// controller itself sets are not, or the state would latch on its own
	// output.
	tests := []struct {
		signal string
		want   ControllerState
	}{
		{"osc", CtrlAwaitingInput},
		{"2004", CtrlAwaitingInput},
		{"title", CtrlAwaitingInput},
		{"idle", CtrlAwaitingInput},
		{"ctrl", CtrlWorking},
		{"perm", CtrlWorking},
	}
	for _, tt := range tests {
		t.Run(tt.signal, func(t *testing.T) {
			p := &Pane{}
			p.inputReady = true
			p.inputSignal = tt.signal
			p.inputReadyAt = time.Now()

			c := &ClaudeCodeController{pane: p, snap: Snapshot{State: CtrlWorking}}
			c.applyTerminalIdle()
			if c.snap.State != tt.want {
				t.Errorf("signal %q: state = %v, want %v", tt.signal, c.snap.State, tt.want)
			}
		})
	}
}

func TestApplyTerminalIdleDoesNotPromoteWhenPaneBusy(t *testing.T) {
	p := &Pane{} // inputReady false — pane is working
	c := &ClaudeCodeController{pane: p, snap: Snapshot{State: CtrlWorking}}
	c.applyTerminalIdle()
	if c.snap.State != CtrlWorking {
		t.Errorf("state = %v, want working", c.snap.State)
	}
}

// TestApplyTerminalIdleTranscriptWins is the anti-flap guarantee: once the
// transcript advances past the moment the terminal went idle, a new turn has
// begun and the stale idle signal must not drag the state back.
func TestApplyTerminalIdleTranscriptWins(t *testing.T) {
	idleAt := time.Now()
	p := &Pane{}
	p.inputReady = true
	p.inputSignal = "osc"
	p.inputReadyAt = idleAt

	c := &ClaudeCodeController{
		pane:        p,
		snap:        Snapshot{State: CtrlWorking},
		lastApplyAt: idleAt.Add(time.Millisecond), // transcript moved later
	}
	c.applyTerminalIdle()
	if c.snap.State != CtrlWorking {
		t.Fatalf("stale idle signal overrode a newer transcript entry: state = %v", c.snap.State)
	}

	// ...and once the terminal goes idle again *after* that entry, promotion
	// resumes. Together these two halves mean the state cannot oscillate.
	p.inputReadyAt = c.lastApplyAt.Add(time.Millisecond)
	c.applyTerminalIdle()
	if c.snap.State != CtrlAwaitingInput {
		t.Fatalf("fresh idle signal was ignored: state = %v", c.snap.State)
	}
}

func TestApplyTerminalIdleLeavesSettledStates(t *testing.T) {
	for _, st := range []ControllerState{CtrlAwaitingPermission, CtrlError, CtrlGone} {
		p := &Pane{}
		p.inputReady = true
		p.inputSignal = "osc"
		p.inputReadyAt = time.Now()

		c := &ClaudeCodeController{pane: p, snap: Snapshot{State: st}}
		c.applyTerminalIdle()
		if c.snap.State != st {
			t.Errorf("state %v was overwritten with %v", st, c.snap.State)
		}
	}
}

// --- issue #3: transcript selection ---------------------------------------

func writeTranscript(t *testing.T, dir, name, prompt string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	quoted, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","cwd":"/tmp/proj","message":{"content":` + string(quoted) + `}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestFindActiveSessionMatchesByPrompt(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "other.jsonl", "someone else's work")
	want := writeTranscript(t, dir, "ours.jsonl", "reply with only the word hello")

	c := &ClaudeCodeController{
		projectDir: dir,
		wantPrompt: "reply with only the word hello",
		spawnedAt:  time.Now().Add(-time.Minute),
	}
	if got := c.findActiveSession(); got != want {
		t.Errorf("findActiveSession() = %q, want %q", got, want)
	}
}

// TestFindActiveSessionWidensWhenProjectDirWrong is the durable fix for the
// encoding contract: even if the computed directory is wrong or missing,
// content matching finds the transcript anywhere under ~/.claude/projects.
func TestFindActiveSessionWidensWhenProjectDirWrong(t *testing.T) {
	projects := t.TempDir()
	real := filepath.Join(projects, "-some-other-encoding")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeTranscript(t, real, "ours.jsonl", "find me")

	c := &ClaudeCodeController{
		projectsDir: projects,
		projectDir:  filepath.Join(projects, "-what-magmux-guessed"), // does not exist
		wantPrompt:  "find me",
		spawnedAt:   time.Now().Add(-time.Second), // inside broadScanWindow
	}
	if got := c.findActiveSession(); got != want {
		t.Errorf("widened scan failed: got %q, want %q", got, want)
	}
}

// TestFindActiveSessionStopsWideningAfterWindow bounds the fallback: the
// widened scan walks every project directory, so a pane whose transcript
// never appears must not keep paying for it on the render goroutine.
func TestFindActiveSessionStopsWideningAfterWindow(t *testing.T) {
	projects := t.TempDir()
	real := filepath.Join(projects, "-some-other-encoding")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, real, "ours.jsonl", "find me")

	c := &ClaudeCodeController{
		projectsDir: projects,
		projectDir:  filepath.Join(projects, "-what-magmux-guessed"),
		wantPrompt:  "find me",
		spawnedAt:   time.Now().Add(-2 * broadScanWindow), // long past the window
	}
	if got := c.findActiveSession(); got != "" {
		t.Errorf("kept widening past broadScanWindow: got %q, want \"\"", got)
	}
}

// TestFindActiveSessionPrefersFreshTranscript covers issue #3's interactive
// fallback: a concurrent claude session in the same cwd is already on disk
// when we attach, so it must lose to the transcript that appears afterwards
// even if the concurrent one was modified more recently.
func TestFindActiveSessionPrefersFreshTranscript(t *testing.T) {
	dir := t.TempDir()
	concurrent := writeTranscript(t, dir, "concurrent.jsonl", "another session")

	c := &ClaudeCodeController{
		projectDir: dir,
		wantPrompt: "", // interactive mode — no prompt to match on
		spawnedAt:  time.Now().Add(-time.Minute),
	}
	// First scan establishes the baseline (records `concurrent` as pre-existing).
	if got := c.findActiveSession(); got != concurrent {
		t.Fatalf("baseline scan: got %q, want the only file present %q", got, concurrent)
	}

	// Our pane's transcript now appears, and the concurrent one keeps being
	// appended to, so it has the newer mtime.
	ours := writeTranscript(t, dir, "ours.jsonl", "our session")
	if err := os.Chtimes(concurrent, time.Now(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := c.findActiveSession(); got != ours {
		t.Errorf("picked %q, want the freshly-created %q", filepath.Base(got), filepath.Base(ours))
	}
}
