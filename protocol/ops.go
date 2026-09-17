package protocol

import "encoding/json"

// OpClass is what an op does, which decides who may call it: "read",
// "control", "display" or "input".
//
// The class is a CAPABILITY statement, not a category for documentation: a
// read-only caller (the view token) is refused everything that is not
// ClassRead, so a plugin that mislabels a pane-steering op as "read" is
// widening its own permissions. That is why magmux never lets a plugin's
// self-declared class grant a viewer anything on its own — see --view-op.
type OpClass string

// The four classes. ClassRead is the only one a read-only caller may use.
const (
	// ClassRead observes and changes nothing: capabilities, list, capture,
	// transcript, ops, and watch/unwatch/resync.
	ClassRead OpClass = "read"
	// ClassControl steers the session: open_pane, close_pane, focus, send.
	ClassControl OpClass = "control"
	// ClassDisplay decorates magmux's own chrome without touching a session:
	// status, tint, overlay.
	ClassDisplay OpClass = "display"
	// ClassInput is the human-equivalent path into a pane's PTY: input. It is
	// its own class rather than control because a controller uses send, which
	// the control panel records; input is what a keyboard would have done.
	ClassInput OpClass = "input"
)

// SourceBuiltin is the Source the registry stamps on magmux's own ops. It is
// in protocol rather than in mux because it is a wire value a client branches
// on: "is this op magmux's, or did a plugin add it" decides whether the view
// token may reach it, and that rule is enforced in the transport layer, which
// must not import mux.
const SourceBuiltin = "magmux"

// ValidClass reports whether c is one of the four classes above.
func ValidClass(c OpClass) bool {
	switch c {
	case ClassRead, ClassControl, ClassDisplay, ClassInput:
		return true
	}
	return false
}

// OpSpec describes one op as magmux advertises it. The JSON tags are the wire
// shape: a spec crosses the socket verbatim in an `ops` reply and, minus
// Source, in a plugin's own `plugin.register`.
//
// Schema is a JSON Schema for the op's args, and is an OBJECT schema even when
// the op takes nothing ({"type":"object"}): an MCP client turns it straight
// into a tool's inputSchema, where null is not a valid value. The registry
// fills an absent one in rather than letting it reach the wire as null.
//
// Source is who registered the op — "magmux" for a built-in, the plugin's name
// otherwise. It is assigned by the registry, never by the registrant, so a
// plugin cannot claim to be magmux.
type OpSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Class       OpClass         `json:"class"`
	Source      string          `json:"source"`
}
