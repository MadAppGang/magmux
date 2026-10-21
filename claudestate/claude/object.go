package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// ErrTorn is a record whose bytes are not one JSON object: a half-written
// file, a file cut at a read bound, an empty file, or a JSON value of another
// kind. It means TORN, not dead (C8). The caller retries once, then keeps the
// last good value.
var ErrTorn = errors.New("claude: not a JSON object (torn or truncated)")

// object is one decoded JSON object, read BY NAME. Its values stay raw until a
// field asks, so a key of the wrong type costs that key and nothing else.
type object struct {
	path     string // the record's path, relative to its tree, for Problems
	m        map[string]json.RawMessage
	problems []model.Problem
}

// decodeObject parses b as exactly one JSON object. Anything else, including
// JSON null and trailing garbage, is ErrTorn.
func decodeObject(path string, b []byte) (*object, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil, ErrTorn
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, ErrTorn
	}
	if dec.More() {
		return nil, ErrTorn
	}
	return &object{path: path, m: m}, nil
}

func (o *object) problem(kind model.ProblemKind, field, detail string) {
	o.problems = append(o.problems, model.Problem{
		Scope: model.ScopeRecord, Kind: kind, Path: o.path, Field: field, Detail: detail,
	})
}

// present reports a key that is present and not JSON null.
func (o *object) present(key string) bool {
	raw, ok := o.m[key]
	return ok && !isNull(raw)
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// str reads a string. Absent and null are "". A value of another JSON type is
// a ProblemUndecoded on that key, and the field is left zero.
func (o *object) str(key string) string {
	raw, ok := o.m[key]
	if !ok || isNull(raw) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		o.problem(model.ProblemUndecoded, key, "is "+jsonKind(raw)+", not text")
		return ""
	}
	return s
}

// strPtr is str with "" as nil.
func (o *object) strPtr(key string) *string {
	if s := o.str(key); s != "" {
		return &s
	}
	return nil
}

// int64Of reads an integral number. ok is false when absent or null; a value
// of another type, or a fraction, is a ProblemUndecoded.
func (o *object) int64Of(key string) (int64, bool) {
	raw, ok := o.m[key]
	if !ok || isNull(raw) {
		return 0, false
	}
	n, isInt := rawInt64(raw)
	if !isInt {
		o.problem(model.ProblemUndecoded, key, "is "+jsonKind(raw)+", not a whole number")
		return 0, false
	}
	return n, true
}

// millis reads an epoch-millisecond instant through DecodeInstantMillis.
func (o *object) millis(key string) *time.Time {
	ms, ok := o.int64Of(key)
	if !ok {
		return nil
	}
	t, err := DecodeInstantMillis(ms)
	if err != nil {
		o.problem(model.ProblemImplausibleInstant, key,
			fmt.Sprintf("%d is not a plausible epoch-millisecond value", ms))
		return nil
	}
	return t
}

// rfc3339 reads an RFC3339 instant through DecodeInstantString.
func (o *object) rfc3339(key string) *time.Time {
	s := o.str(key)
	if s == "" {
		return nil
	}
	t, err := DecodeInstantString(s)
	switch {
	case errors.Is(err, ErrImplausibleInstant):
		o.problem(model.ProblemImplausibleInstant, key, fmt.Sprintf("%q is outside 2001..5138", s))
		return nil
	case err != nil:
		o.problem(model.ProblemUndecoded, key, fmt.Sprintf("%q is not an RFC3339 instant", s))
		return nil
	}
	return t
}

// stringList reads a list of strings, skipping elements that are not text.
func (o *object) stringList(key string) []string {
	raw, ok := o.m[key]
	if !ok || isNull(raw) {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		o.problem(model.ProblemUndecoded, key, "is "+jsonKind(raw)+", not a list")
		return nil
	}
	var out []string
	for _, it := range items {
		var s string
		if json.Unmarshal(it, &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// rawInt64 decodes an integral JSON number.
func rawInt64(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// rawString decodes a JSON string; ok is false for any other kind.
func rawString(raw json.RawMessage) (string, bool) {
	if isNull(raw) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// isTrue reports the JSON literal true. "true", 1 and {} are not true: a
// vendor that starts writing a flag as a string has changed the format.
func isTrue(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "true"
}

// jsonKind names a raw value's kind for a Problem. It never includes the
// value: a value is quoted, if at all, by the caller.
func jsonKind(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "empty"
	}
	switch raw[0] {
	case 'n':
		return "null"
	case '"':
		return "text"
	case 't', 'f':
		return "a boolean"
	case '[':
		return "a list"
	case '{':
		return "an object"
	default:
		return "a number"
	}
}

// TopLevelKeys lists the keys of one JSON object, sorted. ok is false when b
// is not a JSON object. It is how claude/doctor sees the keys this build does
// NOT read, which no typed decoder can show it.
func TopLevelKeys(b []byte) (keys []string, ok bool) {
	o, err := decodeObject("", b)
	if err != nil {
		return nil, false
	}
	for k := range o.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, true
}
