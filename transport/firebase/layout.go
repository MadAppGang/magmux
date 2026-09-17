package firebase

// The database layout, and the one rule that shapes it.
//
// RTDB KEYS CANNOT CONTAIN `.` `$` `#` `[` `]` `/`, and a value may not nest
// more than 32 levels. magmux's three free-form payloads all break that sooner
// or later: an op's JSON Schema can hold `$ref`, a plugin's event data is
// whatever the plugin says, and an op's result is whatever the op returns. So
// all three are stored as JSON STRINGS. A reader parses one field instead of
// walking a subtree, and no plugin can make magmux's mirror unwritable by
// naming a key with a dollar in it.
//
// Row keys are `r0..rN` and event keys are `e000000000042`, never arrays. RTDB
// turns a contiguous integer-keyed object into a JSON array on read, which
// silently changes a client's parse the moment a row goes missing; a letter
// prefix makes the key a string forever.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MadAppGang/magmux/protocol"
)

// sessionPath is `{root}/hosts/{host}/sessions/{sid}` — the node every write in
// this package is relative to. Both segments have been through validSegment, so
// neither can hold a `/`.
func sessionPath(root, host, sid string) string {
	return root + "/hosts/" + host + "/sessions/" + sid
}

// ownersPath is where the owner list is mirrored for the security rules to
// read. It sits ABOVE the session on purpose: the rules have to resolve an
// owner before they know which session a write is for, and a session-scoped
// owner list would let a forged session define its own owners.
func ownersPath(root, host string) string {
	return root + "/hosts/" + host + "/owners"
}

// paneKey is `p{n}`. Same reason as the row keys: ids are sparse after a close,
// and a sparse integer-keyed object is the one shape RTDB will not turn into an
// array — but relying on sparseness for that would make the format depend on
// whether a pane had been closed yet.
func paneKey(id int) string { return fmt.Sprintf("p%d", id) }

// rowKey is `r{y}`.
func rowKey(y int) string { return fmt.Sprintf("r%d", y) }

// eventKey is `e` plus a 12-digit zero-padded sequence, so the ring's keys sort
// lexicographically in the same order they were written. RTDB orders children
// by key, so this is what makes "the last 200 events" a range query rather than
// a client-side sort.
func eventKey(seq uint64) string { return fmt.Sprintf("e%012d", seq) }

// opKey is how an op name becomes an RTDB key.
//
// A built-in is already legal (`open_pane`); a plugin op is `ticket.run_ticket`,
// and the dot is one of the six characters a key may not contain. The MCP
// spelling `ticket__run_ticket` already exists for exactly this problem on
// another transport, so it is reused rather than invented again — one mapping,
// documented in protocol, and a client that knows one knows both.
func opKey(name string) string {
	if plugin, op, ok := protocol.SplitQualifiedOp(name); ok {
		return protocol.ToolName(plugin, op)
	}
	return name
}

// jsonString renders a value as the JSON STRING the layout stores. A marshal
// failure yields "" rather than an error: the alternative is to drop the whole
// flush because one plugin returned something unmarshallable, and an empty
// field is a visible, local failure.
func jsonString(v any) string {
	if v == nil {
		return ""
	}
	if raw, ok := v.(json.RawMessage); ok {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// serverTimestamp is RTDB's own clock. Used for `startedAt` and every `at`,
// because a session's machine may be minutes off and a reader comparing a
// heartbeat against ITS own now needs both sides on one clock.
func serverTimestamp() map[string]any { return map[string]any{".sv": "timestamp"} }

// sanitizeKey reports whether s can be an RTDB key at all. It is used on the
// one key that comes from OUTSIDE — the pushId of an inbound command — because
// a pushId is echoed straight back into a `results/{pushId}` path, and a
// pushId containing a `/` would be a path traversal into another session's
// subtree.
func sanitizeKey(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if strings.ContainsAny(s, ".$#[]/") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}
