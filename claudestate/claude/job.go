package claude

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// Job is one background job record, jobs/<dir>/state.json.
type Job struct {
	Dir, Short, DaemonShort, SessionID string
	// State is MapJobState(state) alone; the effective status also reads
	// tempo (MapJobRecord).
	State                                 model.Status
	StateRaw                              string
	Tempo, Detail, Needs                  *string
	Tokens                                *int64
	CWD, Name, NameSource, CLIVersion     string
	Children                              []string
	CreatedAt, UpdatedAt, ReapedMidWorkAt *time.Time
	LinkScanPath, ResumeSessionID         string
}

// IsJobStatePath reports exactly "<dir>/state.json" relative to the jobs tree.
// Deeper is NEVER a record: the real tree carries decoy state.json files at
// depths 5 and 8 under one job's tmp/, and one parses into a plausible agent.
func IsJobStatePath(rel string) bool {
	dir, file := path.Split(rel)
	if file != JobStateFile {
		return false
	}
	dir = strings.TrimSuffix(dir, "/")
	return dir != "" && dir != "." && dir != ".." && !strings.Contains(dir, "/")
}

// DecodeJob decodes one job record.
//
// err is ErrTorn as for DecodeRecord. A missing sessionId is
// ProblemMissingField and the job is still returned (D11); a daemonShort that
// is not the directory name is ProblemShortIDMismatch, and the record's own
// short wins: the directory is where it was FOUND, not what it says it is.
// Short is daemonShort, else dir.
//
// Five keys are never read at all (KeyIntent and the rest): Job has no field
// to carry them, which is the guarantee.
func DecodeJob(dir string, b []byte) (Job, []model.Problem, error) {
	p := path.Join(dir, JobStateFile)
	o, err := decodeObject(p, b)
	if err != nil {
		return Job{Dir: dir, Short: dir}, nil, err
	}
	j := Job{Dir: dir}
	j.SessionID = o.str(KeySessionID)
	if j.SessionID == "" && !o.hasProblem(KeySessionID) {
		o.problem(model.ProblemMissingField, KeySessionID, "no session id; the job is kept under its directory")
	}
	j.DaemonShort = o.str(KeyDaemonShort)
	j.Short = j.DaemonShort
	if j.Short == "" {
		j.Short = dir
	}
	if j.DaemonShort != "" && j.DaemonShort != dir {
		o.problem(model.ProblemShortIDMismatch, KeyDaemonShort,
			fmt.Sprintf("the directory is %q and the record says %q; the record wins", dir, j.DaemonShort))
	}
	j.StateRaw = o.str(KeyState)
	j.State = MapJobState(j.StateRaw)
	j.Tempo = o.strPtr(KeyTempo)
	j.Detail = o.strPtr(KeyDetail)
	j.Needs = o.strPtr(KeyNeeds)
	if n, ok := o.int64Of(KeyTokens); ok {
		j.Tokens = &n
	}
	j.CWD = o.str(KeyCWD)
	j.Name = o.str(KeyName)
	j.NameSource = o.str(KeyNameSource)
	j.CLIVersion = o.str(KeyCLIVersion)
	j.Children = o.children()
	j.CreatedAt = o.rfc3339(KeyCreatedAt)
	j.UpdatedAt = o.rfc3339(KeyUpdatedAt)
	j.ReapedMidWorkAt = o.rfc3339(KeyReapedMidWorkAt)
	j.LinkScanPath = o.str(KeyLinkScanPath)
	j.ResumeSessionID = o.str(KeyResumeSessionID)
	return j, o.problems, nil
}

// children reads KeyChildren: strings, or objects describing an ARTIFACT (a
// pull request), reduced to its href, else "kind:id". An object is never
// reduced to its bare id, which would publish "158" in a field a reader takes
// for session ids.
func (o *object) children() []string {
	raw, ok := o.m[KeyChildren]
	if !ok || isNull(raw) {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		o.problem(model.ProblemUndecoded, KeyChildren, "is "+jsonKind(raw)+", not a list")
		return nil
	}
	var out []string
	for _, it := range items {
		if s, ok := rawString(it); ok {
			out = append(out, s)
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(it, &obj) != nil {
			continue
		}
		href, _ := rawString(obj[KeyChildHref])
		kind, _ := rawString(obj[KeyChildKind])
		id, _ := rawString(obj[KeyChildID])
		switch {
		case href != "":
			out = append(out, href)
		case kind != "" && id != "":
			out = append(out, kind+":"+id)
		case id != "":
			out = append(out, id)
		}
	}
	return out
}
