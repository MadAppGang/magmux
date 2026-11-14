package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// AgentsEntry is one entry of `claude agents --json`.
type AgentsEntry struct {
	SessionID, ID string
	PID           *int // interactive entries only; nil is how a background entry is recognised
	Kind          model.Kind
	KindRaw       string
	// Status is the interactive entry's registry-vocabulary word, State the
	// background entry's job-vocabulary word. They are DIFFERENT fields and
	// are never conflated.
	Status     model.Status
	StatusRaw  string
	State      model.Status
	StateRaw   string
	Waiting    model.WaitingReason
	WaitingRaw string
	Name, CWD  string
	StartedAt  *time.Time
}

// IsInteractiveEntry is tt's isInteractiveEntry: kind interactive, or no kind
// and a pid, which every measured interactive entry has and no background one.
func IsInteractiveEntry(e AgentsEntry) bool {
	switch e.KindRaw {
	case ValueKindInteractive:
		return true
	case "":
		return e.PID != nil
	default:
		return false
	}
}

// AgentsListing is one decoded listing at the instant the host took it.
type AgentsListing struct {
	Entries []AgentsEntry
	At      time.Time
}

// Lists reports whether an entry of either kind carries sessionID. An empty id
// is never listed.
func (l AgentsListing) Lists(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	for _, e := range l.Entries {
		if e.SessionID == sessionID {
			return true
		}
	}
	return false
}

// DecodeAgents decodes the listing, which is a FLAT ARRAY.
//
// "[]" is a valid empty listing; bytes that are not a JSON array are an error
// (C12). An empty listing that claimed success would be positive evidence of
// death for every background job on the machine, so malformed is never empty.
// An entry with no sessionId is skipped, never fatal: one malformed entry must
// not blank the other 38. Entries come back sorted by session id, because every
// "first in a fixed order wins" rule downstream needs the order to be ours.
func DecodeAgents(raw []byte, at time.Time) (AgentsListing, []model.Problem, error) {
	var items []json.RawMessage
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return AgentsListing{}, nil, fmt.Errorf("claude: the agents listing is not a JSON array")
	}
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return AgentsListing{}, nil, fmt.Errorf("claude: the agents listing is not a JSON array: %w", err)
	}
	l := AgentsListing{At: at, Entries: make([]AgentsEntry, 0, len(items))}
	var problems []model.Problem
	for i, it := range items {
		o, err := decodeObject(fmt.Sprintf("[%d]", i), it)
		if err != nil {
			continue
		}
		e := AgentsEntry{SessionID: o.str(KeySessionID)}
		if e.SessionID == "" {
			continue
		}
		e.ID = o.str(KeyAgentID)
		if n, ok := o.int64Of(KeyPID); ok {
			v := int(n)
			e.PID = &v
		}
		e.KindRaw = o.str(KeyKind)
		switch e.KindRaw {
		case ValueKindInteractive:
			e.Kind = model.KindInteractive
		case ValueKindBackground:
			e.Kind = model.KindBackground
		}
		e.StatusRaw = o.str(KeyStatus)
		e.StateRaw = o.str(KeyState)
		e.WaitingRaw = o.str(KeyWaitingFor)
		if e.StatusRaw != "" {
			e.Status, e.Waiting = MapRegistryStatus(e.StatusRaw, e.WaitingRaw)
		}
		e.State = MapJobState(e.StateRaw)
		e.Name = o.str(KeyName)
		e.CWD = o.str(KeyCWD)
		e.StartedAt = o.millis(KeyStartedAt)
		problems = append(problems, o.problems...)
		l.Entries = append(l.Entries, e)
	}
	sort.SliceStable(l.Entries, func(a, b int) bool { return l.Entries[a].SessionID < l.Entries[b].SessionID })
	return l, problems, nil
}
