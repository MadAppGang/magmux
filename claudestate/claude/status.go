package claude

import (
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// The three status mappings and the twin rule are tt's code, verbatim
// (tt:status.go:102-185, tt:join_twins.go:93-142), in model's vocabulary.
// One vocabulary covers both sources WITHOUT conflating them: blocked is not
// folded into waiting and working is not folded into busy.

// MapRegistryStatus maps a registry record's status and waitingFor. An
// unrecognised status is Unknown with no reason; the caller keeps the raw word.
// A reason is given only for waiting, and an absent or unrecognised waitingFor
// on a waiting session is WaitingUnknown: it IS waiting, for something we
// cannot name.
func MapRegistryStatus(rawStatus, rawWaitingFor string) (model.Status, model.WaitingReason) {
	switch rawStatus {
	case ValueStatusIdle:
		return model.StatusIdle, model.WaitingUnknown
	case ValueStatusBusy:
		return model.StatusBusy, model.WaitingUnknown
	case ValueStatusShell:
		return model.StatusShell, model.WaitingUnknown
	case ValueStatusWaiting:
		return model.StatusWaiting, mapWaitingFor(rawWaitingFor)
	default:
		return model.StatusUnknown, model.WaitingUnknown
	}
}

func mapWaitingFor(raw string) model.WaitingReason {
	switch raw {
	case ValueWaitingInput:
		return model.WaitingInput
	case ValueWaitingPermission:
		return model.WaitingPermission
	case ValueWaitingDialog:
		return model.WaitingDialog
	default:
		return model.WaitingUnknown
	}
}

// MapJobState maps a job record's state alone, with no tempo: the record's own
// word.
func MapJobState(rawState string) model.Status {
	switch rawState {
	case ValueStateBlocked:
		return model.StatusBlocked
	case ValueStateWorking:
		return model.StatusWorking
	case ValueStateDone:
		return model.StatusDone
	case ValueStateFailed:
		return model.StatusFailed
	default:
		return model.StatusUnknown
	}
}

// MapJobRecord maps state AND tempo. state is the end-of-turn classifier's
// outcome and tempo is what the permission bridge writes MID-turn, leaving
// state alone: a job stopped at a question reads working + tempo blocked for
// as long as it waits (measured 33 h), and a state-only mapping calls it
// working. The vendor's own listing reads the pair this way.
func MapJobRecord(rawState, rawTempo string) model.Status {
	s := MapJobState(rawState)
	if s == model.StatusWorking && rawTempo == ValueTempoBlocked {
		return model.StatusBlocked
	}
	return s
}

// MapAgentsStatus maps an agents-listing entry: an entry with a status speaks
// the registry's vocabulary, one with a state the job's.
func MapAgentsStatus(rawStatus, rawState, rawWaitingFor string) (model.Status, model.WaitingReason) {
	if rawStatus != "" {
		return MapRegistryStatus(rawStatus, rawWaitingFor)
	}
	if rawState != "" {
		return MapJobState(rawState), model.WaitingUnknown
	}
	return model.StatusUnknown, model.WaitingUnknown
}

// EffectiveInput is the speakers of one session (§3.4 rule E).
type EffectiveInput struct {
	Registry  *Record      // the registry speaker; nil = none
	Job       *Job         // the job speaker; nil = none
	Agents    *AgentsEntry // an admitted interactive entry; consulted only when Registry and Job are nil
	ProcAlive model.Liveness
}

// EffectiveStatus is the ONE status a session carries.
type EffectiveStatus struct {
	Status     model.Status
	Raw        string
	Since      *time.Time
	From       model.SourceID
	Waiting    model.WaitingReason
	WaitingRaw string
	Conflict   bool // twinsDisagree, E1 only
}

// Effective is rule E: tt's MapRegistryStatus, MapJobRecord, MapAgentsStatus
// and twinStatus, verbatim. Every consumer reads its result and nothing else.
func Effective(in EffectiveInput) EffectiveStatus {
	switch {
	case in.Job != nil && in.Registry != nil:
		v := twinStatus(*in.Job, *in.Registry, in.ProcAlive)
		v.Conflict = twinsDisagree(*in.Job, *in.Registry)
		return v
	case in.Job != nil:
		return fromJob(*in.Job)
	case in.Registry != nil:
		r := in.Registry
		st, why := MapRegistryStatus(r.StatusRaw, r.WaitingRaw)
		v := EffectiveStatus{Status: st, Raw: r.StatusRaw, Since: r.StatusUpdatedAt, From: model.SourceRegistry}
		if st == model.StatusWaiting {
			v.Waiting, v.WaitingRaw = why, r.WaitingRaw
		}
		return v
	case in.Agents != nil:
		e := in.Agents
		st, why := MapAgentsStatus(e.StatusRaw, e.StateRaw, e.WaitingRaw)
		v := EffectiveStatus{Status: st, Raw: e.StatusRaw, From: model.SourceAgents}
		if st == model.StatusWaiting {
			v.Waiting, v.WaitingRaw = why, e.WaitingRaw
		}
		return v
	default:
		return EffectiveStatus{}
	}
}

func fromJob(j Job) EffectiveStatus {
	return EffectiveStatus{
		Status: MapJobRecord(j.StateRaw, deref(j.Tempo)),
		Raw:    j.StateRaw,
		Since:  j.UpdatedAt,
		From:   model.SourceJobs,
	}
}

// twinStatus is the vendor's rule for a job and the process serving it
// (`claude agents --json`, identical in 2.1.276, 2.1.278 and 2.1.280):
//
//  1. the process is busy                                -> working
//  2. the job has settled (done/failed, tempo not active) -> that state
//  3. the job is blocked, or the process is waiting       -> blocked
//  4. otherwise                                           -> working
//
// One deliberate departure, tt's: a settled job whose process is still ALIVE
// is not ended; it reads as the process says (idle, or blocked when waiting).
// A process known to be dead is not believed: the job speaks alone.
func twinStatus(job Job, reg Record, procAlive model.Liveness) EffectiveStatus {
	jobStatus := MapJobRecord(job.StateRaw, deref(job.Tempo))
	j := EffectiveStatus{Status: jobStatus, Raw: job.StateRaw, Since: job.UpdatedAt, From: model.SourceJobs}

	if procAlive == model.LivenessNo {
		return j
	}
	regStatus, regReason := MapRegistryStatus(reg.StatusRaw, reg.WaitingRaw)
	fromReg := func(s model.Status) EffectiveStatus {
		v := EffectiveStatus{Status: s, Raw: reg.StatusRaw, Since: reg.StatusUpdatedAt, From: model.SourceRegistry}
		if regStatus == model.StatusWaiting {
			v.Waiting, v.WaitingRaw = regReason, reg.WaitingRaw
		}
		return v
	}

	switch {
	case regStatus == model.StatusBusy:
		return fromReg(model.StatusWorking)
	case jobStatus.IsTerminal() && deref(job.Tempo) != ValueTempoActive:
		if procAlive == model.LivenessYes {
			if regStatus == model.StatusWaiting {
				return fromReg(model.StatusBlocked)
			}
			return fromReg(model.StatusIdle)
		}
		return j
	case jobStatus == model.StatusBlocked:
		if regStatus == model.StatusWaiting {
			j.Waiting, j.WaitingRaw = regReason, reg.WaitingRaw
		}
		j.Status = model.StatusBlocked
		return j
	case regStatus == model.StatusWaiting:
		return fromReg(model.StatusBlocked)
	default:
		j.Status = model.StatusWorking
		return j
	}
}

// twinsDisagree reports whether the two records, read alone, would put the
// session in different places: one asking for the user and the other not, or
// one busy and the other not working. The merged answer stands; this says it
// was a choice.
func twinsDisagree(job Job, reg Record) bool {
	j := MapJobRecord(job.StateRaw, deref(job.Tempo))
	r, _ := MapRegistryStatus(reg.StatusRaw, reg.WaitingRaw)
	return j.IsWaiting() != r.IsWaiting() || (r == model.StatusBusy) != (j == model.StatusWorking)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
