package obsview

import (
	"fmt"
	"strconv"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// FromModel is THE mapping from the library's document to magmux's wire, and
// the only one: the CLI, the socket verb, MCP, the panel and the audit trail
// all see a session through it (claudestate architecture §4.9). It reads only
// the EFFECTIVE status fields, so every surface reports the one status the
// join decided.
func FromModel(f model.Fleet) Fleet {
	out := Fleet{Epoch: f.Epoch, Sessions: make([]Session, 0, len(f.Sessions))}
	for _, s := range f.Sessions {
		out.Sessions = append(out.Sessions, FromModelSession(s))
	}
	return out
}

// FromModelSession maps one session. See FromModel.
func FromModelSession(s model.Session) Session {
	w := Session{
		ID:           s.Key.SessionID,
		PID:          s.PID,
		Job:          s.Key.Job,
		Name:         s.Name,
		LaunchDir:    s.LaunchDir,
		Status:       statusOf(s.Status),
		StatusRaw:    s.StatusRaw,
		Confidence:   tierOf(s.Confidence),
		StatusSince:  clonePtr(s.StatusSince),
		ProcessAlive: livenessOf(s.ProcessAlive),
		PaneAlive:    livenessOf(s.PaneAlive),
		Attention:    Attention{Needed: clonePtr(s.Attention.Needed), Reason: s.Attention.Reason},
		Fault:        Fault{APIErrors: clonePtr(s.Fault.APIErrors), Detail: faultDetail(s.Fault)},
	}
	if id, ok := PaneID(s.Pane); ok {
		w.PaneID = &id
	}
	for _, p := range s.PreviousNames {
		w.PreviousNames = append(w.PreviousNames, p.Name)
		w.PreviousNameDetail = append(w.PreviousNameDetail, PreviousName{
			Name: p.Name, Until: clonePtr(p.Until), Session: p.SessionID,
		})
	}
	// The waiting reason only where it applies (D14): a session that is not
	// waiting has no reason on the wire, and "dialog open" — which magmux's
	// enum has no word for — rides as the raw word beside a null.
	if s.WaitingApplies() {
		switch s.Waiting {
		case model.WaitingInput:
			w.Waiting = WaitingReasonInput
		case model.WaitingPermission:
			w.Waiting = WaitingReasonPermission
		}
		w.WaitingRaw = s.WaitingRaw
	}
	if s.Conflict != nil {
		w.Conflict = &Conflict{Tier: tierOf(s.Conflict.Confidence), Status: statusOf(s.Conflict.Status), Detail: s.Conflict.Detail}
	}
	if t := s.Transcript; t != nil {
		w.SubagentCount = clonePtr(t.Subagents)
		w.Color = clonePtr(t.Color)
	}
	return w
}

// statusOf maps the library's ONE vocabulary (tt's union) onto magmux's four
// words, adding none (architecture A4):
//
//   - idle, busy, shell and waiting are the same word;
//   - a job's working is busy, and its blocked is waiting;
//   - done, failed and unknown are null, with the raw word in statusRaw. A
//     finished job is therefore null with confidence authoritative: the job
//     said it, in a word magmux's enum does not have (D6).
func statusOf(st model.Status) Status {
	switch st {
	case model.StatusIdle:
		return StatusIdle
	case model.StatusBusy, model.StatusWorking:
		return StatusBusy
	case model.StatusShell:
		return StatusShell
	case model.StatusWaiting, model.StatusBlocked:
		return StatusWaiting
	}
	return StatusUnknown
}

func tierOf(c model.Confidence) Tier {
	switch c {
	case model.ConfidenceInferred:
		return TierInferred
	case model.ConfidenceAuthoritative:
		return TierAuthoritative
	}
	return TierUnknown
}

func livenessOf(l model.Liveness) Liveness {
	switch l {
	case model.LivenessYes:
		return LivenessYes
	case model.LivenessNo:
		return LivenessNo
	}
	return LivenessUnknown
}

// PaneRef is a magmux pane id as the library's opaque handle (ADR-X1): its
// decimal spelling.
func PaneRef(id int) model.PaneRef { return model.PaneRef(strconv.Itoa(id)) }

// PaneID parses a handle PaneRef made back into a pane id. ok is false for
// "", for anything that is not a decimal integer, and for a negative id:
// none of those is an address in magmux's table.
func PaneID(ref model.PaneRef) (int, bool) {
	if ref == "" {
		return 0, false
	}
	id, err := strconv.Atoi(string(ref))
	if err != nil || id < 0 || strconv.Itoa(id) != string(ref) {
		return 0, false
	}
	return id, true
}

// faultDetail is the wire's fault.detail: the last API error's text and, when
// the facts window was only part of the transcript, how much was examined —
// on a zero count as well as on a count, because "no error in the last 200
// KB" is not "no error" (D10). The library discloses the window; the wording
// is the host's.
func faultDetail(f model.Fault) string {
	if f.APIErrors == nil {
		return ""
	}
	window := ""
	if f.Window.Partial {
		window = fmt.Sprintf("the last %s of a %s transcript",
			sizeText(int64(f.Window.TailBytes)), sizeText(f.Window.FileSize))
	}
	if *f.APIErrors == 0 {
		if window == "" {
			return ""
		}
		return "no API-level error in " + window + "; errors before that are not counted"
	}
	detail := f.LastMessage
	if detail == "" {
		detail = "an API-level error was recorded"
	}
	if window != "" {
		detail += " (counted in " + window + "; errors before that are not counted)"
	}
	return detail
}

func sizeText(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", n>>10)
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
}
