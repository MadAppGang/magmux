package mux

// Bash mode and the generic select dialog (review M3, M5a).
//
// Bash mode is pinned on s7-bashmode, a real Claude Code 2.1.301 capture: a
// `!` typed as the first character of an empty box is CONSUMED — it is not
// box text — and the box's glyph becomes `!` (followed by the NBSP the `❯`
// box uses), the footer reads `! for shell mode`, and the submitted command
// is a history entry `! echo BASHMODE-OK` (ordinary space) with its output
// under ⎿.

import (
	"strings"
	"testing"
	"time"
)

// sentShell is what s7's capture typed: `!`, then the command.
const sentShell = "!echo BASHMODE-OK"

func TestBashModeBoxParses(t *testing.T) {
	for _, c := range []struct {
		label   string
		boxText string
	}{
		{"bash-mode-empty", ""}, // `! Try "…"`: the placeholder is faint
		{"bash-mode-typed", "echo BASHMODE-OK"},
	} {
		sc := claudeScreenAt(t, "s7-bashmode", c.label)
		if !sc.BoxVisible || !sc.BoxShell || sc.BoxText != c.boxText {
			t.Errorf("%s: BoxVisible=%v BoxShell=%v BoxText=%q, want a shell box holding %q",
				c.label, sc.BoxVisible, sc.BoxShell, sc.BoxText, c.boxText)
		}
	}
	sc := claudeScreenAt(t, "s7-bashmode", "bash-mode-submitted")
	if !sc.BoxVisible || sc.BoxShell || sc.BoxText != "" {
		t.Errorf("submitted: BoxVisible=%v BoxShell=%v BoxText=%q, want the ordinary empty box back",
			sc.BoxVisible, sc.BoxShell, sc.BoxText)
	}
	if len(sc.Prompts) != 1 || !sc.Prompts[0].Shell || sc.Prompts[0].Text != "echo BASHMODE-OK" {
		t.Errorf("submitted: prompts %+v, want one shell entry \"echo BASHMODE-OK\"", sc.Prompts)
	}
}

// TestBashModeSendVerifiedAndAccepted: the whole gated path for `!cmd` on the
// real screens — the box check treats the leading `!` as the mode switch and
// compares the rest, and the `! cmd` history entry is the evidence.
func TestBashModeSendVerifiedAndAccepted(t *testing.T) {
	p := claudePaneAt(t, "s7-bashmode", "bash-mode-typed")
	g, _ := gatedClaudeOn(p)
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, TypedText: true, Exact: sentShell})
	if !chk.Readiness.Ready || chk.Verdict != BoxVerified || chk.Verified != verifiedExact {
		t.Fatalf("CheckBox = %+v, want ready and verified exact", chk)
	}
	if v := g.InputAccepted(chk.Baseline); v.State != AcceptInBox {
		t.Errorf("before the Enter: %v, want in_box", v.State)
	}
	swapScreen(t, p, "s7-bashmode", "bash-mode-submitted")
	if v := g.InputAccepted(chk.Baseline); v.State != AcceptTaken || v.Evidence != evidenceHistory {
		t.Errorf("after the Enter: %v/%q, want taken by history", v.State, v.Evidence)
	}
}

// TestBashModeMismatchStillRefused: the `!` rule strips the mode switch and
// nothing else, so a different command in the shell box is still a mismatch,
// and a send WITHOUT a leading `!` never verifies against a shell box.
func TestBashModeMismatchStillRefused(t *testing.T) {
	p := claudePaneAt(t, "s7-bashmode", "bash-mode-typed")
	g, _ := gatedClaudeOn(p)
	for _, exact := range []string{"!echo SOMETHING-ELSE", "echo BASHMODE-OK", "!!echo BASHMODE-OK"} {
		if chk := g.CheckBox(BoxExpect{Class: ClassSubmit, TypedText: true, Exact: exact}); chk.Verdict != BoxMismatch {
			t.Errorf("Exact %q: verdict %v, want mismatch", exact, chk.Verdict)
		}
	}
}

// TestShellEntryMatchesOnlyAShellSend: a `! cmd` entry is evidence for a bash
// mode send and for nothing else, and a `❯ cmd` entry is never evidence for a
// bash-mode send — one command text, two different acts.
func TestShellEntryMatchesOnlyAShellSend(t *testing.T) {
	now := time.Now()
	plainBox := claudeScreen{BoxVisible: true, BoxText: "echo BASHMODE-OK", BoxKey: inputKey("echo BASHMODE-OK")}
	shellBox := plainBox
	shellBox.BoxShell = true
	plain := newAcceptBaseline(plainBox, plainBox.BoxText, plainBox.BoxText, 0, now, 0)
	shell := newAcceptBaseline(shellBox, shellBox.BoxText, sentShell, 0, now, 0)
	text := "echo BASHMODE-OK"
	shellEntry := promptEntry{Text: text, Key: inputKey(text), RenderedKey: renderedKey(text), Shell: true}
	plainEntry := promptEntry{Text: text, Key: inputKey(text), RenderedKey: renderedKey(text)}
	for _, c := range []struct {
		name string
		e    promptEntry
		b    AcceptBaseline
		want bool
	}{
		{"shell entry, shell send", shellEntry, shell, true},
		{"plain entry, plain send", plainEntry, plain, true},
		{"shell entry, plain send", shellEntry, plain, false},
		{"plain entry, shell send", plainEntry, shell, false},
	} {
		if got := matches(c.e, c.b); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSelectDialogIsNotDismissable (review M5a): `select` is the catch-all
// for a region with the generic `Enter to confirm · Esc to cancel` footer —
// which is exactly what a boot dialog carries. A reworded trust prompt falls
// through to it, and Escape on the trust prompt EXITS Claude. So only the
// named, measured dialogs are dismissable; `select` is reported and waited
// out, never Escaped.
func TestSelectDialogIsNotDismissable(t *testing.T) {
	rows, cols := claudishScreen(t, "dialog-trust")
	for i, r := range rows {
		if strings.Contains(r.Text, "Is this a project you created or one you trust") {
			rows[i].Text = strings.Replace(r.Text, "Is this a project you created or one you trust", "Do you trust the files in this folder", 1)
		}
	}
	sc := parseClaudeScreen(rows, cols)
	if sc.Dialog != "select" {
		t.Fatalf("a reworded trust prompt parsed as %q, want the select catch-all", sc.Dialog)
	}
	if sc.Dismissable {
		t.Error("the select catch-all is dismissable; Escape on an unrecognised boot dialog can exit Claude")
	}
}
