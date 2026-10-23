package difftest

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// RowKey mirrors model.Key exactly (review 01-M3, 02-M8, 03-H5): FromModel
// copies Session.Key, and FromTT builds the same shape from tt's row.
type RowKey struct {
	SessionID string
	PID       int
	Job       string
	Orphan    string
}

// String spells a key the way model.Key does.
func (k RowKey) String() string {
	return model.Key{SessionID: k.SessionID, PID: k.PID, Job: k.Job, Orphan: k.Orphan}.String()
}

// Value is the normalised form of one field: Null, or a string.
type Value struct {
	Null bool
	S    string
}

// String renders a value for a report.
func (v Value) String() string {
	if v.Null {
		return "null"
	}
	return strconv.Quote(v.S)
}

var null = Value{Null: true}

func str(s string) Value {
	if s == "" {
		return null
	}
	return Value{S: s}
}

func strPtr(p *string) Value {
	if p == nil {
		return null
	}
	return str(*p)
}

func intPtr[T int | int64](p *T) Value {
	if p == nil {
		return null
	}
	return Value{S: strconv.FormatInt(int64(*p), 10)}
}

// pid: 0 and null are the same "no process" on both sides.
func pidValue(pid int) Value {
	if pid <= 0 {
		return null
	}
	return Value{S: strconv.Itoa(pid)}
}

func instant(t *time.Time) Value {
	if t == nil {
		return null
	}
	return Value{S: strconv.FormatInt(t.UnixMilli(), 10)}
}

func boolPtr(b *bool) Value {
	if b == nil {
		return null
	}
	return Value{S: strconv.FormatBool(*b)}
}

func liveness(l model.Liveness) Value {
	switch l {
	case model.LivenessYes:
		return Value{S: "true"}
	case model.LivenessNo:
		return Value{S: "false"}
	}
	return null
}

// list: an empty list and an absent one are the same value, because tt
// normalises nil to [] (review 3 Issue 22) and the library keeps nil.
func list(items []string) Value {
	if len(items) == 0 {
		return null
	}
	b, _ := json.Marshal(items)
	return Value{S: string(b)}
}

func tmux(raw, session, window, pane string) Value {
	return Value{S: strings.Join([]string{raw, session, window, pane}, "|")}
}

// The projected fields (§8.1's table). Each is named by tt's wire member; a
// transcript fact by "transcript." plus its member.
const (
	FieldKind                = "kind"
	FieldShortID             = "shortId"
	FieldPID                 = "pid"
	FieldProcAlive           = "procAlive"
	FieldStatus              = "status"
	FieldStatusRaw           = "statusRaw"
	FieldStatusSince         = "statusSince"
	FieldWaitReason          = "waitReason"
	FieldRawWaitingFor       = "rawWaitingFor"
	FieldAttention           = "attention"
	FieldName                = "name"
	FieldNameSource          = "nameSource"
	FieldFormerNames         = "formerNames"
	FieldCWD                 = "cwd"
	FieldStartedAt           = "startedAt"
	FieldClaudeVersion       = "claudeVersion"
	FieldMessagingSocketPath = "messagingSocketPath"
	FieldTmux                = "tmux"
	FieldPaneAlive           = "paneAlive"
	FieldListedBy            = "listedBy"
	FieldLastActivityAt      = "lastActivityAt"
	FieldReapedMidWorkAt     = "reapedMidWorkAt"
	FieldRecordUpdatedAt     = "recordUpdatedAt"
	FieldNeeds               = "needs"
	FieldDetail              = "detail"
	FieldTempo               = "tempo"
	FieldTokens              = "tokens"
	FieldChildren            = "children"
	FieldResumeSessionID     = "resumeSessionId"
	FieldTranscriptPath      = "transcript.path"
	FieldSubagents           = "transcript.subagents"
	FieldAgentColor          = "transcript.agentColor"
	FieldAPIErrors           = "transcript.apiErrors"
	FieldContextTokens       = "transcript.contextTokens"
	FieldModel               = "transcript.model"
	FieldCompactions         = "transcript.compactions"
	// The three derived ages are checked against tt's own instants and then
	// dropped (§8.1). A row carries one of these only when tt's age disagreed
	// with its own instant, which no library field can explain.
	FieldStatusAgeMs  = "statusAgeMs"
	FieldRecordAgeMs  = "recordAgeMs"
	FieldBlockedForMs = "blockedForMs"
)

// LibFacts is what an attribution predicate needs to know about a LIBRARY row
// beyond its projected fields. A tt row has none.
type LibFacts struct {
	PID              int
	Claimants        []int
	Kind             model.Kind
	KindRaw          string
	HasJob           bool
	HasRegistry      bool
	ProcessAlive     model.Liveness
	AttentionReason  string
	SubagentEvidence model.Evidence
	// RegistryFile is the speaking registry record's base name, JobDir the
	// speaking job's directory: how a decoder Problem that carries a Path and
	// no Key is tied to its session.
	RegistryFile string
	JobDir       string
	// Problems are the problems FromModel tied to this row.
	Problems []model.Problem
}

// Row is one session, projected.
type Row struct {
	Key    RowKey
	Fields map[string]Value
	Lib    *LibFacts // nil on a tt row
}

func (r Row) field(name string) Value {
	if v, ok := r.Fields[name]; ok {
		return v
	}
	return null
}

// FromModel projects a published document. problems are the delivery's; each
// is tied to the rows it is about (by Key, or by the Path of the speaking
// record when a decoder problem carries no Key).
func FromModel(f model.Fleet, problems []model.Problem) []Row {
	rows := make([]Row, 0, len(f.Sessions))
	for _, s := range f.Sessions {
		rows = append(rows, fromSession(s))
	}
	for i := range rows {
		rows[i].Lib.Problems = problemsFor(rows[i], problems)
	}
	return rows
}

func fromSession(s model.Session) Row {
	fields := map[string]Value{
		FieldKind:                str(kindWord(s.Kind)),
		FieldShortID:             str(s.Key.Job),
		FieldPID:                 pidValue(s.PID),
		FieldProcAlive:           liveness(s.ProcessAlive),
		FieldStatus:              str(s.Status.String()),
		FieldStatusRaw:           str(s.StatusRaw),
		FieldStatusSince:         instant(s.StatusSince),
		FieldWaitReason:          libWaiting(s),
		FieldRawWaitingFor:       str(s.WaitingRaw),
		FieldName:                str(s.Name),
		FieldNameSource:          str(s.NameSource),
		FieldCWD:                 str(s.CWD),
		FieldStartedAt:           instant(s.StartedAt),
		FieldClaudeVersion:       str(s.Version),
		FieldMessagingSocketPath: strPtr(s.MessagingSocketPath),
		FieldTmux:                null,
		FieldPaneAlive:           liveness(s.PaneAlive),
		FieldAttention:           boolPtr(s.Attention.Needed),
	}
	var former []string
	for _, p := range s.PreviousNames {
		former = append(former, p.Name)
	}
	fields[FieldFormerNames] = list(former)
	if s.Tmux != nil {
		fields[FieldTmux] = tmux(s.Tmux.Raw, s.Tmux.Session, s.Tmux.Window, s.Tmux.Pane)
	}
	var listed []string
	for _, id := range s.ListedBy {
		listed = append(listed, sourceWord(id))
	}
	fields[FieldListedBy] = list(listed)

	for _, f := range []string{FieldRecordUpdatedAt, FieldReapedMidWorkAt, FieldNeeds, FieldDetail,
		FieldTempo, FieldTokens, FieldChildren, FieldResumeSessionID} {
		fields[f] = null
	}
	lib := &LibFacts{
		PID: s.PID, Claimants: slices.Clone(s.Claimants), Kind: s.Kind, KindRaw: s.KindRaw,
		HasJob: s.Job != nil, HasRegistry: s.Registry != nil, ProcessAlive: s.ProcessAlive,
		AttentionReason: s.Attention.Reason,
	}
	if s.Registry != nil {
		lib.RegistryFile = s.Registry.File
	}
	if j := s.Job; j != nil {
		lib.JobDir = j.Dir
		fields[FieldRecordUpdatedAt] = instant(j.UpdatedAt)
		fields[FieldReapedMidWorkAt] = instant(j.ReapedMidWorkAt)
		fields[FieldNeeds] = strPtr(j.Needs)
		fields[FieldDetail] = strPtr(j.Detail)
		fields[FieldTempo] = strPtr(j.Tempo)
		fields[FieldTokens] = intPtr(j.Tokens)
		fields[FieldChildren] = list(j.Children)
		fields[FieldResumeSessionID] = str(j.ResumeSessionID)
	}

	for _, f := range []string{FieldLastActivityAt, FieldTranscriptPath, FieldSubagents, FieldAgentColor,
		FieldAPIErrors, FieldContextTokens, FieldModel, FieldCompactions} {
		fields[f] = null
	}
	if t := s.Transcript; t != nil {
		lib.SubagentEvidence = t.SubagentEvidence
		fields[FieldLastActivityAt] = instant(t.LastActivityAt)
		// The library's Location.Path is already relative to the projects
		// tree (review 03-M2); tt's absolute path is made relative in FromTT.
		fields[FieldTranscriptPath] = str(t.Location.Path)
		fields[FieldSubagents] = intPtr(t.Subagents)
		fields[FieldAgentColor] = colour(t.Color)
		fields[FieldAPIErrors] = intPtr(t.APIErrors)
		fields[FieldContextTokens] = intPtr(t.ContextTokens)
		fields[FieldModel] = str(t.Model)
		fields[FieldCompactions] = intPtr(t.Compactions)
	}
	return Row{
		Key:    RowKey{SessionID: s.Key.SessionID, PID: s.Key.PID, Job: s.Key.Job, Orphan: s.Key.Orphan},
		Fields: fields,
		Lib:    lib,
	}
}

// colour: a found colour is a value even when it is "" (an explicitly empty
// agent-color record), and an unfound one is null. tt carries the same two
// facts as (agentColor, colorFound).
func colour(c *string) Value {
	if c == nil {
		return null
	}
	return Value{S: "=" + *c}
}

// libWaiting is D14's unification. The library keeps Waiting Unknown unless a
// record's waitingFor decided it, and each host spells "no reason" on its own
// wire (magmux null, tt ""). tt splits that one library value by the status it
// rides on: a WAITING status (a registry or agents record that said waiting)
// with no recognised waitingFor is "unknown" — it is waiting, for what nobody
// said — while BLOCKED comes from a job's state or tempo, never from
// waitingFor, and is "" (tt:internal/observer/status.go MapJobState: "every
// job state yields WaitReasonNone, including blocked"). So:
//
//   - not WaitingApplies(): null;
//   - a decided reason: its word;
//   - Unknown on waiting: "unknown";
//   - Unknown on blocked: null.
func libWaiting(s model.Session) Value {
	if !s.WaitingApplies() {
		return null
	}
	switch s.Waiting {
	case model.WaitingInput:
		return Value{S: "input"}
	case model.WaitingPermission:
		return Value{S: "permission"}
	case model.WaitingDialog:
		return Value{S: "dialog"}
	}
	if s.Status == model.StatusBlocked {
		return null
	}
	return Value{S: "unknown"}
}

func kindWord(k model.Kind) string {
	switch k {
	case model.KindInteractive:
		return "interactive"
	case model.KindBackground:
		return "background"
	}
	return "unknown"
}

// sourceWord is tt's listedBy vocabulary for a source (§8.1: registry,
// jobs, agents-cli).
func sourceWord(id model.SourceID) string {
	switch id {
	case model.SourceRegistry:
		return "registry"
	case model.SourceJobs:
		return "jobs"
	case model.SourceAgents:
		return "agents-cli"
	}
	return id.String()
}

// problemsFor ties problems to a library row: by Key when the problem carries
// one, else by the Path of the record the row's speaker was read from.
func problemsFor(r Row, problems []model.Problem) []model.Problem {
	var out []model.Problem
	for _, p := range problems {
		if p.Key != nil {
			k := *p.Key
			if (RowKey{SessionID: k.SessionID, PID: k.PID, Job: k.Job, Orphan: k.Orphan}) == r.Key {
				out = append(out, p)
			}
			continue
		}
		if p.Path == "" || p.Scope != model.ScopeRecord {
			continue
		}
		switch {
		case r.Lib.RegistryFile != "" && p.Path == r.Lib.RegistryFile:
			out = append(out, p)
		case r.Lib.JobDir != "" && (p.Path == r.Lib.JobDir || strings.HasPrefix(p.Path, r.Lib.JobDir+"/")):
			out = append(out, p)
		case r.Key.Orphan != "" && (p.Path == r.Key.Orphan || strings.HasPrefix(p.Path, strings.TrimPrefix(r.Key.Orphan, "jobs/")+"/")):
			out = append(out, p)
		}
	}
	return out
}
