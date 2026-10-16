package protocol

import (
	"errors"
	"fmt"
)

// Stable, machine-readable failure codes. A caller branches on the code; the
// message beside it is for a human and may be reworded at any time.
const (
	CodeBadRequest    = "bad_request"
	CodeNoSuchPane    = "no_such_pane"
	CodePaneIsControl = "pane_is_control"
	CodePaneDead      = "pane_dead"
	// CodePaneHidden means the pane is alive and holds its id but is not in
	// the layout, so nothing paints it. Distinct from no_such_pane because the
	// pane is real and its history is intact, and distinct from pane_is_control
	// because it is about VISIBILITY: the panel is a perfectly good focus target
	// while it is on screen, and every other pane could in principle be hidden.
	CodePaneHidden  = "pane_hidden"
	CodeUnknownVerb = "unknown_verb"
	// CodeTooSmall means the layout has no room: a split that would leave
	// either half below the minimum usable pane size is refused before
	// anything is forked, and a `resize` that would leave any session pane
	// below it is refused before any pane is touched. Distinct from
	// bad_request because the request was well formed and would succeed on a
	// bigger terminal (or a bigger size), which sends the caller to a
	// different remedy: drop a pane, or find more columns.
	CodeTooSmall = "too_small"
	// CodeForbidden means the caller is authenticated but not allowed: a
	// read-only connection asking for an op that is not class read, or a
	// connection claiming something it never registered for. Distinct from
	// unauthorized, which is about identity, and from unsupported, which is
	// about magmux not having the feature at all.
	CodeForbidden = "forbidden"
	// CodeUnauthorized means the caller presented no credential or a wrong
	// one. It is about IDENTITY, where forbidden is about permission: the
	// remedy for one is a different token and for the other a different
	// request, so a caller that could not tell them apart would retry the wrong
	// thing. It can only arise on a transport that has credentials at all —
	// the unix socket's credential is the filesystem.
	CodeUnauthorized = "unauthorized"
	// CodeTooLarge means the request or message exceeded a hard size bound: an
	// HTTP body, a WebSocket message, a plugin event. Distinct from
	// bad_request because the payload may be perfectly well formed, and the
	// caller's remedy is to send less rather than to send it differently.
	// Also a requested GEOMETRY beyond magmux's bounds (a side over 10000, more
	// than 4,000,000 cells) or, on an interactive terminal, beyond the terminal:
	// the remedy is the same — ask for less.
	CodeTooLarge = "too_large"
	// CodePluginGone means the op existed and its plugin did not answer: the
	// process died, or its connection dropped, with the call in flight. magmux
	// is the gateway here and the plugin is the upstream, which is why it is
	// its own code rather than internal — nothing in magmux failed.
	CodePluginGone = "plugin_gone"
	// CodeNotReady means the socket is up but the layout is not: magmux
	// binds before the first child forks and can therefore be reached before
	// buildGrid has run. Distinct from no_such_pane on purpose — "pane 0 does
	// not exist" and "no pane exists yet" send a caller to opposite places, and
	// the second one is fixed by waiting rather than by using another index.
	CodeNotReady    = "not_ready"
	CodeUnsupported = "unsupported"
	CodeBusy        = "busy"
	CodeTimeout     = "timeout"
	CodeInternal    = "internal"
	// CodeNoController means the pane exists and is perfectly healthy but
	// magmux is not following a tool inside it — a shell, a dev server, a REPL.
	// Distinct from unsupported because the recovery differs: there is nothing
	// to wait for and nothing to fix, so the caller should read the screen.
	CodeNoController = "no_controller"
	// CodeNoTranscript means a controller IS following this pane but has
	// not located the tool's own record of it. It is emphatically not an empty
	// success: "we cannot find its record" and "it has said nothing" send a
	// caller to opposite places, and discovery genuinely lags at session start
	// and can fail outright (see the ~/.claude/projects note in CLAUDE.md).
	CodeNoTranscript = "no_transcript"
	// CodeNotAccepted means the request typed into a pane's input box and the
	// input was not shown to be taken: either the box did not hold what was
	// typed (and no Enter was pressed), or Enter was pressed and no evidence
	// attributable to a user prompt followed. Reason says which (reasons.go).
	// Distinct from busy because something WAS typed, so "wait and send again"
	// is not a safe remedy: the box may already hold the text.
	CodeNotAccepted = "not_accepted"
)

// Error is a verb failure with a stable machine-readable code.
//
// Reason is an optional machine-readable SUB-code (reasons.go): it says which
// busy or which not_accepted, so a caller can branch without parsing the
// message. Typed reports that bytes of THIS request reached the PTY before it
// failed, which is what tells a caller the input box may hold part of it. Both
// are wire-visible as "reason" and "typed", and only when set, so an error
// without them serialises exactly as it always did.
type Error struct {
	Code, Msg string
	Reason    string
	Typed     bool
}

func (e *Error) Error() string { return e.Msg }

// Errf returns an Error with the given code and a message formatted as by
// fmt.Sprintf.
func Errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Errr is Errf with a reason.
func Errr(code, reason, format string, a ...any) *Error {
	return &Error{Code: code, Reason: reason, Msg: fmt.Sprintf(format, a...)}
}

// ReasonOf extracts the reason from a verb failure, or "" when there is none.
// It reads through wrapping, as CodeOf does.
func ReasonOf(err error) string {
	var se *Error
	if errors.As(err, &se) {
		return se.Reason
	}
	return ""
}

// TypedOf reports whether a verb failure says bytes reached the PTY first. It
// is false for nil and for any error that does not carry the flag.
func TypedOf(err error) bool {
	var se *Error
	if errors.As(err, &se) {
		return se.Typed
	}
	return false
}

// CodeOf extracts the machine-readable code from a verb failure, or "".
// A nil error has no code; a non-nil error that carries none is CodeInternal.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return CodeInternal
}
