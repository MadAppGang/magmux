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
	// EventFrame is one pane's screen, delivered to the connections that asked
	// for it with `watch`. It never queues: each (connection, pane) has one
	// latest-wins slot, so a subscriber that falls behind sees fewer frames and
	// never an older screen.
	EventFrame = "frame"
	// EventChanged is the cheap half of watching: a pane moved, go and read it.
	// Rate-limited to one per pane per second.
	EventChanged = "changed"
	// EventOpsChanged says the op list has a new revision, because a plugin
	// registered or died.
	EventOpsChanged = "ops_changed"
	// EventPlugin is one plugin's own event, forwarded to every subscriber. It
	// names the plugin and the event, so a client filters on two fields rather
	// than on a namespaced string it has to parse.
	EventPlugin = "plugin"
	// EventPluginExited says a plugin's process or connection is gone. Its ops
	// are already unregistered by the time this is published (ops_changed comes
	// first), so a client that reacts by re-fetching `ops` cannot see the dead
	// plugin's ops again.
	EventPluginExited = "plugin_exited"
)
