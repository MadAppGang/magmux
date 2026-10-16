package mux

import (
	"os/exec"
	"testing"
)

// shellCfg is the PaneConfig `-e` and open_pane build: the login shell, `-l -c`.
func shellCfg(shell, line string) PaneConfig {
	return PaneConfig{Cmd: shell, Args: []string{"-l", "-c", line}}
}

// TestClaudeLaunchOf is the detection half of the lexer: which launches are a
// DIRECT claude invocation. Everything that is not direct keeps today's loose
// factory for observation and is never gated, so every doubtful row must land
// on "not direct".
func TestClaudeLaunchOf(t *testing.T) {
	rows := []struct {
		line   string
		direct bool
	}{
		// direct
		{"claude", true},
		{"claude 'say hi'", true},
		{"claude say hi", true},
		{"cd /x && claude --model opus 'p'", true},
		{"FOO=1 exec claude", true},
		{"FOO=1 BAR='a b' claude", true},
		{"env FOO=1 claude", true},
		{"nohup claude", true},
		{"command claude", true},
		{"/opt/homebrew/bin/claude", true},
		{"'claude' 'p'", true},
		{`"claude"`, true},
		{"claude >run.log --settings '{\"editorMode\":\"vim\"}' 'p'", true},
		{"claude 2>&1 --settings='{}'", true},
		{"claude $(cat p.txt)", true},
		{"(claude)", true},
		{"claude -- -p", true},             // after a bare --, -p is not a flag
		{"claude --model m -y # -p", true}, // a comment is not an argument
		{"echo start; claude", true},
		{"claude --print-config", true}, // not --print itself

		// not direct
		{"claude -p 'x'", false},
		{"claude --print 'x'", false},
		{"cd /x && claude --model m -p 'x'", false},
		{"bash -c 'claude'", false},
		{"claude; claude", false},
		{"claude && claude", false},
		{"echo claude", false},
		{"echo claude ", false},
		{"# claude", false},
		{"grep claude x", false},
		{"tail -f /tmp/claude", false},
		{`"$CLAUDE_BIN"`, false},
		{"$CLAUDE_BIN 'p'", false},
		{"`which claude`", false},
		{"claudish -i --model m -y", false},
		{"claude-code", false},
		{"exec -a x claude", false}, // an option after a prefix word: not followed
		{"env -i claude", false},
		{"claude <<EOF\nhi\nEOF", false}, // heredoc: unparsed
		{"claude <(echo x)", false},      // process substitution: unparsed
		{"claude 'unbalanced", false},
		{`claude "unbalanced`, false},
		{"claude $(unbalanced", false},
		{"claude >", false}, // a redirection with no target
		{"", false},

		// Non-interactive invocations never show an input box (review M4):
		// subcommands, and the version / help flags anywhere before `--`.
		{"claude mcp list", false},
		{"claude mcp add x -- npx y", false},
		{"claude update", false},
		{"claude upgrade", false},
		{"claude doctor", false},
		{"claude config get model", false},
		{"claude plugin install x", false},
		{"claude auth status", false},
		{"claude setup-token", false},
		{"claude install stable", false},
		{"claude --version", false},
		{"claude -v", false},
		{"claude --help", false},
		{"claude -h", false},
		{"claude --model opus --version", false},
		{"claude --model opus mcp list", true}, // after an option: direct, gated (r3 H2)
		{"claude --settings='{}' mcp list", true},
		{"cd /x && claude doctor", false},
		{"claude 'update the docs'", true},     // a prompt is ONE word; "update" alone is the subcommand
		{"claude fix the doctor script", true}, // only the first positional names a subcommand
		{"claude -- --version", true},          // after a bare --, nothing is a flag
		{"claude --model mcp-model 'p'", true},

		// $TMPDIR containing "claude" (review observation 8): only a command
		// word whose basename IS `claude` is direct, never a path through a
		// directory named claude-501.
		{"/private/tmp/claude-501/x/fakeclaude --flag", false},
		{"/tmp/claude-501/bin/sh -c 'echo hi'", false},
		{"cd /tmp/claude-501 && ./run.sh", false},
		{"/tmp/claude-501/claude", true}, // the basename is claude: a claude binary
	}
	for _, r := range rows {
		got := claudeLaunchOf(shellCfg("/bin/zsh", r.line))
		if got.direct != r.direct {
			t.Errorf("claudeLaunchOf(%q).direct = %v, want %v (info %+v)", r.line, got.direct, r.direct, got)
		}
		if got.line != r.line {
			t.Errorf("claudeLaunchOf(%q).line = %q; the launch line is the -c string verbatim", r.line, got.line)
		}
	}
}

// TestTmpdirClaudeIsObservedNotGated (review observation 8): a $TMPDIR such as
// /private/tmp/claude-501 puts "claude" in paths. The lexer only ever calls a
// command word whose basename is `claude` direct, so none of these is gated.
// The LOOSE factory is a substring match and still attaches an observe-only
// controller where its rule fires (`fakeclaude --flag` holds "claude "); that
// is today's behaviour, it observes and never gates.
func TestTmpdirClaudeIsObservedNotGated(t *testing.T) {
	for _, c := range []struct {
		line     string
		observed bool
	}{
		{"/private/tmp/claude-501/x/fakeclaude --flag", true},
		{"/tmp/claude-501/bin/sh -c 'echo hi'", false},
		{"cd /tmp/claude-501 && ./run.sh", false},
	} {
		cfg := shellCfg("/bin/zsh", c.line)
		if basis, _ := gateDecision("", claudeLaunchOf(cfg)); basis != gateNone {
			t.Errorf("%q: gate basis %q, want none", c.line, basis)
		}
		p := &Pane{cmd: exec.Command(cfg.Cmd, cfg.Args...)}
		if got := claudeCodeFactory(p) != nil; got != c.observed {
			t.Errorf("%q: loose factory attached = %v, want %v", c.line, got, c.observed)
		}
		if inputGateOf(p) != nil {
			t.Errorf("%q: gated", c.line)
		}
	}
}

// TestClaudeLaunchOfShells: the lexer is POSIX, so only POSIX-family shells
// are read at all (fish quotes differently); and an argv launch is judged on
// its own argv.
func TestClaudeLaunchOfShells(t *testing.T) {
	for _, sh := range []string{"/bin/sh", "/bin/bash", "/bin/zsh", "dash", "/usr/bin/ksh", "mksh"} {
		if !claudeLaunchOf(shellCfg(sh, "claude 'p'")).direct {
			t.Errorf("%s -l -c \"claude 'p'\" is not direct", sh)
		}
	}
	if claudeLaunchOf(shellCfg("/opt/homebrew/bin/fish", "claude 'p'")).direct {
		t.Error("a fish -c string was read as direct; the lexer is POSIX")
	}
	if !claudeLaunchOf(PaneConfig{Cmd: "/bin/sh", Args: []string{"-c", "claude"}}).direct {
		t.Error("`sh -c claude` (no -l) is not direct")
	}
	if claudeLaunchOf(PaneConfig{Cmd: "/bin/sh", Args: []string{"-l"}}).direct {
		t.Error("a bare login shell is direct")
	}
	if l := claudeLaunchOf(PaneConfig{Cmd: "/bin/sh", Args: []string{"-l"}}); l.line != "" {
		t.Errorf("a bare login shell has a launch line %q", l.line)
	}

	argv := []struct {
		cfg    PaneConfig
		direct bool
	}{
		{PaneConfig{Cmd: "claude"}, true},
		{PaneConfig{Cmd: "/usr/local/bin/claude", Args: []string{"--model", "m", "say hi"}}, true},
		{PaneConfig{Cmd: "claude", Args: []string{"-p", "x"}}, false},
		{PaneConfig{Cmd: "claude", Args: []string{"--", "-p"}}, true},
		{PaneConfig{Cmd: "claudish", Args: []string{"-i"}}, false},
		{PaneConfig{Cmd: "claude", Args: []string{"mcp", "list"}}, false},
		{PaneConfig{Cmd: "claude", Args: []string{"--version"}}, false},
		{PaneConfig{Cmd: "/usr/local/bin/claude", Args: []string{"doctor"}}, false},
		{PaneConfig{Cmd: "claude", Args: []string{"update the docs"}}, true},
		{PaneConfig{Cmd: "/private/tmp/claude-501/x/fakeclaude", Args: []string{"--flag"}}, false},
	}
	for _, r := range argv {
		l := claudeLaunchOf(r.cfg)
		if l.direct != r.direct {
			t.Errorf("argv %q %q: direct = %v, want %v", r.cfg.Cmd, r.cfg.Args, l.direct, r.direct)
		}
		if l.line != "" {
			t.Errorf("argv %q: launch line %q, want none (argv is read as argv)", r.cfg.Cmd, l.line)
		}
	}
}

// TestLexShellSpans: every token's span is a byte range of the original
// string, and a redirection is ONE skipped unit with its target. Phase 6's
// rewrite replaces spans, so they have to be exact.
func TestLexShellSpans(t *testing.T) {
	line := `FOO=1 claude 2>&1 --settings='{"a":1}' "do x"; echo $(a "b)" c) ok`
	toks, err := lexShell(line)
	if err != nil {
		t.Fatalf("lexShell: %v", err)
	}
	want := []struct {
		kind shTokKind
		raw  string
	}{
		{tokWord, "FOO=1"},
		{tokWord, "claude"},
		{tokRedir, "2>&1"},
		{tokWord, `--settings='{"a":1}'`},
		{tokWord, `"do x"`},
		{tokOp, ";"},
		{tokWord, "echo"},
		{tokWord, `$(a "b)" c)`},
		{tokWord, "ok"},
	}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d: %+v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].kind != w.kind || toks[i].raw(line) != w.raw {
			t.Errorf("token %d = (%v, %q), want (%v, %q)", i, toks[i].kind, toks[i].raw(line), w.kind, w.raw)
		}
	}
	if toks[3].lit != `--settings={"a":1}` || !toks[3].literal {
		t.Errorf("--settings token value = %q literal=%v", toks[3].lit, toks[3].literal)
	}
	if toks[4].lit != "do x" {
		t.Errorf("quoted prompt value = %q, want %q", toks[4].lit, "do x")
	}
	if toks[7].literal {
		t.Error("a $(…) word was reported literal")
	}
}

// TestExtractReadsLaunchLine: the prompt and cwd extractors read the pane's
// pre-rewrite launch line when it has one, and fall back to cmd.Args when it
// does not, which is every struct-literal test pane. For a shell launch both
// sources give the same answer, which is the property that lets Phase 6
// rewrite the exec'd string without moving discovery.
func TestExtractReadsLaunchLine(t *testing.T) {
	dir := t.TempDir()
	for _, line := range []string{
		"claude 'say hi'",
		"cd " + dir + " && claude --model opus 'list files'",
		"claude say hi",
		"claude --dangerously-skip-permissions",
		"claudish -i --model m -y",
	} {
		lit := &Pane{cmd: exec.Command("zsh", "-l", "-c", line)}
		built := &Pane{cmd: exec.Command("zsh", "-l", "-c", "REWRITTEN"), launchLine: line}
		if a, b := extractClaudePrompt(lit), extractClaudePrompt(built); a != b {
			t.Errorf("%q: prompt from cmd.Args %q, from launchLine %q", line, a, b)
		}
		if a, b := extractClaudeCwd(lit), extractClaudeCwd(built); a != b {
			t.Errorf("%q: cwd from cmd.Args %q, from launchLine %q", line, a, b)
		}
	}
}
