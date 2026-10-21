package mux

import (
	"context"
	"errors"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
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

	// Input is the controller's published view of whether the tool would
	// receive input right now (InputGate). It is carried BY VALUE, so nothing
	// is shared after publication, and it is set only for a gated pane
	// (Input.Gated); every other pane leaves it zero.
	//
	// It is a VIEW, refreshed each Poll from history: a send decides on its own
	// synchronous reads, never on this. And it is deliberately NOT part of
	// snapshotChanged: a readiness flip is not a turn, and emitting on one
	// would make the control panel count an IN row that never happened.
	Input InputReadiness
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

// InputGate is an optional interface a ToolController implements when it can
// tell whether the tool will RECEIVE input right now, verify what reached its
// input box, and see whether a submitted prompt was taken. A pane is gated only
// when its controller implements this AND its gateBasis is declared or direct
// (inputGateOf); otherwise `send` behaves byte for byte as it always has.
//
// It is checked by type assertion, like InputNotifier and TranscriptReader.
//
// All methods are called from a lane (or op) goroutine, never the render
// goroutine, with NO magmux lock held. They may take p.mu (via the capture
// helpers) and their own leaf locks, and may do small file reads and one
// ioctl; they must not block on the child, the socket, or treeMu.
type InputGate interface {
	// InputReadiness reports whether a delivery described by q would land now.
	// It is a DEMAND: one synchronous termios sample and one fresh screen read.
	InputReadiness(q ReadinessQuery) InputReadiness
	// CheckBox is the pre-submit verify. One synchronous termios sample and
	// ONE fresh screen read, judged twice: readiness (with Types:false) and the
	// box against want. When the verdict is BoxVerified, the returned Baseline
	// was taken from that same read.
	CheckBox(want BoxExpect) BoxCheck
	// InputAccepted reports what the controller can see of the submission
	// baselined in b. Called repeatedly during the acceptance windows. It
	// decides no write, so it takes no termios sample.
	InputAccepted(b AcceptBaseline) AcceptVerdict
}

// SendClass is what a `send` does to the box: KEYS (no text, no submit),
// TYPE (text or paste, no submit) or SUBMIT (a final Enter).
type SendClass int

const (
	ClassKeys SendClass = iota
	ClassType
	ClassSubmit
)

// ReadinessQuery says what a delivery will do, so the rules that depend on it
// can be applied.
type ReadinessQuery struct {
	Class SendClass
	Types bool // the delivery puts characters into the box: rule h (box_not_empty) applies
	Paste bool // the text goes out bracketed: rule i (bracketed paste on) applies
	// Baseline, when set, is the content this delivery will type into an
	// empty box; the answer then carries Pre, an acceptance baseline for it
	// taken from the SAME read. It is what lets a delivery ask, before a
	// retype, whether somebody else's Enter already submitted its text.
	Baseline string
}

// InputReadiness is one readiness answer. Gated is false for a pane nobody
// gates, and then nothing else is meaningful and the key is omitted.
type InputReadiness struct {
	Gated       bool   `json:"-"`
	Ready       bool   `json:"ready"`
	Reason      string `json:"reason,omitempty"`      // a protocol.Reason* value; "" when Ready
	Detail      string `json:"detail,omitempty"`      // human text, e.g. "termios changed 340ms ago"
	Dismissable bool   `json:"dismissable,omitempty"` // a dialog Escape declines is showing
	BoxHasText  bool   `json:"boxHasText,omitempty"`  // the box holds text
	MaxPaste    int    `json:"-"`                     // largest [Pasted text #N] on screen
	// Pre is the acceptance baseline for ReadinessQuery.Baseline, from this
	// read (zero when no Baseline was asked for, or the gate cannot take one).
	Pre AcceptBaseline `json:"-"`
}

// BoxExpect is what the delivery knows it put in the box.
type BoxExpect struct {
	Class SendClass // ClassType (no submit follows) or ClassSubmit
	// Exact is the full content, when the box was EMPTY at the post-token
	// check and every pre-Enter key was printable: text (typed newlines as
	// "\n") plus those keys' runes. "" = undetermined: only non-empty can be
	// checked.
	Exact     string
	TypedText bool // the delivery wrote text or printable keys (text_mismatch vs empty_box)
	// Pasted: Claude may show this send as a `[Pasted text #N` token. It only
	// WIDENS what verifies; equality still verifies.
	Pasted     bool
	PasteAbove int // MaxPaste at the post-token check; a verifying token must exceed it
	// TypedAt is the moment of this attempt's first byte: a termios change
	// after it (BoxCheck.TermiosChanged) is the positive evidence of a reset
	// that the auto-retype requires.
	TypedAt time.Time
	// Pre is the attempt's pre-typing acceptance baseline (InputReadiness.Pre),
	// when it has one: what BoxCheck.ConvChanged compares the conversation
	// against.
	Pre AcceptBaseline
}

// BoxVerdict is CheckBox's judgement of the box.
type BoxVerdict int

const (
	BoxVerified BoxVerdict = iota
	BoxMismatch
	BoxEmpty
)

// BoxCheck is CheckBox's answer. Verdict is meaningful only when
// Readiness.Ready.
type BoxCheck struct {
	Readiness InputReadiness
	Verdict   BoxVerdict
	Verified  string // "exact" | "paste_token" | "non_empty" (when BoxVerified)
	Detail    string
	Baseline  AcceptBaseline // from THIS read (when BoxVerified)
	// Box is the content the box was judged on (the controller's own key of
	// it), when Readiness.Ready. The settle wait compares it read to read to
	// tell a box still being drawn from one that has stopped changing.
	Box string
	// Excess: a mismatch where the box holds MORE than the send typed (the
	// text drawn twice). It forbids the auto-retype.
	Excess bool
	// TermiosChanged: the pty's termios fingerprint changed after
	// BoxExpect.TypedAt (a cooked/reset sample in the watch's history). It
	// is the evidence that typed text was SWALLOWED by a stdin reset, rather
	// than merely slow to draw, and the auto-retype requires it.
	TermiosChanged bool
	// ConvChanged, when BoxExpect.Pre is set and Readiness.Ready: what changed
	// since that pre-typing baseline that a SUBMIT would have changed — the
	// conversation above the box, or a turn starting. "" = nothing. A submit
	// by another writer before the text was ever drawn leaves an empty box
	// that looks exactly like a silent drop; this is how the two are told
	// apart, and a change forbids the never_drawn retype (r5 M1).
	ConvChanged string
}

// Verified values.
const (
	verifiedExact      = "exact"
	verifiedPasteToken = "paste_token"
	verifiedNonEmpty   = "non_empty"
)

// AcceptBaseline is opaque to mux; the controller fills it from the CheckBox
// read, immediately before the Enter.
type AcceptBaseline struct {
	At          time.Time // the CheckBox read
	VerifiedKey string    // inputKey of the box content that was verified
	SentKey     string    // inputKey(Exact) when determined, else VerifiedKey
	Paste       int       // the verifying token's N (paste_token), else 0
	// controllerData is the controller's own baseline (Claude: the matching
	// entry count and the transcript seq).
	controllerData any
}

// AcceptState is what InputAccepted saw.
type AcceptState int

const (
	AcceptNone  AcceptState = iota // no attributable evidence yet
	AcceptTaken                    // a new matching user-prompt entry, or a matching transcript entry
	AcceptInBox                    // the box is visible and still holds the verified content
)

// AcceptVerdict is InputAccepted's answer.
type AcceptVerdict struct {
	State    AcceptState
	Evidence string // "history" | "transcript" | ""
	Dialog   string // a dialog that appeared (informational; never evidence)
}

// Evidence values.
const (
	evidenceHistory    = "history"
	evidenceTranscript = "transcript"
)

// ── turn history ────────────────────────────────────────────────────────────

// Turn and ToolCall are one entry in a tool's own record of a session, and one
// tool invocation inside it. They are declared in internal/obs and ALIASED
// here.
//
// An alias is the same type, not a conversion, so [TranscriptReader] below is
// the contract it always was, the unexported correlation field on ToolCall
// travels with the type, and no caller — in this package, over the socket, or
// through MCP — notices that they moved. They moved because Go forbids
// importing package main: the observation layer under internal/obs reads the
// same transcripts and would otherwise need a second, parallel Turn type, and
// two vocabularies for one thing diverge the first time either side gains a
// field.
//
// The parser moved with them, to internal/obs/adapters, for the same reason
// and one more: a ToolCall's correlation id is unexported, so whatever builds
// one has to live where it can be set. [parseClaudeTurns] below is what is
// left here — one line, so that every existing caller and test compiles
// untouched.
type (
	Turn     = obs.Turn
	ToolCall = obs.ToolCall
)

// Turn roles. Deliberately the same two words every tool in this space uses,
// so a consumer needs no translation table. Aliased to obs's own, which are
// the values that reach the wire.
const (
	TurnUser      = obs.TurnUser
	TurnAssistant = obs.TurnAssistant
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
