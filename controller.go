package main

import (
	"context"
	"time"
)

// ToolController is a side-channel observer + interactor for a specific
// interactive tool running inside a pane. magmux holds one controller per
// pane that has one attached. Polling-based, no callbacks.
type ToolController interface {
	// Name identifies the controller implementation.
	// E.g. "claude-code", "codex", "opencode".
	Name() string

	// Start begins any long-running work (file watchers, background
	// goroutines). Called once when the controller is attached. Must be
	// idempotent — magmux may call it multiple times.
	Start(ctx context.Context) error

	// Poll returns the current snapshot of the controlled tool's state.
	// Called from magmux's render loop, throttled to ~4Hz.
	// Implementations should be cheap and non-blocking — read a file,
	// parse a small chunk, return. Heavy work goes in Start.
	Poll() (Snapshot, error)

	// Stop tears down resources. Called when the pane closes or the
	// controller is detached.
	Stop() error
}

// Snapshot is the uniform status surface every controller produces.
// All fields are optional except State. Future fields are additive.
type Snapshot struct {
	// State is the high-level lifecycle stage. Required.
	State ControllerState

	// Project is a short label for the work being done, if known.
	// E.g. "magmux", "user-service".
	Project string

	// Model is the model name in use, if applicable. E.g. "claude-opus-4-6".
	Model string

	// LastUserPrompt is the most recent user input the tool received.
	// Empty if unknown.
	LastUserPrompt string

	// LastResponse is the most recent assistant response text the tool
	// produced. For tools that emit structured content (tool calls vs
	// text), this is the last text block. Empty if unknown.
	LastResponse string

	// LastTool is the name of the most recent tool the agent used,
	// if any. E.g. "Bash", "Read", "Edit". Empty if not applicable.
	LastTool string

	// StartedAt is when the controller observed the current turn begin.
	// Zero if unknown or no turn has started yet.
	StartedAt time.Time

	// CompletedAt is when the most recent turn finished. Zero while
	// the tool is still working.
	CompletedAt time.Time

	// Error captures a tool-side error if the agent reported one
	// (auth failure, rate limit, etc). Nil for normal operation.
	Error error
}

// ControllerState is the lifecycle stage of an interactive tool.
type ControllerState int

const (
	CtrlUnknown            ControllerState = iota // controller hasn't observed enough to decide
	CtrlStarting                                  // tool is initializing
	CtrlWorking                                   // tool is actively processing
	CtrlAwaitingInput                             // tool finished a turn, waiting for user
	CtrlAwaitingPermission                        // tool is blocked on a permission prompt
	CtrlError                                     // tool reported an error
	CtrlGone                                      // tool process has exited
)

func (s ControllerState) String() string {
	switch s {
	case CtrlStarting:
		return "starting"
	case CtrlWorking:
		return "working"
	case CtrlAwaitingInput:
		return "awaiting_input"
	case CtrlAwaitingPermission:
		return "awaiting_permission"
	case CtrlError:
		return "error"
	case CtrlGone:
		return "gone"
	default:
		return "unknown"
	}
}

// ControllerFactory inspects a pane's command/env and returns a controller
// if it can handle that tool, or nil if not. magmux walks the registered
// factories in order; the first non-nil result wins.
type ControllerFactory func(p *Pane) ToolController
