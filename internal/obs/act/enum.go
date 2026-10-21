package act

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The three enums in this package share the shape obsview's wire enums have and
// this file is the one place it is implemented here, so [Tier], [Stance] and
// [Requester] cannot drift apart: index 0 is "nothing was determined", it
// marshals to JSON null, and unmarshal does not accept its display name back.
//
// It is a SECOND COPY of obsview's enumSpec, deliberately and with its cost
// stated. The alternative is exporting that shape from obsview, which would
// make a helper nobody outside this layer should use part of the wire
// package's API — and the wire package is the one whose surface is most worth
// keeping small, because every surface imports it.
// Forty lines of duplication against a permanent API commitment is the cheaper
// side of that trade, and the two copies are pinned to each other by
// TestTheEnumShapeMatchesTheCoreVocabulary.
//
// The zero value of every enum here REFUSES rather than permits, which is the
// difference that matters between this copy and the one it mirrors: on the wire an
// unknown value means "nobody determined this fact", and here it means
// "nothing authorised this action".
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
		return nil, fmt.Errorf("act: %s: value %d is out of range", e.typeName, i)
	}
	return json.Marshal(e.names[i])
}

// unmarshal is the inverse. JSON null is index 0, and an unrecognised name is
// an ERROR rather than a silent zero: this package's own records are its own
// contract, and a name that does not decode here is a defect in something
// magmux wrote rather than a foreign tool changing its format.
func (e enumSpec) unmarshal(b []byte) (int, error) {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return 0, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return 0, fmt.Errorf("act: %s: %w", e.typeName, err)
	}
	// Index 0 is skipped deliberately: its name is a display spelling this
	// package never writes, and accepting it would put two encodings of
	// "nothing was determined" on the wire for a consumer to have to know
	// about.
	for i, n := range e.names {
		if i == 0 {
			continue
		}
		if n == s {
			return i, nil
		}
	}
	return 0, fmt.Errorf("act: %s: unrecognised value %q", e.typeName, s)
}

// accepted is every name unmarshal accepts, comma-separated, for an error that
// tells a person what to write instead. Index 0 is left out for the reason
// unmarshal skips it. Built from names rather than typed beside them, so the
// advice cannot drift from what is accepted — which it did, once; see
// parseTier.
func (e enumSpec) accepted() string {
	if len(e.names) < 2 {
		return ""
	}
	return strings.Join(e.names[1:], ", ")
}

// name is the String() half. A value outside the enum prints its ordinal
// rather than panicking or claiming to be the zero value, so a corrupt value
// is visible as a corrupt value.
func (e enumSpec) name(i int) string {
	if i < 0 || i >= len(e.names) {
		return fmt.Sprintf("%s(%d)", e.typeName, i)
	}
	return e.names[i]
}
