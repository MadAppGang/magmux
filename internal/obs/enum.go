package obs

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The enums in this package share one shape: index 0 is "no opinion" and
// every other index is a determined value. enumSpec is the one place that
// shape is implemented, so they cannot drift apart.
//
// Most of them are WIRE enums — [Status], [WaitingReason], [Liveness],
// [Tier] — and marshal. [ShellMark] shares the shape and uses only name(): it
// is evidence the inferred tier weighs rather than a fact the document
// publishes, and its own declaration says why.
//
// The NAMES are the core's own WIRE vocabulary, settled 2026-09-18: a
// determined value marshals to its name, an undetermined one to JSON null.
// Each table says at its own declaration whether it mirrors the observed
// tool's spelling or normalises it — see statusEnum and waitingReasonEnum,
// which differ on exactly that and for stated reasons.
//
// A name being the same word Claude uses does not make it Claude's. The two
// are homographs with independent lifetimes, and NFR6 is enforced against
// DECODE SITES rather than against spellings for that reason; see
// adapters.TestOnlySchemaFileDecodesClaudeStrings.
//
// Index 0 is the one name that is not a wire value. It is a display spelling
// for String, and unmarshal does not accept it: null is the single encoding of
// "undetermined", and a second spelling of it would be a second thing a
// consumer has to test for.
type enumSpec struct {
	typeName string
	names    []string
}

// marshal writes JSON null for index 0 and the value's name otherwise.
func (e enumSpec) marshal(i int) ([]byte, error) {
	if i == 0 {
		return []byte("null"), nil
	}
	if i < 0 || i >= len(e.names) {
		return nil, fmt.Errorf("obs: %s: value %d is out of range", e.typeName, i)
	}
	return json.Marshal(e.names[i])
}

// unmarshal is the inverse. JSON null is index 0.
//
// An unrecognised name is an ERROR, not a silent zero. This package's own
// document is its own contract: a name that does not decode here is a defect
// in something magmux wrote, not a tool changing its format underneath us.
// Degrading to "no opinion" is what the adapters do with FOREIGN data, and
// that leniency stops at the adapter boundary on purpose.
func (e enumSpec) unmarshal(b []byte) (int, error) {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return 0, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return 0, fmt.Errorf("obs: %s: %w", e.typeName, err)
	}
	// Index 0 is skipped deliberately: its name is a display spelling that this
	// package never writes, and accepting it would put two encodings of "no
	// opinion" on the wire for a consumer to have to know about.
	for i, n := range e.names {
		if i == 0 {
			continue
		}
		if n == s {
			return i, nil
		}
	}
	return 0, fmt.Errorf("obs: %s: unrecognised value %q", e.typeName, s)
}

// name is the String() half. A value outside the enum prints its ordinal
// rather than panicking or claiming to be Unknown, so a corrupt value is
// visible as a corrupt value.
func (e enumSpec) name(i int) string {
	if i < 0 || i >= len(e.names) {
		return fmt.Sprintf("%s(%d)", e.typeName, i)
	}
	return e.names[i]
}

// Ptr returns a pointer to v.
//
// Every optional field in this package is a pointer whose nil means
// "undetermined", so constructing one is common enough to deserve a helper:
// obs.Ptr(0) is a determined zero, and nil is no opinion.
func Ptr[T any](v T) *T { return &v }

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
