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
	// anything is forked. Distinct from bad_request because the request was
	// well formed and would succeed on a bigger terminal, which sends the
	// caller to a different remedy: drop a pane, or find more columns.
	CodeTooSmall = "too_small"
	// CodeForbidden means the caller is authenticated but not allowed: a
	// read-only connection asking for an op that is not class read, or a
	// connection claiming something it never registered for. Distinct from
	// unauthorized, which is about identity, and from unsupported, which is
	// about magmux not having the feature at all.
	CodeForbidden = "forbidden"
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
)

// Error is a verb failure with a stable machine-readable code.
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Msg }

// Errf returns an Error with the given code and a message formatted as by
// fmt.Sprintf.
func Errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
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
