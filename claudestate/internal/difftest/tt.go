package difftest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// ttDoc is the part of tt's `tt sessions --json` snapshot (schema 1) the
// projection reads. Members the projection does not name are one side only
// and are deliberately not decoded (§8.1's last row).
type ttDoc struct {
	ObservedAt string      `json:"observedAt"`
	Sessions   []ttSession `json:"sessions"`
}

type ttTmux struct {
	Raw     string `json:"raw"`
	Session string `json:"session"`
	Window  string `json:"window"`
	Pane    string `json:"pane"`
}

type ttTranscript struct {
	Path          string `json:"path"`
	AgentColor    string `json:"agentColor"`
	ColorFound    bool   `json:"colorFound"`
	APIErrors     int    `json:"apiErrors"`
	Subagents     *int   `json:"subagents"`
	Known         bool   `json:"subagentsKnown"`
	ContextTokens *int64 `json:"contextTokens"`
	Model         string `json:"model"`
	Compactions   int    `json:"compactions"`
}

type ttSession struct {
	SessionID   string   `json:"sessionId"`
	ShortID     *string  `json:"shortId"`
	PID         *int     `json:"pid"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	NameSource  string   `json:"nameSource"`
	FormerNames []string `json:"formerNames"`
	CWD         string   `json:"cwd"`
	StartedAt   *string  `json:"startedAt"`

	Status        string  `json:"status"`
	RawStatus     string  `json:"rawStatus"`
	StatusSince   *string `json:"statusSince"`
	StatusAgeMs   *int64  `json:"statusAgeMs"`
	BlockedForMs  *int64  `json:"blockedForMs"`
	WaitReason    string  `json:"waitReason"`
	RawWaitingFor *string `json:"rawWaitingFor"`
	Attention     bool    `json:"attention"`

	LastActivityAt  *string  `json:"lastActivityAt"`
	Needs           *string  `json:"needs"`
	Detail          *string  `json:"detail"`
	Tempo           *string  `json:"tempo"`
	Tokens          *int64   `json:"tokens"`
	Children        []string `json:"children"`
	ResumeSessionID *string  `json:"resumeSessionId"`
	RecordUpdatedAt *string  `json:"recordUpdatedAt"`
	RecordAgeMs     *int64   `json:"recordAgeMs"`
	ReapedMidWorkAt *string  `json:"reapedMidWorkAt"`

	Tmux      *ttTmux  `json:"tmux"`
	PaneAlive *bool    `json:"paneAlive"`
	ProcAlive *bool    `json:"procAlive"`
	ListedBy  []string `json:"listedBy"`

	ClaudeVersion       string  `json:"claudeVersion"`
	MessagingSocketPath *string `json:"messagingSocketPath"`

	Transcript *ttTranscript `json:"transcript"`
}

// ErrNotATTDocument is FromTT's answer to bytes that do not decode as tt's
// snapshot.
var ErrNotATTDocument = errors.New("difftest: not a tt sessions document")

// FromTT keys a row with a shortId as {sessionId, Job: shortId}, else
// {sessionId, PID: pid (null->0)}. projectsPath is the canonical projects
// prefix ("/testdata/projects" for the corpus, the real path live); transcript
// paths are normalised to ProjectsRel(projectsPath, p), or "outside:<clean p>".
//
// The three derived ages are checked against tt's OWN instants, |age −
// (observedAt − instant)| ≤ 1 ms, and then dropped as representational; an age
// that fails the check stays on the row as a field the library cannot match.
func FromTT(doc []byte, projectsPath string) (rows []Row, observedAt time.Time, err error) {
	var d ttDoc
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&d); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrNotATTDocument, err)
	}
	observedAt, err = time.Parse(time.RFC3339Nano, d.ObservedAt)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: observedAt %q: %v", ErrNotATTDocument, d.ObservedAt, err)
	}
	for i, s := range d.Sessions {
		r, err := fromTTSession(s, projectsPath, observedAt)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("difftest: tt session %d (%s): %w", i, s.SessionID, err)
		}
		rows = append(rows, r)
	}
	return rows, observedAt, nil
}

func fromTTSession(s ttSession, projectsPath string, observedAt time.Time) (Row, error) {
	key := RowKey{SessionID: s.SessionID}
	if s.ShortID != nil && *s.ShortID != "" {
		key.Job = *s.ShortID
	} else if s.PID != nil {
		key.PID = *s.PID
	}
	pid := 0
	if s.PID != nil {
		pid = *s.PID
	}
	parsed := map[string]Value{}
	for name, raw := range map[string]*string{
		FieldStartedAt: s.StartedAt, FieldStatusSince: s.StatusSince, FieldLastActivityAt: s.LastActivityAt,
		FieldRecordUpdatedAt: s.RecordUpdatedAt, FieldReapedMidWorkAt: s.ReapedMidWorkAt,
	} {
		v, err := ttInstant(raw)
		if err != nil {
			return Row{}, fmt.Errorf("%s: %w", name, err)
		}
		parsed[name] = v
	}
	fields := map[string]Value{
		FieldKind:                str(s.Kind),
		FieldShortID:             strPtr(s.ShortID),
		FieldPID:                 pidValue(pid),
		FieldProcAlive:           boolPtr(s.ProcAlive),
		FieldStatus:              str(s.Status),
		FieldStatusRaw:           str(s.RawStatus),
		FieldWaitReason:          ttWaiting(s.WaitReason),
		FieldRawWaitingFor:       strPtr(s.RawWaitingFor),
		FieldAttention:           Value{S: strconv.FormatBool(s.Attention)},
		FieldName:                str(s.Name),
		FieldNameSource:          str(s.NameSource),
		FieldFormerNames:         list(s.FormerNames),
		FieldCWD:                 str(s.CWD),
		FieldClaudeVersion:       str(s.ClaudeVersion),
		FieldMessagingSocketPath: strPtr(s.MessagingSocketPath),
		FieldTmux:                null,
		FieldPaneAlive:           boolPtr(s.PaneAlive),
		FieldListedBy:            list(s.ListedBy),
		FieldNeeds:               strPtr(s.Needs),
		FieldDetail:              strPtr(s.Detail),
		FieldTempo:               strPtr(s.Tempo),
		FieldTokens:              intPtr(s.Tokens),
		FieldChildren:            list(s.Children),
		FieldResumeSessionID:     strPtr(s.ResumeSessionID),
	}
	for name, v := range parsed {
		fields[name] = v
	}
	if s.Tmux != nil {
		fields[FieldTmux] = tmux(s.Tmux.Raw, s.Tmux.Session, s.Tmux.Window, s.Tmux.Pane)
	}
	for _, f := range []string{FieldTranscriptPath, FieldSubagents, FieldAgentColor, FieldAPIErrors,
		FieldContextTokens, FieldModel, FieldCompactions} {
		fields[f] = null
	}
	if t := s.Transcript; t != nil {
		fields[FieldTranscriptPath] = ttPath(projectsPath, t.Path)
		fields[FieldSubagents] = intPtr(t.Subagents)
		if t.Known != (t.Subagents != nil) {
			// subagentsKnown is exactly "subagents is not null" in tt
			// (transcriptfs/subagents.go countSubagents); a row where it is
			// not cannot be projected, so it stays visible.
			fields[FieldSubagents] = Value{S: fmt.Sprintf("inconsistent: subagents %v, subagentsKnown %v", t.Subagents != nil, t.Known)}
		}
		if t.ColorFound {
			fields[FieldAgentColor] = Value{S: "=" + t.AgentColor}
		} else if t.AgentColor != "" {
			fields[FieldAgentColor] = Value{S: "inconsistent: agentColor without colorFound"}
		}
		fields[FieldAPIErrors] = Value{S: strconv.Itoa(t.APIErrors)}
		fields[FieldContextTokens] = intPtr(t.ContextTokens)
		fields[FieldModel] = str(t.Model)
		fields[FieldCompactions] = Value{S: strconv.Itoa(t.Compactions)}
	}

	checkAge(fields, FieldStatusAgeMs, s.StatusAgeMs, parsed[FieldStatusSince], observedAt)
	checkAge(fields, FieldRecordAgeMs, s.RecordAgeMs, parsed[FieldRecordUpdatedAt], observedAt)
	checkAge(fields, FieldBlockedForMs, s.BlockedForMs, parsed[FieldStatusSince], observedAt)
	return Row{Key: key, Fields: fields}, nil
}

// checkAge records an age that tt's own instant does not explain.
func checkAge(fields map[string]Value, name string, age *int64, since Value, observedAt time.Time) {
	if age == nil {
		return
	}
	if since.Null {
		fields[name] = Value{S: fmt.Sprintf("inconsistent: %d ms with no instant", *age)}
		return
	}
	ms, _ := strconv.ParseInt(since.S, 10, 64)
	want := observedAt.UnixMilli() - ms
	if d := *age - want; d < -1 || d > 1 {
		fields[name] = Value{S: fmt.Sprintf("inconsistent: %d ms, observedAt − instant = %d ms", *age, want)}
	}
}

func ttInstant(raw *string) (Value, error) {
	if raw == nil {
		return null, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		return null, err
	}
	return Value{S: strconv.FormatInt(t.UnixMilli(), 10)}, nil
}

// ttWaiting is D14's unification, tt's half: "" is "not applicable".
func ttWaiting(w string) Value {
	switch w {
	case "":
		return null
	case "input-needed":
		return Value{S: "input"}
	case "permission-prompt":
		return Value{S: "permission"}
	}
	return Value{S: w}
}

// ttPath normalises tt's absolute transcript path (review 03-M2).
func ttPath(projectsPath, p string) Value {
	if p == "" {
		return null
	}
	if rel, ok := claude.ProjectsRel(projectsPath, p); ok {
		return Value{S: rel}
	}
	return Value{S: "outside:" + filepath.Clean(p)}
}
