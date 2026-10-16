package mux

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// normalQ is the word the rewrite emits for an empty merge.
const normalQ = `--settings='{"editorMode":"normal"}'`

// rewriteRow is one TestRewriteClaudeLaunch case. want "" means "byte-
// identical" (or, for merged_file, "names the private copy"); {DIR} in line
// or want is the test's scratch directory.
type rewriteRow struct {
	name     string
	line     string
	want     string
	override editorOverride
}

// rewriteFixtures writes the settings files the table names and returns the
// directory and the substitution for its {DIR} placeholder.
func rewriteFixtures(t *testing.T) (dir string, sub func(string) string) {
	t.Helper()
	dir = sockTestDir(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("s.json", `{"a":1,"editorMode":"vim","env":{"SECRET":"v"}}`)
	write("rel.json", `{"r":true}`)
	write("arr.json", `[1,2]`)
	write("bad.json", `{"a":`)
	return dir, func(s string) string { return strings.ReplaceAll(s, "{DIR}", dir) }
}

// rewriteTable is the §8 Phase 6 table. {DIR} is a scratch directory holding
// s.json (absolute), rel.json, arr.json and bad.json.
func rewriteTable() []rewriteRow {
	return []rewriteRow{
		// added
		{"bare", "claude", "claude " + normalQ, overrideAdded},
		{"quoted prompt", "claude 'p'", "claude " + normalQ + " 'p'", overrideAdded},
		{"unquoted prompt", "claude p", "claude " + normalQ + " p", overrideAdded},
		{"after cd", "cd /x && claude --model opus 'p'", "cd /x && claude " + normalQ + " --model opus 'p'", overrideAdded},
		{"assignment exec", "FOO=1 exec claude", "FOO=1 exec claude " + normalQ, overrideAdded},
		{"absolute claude", "/opt/homebrew/bin/claude", "/opt/homebrew/bin/claude " + normalQ, overrideAdded},
		{"glued redirection", "claude>log", "claude " + normalQ + ">log", overrideAdded},
		{"subshell", "(claude)", "(claude " + normalQ + ")", overrideAdded},
		{"expansion prompt", "claude $(cat p.txt)", "claude " + normalQ + " $(cat p.txt)", overrideAdded},
		{"after --", `claude -- --settings '{"x":1}'`, "claude " + normalQ + ` -- --settings '{"x":1}'`, overrideAdded},

		// merged, inline
		{"two-token inline", `claude --settings '{"a":1,"editorMode":"vim"}' 'p'`,
			`claude --settings='{"a":1,"editorMode":"normal"}' 'p'`, overrideMerged},
		{"one-token inline", `claude --settings='{"a":1}'`,
			`claude --settings='{"a":1,"editorMode":"normal"}'`, overrideMerged},
		{"double-quoted inline", `claude --settings "{\"a\":1}" 'p'`,
			`claude --settings='{"a":1,"editorMode":"normal"}' 'p'`, overrideMerged},
		{"space in a string", `claude --settings '{"m":"a b"}'`,
			`claude --settings='{"editorMode":"normal","m":"a` + `\` + `u0020b"}'`, overrideMerged},
		{"quote in a string", `claude --settings '{"m":"it'\''s"}'`,
			`claude --settings='{"editorMode":"normal","m":"it` + `\` + `u0027s"}'`, overrideMerged},
		{"whitespace in json", `claude --settings '{ "a" : [1, 2] }'`,
			`claude --settings='{"a":[1,2],"editorMode":"normal"}'`, overrideMerged},
		{"redirection before", `claude >run.log --settings '{"editorMode":"vim"}' 'p'`,
			`claude >run.log --settings='{"editorMode":"normal"}' 'p'`, overrideMerged},
		{"redirection after", `claude 2>&1 --settings='{}'`,
			`claude 2>&1 --settings='{"editorMode":"normal"}'`, overrideMerged},
		{"the last one is merged", `claude --settings='{"a":1}' --settings '{"b":2}' p`,
			`claude --settings='{"a":1}' --settings='{"b":2,"editorMode":"normal"}' p`, overrideMerged},
		{"tab between", "claude --settings\t'{}'", `claude --settings='{"editorMode":"normal"}'`, overrideMerged},

		// merged_file (want is checked by property, not literally)
		{"absolute file", "claude --settings {DIR}/s.json 'p'", "", overrideMergedFile},
		{"absolute file, = form", "claude --settings='{DIR}/s.json'", "", overrideMergedFile},

		// unparsed: byte-identical
		{"relative after cd", "cd {DIR} && claude --settings rel.json", "", overrideUnparsed},
		{"$HOME file", "claude --settings $HOME/s.json", "", overrideUnparsed},
		{"quoted $HOME file", `claude --settings "$HOME/s.json"`, "", overrideUnparsed},
		{"single-quoted $ file", `claude --settings '$HOME/s.json'`, "", overrideUnparsed},
		{"= expansion", "claude --settings=$S", "", overrideUnparsed},
		{"expansion spelling the flag", `claude "--settings=$S"`, "", overrideUnparsed},
		{"missing file", "claude --settings /nonexistent/magmux/s.json", "", overrideUnparsed},
		{"array file", "claude --settings {DIR}/arr.json", "", overrideUnparsed},
		{"invalid file", "claude --settings {DIR}/bad.json", "", overrideUnparsed},
		{"invalid inline", `claude --settings '{"a":'`, "", overrideUnparsed},
		{"inline null-ish", `claude --settings '{} x'`, "", overrideUnparsed},
		{"dash value", "claude --settings -", "", overrideUnparsed},
		{"flag as value", "claude --settings --model m", "", overrideUnparsed},
		{"no value", "claude --settings", "", overrideUnparsed},
		{"empty value", "claude --settings ''", "", overrideUnparsed},
		{"empty = value", "claude --settings=", "", overrideUnparsed},
		{"operator after flag", "claude --settings; echo", "", overrideUnparsed},
		// [G]: shell syntax between --settings and its value
		{"G redirection between", `claude --settings >run.log '{"editorMode":"vim","x":1}' 'prompt'`, "", overrideUnparsed},
		{"G 2>&1 between", `claude --settings 2>&1 '{"a":1}'`, "", overrideUnparsed},
		{"G glued redirection", `claude --settings>log '{}'`, "", overrideUnparsed},
		{"G assignment value", "claude --settings X=1 'p'", "", overrideUnparsed},
		{"G continuation", "claude --settings \\\n'{\"a\":1}'", "", overrideUnparsed},
		{"G comment", "claude --settings # c\n'{}'", "", overrideUnparsed},

		// not direct: byte-identical, nothing recorded
		{"heredoc", "claude <<EOF\nhi\nEOF", "", overrideNone},
		{"bash -c", "bash -c 'claude'", "", overrideNone},
		{"two claudes", "claude; claude", "", overrideNone},
		{"echo", "echo claude ", "", overrideNone},
		{"comment", "# claude", "", overrideNone},
		{"print mode", "claude -p 'x'", "", overrideNone},
		{"wrapper", "claudish -i --model m -y", "", overrideNone},
	}
}

// TestRewriteClaudeLaunch is the table, plus every property on every row.
func TestRewriteClaudeLaunch(t *testing.T) {
	dir, sub := rewriteFixtures(t)
	private := settingsDir(dir)
	for _, r := range rewriteTable() {
		t.Run(r.name, func(t *testing.T) {
			line := sub(r.line)
			cfg := shellCfg("/bin/zsh", line)
			launch := claudeLaunchOf(cfg)
			basis, _ := gateDecision(cfg.Controller, launch)
			rw := rewriteClaudeLaunch(cfg, launch, basis, private)
			if rw.file != "" {
				t.Cleanup(func() { os.Remove(rw.file) })
			}
			if rw.override != r.override {
				t.Errorf("override = %q, want %q (why %q)", rw.override, r.override, rw.why)
			}
			got := rw.args[len(rw.args)-1]
			switch {
			case r.override == overrideMergedFile:
				if rw.file == "" || !strings.Contains(got, "--settings='"+rw.file+"'") {
					t.Errorf("got %q, want the flag to name the private copy %q", got, rw.file)
				}
			case r.want == "":
				if got != line {
					t.Errorf("got %q, want the input byte-identical", got)
				}
			default:
				if want := sub(r.want); got != want {
					t.Errorf("got  %q\nwant %q", got, want)
				}
			}
			checkRewriteProperties(t, cfg, rw)
		})
	}
}

// checkRewriteProperties is the Phase 6 property, on one rewrite:
//
//  1. the output is the input, OR re-lexing it finds a direct invocation whose
//     effective (last) --settings is the one-token `--settings=` form, with
//     editorMode "normal" and every key of the original effective settings;
//  2. extractClaudePrompt over the pane (launchLine = the original) equals
//     today's answer from the original argv;
//  3. every skipped unit of the input (each redirection with its target)
//     appears byte-identical, in order, in the output;
//  4. the bytes outside the one replaced span are untouched.
func checkRewriteProperties(t *testing.T, cfg PaneConfig, rw launchRewrite) {
	t.Helper()
	in := cfg.Args[len(cfg.Args)-1]
	out := rw.args[len(rw.args)-1]
	if !slices.Equal(rw.args[:len(rw.args)-1], cfg.Args[:len(cfg.Args)-1]) {
		t.Fatalf("argv outside the -c string changed: %q -> %q", cfg.Args, rw.args)
	}

	// (2) discovery reads the original.
	shell := cfg.Cmd
	today := extractClaudePrompt(&Pane{cmd: exec.Command(shell, cfg.Args...)})
	built := extractClaudePrompt(&Pane{cmd: exec.Command(shell, rw.args...), launchLine: in})
	if today != built {
		t.Errorf("extractClaudePrompt moved: %q -> %q", today, built)
	}

	if out == in {
		if rw.override != overrideNone && rw.override != overrideUnparsed {
			t.Errorf("byte-identical output recorded as %q", rw.override)
		}
		if rw.file != "" {
			t.Errorf("an unrewritten launch owns a settings copy %q", rw.file)
		}
		return
	}
	switch rw.override {
	case overrideAdded, overrideMerged, overrideMergedFile:
	default:
		t.Fatalf("output changed but override is %q", rw.override)
	}

	// (1) the effective --settings of the output.
	before := claudeLaunchOf(cfg)
	after := claudeLaunchOf(shellCfg(cfg.Cmd, out))
	if !after.direct {
		t.Fatalf("the rewrite is no longer a direct invocation: %q", out)
	}
	inUses, why := shellSettings(before)
	if why != "" {
		t.Fatalf("a rewritten input had an unparsable --settings: %s", why)
	}
	outUses, why := shellSettings(after)
	if why != "" || len(outUses) == 0 {
		t.Fatalf("re-lexing %q: uses=%d why=%q", out, len(outUses), why)
	}
	if want := max(len(inUses), 1); len(outUses) != want {
		t.Errorf("output has %d --settings, want %d", len(outUses), want)
	}
	last := outUses[len(outUses)-1]
	if raw := out[last.start:last.end]; !strings.HasPrefix(raw, "--settings=") {
		t.Errorf("effective flag %q is not the one-token = form", raw)
	}
	got := settingsObject(t, last.value)
	if string(got["editorMode"]) != `"normal"` {
		t.Errorf("effective editorMode = %s, want \"normal\"", got["editorMode"])
	}
	orig := map[string]json.RawMessage{}
	if len(inUses) > 0 {
		u := inUses[len(inUses)-1]
		v := u.value
		if !isInlineSettings(v) {
			path, why := settingsPath(u, true, true, cfg.Dir, cfg.Env)
			if why != "" {
				t.Fatalf("a rewritten input's settings path: %s", why)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			v = string(b)
		}
		orig = settingsObject(t, v)
	}
	for k, v := range orig {
		if k == "editorMode" {
			continue
		}
		if compactJSON(t, got[k]) != compactJSON(t, v) {
			t.Errorf("key %q: %s -> %s", k, v, got[k])
		}
	}
	if rw.override == overrideMergedFile {
		fi, err := os.Stat(rw.file)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("private copy %q: %v mode %v, want 0600", rw.file, err, fi)
		}
		if strings.Contains(out, "SECRET") {
			t.Errorf("a settings file's contents reached the command line: %q", out)
		}
	}

	// (3) skipped units, byte-identical and in order.
	if a, b := redirs(before), redirs(after); !slices.Equal(a, b) {
		t.Errorf("skipped units changed: %q -> %q", a, b)
	}

	// (4) one span: an insertion right after the claude word, or the input's
	// effective flag span replaced by the output's.
	if len(inUses) == 0 {
		c := before.toks[before.claude]
		if want := in[:c.end] + " " + normalQ + in[c.end:]; out != want {
			t.Errorf("insertion: got %q, want %q", out, want)
		}
	} else {
		u := inUses[len(inUses)-1]
		if out[:u.start] != in[:u.start] || out[last.end:] != in[u.end:] {
			t.Errorf("bytes outside the flag span changed: %q -> %q", in, out)
		}
	}
}

func redirs(l launchInfo) []string {
	var out []string
	for _, t := range l.toks {
		if t.kind == tokRedir {
			out = append(out, t.raw(l.line))
		}
	}
	return out
}

// settingsObject parses an effective --settings value: inline JSON, or a path
// to a file holding it.
func settingsObject(t *testing.T, v string) map[string]json.RawMessage {
	t.Helper()
	if !isInlineSettings(v) {
		b, err := os.ReadFile(v)
		if err != nil {
			t.Fatalf("read %q: %v", v, err)
		}
		v = string(b)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		t.Fatalf("parse %q: %v", v, err)
	}
	return m
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "<invalid " + string(raw) + ">"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestRewriteRelativeAndTildeFiles: a relative path resolves against cfg.Dir
// when nothing precedes the invocation, and an UNQUOTED ~/ against $HOME; a
// quoted '~/…' is a literal relative path, which is not a file here.
func TestRewriteRelativeAndTildeFiles(t *testing.T) {
	dir, _ := rewriteFixtures(t)
	t.Setenv("HOME", dir)
	private := settingsDir(dir)
	for _, r := range []struct {
		line string
		want editorOverride
	}{
		{"claude --settings rel.json", overrideMergedFile},
		{"FOO=1 claude --settings=rel.json p", overrideMergedFile},
		{"claude --settings ~/s.json", overrideMergedFile},
		{"claude --settings '~/s.json'", overrideUnparsed},
		{"echo hi; claude --settings rel.json", overrideUnparsed},
	} {
		cfg := shellCfg("/bin/sh", r.line)
		cfg.Dir = dir
		rw := rewriteClaudeLaunch(cfg, claudeLaunchOf(cfg), gateDirect, private)
		if rw.file != "" {
			defer os.Remove(rw.file)
		}
		if rw.override != r.want {
			t.Errorf("%q: override %q, want %q (why %q)", r.line, rw.override, r.want, rw.why)
		}
		checkRewriteProperties(t, cfg, rw)
	}
}

// TestRewriteArgvLaunch: an argv launch gets the same merge without quoting.
func TestRewriteArgvLaunch(t *testing.T) {
	dir, _ := rewriteFixtures(t)
	private := settingsDir(dir)
	rows := []struct {
		args, want []string
		override   editorOverride
	}{
		{[]string{"--model", "m", "p"}, []string{`--settings={"editorMode":"normal"}`, "--model", "m", "p"}, overrideAdded},
		{[]string{"--settings", `{"a":1}`, "p"}, []string{`--settings={"a":1,"editorMode":"normal"}`, "p"}, overrideMerged},
		{[]string{"--settings"}, []string{"--settings"}, overrideUnparsed},
		{[]string{"--", "--settings", "x"}, []string{`--settings={"editorMode":"normal"}`, "--", "--settings", "x"}, overrideAdded},
	}
	for _, r := range rows {
		cfg := PaneConfig{Cmd: "/usr/local/bin/claude", Args: r.args}
		rw := rewriteClaudeLaunch(cfg, claudeLaunchOf(cfg), gateDirect, private)
		if rw.override != r.override || !slices.Equal(rw.args, r.want) {
			t.Errorf("%q: got %q %q, want %q %q", r.args, rw.override, rw.args, r.override, r.want)
		}
	}
	cfg := PaneConfig{Cmd: "claude", Args: []string{"--settings=" + filepath.Join(dir, "s.json")}}
	rw := rewriteClaudeLaunch(cfg, claudeLaunchOf(cfg), gateDirect, private)
	defer os.Remove(rw.file)
	if rw.override != overrideMergedFile || len(rw.args) != 1 || rw.args[0] != "--settings="+rw.file {
		t.Errorf("argv file: %q %q", rw.override, rw.args)
	}
}

// TestRewriteOnlyGatedDirect: an ungated pane (plugin-owned, or not direct) is
// never rewritten, and a declared DIRECT pane is.
func TestRewriteOnlyGatedDirect(t *testing.T) {
	cfg := shellCfg("/bin/sh", "claude 'p'")
	launch := claudeLaunchOf(cfg)
	if rw := rewriteClaudeLaunch(cfg, launch, gateNone, t.TempDir()); rw.override != overrideNone || !slices.Equal(rw.args, cfg.Args) {
		t.Errorf("an ungated direct pane was rewritten: %q %q", rw.override, rw.args)
	}
	if rw := rewriteClaudeLaunch(cfg, launch, gateDeclared, t.TempDir()); rw.override != overrideAdded {
		t.Errorf("a declared direct pane: override %q, want added", rw.override)
	}
}

// TestSettingsDirRefusesForeignMode: a private directory that is 0755 (made by
// something else, or a umask nobody checked) is never written into; the file
// case falls back to unparsed, byte-identical.
func TestSettingsDirRefusesForeignMode(t *testing.T) {
	dir, sub := rewriteFixtures(t)
	private := settingsDir(dir)
	if err := os.Mkdir(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := shellCfg("/bin/sh", sub("claude --settings {DIR}/s.json"))
	rw := rewriteClaudeLaunch(cfg, claudeLaunchOf(cfg), gateDirect, private)
	if rw.override != overrideUnparsed || !slices.Equal(rw.args, cfg.Args) || rw.file != "" {
		t.Errorf("0755 dir: override %q args %q file %q, want unparsed and byte-identical", rw.override, rw.args, rw.file)
	}
	if es, _ := os.ReadDir(private); len(es) != 0 {
		t.Errorf("wrote into a foreign-mode directory: %v", es)
	}
	if fi, _ := os.Stat(private); fi.Mode().Perm() != 0o755 {
		t.Errorf("the refused directory was chmod'ed to %#o", fi.Mode().Perm())
	}
}

// FuzzRewriteClaudeLaunch: no input panics, and every input satisfies the
// properties.
func FuzzRewriteClaudeLaunch(f *testing.F) {
	for _, r := range rewriteTable() {
		f.Add(strings.ReplaceAll(r.line, "{DIR}", "/tmp"))
	}
	f.Add(`claude --settings '{"a":{"b":[1," x "]}}' --model m 'p q' 2>&1 | tee log`)
	f.Add(`env A=1 nohup claude >a 2>b --settings="{\"k\":\"'\"}" -- x`)
	f.Fuzz(func(t *testing.T, line string) {
		dir := sockTestDir(t)
		cfg := shellCfg("/bin/sh", line)
		cfg.Dir = dir
		launch := claudeLaunchOf(cfg)
		rw := rewriteClaudeLaunch(cfg, launch, gateDirect, settingsDir(dir))
		if rw.file != "" {
			defer os.Remove(rw.file)
		}
		checkRewriteProperties(t, cfg, rw)
	})
}

// TestDeclaredWrapperIsDelegated: a declared pane whose command is not a direct
// invocation is the wrapper's to override. It runs byte-identical, owns no
// settings copy, and says `delegated`.
func TestDeclaredWrapperIsDelegated(t *testing.T) {
	fake := fakeToolShell(t)
	settings := filepath.Join(fake, "w.json")
	if err := os.WriteFile(settings, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	line := "claudish -i --model m -y --settings " + settings
	cfg := PaneConfig{Cmd: os.Getenv("SHELL"), Args: []string{"-c", line}, Controller: claudeCodeName, SockDir: fake}
	p, err := newPane(0, 0, 24, 80, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Magmux{}
	defer func() { m.reapPane(p, true); m.reapChild(p) }()

	if p.gateBasis != gateDeclared || p.editorOverride != overrideDelegated {
		t.Errorf("gateBasis %q editorOverride %q, want declared/delegated", p.gateBasis, p.editorOverride)
	}
	if p.settingsFile != "" {
		t.Errorf("a delegated pane owns a settings copy %q", p.settingsFile)
	}
	if got := p.cmd.Args[len(p.cmd.Args)-1]; got != line {
		t.Errorf("exec'd %q, want the declared wrapper byte-identical", got)
	}
	want := []string{"-i", "--model", "m", "-y", "--settings", settings}
	if got := readArgv(t, fake, "claudish"); !slices.Equal(got, want) {
		t.Errorf("the wrapper ran with %q, want %q", got, want)
	}
}

// TestDirectPaneRunsRewritten: what the child actually receives, end to end
// through newPane — and launchLine keeps the original for discovery.
func TestDirectPaneRunsRewritten(t *testing.T) {
	fake := fakeToolShell(t)
	line := `claude --settings '{"a":1,"editorMode":"vim"}' 'say hi'`
	cfg := PaneConfig{Cmd: os.Getenv("SHELL"), Args: []string{"-c", line}, SockDir: fake}
	p, err := newPane(0, 0, 24, 80, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Magmux{}
	defer func() { m.reapPane(p, true); m.reapChild(p) }()
	if p.gateBasis != gateDirect || p.editorOverride != overrideMerged {
		t.Errorf("gateBasis %q editorOverride %q, want direct/merged", p.gateBasis, p.editorOverride)
	}
	if p.launchLine != line {
		t.Errorf("launchLine %q, want the original %q", p.launchLine, line)
	}
	want := []string{`--settings={"a":1,"editorMode":"normal"}`, "say hi"}
	if got := readArgv(t, fake, "claude"); !slices.Equal(got, want) {
		t.Errorf("claude ran with %q, want %q", got, want)
	}
	// Discovery's answer is today's, read off the ORIGINAL line — including
	// today's two-token-flag quirk, which returns the flag's value (§10).
	today := extractClaudePrompt(&Pane{cmd: exec.Command(cfg.Cmd, cfg.Args...)})
	if got := extractClaudePrompt(p); got != today {
		t.Errorf("extractClaudePrompt = %q, want today's %q", got, today)
	}
}

// TestSettingsTempFileRemovedOnReap: a settings FILE becomes a private 0600
// copy in the pane's settings directory — merged, its secrets off the command
// line — and the copy is gone once the child has been reaped.
func TestSettingsTempFileRemovedOnReap(t *testing.T) {
	fake := fakeToolShell(t)
	src := filepath.Join(fake, "user.json")
	if err := os.WriteFile(src, []byte(`{"editorMode":"vim","env":{"API_KEY":"s3cret"},"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := PaneConfig{Cmd: os.Getenv("SHELL"), Args: []string{"-c", "claude --settings " + src + " 'p'"}, SockDir: fake}
	p, err := newPane(0, 0, 24, 80, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Magmux{}
	reaped := false
	defer func() {
		if !reaped {
			m.reapPane(p, true)
			m.reapChild(p)
		}
	}()

	if p.editorOverride != overrideMergedFile || p.settingsFile == "" {
		t.Fatalf("editorOverride %q settingsFile %q, want merged_file and a copy", p.editorOverride, p.settingsFile)
	}
	if filepath.Dir(p.settingsFile) != settingsDir(fake) {
		t.Errorf("copy in %q, want the pane's settings dir %q", filepath.Dir(p.settingsFile), settingsDir(fake))
	}
	fi, err := os.Stat(p.settingsFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("copy %v mode %v, want 0600", err, fi)
	}
	got := settingsObject(t, p.settingsFile)
	if string(got["editorMode"]) != `"normal"` || string(got["model"]) != `"opus"` || compactJSON(t, got["env"]) != `{"API_KEY":"s3cret"}` {
		t.Errorf("copy holds %v; want editorMode normal and every original key", got)
	}
	if args := strings.Join(p.cmd.Args, " "); strings.Contains(args, "s3cret") || strings.Contains(args, "API_KEY") {
		t.Errorf("the settings file's secret reached the command line: %q", args)
	}
	if argv := readArgv(t, fake, "claude"); len(argv) != 2 || argv[0] != "--settings="+p.settingsFile || argv[1] != "p" {
		t.Errorf("claude ran with %q", argv)
	}

	copyPath := p.settingsFile
	m.reapPane(p, true)
	done := make(chan struct{})
	go func() { m.reapChild(p); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reapChild never returned")
	}
	reaped = true
	if _, err := os.Stat(copyPath); !os.IsNotExist(err) {
		t.Errorf("the copy survived the reap: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the caller's own file was touched: %v", err)
	}
}

// TestSettingsCopyRemovedOnSpawnFailure: a copy made for a launch that never
// starts is removed on the way out of spawnPTY.
func TestSettingsCopyRemovedOnSpawnFailure(t *testing.T) {
	dir, sub := rewriteFixtures(t)
	cfg := PaneConfig{Cmd: filepath.Join(dir, "no-such-shell", "sh"), Args: []string{"-c", sub("claude --settings {DIR}/s.json")}, SockDir: dir}
	if _, err := newPane(0, 0, 24, 80, cfg); err == nil {
		t.Fatal("a missing shell spawned")
	}
	if es, _ := os.ReadDir(settingsDir(dir)); len(es) != 0 {
		t.Errorf("spawn failure left %d settings copies", len(es))
	}
}

// TestPluginPaneNotRewritten: a plugin's pane is never gated, so it is never
// rewritten either — over open_pane on the real socket, its claude receives
// the caller's --settings exactly, and the pane records no override.
func TestPluginPaneNotRewritten(t *testing.T) {
	fake := fakeToolShell(t)
	m := startPluginMagmux(t, "-e", "sleep 60")
	p := dialPlugin(t, m.sock)
	replyOK(t, p.register("ticket", m.token, nil))

	id := int(replyOK(t, p.request(map[string]any{
		"type": "open_pane", "cmd": `claude --settings '{"a":1}' 'p'`, "controller": "self",
	}))["pane"].(float64))

	pe := listPanesByID(t, p)[id]
	for _, k := range []string{"gateBasis", "editorOverride"} {
		if v, ok := pe[k]; ok {
			t.Errorf("the plugin's pane carries %s=%v", k, v)
		}
	}
	if cmd, _ := pe["cmd"].(string); !strings.HasSuffix(cmd, `claude --settings '{"a":1}' 'p'`) {
		t.Errorf("cmd %q, want the open_pane command unrewritten", cmd)
	}
	if got, want := readArgv(t, fake, "claude"), []string{"--settings", `{"a":1}`, "p"}; !slices.Equal(got, want) {
		t.Errorf("the plugin pane's claude ran with %q, want %q", got, want)
	}
}
