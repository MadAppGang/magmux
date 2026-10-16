package mux

// The Claude screen reader, pinned on REAL Claude Code screens.
//
// Every screen here is a byte capture of Claude Code 2.1.301 (testdata/
// claude-screens/*.raw, recorded under `script` inside a 100x39 magmux pane)
// replayed through magmux's own VT parser into a bare pane, so the parser reads
// exactly the cells it would read in production — faint attributes, the NBSP
// Claude writes after the box's ❯, the blinking tool bullet and all. The
// .marks.json beside each raw file names the byte offset of every labelled
// moment and carries magmux's own text capture of that moment as a cross-check
// (TestClaudeCapturesReplayFaithfully). The three boot dialogs magmux could not
// capture (every scratch directory was already trusted) come from claudish's
// own 2.1.290 captures, as text. Nothing in this file is a hand-drawn Claude
// screen; testdata/claude-screens/README.md lists every file's provenance.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// The exact text each capture scenario sent (captures/scen/*.json), so a key
// can be checked against what was typed and not only against another screen.
const (
	sentFrance  = "Please answer in one short sentence: what is the capital of France? This line is deliberately long so that it wraps in the middle of a word somewherearoundhere and continues on the next row."
	sentRespond = "Respond with a single line that starts with the exact words: Respond with a single line"
	sentQuote   = "Reply with exactly these three lines and nothing else. Line one: ❯ not a prompt. Line two: Do you want to proceed? Line three: Esc to cancel · Tab to amend"
	sentPaste1  = "first pasted line\nsecond pasted line\nthird pasted line\nfourth line: reply with the word ok"
	sentPaste2  = "another\npasted\nblock"
	sentBash    = "Run the Bash command: sleep 25 && echo DONE-ONE"
	sentQueued  = "QUEUED-SECOND say hi"
	sentQueued3 = "QUEUED-THIRD long queued pasted\nblock line two\nline three"
)

// sentLong is s1's 829-byte typed prompt (box-long-prompt).
var sentLong = "Count from one to five in words, then stop. Ignore the rest of this message, it only exists to make the prompt long enough to wrap across more than ten rows of the input box: " +
	strings.Repeat("aaaa bbbb cccc dddd eeee ffff gggg hhhh iiii jjjj kkkk llll mmmm nnnn oooo pppp qqqq rrrr ssss tttt uuuu vvvv wwww xxxx yyyy zzzz ", 5) + "end."

// sentAutoLong is s3's 955-byte single-line AUTO send (unbracketed).
var sentAutoLong = "Reply with the single word ok. PADDING " + strings.Repeat("xxxxxxxxxx ", 83) + "end"

// claudeMark is one entry of a .marks.json file.
type claudeMark struct {
	Label  string `json:"label"`
	Offset int    `json:"offset"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
	Text   string `json:"text"`
}

const claudeScreensDir = "testdata/claude-screens"

func loadClaudeMarks(t *testing.T, scenario string) []claudeMark {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(claudeScreensDir, scenario+".marks.json"))
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	var marks []claudeMark
	if err := json.Unmarshal(b, &marks); err != nil {
		t.Fatalf("parse marks: %v", err)
	}
	return marks
}

func claudeMarkOf(t *testing.T, scenario, label string) claudeMark {
	t.Helper()
	for _, m := range loadClaudeMarks(t, scenario) {
		if m.Label == label {
			return m
		}
	}
	t.Fatalf("%s has no mark %q", scenario, label)
	return claudeMark{}
}

// replayClaude feeds raw[0:offset] of a capture into a fresh rows x cols pane.
func replayClaude(t *testing.T, scenario string, offset, rows, cols int) *Pane {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(claudeScreensDir, scenario+".raw"))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if offset > len(raw) {
		t.Fatalf("%s: offset %d past the capture's %d bytes", scenario, offset, len(raw))
	}
	p := newScrollPane(rows, cols)
	feed(p, string(raw[:offset]))
	return p
}

// claudePaneAt is the pane at a labelled moment.
func claudePaneAt(t *testing.T, scenario, label string) *Pane {
	t.Helper()
	m := claudeMarkOf(t, scenario, label)
	return replayClaude(t, scenario, m.Offset, m.Rows, m.Cols)
}

// claudeScreenAt parses the screen at a labelled moment.
func claudeScreenAt(t *testing.T, scenario, label string) claudeScreen {
	t.Helper()
	v := claudePaneAt(t, scenario, label).tailView(0)
	return parseClaudeScreen(v.Rows, v.Cols)
}

// Derived frames: moments the capture contains but no mark names. Claude ends
// every frame by showing the cursor (ESC[?25h), so a prefix ending there is a
// complete frame; TestDerivedFramesAreFrameEnds pins that each offset is one.
const (
	// s4-midturn, between queued-c and queued-paste: the background Bash row
	// (row 16) with its running ⏺ blinked OFF, then the next frame with it ON.
	s4BlinkOff = 12771
	s4BlinkOn  = 12825
)

// claudishScreen loads one of claudish's 2.1.290 text captures as rows. A text
// capture carries no attributes, so every rune is non-faint.
func claudishScreen(t *testing.T, name string) ([]styledRow, int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(claudeScreensDir, "claudish-2.1.290-"+name+".json"))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var cap struct {
		Cols  int      `json:"cols"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(b, &cap); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return textRows(cap.Lines), cap.Cols
}

// researchScreen loads one `=== title` block of the F9 text capture
// (research-2.1.301-midturn.txt: magmux text captures, each row prefixed "|").
func researchScreen(t *testing.T, title string) []styledRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(claudeScreensDir, "research-2.1.301-midturn.txt"))
	if err != nil {
		t.Fatalf("read research capture: %v", err)
	}
	var lines []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "=== ") {
			in = strings.TrimPrefix(l, "=== ") == title
			continue
		}
		if in && strings.HasPrefix(l, "|") {
			lines = append(lines, strings.TrimPrefix(l, "|"))
		}
	}
	if len(lines) == 0 {
		t.Fatalf("research capture has no screen %q", title)
	}
	return textRows(lines)
}

func textRows(lines []string) []styledRow {
	rows := make([]styledRow, len(lines))
	for i, l := range lines {
		l = strings.TrimRight(l, " ")
		rows[i] = styledRow{Text: l, Faint: make([]bool, utf8.RuneCountInString(l))}
	}
	return rows
}

func promptKeys(ps []promptEntry) []string {
	var keys []string
	for _, p := range ps {
		keys = append(keys, p.Key)
	}
	return keys
}

// ── the testdata itself ─────────────────────────────────────────────────────

// TestClaudeCapturesReplayFaithfully is the cross-check that makes the rest of
// this file mean anything: replaying a capture's prefix through magmux's VT
// parser reproduces the text magmux itself captured at that moment. The only
// rows allowed to differ are a live token counter, which advanced between the
// capture call and the byte offset being taken.
func TestClaudeCapturesReplayFaithfully(t *testing.T) {
	norm := func(s string) string { return strings.TrimRight(strings.ReplaceAll(s, "\u00a0", " "), " ") }
	for _, sc := range []string{"s1-box-history", "s2-permission", "s3-paste", "s4-midturn", "s5-insert-plan", "s6-markdown"} {
		for _, m := range loadClaudeMarks(t, sc) {
			got := strings.Split(replayClaude(t, sc, m.Offset, m.Rows, m.Cols).capture(0, false).Text, "\n")
			want := strings.Split(m.Text, "\n")
			for i, g := range got {
				w := ""
				if i < len(want) {
					w = want[i]
				}
				if norm(g) == norm(w) {
					continue
				}
				if strings.Contains(g, " tokens") && strings.Contains(w, " tokens") {
					continue // a spinner's live token count moved between the two reads
				}
				t.Errorf("%s/%s row %d:\n replay  %q\n capture %q", sc, m.Label, i, g, w)
			}
		}
	}
}

func TestDerivedFramesAreFrameEnds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(claudeScreensDir, "s4-midturn.raw"))
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int{s4BlinkOff, s4BlinkOn} {
		if !strings.HasSuffix(string(raw[:off]), "\x1b[?25h") {
			t.Errorf("offset %d is not the end of a Claude frame (ESC[?25h)", off)
		}
	}
}

// ── tailRows ────────────────────────────────────────────────────────────────

// TestTailRowsFaintIsPerRune: one flag per emitted rune, on every row of every
// capture, and the placeholder's runes are the faint ones.
func TestTailRowsFaintIsPerRune(t *testing.T) {
	for _, sc := range []string{"s1-box-history", "s2-permission", "s3-paste", "s4-midturn", "s5-insert-plan", "s6-markdown"} {
		for _, m := range loadClaudeMarks(t, sc) {
			for i, r := range replayClaude(t, sc, m.Offset, m.Rows, m.Cols).tailRows(0) {
				if n := utf8.RuneCountInString(r.Text); n != len(r.Faint) {
					t.Fatalf("%s/%s row %d: %d runes, %d faint flags: %q", sc, m.Label, i, n, len(r.Faint), r.Text)
				}
			}
		}
	}

	rows := claudePaneAt(t, "s1-box-history", "empty-box-placeholder").tailRows(0)
	var box styledRow
	for _, r := range rows {
		if strings.HasPrefix(r.Text, "❯") {
			box = r
		}
	}
	if !strings.Contains(box.Text, `Try "create a util logging.py`) {
		t.Fatalf("no placeholder row found: %q", box.Text)
	}
	if box.Faint[0] || box.Faint[1] {
		t.Errorf("the box's ❯ and its NBSP must not be faint: %v", box.Faint[:2])
	}
	for i, f := range box.Faint[2:] {
		if !f {
			t.Errorf("placeholder rune %d is not faint", i+2)
		}
	}
}

// TestTailRowsWideAndStyledTrailing: a wide character is one rune and one
// flag, and styled trailing blanks are trimmed from both slices together. No
// capture has a wide character in the box, so this one feeds the VT parser
// directly — it is a property of the cell walk, not of Claude's UI.
func TestTailRowsWideAndStyledTrailing(t *testing.T) {
	p := newScrollPane(3, 20)
	feed(p, "日本\x1b[2mx\x1b[0m\r\n\x1b[48;5;255mab   \x1b[0m")
	rows := p.tailRows(0)
	if rows[0].Text != "日本x" || !slices.Equal(rows[0].Faint, []bool{false, false, true}) {
		t.Errorf("wide row: %q %v", rows[0].Text, rows[0].Faint)
	}
	if rows[1].Text != "ab" || len(rows[1].Faint) != 2 {
		t.Errorf("styled trailing blanks: %q %v", rows[1].Text, rows[1].Faint)
	}
	if got := p.tailRows(1); len(got) != 1 || got[0].Text != "" {
		t.Errorf("tailRows(1) = %#v, want the last row only", got)
	}
}

// TestTailRowsReadsTheLiveScreen: whatever the human scrolled to, tailRows is
// the live screen.
func TestTailRowsReadsTheLiveScreen(t *testing.T) {
	p := newScrollPane(4, 20)
	feedLines(p, "L", 12)
	p.mu.Lock()
	p.screen.scrollBackBy(5)
	p.mu.Unlock()
	rows := p.tailRows(0)
	if p.screen.sbOff == 0 || rows[2].Text != "L12" {
		t.Errorf("tailRows read a scrolled view (sbOff %d): %#v", p.screen.sbOff, rows)
	}
}

// ── inputKey ────────────────────────────────────────────────────────────────

// TestInputKeyIgnoresWrapAndWidth: the box (wrapped at the pane width,
// mid-word), the history entry Claude makes of it and the unwrapped text that
// was sent all give one key, for the short wrapped prompt and the 9-row one.
//
// Missing testdata, stated rather than faked: the plan asks for the SAME
// prompt captured at 40 and at 120 columns. Every capture is 100 columns, and
// replaying a 100-column capture at another width does not re-lay it out
// (Claude positions for the width it was told), so the 40/120 pair needs a new
// capture. The wrap independence is pinned here across box, history and the
// unwrapped send instead.
func TestInputKeyIgnoresWrapAndWidth(t *testing.T) {
	box := claudeScreenAt(t, "s1-box-history", "box-wrapped-text")
	if !strings.Contains(box.BoxText, "\n") {
		t.Fatalf("the capture should wrap the box: %q", box.BoxText)
	}
	hist := claudeScreenAt(t, "s1-box-history", "history-entry-after-turn")
	if len(hist.Prompts) != 1 {
		t.Fatalf("want 1 entry, got %#v", hist.Prompts)
	}
	if k := inputKey(sentFrance); box.BoxKey != k || hist.Prompts[0].Key != k {
		t.Errorf("keys differ:\n box  %q\n hist %q\n sent %q", box.BoxKey, hist.Prompts[0].Key, k)
	}

	longBox := claudeScreenAt(t, "s1-box-history", "box-long-prompt")
	longHist := claudeScreenAt(t, "s1-box-history", "history-long-prompt")
	if n := strings.Count(longBox.BoxText, "\n"); n < 8 {
		t.Fatalf("the long prompt should fill 9 box rows, has %d breaks", n)
	}
	if len(longHist.Prompts) != 2 {
		t.Fatalf("want 2 entries, got %d", len(longHist.Prompts))
	}
	// Facts these captures settle (2): a long typed prompt is NOT abbreviated
	// in history, so key equality over the whole text is available.
	if k := inputKey(sentLong); longBox.BoxKey != k || longHist.Prompts[1].Key != k {
		t.Errorf("long prompt keys differ:\n box  %q\n hist %q", longBox.BoxKey, longHist.Prompts[1].Key)
	}
}

func TestInputKey(t *testing.T) {
	if got := inputKey(" a b\n\tc\u00a0d "); got != "abcd" {
		t.Errorf("inputKey dropped the wrong runes: %q", got)
	}
	big := strings.Repeat("x y ", inputKeyMax)
	k1, k2 := inputKey(big), inputKey(big+"z")
	if len(k1) > inputKeyMax+32 {
		t.Errorf("an unbounded key: %d bytes", len(k1))
	}
	if k1 == k2 {
		t.Errorf("two texts of different length past the bound gave one key")
	}
	if inputKey(big) != inputKey(strings.ReplaceAll(big, " ", "\n  ")) {
		t.Errorf("whitespace changed a bounded key")
	}
}

// ── parseClaudeScreen over the captures ─────────────────────────────────────

type screenWant struct {
	box       bool
	boxText   string // compared when box
	boxKey    string // compared when non-empty
	pastes    []int
	maxPaste  int
	insert    bool
	working   bool
	dialog    string
	dismiss   bool
	prompts   []string // keys of Prompts, top to bottom; nil = none
	queued    []bool   // per prompt, when non-nil
	checkKeys bool
}

func checkScreen(t *testing.T, name string, sc claudeScreen, w screenWant) {
	t.Helper()
	if sc.BoxVisible != w.box {
		t.Errorf("%s: BoxVisible=%v want %v", name, sc.BoxVisible, w.box)
	}
	if w.box && sc.BoxText != w.boxText {
		t.Errorf("%s: BoxText=%q want %q", name, sc.BoxText, w.boxText)
	}
	if w.boxKey != "" && sc.BoxKey != w.boxKey {
		t.Errorf("%s: BoxKey=%q want %q", name, sc.BoxKey, w.boxKey)
	}
	if sc.BoxKey != inputKey(sc.BoxText) {
		t.Errorf("%s: BoxKey is not inputKey(BoxText)", name)
	}
	if !slices.Equal(sc.BoxPastes, w.pastes) {
		t.Errorf("%s: BoxPastes=%v want %v", name, sc.BoxPastes, w.pastes)
	}
	if sc.MaxPaste != w.maxPaste {
		t.Errorf("%s: MaxPaste=%d want %d", name, sc.MaxPaste, w.maxPaste)
	}
	if sc.Insert != w.insert {
		t.Errorf("%s: Insert=%v want %v", name, sc.Insert, w.insert)
	}
	if sc.Working != w.working {
		t.Errorf("%s: Working=%v want %v", name, sc.Working, w.working)
	}
	if sc.Dialog != w.dialog || sc.Dismissable != w.dismiss {
		t.Errorf("%s: Dialog=%q/%v want %q/%v", name, sc.Dialog, sc.Dismissable, w.dialog, w.dismiss)
	}
	if got := promptKeys(sc.Prompts); !slices.Equal(got, w.prompts) {
		t.Errorf("%s: Prompts=\n %q\nwant\n %q", name, got, w.prompts)
	}
	if w.queued != nil {
		var q []bool
		for _, p := range sc.Prompts {
			q = append(q, p.Queued)
		}
		if !slices.Equal(q, w.queued) {
			t.Errorf("%s: Queued=%v want %v", name, q, w.queued)
		}
	}
	for _, p := range sc.Prompts {
		if p.Key != inputKey(p.Text) {
			t.Errorf("%s: entry Key is not inputKey(Text): %q", name, p.Text)
		}
		if p.RenderedKey != renderedKey(p.Text) {
			t.Errorf("%s: entry RenderedKey is not renderedKey(Text): %q", name, p.Text)
		}
	}
}

func TestParseClaudeScreenS1(t *testing.T) {
	k := inputKey
	for _, c := range []struct {
		label string
		want  screenWant
	}{
		// The idle box: ❯ + NBSP + a FAINT `Try "…"` placeholder reads empty.
		{"empty-box-placeholder", screenWant{box: true}},
		{"box-wrapped-text", screenWant{box: true,
			boxText: "Please answer in one short sentence: what is the capital of France? This line is deliberately\n" +
				"long so that it wraps in the middle of a word somewherearoundhere and continues on the next row.",
			boxKey: k(sentFrance)}},
		{"submitted-working", screenWant{box: true, working: true, prompts: []string{k(sentFrance)}}},
		{"history-entry-after-turn", screenWant{box: true, prompts: []string{k(sentFrance)}}},
		{"box-long-prompt", screenWant{box: true, boxText: claudeScreenAt(t, "s1-box-history", "box-long-prompt").BoxText,
			boxKey: k(sentLong), prompts: []string{k(sentFrance)}}},
		{"history-long-prompt", screenWant{box: true, prompts: []string{k(sentFrance), k(sentLong)}}},
		// The assistant's `⏺ Respond with a single line, …` forms no entry.
		{"assistant-prose-same-words", screenWant{box: true, prompts: []string{k(sentFrance), k(sentLong), k(sentRespond)}}},
		// `⏺ ❯ not a prompt.`, `Do you want to proceed?` and `Esc to cancel` in
		// the assistant's reply: no entry, and no dialog (the box is showing).
		{"assistant-quotes-prompt-glyph-and-dialog", screenWant{box: true, prompts: []string{k(sentLong), k(sentRespond), k(sentQuote)}}},
		// Facts these captures settle (3): /clear and /compact leave ordinary
		// `❯ /…` entries; `⎿  Compacted` ends the second.
		{"after-slash-clear", screenWant{box: true, prompts: []string{"/clear"}}},
		{"after-slash-compact", screenWant{box: true, prompts: []string{"/clear", "Sayok", "/compact"}}},
	} {
		checkScreen(t, c.label, claudeScreenAt(t, "s1-box-history", c.label), c.want)
	}
}

func TestParseClaudeScreenDialogs(t *testing.T) {
	k := inputKey
	perm := "Use the Bash tool to run exactly: touch zz-cap-perm.txt"
	ask := "Use the AskUserQuestion tool to ask me which colour I prefer, with the two options red and blue. Do nothing else."
	checkScreen(t, "dialog-permission", claudeScreenAt(t, "s2-permission", "dialog-permission"),
		screenWant{dialog: "permission", dismiss: true})
	checkScreen(t, "after-escape-permission", claudeScreenAt(t, "s2-permission", "after-escape-permission"),
		screenWant{box: true, prompts: []string{k(perm)}})
	// Facts these captures settle (4): the question dialog draws `❯ 1. Red`
	// at column 0 under a rule — box-shaped — and it is neither the box nor a
	// user-prompt entry; the `Enter to select · ↑/↓ to navigate` footer is
	// the discriminator.
	checkScreen(t, "dialog-question", claudeScreenAt(t, "s2-permission", "dialog-question"),
		screenWant{dialog: "question", dismiss: true})
	checkScreen(t, "after-escape-question", claudeScreenAt(t, "s2-permission", "after-escape-question"),
		screenWant{box: true, prompts: []string{k(perm), k(ask)}})

	// Facts these captures settle (6): vim INSERT is Claude's own
	// `-- INSERT --` footer token; the user's status line ` I ` is not it.
	checkScreen(t, "insert-footer-empty", claudeScreenAt(t, "s5-insert-plan", "insert-footer-empty"),
		screenWant{box: true, insert: true})
	// The 2.1.301 plan dialog has NO `Esc to cancel` row, and Escape still
	// declines it: after-escape-plan shows the box back, plan mode kept and
	// nothing approved. Orchestrator ruling (phase 5): it IS dismissable, so
	// dismissability is not keyed on that row for `plan`.
	checkScreen(t, "dialog-plan", claudeScreenAt(t, "s5-insert-plan", "dialog-plan"),
		screenWant{dialog: "plan", dismiss: true})
	plan := "Plan how to create an empty file named zz-plan.txt, then present the plan with the ExitPlanMode tool. Keep the plan to one line."
	checkScreen(t, "after-escape-plan", claudeScreenAt(t, "s5-insert-plan", "after-escape-plan"),
		screenWant{box: true, insert: true, prompts: []string{k(plan)}})

	// Boot dialogs, from claudish's 2.1.290 captures. Never dismissable:
	// Escape on the trust prompt exits Claude.
	for _, c := range []struct{ name, kind string }{
		{"dialog-trust", "trust"},
		{"dialog-bypass", "bypass"},
		// The onboarding marker sits 16 non-empty rows above the bottom, past
		// dialogRegionMax, so it is not named; with no box the pane is refused
		// as starting either way. Pinned so a change to the region is a choice.
		{"dialog-onboarding", ""},
	} {
		rows, cols := claudishScreen(t, c.name)
		checkScreen(t, c.name, parseClaudeScreen(rows, cols), screenWant{dialog: c.kind})
	}
}

func TestParseClaudeScreenPastes(t *testing.T) {
	k := inputKey
	checkScreen(t, "paste-in-box", claudeScreenAt(t, "s3-paste", "paste-in-box"),
		screenWant{box: true, boxText: "[Pasted text #1 +3 lines]", pastes: []int{1}, maxPaste: 1})
	// Facts these captures settle (1): a SMALL paste is not collapsed.
	checkScreen(t, "two-pastes-in-box", claudeScreenAt(t, "s3-paste", "two-pastes-in-box"),
		screenWant{box: true, boxText: "[Pasted text #1 +3 lines]another\npasted\nblock", pastes: []int{1}, maxPaste: 1})
	// In HISTORY the paste is shown EXPANDED, not as its token: the entry's
	// key is the full text, and its Paste is 0. (The design assumed the
	// history row would start with the token; it does not.)
	sub := claudeScreenAt(t, "s3-paste", "paste-submitted")
	checkScreen(t, "paste-submitted", sub, screenWant{box: true, prompts: []string{k(sentPaste1 + sentPaste2)}})
	if sub.Prompts[0].Paste != 0 {
		t.Errorf("a history entry carries a paste token: %+v", sub.Prompts[0])
	}
	// A 955-byte single-line AUTO send collapses to `[Pasted text #2]` (no
	// `+N lines`; N is session-global), and history shows it in full.
	checkScreen(t, "auto-long-in-box", claudeScreenAt(t, "s3-paste", "auto-long-in-box"),
		screenWant{box: true, boxText: "[Pasted text #2]", pastes: []int{2}, maxPaste: 2,
			prompts: []string{k(sentPaste1 + sentPaste2)}})
	al := claudeScreenAt(t, "s3-paste", "auto-long-submitted")
	if n := len(al.Prompts); n == 0 || al.Prompts[n-1].Key != k(sentAutoLong) {
		t.Errorf("auto-long-submitted: last entry is not the long send: %q", promptKeys(al.Prompts))
	}
}

func TestParseClaudeScreenMidTurn(t *testing.T) {
	k := inputKey
	for _, l := range []string{"turn-running-a", "turn-running-b", "turn-running-c", "turn-running-d"} {
		// Facts these captures settle (5): during a running turn the box is
		// present and empty.
		checkScreen(t, l, claudeScreenAt(t, "s4-midturn", l), screenWant{box: true, working: true, prompts: []string{k(sentBash)}})
	}
	for _, l := range []string{"queued-a", "queued-b", "queued-c"} {
		// The queued entry, its `ctrl+x ctrl+s to send now` hint (which never
		// joins the text) and the faint `Press up to edit queued messages`
		// placeholder (which reads as an empty box).
		checkScreen(t, l, claudeScreenAt(t, "s4-midturn", l), screenWant{box: true, working: true,
			prompts: []string{k(sentBash), k(sentQueued)}, queued: []bool{false, true}})
	}
	// A queued PASTE-mode send is shown EXPANDED in history.
	checkScreen(t, "queued-paste", claudeScreenAt(t, "s4-midturn", "queued-paste"), screenWant{box: true, working: true,
		prompts: []string{k(sentBash), k(sentQueued), k(sentQueued3)}})
	checkScreen(t, "after-turns", claudeScreenAt(t, "s4-midturn", "after-turns"), screenWant{box: true,
		prompts: []string{k(sentQueued), k(sentQueued3)}})
}

// TestBoxShapedQuestionIsNotTheBox: in the real capture the question and a
// blank row sit between the dialog's rule and `❯ 1. Red`, so the box shape
// fails on its own. A question with no header would put `❯ 1. Red` directly
// under the rule — exactly the box's shape. That variant is SPLICED from the
// real rows (the rows between the rule and the option deleted, nothing
// added): only the `Enter to select` footer below tells it from a box whose
// user typed a numbered list.
func TestBoxShapedQuestionIsNotTheBox(t *testing.T) {
	rows := claudePaneAt(t, "s2-permission", "dialog-question").tailRows(0)
	opt := slices.IndexFunc(rows, func(r styledRow) bool { return strings.HasPrefix(r.Text, "❯ 1. Red") })
	rule := -1
	for i := opt - 1; i >= 0; i-- {
		if isBoxRule(rows[i], 100) {
			rule = i
			break
		}
	}
	if opt < 0 || rule < 0 || opt-rule < 2 {
		t.Fatalf("capture shape changed: option row %d, rule %d", opt, rule)
	}
	spliced := append(append([]styledRow(nil), rows[:rule+1]...), rows[opt:]...)
	sc := parseClaudeScreen(spliced, 100)
	if sc.BoxVisible || len(sc.Prompts) != 0 {
		t.Errorf("a box-shaped question parsed as the box: %+v", sc)
	}
	if sc.Dialog != "question" || !sc.Dismissable {
		t.Errorf("Dialog=%q/%v, want question/true", sc.Dialog, sc.Dismissable)
	}
}

// TestEntryKeyStableAcrossBulletBlink (r3 H3): a running tool's ⏺ blinks, and
// the entry boundary must be the same in both phases.
func TestEntryKeyStableAcrossBulletBlink(t *testing.T) {
	off := replayClaude(t, "s4-midturn", s4BlinkOff, 39, 100).tailView(0)
	on := replayClaude(t, "s4-midturn", s4BlinkOn, 39, 100).tailView(0)
	if !slices.ContainsFunc(off.Rows, func(r styledRow) bool { return strings.HasPrefix(r.Text, "  Bash(sleep 25") }) {
		t.Fatalf("the OFF frame does not show the blinked-off tool row")
	}
	if !slices.ContainsFunc(on.Rows, func(r styledRow) bool { return strings.HasPrefix(r.Text, "⏺ Bash(sleep 25") }) {
		t.Fatalf("the ON frame does not show the tool bullet")
	}
	a, b := parseClaudeScreen(off.Rows, off.Cols), parseClaudeScreen(on.Rows, on.Cols)
	// Both frames are after the third (pasted) queued send: three entries.
	if !slices.Equal(promptKeys(a.Prompts), promptKeys(b.Prompts)) || len(a.Prompts) != 3 {
		t.Errorf("entries differ across the blink:\n off %q\n on  %q", promptKeys(a.Prompts), promptKeys(b.Prompts))
	}

	// The F9 text capture shows the shape the rule exists for: the blinked-off
	// tool row DIRECTLY under the `❯` entry, with no blank row between.
	first := researchScreen(t, "first turn working")
	second := researchScreen(t, "after mid-turn send")
	want := inputKey("Run the Bash command: sleep 20 && echo DONE-ONE")
	pa, pb := parseClaudeScreen(first, 100), parseClaudeScreen(second, 100)
	if len(pa.Prompts) != 1 || pa.Prompts[0].Key != want {
		t.Errorf("the tool row joined the entry (blink off): %q", promptKeys(pa.Prompts))
	}
	if len(pb.Prompts) != 2 || pb.Prompts[0].Key != want || !pb.Prompts[1].Queued {
		t.Errorf("blink on / queued: %+v", pb.Prompts)
	}
}

// TestRuleRow pins the rule predicate; no capture carries a labelled rule, so
// the label case is a predicate-level row, not a screen.
func TestRuleRow(t *testing.T) {
	full := strings.Repeat("─", 100)
	for _, c := range []struct {
		text string
		cols int
		want bool
	}{
		{full, 100, true},
		{strings.Repeat("─", 70) + " my-agent ──", 100, true},
		{"  " + strings.Repeat("─", 96), 100, true},
		{strings.Repeat("─", 59), 100, false},
		{strings.Repeat("─", 24), 40, true},
		{strings.Repeat("─", 19), 20, false},
		{strings.Repeat("╌", 100), 100, false},
	} {
		if got := isRuleRow(c.text, c.cols); got != c.want {
			t.Errorf("isRuleRow(%q…, %d) = %v", c.text[:min(len(c.text), 12)], c.cols, got)
		}
	}
}
