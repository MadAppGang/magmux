package obs

import (
	"cmp"
	"slices"
	"time"
)

// Session is one observed agent session, as of one instant.
//
// Every field is either required and always meaningful, or optional and
// spelled so that "undetermined" is representable: a pointer whose nil means
// no opinion, or an enum whose zero value is Unknown. There is no field whose
// absence has to be guessed from another field's value.
//
// The three axes are deliberately separate and none of them is derivable from
// the others:
//
//   - [Status] is what the session is DOING.
//   - [Liveness] is whether the session still EXISTS, which is a fact about
//     the process and the pane, not about the work.
//   - [Fault] is what has gone WRONG, which an idle session can have and a
//     busy session can be free of.
//
// Folding any two of them together is the mistake this struct is shaped to
// prevent. A session that errored an hour ago and is now waiting for the user
// is `StatusWaiting` with a non-empty Fault, and a consumer that only knows
// about "error" or "waiting" cannot say that.
type Session struct {
	// ID is the tool's own session identifier, kept verbatim. It is the only
	// identifier that survives a restart of anything magmux owns.
	ID string `json:"id"`

	// PID is the process magmux spawned, and is the subject of ProcessAlive.
	PID int `json:"pid"`

	// PaneID is the magmux pane the session is running in. nil when the
	// session is not one of magmux's panes — which is not hypothetical:
	// discovery can see a session magmux did not start.
	PaneID *int `json:"paneId"`

	// Name is the session's display name, and PreviousNames the ones it has
	// had before, preserved in order rather than flattened to the latest. A
	// session that was renamed is still findable under what a human last
	// called it.
	Name          string   `json:"name,omitempty"`
	PreviousNames []string `json:"previousNames,omitempty"`

	// PreviousNameDetail is PreviousNames with what the tool recorded beside
	// each name: when it stopped being current, and the session id it was
	// recorded under. It is PARALLEL to PreviousNames — same length, same
	// order, PreviousNameDetail[i].Name == PreviousNames[i] — and the decoder
	// writes both from one loop, so they cannot disagree.
	//
	// # Why a second key rather than a richer previousNames
	//
	// previousNames is on the wire as an array of strings, and a consumer
	// reading it would break if its elements became objects. So it keeps its
	// shape, and the detail rides beside it as an ADDITIVE key: a consumer
	// that knows nothing of it sees exactly the document it saw before, and
	// one that wants the "until when" reads it here. That is a wire-vocabulary
	// change under architecture-v4 §9, made in fix round 2, and it is the
	// compatible one of the two available.
	PreviousNameDetail []PreviousName `json:"previousNameDetail,omitempty"`

	// LaunchDir is the directory the tool was LAUNCHED in, which is not
	// necessarily the directory it is working in now. It is recorded because
	// it is the input to the tool's own on-disk layout — see
	// adapters.EncodeProjectDir, where using the live directory instead is a
	// silent 4%-wrong answer.
	LaunchDir string `json:"launchDir,omitempty"`

	// Status is what the session is doing, and StatusRaw is the string the
	// tool actually wrote, kept verbatim so that a value magmux does not
	// recognise is still discoverable rather than merely absent. StatusRaw is
	// the ONLY source of truth when Status is StatusUnknown.
	Status    Status `json:"status"`
	StatusRaw string `json:"statusRaw,omitempty"`

	// Confidence is which [Tier] the published Status came from, and it is a
	// field rather than a thing to infer: "the transcript says busy" and "the
	// screen has been quiet, and there is no transcript" are the same word on
	// the wire and two very different claims behind it.
	Confidence Tier `json:"confidence"`

	// Conflict is what the LOSING tier said, when the two disagreed. Absent —
	// not null — when they agreed or when only one of them had anything to
	// say. See [Conflict], which explains at length why a conflict is not an
	// error.
	Conflict *Conflict `json:"conflict,omitempty"`

	// StatusSince is when the session ENTERED its current status, verbatim
	// from whatever reported it. nil means undetermined.
	//
	// There is deliberately no "in this status for" field beside it: see
	// [Session.InStatusFor]. There is also deliberately no correction applied
	// to it — a status that has not changed for three days is a fact about the
	// session, not a stale record, and cross-checking it against unrelated
	// file activity manufactures wrong answers out of a correctly written
	// field.
	StatusSince *time.Time `json:"statusSince"`

	// Waiting is what a StatusWaiting session is blocked on, and WaitingRaw
	// the string the tool wrote, on the same terms as StatusRaw.
	Waiting    WaitingReason `json:"waitingOn"`
	WaitingRaw string        `json:"waitingOnRaw,omitempty"`

	// ProcessAlive and PaneAlive are the two independent gates on whether a
	// session should be reported at all. They are separate fields because they
	// fail separately and a consumer needs to know which one rejected a
	// session: a dead process and a vanished pane are different situations
	// with different remedies.
	ProcessAlive Liveness `json:"processAlive"`
	PaneAlive    Liveness `json:"paneAlive"`

	// SubagentCount is how many subagents the session has running. nil is "we
	// did not determine this"; a pointer to 0 is a determined zero. The two
	// are different answers, and which one is true is the difference between
	// a session that has finished its work and one nobody has counted.
	SubagentCount *int `json:"subagentCount"`

	// Color is the session's assigned colour. nil is "no record of one",
	// which is NOT the same as a session explicitly coloured with the
	// default — the empty string is a value here, not an absence.
	Color *string `json:"color"`

	// Attention is whether a human needs to look at this session.
	Attention Attention `json:"attention"`

	// Fault is what has gone wrong. Its zero value is "no opinion", not "no
	// faults", and it is ORTHOGONAL to Status.
	Fault Fault `json:"fault"`
}

// PreviousName is one name a session was known by before its current one.
//
// # Until is kept because the tool shows it
//
// Claude Code's own session listing renders a renamed peer as "says it was
// agent-fleet-sessions until 5m ago" — the name AND the instant. A former name
// reduced to its bare string is the flattening requirements.md R2 forbids, one
// level down: "it was called X" without "until when" cannot say which of two
// names a person used last week.
//
// # Session is published, verbatim, and its meaning is NOT claimed
//
// Claude Code writes a session id beside each former name. A former name
// recorded under a DIFFERENT id from the session's own is what a resumed or
// forked session would plausibly look like, and if so it is the only trail
// from this session back to the transcript that name belongs to — which is
// the reason not to drop it. But that reading is a hypothesis: the one live
// record carrying the field (Claude Code 2.1.276, 2026-09-19) records the
// session's OWN id, and no differing id has been observed. So the value is
// published exactly as written, like StatusRaw, and nothing here interprets
// it — a consumer compares it with [Session.ID] itself.
type PreviousName struct {
	// Name is the former name, verbatim.
	Name string `json:"name"`

	// Until is when the name stopped being current, verbatim from the tool.
	// nil is "not recorded": a string-shaped former name carries no instant,
	// and one whose instant is not a plausible epoch-millisecond value is not
	// guessed at. It is NEVER omitted, because null is how this document says
	// undetermined and an absent key would read as "this format has no such
	// thing".
	Until *time.Time `json:"until"`

	// Session is the session id the tool recorded beside the name, or "" when
	// it recorded none. See the type comment for what it is not claimed to
	// mean.
	Session string `json:"session,omitempty"`
}

// Attention is whether a human needs to look at a session.
//
// Needed has three states and nil is one of them. It is a *bool rather than a
// bool because a flag that was never determined is not the same as a flag that
// was determined to be false, and coercing the first into the second is how a
// session that needs a human goes unreported: the UI cannot tell "no" from
// "we never looked".
//
// This type is the VALUE. The rule that produces it — which facts make it
// true, which make it false, and which leave it nil — is written out in one
// place, the package documentation of
// [github.com/MadAppGang/magmux/internal/obs/verdict], so that a reader who
// disagrees with the policy can argue with it instead of reconstructing it
// from branches.
type Attention struct {
	// Needed is nil when undetermined. It must never be silently coerced to
	// false, and it marshals to JSON null so that a consumer sees the
	// undetermined case as a third value rather than as a negative.
	Needed *bool `json:"needed"`

	// Reason says WHY, in magmux's own words, and is where the uncertainty
	// behind a nil Needed is explained. It is free text: it is shown to a
	// human, never switched on.
	Reason string `json:"reason,omitempty"`
}

// Fault is what has gone wrong in a session.
//
// It is a field of [Session] beside Status, not a Status value, because the
// two answer different questions and a session can be both. The zero value is
// no opinion: no faults were determined, which is not a claim that there are
// none.
type Fault struct {
	// APIErrors is how many API-level errors the session has hit RECENTLY:
	// the count within the last 200 KB of its transcript, which for any
	// transcript larger than that is the recent part of the session and not
	// all of it. Errors before the window are not counted, and when the window
	// is partial Detail says so — on a zero as well as on a count.
	//
	// RECENT is a decision, made in fix round 1 and recorded as reversible.
	// Attention rule 3 is the consumer that acts on this field, and an error
	// from twenty megabytes ago is not a reason to interrupt a human now; a
	// whole-session count would make rule 3 fire for any session that had
	// ever hit one. The key keeps its name because renaming it is a schema
	// change. Measured on 2026-09-19: 13 of 36 live sessions had errors only
	// outside the window, and each correctly reads 0 here — with the window
	// disclosed, which until that round it was not.
	//
	// nil is "we did not determine this" — most often because no transcript
	// was found at all — and a pointer to 0 is a determined zero for the
	// window. A session whose transcript could not be located has no error
	// count, and reporting that as zero errors is the difference between
	// "healthy" and "unexamined".
	//
	// Errors a tool RECOVERED from do not belong here. A count that includes
	// them flags healthy sessions as broken, which was measured at 29 of 32.
	APIErrors *int `json:"apiErrors"`

	// Detail is a human-readable note, in magmux's own words, about the most
	// recent fault — and, whenever the count covers only part of the
	// transcript, how much: "no API-level error in the last 200 KB of a 14.2
	// MB transcript; errors before that are not counted". Free text; never
	// switched on. It is empty for a zero counted over the WHOLE transcript,
	// which is exact and needs no qualification.
	Detail string `json:"detail,omitempty"`
}

// InStatusFor reports how long the session has been in its current status, as
// of now. ok is false when StatusSince was never determined.
//
// The duration is computed here rather than stored on [Session] on purpose. A
// stored duration is wrong the moment after it is written, and it would have
// to be recomputed by every reader anyway; storing it just gives a reader the
// chance to believe a number that has silently expired.
//
// The result is AN ANSWER, not a lower bound.
//
// This comment used to say the opposite — that the duration under-reports,
// because the instant behind it is restamped by events that are not status
// changes. That was refuted by measurement on 2026-09-18: a session sat busy
// for 31.7 minutes of continuous work with exactly ONE write, the transition
// into busy, and a 15-minute probe at 3-second intervals saw no restamp at
// all. The record is written when something CHANGES, so an idle session's
// instant is old and exact, and the duration derived from it is the real
// elapsed time rather than a floor under it.
//
// Said with its limits, because it is one window on one machine: the claim of
// restamping came from two observations, 626s and 355s apart, which the
// measurement neither reproduced nor explained away. "Exact as measured" is
// the honest reading, not "restamping is impossible". Either way the duration
// is reported and never CORRECTED — adjusting it would need an activity signal
// that does not exist, and the field it would overwrite is the only source for
// how long a session has been blocked.
//
// A negative result is returned as-is. It means the reader's clock is behind
// the instant it was handed, which is a fact worth seeing rather than one to
// clamp away.
func (s Session) InStatusFor(now time.Time) (time.Duration, bool) {
	if s.StatusSince == nil {
		return 0, false
	}
	return now.Sub(*s.StatusSince), true
}

// Clone returns a deep copy: no pointer and no slice is shared with the
// original, so the copy can be read by anything, for as long as it likes,
// while the original goes on changing.
//
// This is the operation the publish boundary is built on. A document is copied
// once, on the way out, and is never touched again afterwards — which is what
// makes it safe to hand to a reader that holds no lock.
func (s Session) Clone() Session {
	c := s
	c.PaneID = clonePtr(s.PaneID)
	c.PreviousNames = slices.Clone(s.PreviousNames)
	if s.PreviousNameDetail != nil {
		// Element by element: each one carries a pointer, and a shallow
		// slices.Clone would share every Until with the original.
		c.PreviousNameDetail = make([]PreviousName, len(s.PreviousNameDetail))
		for i, p := range s.PreviousNameDetail {
			p.Until = clonePtr(p.Until)
			c.PreviousNameDetail[i] = p
		}
	}
	c.StatusSince = clonePtr(s.StatusSince)
	c.SubagentCount = clonePtr(s.SubagentCount)
	c.Color = clonePtr(s.Color)
	c.Attention.Needed = clonePtr(s.Attention.Needed)
	c.Conflict = clonePtr(s.Conflict)
	c.Fault.APIErrors = clonePtr(s.Fault.APIErrors)
	return c
}

// Fleet is the set of sessions observed at ONE instant.
//
// The epoch belongs to the document and not to each session, and that is the
// whole reason this type exists rather than a bare slice. The underlying
// records mutate between reads — a sixth of them were measured changing status
// inside five minutes — so a consumer comparing two documents has to compare
// when they were taken, and a per-session timestamp cannot tell it whether two
// sessions were seen together or an hour apart.
type Fleet struct {
	// Epoch is when this document was taken, from the reader's clock. It is
	// not optional: a document that does not say when it was true is claiming
	// to be durable, which it is not.
	Epoch time.Time `json:"epoch"`

	// Sessions is every session observed at Epoch. Order is the caller's
	// choice; see [Fleet.Sort] for a deterministic one.
	Sessions []Session `json:"sessions"`
}

// Clone returns a deep copy of the document, sessions included.
func (f Fleet) Clone() Fleet {
	c := f
	if f.Sessions != nil {
		c.Sessions = make([]Session, len(f.Sessions))
		for i, s := range f.Sessions {
			c.Sessions[i] = s.Clone()
		}
	}
	return c
}

// Sort orders the sessions by PID and then by ID, in place.
//
// No ordering is specified anywhere in the requirements, and this is magmux's
// answer rather than a discovered one. It exists because the alternative is Go
// map iteration order, which makes every diff between two documents noise —
// and this document is meant to be diffed.
func (f Fleet) Sort() {
	slices.SortFunc(f.Sessions, func(a, b Session) int {
		if c := cmp.Compare(a.PID, b.PID); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}
