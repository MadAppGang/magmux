package protocol

// Version is the version reported by `capabilities`. Bumped only when a
// client that understood the previous value would misread the new one.
const Version = 1

// Event types: the "type" of each line magmux sends its subscribers, and the
// list the `capabilities` verb advertises as "events".
const (
	// EventSnapshot comes in two shapes. The aggregate carries "panes", every
	// pane at once, and is the first line on every connection; the per-pane
	// one carries "pane" and is sent when that pane's state changes.
	EventSnapshot   = "snapshot"
	EventExit       = "exit"
	EventControl    = "control"
	EventPaneOpened = "pane_opened"
	EventPaneClosed = "pane_closed"
	// EventResults is the aggregate sent once at shutdown, before
	// EventShutdown.
	EventResults  = "results"
	EventShutdown = "shutdown"
	// EventReply answers one request that carried an "id", and goes only to
	// the connection that sent it.
	EventReply = "reply"
)
