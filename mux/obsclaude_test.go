package mux

// Phase 4's magmux-side surface: the one encoder, the moved turn types, and
// the launch-directory port.
//
// NOTHING here reads ~/.claude, probes a live pid or speaks to tmux. The panes
// are PTY-less control panes carrying a pid nobody ever signals.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
)

// ── one encoder ─────────────────────────────────────────────────────────────

// There is ONE project-directory encoder, and magmux's own is it.
//
// The legacy implementation was wrong in two independent ways and both failed
// silently — the derived directory simply did not exist, discovery fell through
// to the content scan, and a controller sat in `starting` until it found the
// transcript some other way. The cases below are the two divergences, and they
// are the two an equality check against the adapter cannot be faked past: a
// character outside the basic multilingual plane, which is a surrogate PAIR and
// becomes TWO dashes, and a path whose encoding exceeds 200 characters, which
// is truncated and hashed.
func TestTheProjectDirectoryEncoderIsTheVerifiedOne(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{"an ordinary path", "/Users/user/dev/magmux"},
		{"a worktree path with dots", "/Users/user/dev/magmux/.claude/worktrees/finish-1"},
		{"an underscore and a space", "/Users/user/Documents/Claude/Projects/Models bioses"},
		{"a character outside the BMP", "/Users/user/dev/\U0001F600x"},
		{"deep enough to hit the hash branch", "/Users/user/" + strings.Repeat("deep/", 60) + "end"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := encodeProjectDir(c.dir)
			want := adapters.EncodeProjectDir(c.dir)
			if got != want {
				t.Errorf("encodeProjectDir(%q)\n got: %q\nwant: %q", c.dir, got, want)
			}
		})
	}

	// The two divergences, asserted as PROPERTIES rather than as a comparison
	// with the adapter, so that this test still means something if both sides
	// were changed together.
	emoji := encodeProjectDir("/Users/user/dev/\U0001F600x")
	if want := len("-Users-user-dev-") + 2 + 1; len(emoji) != want {
		t.Errorf("a non-BMP character encoded to %d characters (%q); it is a surrogate pair and "+
			"must become TWO dashes, for a total of %d", len(emoji), emoji, want)
	}
	deep := encodeProjectDir("/Users/user/" + strings.Repeat("deep/", 60) + "end")
	if len(deep) <= 200 {
		t.Fatalf("the deep path encoded to %d characters; the test no longer reaches the hash branch",
			len(deep))
	}
	if !strings.Contains(deep[200:], "-") {
		t.Errorf("a path over 200 characters encoded to %q, with no truncate-and-hash suffix", deep)
	}
}

// ── the moved turn types ────────────────────────────────────────────────────

// Turn and ToolCall are ALIASES of the observation layer's types, not copies.
//
// An alias is the same type, which is what lets them move out of package main
// without touching the TranscriptReader contract or any caller. A future
// `type Turn struct{…}` here would compile everywhere else in this package and
// break the moment anything tried to hand a Turn across the boundary — so the
// assignment below, with no conversion, is the whole test.
func TestTurnAndToolCallAreTheObservationLayersTypes(t *testing.T) {
	var (
		mine  Turn
		other obs.Turn
	)
	other = mine
	mine = other
	_ = mine

	var (
		myCall    ToolCall
		otherCall obs.ToolCall
	)
	otherCall = myCall
	myCall = otherCall
	_ = myCall

	if TurnUser != obs.TurnUser || TurnAssistant != obs.TurnAssistant {
		t.Errorf("the role constants have diverged: %q/%q here, %q/%q there",
			TurnUser, TurnAssistant, obs.TurnUser, obs.TurnAssistant)
	}
}

// The parser still produces the same turns through the alias, including a tool
// call paired with its result — which is the part that needs the unexported
// correlation field, and therefore the part that proves the field travelled
// with the type.
func TestTheMovedParserStillPairsAToolCallWithItsResult(t *testing.T) {
	const body = `{"type":"user","message":{"role":"user","content":"audit the parser"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"a.go\nb.go"}]}}
`
	turns, err := parseClaudeTurns(strings.NewReader(body), 6, 1<<20)
	if err != nil {
		t.Fatalf("parseClaudeTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("parsed %d turns, want 2", len(turns))
	}
	if turns[0].Role != TurnUser || turns[0].Text != "audit the parser" {
		t.Errorf("first turn = %+v, want the user's prompt", turns[0])
	}
	if len(turns[1].Tools) != 1 {
		t.Fatalf("second turn has %d tool calls, want 1", len(turns[1].Tools))
	}
	call := turns[1].Tools[0]
	if call.Name != "Bash" || !strings.Contains(call.Input, "ls") {
		t.Errorf("tool call = %+v, want Bash with its input", call)
	}
	if !strings.Contains(call.Result, "a.go") {
		t.Errorf("tool result = %q; the result did not reach the call it answers, which is what "+
			"the unexported correlation id is for", call.Result)
	}
}

// TestTheControllerFindsTheProjectDirectoryOfANonASCIILaunchDir is the
// regression at the level it bit: the pane controller's own Start, deriving
// the directory it will tail from the directory the pane launches claude in.
//
// main's encoder replaced RUNES with dashes; Claude Code replaces UTF-16 CODE
// UNITS, so a character outside the basic multilingual plane — an emoji in a
// directory name — is TWO dashes in the name Claude writes and was one in the
// name magmux looked for. The directory magmux derived did not exist, and
// discovery fell back to the scan without a word. The expected name here is
// computed by an independent rule, per UTF-16 code unit, so this fails against
// the per-rune encoder and does not merely compare the adapter with itself.
//
// Nothing under ~/.claude is touched: CLAUDE_CONFIG_DIR is a temp directory.
func TestTheControllerFindsTheProjectDirectoryOfANonASCIILaunchDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(t.TempDir(), "proj-\U0001F600-é")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	c := newClaudeCodeController(paneWithCmd("cd " + dir + " && claude 'hi'"))
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c.cwd != dir {
		t.Fatalf("cwd = %q, want the cd target %q", c.cwd, dir)
	}

	perCodeUnit := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
				b.WriteRune(r)
				continue
			}
			b.WriteString(strings.Repeat("-", len(utf16.Encode([]rune{r}))))
		}
		return b.String()
	}
	perRune := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
		}
		return b.String()
	}
	want := perCodeUnit(dir)
	if len(want) > 200 {
		t.Skipf("the temp path is %d characters encoded, past the 200-character hash branch; "+
			"this case is about the code-unit rule", len(want))
	}
	if perRune(dir) == want {
		t.Fatal("the launch directory does not tell the two rules apart; the test is vacuous")
	}
	if got := filepath.Base(c.projectDir); got != want {
		t.Errorf("the controller will tail %q, want %q — Claude Code writes a non-BMP character as "+
			"two dashes, one per UTF-16 code unit", got, want)
	}
}

// ── the launch-directory port ───────────────────────────────────────────────

// The port answers with what magmux passed to the child, found by the same
// ancestry walk the pane gate uses: the session's pid is the tool's and the
// pane's is the shell's, so they are never equal.
func TestLaunchDirIsTheDirectoryMagmuxSpawnedWith(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	const spawned = "/Users/user/dev/sampleapp"

	p := m.paneByID(0)
	p.mu.Lock()
	p.cmd = &exec.Cmd{Process: &os.Process{Pid: 400}, Dir: spawned}
	p.mu.Unlock()
	givePID(t, m.paneByID(1), 500)

	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{
		900: 850, // claude
		850: 400, // the shell magmux spawned
		400: 300,
		300: 1,
	})}

	dir, ok := loc.LaunchDir(900)
	if !ok {
		t.Fatal("LaunchDir(900) found nothing; the tool's pid is never the pane's")
	}
	if dir != spawned {
		t.Errorf("LaunchDir(900) = %q, want %q", dir, spawned)
	}
}

// A pane started with no directory of its own inherited magmux's, so that is
// the answer. magmux never chdirs, which is what makes it stable.
func TestLaunchDirFallsBackToMagmuxsOwnDirectory(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)
	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{700: 400, 400: 1})}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir, ok := loc.LaunchDir(700)
	if !ok {
		t.Fatal("LaunchDir(700) found nothing")
	}
	if dir != wd {
		t.Errorf("LaunchDir(700) = %q, want magmux's own directory %q", dir, wd)
	}
}

// A session that is not in any of this magmux's panes gets NO answer, rather
// than magmux's own directory.
//
// The difference matters because a wrong directory is not harmless here even
// though it cannot produce a wrong session: it produces a stat in a directory
// that has nothing to do with the session, and then the scan runs anyway. An
// answer nobody can stand behind is worse than no answer.
func TestLaunchDirRefusesASessionThatIsNotOurs(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), 400)
	loc := &muxPaneLocator{m: m, ppid: tree(map[int]int{999: 998, 998: 1})}

	if dir, ok := loc.LaunchDir(999); ok {
		t.Errorf("LaunchDir(999) = %q, ok; that session belongs to no pane of ours", dir)
	}
}

// ── the controller's registry read, through adapters ────────────────────────

// TestTheControllerTreatsATornRegistryFileAsNotFound pins the mapping from the
// adapter's decode onto the controller's "not found", which its cache and its
// scan act on: exactly what the old json.Unmarshal refused is refused, and a
// record with any other shape of extra keys is still found.
func TestTheControllerTreatsATornRegistryFileAsNotFound(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		return p
	}
	const whole = `{"status":"waiting","waitingFor":"input needed","sessionId":"s","pid":1,"extra":[1,{}]}`
	for name, body := range map[string]string{
		"torn":                 whole[:len(whole)/2],
		"empty":                "",
		"an array":             `[1]`,
		"a wrong-typed status": `{"status":3,"sessionId":"s"}`,
		"trailing garbage":     whole + ` }`,
	} {
		if e, ok := readRegEntry(write(name+".json", body)); ok {
			t.Errorf("%s: readRegEntry found %+v; the controller must treat it as not found", name, e)
		}
	}
	e, ok := readRegEntry(write("whole.json", whole))
	if !ok || e.Status != "waiting" || e.WaitingFor != "input needed" || e.SessionID != "s" {
		t.Errorf("a whole record read as %+v, %v", e, ok)
	}
	if e, ok := readRegEntry(write("null.json", "null")); !ok || e != (regEntry{}) {
		t.Errorf("a JSON null record read as %+v, %v; it was always found, as the zero entry", e, ok)
	}
}

// TestTheControllerTreatsAnUnknownRegistryStatusAsNeitherWaitingNorBusy: the
// status is compared through adapters.DecodeStatus, so a word this build does
// not know — a newer Claude's — is neither of the two the gate acts on, and the
// two it knows are exactly themselves.
func TestTheControllerTreatsAnUnknownRegistryStatusAsNeitherWaitingNorBusy(t *testing.T) {
	for _, raw := range []string{"compacting", "", "Waiting", "BUSY"} {
		if regStatusIs(raw, obs.StatusWaiting) || regStatusIs(raw, obs.StatusBusy) {
			t.Errorf("registry status %q counted as waiting or busy", raw)
		}
	}
	if !regStatusIs("waiting", obs.StatusWaiting) || regStatusIs("waiting", obs.StatusBusy) {
		t.Error(`"waiting" is not exactly waiting`)
	}
	if !regStatusIs("busy", obs.StatusBusy) || regStatusIs("busy", obs.StatusWaiting) {
		t.Error(`"busy" is not exactly busy`)
	}
}
