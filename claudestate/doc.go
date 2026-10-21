// Package claudestate is one library for observing Claude Code sessions.
//
// It owns two things and nothing else. The CONTRACT: decoding everything
// Claude Code writes under its root (the session registry, the background job
// records, the transcripts), including the vendor's own rule for merging a
// background session's two records. And the RUNTIME: the watcher, one actor
// per session, and the hub. Panes, surfaces, the wire format, actions, policy,
// the audit trail and notification delivery belong to the HOST.
//
// The module is hexagonal. The library declares the ports (package ports) and
// each host implements them; a pane handle is an opaque [model.PaneRef], so no
// port speaks tmux or PTY vocabulary.
//
// # The Claude root is READ-ONLY, by construction
//
// Nothing in this module creates, writes, renames or removes anything. That is
// a guard test (internal/audit, G2), not a convention.
//
// # Import direction
//
// Enforced by internal/audit's G13 with `go list` over non-test packages:
//
//	model                 <- stdlib only
//	ports                 <- model
//	stage                 <- model
//	claude                <- model
//	claude/doctor         <- claude, model, ports
//	join                  <- claude, model
//	watch                 <- model, ports, fsnotify   (the ONLY importer of fsnotify)
//	internal/source       <- claude, model, ports
//	internal/termev       <- model, ports
//	verdict               <- internal/termev, stage, model, ports
//	internal/claudecode   <- claude, internal/source, stage, model, ports
//	pipeline              <- model, ports, stage, claude, join, watch, internal/source, verdict, internal/claudecode
//	proclive              <- model, ports
//	lint                  <- claude
//	internal/difftest     <- model, join, verdict, claude
//	_diff                 <- anything in the module
//	_probe                <- stdlib ONLY; nothing in the module imports it
//
// No library package imports pipeline. Nothing imports a magmux package outside
// this module, which the dependency pin (G10) makes structurally impossible:
// this module does not require the root.
//
// # Concurrency
//
// Channels only. There is no mutex, no atomic and no ticker anywhere in the
// module, test files included (G3, G5), and every timer is one-shot and comes
// from [ports.Clock] (G4).
package claudestate
