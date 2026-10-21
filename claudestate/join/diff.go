package join

import (
	"reflect"
	"slices"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// ChangeType is what happened to a key between two documents.
type ChangeType uint8

const (
	ChangeUnknown ChangeType = iota
	ChangeAdded
	ChangeChanged
	ChangeRemoved
)

var changeTypeNames = []string{"unknown", "added", "changed", "removed"}

func (c ChangeType) String() string {
	if int(c) < len(changeTypeNames) {
		return changeTypeNames[c]
	}
	return "ChangeType(?)"
}

// Change is one key's difference between two documents. Fields names the
// model.Session fields that differ, in struct order, for ChangeChanged.
type Change struct {
	Type   ChangeType
	Key    model.Key
	Fields []string
	Reason string
}

// ReasonRemoved is a removal's reason: no record claims the key any more.
// Removal needs that positive evidence; a failure never removes (C14).
const ReasonRemoved = "no record claims this key"

// Diff is the per-key difference between two documents (FR16, tt:diff.go). It
// is keyed by model.Key, so two sessions sharing a session id never shadow
// each other, and it is in key order, so two runs produce identical output.
//
// The model carries no clock-derived field (hosts derive ages at encode time),
// so every field is compared and a quiet machine produces no change.
func Diff(prev, next model.Fleet) []Change {
	before := map[model.Key]model.Session{}
	for _, s := range prev.Sessions {
		before[s.Key] = s
	}
	after := map[model.Key]model.Session{}
	for _, s := range next.Sessions {
		after[s.Key] = s
	}
	var out []Change
	for k, s := range after {
		old, had := before[k]
		if !had {
			out = append(out, Change{Type: ChangeAdded, Key: k})
			continue
		}
		if fields := ChangedFields(old, s); len(fields) > 0 {
			out = append(out, Change{Type: ChangeChanged, Key: k, Fields: fields})
		}
	}
	for k := range before {
		if _, still := after[k]; !still {
			out = append(out, Change{Type: ChangeRemoved, Key: k, Reason: ReasonRemoved})
		}
	}
	slices.SortFunc(out, func(a, b Change) int { return a.Key.Compare(b.Key) })
	return out
}

var sessionFields = func() []string {
	t := reflect.TypeOf(model.Session{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if name := t.Field(i).Name; name != "Key" {
			out = append(out, name)
		}
	}
	return out
}()

// ChangedFields names the fields that differ between two observations of one
// session, in struct order. A nil slice and an empty one differ, as they do on
// a wire that preserves the distinction; two instants are compared as
// instants.
func ChangedFields(a, b model.Session) []string {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	var out []string
	for _, name := range sessionFields {
		if !equalValue(va.FieldByName(name), vb.FieldByName(name)) {
			out = append(out, name)
		}
	}
	return out
}

var timeType = reflect.TypeOf(time.Time{})

func equalValue(a, b reflect.Value) bool {
	switch {
	case a.Type() == timeType:
		return a.Interface().(time.Time).Equal(b.Interface().(time.Time))
	case a.Kind() == reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return equalValue(a.Elem(), b.Elem())
	case a.Kind() == reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !equalValue(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case a.Kind() == reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if !equalValue(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}

// Ages are a session's durations at an instant (§3.4 consumers table). The
// library publishes instants only; hosts derive ages at encode time (FR16).
type Ages struct {
	// InStatus is now − StatusSince; nil when the status has no instant.
	InStatus *time.Duration
	// RecordAge is now − Job.UpdatedAt (tt's recordAgeMs); nil with no job.
	RecordAge *time.Duration
	// BlockedFor is InStatus while WaitingApplies(), else nil: a view of
	// InStatus, never a second measurement.
	BlockedFor *time.Duration
}

// AgesAt derives a session's ages at now.
func AgesAt(s model.Session, now time.Time) Ages {
	var a Ages
	if s.StatusSince != nil {
		d := now.Sub(*s.StatusSince)
		a.InStatus = &d
		if s.WaitingApplies() {
			b := d
			a.BlockedFor = &b
		}
	}
	if s.Job != nil && s.Job.UpdatedAt != nil {
		d := now.Sub(*s.Job.UpdatedAt)
		a.RecordAge = &d
	}
	return a
}
