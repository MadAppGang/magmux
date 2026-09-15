package protocol

import "encoding/json"

// OpClass is what an op does, which decides who may call it: "read",
// "control", "display" or "input".
type OpClass string

// OpSpec describes one op as magmux advertises it.
type OpSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Class       OpClass         `json:"class"`
	Source      string          `json:"source"`
}
