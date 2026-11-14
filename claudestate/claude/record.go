package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// Record is one registry record, sessions/<pid>.json, in model's vocabulary.
type Record struct {
	File      string // base name
	SessionID string
	// PID is the body pid, else the filename's (PIDFromName == true).
	PID         int
	PIDFromName bool
	NamePID     int // the filename's pid, 0 when the name is not numeric

	// Status is MapRegistryStatus of this record alone; StatusRaw is what the
	// tool wrote, the only truth when Status is Unknown.
	Status    model.Status
	StatusRaw string
	// Waiting is MapRegistryStatus's reason (Unknown unless the record says
	// waiting); WaitingRaw is the raw waitingFor whatever the status.
	Waiting    model.WaitingReason
	WaitingRaw string

	StatusUpdatedAt, UpdatedAt, StartedAt *time.Time
	Name, NameSource                      string
	FormerNames                           []model.PreviousName
	CWD                                   string
	Tmux                                  *model.TmuxRef
	Version                               string
	MessagingSocketPath                   *string
	// Kind: ValueKindBG is Background, ValueKindInteractive or absent is
	// Interactive, anything else Unknown with KindRaw kept.
	Kind         model.Kind
	KindRaw      string
	JobID        string
	PeerProtocol *int
	PeerFeatures []string
}

// IsRecordName reports "<x>.json". A ".key" sibling is never a record.
func IsRecordName(name string) bool {
	return strings.HasSuffix(name, RecordSuffix) && len(name) > len(RecordSuffix) && !strings.Contains(name, "/")
}

// PIDFromName reads "<pid>.json" -> pid. The pid in the NAME is an index into
// the record, not a second opinion about it: the body wins (D11).
func PIDFromName(name string) (int, bool) {
	base, ok := strings.CutSuffix(name, RecordSuffix)
	if !ok || base == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(base)
	if err != nil || pid <= 0 || strconv.Itoa(pid) != base {
		return 0, false
	}
	return pid, true
}

var tmuxRef = regexp.MustCompile(tmuxPattern)

// ParseTmux splits a tmux value into session, window and pane, sigils kept.
// An unparseable value sets only Raw: a pane gate can still match the raw
// string verbatim.
func ParseTmux(raw string) model.TmuxRef {
	m := tmuxRef.FindStringSubmatch(raw)
	if m == nil {
		return model.TmuxRef{Raw: raw}
	}
	return model.TmuxRef{Raw: raw, Session: m[1], Window: m[2], Pane: m[3]}
}

// LooksLikePermission is the pane controller's readiness heuristic, moved here
// so the one Claude-string judgement lives in the contract: true when the raw
// waitingFor CONTAINS "permission". It does not change Waiting.
func LooksLikePermission(waitingRaw string) bool {
	return strings.Contains(waitingRaw, permissionWord)
}

// DecodeRecord decodes one registry record.
//
// err is ErrTorn when b is not exactly one JSON object (C8): torn, not dead,
// and the caller retries. Every field-level difficulty is a Problem and never
// an error:
//   - a missing sessionId, or a missing pid with no numeric filename, is
//     ProblemMissingField (D11); a missing body pid takes the filename's;
//   - a body pid that disagrees with the filename is ProblemPIDMismatch, and
//     the body wins;
//   - an instant outside 1e12..1e14 ms is ProblemImplausibleInstant (D4);
//   - a value of the wrong JSON type is ProblemUndecoded with Field set, and
//     that field is left zero.
//
// Unknown keys are ignored: a future Claude with five new keys decodes exactly
// as today's does.
func DecodeRecord(name string, b []byte) (Record, []model.Problem, error) {
	o, err := decodeObject(name, b)
	if err != nil {
		return Record{File: name}, nil, err
	}
	r := Record{File: name}
	r.NamePID, _ = PIDFromName(name)

	r.SessionID = o.str(KeySessionID)
	if r.SessionID == "" && !o.hasProblem(KeySessionID) {
		o.problem(model.ProblemMissingField, KeySessionID, "no session id; the record is kept, keyed by what is unique to it")
	}

	body, hasBody := o.int64Of(KeyPID)
	switch {
	case hasBody && body > 0:
		r.PID = int(body)
		if r.NamePID > 0 && r.NamePID != r.PID {
			o.problem(model.ProblemPIDMismatch, KeyPID,
				fmt.Sprintf("the file says %d and the record says %d; the record wins", r.NamePID, r.PID))
		}
	case r.NamePID > 0:
		r.PID, r.PIDFromName = r.NamePID, true
		if !o.hasProblem(KeyPID) {
			o.problem(model.ProblemMissingField, KeyPID, "no usable pid in the record; the filename's is used")
		}
	default:
		if !o.hasProblem(KeyPID) {
			o.problem(model.ProblemMissingField, KeyPID, "no usable pid in the record or its name")
		}
	}

	r.StatusRaw = o.str(KeyStatus)
	r.WaitingRaw = o.str(KeyWaitingFor)
	r.Status, r.Waiting = MapRegistryStatus(r.StatusRaw, r.WaitingRaw)

	r.StatusUpdatedAt = o.millis(KeyStatusUpdatedAt)
	r.UpdatedAt = o.millis(KeyUpdatedAt)
	r.StartedAt = o.millis(KeyStartedAt)

	r.Name = o.str(KeyName)
	r.NameSource = o.str(KeyNameSource)
	r.FormerNames = o.formerNames()
	r.CWD = o.str(KeyCWD)
	if t := o.str(KeyTmux); t != "" {
		ref := ParseTmux(t)
		r.Tmux = &ref
	}
	r.Version = o.str(KeyVersion)
	r.MessagingSocketPath = o.strPtr(KeyMessagingSocketPath)

	r.KindRaw = o.str(KeyKind)
	r.Kind = registryKind(r.KindRaw)
	r.JobID = o.str(KeyJobID)
	if n, ok := o.int64Of(KeyPeerProtocol); ok {
		v := int(n)
		r.PeerProtocol = &v
	}
	r.PeerFeatures = o.stringList(KeyPeerFeatures)

	return r, o.problems, nil
}

func registryKind(raw string) model.Kind {
	switch raw {
	case ValueKindBG:
		return model.KindBackground
	case ValueKindInteractive, "":
		return model.KindInteractive
	default:
		return model.KindUnknown
	}
}

func (o *object) hasProblem(field string) bool {
	for _, p := range o.problems {
		if p.Field == field {
			return true
		}
	}
	return false
}

// formerNames reads KeyFormerNames in both of Claude's shapes, element by
// element. One bad element costs that element and is reported by index; a
// name whose until or sessionId is bad keeps the name and drops the bad part.
// Order is preserved: flattening to the latest would lose the only trail back
// to what a human last called the session.
func (o *object) formerNames() []model.PreviousName {
	raw, ok := o.m[KeyFormerNames]
	if !ok || isNull(raw) {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		o.problem(model.ProblemUndecoded, KeyFormerNames, "is "+jsonKind(raw)+", not a list; no previous name was read")
		return nil
	}
	var out []model.PreviousName
	for i, it := range items {
		if s, ok := rawString(it); ok {
			out = append(out, model.PreviousName{Name: s})
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(it, &obj) != nil || obj == nil {
			o.problem(model.ProblemUndecoded, KeyFormerNames,
				fmt.Sprintf("element %d is %s, neither a name nor an object carrying one", i, jsonKind(it)))
			continue
		}
		name, ok := rawString(obj[KeyFormerNameName])
		if !ok {
			o.problem(model.ProblemUndecoded, KeyFormerNames,
				fmt.Sprintf("element %d is an object with no text %q, so it names nothing", i, KeyFormerNameName))
			continue
		}
		p := model.PreviousName{Name: name}
		if u, present := obj[KeyFormerNameUntil]; present && !isNull(u) {
			ms, isInt := rawInt64(u)
			switch t, err := DecodeInstantMillis(ms); {
			case !isInt:
				o.problem(model.ProblemUndecoded, KeyFormerNames,
					fmt.Sprintf("element %d keeps its name, but its %q is %s", i, KeyFormerNameUntil, jsonKind(u)))
			case err != nil:
				o.problem(model.ProblemImplausibleInstant, KeyFormerNames,
					fmt.Sprintf("element %d keeps its name, but its %q is %d", i, KeyFormerNameUntil, ms))
			default:
				p.Until = t
			}
		}
		if sid, present := obj[KeyFormerNameSessionID]; present && !isNull(sid) {
			if s, ok := rawString(sid); ok {
				p.SessionID = s
			} else {
				o.problem(model.ProblemUndecoded, KeyFormerNames,
					fmt.Sprintf("element %d keeps its name, but its %q is %s", i, KeyFormerNameSessionID, jsonKind(sid)))
			}
		}
		out = append(out, p)
	}
	return out
}
