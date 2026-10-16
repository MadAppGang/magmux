package mux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/sockdir"
)

// ── the input gate: which panes, and the one predicate ────────────────────────
//
// A pane's input is gated when two things hold: its controller implements
// InputGate, and its gateBasis says the pane was declared a Claude pane or was
// launched as a direct `claude` invocation. Both halves are needed. A loose
// substring match (`grep claude x`, a wrapper nobody declared) attaches a
// controller for OBSERVATION and is never gated, because magmux cannot know
// what such a pane runs.
//
// inputGateOf is THE predicate. Every gate decision, the list/results `input`
// key and Snapshot.Input.Gated go through it, so "gated" cannot mean one thing
// for a send and another for the published view.

// gateBasis is why a pane's input is gated; "" means never gated. Write-once
// before publication and read lock-free, like id and label.
type gateBasis string

const (
	gateNone     gateBasis = ""         // not gated: today's send
	gateDeclared gateBasis = "declared" // --controller claude-code / open_pane controller:"claude-code"
	gateDirect   gateBasis = "direct"   // the lexer found exactly one command-position claude word
)

// editorOverride is what magmux did about Claude's editorMode for this pane;
// "" means nothing was attempted. Write-once before publication.
type editorOverride string

const (
	overrideNone       editorOverride = ""
	overrideAdded      editorOverride = "added"       // no --settings: one was inserted
	overrideMerged     editorOverride = "merged"      // merged into the caller's inline --settings
	overrideMergedFile editorOverride = "merged_file" // the caller's settings file, merged into a private copy
	overrideUnparsed   editorOverride = "unparsed"    // a rewrite was unsafe; the command is byte-identical
	overrideDelegated  editorOverride = "delegated"   // a declared wrapper owns the override
)

// claudeCodeName is the Claude controller's name: what Name() reports, what
// `--controller` and open_pane's `controller` accept, and the resolved value
// PaneConfig.Controller carries for a declared pane.
const claudeCodeName = "claude-code"

// knownControllers is the --controller error's "known:" list.
const knownControllers = claudeCodeName

// gateDecision is newPane's pre-spawn verdict from the RESOLVED declaration
// and what the lexer found in the launch, in the attach order's priority:
//
//  1. a plugin owns the pane: never gated, never rewritten;
//  2. declared claude-code: gated whatever the command is. A command that is
//     not a direct invocation is a wrapper, and the wrapper owns the editor
//     override (`delegated`); R2's footer check catches one that did not;
//  3. a direct invocation: gated;
//  4. anything else: not gated. The loose factory may still observe it.
//
// The editor override of a direct invocation (`added`, `merged`, …) is the
// rewrite's to decide, not this function's.
func gateDecision(controller string, launch launchInfo) (gateBasis, editorOverride) {
	switch {
	case strings.HasPrefix(controller, pluginControllerPrefix):
		return gateNone, overrideNone
	case controller == claudeCodeName:
		if launch.direct {
			return gateDeclared, overrideNone
		}
		return gateDeclared, overrideDelegated
	case launch.direct:
		return gateDirect, overrideNone
	}
	return gateNone, overrideNone
}

// settingsDir is the private per-uid directory a pane's merged settings copy
// lives in: <sockdir>/magmux-settings-<euid>. It is computed from the PANE's
// sockdir (PaneConfig.SockDir), so a Magmux with its own sockDir writes and
// sweeps the same directory; empty falls back to the package default.
func settingsDir(sockDir string) string {
	if sockDir == "" {
		sockDir = sockdir.Dir
	}
	return filepath.Join(sockDir, fmt.Sprintf("magmux-settings-%d", os.Geteuid()))
}

// inputGateOf returns the pane's InputGate when the pane is gated, else nil.
func inputGateOf(p *Pane) InputGate {
	if p == nil || p.gateBasis == gateNone {
		return nil
	}
	g, _ := p.controller.(InputGate)
	return g
}

// boxVerdictReason maps a CheckBox verdict that withholds the Enter onto its
// not_accepted reason. An empty box is text_mismatch when the send typed
// something (its text was lost) and empty_box when it typed nothing (an
// Enter-only submit on an empty box). "" for BoxVerified.
func boxVerdictReason(v BoxVerdict, want BoxExpect) string {
	switch v {
	case BoxMismatch:
		return protocol.ReasonTextMismatch
	case BoxEmpty:
		if want.TypedText {
			return protocol.ReasonTextMismatch
		}
		return protocol.ReasonEmptyBox
	}
	return ""
}
