package mux

import (
	"errors"
	"path/filepath"
	"strings"
)

// ── locating a direct claude invocation in a launch ──────────────────────────
//
// A pane's input is gated when it was DECLARED a Claude pane or when its
// launch is a DIRECT claude invocation (inputgate.go). This file decides the
// second, before spawn, from the same strings the child will run.
//
// It is a minimal POSIX lexer. It evaluates nothing: it produces tokens with
// byte spans in the original string, so the editor-mode rewrite can later
// replace one span and leave every other byte alone. Anything it cannot read
// with certainty — an unbalanced quote, a heredoc, a process substitution —
// makes the whole string unparsed, and an unparsed string is NOT direct. Every
// doubt therefore lands on "not direct", which is today's behaviour: the loose
// factory still observes the pane, and nothing gates it.

// posixShells are the shells whose -c string the lexer may read. fish treats
// `\\` and `\'` as escapes inside single quotes, so its strings are another
// language; under any other shell, --controller is the way in.
var posixShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true}

// claudeWordBase is the program name a direct invocation runs.
const claudeWordBase = "claude"

type shTokKind uint8

const (
	tokWord  shTokKind = iota // a word
	tokOp                     // a control operator: ; & && | || |& ;; ( ) newline
	tokRedir                  // a redirection AND its target: one skipped unit
)

// shTok is one token. start/end are a byte span of the lexed string.
type shTok struct {
	kind       shTokKind
	start, end int
	// lit is a word's value with quoting removed. It means something only
	// when literal is true: a word holding $ or a backtick is an expansion,
	// and its value is whatever the shell makes of it at run time.
	lit     string
	literal bool
	// assign: the raw word begins with NAME= (isAssignment).
	assign bool
}

func (t shTok) raw(s string) string { return s[t.start:t.end] }

// errUnparsed is every lexer refusal. There is one, because the caller's only
// question is "can I be certain", and the answer to every failure is no.
var errUnparsed = errors.New("shell string not parsed")

// launchInfo is what claudeLaunchOf found.
type launchInfo struct {
	// line is the -c string, verbatim, for a POSIX shell launch; "" for an
	// argv launch or any other shape. It is what p.launchLine records.
	line string
	// toks is the lexed line; nil when it was not a shell launch or did not
	// lex (unparsed).
	toks     []shTok
	unparsed bool
	// claude indexes toks at THE claude word, -1 when there is not exactly one.
	claude int
	// args indexes toks at the invocation's argument words, in order: every
	// word after the claude word up to the next control operator, minus the
	// skipped redirections.
	args []int
	// print: the invocation carries -p / --print before a bare `--`. Print
	// mode never shows an input box, so it is never direct.
	print bool
	// direct: exactly one command-position claude word, not in print mode.
	direct bool
}

// claudeLaunchOf decides whether cfg launches claude DIRECTLY:
//
//   - an argv launch whose program's base name is `claude`; or
//   - `[-l] -c <s>` under a POSIX-family shell, where the lexer finds exactly
//     one command-position claude word in <s>.
//
// Print mode (-p / --print before a bare `--`) is not direct in either form.
func claudeLaunchOf(cfg PaneConfig) launchInfo {
	info := launchInfo{claude: -1}
	if filepath.Base(cfg.Cmd) == claudeWordBase {
		info.print = printMode(cfg.Args)
		info.direct = !info.print
		return info
	}
	line, ok := shellLine(cfg)
	if !ok {
		return info
	}
	info.line = line
	toks, err := lexShell(line)
	if err != nil {
		info.unparsed = true
		return info
	}
	info.toks = toks
	if !posixShells[filepath.Base(cfg.Cmd)] {
		// Lexed for the record, never trusted: this is not the language the
		// shell will read.
		return info
	}
	found := 0
	forEachSimpleCommand(toks, func(words []int) {
		at := commandWord(toks, words)
		if at < 0 || !isClaudeWord(toks[words[at]]) {
			return
		}
		found++
		info.claude = words[at]
		info.args = words[at+1:]
	})
	if found != 1 {
		info.claude, info.args = -1, nil
		return info
	}
	vals := make([]string, 0, len(info.args))
	for _, i := range info.args {
		if toks[i].literal {
			vals = append(vals, toks[i].lit)
		} else {
			vals = append(vals, "") // an expansion is not a flag we can see
		}
	}
	info.print = printMode(vals)
	info.direct = !info.print
	return info
}

// shellLine returns <s> from `[-l] -c <s>`.
func shellLine(cfg PaneConfig) (string, bool) {
	a := cfg.Args
	if len(a) == 3 && a[0] == "-l" && a[1] == "-c" {
		return a[2], true
	}
	if len(a) == 2 && a[0] == "-c" {
		return a[1], true
	}
	return "", false
}

// printMode reports -p / --print before a bare `--`.
func printMode(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "-p", "--print":
			return true
		}
	}
	return false
}

// isClaudeWord: a literal word whose value is `claude`, or a path to one.
func isClaudeWord(t shTok) bool {
	return t.kind == tokWord && t.literal && t.lit != "" && filepath.Base(t.lit) == claudeWordBase
}

// forEachSimpleCommand hands fn the word indexes of each simple command —
// the words between control operators, redirections excluded (they are
// skipped units and do not end a command).
func forEachSimpleCommand(toks []shTok, fn func(words []int)) {
	var words []int
	flush := func() {
		if len(words) > 0 {
			fn(words)
		}
		words = nil
	}
	for i, t := range toks {
		switch t.kind {
		case tokOp:
			flush()
		case tokWord:
			words = append(words, i)
		}
	}
	flush()
}

// commandWord returns the position in words of the command word: past the
// leading NAME=value assignments, the prefix words exec / command / nohup,
// and `env` with its own assignments. A prefix word followed by an option
// (`exec -a x claude`, `env -i claude`) is not followed: what the option
// does to the next word is the prefix's business, and guessing it is how a
// pane gets gated that should not be. -1 when the command is only
// assignments.
func commandWord(toks []shTok, words []int) int {
	k := 0
	for k < len(words) && isAssignment(toks[words[k]]) {
		k++
	}
	for k < len(words) {
		t := toks[words[k]]
		if !t.literal {
			return k
		}
		switch t.lit {
		case "exec", "command", "nohup":
			k++
		case "env":
			k++
			for k < len(words) && isAssignment(toks[words[k]]) {
				k++
			}
		default:
			return k
		}
	}
	return -1
}

// isAssignment: the word's RAW text begins with `NAME=`. The raw text, not
// the value: `'A=1'` is a quoted command word whose value merely looks like an
// assignment, and a name cannot contain a quote, so testing the raw bytes is
// exactly the shell's rule. The value after `=` may be anything, expansions
// included (`FOO=$(x) claude` still runs claude).
func isAssignment(t shTok) bool {
	return t.kind == tokWord && t.assign
}

// assignName reports whether s begins with NAME= where NAME is a shell name.
func assignName(s string) bool {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := s[i]
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// ── the lexer ───────────────────────────────────────────────────────────────

func isBlank(c byte) bool { return c == ' ' || c == '\t' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isMeta: a byte that ends an unquoted word.
func isMeta(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', '(', ')', '<', '>':
		return true
	}
	return false
}

// lexShell splits s into words, control operators and redirections, with
// byte spans. It knows:
//
//   - words; '…' (literal); "…" with the \" \\ \$ \` escapes; \x outside quotes;
//   - $(…), ${…} and `…` as opaque word content, depth-tracked and quote-aware;
//   - # comments at the start of a word;
//   - control operators: ; ;; & && | || |& ( ) and newline;
//   - redirections < > >> >| <> <& >& N< N> N>> N>&M &> &>>, each one skipped
//     unit WITH its target word. A redirection does not end a simple command.
//
// A heredoc (<<, <<-), a process substitution (<( >( ), an unbalanced quote or
// substitution, and a redirection with no target all return errUnparsed.
func lexShell(s string) ([]shTok, error) {
	var toks []shTok
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case isBlank(c):
			i++
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i += 2 // a line continuation between words is a blank
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '&' && i+1 < len(s) && s[i+1] == '>', c == '<', c == '>':
			t, err := lexRedir(s, i, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i = t.end
		case isDigit(c) && ioNumberOp(s, i) > 0:
			t, err := lexRedir(s, i, ioNumberOp(s, i))
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i = t.end
		case c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')':
			n := 1
			if i+1 < len(s) {
				switch s[i : i+2] {
				case "&&", "||", ";;", "|&":
					n = 2
				}
			}
			toks = append(toks, shTok{kind: tokOp, start: i, end: i + n})
			i += n
		default:
			end, lit, literal, err := readWord(s, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, shTok{
				kind: tokWord, start: i, end: end,
				lit: lit, literal: literal, assign: assignName(s[i:end]),
			})
			i = end
		}
	}
	return toks, nil
}

// ioNumberOp returns the index of the < or > that an all-digit prefix at i
// is the IO number of (`2>`), or 0 when the digits are an ordinary word.
func ioNumberOp(s string, i int) int {
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if j < len(s) && (s[j] == '<' || s[j] == '>') {
		return j
	}
	return 0
}

// lexRedir reads one redirection whose operator begins at op (start is where
// its IO number, if any, began) together with its target word.
func lexRedir(s string, start, op int) (shTok, error) {
	next := func(k int) byte {
		if k < len(s) {
			return s[k]
		}
		return 0
	}
	j := op
	switch s[j] {
	case '&': // &> and &>>
		j += 2
		if next(j) == '>' {
			j++
		}
	case '<':
		switch next(j + 1) {
		case '<', '(': // a heredoc; a process substitution
			return shTok{}, errUnparsed
		case '>', '&':
			j += 2
		default:
			j++
		}
	default: // '>'
		switch next(j + 1) {
		case '(':
			return shTok{}, errUnparsed
		case '>', '|', '&':
			j += 2
		default:
			j++
		}
	}
	for j < len(s) && isBlank(s[j]) {
		j++
	}
	if j >= len(s) || isMeta(s[j]) || s[j] == '#' || (s[j] == '\\' && next(j+1) == '\n') {
		return shTok{}, errUnparsed // no target
	}
	end, _, _, err := readWord(s, j)
	if err != nil {
		return shTok{}, err
	}
	return shTok{kind: tokRedir, start: start, end: end}, nil
}

// readWord reads the word starting at i and returns where it ends, its value
// with quoting removed, and whether that value is literal (no $ or backtick).
func readWord(s string, i int) (end int, lit string, literal bool, err error) {
	var b strings.Builder
	literal = true
	for i < len(s) && !isMeta(s[i]) {
		switch c := s[i]; c {
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return 0, "", false, errUnparsed
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case '"':
			j, err := readDouble(s, i+1, &b, &literal)
			if err != nil {
				return 0, "", false, err
			}
			i = j
		case '\\':
			if i+1 >= len(s) {
				return 0, "", false, errUnparsed
			}
			if s[i+1] != '\n' { // \ + newline is a continuation: nothing
				b.WriteByte(s[i+1])
			}
			i += 2
		case '$', '`':
			j, err := skipExpansion(s, i)
			if err != nil {
				return 0, "", false, err
			}
			literal = false
			b.WriteString(s[i:j])
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return i, b.String(), literal, nil
}

// readDouble reads a "…" body starting just after the opening quote, writes
// its value to b, and returns the index after the closing quote.
func readDouble(s string, i int, b *strings.Builder, literal *bool) (int, error) {
	for i < len(s) {
		switch c := s[i]; c {
		case '"':
			return i + 1, nil
		case '\\':
			if i+1 < len(s) {
				switch n := s[i+1]; n {
				case '"', '\\', '$', '`':
					b.WriteByte(n)
					i += 2
					continue
				case '\n':
					i += 2
					continue
				}
			}
			b.WriteByte('\\')
			i++
		case '$', '`':
			j, err := skipExpansion(s, i)
			if err != nil {
				return 0, err
			}
			*literal = false
			b.WriteString(s[i:j])
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return 0, errUnparsed
}

// skipExpansion returns the index just past the expansion that starts at i:
// $(…), $((…)), ${…} and `…` are skipped whole; any other $ is one byte (the
// name after it is ordinary word text, and the word is non-literal anyway).
func skipExpansion(s string, i int) (int, error) {
	if s[i] == '`' {
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '`':
				return j + 1, nil
			}
		}
		return 0, errUnparsed
	}
	if i+1 < len(s) {
		switch s[i+1] {
		case '(':
			return skipNested(s, i+2, '(', ')')
		case '{':
			return skipNested(s, i+2, '{', '}')
		}
	}
	return i + 1, nil
}

// skipNested skips to just past the close that balances an already-consumed
// open, honouring quotes, escapes and nested expansions on the way.
func skipNested(s string, i int, open, close byte) (int, error) {
	depth := 1
	var discard strings.Builder
	lit := true
	for i < len(s) {
		switch c := s[i]; {
		case c == '\\':
			i += 2
			continue
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return 0, errUnparsed
			}
			i += j + 2
			continue
		case c == '"':
			j, err := readDouble(s, i+1, &discard, &lit)
			if err != nil {
				return 0, err
			}
			i = j
			continue
		case c == '`':
			j, err := skipExpansion(s, i)
			if err != nil {
				return 0, err
			}
			i = j
			continue
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
		i++
	}
	return 0, errUnparsed
}
