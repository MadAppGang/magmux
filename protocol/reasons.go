package protocol

// Reasons: the machine-readable sub-codes an Error may carry beside its code.
//
// A code says what KIND of failure it was and what class of remedy applies; a
// reason says which one of that kind, so a caller can tell a pane that is still
// booting from one whose hub queue is full without reading the message. A
// reason never changes what a code means, and a caller that knows only the
// codes keeps working. Like the codes, these strings are stable wire values.
const (
	// ── busy: nothing was typed (or, with typed, the Enter was withheld) ──

	// ReasonStarting: the tool is booting — no input box yet, its terminal
	// has not settled, or the box has only just appeared.
	ReasonStarting = "starting"
	// ReasonTerminalNotReady: the pane was ready before, and its terminal is
	// now not raw, changed too recently, or lacks bracketed paste for a paste.
	ReasonTerminalNotReady = "terminal_not_ready"
	// ReasonNoInputBox: the pane was ready before, and now shows neither an
	// input box nor a recognised dialog.
	ReasonNoInputBox = "no_input_box"
	// ReasonBoxNotEmpty: the request would type into a box that already holds
	// text that is not its own.
	ReasonBoxNotEmpty = "box_not_empty"
	// ReasonVimMode: the box is in vim editing mode, where typed text would be
	// read as commands.
	ReasonVimMode = "vim_mode"
	// ReasonDialogPrefix is followed by the dialog's kind: permission,
	// question, plan, select, trust, bypass, api_key or onboarding.
	ReasonDialogPrefix = "dialog:"
	// ReasonTypingInProgress: another delivery held the pane's typing token
	// past the readiness deadline.
	ReasonTypingInProgress = "typing_in_progress"
	// ReasonQueueFull: this connection's lane for the pane is full.
	ReasonQueueFull = "queue_full"
	// ReasonInFlight: a cap on concurrent requests was reached — this
	// connection's, or a plugin's.
	ReasonInFlight = "in_flight"

	// ── not_accepted ──

	// ReasonTextMismatch: the box did not hold what was typed. No Enter was
	// pressed; the box may hold partial text. magmux never types the text a
	// second time on its own, including when the box stayed empty.
	ReasonTextMismatch = "text_mismatch"
	// ReasonEmptyBox: a submit found the box empty. No Enter was pressed.
	ReasonEmptyBox = "empty_box"
	// ReasonStillInBox: Enter was pressed (once or twice) and the text is
	// still in the box, unsubmitted. Do not retype it.
	ReasonStillInBox = "still_in_box"
	// ReasonUnconfirmed: Enter was pressed and no evidence attributable to a
	// user prompt appeared in time. The outcome is uncertain.
	ReasonUnconfirmed = "unconfirmed"

	// ── forbidden ──

	// ReasonReadOnly: the request came in on the read-only view socket
	// (--view-sock), which may only read and watch.
	ReasonReadOnly = "read_only"
)

// TypingStopped values: why a gated send stopped typing before its last
// write. Wire-visible as "typingStopped", beside "writes" and "writesTotal",
// and present only when writes < writesTotal. A stopped send never presses
// Enter, and magmux never writes the rest on its own.
const (
	// TypingStoppedDialog: a dialog appeared between two writes.
	TypingStoppedDialog = "dialog"
	// TypingStoppedTerminalReset: the terminal's termios changed after the
	// first byte, so the reader may have reset and typing on could join copies.
	TypingStoppedTerminalReset = "terminal_reset"
	// TypingStoppedBoxChanged: the box held something other than what had been
	// written so far, unchanged for the settle interval.
	TypingStoppedBoxChanged = "box_changed"
	// TypingStoppedDeadline: the pane was still not ready for the next write,
	// or its box was still changing, at the send's readiness deadline.
	TypingStoppedDeadline = "deadline"
	// TypingStoppedCancelled: magmux shut the delivery down (Quiesce).
	TypingStoppedCancelled = "cancelled"
	// TypingStoppedPaneDead: the child exited, its PTY closed, or the pane was
	// closed; the error's code says which.
	TypingStoppedPaneDead = "pane_dead"
	// TypingStoppedNotReady: the pane was not ready for the next write, and
	// what the box should hold was unknowable (an editing key was written), so
	// the delivery could not wait for it.
	TypingStoppedNotReady = "not_ready"
)
