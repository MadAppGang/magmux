// Package join turns one cycle's decoded records into sessions (§3.4).
//
// Rule 1, the key is a function of ONE record's own identity fields:
//
//	K1  a registry record with kind "bg", a jobId and a sessionId -> {SessionID, Job: jobId}
//	K2  a job record                                        -> {SessionID, Job: Short}
//	    with no sessionId                                   -> {Orphan: "jobs/" + dir}
//	K3  any other registry record with a sessionId or a pid,
//	    and an admitted interactive agents entry            -> {SessionID, PID}
//	K4  a registry record with neither                      -> {Orphan: file base name}
//
// Rule 2, records that produce the same key are ONE session: nothing is
// re-keyed and nothing is published twice. The join chooses speakers among a
// key's claimants each cycle, and the key never depends on which one speaks.
//
// Rule 3, ONE effective status per session: claude.Effective over the
// speakers, and nothing else writes the six effective fields here.
//
// Build is a pure function of its input: no clock, no I/O, and input order
// never matters.
package join

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

// Inputs is everything one cycle read.
type Inputs struct {
	Records []claude.Record
	Jobs    []claude.Job
	Agents  *claude.AgentsListing
	// Alive answers a pid's liveness; nil means every pid is Unknown, never
	// No (FR21).
	Alive func(pid int) model.Liveness
}

// orphanJobPrefix keeps an id-less job's orphan key apart from a registry
// file's: a file base name never contains "/".
const orphanJobPrefix = "jobs/"

// RegistryKey is a registry record's key by rule 1 (K1, K3 or K4).
func RegistryKey(r claude.Record) model.Key {
	switch {
	case r.Kind == model.KindBackground && r.JobID != "" && r.SessionID != "":
		return model.Key{SessionID: r.SessionID, Job: r.JobID}
	case r.SessionID != "" || r.PID > 0:
		return model.Key{SessionID: r.SessionID, PID: r.PID}
	default:
		return model.Key{Orphan: r.File}
	}
}

// JobKey is a job record's key by rule 1 (K2, or an id-less job's orphan).
func JobKey(j claude.Job) model.Key {
	if j.SessionID == "" {
		return model.Key{Orphan: orphanJobPrefix + j.Dir}
	}
	return model.Key{SessionID: j.SessionID, Job: j.Short}
}

// group is one key's claimants.
type group struct {
	key   model.Key
	regs  []claude.Record
	jobs  []claude.Job
	agent *claude.AgentsEntry
}

// Build is §3.4 in full. It returns one session per key, sorted by key, and
// the duplicate-claim problems rule 2 raises, each naming its key.
func Build(in Inputs) ([]model.Session, []model.Problem) {
	alive := in.Alive
	if alive == nil {
		alive = func(int) model.Liveness { return model.LivenessUnknown }
	}
	live := func(pid int) model.Liveness {
		if pid <= 0 {
			return model.LivenessUnknown
		}
		return alive(pid)
	}

	groups := map[model.Key]*group{}
	at := func(k model.Key) *group {
		g := groups[k]
		if g == nil {
			g = &group{key: k}
			groups[k] = g
		}
		return g
	}
	claimedIDs, claimedPIDs := map[string]bool{}, map[int]bool{}
	for _, r := range in.Records {
		g := at(RegistryKey(r))
		g.regs = append(g.regs, r)
		if r.SessionID != "" {
			claimedIDs[r.SessionID] = true
		}
		if r.PID > 0 {
			claimedPIDs[r.PID] = true
		}
	}
	for _, j := range in.Jobs {
		g := at(JobKey(j))
		g.jobs = append(g.jobs, j)
	}
	// FR15 admission (tt:join_agents.go:42-94): an interactive entry creates a
	// session only when NEITHER its id NOR its pid is on any registry record.
	// <pid>.json is the process's own authority, and a cached listing may
	// predate a /clear. Entries are in session-id order, so a duplicate in the
	// listing is resolved by that order: the first wins.
	if in.Agents != nil {
		for i := range in.Agents.Entries {
			e := &in.Agents.Entries[i]
			if e.SessionID == "" || !claude.IsInteractiveEntry(*e) || claimedIDs[e.SessionID] {
				continue
			}
			if e.PID != nil && claimedPIDs[*e.PID] {
				continue
			}
			claimedIDs[e.SessionID] = true
			pid := 0
			if e.PID != nil {
				pid = *e.PID
				claimedPIDs[pid] = true
			}
			at(model.Key{SessionID: e.SessionID, PID: pid}).agent = e
		}
	}

	keys := make([]model.Key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, model.Key.Compare)

	sessions := make([]model.Session, 0, len(keys))
	var problems []model.Problem
	for _, k := range keys {
		s, ps := build(groups[k], in.Agents, live)
		sessions = append(sessions, s)
		problems = append(problems, ps...)
	}
	return sessions, problems
}

func build(g *group, listing *claude.AgentsListing, live func(int) model.Liveness) (model.Session, []model.Problem) {
	var problems []model.Problem
	key := g.key
	keyRef := func() *model.Key { k := key; return &k }

	reg, regProblem := registrySpeaker(g, live)
	if regProblem != nil {
		regProblem.Key = keyRef()
		problems = append(problems, *regProblem)
	}
	job, jobProblem := jobSpeaker(g)
	if jobProblem != nil {
		jobProblem.Key = keyRef()
		problems = append(problems, *jobProblem)
	}

	s := model.Session{Key: key}
	for _, r := range g.regs {
		s.Claimants = append(s.Claimants, r.PID)
	}
	slices.Sort(s.Claimants)

	// The pid is the registry speaker's whenever a registry claimant exists,
	// dead or alive (tt sets it unconditionally); else the admitted entry's.
	switch {
	case reg != nil:
		s.PID = reg.PID
	case g.agent != nil && g.agent.PID != nil:
		s.PID = *g.agent.PID
	}
	s.ProcessAlive = live(s.PID)

	// The effective status: rule E, over this cycle's speakers.
	in := claude.EffectiveInput{Registry: reg, Job: job, ProcAlive: model.LivenessUnknown}
	if reg != nil {
		in.ProcAlive = live(reg.PID)
	}
	if reg == nil && job == nil {
		in.Agents = g.agent
	}
	eff := claude.Effective(in)
	s.Status, s.StatusRaw, s.StatusSince, s.StatusFrom = eff.Status, eff.Raw, eff.Since, eff.From
	s.Waiting, s.WaitingRaw = eff.Waiting, eff.WaitingRaw

	// Facts: the registry speaker's when it has them (a DEAD speaker still
	// supplies them; liveness changes only what Effective decides), else the
	// job speaker's, else the agents entry's: overlayTwin's precedence.
	if job != nil {
		s.Name, s.NameSource, s.CWD = job.Name, job.NameSource, job.CWD
		s.StartedAt = job.CreatedAt
		s.Version = job.CLIVersion
		s.Job = jobFacts(*job, len(g.regs) > 0, eff.Conflict)
	} else if g.agent != nil && reg == nil {
		s.Name, s.CWD, s.StartedAt = g.agent.Name, g.agent.CWD, g.agent.StartedAt
		s.KindRaw = g.agent.KindRaw
	}
	if reg != nil {
		if reg.Name != "" {
			s.Name = reg.Name
		}
		if reg.NameSource != "" {
			s.NameSource = reg.NameSource
		}
		if reg.CWD != "" {
			s.CWD = reg.CWD
		}
		if reg.StartedAt != nil {
			s.StartedAt = reg.StartedAt
		}
		if reg.Version != "" {
			s.Version = reg.Version
		}
		s.PreviousNames = reg.FormerNames
		s.MessagingSocketPath = reg.MessagingSocketPath
		s.Tmux = reg.Tmux
		s.KindRaw = reg.KindRaw
		s.Registry = &model.RegistryFacts{
			File: reg.File, Status: reg.Status, StatusRaw: reg.StatusRaw, StatusSince: reg.StatusUpdatedAt,
			Waiting: reg.Waiting, WaitingRaw: reg.WaitingRaw, UpdatedAt: reg.UpdatedAt,
		}
	}

	// Background when a job claims the key, or when the key has a job (K1,
	// K2). A spare's own kind:"bg" with no jobId is background by its word.
	switch {
	case s.Job != nil || key.Job != "":
		s.Kind = model.KindBackground
	case reg != nil:
		s.Kind = reg.Kind
	case g.agent != nil:
		s.Kind = model.KindInteractive
	}

	// ListedBy is membership: registry, jobs, agents, in that order.
	if len(g.regs) > 0 {
		s.ListedBy = append(s.ListedBy, model.SourceRegistry)
	}
	if len(g.jobs) > 0 {
		s.ListedBy = append(s.ListedBy, model.SourceJobs)
	}
	if listing != nil && listing.Lists(key.SessionID) {
		s.ListedBy = append(s.ListedBy, model.SourceAgents)
	}
	// The session shares no pointer or slice with the records it was built from.
	return s.Clone(), problems
}

// registrySpeaker chooses whose words a key's registry claimants carry.
//
//   - K1 (two kind:"bg" processes claiming one job): the best liveness (Yes,
//     then Unknown, then No), then the lowest pid, with ProblemDuplicateJobClaim
//     naming every pid. Unlike tt, which takes the lowest pid even when dead
//     and leaves the live claimant unrepresented (cited D12c).
//   - K3 (one id and one pid in two files): the lowest file name, BYTE-WISE,
//     which is tt's order exactly, with ProblemDuplicateKey naming every file.
func registrySpeaker(g *group, live func(int) model.Liveness) (*claude.Record, *model.Problem) {
	if len(g.regs) == 0 {
		return nil, nil
	}
	regs := slices.Clone(g.regs)
	if len(regs) == 1 {
		return &regs[0], nil
	}
	var pids []int
	var files []string
	for _, r := range regs {
		pids = append(pids, r.PID)
		files = append(files, r.File)
	}
	slices.Sort(pids)
	slices.Sort(files)
	if g.key.Job != "" {
		rank := func(l model.Liveness) int {
			switch l {
			case model.LivenessYes:
				return 0
			case model.LivenessUnknown:
				return 1
			default:
				return 2
			}
		}
		slices.SortStableFunc(regs, func(a, b claude.Record) int {
			if c := cmp.Compare(rank(live(a.PID)), rank(live(b.PID))); c != 0 {
				return c
			}
			if c := cmp.Compare(a.PID, b.PID); c != 0 {
				return c
			}
			return strings.Compare(a.File, b.File)
		})
		return &regs[0], &model.Problem{
			Scope: model.ScopeRecord, Kind: model.ProblemDuplicateJobClaim, PIDs: pids,
			Detail: fmt.Sprintf("%d processes claim job %s: pids %v; the speaker is pid %d", len(regs), g.key.Job, pids, regs[0].PID),
		}
	}
	slices.SortStableFunc(regs, func(a, b claude.Record) int { return strings.Compare(a.File, b.File) })
	return &regs[0], &model.Problem{
		Scope: model.ScopeRecord, Kind: model.ProblemDuplicateKey, PIDs: pids,
		Detail: fmt.Sprintf("%d registry files describe one session: %s; %s speaks", len(regs), strings.Join(files, ", "), regs[0].File),
	}
}

// jobSpeaker chooses among a K2 key's job records: the lowest directory name,
// byte-wise, tt's order (tt:join_jobs.go:71-91), with ProblemDuplicateKey
// naming every directory.
func jobSpeaker(g *group) (*claude.Job, *model.Problem) {
	if len(g.jobs) == 0 {
		return nil, nil
	}
	jobs := slices.Clone(g.jobs)
	slices.SortStableFunc(jobs, func(a, b claude.Job) int { return strings.Compare(a.Dir, b.Dir) })
	if len(jobs) == 1 {
		return &jobs[0], nil
	}
	var dirs []string
	for _, j := range jobs {
		dirs = append(dirs, j.Dir)
	}
	return &jobs[0], &model.Problem{
		Scope: model.ScopeRecord, Kind: model.ProblemDuplicateKey,
		Detail: fmt.Sprintf("%d job directories describe one session: %s; %s speaks", len(jobs), strings.Join(dirs, ", "), jobs[0].Dir),
	}
}

func jobFacts(j claude.Job, twin, conflict bool) *model.JobFacts {
	return &model.JobFacts{
		Dir: j.Dir, Short: j.Short, State: j.State, StateRaw: j.StateRaw,
		Tempo: j.Tempo, Needs: j.Needs, Detail: j.Detail, Tokens: j.Tokens, Children: j.Children,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, ReapedMidWorkAt: j.ReapedMidWorkAt,
		Name: j.Name, NameSource: j.NameSource, CWD: j.CWD,
		ResumeSessionID: j.ResumeSessionID, LinkScanPath: j.LinkScanPath, CLIVersion: j.CLIVersion,
		Twin: twin, Conflict: conflict,
	}
}
