package mux

import "github.com/MadAppGang/magmux/protocol"

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
