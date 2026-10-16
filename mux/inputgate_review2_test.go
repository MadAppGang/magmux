package mux

// Review round 2 (gpt H2, H3, H4, M5): acceptance must be ATTRIBUTABLE to this
// send, the first byte must follow a fresh readiness sample, and the settings
// copy must resolve in the child's context.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// ── A (H2): transcript freshness ────────────────────────────────────────────

// userLine is one transcript user entry as Claude writes it. A zero ts leaves
// the timestamp out.
func userLine(text string, ts time.Time) []byte {
	e := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
	if !ts.IsZero() {
		e["timestamp"] = ts.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	b, _ := json.Marshal(e)
	return b
}

// TestTranscriptOldEntryConsumedLateIsNotEvidence: an identical prompt WRITTEN
// before the verify, but consumed by the render goroutine only after a new,
// lost submission, is an earlier submission — not this one. Freshness is the
// entry's own timestamp against the verify's, never the moment it was read.
func TestTranscriptOldEntryConsumedLateIsNotEvidence(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
	g, _ := gatedClaudeOn(p)
	written := time.Now().Add(-30 * time.Second) // the earlier submission
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
	if chk.Verdict != BoxVerified {
		t.Fatalf("check: %+v", chk)
	}
	swapScreen(t, p, "s1-box-history", "empty-box-placeholder") // lost: no entry, empty box

	g.applyLine(userLine(sentFrance, written)) // polled late
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptNone {
		t.Fatalf("an old identical transcript entry consumed late counted: %+v", got)
	}
	g.applyLine(userLine(sentFrance, time.Time{})) // no timestamp: freshness unknown
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptNone {
		t.Fatalf("a transcript entry with no timestamp counted: %+v", got)
	}
	g.applyLine(userLine(sentFrance, time.Now())) // this send's entry
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptTaken || got.Evidence != evidenceTranscript {
		t.Errorf("a fresh transcript entry: %+v, want taken/transcript", got)
	}
}

// ── B (H3): history attribution to a frontier ───────────────────────────────

const frontierCols = 100

func frontierRule() string { return strings.Repeat("─", frontierCols) }

// longPrompt is a prompt that wraps to n rows: its rows as Claude draws them
// after a `❯ ` (the first) or a two-space indent (the rest), and its text.
func longPrompt(n int) (rows []string, text string) {
	var words []string
	for i := range n {
		w := fmt.Sprintf("blue%02d %s", i, strings.Repeat("x", 80))
		words = append(words, w)
		if i == 0 {
			rows = append(rows, "❯ "+w)
		} else {
			rows = append(rows, "  "+w)
		}
	}
	return rows, strings.Join(words, " ")
}

// claudeFrame lays a document out the way Claude does in a 39-row pane: the
// conversation's BOTTOM rows above the box, the box, two footer rows.
func claudeFrame(conv, box []string) []styledRow {
	const height = 39
	tail := []string{frontierRule()}
	tail = append(tail, box...)
	tail = append(tail, frontierRule(), "  * Haiku | $0.00", "  ⏵⏵ auto mode on (shift+tab to cycle)")
	room := height - len(tail)
	if len(conv) > room {
		conv = conv[len(conv)-room:]
	}
	var rows []styledRow
	for len(rows)+len(conv)+len(tail) < height {
		rows = append(rows, styledRow{})
	}
	for _, s := range append(append([]string(nil), conv...), tail...) {
		rows = append(rows, styledRow{Text: s})
	}
	return rows
}

// TestHistoryEvidenceIsAttributedToTheFrontier: a long box hides an OLDER
// identical prompt at the verify; the new text is lost, the box shrinks, and
// the old entry scrolls back into view. More matching entries are VISIBLE,
// and none of them is new: only an entry below the conversation's frontier at
// the verify is this send's.
func TestHistoryEvidenceIsAttributedToTheFrontier(t *testing.T) {
	promptRows, sent := longPrompt(20)
	conv := []string{
		" ▐▛███▛█   Claude Code v2.1.301",
		"▝▜██████▀  Haiku 5.5 · Claude Max",
		" ▝▝   ▝▝   ~/scratch",
		"",
	}
	conv = append(conv, promptRows...) // the OLDER identical prompt
	conv = append(conv, "", "⏺ blue", "", "✻ Cooked for 1s · done 8:53 pm", "",
		strings.Repeat(" ", 86)+"47363 tokens")

	before := parseClaudeScreen(claudeFrame(conv, promptRows), frontierCols)
	if !before.BoxVisible || before.BoxKey != inputKey(sent) {
		t.Fatalf("the verify screen does not hold the long prompt in its box: %+v", before)
	}
	if len(before.Prompts) != 0 {
		t.Fatalf("the long box should hide the old entry's ❯ row; %d entries visible", len(before.Prompts))
	}
	b := newAcceptBaseline(before, before.BoxText, sent, 0, time.Now(), 0)

	// Lost: the box empties, shrinks to one row, and the old entry reappears.
	lost := parseClaudeScreen(claudeFrame(conv, []string{"❯ "}), frontierCols)
	if countMatches(lost.Prompts, b) != 1 {
		t.Fatalf("setup: the old identical entry should be visible again (%d)", countMatches(lost.Prompts, b))
	}
	if got := acceptVerdict(lost, b, nil); got.State != AcceptNone {
		t.Errorf("the old entry scrolled back into view counted as this send's: %+v", got)
	}

	// Taken: the same prompt as a NEW entry below the frontier.
	taken := append(append([]string(nil), conv...), "")
	taken = append(taken, promptRows...)
	taken = append(taken, "", "· Germinating… (0s)")
	got := acceptVerdict(parseClaudeScreen(claudeFrame(taken, []string{"❯ "}), frontierCols), b, nil)
	if got.State != AcceptTaken || got.Evidence != evidenceHistory {
		t.Errorf("a new entry below the frontier: %+v, want taken/history", got)
	}

	// The frontier scrolled off (a reply pushed it away) while the new entry
	// is still on screen: nothing visible can be ATTRIBUTED — the same picture
	// arises when a resize reflows the frontier and an old entry reappears —
	// so the history gives no evidence and the transcript has to.
	far := append(append([]string(nil), conv...), "")
	far = append(far, promptRows...)
	far = append(far, "")
	for i := range 12 { // 12 + 1 + 20 + 1 = the 34 conversation rows
		far = append(far, fmt.Sprintf("⏺ reply line %d", i))
	}
	farSc := parseClaudeScreen(claudeFrame(far, []string{"❯ "}), frontierCols)
	if countMatches(farSc.Prompts, b) != 1 {
		t.Fatalf("setup: the new entry should still be visible (%d)", countMatches(farSc.Prompts, b))
	}
	if got := acceptVerdict(farSc, b, nil); got.State != AcceptNone {
		t.Errorf("no frontier on screen, yet history evidence: %+v", got)
	}
}

// TestNoFrontierIsNoHistoryEvidence: a box so long that the verify screen
// shows NO conversation row at all leaves nothing to attribute an entry to.
func TestNoFrontierIsNoHistoryEvidence(t *testing.T) {
	promptRows, sent := longPrompt(33)
	before := parseClaudeScreen(claudeFrame(nil, promptRows), frontierCols)
	if !before.BoxVisible {
		t.Fatalf("no box: %+v", before)
	}
	b := newAcceptBaseline(before, before.BoxText, sent, 0, time.Now(), 0)
	conv := append([]string{"", "❯ unrelated", ""}, promptRows...)
	after := parseClaudeScreen(claudeFrame(conv, []string{"❯ "}), frontierCols)
	if countMatches(after.Prompts, b) != 1 {
		t.Fatalf("setup: one matching entry expected, %d", countMatches(after.Prompts, b))
	}
	if got := acceptVerdict(after, b, nil); got.State != AcceptNone {
		t.Errorf("an entry with no frontier to attribute it to counted: %+v", got)
	}
}

// ── C (H4): the first byte follows a FRESH readiness sample ────────────────

// TestFirstKeyRechecksReadinessAfterItsGap: a keys-only gated submit pauses
// keyGap before its first key. Readiness that flips during that pause (a reset
// to cooked, a dialog) must stop the first key: no byte reaches the child
// until the pane is ready again.
func TestFirstKeyRechecksReadinessAfterItsGap(t *testing.T) {
	const notReadyFor = 250 * time.Millisecond
	var until atomic.Int64 // unix nanos; 0 = not armed
	g := &fakeGate{readyFn: func(_ ReadinessQuery, n int) InputReadiness {
		switch {
		case n == 0:
			return readyNow()
		case n == 1: // the post-token check: ready, and the terminal resets right after
			until.Store(time.Now().Add(notReadyFor).UnixNano())
			return readyNow()
		case time.Now().UnixNano() < until.Load():
			return notReady(protocol.ReasonTerminalNotReady)
		}
		return readyNow()
	}}
	m, _, log := gatedMux(t, g)
	ch := startSend(t, m, t.Context(), sendRequest{keys: []string{"h", "i", "enter"}})
	for until.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	end := time.Unix(0, until.Load()).Add(-10 * time.Millisecond)
	for time.Now().Before(end) {
		if s := log.String(); s != "" {
			t.Fatalf("%q reached the child while the pane was not ready", s)
		}
		time.Sleep(2 * time.Millisecond)
	}
	o := awaitSend(t, ch, 30*time.Second)
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hi\r" {
		t.Errorf("bytes %q, want %q", got, "hi\r")
	}
}

// TestFirstKeyNotReadyToDeadlineIsBusy: when the pane never comes back after
// the first key's gap, the send is busy at its deadline with nothing typed.
func TestFirstKeyNotReadyToDeadlineIsBusy(t *testing.T) {
	g := &fakeGate{readyFn: func(_ ReadinessQuery, n int) InputReadiness {
		if n <= 1 {
			return readyNow()
		}
		return dialogUp("permission", false)
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{keys: []string{"h", "i", "enter"}, readyTimeoutMs: 300})
	wantErr(t, o, protocol.CodeBusy, protocol.ReasonDialogPrefix+"permission", false)
	if got := log.settle(); got != "" {
		t.Errorf("bytes %q reached a pane that never became ready again", got)
	}
}

// ── D (M5): the settings copy in the child's context ────────────────────────

// TestSettingsCopyPathIsAbsolute: a relative --sock-dir gives a relative
// private directory; the copy's path is emitted into a command that may `cd`,
// so it must be absolute, and must name the file that was written.
func TestSettingsCopyPathIsAbsolute(t *testing.T) {
	dir, sub := rewriteFixtures(t)
	t.Chdir(dir)
	if err := os.Mkdir("rel-sock", 0o700); err != nil { // the socket directory exists: magmux bound there
		t.Fatal(err)
	}
	private := settingsDir("rel-sock") // relative, as --sock-dir rel-sock gives
	cfg := shellCfg("/bin/sh", sub("cd / && claude --settings {DIR}/s.json"))
	rw := rewriteClaudeLaunch(cfg, claudeLaunchOf(cfg), gateDirect, private)
	if rw.file != "" {
		defer os.Remove(rw.file)
	}
	if rw.override != overrideMergedFile {
		t.Fatalf("override %q (why %q)", rw.override, rw.why)
	}
	if !filepath.IsAbs(rw.file) {
		t.Fatalf("settings copy path %q is relative; the child runs `cd /` first", rw.file)
	}
	if !strings.Contains(rw.args[len(rw.args)-1], "'"+rw.file+"'") {
		t.Errorf("the line does not name the absolute copy: %q", rw.args[len(rw.args)-1])
	}
	if b, err := os.ReadFile(rw.file); err != nil || !strings.Contains(string(b), `"editorMode":"normal"`) {
		t.Errorf("the absolute path does not hold the merged copy: %q %v", b, err)
	}
}

// TestTildeSettingsUseTheChildsHome: an unquoted `~/` is expanded by the
// CHILD's shell, with the child's HOME — which cfg.Env may set — so the copy
// must be made from that file, never magmux's. A HOME that cannot be resolved
// to an absolute directory, or a command before the invocation (which could
// change HOME), leaves the launch unparsed.
func TestTildeSettingsUseTheChildsHome(t *testing.T) {
	magmuxHome, _ := rewriteFixtures(t) // s.json here carries "SECRET"
	t.Setenv("HOME", magmuxHome)
	childHome := sockTestDir(t)
	if err := os.WriteFile(filepath.Join(childHome, "s.json"), []byte(`{"child":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	private := settingsDir(magmuxHome)
	for _, r := range []struct {
		name, line string
		env        []string
		want       editorOverride
		body       string // a substring the copy must hold
	}{
		{"child HOME from cfg.Env", "claude --settings ~/s.json", []string{"HOME=" + childHome}, overrideMergedFile, `"child":true`},
		{"last HOME wins", "claude --settings=~/s.json", []string{"HOME=/nonexistent", "HOME=" + childHome}, overrideMergedFile, `"child":true`},
		{"inherited HOME", "claude --settings ~/s.json", nil, overrideMergedFile, `"SECRET"`},
		{"relative child HOME", "claude --settings ~/s.json", []string{"HOME=rel"}, overrideUnparsed, ""},
		{"empty child HOME", "claude --settings ~/s.json", []string{"HOME="}, overrideUnparsed, ""},
		{"HOME changed before", "export HOME=" + childHome + "; claude --settings ~/s.json", nil, overrideUnparsed, ""},
	} {
		cfg := shellCfg("/bin/sh", r.line)
		cfg.Env = r.env
		rw := rewriteClaudeLaunch(cfg, claudeLaunchOf(cfg), gateDirect, private)
		if rw.file != "" {
			defer os.Remove(rw.file)
		}
		if rw.override != r.want {
			t.Errorf("%s: override %q (why %q), want %q", r.name, rw.override, rw.why, r.want)
			continue
		}
		if r.body != "" {
			if b, err := os.ReadFile(rw.file); err != nil || !strings.Contains(string(b), r.body) {
				t.Errorf("%s: the copy %q holds %q, want it to contain %s", r.name, rw.file, b, r.body)
			}
		}
		checkRewriteProperties(t, cfg, rw)
	}
}
