package mux

// Research F10: Claude RENDERS markdown in the history copy of a user prompt
// and shows the RAW text in the input box. Pinned on s6-markdown, a real
// 2.1.301 capture (testdata/claude-screens): a markdown probe typed into the
// box, the same probe in history, claudish's real launch line (a backticked
// path) in both, and the faint suggested-next-prompt Claude leaves in the box
// after a turn.

import (
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/protocol"
)

// The exact text the s6 capture typed. The box shows it raw, so the box
// screens are what these are read off and checked against.
const (
	sentMarkdown = "Reply ok. md probe: `code span` **bold** *ital* _under_ ~~strike~~ [link](http://x.y) <b>tag</b> " +
		`a\*b # notheading > nq - nl 1. nn $HOME ` + "``double`` end"
	sentClaudish = "Your task is in the file `/tmp/claudish-mux-501/launch-NemaeL/turn-1.md`. Reply only with the word ok, do not read it."
	// s6Suggestion is the faint suggestion Claude drew in the box after the
	// claudish turn (claudish-line-in-history).
	s6Suggestion = "now read that file and do the task"
)

// TestBoxShowsMarkdownRaw: the pre-Enter verify stays EXACT against the raw
// box, because the box shows the raw markdown (measured).
func TestBoxShowsMarkdownRaw(t *testing.T) {
	for _, c := range []struct{ label, sent string }{
		{"markdown-in-box", sentMarkdown},
		{"claudish-line-in-box", sentClaudish},
	} {
		sc := claudeScreenAt(t, "s6-markdown", c.label)
		if sc.BoxKey != inputKey(c.sent) {
			t.Errorf("%s: box key %q, want the raw sent key %q", c.label, sc.BoxKey, inputKey(c.sent))
		}
		g, _ := gatedClaudeOn(claudePaneAt(t, "s6-markdown", c.label))
		chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: c.sent, TypedText: true})
		if chk.Verdict != BoxVerified || chk.Verified != verifiedExact {
			t.Errorf("%s: CheckBox %+v, want verified exact", c.label, chk)
		}
		// The verify is exact, not folded: the same text without its
		// backticks is NOT what the box holds.
		bare := strings.NewReplacer("`", "", "*", "", "~", "").Replace(c.sent)
		if chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: bare, TypedText: true}); chk.Verdict != BoxMismatch {
			t.Errorf("%s: a delimiter-stripped Exact verified against the raw box: %+v", c.label, chk)
		}
	}
}

// TestHistoryShowsMarkdownRendered pins the measured rendering itself, so a
// Claude that stops rendering (or renders more) is seen here first.
func TestHistoryShowsMarkdownRendered(t *testing.T) {
	sc := claudeScreenAt(t, "s6-markdown", "markdown-in-history")
	if len(sc.Prompts) != 1 {
		t.Fatalf("entries: %q", promptKeys(sc.Prompts))
	}
	e := sc.Prompts[0]
	for _, gone := range []string{"`", "**", "*ital*", "~~"} {
		if strings.Contains(e.Text, gone) {
			t.Errorf("history still shows %q: %q", gone, e.Text)
		}
	}
	for _, kept := range []string{"_under_", "[link](http://x.y)", "<b>tag</b>", `a\*b`, "$HOME", "code span", "double"} {
		if !strings.Contains(e.Text, kept) {
			t.Errorf("history lost %q: %q", kept, e.Text)
		}
	}
	if e.Key == inputKey(sentMarkdown) {
		t.Fatalf("the history key equals the raw key: the capture no longer shows the F10 rendering")
	}
	if e.RenderedKey != renderedKey(sentMarkdown) {
		t.Errorf("rendered keys differ:\n hist %q\n sent %q", e.RenderedKey, renderedKey(sentMarkdown))
	}
}

func TestRenderedKey(t *testing.T) {
	if got := renderedKey(" `a` **b**\n*c* ~~d~~ _e_ \\*f "); got != `abcd_e_\f` {
		t.Errorf("renderedKey = %q", got)
	}
	if inputKey("`x`") != "`x`" {
		t.Errorf("inputKey must not fold markdown")
	}
	big := strings.Repeat("`x` ", inputKeyMax)
	if renderedKey(big) != renderedKey(strings.ReplaceAll(big, "`", "")) {
		t.Errorf("a bounded rendered key depends on the delimiters")
	}
}

// TestMarkdownPromptAccepted is F10's defect: Claude TOOK the send, and the
// gate must say so. Both pairs go through CheckBox and InputAccepted, the
// path a gated send takes.
func TestMarkdownPromptAccepted(t *testing.T) {
	for _, c := range []struct{ before, after, sent string }{
		{"markdown-in-box", "markdown-in-history", sentMarkdown},
		{"claudish-line-in-box", "claudish-line-in-history", sentClaudish},
	} {
		p := claudePaneAt(t, "s6-markdown", c.before)
		g, _ := gatedClaudeOn(p)
		chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: c.sent, TypedText: true})
		if chk.Verdict != BoxVerified {
			t.Fatalf("%s: check %+v", c.before, chk)
		}
		if got := g.InputAccepted(chk.Baseline); got.State != AcceptInBox {
			t.Errorf("%s: before the Enter: %+v, want InBox", c.before, got)
		}
		swapScreen(t, p, "s6-markdown", c.after)
		if got := g.InputAccepted(chk.Baseline); got.State != AcceptTaken || got.Evidence != evidenceHistory {
			t.Errorf("%s: %+v, want Taken/history", c.after, got)
		}
	}
}

// TestMarkdownFoldDoesNotWidenToOtherWords: the fold drops three delimiter
// runes and nothing else, so a prompt that differs by one backtick-free word
// still has no evidence.
func TestMarkdownFoldDoesNotWidenToOtherWords(t *testing.T) {
	for _, sent := range []string{
		strings.Replace(sentClaudish, "the word ok", "the word no", 1),
		strings.Replace(sentClaudish, "turn-1.md", "turn-2.md", 1),
		strings.Replace(sentClaudish, "`/tmp", "_/tmp", 1), // `_` is kept by Claude
	} {
		p := claudePaneAt(t, "s6-markdown", "claudish-line-in-box")
		g, _ := gatedClaudeOn(p)
		b := baselineAt(t, g, claudeScreenAt(t, "s6-markdown", "claudish-line-in-box"), sent)
		swapScreen(t, p, "s6-markdown", "claudish-line-in-history")
		if got := g.InputAccepted(b); got.State != AcceptNone {
			t.Errorf("%q: %+v, want None", sent, got)
		}
	}
}

// TestSuggestionIsAnEmptyBox: after a turn Claude can put a suggested next
// prompt in the box. It is SGR 2 faint (measured: ESC[2m … ESC[22m in the
// raw bytes), so it is a placeholder: the box is EMPTY for readiness, for the
// Enter-only verify and for acceptance.
func TestSuggestionIsAnEmptyBox(t *testing.T) {
	var box styledRow
	for _, r := range claudePaneAt(t, "s6-markdown", "claudish-line-in-history").tailRows(0) {
		if strings.HasPrefix(r.Text, "❯") && strings.Contains(r.Text, s6Suggestion) {
			box = r
		}
	}
	if box.Text == "" {
		t.Fatalf("no suggestion row in the capture")
	}
	if box.Faint[0] || box.Faint[1] {
		t.Errorf("the box's ❯ and its NBSP must not be faint: %v", box.Faint[:2])
	}
	for i, f := range box.Faint[2:] {
		if !f {
			t.Errorf("suggestion rune %d is not faint", i+2)
		}
	}

	sc := claudeScreenAt(t, "s6-markdown", "claudish-line-in-history")
	if !sc.BoxVisible || sc.BoxText != "" {
		t.Fatalf("box visible=%v text %q, want an empty box", sc.BoxVisible, sc.BoxText)
	}
	for _, e := range sc.Prompts {
		if strings.Contains(e.Text, s6Suggestion) {
			t.Errorf("the suggestion became a prompt entry: %q", e.Text)
		}
	}

	p := claudePaneAt(t, "s6-markdown", "claudish-line-in-history")
	g, _ := gatedClaudeOn(p)
	if r := g.InputReadiness(ReadinessQuery{Class: ClassType, Types: true}); !r.Ready || r.BoxHasText {
		t.Errorf("readiness on the suggestion box: %+v, want ready and empty", r)
	}
	enter := g.CheckBox(BoxExpect{Class: ClassSubmit})
	if enter.Verdict != BoxEmpty {
		t.Errorf("Enter-only CheckBox: %+v, want empty", enter)
	}
	if r := boxVerdictReason(enter.Verdict, BoxExpect{Class: ClassSubmit}); r != protocol.ReasonEmptyBox {
		t.Errorf("Enter-only on the suggestion maps to %q, want empty_box", r)
	}
	// A send of the suggestion's own words: the faint text is not that send
	// sitting in the box.
	b := baselineAt(t, g, claudeScreenAt(t, "s6-markdown", "claudish-line-in-box"), s6Suggestion)
	if got := g.InputAccepted(b); got.State != AcceptNone {
		t.Errorf("acceptance read the suggestion as content: %+v", got)
	}
}
