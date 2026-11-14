package model

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// Fleet is the set of sessions observed at ONE instant. The epoch belongs to
// the document, not to each session: a consumer comparing two documents must
// compare when they were taken.
type Fleet struct {
	Epoch    time.Time
	Sessions []Session
	// Sources is always exactly four entries, in this order: Registry, Jobs,
	// Agents, Watch (§3.1).
	Sources []SourceReport
}

// Clone returns a deep copy of the document.
func (f Fleet) Clone() Fleet {
	c := f
	if f.Sessions != nil {
		c.Sessions = make([]Session, len(f.Sessions))
		for i, s := range f.Sessions {
			c.Sessions[i] = s.Clone()
		}
	}
	if f.Sources != nil {
		c.Sources = make([]SourceReport, len(f.Sources))
		for i, r := range f.Sources {
			r.At = clonePtr(r.At)
			c.Sources[i] = r
		}
	}
	return c
}

// Sort orders the sessions in place by PID, then Key.SessionID, then Key.Job,
// then Key.Orphan. On today's data that is today's order (PID, then id), and
// the two key fields after it make it total.
func (f Fleet) Sort() {
	slices.SortStableFunc(f.Sessions, func(a, b Session) int {
		if c := cmp.Compare(a.PID, b.PID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Key.SessionID, b.Key.SessionID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Key.Job, b.Key.Job); c != 0 {
			return c
		}
		return cmp.Compare(a.Key.Orphan, b.Key.Orphan)
	})
}

// SourceReport is how one source fared in the cycle that built a document.
type SourceReport struct {
	ID     SourceID
	State  SourceState
	At     *time.Time
	Detail string
}

// Problem is something a cycle could not do, or found wrong. A problem is
// reported, never swallowed: the way this subsystem fails is silence.
type Problem struct {
	Scope Scope
	Kind  ProblemKind
	// Path is the file or directory, relative to its tree; "" for a
	// source-level problem. A watch problem carries the absolute path it
	// armed, because the watcher knows no tree.
	Path string
	// Key is the session the problem is about, when known.
	Key *Key
	// Field is the Claude key for a field-level problem (D4, D11).
	Field string
	// PIDs: every claimant pid, ascending, for a duplicate claim.
	PIDs   []int
	Detail string
}

// Error renders the problem as "<scope>: <kind>[: <key>][: <path>][: field <f>][: <detail>]".
func (p Problem) Error() string {
	parts := []string{p.Scope.String(), p.Kind.String()}
	if p.Key != nil {
		parts = append(parts, p.Key.String())
	}
	if p.Path != "" {
		parts = append(parts, p.Path)
	}
	if p.Field != "" {
		parts = append(parts, "field "+p.Field)
	}
	if p.Detail != "" {
		parts = append(parts, p.Detail)
	}
	return strings.Join(parts, ": ")
}

// WatchTarget is one object the watcher is asked to arm.
type WatchTarget struct {
	Path string // absolute OS path
	Dir  bool
	Kind TargetKind
}
