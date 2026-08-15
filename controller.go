package main

import (
	"context"
	"errors"
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

// InputNotifier is an optional interface a ToolController may implement to
// learn that input was pushed into its pane from outside magmux — a pilot's
// `send` on the IPC socket.
//
// It exists because a controller's idle state is otherwise one-way. A
// controller promotes itself to CtrlAwaitingInput when the terminal looks
// idle, but only the tool's own transcript can move it back to working. When
// a pilot injects an instruction and the transcript is missing or lagging,
// nothing else would ever unstick the state, and the pilot would wait
// forever for a turn that had in fact already begun.
//
// Implementations should treat the call as "a new turn is starting now" and
// must remain safe if the tool ignores the input — the ordinary idle
// heuristics have to be able to settle the state again on their own.
type InputNotifier interface {
	NotifyInput()
}

// ── turn history ────────────────────────────────────────────────────────────

// Turn is one entry in a tool's own record of a session.
//
// The granularity is one MESSAGE, not one exchange: a user prompt is a Turn,
// and the assistant's whole reply to it — however many entries the tool split
// that across in its record — is the next Turn. That is what makes Role
// meaningful, and what makes "the last four turns" read as a conversation
// rather than as a fragment of one.
type Turn struct {
	// Role is who spoke: TurnUser or TurnAssistant.
	Role string

	// Text is the complete text of the turn, never truncated. A turn that was
	// nothing but tool calls has none, which is normal and is not a failure.
	Text string

	// Tools are the tool calls made during this turn, in the order the tool
	// recorded them, each with the result it produced.
	Tools []ToolCall

	// Timestamp is when the tool recorded the turn's first entry. Zero when
	// the record carried none.
	Timestamp time.Time
}

// ToolCall is one tool invocation and its outcome, as the tool itself recorded
// them. Input and Result are the whole point: Snapshot.LastTool has only ever
// carried a tool's NAME, which tells a reader that something ran and nothing
// whatever about what it did or what it found.
type ToolCall struct {
	Name   string // e.g. "Bash", "Edit"
	Input  string // the arguments, as JSON, exactly as the tool recorded them
	Result string // what the tool returned; empty if it had not returned yet

	// id pairs a call with its result inside the tool's record. Unexported
	// because it is the parser's bookkeeping and means nothing to a caller.
	id string
}

// Turn roles. Deliberately the same two words every tool in this space uses,
// so a consumer needs no translation table.
const (
	TurnUser      = "user"
	TurnAssistant = "assistant"
)

// Transcript request bounds. These bound MEMORY, not presentation: a caller
// that wants to show less truncates at its own boundary, where it knows what
// its budget is.
const (
	// defaultTranscriptTurns is what a caller who asked for "some" history
	// gets. Six turns is three exchanges — the instruction just given and the
	// two before it, which is the context a driver almost always wants and
	// rarely more than it can afford.
	defaultTranscriptTurns = 6

	// maxTranscriptTurns caps one request. A driver asking for 5,000 turns is
	// asking for a whole session's history, which no consumer can use and
	// every consumer would have to pay to receive.
	maxTranscriptTurns = 200
)

// normalizeTranscriptTurns applies the default and the cap. Callers on both
// sides of the socket use it, so a reply's "requested" field and the number of
// turns actually read can never disagree about what was asked for.
func normalizeTranscriptTurns(n int) int {
	switch {
	case n <= 0:
		return defaultTranscriptTurns
	case n > maxTranscriptTurns:
		return maxTranscriptTurns
	}
	return n
}

// TranscriptReader is an optional interface a ToolController may implement to
// produce real turn history from the tool's OWN on-disk record, rather than
// from the screen.
//
// It is optional for the same reason InputNotifier is, and is checked the same
// way — a type assertion at the call site. Only a controller that has found a
// durable record of the session can answer this; a controller that watches a
// pane by other means simply does not implement it, and must not be forced to
// pretend.
//
// It exists because Snapshot is deliberately a STATUS surface: the latest
// prompt, the latest response text, and the NAME of the latest tool. That is
// what a status line needs and almost none of what an agent driving the
// session needs — it can see neither what a tool was asked to do, nor what the
// tool answered, nor anything at all about the turn before this one.
//
// Implementations must:
//
//   - return the last `turns` turns, OLDEST FIRST, and fewer than that when
//     fewer exist;
//   - never truncate a turn's text. Truncation is a presentation decision and
//     belongs at the boundary that has a context budget to defend;
//   - bound their own memory anyway — "do not truncate" is not "read a 200MB
//     record into RAM";
//   - be safe to call from a goroutine other than the one calling Poll, and
//     hold no lock across their file I/O.
//
// A controller that has not located the record returns errNoTranscript, and
// never an empty slice. Those two answers send a caller to opposite places:
// "this session has said nothing" invites it to proceed, while "we cannot find
// this session's record" invites it to retry or to read the screen instead.
// The distinction is not hypothetical — transcript discovery is undocumented
// territory owned by the tool (see the ~/.claude/projects note in CLAUDE.md),
// it lags by seconds at session start, and it sometimes fails outright.
type TranscriptReader interface {
	Transcript(turns int) ([]Turn, error)
}

// errNoTranscript reports that a controller could not reach the tool's record:
// it has not been discovered yet, or it has gone away since. Wrapped rather
// than compared, so a caller uses errors.Is.
var errNoTranscript = errors.New("no transcript has been located for this pane")

// ControllerFactory inspects a pane's command/env and returns a controller
// if it can handle that tool, or nil if not. magmux walks the registered
// factories in order; the first non-nil result wins.
type ControllerFactory func(p *Pane) ToolController
