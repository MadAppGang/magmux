//go:build claude_e2e

package mux

// ── real-tool validation: the input gate against a REAL Claude Code ──────────
//
// TestClaudeE2E drives V1.1–V1.6 of the input-gate validation criteria against
// the user's own Claude Code, with the user's own ~/.claude config, through the
// magmux binary TestMain builds. It costs real model calls (haiku, a handful of
// one-word turns), so it is opt-in three times over and NEVER runs in CI:
//
//	MAGMUX_CLAUDE_E2E=1 go test -tags claude_e2e -run TestClaudeE2E -count=1 -timeout 30m -v ./mux/
//
// The `claude_e2e` build tag is the first gate: without it this file is not
// compiled at all, so the default `go test ./...` contains no test that reads
// the real Claude config or runs `claude`. That is the one, owner-accepted
// exception to the rule that no test may do either, and no CI job passes the
// tag. This must stay the ONLY file carrying it.
//
// It skips, saying why, unless MAGMUX_CLAUDE_E2E=1 AND `claude` is on PATH.
// MAGMUX_E2E_LOG=<file> appends every log line (timelines, every reply's
// fields) to that file as well as to t.Log; MAGMUX_E2E_RUNS overrides the
// early-send repeat count (default 5, the criterion's minimum).
//
// Every magmux here is headless, on its own short --sock-dir, with one pane.
// The child env has CLAUDE_CODE_CHILD_SESSION and CLAUDECODE removed: run from
// inside a Claude Code session they are inherited, and they suppress the
// child's session registry and transcript, which is exactly the evidence the
// gate and acceptance read.
//
// What each criterion asserts:
//
//	V1.1 early send (≤1.5s after spawn) ×N on a DIRECT `-e claude` pane and a
//	     DECLARED `--controller claude-code` wrapper (`sh -c 'exec claude …'`):
//	     every reply is ok-with-evidence or busy, and an ok reply whose prompt
//	     never became a turn (read off the pane after the reply) is a failure.
//	V1.2 a late send (≥5s): ok with evidence, and the turn happens.
//	V1.3 a direct pane never paints `-- INSERT --`, even when the caller's own
//	     --settings asks for vim, and the caller's other keys survive the merge.
//	V1.4 a send while Claude is working: accepted with evidence "history".
//	V1.5 a permission dialog: a plain send is busy dialog:permission with
//	     nothing typed; dismissDialogs declines it with Escape (the command
//	     never runs) and then types, submits and is accepted.
//	V1.6 a plain `sh` pane: the ungated reply shape and no wait.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ce2eEnv       = "MAGMUX_CLAUDE_E2E"
	ce2eLogEnv    = "MAGMUX_E2E_LOG"
	ce2eRunsEnv   = "MAGMUX_E2E_RUNS"
	ce2eModel     = "haiku"
	ce2eEarlyMax  = 1500 * time.Millisecond // V1.1's "early"
	ce2eLateAfter = 5 * time.Second         // V1.2's "late"
	ce2eTurnWait  = 120 * time.Second       // a one-word haiku turn, generously
	ce2eReplyWait = 90 * time.Second        // > the default send budget (~18s) with room
)

// ce2eStrippedEnv are removed from the child env (see the file comment), plus
// magmux's own exports, which the test's magmux sets afresh for its panes.
var ce2eStrippedEnv = []string{"CLAUDE_CODE_CHILD_SESSION", "CLAUDECODE", "MAGMUX_SOCK", "MAGMUX_SOCK_DIR", "MAGMUX_THEME"}

func TestClaudeE2E(t *testing.T) {
	if os.Getenv(ce2eEnv) != "1" {
		t.Skipf("real-Claude validation is opt-in: set %s=1 (and have `claude` on PATH) to run it; it makes real model calls and never runs in CI", ce2eEnv)
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("%s=1 but `claude` is not on PATH: %v", ce2eEnv, err)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY test requires darwin or linux")
	}
	lg := newCE2ELog(t)
	out, _ := exec.Command(claudePath, "--version").CombinedOutput()
	lg.logf("claude: %s (%s)", strings.TrimSpace(string(out)), claudePath)
	lg.logf("user editorMode: %s", ce2eUserEditorMode())
	lg.logf("SHELL=%s (the -e line runs under $SHELL -l -c; a non-POSIX shell is never direct)", os.Getenv("SHELL"))

	runs := 5
	if v, err := strconv.Atoi(os.Getenv(ce2eRunsEnv)); err == nil && v > 0 {
		runs = v
	}

	t.Run("V1.1_early_direct", func(t *testing.T) {
		for i := range runs {
			ce2eEarlySend(t, newCE2ELog(t), i, runs, false)
		}
	})
	t.Run("V1.1_early_wrapper", func(t *testing.T) {
		for i := range runs {
			ce2eEarlySend(t, newCE2ELog(t), i, runs, true)
		}
	})
	t.Run("V1.2_late", func(t *testing.T) { ce2eLateSend(t, newCE2ELog(t)) })
	t.Run("V1.3_footer_and_caller_settings", func(t *testing.T) { ce2eFooterAndSettings(t, newCE2ELog(t)) })
	t.Run("V1.4_midturn", func(t *testing.T) { ce2eMidTurn(t, newCE2ELog(t)) })
	t.Run("V1.5_permission_dialog", func(t *testing.T) { ce2ePermissionDialog(t, newCE2ELog(t)) })
	t.Run("V1.6_shell", func(t *testing.T) { ce2eShell(t, newCE2ELog(t)) })
}

// ── the criteria ────────────────────────────────────────────────────────────

func ce2eDirectCmd(extra string) string {
	cmd := "claude --model " + ce2eModel
	if extra != "" {
		cmd += " " + extra
	}
	return cmd
}

// ce2eWrapperCmd is a wrapper that runs claude itself, the way claudish does:
// not a direct invocation, so it is gated only because --controller declares
// it, and its editor override is delegated. Like claudish's overlay it passes
// its own normal-mode --settings, so the user's vim setting does not turn
// every send into busy/vim_mode.
func ce2eWrapperCmd() string {
	inner := `exec claude --model ` + ce2eModel + ` --settings '{"editorMode":"normal"}'`
	return "sh -c " + ce2eShellQuote(inner)
}

func ce2eEarlySend(t *testing.T, lg *ce2eLog, run, runs int, wrapper bool) {
	t.Helper()
	shape := "direct"
	args := []string{"-e", ce2eDirectCmd("")}
	if wrapper {
		shape = "wrapper"
		args = []string{"--controller", "claude-code", "-e", ce2eWrapperCmd()}
	}
	h := startCE2E(t, lg, args...)
	defer h.stop()
	h.watchPane(0, !wrapper)

	entry := h.paneEntry(0)
	lg.logf("[%s run %d/%d] list: %s", shape, run+1, runs, ce2eFields(entry, "controller", "gateBasis", "editorOverride", "cmd"))
	wantBasis := "direct"
	if wrapper {
		wantBasis = "declared"
	}
	if got := ce2eStr(entry, "gateBasis"); got != wantBasis {
		t.Fatalf("[%s] gateBasis = %q, want %q (the pane is not gated; nothing below would mean anything)", shape, got, wantBasis)
	}
	if wrapper && ce2eStr(entry, "editorOverride") != "delegated" {
		t.Errorf("[wrapper] editorOverride = %q, want delegated", ce2eStr(entry, "editorOverride"))
	}

	// Spread the sends across the early window: 0, then evenly up to ~1.2s.
	at := time.Duration(run) * (1200 * time.Millisecond) / time.Duration(max(runs-1, 1))
	h.sleepUntil(at)
	tok := ce2eToken("E" + strconv.Itoa(run))
	offset := h.since()
	if offset > ce2eEarlyMax {
		lg.logf("[%s run %d] WARN: the send left %s after spawn, past the %s early window (slow socket bind?)", shape, run+1, offset, ce2eEarlyMax)
	}
	rep, took := h.send(map[string]any{"pane": 0, "text": ce2ePrompt(tok), "label": "e2e early " + shape})
	lg.logf("[%s run %d] early send at +%s, reply after %s: %s", shape, run+1, offset.Round(time.Millisecond), took.Round(time.Millisecond), ce2eJSON(rep))
	h.judgeSubmit(shape, tok, rep, true)
}

func ce2eLateSend(t *testing.T, lg *ce2eLog) {
	h := startCE2E(t, lg, "-e", ce2eDirectCmd(""))
	defer h.stop()
	h.watchPane(0, true)
	h.sleepUntil(ce2eLateAfter)
	// "Late" means past boot: wait for the published view to say ready too,
	// so this is the steady-state path and not a second early send.
	h.waitInput(0, 30*time.Second, func(in map[string]any) bool { return in["ready"] == true })
	tok := ce2eToken("L")
	offset := h.since()
	rep, took := h.send(map[string]any{"pane": 0, "text": ce2ePrompt(tok), "label": "e2e late"})
	lg.logf("late send at +%s, reply after %s: %s", offset.Round(time.Millisecond), took.Round(time.Millisecond), ce2eJSON(rep))
	h.judgeSubmit("late", tok, rep, false)
}

func ce2eFooterAndSettings(t *testing.T, lg *ce2eLog) {
	// The caller's own --settings asks for vim AND carries a key of its own.
	// The merge must flip editorMode and keep the other key, byte for byte.
	h := startCE2E(t, lg, "-e", ce2eDirectCmd(`--settings '{"editorMode":"vim","spinnerTipsEnabled":false}'`))
	defer h.stop()
	h.watchPane(0, true)
	entry := h.paneEntry(0)
	lg.logf("list: %s", ce2eFields(entry, "gateBasis", "editorOverride", "cmd"))
	if got := ce2eStr(entry, "editorOverride"); got != "merged" {
		t.Errorf("editorOverride = %q, want merged", got)
	}
	cmd := ce2eStr(entry, "cmd")
	for _, want := range []string{`"editorMode":"normal"`, `"spinnerTipsEnabled":false`} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the rewritten command does not carry %s: %s", want, cmd)
		}
	}
	if strings.Contains(cmd, `"editorMode":"vim"`) {
		t.Errorf("the caller's editorMode survived the merge: %s", cmd)
	}
	// Boot fully, then type and submit: the footer is checked throughout by
	// watchPane, which fails the test on any `-- INSERT --`.
	h.waitInput(0, 30*time.Second, func(in map[string]any) bool { return in["ready"] == true })
	tok := ce2eToken("S")
	rep, took := h.send(map[string]any{"pane": 0, "text": ce2ePrompt(tok), "label": "e2e settings"})
	lg.logf("send, reply after %s: %s", took.Round(time.Millisecond), ce2eJSON(rep))
	h.judgeSubmit("settings", tok, rep, false)
}

func ce2eMidTurn(t *testing.T, lg *ce2eLog) {
	// The first turn sleeps in a pre-approved Bash call, so there is a long,
	// deterministic window in which Claude is working.
	h := startCE2E(t, lg, "-e", ce2eDirectCmd(`--allowedTools 'Bash(sleep:*)'`))
	defer h.stop()
	h.watchPane(0, true)
	h.waitInput(0, 30*time.Second, func(in map[string]any) bool { return in["ready"] == true })

	tok1 := ce2eToken("M1")
	first := "Run `sleep 15` with the Bash tool, then reply with only the word " + tok1 + "."
	rep, took := h.send(map[string]any{"pane": 0, "text": first, "label": "e2e mid 1"})
	firstAt := time.Now() // the first turn cannot finish before this + its 15s sleep
	lg.logf("first send, reply after %s: %s", took.Round(time.Millisecond), ce2eJSON(rep))
	h.requireOkEvidence("mid-turn first", rep)

	// Working: the sleep is on screen (and no permission dialog took its place).
	h.waitScreen(0, 60*time.Second, "the sleep tool call", func(s string) bool {
		return strings.Contains(s, "sleep 15")
	})
	if in := h.inputView(0); strings.HasPrefix(ce2eStr(in, "reason"), "dialog:") {
		t.Fatalf("a dialog (%v) showed instead of the pre-approved sleep; check --allowedTools against this Claude version", in["reason"])
	}
	time.Sleep(time.Second)

	tok2 := ce2eToken("M2")
	rep, took = h.send(map[string]any{"pane": 0, "text": ce2ePrompt(tok2), "label": "e2e mid 2"})
	lg.logf("mid-turn send, reply after %s: %s", took.Round(time.Millisecond), ce2eJSON(rep))
	h.requireOkEvidence("mid-turn second", rep)
	lg.logf("mid-turn send evidence = %q", ce2eStr(ce2eMap(rep["result"]), "evidence"))
	// Taken mid-turn: confirmed before the first turn's sleep could have
	// finished, and before the first turn had answered.
	if since := time.Since(firstAt); since >= 15*time.Second {
		t.Errorf("the mid-turn send was confirmed %s after the first, past its 15s sleep: not a mid-turn send", since.Round(time.Millisecond))
	}
	if ce2eAnswered(h.capture(0), tok1) {
		t.Errorf("the first turn had already answered when the mid-turn send was confirmed: not a mid-turn send")
	}

	// Both prompts are `❯` entries in the pane. Which one the MODEL then
	// answers is its own choice (given a queued prompt it may answer only the
	// second), so that is logged and never judged.
	h.waitScreen(0, ce2eTurnWait, "both prompts as ❯ entries", func(s string) bool {
		return ce2ePromptRow(s, "Run sleep 15") && ce2ePromptRow(s, "Please reply with only the word "+tok2)
	})
	deadline := time.Now().Add(ce2eTurnWait)
	for {
		s := h.capture(0)
		a1, a2 := ce2eAnswered(s, tok1), ce2eAnswered(s, tok2)
		if a1 || a2 {
			lg.logf("  +%-7s answered: %s=%v %s=%v", h.since().Round(time.Millisecond), tok1, a1, tok2, a2)
			return
		}
		if time.Now().After(deadline) {
			lg.logf("no answer carrying %s or %s within %s (model behaviour, not judged); screen:\n%s", tok1, tok2, ce2eTurnWait, s)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ce2ePromptRow: some `❯` history row in the pane begins with prefix. Claude
// renders inline code without its backticks, so prefix is the rendered text.
func ce2ePromptRow(screen, prefix string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "❯ "+prefix) {
			return true
		}
	}
	return false
}

func ce2ePermissionDialog(t *testing.T, lg *ce2eLog) {
	h := startCE2E(t, lg, "-e", ce2eDirectCmd("--permission-mode default"))
	defer h.stop()
	h.watchPane(0, true)
	h.waitInput(0, 30*time.Second, func(in map[string]any) bool { return in["ready"] == true })

	marker := filepath.Join(h.dir, "perm-"+ce2eToken("P"))
	ask := "Use the Bash tool to run exactly this command and nothing else: touch " + marker
	rep, took := h.send(map[string]any{"pane": 0, "text": ask, "label": "e2e perm ask"})
	lg.logf("ask send, reply after %s: %s", took.Round(time.Millisecond), ce2eJSON(rep))
	h.requireOkEvidence("permission ask", rep)

	h.waitInput(0, 90*time.Second, func(in map[string]any) bool { return in["reason"] == "dialog:permission" })
	lg.logf("dialog up at +%s: %s", h.since().Round(time.Millisecond), ce2eJSON(h.inputView(0)))

	// A plain send: refused while the dialog shows, and nothing typed.
	tok2 := ce2eToken("P2")
	rep, took = h.send(map[string]any{"pane": 0, "text": ce2ePrompt(tok2), "readyTimeoutMs": 2000, "label": "e2e perm plain"})
	lg.logf("plain send into the dialog, reply after %s: %s", took.Round(time.Millisecond), ce2eJSON(rep))
	if rep["ok"] == true || ce2eStr(rep, "code") != "busy" || ce2eStr(rep, "reason") != "dialog:permission" {
		t.Errorf("plain send into a permission dialog = %s, want busy dialog:permission", ce2eJSON(rep))
	}
	if rep["typed"] == true {
		t.Errorf("the refused send says it typed: %s", ce2eJSON(rep))
	}
	if strings.Contains(h.capture(0), tok2) {
		t.Errorf("the refused send's text reached the screen")
	}

	// The same with dismissDialogs: Escape declines, then type, submit, accept.
	tok3 := ce2eToken("P3")
	rep, took = h.send(map[string]any{"pane": 0, "text": ce2ePrompt(tok3), "dismissDialogs": true, "label": "e2e perm dismiss"})
	lg.logf("dismissDialogs send, reply after %s: %s", took.Round(time.Millisecond), ce2eJSON(rep))
	h.requireOkEvidence("dismissDialogs", rep)
	res := ce2eMap(rep["result"])
	if d, _ := res["dismissed"].([]any); !slices.Contains(d, any("permission")) {
		t.Errorf("dismissed = %v, want it to name permission", res["dismissed"])
	}
	if ce2eStr(res, "verified") == "" {
		t.Errorf("no verified on the dismissDialogs reply: %s", ce2eJSON(rep))
	}
	h.waitAnswer(0, tok3)
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("%s exists: the permission dialog was APPROVED, not declined", marker)
	}
}

func ce2eShell(t *testing.T, lg *ce2eLog) {
	h := startCE2E(t, lg, "-e", "sh")
	defer h.stop()
	entry := h.paneEntry(0)
	lg.logf("list: %s", ce2eFields(entry, "controller", "gateBasis", "editorOverride", "input", "cmd"))
	for _, k := range []string{"gateBasis", "editorOverride", "input"} {
		if _, ok := entry[k]; ok {
			t.Errorf("a plain sh pane carries %q: %s", k, ce2eJSON(entry))
		}
	}
	time.Sleep(500 * time.Millisecond) // let sh print its prompt
	tok := ce2eToken("SH")
	rep, took := h.send(map[string]any{"pane": 0, "text": "echo " + tok})
	lg.logf("sh send, reply after %s: %s", took.Round(time.Millisecond), ce2eJSON(rep))
	if rep["ok"] != true {
		t.Fatalf("sh send failed: %s", ce2eJSON(rep))
	}
	res := ce2eMap(rep["result"])
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"bytes", "enter", "keys", "pane"}; !slices.Equal(keys, want) {
		t.Errorf("ungated reply keys = %v, want exactly %v (byte-identical to an ungated send)", keys, want)
	}
	// No gate wait: the ungated path pauses only for the pre-Enter settle.
	if took > 2*time.Second {
		t.Errorf("an ungated send took %s; it must not wait for readiness", took)
	}
	h.waitScreen(0, 10*time.Second, "the echo's output", func(s string) bool {
		for _, line := range strings.Split(s, "\n") {
			if strings.TrimSpace(line) == tok {
				return true
			}
		}
		return false
	})
}

// ── the harness ─────────────────────────────────────────────────────────────

type ce2eHarness struct {
	t       *testing.T
	lg      *ce2eLog
	dir     string
	cmd     *exec.Cmd
	spawnAt time.Time

	conn  net.Conn
	wmu   sync.Mutex
	pmu   sync.Mutex
	next  int
	waits map[int]chan map[string]any

	done    chan struct{}
	wg      sync.WaitGroup
	stopped sync.Once
}

func startCE2E(t *testing.T, lg *ce2eLog, args ...string) *ce2eHarness {
	t.Helper()
	bin := magmuxBinForTest(t)
	// /tmp, not t.TempDir(): a macOS TempDir pushes the socket path past the
	// ~104-byte limit, and magmux would fall back to /tmp silently.
	dir, err := os.MkdirTemp("/tmp", "mmxe2e-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	id := "e2e" + ce2eToken("")
	full := append([]string{"--headless", "--no-status", "--sock-dir", dir, "--id", id}, args...)
	cmd := exec.Command(bin, full...)
	// The repo root is a directory the user already trusts, so no trust dialog.
	cmd.Dir = root
	cmd.Env = append(ce2eChildEnv(), "COLUMNS=120", "LINES=40")
	logFile, err := os.Create(filepath.Join(dir, "magmux.out"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	h := &ce2eHarness{t: t, lg: lg, dir: dir, cmd: cmd, spawnAt: time.Now(),
		waits: map[int]chan map[string]any{}, done: make(chan struct{})}
	lg.logf("magmux %s (pid %d)", strings.Join(full, " "), cmd.Process.Pid)
	t.Cleanup(func() {
		h.stop()
		logFile.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logFile.Name()); err == nil && len(b) > 0 {
				lg.logf("magmux stdout/stderr:\n%s", b)
			}
		}
		os.RemoveAll(dir)
	})

	sock := filepath.Join(dir, "magmux-"+id+".sock")
	deadline := time.Now().Add(dialWait)
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			h.conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not connect to %s: %v", sock, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	lg.logf("socket up at +%s", h.since().Round(time.Millisecond))
	h.wg.Add(1)
	go h.readLoop()
	return h
}

func (h *ce2eHarness) since() time.Duration { return time.Since(h.spawnAt) }

func (h *ce2eHarness) sleepUntil(d time.Duration) {
	if left := d - h.since(); left > 0 {
		time.Sleep(left)
	}
}

// stop ends the watchers, then magmux. Idempotent; the watchers must be gone
// before the test returns, or their t.Log would panic.
func (h *ce2eHarness) stop() {
	h.stopped.Do(func() {
		close(h.done)
		if h.conn != nil {
			h.conn.Close()
		}
		stopMagmux(h.t, h.cmd)
		h.wg.Wait()
	})
}

func (h *ce2eHarness) readLoop() {
	defer h.wg.Done()
	sc := bufio.NewScanner(h.conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil || m["type"] != "reply" {
			continue
		}
		id, ok := m["id"].(float64)
		if !ok {
			continue
		}
		h.pmu.Lock()
		ch := h.waits[int(id)]
		delete(h.waits, int(id))
		h.pmu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// request sends one message with an id and returns its reply.
func (h *ce2eHarness) request(msg map[string]any, timeout time.Duration) map[string]any {
	h.t.Helper()
	h.pmu.Lock()
	h.next++
	id := h.next
	ch := make(chan map[string]any, 1)
	h.waits[id] = ch
	h.pmu.Unlock()
	msg["id"] = id
	b, _ := json.Marshal(msg)
	h.wmu.Lock()
	_, err := h.conn.Write(append(b, '\n'))
	h.wmu.Unlock()
	if err != nil {
		h.t.Fatalf("write %s: %v", msg["type"], err)
	}
	select {
	case rep := <-ch:
		return rep
	case <-time.After(timeout):
		h.t.Fatalf("no reply to %s within %s", ce2eJSON(msg), timeout)
	case <-h.done:
		h.t.Fatalf("harness stopped while waiting for %s", msg["type"])
	}
	return nil
}

func (h *ce2eHarness) send(fields map[string]any) (map[string]any, time.Duration) {
	h.t.Helper()
	fields["type"] = "send"
	start := time.Now()
	rep := h.request(fields, ce2eReplyWait)
	return rep, time.Since(start)
}

func (h *ce2eHarness) paneEntry(idx int) map[string]any {
	h.t.Helper()
	rep := h.request(map[string]any{"type": "list"}, 10*time.Second)
	panes, _ := ce2eMap(rep["result"])["panes"].([]any)
	for _, p := range panes {
		e := ce2eMap(p)
		if n, ok := e["pane"].(float64); ok && int(n) == idx {
			return e
		}
	}
	h.t.Fatalf("pane %d not in list: %s", idx, ce2eJSON(rep))
	return nil
}

func (h *ce2eHarness) inputView(idx int) map[string]any { return ce2eMap(h.paneEntry(idx)["input"]) }

func (h *ce2eHarness) capture(idx int) string {
	h.t.Helper()
	rep := h.request(map[string]any{"type": "capture", "pane": idx}, 10*time.Second)
	return ce2eStr(ce2eMap(rep["result"]), "text")
}

// watchPane logs every change of the published input view (the readiness
// timeline: no box, the boot floor, the settle, ready — with times since
// spawn) and checks every screen for `-- INSERT --`, which fails the test on
// a pane magmux rewrote (failInsert) and is logged on any other.
func (h *ce2eHarness) watchPane(idx int, failInsert bool) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		last, sawInsert := "", false
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-h.done:
				return
			case <-tick.C:
			}
			// A raw request, not h.request: Fatal is not legal off the test
			// goroutine, and a watcher that cannot read just stops.
			view, screen, ok := h.peek(idx)
			if !ok {
				return
			}
			if v := ce2eJSON(view); v != last {
				h.lg.logf("  +%-7s input %s", h.since().Round(time.Millisecond), v)
				last = v
			}
			if !sawInsert && strings.Contains(screen, "-- INSERT --") {
				sawInsert = true
				h.lg.logf("  +%-7s -- INSERT -- on screen", h.since().Round(time.Millisecond))
				if failInsert {
					h.t.Errorf("pane %d painted `-- INSERT --` although magmux overrides the editor mode:\n%s", idx, screen)
				}
			}
		}
	}()
}

// peek is watchPane's read: list + capture with no Fatal.
func (h *ce2eHarness) peek(idx int) (map[string]any, string, bool) {
	get := func(msg map[string]any) (map[string]any, bool) {
		h.pmu.Lock()
		h.next++
		id := h.next
		ch := make(chan map[string]any, 1)
		h.waits[id] = ch
		h.pmu.Unlock()
		msg["id"] = id
		b, _ := json.Marshal(msg)
		h.wmu.Lock()
		_, err := h.conn.Write(append(b, '\n'))
		h.wmu.Unlock()
		if err != nil {
			return nil, false
		}
		select {
		case rep := <-ch:
			return rep, true
		case <-time.After(10 * time.Second):
			return nil, false
		case <-h.done:
			return nil, false
		}
	}
	lst, ok := get(map[string]any{"type": "list"})
	if !ok {
		return nil, "", false
	}
	var view map[string]any
	panes, _ := ce2eMap(lst["result"])["panes"].([]any)
	for _, p := range panes {
		if e := ce2eMap(p); e["pane"] == float64(idx) {
			view = ce2eMap(e["input"])
		}
	}
	shot, ok := get(map[string]any{"type": "capture", "pane": idx})
	if !ok {
		return view, "", false
	}
	return view, ce2eStr(ce2eMap(shot["result"]), "text"), true
}

func (h *ce2eHarness) waitInput(idx int, within time.Duration, pred func(map[string]any) bool) {
	h.t.Helper()
	deadline := time.Now().Add(within)
	for {
		in := h.inputView(idx)
		if pred(in) {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("pane %d: the input view never reached the wanted state in %s; last %s\nscreen:\n%s",
				idx, within, ce2eJSON(in), h.capture(idx))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *ce2eHarness) waitScreen(idx int, within time.Duration, what string, pred func(string) bool) string {
	h.t.Helper()
	deadline := time.Now().Add(within)
	for {
		s := h.capture(idx)
		if pred(s) {
			return s
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("pane %d: %s never appeared in %s; screen:\n%s", idx, what, within, s)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitAnswer waits for the turn a prompt carrying tok produced: a screen row
// holding tok that is NOT the prompt's own row (Claude answers with the word).
// A prompt left in the box, or lost, never produces one.
func (h *ce2eHarness) waitAnswer(idx int, tok string) {
	h.t.Helper()
	h.waitScreen(idx, ce2eTurnWait, "the answer carrying "+tok, func(s string) bool {
		return ce2eAnswered(s, tok)
	})
	h.lg.logf("  +%-7s answer %s on screen", h.since().Round(time.Millisecond), tok)
}

func ce2eAnswered(screen, tok string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, tok) && !strings.Contains(line, "reply with only the word") &&
			!strings.Contains(line, "touch ") && !strings.Contains(line, "sleep 15") {
			return true
		}
	}
	return false
}

// requireOkEvidence: a submit replied ok, with the evidence it was taken.
func (h *ce2eHarness) requireOkEvidence(what string, rep map[string]any) {
	h.t.Helper()
	if rep["ok"] != true {
		h.t.Fatalf("%s: %s, want ok\nscreen:\n%s", what, ce2eJSON(rep), h.capture(0))
	}
	res := ce2eMap(rep["result"])
	if a := ce2eStr(res, "accept"); a != "confirmed" && a != "resent_enter" {
		h.t.Errorf("%s: accept = %q, want confirmed or resent_enter", what, a)
	}
	if e := ce2eStr(res, "evidence"); e != "history" && e != "transcript" {
		h.t.Errorf("%s: evidence = %q, want history or transcript", what, e)
	}
}

// judgeSubmit applies V1.1/V1.2's rule to one submit and cross-checks it
// against the pane: ok must carry evidence AND become a turn; busy must not
// have typed what it says it did not type. busyOK is false for a late send,
// which has no excuse to be refused.
func (h *ce2eHarness) judgeSubmit(what, tok string, rep map[string]any, busyOK bool) {
	h.t.Helper()
	switch {
	case rep["ok"] == true:
		h.requireOkEvidence(what, rep)
		h.waitAnswer(0, tok) // an ok whose prompt never became a turn is LOST text: Fatal inside
	case ce2eStr(rep, "code") == "busy" && busyOK:
		h.lg.logf("[%s] busy (%s, typed=%v): an allowed outcome", what, ce2eStr(rep, "reason"), rep["typed"] == true)
		if rep["typed"] != true && strings.Contains(h.capture(0), tok) {
			h.t.Errorf("[%s] busy without typed, but the text is on screen", what)
		}
	default:
		h.t.Errorf("[%s] reply %s: want ok-with-evidence%s\nscreen:\n%s", what, ce2eJSON(rep),
			map[bool]string{true: " or busy", false: ""}[busyOK], h.capture(0))
	}
}

// ── small helpers ───────────────────────────────────────────────────────────

func ce2ePrompt(tok string) string {
	return "Please reply with only the word " + tok + " and nothing else."
}

func ce2eToken(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return prefix + strings.ToUpper(hex.EncodeToString(b))
}

func ce2eShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func ce2eChildEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(ce2eStrippedEnv, name) || name == "COLUMNS" || name == "LINES" {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// ce2eUserEditorMode reports the user's own editorMode, so a log says whether
// V1.3 on a plain direct pane was a real test (vim on) or a vacuous one.
func ce2eUserEditorMode() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "unknown (" + err.Error() + ")"
		}
		dir = filepath.Join(home, ".claude")
	}
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	var s struct {
		EditorMode string `json:"editorMode"`
	}
	if json.Unmarshal(b, &s) != nil || s.EditorMode == "" {
		return "unset (normal)"
	}
	if s.EditorMode != "vim" {
		return s.EditorMode
	}
	return "vim (the override is under test on every direct pane)"
}

func ce2eMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func ce2eStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func ce2eJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func ce2eFields(m map[string]any, keys ...string) string {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return ce2eJSON(out)
}

// ce2eLog writes to t.Log and, when MAGMUX_E2E_LOG is set, appends to it.
type ce2eLog struct {
	t  *testing.T
	mu sync.Mutex
}

var ce2eFileMu sync.Mutex

func newCE2ELog(t *testing.T) *ce2eLog { return &ce2eLog{t: t} }

func (l *ce2eLog) logf(format string, a ...any) {
	l.t.Helper()
	msg := fmt.Sprintf(format, a...)
	l.t.Log(msg)
	path := os.Getenv(ce2eLogEnv)
	if path == "" {
		return
	}
	ce2eFileMu.Lock()
	defer ce2eFileMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s %s\n", time.Now().Format("15:04:05.000"), l.t.Name(), msg)
}
