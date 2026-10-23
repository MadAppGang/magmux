package obsview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// The WIRE: the document `magmux agents --json`, the socket `agents` verb and
// MCP's list_agents carry, with its exact tags, omitempty use and enum
// spellings. It moved here from internal/obs when claudestate replaced the
// observation layer (claudestate architecture §2.2): the library's model has
// no json tags and no JSON methods (its guard G6), because the wire belongs to
// each host. [FromModel] is the one mapping from the library's session to this
// one.
//
// One member is new, `job` (omitempty), which only a job-keyed row carries
// (D6). Every other member, its tag and its spelling is the pre-library
// wire's, and obsview/golden_test.go holds every difference to a citation in
// wirechanges.go.

// Session is one observed agent session, as one wire row.
//
// Every field is either required and always meaningful, or optional and
// spelled so that "undetermined" is representable: a pointer whose nil means
// no opinion, or an enum whose zero value is Unknown and marshals to null.
//
// The three axes are deliberately separate and none is derivable from the
// others: Status is what the session is DOING, Liveness whether it still
// EXISTS, and Fault what has gone WRONG.
type Session struct {
	// ID is the tool's own session identifier, kept verbatim. "" on a row the
	// library kept with a ProblemMissingField (D11): the tag is not
	// omitempty, as it never was.
	ID string `json:"id"`

	// PID is the process the session runs as; 0 for a background job no
	// registry record claims.
	PID int `json:"pid"`

	// Job is the background job a job-keyed row is (D6): the record's
	// daemonShort, else its directory. Absent on every other row, so a
	// consumer that never heard of jobs sees the document it always saw.
	Job string `json:"job,omitempty"`

	// PaneID is the magmux pane the session is running in. nil when the
	// session is not one of magmux's panes, which is not hypothetical:
	// discovery sees sessions magmux did not start.
	PaneID *int `json:"paneId"`

	// Name is the session's display name, and PreviousNames the ones it has
	// had before, in order. PreviousNameDetail is PARALLEL to PreviousNames —
	// same length, same order — and carries when each name stopped being
	// current and the session id it was recorded under. previousNames keeps
	// its shape (an array of strings) and the detail rides beside it as an
	// additive key, so a consumer that knows nothing of it is unaffected.
	Name               string         `json:"name,omitempty"`
	PreviousNames      []string       `json:"previousNames,omitempty"`
	PreviousNameDetail []PreviousName `json:"previousNameDetail,omitempty"`

	// LaunchDir is the directory the tool was LAUNCHED in, as magmux knows
	// it from the pane it spawned (the launchdir host stage). Never the
	// record's cwd, which is the LIVE directory.
	LaunchDir string `json:"launchDir,omitempty"`

	// Status is magmux's word for the effective status, StatusRaw the tool's
	// own word kept verbatim. A status magmux does not have a word for is
	// null here with the raw word beside it.
	Status    Status `json:"status"`
	StatusRaw string `json:"statusRaw,omitempty"`

	// Confidence names which tier the status came from.
	Confidence Tier `json:"confidence"`

	// Conflict is the losing tier's reading, when the two disagreed.
	Conflict *Conflict `json:"conflict,omitempty"`

	// StatusSince is when the session entered its status, as a record states
	// it; never a file's mtime.
	StatusSince *time.Time `json:"statusSince"`

	// Waiting is what a waiting session is blocked on, WaitingRaw the tool's
	// own word.
	Waiting    WaitingReason `json:"waitingOn"`
	WaitingRaw string        `json:"waitingOnRaw,omitempty"`

	// ProcessAlive and PaneAlive are the two gates, kept apart because they
	// fail apart.
	ProcessAlive Liveness `json:"processAlive"`
	PaneAlive    Liveness `json:"paneAlive"`

	// SubagentCount and Color are the session's own transcript's facts; nil
	// when no transcript was located and read.
	SubagentCount *int    `json:"subagentCount"`
	Color         *string `json:"color"`

	Attention Attention `json:"attention"`
	Fault     Fault     `json:"fault"`
}

// InStatusFor reports how long the session has been in its status as of now,
// and whether that is known at all.
func (s Session) InStatusFor(now time.Time) (time.Duration, bool) {
	if s.StatusSince == nil {
		return 0, false
	}
	return now.Sub(*s.StatusSince), true
}

// Clone returns a deep copy: no pointer or slice is shared with s.
func (s Session) Clone() Session {
	c := s
	c.PaneID = clonePtr(s.PaneID)
	c.PreviousNames = slices.Clone(s.PreviousNames)
	if s.PreviousNameDetail != nil {
		c.PreviousNameDetail = make([]PreviousName, len(s.PreviousNameDetail))
		for i, p := range s.PreviousNameDetail {
			p.Until = clonePtr(p.Until)
			c.PreviousNameDetail[i] = p
		}
	}
	c.StatusSince = clonePtr(s.StatusSince)
	c.SubagentCount = clonePtr(s.SubagentCount)
	c.Color = clonePtr(s.Color)
	c.Attention.Needed = clonePtr(s.Attention.Needed)
	c.Conflict = clonePtr(s.Conflict)
	c.Fault.APIErrors = clonePtr(s.Fault.APIErrors)
	return c
}

// PreviousName is one name a session was known by before its current one.
type PreviousName struct {
	Name string `json:"name"`
	// Until is when it stopped being current; nil when the tool did not say.
	Until *time.Time `json:"until"`
	// Session is the session id the name was recorded under, when the tool
	// recorded one.
	Session string `json:"session,omitempty"`
}

// Attention is whether a human needs to look at a session. Needed has three
// states and nil is one of them: a flag never determined is not a flag
// determined to be false.
type Attention struct {
	Needed *bool  `json:"needed"`
	Reason string `json:"reason,omitempty"`
}

// Fault is what has gone wrong, ORTHOGONAL to Status. APIErrors nil means
// nobody counted; Detail is the last error's text and the window it was
// counted in, in magmux's words.
type Fault struct {
	APIErrors *int   `json:"apiErrors"`
	Detail    string `json:"detail,omitempty"`
}

// Conflict is the losing tier's reading when two tiers disagreed.
type Conflict struct {
	Tier   Tier   `json:"tier"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Fleet is the set of sessions observed at ONE instant. The epoch belongs to
// the document, not to each session.
type Fleet struct {
	Epoch    time.Time `json:"epoch"`
	Sessions []Session `json:"sessions"`
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
	return c
}

// ── the wire enums ──────────────────────────────────────────────────────────
//
// Index 0 of every enum is "no opinion", marshals to null and to nothing else,
// and reads back from null. The names are the WIRE vocabulary and deliberately
// mirror Claude Code's own spelling: they are HOMOGRAPHS of Claude's words,
// not copies, and if Claude renames a value the library's decoder absorbs it
// and these stay put. No word was added when the library's vocabulary grew to
// tt's union (architecture A4): FromModel maps the new words onto these.

// Status is what an observed session is doing, in magmux's words.
type Status uint8

const (
	StatusUnknown Status = iota
	StatusIdle
	StatusBusy
	StatusShell
	StatusWaiting
)

var statusEnum = enumSpec{"Status", []string{"unknown", "idle", "busy", "shell", "waiting"}}

func (s Status) String() string               { return statusEnum.name(int(s)) }
func (s Status) MarshalJSON() ([]byte, error) { return statusEnum.marshal(int(s)) }
func (s *Status) UnmarshalJSON(b []byte) error {
	i, err := statusEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = Status(i)
	return nil
}

// WaitingReason is what a waiting session is blocked on. "none" stays in the
// enum so that a document carrying it still decodes; nothing produces it.
type WaitingReason uint8

const (
	WaitingReasonUnknown WaitingReason = iota
	WaitingReasonNone
	WaitingReasonInput
	WaitingReasonPermission
)

var waitingReasonEnum = enumSpec{"WaitingReason", []string{"unknown", "none", "input", "permission"}}

func (w WaitingReason) String() string               { return waitingReasonEnum.name(int(w)) }
func (w WaitingReason) MarshalJSON() ([]byte, error) { return waitingReasonEnum.marshal(int(w)) }
func (w *WaitingReason) UnmarshalJSON(b []byte) error {
	i, err := waitingReasonEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*w = WaitingReason(i)
	return nil
}

// Liveness is a tri-state answer to "does this still exist?".
type Liveness uint8

const (
	LivenessUnknown Liveness = iota
	LivenessYes
	LivenessNo
)

var livenessEnum = enumSpec{"Liveness", []string{"unknown", "yes", "no"}}

func (l Liveness) String() string               { return livenessEnum.name(int(l)) }
func (l Liveness) MarshalJSON() ([]byte, error) { return livenessEnum.marshal(int(l)) }
func (l *Liveness) UnmarshalJSON(b []byte) error {
	i, err := livenessEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*l = Liveness(i)
	return nil
}

// Tier is a CONFIDENCE tier: where a status came from.
type Tier uint8

const (
	TierUnknown Tier = iota
	TierInferred
	TierAuthoritative
)

var tierEnum = enumSpec{"Tier", []string{"unknown", "inferred", "authoritative"}}

func (t Tier) String() string               { return tierEnum.name(int(t)) }
func (t Tier) MarshalJSON() ([]byte, error) { return tierEnum.marshal(int(t)) }
func (t *Tier) UnmarshalJSON(b []byte) error {
	i, err := tierEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*t = Tier(i)
	return nil
}

// enumSpec is the one implementation of the wire enums' shape.
type enumSpec struct {
	typeName string
	names    []string
}

func (e enumSpec) marshal(i int) ([]byte, error) {
	if i == 0 {
		return []byte("null"), nil
	}
	if i < 0 || i >= len(e.names) {
		return nil, fmt.Errorf("obsview: %s: value %d is out of range", e.typeName, i)
	}
	return json.Marshal(e.names[i])
}

// unmarshal reads null as index 0. An unrecognised word is an error rather
// than a silent Unknown: a decoder that quietly turned a new word into "no
// opinion" would hide the one thing a reader needs to notice.
func (e enumSpec) unmarshal(b []byte) (int, error) {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return 0, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return 0, fmt.Errorf("obsview: %s: %w", e.typeName, err)
	}
	for i, n := range e.names {
		if i != 0 && n == s {
			return i, nil
		}
	}
	return 0, fmt.Errorf("obsview: %s: unrecognised value %q", e.typeName, s)
}

func (e enumSpec) name(i int) string {
	if i < 0 || i >= len(e.names) {
		return fmt.Sprintf("%s(%d)", e.typeName, i)
	}
	return e.names[i]
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
