package protocol

// The plugin protocol: the messages a plugin process and magmux exchange over
// the ordinary control socket.
//
// A plugin is not a second kind of client. It connects to the same socket,
// speaks the same line-delimited JSON, and may use every ordinary verb —
// `open_pane`, `send`, `watch`. What the messages here add is the other
// direction: a plugin ADVERTISES ops, magmux INVOKES them, and a plugin that
// owns a pane's controller pushes its own observations back.
//
// They are decoded from the raw line into the types below and never into the
// multiplexer's own request struct. That separation is deliberate: a plugin
// message carries a token and an identity claim, and letting it share a decode
// path with the ordinary verbs would put fields on the wire that could reach a
// verb's handler by accident.

import (
	"encoding/json"
	"strings"
)

// The plugin-protocol message types. Two of them travel magmux → plugin
// (MsgInvoke, MsgInvokeCancel); the rest travel plugin → magmux.
//
// MsgInvoke is deliberately not called "call": `call` is the ordinary socket
// verb a CLIENT sends magmux, and one word meaning opposite directions on one
// socket is a bug waiting for a reader in a hurry.
const (
	MsgPluginRegister     = "plugin.register"
	MsgInvoke             = "invoke"
	MsgInvokeResult       = "invoke_result"
	MsgInvokeCancel       = "invoke_cancel"
	MsgPluginEvent        = "plugin.event"
	MsgControllerSnapshot = "controller.snapshot"
)

// IsPluginMessage reports whether a line's "type" belongs to the plugin
// protocol and must therefore be routed to the plugin host rather than decoded
// as an ordinary verb.
//
// It names only the plugin → magmux directions, because those are the only ones
// magmux ever RECEIVES. A client that sent `invoke` would be claiming to be
// magmux, and it falls through to the ordinary unknown-verb answer.
func IsPluginMessage(typ string) bool {
	switch typ {
	case MsgPluginRegister, MsgInvokeResult, MsgPluginEvent, MsgControllerSnapshot:
		return true
	}
	return false
}

// Envelope is the peek: the two fields every message has, decoded from the raw
// line before anything decides what the rest of it means.
//
// ID is json.RawMessage so a numeric or string id round-trips verbatim, exactly
// as it does for the ordinary verbs.
type Envelope struct {
	Type string          `json:"type"`
	ID   json.RawMessage `json:"id,omitempty"`
}

// PluginRegister is a plugin announcing itself: who it is, what it offers, and
// the token that proves magmux started it (or that its operator gave it the
// session's own token).
//
// Ops carries OpSpec minus Source: Source is stamped by the registry, never by
// the registrant, so a plugin cannot claim its ops are magmux's.
type PluginRegister struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Token   string          `json:"token"`
	Name    string          `json:"name"`
	Version string          `json:"version,omitempty"`
	Ops     []OpSpec        `json:"ops"`
	Events  []string        `json:"events,omitempty"`
}

// Invoke is magmux asking a plugin to run one of its ops.
//
// Op is the BARE name the plugin registered (`run_ticket`), not the qualified
// one the registry advertises (`ticket.run_ticket`): a plugin should not have to
// strip its own name off every request to find its handler.
//
// DeadlineMs is how long magmux will wait. It is told rather than left implicit
// so a plugin can fail fast with its own diagnosis instead of being cut off with
// magmux's.
type Invoke struct {
	Type       string          `json:"type"`
	Call       string          `json:"call"`
	Op         string          `json:"op"`
	Args       json.RawMessage `json:"args,omitempty"`
	Caller     InvokeCaller    `json:"caller"`
	DeadlineMs int             `json:"deadlineMs"`
}

// InvokeCaller is who asked, as the plugin is told it. It is the hub's Caller
// minus Plugin: a plugin is never told which other plugin is calling it,
// because that would be an authorisation surface nobody designed.
//
// Client is SELF-DECLARED by the caller and is a label only. A plugin that
// authorises on it is authorising on a string anybody may send.
type InvokeCaller struct {
	Transport string `json:"transport,omitempty"`
	Conn      string `json:"conn,omitempty"`
	Client    string `json:"client,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// InvokeCancel tells a plugin magmux has stopped waiting: the caller's deadline
// expired, the connection went away, or magmux is shutting down. A plugin that
// ignores it is not killed — it simply keeps running work whose answer nobody
// will read.
type InvokeCancel struct {
	Type string `json:"type"`
	Call string `json:"call"`
}

// InvokeResult is a plugin answering one invoke. Code is one of this package's
// codes; a result with ok:false and no code is reported to the caller as
// internal, because a failure with no machine-readable cause is magmux's
// problem to describe rather than the caller's to branch on.
type InvokeResult struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Call   string          `json:"call"`
	OK     bool            `json:"ok"`
	Result map[string]any  `json:"result,omitempty"`
	Code   string          `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// PluginEvent is a plugin telling every subscriber something happened. It must
// name an event the plugin DECLARED at registration: the declaration is what
// makes the stream describable to a client that has never heard of this plugin,
// and an undeclared event is refused rather than forwarded.
//
// Pane is optional — an event may be about the session as a whole — and Data is
// the plugin's own shape, carried verbatim.
type PluginEvent struct {
	ID    json.RawMessage `json:"id,omitempty"`
	Event string          `json:"event"`
	Pane  *int            `json:"pane,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// ControllerSnapshot is a plugin reporting what the tool in its pane is doing.
// It is accepted only from the connection whose plugin owns that pane's
// controller: a plugin may describe the session it is driving and no other.
//
// The fields mirror magmux's own controller snapshot, so a plugin-observed pane
// and a magmux-observed one produce the same `snapshot` event and the same
// entry in `results`.
type ControllerSnapshot struct {
	ID       json.RawMessage `json:"id,omitempty"`
	Pane     *int            `json:"pane"`
	State    string          `json:"state"`
	Response string          `json:"response,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Prompt   string          `json:"prompt,omitempty"`
	Model    string          `json:"model,omitempty"`
	Project  string          `json:"project,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// ── the name grammar ────────────────────────────────────────────────────────
//
// Both grammars are narrow on purpose, and the ONE reason is that a plugin op's
// name has to survive three namespaces unchanged: magmux's own op table
// (`ticket.run_ticket`), an MCP tool name (`ticket__run_ticket`) and a Firebase
// RTDB key. A plugin name therefore excludes `_`, so a qualified name splits
// unambiguously at its first `__`, and both halves exclude `.` and `/` so no
// name can ever be a path or an RTDB path segment.

// Length bounds. 32 + 2 + 30 = 64, which is the MCP tool-name ceiling.
const (
	MaxPluginNameLen = 32
	MaxOpNameLen     = 30
)

// ValidPluginName reports whether s matches [a-z][a-z0-9-]{0,31}.
func ValidPluginName(s string) bool {
	if len(s) == 0 || len(s) > MaxPluginNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		case i > 0 && c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidOpName reports whether s matches [a-z][a-z0-9_]{0,29}. It is the grammar
// for the BARE op name a plugin registers, not for the qualified one.
func ValidOpName(s string) bool {
	if len(s) == 0 || len(s) > MaxOpNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		case i > 0 && c == '_':
		default:
			return false
		}
	}
	return true
}

// ValidEventName uses the op grammar. An event is a name in the same namespace
// as an op — a client sees both in one `ops`/event vocabulary — and two
// grammars for two kinds of identifier would be one more thing to get wrong.
func ValidEventName(s string) bool { return ValidOpName(s) }

// QualifiedOp is how a plugin op appears in the op table and on the wire:
// `<plugin>.<op>`. It is the name a `call` uses and the name --view-op grants.
func QualifiedOp(plugin, op string) string { return plugin + "." + op }

// SplitQualifiedOp reverses QualifiedOp. It splits at the FIRST dot, which is
// unambiguous because neither grammar admits one.
func SplitQualifiedOp(name string) (plugin, op string, ok bool) {
	plugin, op, ok = strings.Cut(name, ".")
	if !ok || plugin == "" || op == "" {
		return "", "", false
	}
	return plugin, op, true
}

// ToolName is how a plugin op appears to an MCP client: `<plugin>__<op>`. MCP
// tool names admit no dot, which is the whole reason this second spelling
// exists.
//
// A client must resolve a tool name through the table built from `ops` rather
// than by splitting the string. SplitToolName exists for magmux's own use and
// is safe only because the grammars forbid `_` in a plugin name.
func ToolName(plugin, op string) string { return plugin + "__" + op }

// SplitToolName reverses ToolName at the first `__`.
func SplitToolName(name string) (plugin, op string, ok bool) {
	plugin, op, ok = strings.Cut(name, "__")
	if !ok || plugin == "" || op == "" {
		return "", "", false
	}
	return plugin, op, true
}
