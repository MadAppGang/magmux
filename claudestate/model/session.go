package model

import (
	"slices"
	"time"
)

// Session is one observed session at one instant.
//
// It is a plain value: no json tags, and no method that reads a clock. A host
// derives ages at encode time from the document's epoch.
//
// The six EFFECTIVE fields (Status, StatusRaw, StatusSince, StatusFrom,
// Waiting, WaitingRaw) are what every consumer reads. They are computed once,
// by the join, and only verdict's inferred tier may later fill Status, when
// the join left it Unknown with an empty StatusRaw (C19).
type Session struct {
	Key Key
	// PID is the registry speaker's pid, else the admitted agents entry's;
	// 0 = neither (§3.4 rule 2).
	PID int
	// Claimants is every registry claimant's pid, ascending; nil when there
	// is none.
	Claimants []int
	// Kind: Background when a job claims the key; else the record's own kind.
	Kind    Kind
	KindRaw string

	Name, NameSource string
	PreviousNames    []PreviousName // order preserved
	// CWD is the LIVE directory a record states: the registry speaker's cwd,
	// else the job's, else the agents entry's.
	CWD string
	// LaunchDir is the directory the tool was LAUNCHED in, as a HOST knows
	// it. The library never sets it (§3.4 rule 2).
	LaunchDir string
	// StartedAt is registry startedAt, else the job's createdAt, else the
	// agents entry's startedAt.
	StartedAt *time.Time

	// The EFFECTIVE status (§3.4 rule E).
	Status    Status
	StatusRaw string
	// StatusSince is a record's own instant (statusUpdatedAt or the job's
	// updatedAt), never a file's mtime (D7).
	StatusSince *time.Time
	// StatusFrom is which record decided Status; SourceUnknown when none did,
	// or when verdict's inferred tier filled it (C19).
	StatusFrom SourceID
	// Waiting and WaitingRaw are non-Unknown / non-"" only when
	// Status.IsWaiting().
	Waiting    WaitingReason
	WaitingRaw string

	// Registry is the speaking registry record's own words; nil = no
	// registry claimant.
	Registry            *RegistryFacts
	Version             string
	MessagingSocketPath *string
	Tmux                *TmuxRef // parsed from the record's tmux value
	Pane                PaneRef
	// PaneAlive is "the pane is part of the host", NOT "a process runs in it".
	PaneAlive Liveness
	// ProcessAlive is the liveness of PID; Unknown when PID == 0.
	ProcessAlive Liveness
	// ListedBy is membership: registry, jobs, agents — in that order, no
	// duplicates.
	ListedBy []SourceID
	// Job is non-nil iff a job record claims this key.
	Job *JobFacts
	// Transcript is nil when not located, or not read this cycle.
	Transcript *TranscriptFacts
	Confidence Confidence
	Conflict   *Conflict
	Attention  Attention
	Fault      Fault
}

// WaitingApplies reports whether the effective status needs a human (D14).
func (s Session) WaitingApplies() bool { return s.Status.IsWaiting() }

// Clone returns a deep copy: no pointer or slice is shared with s.
func (s Session) Clone() Session {
	c := s
	c.Claimants = slices.Clone(s.Claimants)
	if s.PreviousNames != nil {
		c.PreviousNames = make([]PreviousName, len(s.PreviousNames))
		for i, p := range s.PreviousNames {
			p.Until = clonePtr(p.Until)
			c.PreviousNames[i] = p
		}
	}
	c.StartedAt = clonePtr(s.StartedAt)
	c.StatusSince = clonePtr(s.StatusSince)
	if s.Registry != nil {
		r := *s.Registry
		r.StatusSince = clonePtr(r.StatusSince)
		r.UpdatedAt = clonePtr(r.UpdatedAt)
		c.Registry = &r
	}
	c.MessagingSocketPath = clonePtr(s.MessagingSocketPath)
	c.Tmux = clonePtr(s.Tmux)
	c.ListedBy = slices.Clone(s.ListedBy)
	if s.Job != nil {
		j := *s.Job
		j.Tempo = clonePtr(j.Tempo)
		j.Needs = clonePtr(j.Needs)
		j.Detail = clonePtr(j.Detail)
		j.Tokens = clonePtr(j.Tokens)
		j.Children = slices.Clone(j.Children)
		j.CreatedAt = clonePtr(j.CreatedAt)
		j.UpdatedAt = clonePtr(j.UpdatedAt)
		j.ReapedMidWorkAt = clonePtr(j.ReapedMidWorkAt)
		c.Job = &j
	}
	if s.Transcript != nil {
		t := *s.Transcript
		t.APIErrors = clonePtr(t.APIErrors)
		t.Color = clonePtr(t.Color)
		t.Subagents = clonePtr(t.Subagents)
		t.ContextTokens = clonePtr(t.ContextTokens)
		t.Compactions = clonePtr(t.Compactions)
		t.LastActivityAt = clonePtr(t.LastActivityAt)
		t.LatestTextAt = clonePtr(t.LatestTextAt)
		c.Transcript = &t
	}
	c.Conflict = clonePtr(s.Conflict)
	c.Attention.Needed = clonePtr(s.Attention.Needed)
	c.Fault.APIErrors = clonePtr(s.Fault.APIErrors)
	return c
}

// RegistryFacts are the speaking registry record's own words.
type RegistryFacts struct {
	File        string // base name of the speaking record
	Status      Status // MapRegistryStatus of this record alone
	StatusRaw   string
	StatusSince *time.Time // statusUpdatedAt, verbatim
	Waiting     WaitingReason
	WaitingRaw  string
	UpdatedAt   *time.Time // the record's updatedAt, verbatim
}

// PreviousName is a name a session used to have.
type PreviousName struct {
	Name      string
	Until     *time.Time
	SessionID string
}

// TmuxRef is the tmux value a record carries. It is a fact Claude wrote about
// the session, not tmux's API. Sigils are kept verbatim; an unparseable value
// sets only Raw.
type TmuxRef struct{ Raw, Session, Window, Pane string }

// JobFacts are one background job record's own words.
type JobFacts struct {
	Dir string // the directory name
	// Short is daemonShort, else Dir, whatever the key: it equals Key.Job for
	// a K2 key, and Key.Job is "" for an orphan job.
	Short string
	// State is MapJobState(state) alone, no tempo: the record's own word.
	State                                 Status
	StateRaw                              string
	Tempo                                 *string
	Needs, Detail                         *string
	Tokens                                *int64
	Children                              []string
	CreatedAt, UpdatedAt, ReapedMidWorkAt *time.Time
	// Name, NameSource and CWD are the job record's own; the Session fields
	// prefer the registry's.
	Name, NameSource, CWD                     string
	ResumeSessionID, LinkScanPath, CLIVersion string
	Twin                                      bool // a registry claimant shares this key (D6)
	Conflict                                  bool // the two records alone would disagree
}

// Location is where a session's transcript was found, this cycle.
type Location struct {
	// Path is relative to Root.Projects.FS, slash-separated; "" when
	// Matches != 1.
	Path string
	// Matches: 0 = not found; >= 2 = ambiguous, and nothing is opened (D5).
	Matches int
	// Declared: located from a declared path or session id (FR6); never
	// falls back.
	Declared bool
}

// Window describes one bounded read of a file's tail (D10).
type Window struct {
	TailBytes int
	FileSize  int64
	Partial   bool // the file is larger than the tail read
}

// TranscriptFacts are what one read of a transcript's tail established.
type TranscriptFacts struct {
	Location         Location
	Window           Window // the facts window
	APIErrors        *int   // isApiErrorMessage only; nil = not read
	LastAPIError     string // raw text; the host decides exposure (D13)
	Color            *string
	Subagents        *int
	SubagentEvidence Evidence // D1, C7
	ContextTokens    *int64   // D3
	Model            string
	Compactions      *int
	TimingWindow     Window     // a suffix of the same read
	LastActivityAt   *time.Time // newest timestamped record in the timing window
	LatestText       string     // newest assistant text in the timing window, raw
	LatestTextAt     *time.Time
}

// Attention is verdict's answer to "does a human need to look at this?".
// Needed nil means undetermined; Reason is verdict's words.
type Attention struct {
	Needed *bool
	Reason string
}

// Fault is a session's API-error evidence. It is orthogonal to Status.
type Fault struct {
	APIErrors   *int
	Window      Window
	LastMessage string
}

// Conflict is two tiers disagreeing about a session's status.
type Conflict struct {
	Confidence Confidence
	Status     Status
	Detail     string
}

// ShellState is what the generic tier knows about one pane's shell.
type ShellState struct {
	// AtPrompt is the foreground-group answer behind the job-control latch.
	// LivenessNo means "not at a prompt", NOT "work is happening".
	AtPrompt Liveness
	Mark     ShellMark
	// Exit is the last completed command's status; nil = none reported.
	Exit *int
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
