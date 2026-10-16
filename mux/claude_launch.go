package mux

import (
	"errors"
	"os"
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
	// noBox: the invocation never shows an input box — print mode (-p /
	// --print), a subcommand (`claude mcp list`, `claude update`), or
	// --version / --help — so it is never direct (noInputBox).
	noBox bool
	// direct: exactly one command-position claude word, and not noBox.
	direct bool
}

// claudeLaunchOf decides whether cfg launches claude DIRECTLY:
//
//   - an argv launch whose program's base name is `claude`; or
//   - `[-l] -c <s>` under a POSIX-family shell, where the lexer finds exactly
//     one command-position claude word in <s>.
//
// An invocation that never shows an input box (noInputBox: print mode, a
// subcommand, --version / --help) is not direct in either form, so it is
// neither gated nor rewritten.
func claudeLaunchOf(cfg PaneConfig) launchInfo {
	info := launchInfo{claude: -1}
	if filepath.Base(cfg.Cmd) == claudeWordBase {
		info.noBox = noInputBox(cfg.Args)
		info.direct = !info.noBox
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
	info.noBox = noInputBox(vals)
	info.direct = !info.noBox
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

// claudeSubcommands are Claude Code's subcommands (2.1.301 `claude --help`,
// plus `config` and `migrate-installer` from earlier releases). None of them
// is the interactive session with an input box. `attach` does open a session,
// but one magmux did not launch and has not rewritten, so it is left
// ungated with the rest: doubt lands on "not direct".
var claudeSubcommands = map[string]bool{
	"agents": true, "attach": true, "auth": true, "auto-mode": true, "config": true,
	"doctor": true, "gateway": true, "import": true, "install": true, "kill": true,
	"logs": true, "mcp": true, "migrate-installer": true, "plugin": true, "plugins": true,
	"purge": true, "respawn": true, "rm": true, "setup-token": true, "stop": true,
	"ultrareview": true, "update": true, "upgrade": true,
}

// noInputBox reports an invocation that never shows an input box, judged on
// the arguments before a bare `--`:
//
//   - -p / --print, or -v / --version / -h / --help, anywhere;
//   - a subcommand as the FIRST argument, with no option before it.
//
// A subcommand anywhere later is NOT recognised, deliberately (r3 gpt H2).
// Claude's options take values of every arity — none (`--verbose`), one
// (`--name <name>`), several (`--add-dir <directories...>`, `--mcp-config
// <configs...>`) and an optional one (`--resume [value]`, `-d, --debug
// [filter]`; claude-2.1.301-help.txt) — so after any option a positional word
// may be that option's value, and telling `claude --name doctor` (an
// interactive session NAMED doctor) from `claude --model opus mcp list` needs
// an arity table magmux does not own and that changes between releases. The
// conservative answer is the gated one: a direct launch is gated and
// rewritten, and a send to a pane that is really non-interactive just answers
// busy (no input box). Ungating an interactive session would lose both the
// editor-mode override and the gate.
//
// An expansion ("" here) is a positional that names nothing.
func noInputBox(args []string) bool {
	for i, a := range args {
		switch a {
		case "--":
			return false
		case "-p", "--print", "-v", "--version", "-h", "--help":
			return true
		}
		if i == 0 && claudeSubcommands[a] {
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
			if i+1 >= len(s) { // a trailing backslash: readWord's rule
				return 0, errUnparsed
			}
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

// ── the editor-mode rewrite ─────────────────────────────────────────────────
//
// A DIRECT invocation is rewritten so Claude starts with editorMode "normal",
// whatever the user's own settings say: magmux types into the input box, and a
// box in vim mode eats the first keystrokes as commands. The rewrite is ONE
// span replaced in the -c string (or one argv element replaced or inserted):
//
//   - no --settings: ` --settings=<Q>` is inserted after the claude word;
//   - an inline JSON --settings: merged, and the flag's whole span (one token,
//     or two adjacent ones) becomes `--settings=<Q>`;
//   - a settings FILE: merged into a private 0600 copy (claude_settings.go),
//     and the span becomes `--settings='<copy>'`.
//
// The `=` form always, because extractClaudePrompt skips a flag as one token
// and would return a two-token flag's value as the prompt. Claude keeps only
// the LAST --settings (F1), so the last one is the one merged.
//
// Every doubt — an expansion in the value, a relative path after a `cd`, a
// file that will not parse, any shell syntax between `--settings` and its
// value ([G]) — leaves the command byte-identical and records `unparsed`. The
// pane still launches, and the gate's vim_mode check is what then refuses to
// type into it.

const settingsFlag = "--settings"

// launchRewrite is what rewriteClaudeLaunch decided.
type launchRewrite struct {
	// args is what to exec the pane's program with: cfg.Args itself when
	// nothing was rewritten.
	args []string
	// override is "" when the pane is not a rewrite candidate at all (not
	// direct, plugin-owned, ungated), and otherwise the outcome.
	override editorOverride
	// file is a private settings copy the pane now OWNS and must remove; "".
	file string
	// why says what made it unparsed, for the debug log.
	why string
}

// settingsUse is one --settings occurrence in an invocation.
type settingsUse struct {
	value string // the value with quoting removed
	tilde bool   // the value begins with an UNQUOTED ~/ (the shell expands it)
	// start/end: the flag's span in the -c string (both tokens of a two-token
	// flag). For an argv launch, start is the index into cfg.Args and end is
	// one past its value.
	start, end int
}

// rewriteClaudeLaunch decides the editor-mode rewrite of cfg. basis is the
// pane's gateBasis: only a gated, direct pane is ever rewritten (a plugin's
// direct-claude pane is not gated and is left alone). dir is the pane's
// private settings directory.
func rewriteClaudeLaunch(cfg PaneConfig, launch launchInfo, basis gateBasis, dir string) launchRewrite {
	rw := launchRewrite{args: cfg.Args}
	if basis == gateNone || !launch.direct {
		return rw
	}
	unparsed := func(why string) launchRewrite {
		return launchRewrite{args: cfg.Args, override: overrideUnparsed, why: why}
	}

	var (
		uses []settingsUse
		why  string
		// relOK: a relative settings path resolves against cfg.Dir only when
		// nothing runs before the invocation; a `cd` would move the base.
		relOK = true
		// homeOK: a `~/` path is expanded with the HOME the shell has when it
		// reaches the invocation, so nothing before it may be able to set it.
		homeOK = true
	)
	if launch.line == "" {
		uses, why = argvSettings(cfg.Args)
	} else {
		uses, why = shellSettings(launch)
		for _, t := range launch.toks[:launch.claude] {
			if t.kind == tokOp {
				relOK = false
				break
			}
		}
		homeOK = homeKept(launch)
	}
	if why != "" {
		return unparsed(why)
	}

	if len(uses) == 0 {
		q, err := mergeEditorMode([]byte("{}"))
		if err != nil {
			return unparsed(err.Error())
		}
		rw.override = overrideAdded
		if launch.line == "" {
			rw.args = append([]string{settingsFlag + "=" + string(q)}, cfg.Args...)
			return rw
		}
		c := launch.toks[launch.claude]
		rw.args = withLine(cfg.Args, launch.line[:c.end]+" "+settingsFlag+"="+shellQuoteJSON(q)+launch.line[c.end:])
		return rw
	}

	use := uses[len(uses)-1] // Claude keeps the last (F1)
	var word string          // the replacement's value part
	if isInlineSettings(use.value) {
		merged, err := mergeEditorMode([]byte(use.value))
		if err != nil {
			return unparsed("inline --settings: " + err.Error())
		}
		rw.override = overrideMerged
		word = string(merged)
		if launch.line != "" {
			word = shellQuoteJSON(merged)
		}
	} else {
		path, why := settingsPath(use, relOK, homeOK, cfg.Dir, cfg.Env)
		if why != "" {
			return unparsed(why)
		}
		b, err := readSettingsFile(path)
		if err != nil {
			return unparsed("settings file: " + err.Error())
		}
		merged, err := mergeEditorMode(b)
		if err != nil {
			return unparsed("settings file " + path + ": " + err.Error())
		}
		file, err := writeSettingsCopy(dir, merged)
		if err != nil {
			return unparsed("private copy: " + err.Error())
		}
		rw.override, rw.file, word = overrideMergedFile, file, file
		if launch.line != "" {
			word = "'" + file + "'"
		}
	}

	flag := settingsFlag + "=" + word
	if launch.line == "" {
		args := make([]string, 0, len(cfg.Args))
		args = append(args, cfg.Args[:use.start]...)
		args = append(args, flag)
		rw.args = append(args, cfg.Args[use.end:]...)
		return rw
	}
	rw.args = withLine(cfg.Args, launch.line[:use.start]+flag+launch.line[use.end:])
	return rw
}

// isInlineSettings: the value is JSON text rather than a path — its first byte
// past JSON's OWN whitespace (space, tab, LF, CR; not strings.TrimSpace's
// wider set, which would accept a `\v{}` Claude itself cannot parse) is `{`.
func isInlineSettings(v string) bool {
	return strings.HasPrefix(strings.TrimLeft(v, " \t\n\r"), "{")
}

// withLine returns a copy of a `[-l] -c <s>` argv with <s> replaced.
func withLine(args []string, line string) []string {
	out := append([]string(nil), args...)
	out[len(out)-1] = line
	return out
}

// settingsPath resolves a --settings value that is not inline JSON to the file
// it names, or says why it cannot: an expansion, `-`, a relative path that a
// preceding command could have moved the base of, a `~/` whose HOME is not
// knowable. env is cfg.Env, the child's overrides.
func settingsPath(use settingsUse, relOK, homeOK bool, base string, env []string) (string, string) {
	v := use.value
	switch {
	case v == "" || v == "-":
		return "", "--settings has no usable value"
	case strings.ContainsAny(v, "$`"):
		return "", "--settings value holds an expansion"
	case use.tilde:
		// The CHILD's shell expands the ~, with the child's HOME (review M5):
		// cfg.Env's last HOME= when it sets one, else the HOME the child
		// inherits from magmux. A command before the invocation that could
		// change HOME (`export HOME=…;`) makes it unknowable; a plain `cd`
		// cannot (homeKept).
		if !homeOK {
			return "", "~/ --settings path after a command that could change HOME"
		}
		home, ok := childHome(env)
		if !ok {
			return "", "~/ --settings path with no absolute HOME for the child"
		}
		return filepath.Join(home, v[2:]), ""
	case filepath.IsAbs(v):
		return v, ""
	case !relOK:
		return "", "relative --settings path after another command"
	}
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", "relative --settings path with no working directory"
		}
		base = wd
	}
	return filepath.Join(base, v), ""
}

// homeSafeCommands are the commands that cannot change the shell's HOME:
// directory moves and a few inert builtins. Everything else — an assignment,
// export / declare / readonly / unset, eval, source / `.`, env, read, a
// function definition, any command magmux does not know — might, and so does
// a word that is an expansion (`${HOME:=x}` assigns). An allowlist, because
// doubt must land on unparsed, never on a guessed HOME.
var homeSafeCommands = map[string]bool{
	"cd": true, "pushd": true, "popd": true, "true": true, "false": true, ":": true,
	"echo": true, "mkdir": true, "sleep": true, "pwd": true, "clear": true,
}

// homeKept reports that no simple command BEFORE the invocation (the ones a
// control operator ends) can change HOME: each is a homeSafeCommand whose
// every word is literal and none an assignment. The invocation's own prefix
// assignments (`HOME=/x claude …`) are not before it: the shell expands the
// `~` before it applies them, with the HOME it already had.
func homeKept(launch launchInfo) bool {
	last := -1
	for i, t := range launch.toks[:launch.claude] {
		if t.kind == tokOp {
			last = i
		}
	}
	if last < 0 {
		return true
	}
	ok := true
	forEachSimpleCommand(launch.toks[:last], func(words []int) {
		first := launch.toks[words[0]]
		if !first.literal || !homeSafeCommands[first.lit] {
			ok = false
		}
		for _, w := range words {
			if t := launch.toks[w]; !t.literal || isAssignment(t) {
				ok = false
			}
		}
	})
	return ok
}

// childHome is the HOME the pane's child will have: the LAST HOME= in env
// (cfg.Env goes last in spawnPTY, and os/exec keeps the last occurrence),
// else magmux's own, which the child inherits. Only an absolute directory is
// an answer — an empty or relative HOME is not something magmux can expand
// the way the child's shell would.
func childHome(env []string) (string, bool) {
	home, set := "", false
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "HOME="); ok {
			home, set = v, true
			break
		}
	}
	if !set {
		home = os.Getenv("HOME")
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", false
	}
	return filepath.Clean(home), true
}

// shellSettings finds every --settings in a lexed direct invocation, in
// order, or says why the invocation cannot be rewritten safely. The rules:
//
//   - after a bare `--`, nothing is a flag;
//   - a word that is an expansion and could spell --settings is unparsed;
//   - `--settings=<w>` is one token; its value must be literal;
//   - `--settings <w>` is two, and only when the bytes between them are blanks
//     and nothing else ([G]): a redirection, a comment, a line continuation or
//     a control operator there is unparsed, and so is a value that is an
//     assignment, an expansion, empty, or another flag.
func shellSettings(launch launchInfo) ([]settingsUse, string) {
	line, toks := launch.line, launch.toks
	var uses []settingsUse
	for k := 0; k < len(launch.args); k++ {
		i := launch.args[k]
		t := toks[i]
		if !t.literal {
			if strings.HasPrefix(t.lit, settingsFlag) {
				return nil, "--settings spelled with an expansion"
			}
			continue
		}
		switch {
		case t.lit == "--":
			return uses, ""
		case t.lit == settingsFlag:
			if i+1 >= len(toks) || toks[i+1].kind != tokWord {
				return nil, "--settings with no value word after it"
			}
			v := toks[i+1]
			if gap := line[t.end:v.start]; gap == "" || strings.Trim(gap, " \t") != "" {
				return nil, "shell syntax between --settings and its value"
			}
			if v.assign || !v.literal || v.lit == "" || strings.HasPrefix(v.lit, "-") {
				return nil, "--settings value is not a plain word"
			}
			uses = append(uses, settingsUse{
				value: v.lit, tilde: strings.HasPrefix(v.raw(line), "~/"),
				start: t.start, end: v.end,
			})
			if k+1 < len(launch.args) && launch.args[k+1] == i+1 {
				k++
			}
		case strings.HasPrefix(t.lit, settingsFlag+"="):
			uses = append(uses, settingsUse{
				value: t.lit[len(settingsFlag)+1:],
				tilde: strings.HasPrefix(t.raw(line), settingsFlag+"=~/"),
				start: t.start, end: t.end,
			})
		}
	}
	return uses, ""
}

// argvSettings is shellSettings for an argv launch, where every element is
// already a literal value and there is no shell syntax to be adjacent to.
func argvSettings(args []string) ([]settingsUse, string) {
	var uses []settingsUse
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return uses, ""
		case a == settingsFlag:
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return nil, "--settings with no value"
			}
			uses = append(uses, settingsUse{value: args[i+1], start: i, end: i + 2})
			i++
		case strings.HasPrefix(a, settingsFlag+"="):
			uses = append(uses, settingsUse{value: a[len(settingsFlag)+1:], start: i, end: i + 1})
		}
	}
	return uses, ""
}
