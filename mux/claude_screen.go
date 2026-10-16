package mux

// ── the Claude Code screen reader ────────────────────────────────────────────
//
// A pure function over styled rows (Pane.tailRows): where the input box is and
// what it holds, which rows above it are the user's own prompts, and whether a
// dialog has replaced the box. It reads cells, so it can be wrong only in the
// direction its callers can survive — a box it cannot find is a pane that is
// not ready, never one that is.
//
// Every shape below is pinned on real Claude Code 2.1.301 captures
// (testdata/claude-screens), and two of the facts they settled shape the code
// more than the design's sketch did:
//
//   - Claude writes the box's `❯` followed by a NO-BREAK SPACE (U+00A0), and its
//     history entries' `❯` followed by an ordinary space. Both are accepted.
//   - Claude's placeholders (`Try "…"`, `Press up to edit queued messages`) are
//     SGR 2 faint; the queued-message hint is NOT faint (a grey colour), so it
//     is excluded by name, not by attribute.

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// claudeScreen is one parse of a Claude Code screen.
type claudeScreen struct {
	BoxVisible bool
	// BoxText is the typed text: the ❯ row and its continuation rows, the
	// two-space indent removed, FAINT runes dropped, each row right-trimmed,
	// joined by "\n". "" = empty, or a placeholder only.
	BoxText string
	BoxKey  string // inputKey(BoxText)
	// BoxShell: the box's glyph is `!`, bash mode (s7). The `!` that switched
	// the mode is not in BoxText.
	BoxShell  bool
	BoxPastes []int // N of every `[Pasted text #N` token inside BoxText
	Insert    bool  // `-- INSERT --` on a row BELOW the box's lower rule
	Working   bool  // a spinner row (`✻ Verb… (…)`) just above the box's top rule
	// Dialog is "" | permission | question | plan | select | trust | bypass |
	// api_key | onboarding. Read only when no box is visible, and only inside
	// the dialog region.
	Dialog      string
	Dismissable bool          // a dialog Escape declines (its `Esc to cancel` row, or a plan dialog)
	Prompts     []promptEntry // USER-PROMPT entries above the box, top to bottom
	MaxPaste    int           // largest N of any `[Pasted text #N` token in the box or the entries

	// conv is the conversation region (every row above the box's top rule)
	// and spinnerRow the first of its rows in the spinner zone (len(conv)
	// when there is no spinner): what acceptance's frontier is read from.
	conv       []styledRow
	spinnerRow int
}

// promptEntry is one user prompt as Claude shows it in the conversation: a `❯`
// row at column 0 plus its continuation rows.
type promptEntry struct {
	Text string // joined like BoxText
	Key  string // inputKey(Text)
	// RenderedKey is renderedKey(Text): Claude renders markdown in history,
	// so this, not Key, is what acceptance compares (F10).
	RenderedKey string
	Paste       int  // N when Text begins with `[Pasted text #N`; 0 otherwise
	Queued      bool // the F9 hint row `ctrl+x ctrl+s to send now` followed it
	// Shell: a bash-mode entry, `! cmd` (s7). Evidence only for a bash-mode
	// send, and a `❯` entry never is for one (matches).
	Shell bool
	// row/end: the screen rows of its `❯` row and of its last row (the
	// queued hint, when it has one). Acceptance places it against the
	// frontier with them.
	row, end int
}

const (
	// dialogRegionMax bounds the dialog region in NON-EMPTY rows. In the
	// 2.1.301 captures the permission marker is the 8th from the bottom.
	dialogRegionMax = 14
	// inputKeyMax bounds every comparison: a longer text is keyed on its first
	// inputKeyMax bytes plus its non-space rune count.
	inputKeyMax = 64 << 10
	// historyMatchMin is the shortest entry key that may match by prefix
	// (acceptance, for a history row Claude abbreviates). 2.1.301 does not
	// abbreviate (testdata: history-long-prompt), so equality is the measured
	// case and this branch only widens it to a long, exact prefix.
	historyMatchMin = 48
)

var (
	pasteTokenRe = regexp.MustCompile(`\[Pasted text #([0-9]+)`)
	// toolRowRe is the `Tool(…)` shape of a tool-call row. It ends a user
	// entry whether or not the row's blinking ⏺ is showing (r3 H3).
	toolRowRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*\(`)
	// queuedHintRe is the F9 hint under a queued (mid-turn) prompt.
	queuedHintRe = regexp.MustCompile(`^\s*ctrl\+x ctrl\+s to send now\s*$`)
	// optionRowRe is a numbered dialog option, optionally under the cursor.
	optionRowRe = regexp.MustCompile(`^\s*(❯\s*)?1\.\s`)
	// cursorRowRe is a boot dialog's selected option (`❯ No, exit`).
	cursorRowRe = regexp.MustCompile(`^\s*❯\s+\S`)
	// boxOptionRe is a ❯ row shaped like a dialog option (`❯ 1. Red`).
	boxOptionRe = regexp.MustCompile(`^❯\s[0-9]+\.\s`)
	// selectFooterRe is the footer of Claude's selection dialogs; under a
	// box-shaped region it says the region is a dialog, not the box.
	selectFooterRe = regexp.MustCompile(`Enter to (select|confirm)\b`)
	// spinnerRe is Claude's working row: a glyph, a capitalised verb ending in
	// an ellipsis, and usually a parenthesised status.
	spinnerRe = regexp.MustCompile(`^[·✢✳✶✻✽*●∗]\s+[A-Z][\w'-]*…`)
)

// claudeDialogMarkers is THE dialog table, for Claude Code 2.1.301
// (testdata/claude-screens s2, s5) and the three boot dialogs of 2.1.290
// (claudish's captures, testdata/claude-screens/claudish-*); cross-checked
// against claudish's screen-classifier (packages/cli/src/pane/
// screen-classifier.ts, CHOICE_DIALOGS / NAMED_DIALOGS / SELECT_FOOTER).
//
// A kind matches only on its marker AND its anchor, both inside the dialog
// region. Order matters: the boot dialogs carry the generic select footer
// (`Enter to confirm · Esc to cancel`), so `select` is last.
var claudeDialogMarkers = []struct {
	kind   string
	marker *regexp.Regexp
	anchor dialogAnchor
}{
	{"permission", regexp.MustCompile(`Do you want to (proceed|create|make this edit to|overwrite|delete)\b.*\?`), anchorOptionBelow},
	{"plan", regexp.MustCompile(`Would you like to proceed\?`), anchorOptionBelow},
	{"question", regexp.MustCompile(`Enter to select · ↑/↓ to navigate`), anchorBottomRows},
	{"trust", regexp.MustCompile(`Is this a project you created or one you trust`), anchorCursorRow},
	{"bypass", regexp.MustCompile(`running in Bypass Permissions mode`), anchorCursorRow},
	{"api_key", regexp.MustCompile(`Detected a custom API key in your environment`), anchorCursorRow},
	{"onboarding", regexp.MustCompile(`Choose the text style`), anchorCursorRow},
	{"select", regexp.MustCompile(`Enter to (confirm|select)\b.*Esc to cancel`), anchorBottomRows},
}

type dialogAnchor int

const (
	anchorOptionBelow dialogAnchor = iota // a `1.` option row within the 2 rows below the marker
	anchorBottomRows                      // the marker is one of the region's bottom 2 rows
	anchorCursorRow                       // a `❯ <option>` row anywhere in the region
)

// dismissableDialogs are the kinds an Escape DECLINES: only the NAMED,
// measured ones. Boot dialogs are never among them — Escape on the trust
// prompt exits Claude — and neither is the `select` catch-all, because the
// boot dialogs carry its very footer: a trust prompt a later Claude rewords
// falls through to `select`, and Escaping it would end the session instead of
// declining anything (review M5a). `select` is reported and waited out.
var dismissableDialogs = map[string]bool{"permission": true, "question": true, "plan": true}

// dismissableWithoutHint are the dismissable kinds whose dialog carries NO
// `Esc to cancel` row, so the hint cannot be what proves Escape declines them.
// The 2.1.301 plan dialog (ExitPlanMode, "Would you like to proceed?") is one:
// measured, Escape closes it, Claude stays in plan mode, nothing is approved,
// and the input box returns (s5-insert-plan, after-escape-plan). Its marker
// and its option-row anchor are what identify it instead.
var dismissableWithoutHint = map[string]bool{"plan": true}

// inputKey is THE normaliser, used on BOTH sides of every comparison: every
// rune for which unicode.IsSpace holds is removed, and nothing else. Wrap
// positions, newline rendering, continuation indentation, the NBSP after the
// box's ❯ and the pane's width therefore cannot cause a mismatch, and any lost
// non-space rune does. A key over inputKeyMax bytes keeps its first
// inputKeyMax bytes plus its total non-space rune count, so a huge paste costs
// a bounded comparison. The bound is applied AFTER whitespace is removed, so
// the bounded key is as wrap-independent as the unbounded one.
func inputKey(s string) string { return normaliseKey(s, keyRaw) }

// renderedKey is inputKey in its RENDERED mode, for one comparison only: a
// HISTORY entry against the text a send verified or sent. Claude renders
// markdown in the history copy of a user prompt and shows the raw text in the
// box (research F10, testdata s6-markdown): `code` → code, “x“ → x,
// **b** → b, *i* → i, ~~s~~ → s, while `_u_`, links, tags, `a\*b` and
// mid-line `#`/`>`/`-`/`1.` are kept as typed. So the three delimiter runes
// '`', '*' and '~' are removed as well, from BOTH sides. Everything else —
// the box verify above all, which must stay exact against the raw box, the
// box-still-holds-it check and the transcript (which stores the raw text) —
// keeps inputKey.
func renderedKey(s string) string { return normaliseKey(s, keyRendered) }

// keyMode selects inputKey's two forms; normaliseKey is their one body.
type keyMode uint8

const (
	keyRaw      keyMode = iota // whitespace removed
	keyRendered                // whitespace and the markdown delimiters removed
)

// isMarkdownDelim: the runes Claude's history rendering drops (F10).
func isMarkdownDelim(r rune) bool { return r == '`' || r == '*' || r == '~' }

func normaliseKey(s string, mode keyMode) string {
	var b strings.Builder
	n, over := 0, false
	for _, r := range s {
		if unicode.IsSpace(r) || (mode == keyRendered && isMarkdownDelim(r)) {
			continue
		}
		n++
		if !over && b.Len()+utf8.RuneLen(r) > inputKeyMax {
			over = true
		}
		if !over {
			b.WriteRune(r)
		}
	}
	if over {
		b.WriteString("\x00")
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

// isRuleRow: a row whose longest run of `─` is at least max(20, ⌈0.6·cols⌉)
// runes. It may carry a label.
func isRuleRow(text string, cols int) bool {
	need := (cols*6 + 9) / 10
	if need < 20 {
		need = 20
	}
	run, best := 0, 0
	for _, r := range text {
		if r == '─' {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 0
		}
	}
	return best >= need
}

// isBoxRule is a rule row starting at column 0, which is how Claude draws the
// box's two rules. Dialog sections indent theirs.
func isBoxRule(r styledRow, cols int) bool {
	return strings.HasPrefix(r.Text, "─") && isRuleRow(r.Text, cols)
}

// promptRowStart recognises a prompt row: a `❯` — or, in bash mode, a `!` —
// at column 0 followed by one space (or the NBSP Claude writes in the box),
// and returns how many runes to skip to reach the text. shell is true for the
// `!` glyph.
//
// The `!` glyph is measured (s7-bashmode): a `!` typed as the first character
// of an empty box is CONSUMED as a mode switch and replaces the `❯`, in the box
// (`!` + NBSP) and in the history entry (`! ` + space). It is never box text.
func promptRowStart(r styledRow) (skip int, shell, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(r.Text, "❯"):
		rest = r.Text[len("❯"):]
	case strings.HasPrefix(r.Text, "!"):
		rest, shell = r.Text[1:], true
	default:
		return 0, false, false
	}
	if rest == "" {
		return 1, shell, true
	}
	c, _ := utf8.DecodeRuneInString(rest)
	if c == ' ' || c == ' ' {
		return 2, shell, true
	}
	return 0, false, false
}

// visibleText is r.Text with its first `skip` runes and every FAINT rune
// dropped, right-trimmed of spaces and NBSPs.
func visibleText(r styledRow, skip int) string {
	var b strings.Builder
	i := 0
	for _, c := range r.Text {
		if i >= skip && (i >= len(r.Faint) || !r.Faint[i]) {
			b.WriteRune(c)
		}
		i++
	}
	return strings.TrimRight(b.String(), "  ")
}

// allFaint reports whether every non-space rune of r is faint.
func allFaint(r styledRow) bool {
	seen := false
	i := 0
	for _, c := range r.Text {
		if !unicode.IsSpace(c) {
			if i >= len(r.Faint) || !r.Faint[i] {
				return false
			}
			seen = true
		}
		i++
	}
	return seen
}

// plain is r.Text with NBSPs as spaces, for the regexes (RE2's \s is ASCII).
func plain(r styledRow) string { return strings.ReplaceAll(r.Text, " ", " ") }

func pasteNs(s string) []int {
	var ns []int
	for _, m := range pasteTokenRe.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			ns = append(ns, n)
		}
	}
	return ns
}

// joinRows builds BoxText / an entry's Text: the first row after its `❯ `,
// each later row without its two-space indent, faint runes dropped, joined by
// "\n", trailing blank lines dropped.
func joinRows(first styledRow, firstSkip int, rest []styledRow) string {
	parts := []string{visibleText(first, firstSkip)}
	for _, r := range rest {
		skip := 0
		if strings.HasPrefix(r.Text, "  ") {
			skip = 2
		}
		parts = append(parts, visibleText(r, skip))
	}
	return strings.TrimRight(strings.Join(parts, "\n"), "\n")
}

// parseClaudeScreen reads one screen. cols is the pane width the rows were laid
// out at (the rule threshold depends on it).
func parseClaudeScreen(rows []styledRow, cols int) claudeScreen {
	var sc claudeScreen
	top, bottom, ok := findBox(rows, cols)
	if ok {
		sc.BoxVisible = true
		skip, shell, _ := promptRowStart(rows[top+1])
		sc.BoxShell = shell
		sc.BoxText = joinRows(rows[top+1], skip, rows[top+2:bottom])
		sc.BoxKey = inputKey(sc.BoxText)
		sc.BoxPastes = pasteNs(sc.BoxText)
		for _, r := range rows[bottom+1:] {
			if strings.Contains(r.Text, "-- INSERT --") {
				sc.Insert = true
			}
		}
		sc.conv, sc.spinnerRow = rows[:top], top
		for i := top - 1; i >= 0 && i >= top-3; i-- {
			if spinnerRe.MatchString(strings.TrimLeft(plain(rows[i]), " ")) {
				sc.Working = true
				sc.spinnerRow = i
			}
		}
		sc.Prompts = parsePromptEntries(rows[:top])
	} else {
		sc.Dialog, sc.Dismissable = parseDialog(rows, cols)
	}
	for _, n := range sc.BoxPastes {
		sc.MaxPaste = max(sc.MaxPaste, n)
	}
	for _, e := range sc.Prompts {
		for _, n := range pasteNs(e.Text) {
			sc.MaxPaste = max(sc.MaxPaste, n)
		}
	}
	return sc
}

// findBox scans from the bottom up for the box: a column-0 rule, directly
// above it zero or more continuation rows (two-space indent, or blank: a typed
// empty line), above those a `❯` row, and directly above that a second
// column-0 rule. All of it must hold. The lowest such structure wins.
//
// A box-shaped selection dialog — `❯ 1. Red` with numbered options under a
// rule — is refused when its `Enter to select|confirm` footer is below the
// lower rule (captures, fact 4): the ❯ row's option shape alone is not enough,
// because a user may type a numbered list.
func findBox(rows []styledRow, cols int) (top, bottom int, ok bool) {
	for b := len(rows) - 1; b >= 2; b-- {
		if !isBoxRule(rows[b], cols) {
			continue
		}
		j := b - 1
		for j > 0 && (rows[j].Text == "" || strings.HasPrefix(rows[j].Text, "  ")) {
			j--
		}
		if _, _, isPrompt := promptRowStart(rows[j]); !isPrompt || j == 0 || !isBoxRule(rows[j-1], cols) {
			continue
		}
		if boxOptionRe.MatchString(plain(rows[j])) && footerBelow(rows[b+1:]) {
			continue
		}
		return j - 1, b, true
	}
	return 0, 0, false
}

func footerBelow(rows []styledRow) bool {
	for _, r := range rows {
		if selectFooterRe.MatchString(plain(r)) {
			return true
		}
	}
	return false
}

// parsePromptEntries finds the USER-PROMPT entries in the conversation region
// (every row above the box's top rule). An entry starts at a non-faint `❯` in
// column 0 followed by a space, and continues over rows that begin with
// exactly two spaces, whose first non-space rune is not ⎿ or ⏺, that are not a
// `Tool(…)` row, not entirely faint and not the queued hint. Claude puts
// assistant text after `⏺`, tool output under `⎿` and spinners behind their
// own glyph, so none of them can put a `❯` in column 0; a model writing `❯` in
// its prose has it after `⏺ ` or on an indented row.
func parsePromptEntries(region []styledRow) []promptEntry {
	var out []promptEntry
	for i := 0; i < len(region); i++ {
		skip, shell, ok := promptRowStart(region[i])
		if !ok || (len(region[i].Faint) > 0 && region[i].Faint[0]) {
			continue
		}
		first := region[i]
		j := i + 1
		queued := false
		for ; j < len(region); j++ {
			r := region[j]
			if queuedHintRe.MatchString(plain(r)) {
				queued = true
				break
			}
			if !continuesEntry(r) {
				break
			}
		}
		text := joinRows(first, skip, region[i+1:j])
		e := promptEntry{Text: text, Key: inputKey(text), RenderedKey: renderedKey(text), Queued: queued, Shell: shell,
			row: i, end: j - 1}
		if queued {
			e.end = j // the hint row
		}
		if m := pasteTokenRe.FindStringSubmatchIndex(text); m != nil && m[0] == 0 {
			e.Paste, _ = strconv.Atoi(text[m[2]:m[3]])
		}
		out = append(out, e)
		i = j - 1
	}
	return out
}

func continuesEntry(r styledRow) bool {
	t := plain(r)
	if !strings.HasPrefix(t, "  ") || strings.HasPrefix(t, "   ") {
		return false
	}
	body := t[2:]
	if strings.HasPrefix(body, "⎿") || strings.HasPrefix(body, "⏺") || toolRowRe.MatchString(body) {
		return false
	}
	return !allFaint(r)
}

// parseDialog reads the dialog region: from the bottom non-empty row upward,
// ending at the first rule row (any rule, indented or not) or after
// dialogRegionMax non-empty rows. Conversation history is above it by
// construction, and the caller reads it only when no box is visible, so a
// model writing "Do you want to proceed?" cannot produce a dialog.
func parseDialog(rows []styledRow, cols int) (kind string, dismissable bool) {
	var region []string // bottom-up, non-empty rows only
	for i := len(rows) - 1; i >= 0 && len(region) < dialogRegionMax; i-- {
		t := strings.TrimRight(plain(rows[i]), " ")
		if t == "" {
			continue
		}
		if isRuleRow(t, cols) {
			break
		}
		region = append(region, t)
	}
	if len(region) == 0 {
		return "", false
	}
	escHint := false
	for i := 0; i < len(region) && i < 2; i++ {
		if strings.Contains(region[i], "Esc to cancel") {
			escHint = true
		}
	}
	for _, d := range claudeDialogMarkers {
		for i, t := range region {
			if !d.marker.MatchString(t) || !anchored(d.anchor, region, i) {
				continue
			}
			return d.kind, dismissableDialogs[d.kind] && (escHint || dismissableWithoutHint[d.kind])
		}
	}
	return "", false
}

// anchored checks a marker found at region[i] (region is bottom-up, so the
// rows BELOW the marker are region[i-1], region[i-2]).
func anchored(a dialogAnchor, region []string, i int) bool {
	switch a {
	case anchorOptionBelow:
		for k := i - 1; k >= 0 && k >= i-2; k-- {
			if optionRowRe.MatchString(region[k]) {
				return true
			}
		}
	case anchorBottomRows:
		return i < 2
	case anchorCursorRow:
		for _, t := range region {
			if cursorRowRe.MatchString(t) {
				return true
			}
		}
	}
	return false
}
