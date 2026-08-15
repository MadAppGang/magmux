package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// --- injected instructions start a clean turn -----------------------------

// TestConsumeInjectedClearsPreviousResponse pins that an injected instruction
// starts a turn with no answer attached. consumeInjected demotes a settled
// snapshot back to working; if it leaves LastResponse standing, pollControllers
// (which always emits the `response` key) broadcasts the *previous* turn's text
// against the new turn, and a tool-only turn — or a lagging transcript — never
// overwrites it.
func TestConsumeInjectedClearsPreviousResponse(t *testing.T) {
	p := &Pane{} // not idle: a fresh instruction just cleared inputReady
	c := &ClaudeCodeController{pane: p, snap: Snapshot{
		State:        CtrlAwaitingInput,
		LastResponse: "Added the parser.",
		LastTool:     "Edit",
		CompletedAt:  time.Now(),
	}}

	c.NotifyInput()
	snap, err := c.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if snap.State != CtrlWorking {
		t.Fatalf("state = %v, want working", snap.State)
	}
	if snap.LastResponse != "" {
		t.Errorf("LastResponse = %q, want \"\" — the new turn inherited the previous turn's answer",
			snap.LastResponse)
	}
	if snap.LastTool != "" {
		t.Errorf("LastTool = %q, want \"\"", snap.LastTool)
	}
}

// TestInjectedToolOnlyTurnReportsNoResponse is the whole failure chain in one
// test, with no transcript on disk at all — the case consumeInjected exists for.
// Turn 1 answers in words; an instruction is injected; turn 2 is pure tool calls
// and settles from the terminal's own idle detection. The snapshot magmux
// broadcasts for turn 2 must carry no response, so send_and_wait reports the
// empty turn (and describeTurn's awaiting-input-with-no-response branch, which
// appends the captured screen, becomes reachable) instead of replaying turn 1.
func TestInjectedToolOnlyTurnReportsNoResponse(t *testing.T) {
	p := &Pane{}
	c := &ClaudeCodeController{pane: p, snap: Snapshot{State: CtrlWorking}}

	// Turn 1 finishes with an answer and the pane goes idle.
	c.snap.LastResponse = "Added the parser."
	p.inputReady, p.inputSignal, p.inputReadyAt = true, "osc", time.Now()
	if snap, _ := c.Poll(); snap.State != CtrlAwaitingInput || snap.LastResponse != "Added the parser." {
		t.Fatalf("turn 1: state = %v, response = %q", snap.State, snap.LastResponse)
	}

	// A pilot injects turn 2. injectPTY clears the pane's completion state the
	// way a keystroke would, and NotifyInput tells the controller.
	p.inputReady, p.inputSignal = false, ""
	c.NotifyInput()
	if snap, _ := c.Poll(); snap.State != CtrlWorking || snap.LastResponse != "" {
		t.Fatalf("after inject: state = %v, response = %q, want working with no response",
			snap.State, snap.LastResponse)
	}

	// Turn 2 is Edit + Bash with no assistant text, and settles from the
	// terminal alone (no stop_hook_summary, transcript never discovered).
	p.inputReady, p.inputSignal, p.inputReadyAt = true, "osc", time.Now()
	snap, _ := c.Poll()
	if snap.State != CtrlAwaitingInput {
		t.Fatalf("turn 2: state = %v, want awaiting_input", snap.State)
	}
	if snap.LastResponse != "" {
		t.Errorf("turn 2 reported %q — that is turn 1's answer against a tool-only turn",
			snap.LastResponse)
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

// --- turn history ---------------------------------------------------------

// writeJSONL writes a transcript verbatim, one JSONL entry per line, so a test
// can state the exact record Claude Code would have produced.
func writeJSONL(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// controllerOn returns a controller already locked onto path, as pollTranscript
// leaves one after discovery has succeeded.
func controllerOn(path string) *ClaudeCodeController {
	c := &ClaudeCodeController{}
	c.setSessionPath(path)
	return c
}

func jsonQuote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTranscriptCarriesFullTextAndToolDetail is the whole gap in one test.
// Snapshot exposes the LATEST response and the NAME of the latest tool, and
// read_pane cut that response at 300 characters; none of it could answer "what
// did the tool actually do". The record on disk has all of it, so Transcript
// must hand it over whole: no truncation of the text at this layer, and every
// tool call carrying its input and its result.
func TestTranscriptCarriesFullTextAndToolDetail(t *testing.T) {
	dir := t.TempDir()
	// No trailing space: the parser trims each text block, and a fixture that
	// ends in one would fail the containment check for a reason that has
	// nothing to do with truncation.
	long := strings.TrimSpace(strings.Repeat("the migration touched every table. ", 40))
	if len(long) <= 300 {
		t.Fatal("fixture is too short to prove anything about the 300-char cut")
	}
	path := writeJSONL(t, dir, "s.jsonl",
		`{"type":"user","cwd":"/tmp/proj","timestamp":"2026-08-15T10:00:00.000Z","message":{"content":"audit the parser"}}`,
		`{"type":"assistant","timestamp":"2026-08-15T10:00:01.000Z","message":{"model":"claude-opus-5","content":[`+
			`{"type":"text","text":`+jsonQuote(t, long)+`},`+
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./...","timeout":120}}]}}`,
		`{"type":"user","timestamp":"2026-08-15T10:00:09.000Z","message":{"content":[`+
			`{"type":"tool_result","tool_use_id":"t1","content":"ok  magmux 0.4s"}]}}`,
		`{"type":"assistant","timestamp":"2026-08-15T10:00:10.000Z","message":{"content":[{"type":"text","text":"Done."}]}}`,
	)

	turns, err := controllerOn(path).Transcript(10)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	// Two turns, not three: the tool RESULT is filed as a `user` entry by
	// Claude Code and must not become a turn of its own, exactly as the tailer
	// must not treat it as a turn boundary.
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (a prompt and the reply): %+v", len(turns), turns)
	}
	if turns[0].Role != TurnUser || turns[0].Text != "audit the parser" {
		t.Errorf("turn 0 = %q/%q, want user/\"audit the parser\"", turns[0].Role, turns[0].Text)
	}
	if turns[0].Timestamp.IsZero() {
		t.Error("turn 0 carries no timestamp")
	}

	a := turns[1]
	if a.Role != TurnAssistant {
		t.Fatalf("turn 1 role = %q, want assistant", a.Role)
	}
	if !strings.Contains(a.Text, long) {
		t.Errorf("the long response did not survive whole (got %d chars, want at least %d)",
			len(a.Text), len(long))
	}
	if !strings.Contains(a.Text, "Done.") {
		t.Error("the second assistant entry of the same turn was dropped instead of appended")
	}
	if len(a.Tools) != 1 {
		t.Fatalf("got %d tool calls, want 1: %+v", len(a.Tools), a.Tools)
	}
	call := a.Tools[0]
	if call.Name != "Bash" {
		t.Errorf("tool name = %q, want Bash", call.Name)
	}
	if !strings.Contains(call.Input, "go test ./...") {
		t.Errorf("tool input = %q, want it to carry the command", call.Input)
	}
	if call.Result != "ok  magmux 0.4s" {
		t.Errorf("tool result = %q, want the result the tool returned", call.Result)
	}
}

// TestTranscriptToolResultBlockArray covers the other shape Claude Code writes
// results in: a block array rather than a bare string.
func TestTranscriptToolResultBlockArray(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONL(t, dir, "s.jsonl",
		`{"type":"user","message":{"content":"read it"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t9","name":"Read","input":{"file_path":"/x"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t9","content":[`+
			`{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}}`,
	)
	turns, err := controllerOn(path).Transcript(10)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(turns) != 2 || len(turns[1].Tools) != 1 {
		t.Fatalf("unexpected shape: %+v", turns)
	}
	if got := turns[1].Tools[0].Result; got != "line one\nline two" {
		t.Errorf("result = %q, want both text blocks flattened", got)
	}
}

// TestTranscriptReturnsTheLastNTurnsInOrder pins both halves of the contract:
// N means the NEWEST n, and they come back oldest first so the reader can read
// them as a conversation. Asking for more than exists returns what exists.
func TestTranscriptReturnsTheLastNTurnsInOrder(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 4; i++ {
		lines = append(lines,
			`{"type":"user","message":{"content":"prompt `+string(rune('0'+i))+`"}}`,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"answer `+string(rune('0'+i))+`"}]}}`)
	}
	path := writeJSONL(t, dir, "s.jsonl", lines...)
	c := controllerOn(path)

	turns, err := c.Transcript(3)
	if err != nil {
		t.Fatalf("Transcript(3): %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("Transcript(3) returned %d turns", len(turns))
	}
	want := []string{"answer 3", "prompt 4", "answer 4"}
	for i, w := range want {
		if turns[i].Text != w {
			t.Errorf("turn %d = %q, want %q — the last 3 turns, oldest first", i, turns[i].Text, w)
		}
	}

	all, err := c.Transcript(50)
	if err != nil {
		t.Fatalf("Transcript(50): %v", err)
	}
	if len(all) != 8 {
		t.Fatalf("asking for more turns than exist returned %d, want all 8", len(all))
	}
	if all[0].Text != "prompt 1" || all[7].Text != "answer 4" {
		t.Errorf("full history is out of order: first %q, last %q", all[0].Text, all[7].Text)
	}
}

// TestTranscriptWithoutDiscoveryIsAnHonestError is the invariant that matters
// most in the field. ~/.claude/projects is undocumented territory owned by
// Claude Code: discovery lags at session start and sometimes fails outright,
// and a pane can be perfectly healthy with no transcript located. Returning an
// empty slice there would tell an agent "this session has said nothing", which
// is a claim about the session and is false.
func TestTranscriptWithoutDiscoveryIsAnHonestError(t *testing.T) {
	var undiscovered ClaudeCodeController
	turns, err := undiscovered.Transcript(5)
	if !errors.Is(err, errNoTranscript) {
		t.Fatalf("err = %v, want errNoTranscript", err)
	}
	if len(turns) != 0 {
		t.Errorf("an undiscovered transcript returned %d turns", len(turns))
	}

	// Located once and gone since — rotated, compacted, deleted. Still "we
	// cannot reach the record", never "the session said nothing".
	gone := controllerOn(filepath.Join(t.TempDir(), "vanished.jsonl"))
	if _, err := gone.Transcript(5); !errors.Is(err, errNoTranscript) {
		t.Errorf("a vanished transcript gave err = %v, want errNoTranscript", err)
	}
}

// TestTranscriptReadsTheFileTheTailerLockedOnto is why Transcript reuses
// discovery instead of resolving a path of its own. Two readers that each
// decide which file belongs to a pane are two readers that can disagree, and
// the one that is wrong reports another session's work as this pane's.
func TestTranscriptReadsTheFileTheTailerLockedOnto(t *testing.T) {
	dir := t.TempDir()
	writeJSONL(t, dir, "someone-else.jsonl",
		`{"type":"user","message":{"content":"another session entirely"}}`)
	writeJSONL(t, dir, "ours.jsonl",
		`{"type":"user","message":{"content":"reply with only the word hello"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`)

	c := &ClaudeCodeController{
		projectDir: dir,
		wantPrompt: "reply with only the word hello",
		spawnedAt:  time.Now().Add(-time.Minute),
	}
	if err := c.pollTranscript(); err != nil {
		t.Fatalf("pollTranscript: %v", err)
	}
	if c.sessionPath == "" {
		t.Fatal("the tailer never locked on, so the test proves nothing")
	}

	turns, err := c.Transcript(10)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(turns) != 2 || turns[0].Text != "reply with only the word hello" {
		t.Fatalf("Transcript read a different session than the tailer: %+v", turns)
	}
}

// TestParseClaudeTurnsBoundsItsMemory covers the cap the reader owes the
// machine. It never shortens a field — that is presentation's job — so the only
// bound it can enforce is dropping whole turns, oldest first, and the newest
// turn always survives because it is the one the caller is acting on.
func TestParseClaudeTurnsBoundsItsMemory(t *testing.T) {
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines,
			`{"type":"user","message":{"content":`+jsonQuote(t, strings.Repeat("x", 500))+`}}`)
	}
	body := strings.Join(lines, "\n")

	turns, err := parseClaudeTurns(strings.NewReader(body), 6, 1200)
	if err != nil {
		t.Fatalf("parseClaudeTurns: %v", err)
	}
	if len(turns) == 0 || len(turns) >= 6 {
		t.Fatalf("byte budget was not enforced: got %d turns of 6, each 500 bytes, budget 1200",
			len(turns))
	}

	// Even a single turn larger than the whole budget comes back, whole: a
	// caller that asked for history and gets nothing cannot tell that apart
	// from a session that has said nothing.
	one, err := parseClaudeTurns(strings.NewReader(body), 6, 10)
	if err != nil {
		t.Fatalf("parseClaudeTurns: %v", err)
	}
	if len(one) != 1 || len(one[0].Text) != 500 {
		t.Fatalf("got %d turns; want exactly the newest one, untruncated", len(one))
	}
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
