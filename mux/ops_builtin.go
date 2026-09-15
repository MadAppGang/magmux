package mux

// The built-in ops: magmux's own verbs, as the hub advertises and calls them.
//
// Every op here WRAPS the existing dispatch rather than reimplementing it. An
// op builds the same sockMsg the socket reader would have built and hands it
// to dispatchSocketVerbExt, so there is exactly one dispatch table and one
// implementation of every verb. That is not tidiness: the panel's provenance
// rules (an OUT row per request, an ack that never closes a turn) live inside
// those handlers, and a second path into a verb would be a second path around
// the ledger.
//
// What the op layer adds is a SPEC — a name, a class and a schema — so a
// caller that has never heard of magmux can list what it may ask for, and so
// the read-only view token can be enforced on the class rather than on a list
// somebody keeps by hand.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// opSource is the Source stamped on every built-in spec. A plugin registers
// under its own name, so `ops` always says who owns what.
const opSource = "magmux"

// bus returns this magmux's hub, building it and registering the built-in ops
// on first use.
//
// Lazy on purpose. Every unit test in this package builds a Magmux as a struct
// literal and never calls init(), and broadcastEvent is reachable from most of
// them, so a hub that only exists when init() ran would be a nil dereference in
// half the suite. The Once also makes the registration happen exactly once
// however many goroutines race to the first event.
func (m *Magmux) bus() *hub.Hub {
	m.hubOnce.Do(func() {
		if m.hub == nil {
			m.hub = hub.New()
		}
		// Which ops are DELIVERED to a pane rather than merely called. The hub
		// cannot answer that on its own: resolving a `pane` field means knowing
		// that the wire takes an index or a string, that `send` with no pane
		// falls back to the pilot's target, and that ids are sparse.
		m.hub.SetLaneKey(m.laneKeyFor)
		// The streaming port, installed with the hub rather than on first use.
		// A `watch` can arrive at any moment — before a pane has been opened
		// dynamically, before the panel is shown — and a hub with no Watcher
		// answers every one of them `unsupported`.
		m.hub.SetWatcher(m.streamer())
		if err := m.hub.Register(opSource, m.builtinOps()...); err != nil {
			// The op table is a compile-time constant in everything but type,
			// so a failure here is a magmux bug and not a caller's. Say so
			// where a developer will see it and carry on with no ops rather
			// than taking the session down.
			if dbgFile != nil {
				fmt.Fprintf(dbgFile, "registering built-in ops: %v\n", err)
			}
		}
	})
	return m.hub
}

// ── the table ───────────────────────────────────────────────────────────────

// builtinOps is the one place a verb becomes an op. The classes are what the
// view token is enforced on:
//
//   - read observes and changes nothing;
//   - control steers a session (it can open a shell, so it is never a viewer's);
//   - display decorates magmux's own chrome and touches no session.
//
// `pilot` and `agent` are deliberately absent. They are legacy socket-only
// protocols with their own shapes — a pilot's start/finish bookkeeping, an
// agent hook's event stream — and neither is a request another transport
// should be able to make.
func (m *Magmux) builtinOps() []hub.Op {
	return []hub.Op{
		m.op("capabilities", protocol.ClassRead,
			"What this magmux is and what it can be asked for: protocol version, geometry, "+
				"scrollback depth, the verb list and the event list.",
			objectSchema(nil)),

		m.op("list", protocol.ClassRead,
			"Every pane and its state, exactly as the connect-time snapshot and the shutdown "+
				"results report it, so a poller and a subscriber can never be told different things.",
			objectSchema(nil)),

		m.op("ops", protocol.ClassRead,
			"Every op this magmux offers, with its class and argument schema, and the revision "+
				"of that list. Plugins add and remove ops while magmux runs; the revision is how "+
				"a client knows the list it cached is stale.",
			objectSchema(nil)),

		m.op("capture", protocol.ClassRead,
			"Read a pane's screen as text. `lines` keeps the last N rows; `offset` reaches back "+
				"into scrollback, in rows, from the bottom of the live screen.",
			objectSchema(map[string]any{
				"pane":   paneProp,
				"lines":  intProp("Keep only the last N rows. 0 means the whole screen."),
				"offset": intProp("Rows of scrollback to reach back through. 0 is the live screen."),
			})),

		m.op("transcript", protocol.ClassRead,
			"Read a pane's turns from the tool's OWN record on disk — full text, tool inputs and "+
				"tool results — rather than from the screen. Only for a pane magmux is following "+
				"with a controller.",
			objectSchema(map[string]any{
				"pane":  paneProp,
				"lines": intProp("How many of the most recent turns to return."),
			})),

		m.op("open_pane", protocol.ClassControl,
			"Split a pane and run a command in the new half. `cmd` goes to the user's login "+
				"shell exactly as -e does, so pipelines and `cd x && y` work.",
			objectSchema(map[string]any{
				"cmd":    strProp("Command line, run through the login shell."),
				"cwd":    strProp("Working directory. `dir` is accepted as a synonym."),
				"dir":    strProp("Synonym for cwd."),
				"env":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Extra KEY=VALUE entries for the child."},
				"label":  strProp("Short name for this pane in the panel and in list."),
				"target": paneProp,
				"split":  strProp("auto | horizontal | vertical."),
				"ratio":  map[string]any{"type": "number", "description": "First half's share of the space. 0 means half."},
				"focus":  boolProp("Move the keyboard to the new pane."),
			}, "cmd")),

		m.op("close_pane", protocol.ClassControl,
			"Close a pane and reap its child. The id is kept as a tombstone, so every other "+
				"pane keeps the index its caller already knows.",
			objectSchema(map[string]any{
				"pane":  paneProp,
				"force": boolProp("Escalate to SIGKILL rather than waiting."),
			}, "pane")),

		m.op("focus", protocol.ClassControl,
			"Move keyboard focus to a pane.",
			objectSchema(map[string]any{"pane": paneProp}, "pane")),

		m.op("send", protocol.ClassControl,
			"Deliver an instruction to a session: text, then named keys, then Enter. This is "+
				"the controller's path — it is recorded in the control panel as an OUT row and "+
				"tells the pane's controller that a new turn is starting.",
			objectSchema(map[string]any{
				"pane":  paneProp,
				"text":  strProp("The instruction. Sent as a bracketed paste when the pane asked for one."),
				"keys":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Named keys to press after the text, e.g. [\"tab\",\"down\"]."},
				"enter": boolProp("Submit after the text. Defaults to true."),
				"label": strProp("Short tag for the panel's OUT row, e.g. \"step 2/5\"."),
			})),

		// input is built by hand: it is the one built-in that does not wrap a
		// socket verb, because there is no `{"type":"input"}` to wrap. See
		// input.go for why it must not join the verb table.
		m.inputOp(),

		m.op("status", protocol.ClassDisplay,
			"Set magmux's status bar text.",
			objectSchema(map[string]any{"text": strProp("Status bar text. Empty clears it.")})),

		m.op("tint", protocol.ClassDisplay,
			"Tint a pane's border. \"*\" tints every live pane; \"reset\" clears it.",
			objectSchema(map[string]any{
				"pane":  paneProp,
				"color": strProp("Colour name, or \"reset\"."),
			})),

		m.op("overlay", protocol.ClassDisplay,
			"Put a short banner over a pane. Empty text clears it.",
			objectSchema(map[string]any{
				"pane":  paneProp,
				"text":  strProp("Banner text."),
				"style": strProp("Banner style, e.g. \"error\"."),
			})),
	}
}

// op builds one spec plus the wrapper that runs the verb of the same name.
func (m *Magmux) op(name string, class protocol.OpClass, description string, schema json.RawMessage) hub.Op {
	return hub.Op{
		Spec: protocol.OpSpec{
			Name:        name,
			Description: description,
			Schema:      schema,
			Class:       class,
			Source:      opSource,
		},
		Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
			msg, err := opMsg(name, c, args)
			if err != nil {
				return nil, err
			}
			return m.dispatchOp(ctx, msg)
		},
	}
}

// opMsg turns an op's args into the sockMsg the verb already knows how to run.
//
// The verb name and the caller are magmux's to set, never the payload's: Type
// is overwritten so `args` cannot smuggle a different verb, and ID is cleared
// because the op path answers through its return value and a stray id would
// hand the same request two ways to be answered.
func opMsg(verb string, c hub.Caller, args json.RawMessage) (sockMsg, error) {
	var msg sockMsg
	if trimmed := bytes.TrimSpace(args); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			return msg, sockErrf(sockCodeBadRequest, "%s: args must be a JSON object (%v)", verb, err)
		}
	}
	msg.Type = verb
	msg.ID = nil
	msg.caller = c
	return msg, nil
}

// dispatchOp runs one verb on the caller's goroutine and returns its outcome,
// including for a verb whose work outlives the dispatch call.
//
// `send` is the only such verb: its writes are paced across hundreds of
// milliseconds, so on the socket it returns errReplyDeferred and answers later
// through a callback. An op has to answer by returning, so this is where the
// two shapes meet — it waits for that callback rather than spawning a second
// way to reply.
//
// The ctx bounds the WAIT and nothing else. A delivery already under way keeps
// going: it holds no lock, it is the pane's own instruction, and abandoning it
// half-typed would leave a session with part of a command in its prompt. What
// expires is the caller's patience, and the answer says exactly that.
func (m *Magmux) dispatchOp(ctx context.Context, msg sockMsg) (map[string]any, error) {
	type outcome struct {
		result map[string]any
		err    error
	}
	// Buffered, and written with a non-blocking send: if the wait below has
	// already given up, the delivery goroutine must not block on a channel
	// nobody is reading.
	answered := make(chan outcome, 1)
	// The verb's own work gets this ctx; the wait below gets it too, but they
	// are different things. On a lane, ctx is the item's, which Quiesce cancels
	// — so a `send` reached through `call` stops between keystrokes exactly as
	// the direct verb does, instead of typing on into a session teardown has
	// already reported on.
	msg.runCtx = ctx
	result, err := m.dispatchSocketVerbExt(msg, func(r map[string]any, e error) {
		select {
		case answered <- outcome{r, e}:
		default:
		}
	})
	if !errors.Is(err, errReplyDeferred) {
		return result, err
	}
	select {
	case o := <-answered:
		return o.result, o.err
	case <-ctx.Done():
		return nil, sockErrf(sockCodeTimeout,
			"%s is still being delivered to the pane; the wait for its outcome expired", msg.Type)
	}
}

// ── schema helpers ──────────────────────────────────────────────────────────
//
// Small and local rather than shared with mcp's: that package is a separate
// process that must not import mux, and these produce raw JSON for the wire
// while its produce map[string]any for a tool definition.

func objectSchema(props map[string]any, required ...string) json.RawMessage {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	raw, err := json.Marshal(s)
	if err != nil {
		// Unreachable: every value above is a literal of a marshalable type.
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// paneProp is the shared `pane` schema. anyOf rather than a two-element type
// array, because several clients validate the latter poorly.
var paneProp = map[string]any{
	"anyOf":       []any{map[string]any{"type": "integer"}, map[string]any{"type": "string"}},
	"description": "Pane index, or \"*\" where the verb takes a fan-out.",
}
