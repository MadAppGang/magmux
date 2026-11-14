package model

import "fmt"

// Every enum in this package shares one shape: index 0 is "no opinion" and
// every other index is a determined value. enumName is the one place the
// String half of that shape is implemented, so the enums cannot drift apart.
//
// The spelling is the lower-case words after the type prefix, joined with "-"
// when there is more than one ("unknown", "idle", "no-transcript",
// "ambiguous-transcript"). A value outside the enum prints "<Type>(<n>)", so a
// corrupt value is visible as a corrupt value rather than claiming to be
// Unknown.
//
// No enum here has a JSON method, and no struct here has a json tag (guard G6):
// the wire belongs to each host.
func enumName(typeName string, names []string, i int) string {
	if i < 0 || i >= len(names) {
		return fmt.Sprintf("%s(%d)", typeName, i)
	}
	return names[i]
}

// Kind is what sort of session a record describes.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindInteractive
	KindBackground
)

var kindNames = []string{"unknown", "interactive", "background"}

func (k Kind) String() string { return enumName("Kind", kindNames, int(k)) }

// Status is ONE vocabulary, tt's union, used both for a session's effective
// status and for each source's own word. The zero value means "not
// determined", never "idle".
type Status uint8

const (
	StatusUnknown Status = iota
	StatusIdle
	StatusBusy
	StatusShell
	StatusWaiting
	StatusWorking
	StatusBlocked
	StatusDone
	StatusFailed
)

var statusNames = []string{
	"unknown", "idle", "busy", "shell", "waiting", "working", "blocked", "done", "failed",
}

func (s Status) String() string { return enumName("Status", statusNames, int(s)) }

// IsWaiting reports a status that needs a human: waiting or blocked (tt's
// IsAttention).
func (s Status) IsWaiting() bool { return s == StatusWaiting || s == StatusBlocked }

// IsWorking reports a status that is doing work: busy or working.
func (s Status) IsWorking() bool { return s == StatusBusy || s == StatusWorking }

// IsTerminal reports a status that has ended: done or failed.
func (s Status) IsTerminal() bool { return s == StatusDone || s == StatusFailed }

// WaitingReason is what a waiting session is blocked on.
type WaitingReason uint8

const (
	WaitingUnknown WaitingReason = iota
	WaitingInput
	WaitingPermission
	WaitingDialog
)

var waitingNames = []string{"unknown", "input", "permission", "dialog"}

func (w WaitingReason) String() string { return enumName("WaitingReason", waitingNames, int(w)) }

// Liveness is a tri-state answer to "does this still exist?". It is not a
// bool because the third answer is real and common: nobody asked, or nobody
// could tell. A nil port answers Unknown, never No.
type Liveness uint8

const (
	LivenessUnknown Liveness = iota
	LivenessYes
	LivenessNo
)

var livenessNames = []string{"unknown", "yes", "no"}

func (l Liveness) String() string { return enumName("Liveness", livenessNames, int(l)) }

// Confidence is how a status was reached.
type Confidence uint8

const (
	ConfidenceUnknown Confidence = iota
	ConfidenceInferred
	ConfidenceAuthoritative
)

var confidenceNames = []string{"unknown", "inferred", "authoritative"}

func (c Confidence) String() string { return enumName("Confidence", confidenceNames, int(c)) }

// Evidence says what a count rests on (D1).
type Evidence uint8

const (
	EvidenceUnknown Evidence = iota
	EvidenceAbsent
	EvidenceCounted
	EvidenceUnreadable
	EvidenceNoTranscript
	EvidenceAmbiguous
)

var evidenceNames = []string{"unknown", "absent", "counted", "unreadable", "no-transcript", "ambiguous"}

func (e Evidence) String() string { return enumName("Evidence", evidenceNames, int(e)) }

// ShellMark is what a shell's OSC 133 marks last said about its foreground.
// Unknown is by far the commonest value and means "this shell sends no
// marks", never "nothing is running".
type ShellMark uint8

const (
	ShellMarkUnknown ShellMark = iota
	ShellMarkPrompt
	ShellMarkRunning
)

var shellMarkNames = []string{"unknown", "prompt", "running"}

func (m ShellMark) String() string { return enumName("ShellMark", shellMarkNames, int(m)) }

// SourceID names one of the four sources a document reports on.
type SourceID uint8

const (
	SourceUnknown SourceID = iota
	SourceRegistry
	SourceJobs
	SourceAgents
	SourceWatch
)

var sourceIDNames = []string{"unknown", "registry", "jobs", "agents", "watch"}

func (s SourceID) String() string { return enumName("SourceID", sourceIDNames, int(s)) }

// SourceState is how a source fared in one cycle.
type SourceState uint8

const (
	SourceStateUnknown SourceState = iota
	SourceOff
	SourceOK
	SourceDegraded
	SourceFailed
)

var sourceStateNames = []string{"unknown", "off", "ok", "degraded", "failed"}

func (s SourceState) String() string { return enumName("SourceState", sourceStateNames, int(s)) }

// TargetKind is what a watch target is for. It decides the shedding order
// (D15): transcripts go first, then job transcripts, then job directories; the
// spine is never shed one by one.
type TargetKind uint8

const (
	TargetUnknown TargetKind = iota
	TargetSpine
	TargetJobDir
	TargetTranscript
	TargetJobTranscript
)

var targetKindNames = []string{"unknown", "spine", "job-dir", "transcript", "job-transcript"}

func (k TargetKind) String() string { return enumName("TargetKind", targetKindNames, int(k)) }

// Scope is what a Problem is about.
type Scope uint8

const (
	ScopeUnknown Scope = iota
	ScopeRecord
	ScopeSource
	ScopeWatch
	ScopeHost
)

var scopeNames = []string{"unknown", "record", "source", "watch", "host"}

func (s Scope) String() string { return enumName("Scope", scopeNames, int(s)) }

// ProblemKind is what went wrong.
type ProblemKind uint8

const (
	ProblemUnknown ProblemKind = iota
	ProblemTorn
	ProblemAbsent
	ProblemUnreadable
	ProblemPIDMismatch
	ProblemUndecoded
	ProblemMissingField        // D11
	ProblemImplausibleInstant  // D4
	ProblemDuplicateKey        // §3.4 rule 2
	ProblemDuplicateJobClaim   // §3.4 rule 2
	ProblemShortIDMismatch     // K2
	ProblemAmbiguousTranscript // D5
	ProblemOutsideRoot
	ProblemNotGranted // C17
	ProblemWatchArm
	ProblemWatchLimit // EMFILE / ENOSPC
	ProblemOverBudget // D15
	ProblemNotConverged
	ProblemAgentsListing
	ProblemHost
)

var problemKindNames = []string{
	"unknown", "torn", "absent", "unreadable", "pid-mismatch", "undecoded",
	"missing-field", "implausible-instant", "duplicate-key", "duplicate-job-claim",
	"short-id-mismatch", "ambiguous-transcript", "outside-root", "not-granted",
	"watch-arm", "watch-limit", "over-budget", "not-converged", "agents-listing", "host",
}

func (k ProblemKind) String() string { return enumName("ProblemKind", problemKindNames, int(k)) }

// PaneRef is an opaque host pane handle. "" means no pane. A host formats its
// own handle into it (magmux an int, tt a "%107"-style id) and parses it back;
// the library never looks inside.
type PaneRef string
