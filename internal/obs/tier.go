package obs

// Tier is where a fact came from, and therefore how much it can be trusted.
//
// There are exactly TWO determined tiers and there will not be a third.
//
//   - [TierInferred] is TERMINAL-derived: what the pane's own PTY traffic
//     implies, with no help from the tool running inside it. The staleness
//     cascade is the whole of it today; tcgetpgrp and OSC 133 join it later.
//     It can observe any tool, including one nobody has written an adapter
//     for, and it is a guess about a session made from the outside.
//   - [TierAuthoritative] is HARNESS-derived: the tool's own record of
//     itself — Claude Code's session registry and its transcript. It is what
//     the session says about the session.
//
// # Why there is no third tier
//
// A `stale_authoritative` tier was proposed to answer "what if the harness
// stops writing its log?" and was REJECTED, and the rejection is recorded here
// rather than in a plan because a plan is not where someone about to add one
// will look. That question is already answered by the two liveness gates, both
// of which are direct evidence rather than inference from a file's mtime: a
// harness that stopped writing because its process died is ProcessAlive no,
// and one that stopped because the pane was closed is PaneAlive no. A third
// tier would re-answer a question two fields already answer, and the day the
// three disagreed there would be no rule for which of them was right.
//
// # The zero value
//
// [TierUnknown] is "no tier", which is what a fact nobody established has. It
// marshals to null like every other undetermined value in this package, and it
// is never a claim that a fact is untrustworthy — only that nothing produced
// it.
type Tier uint8

const (
	// TierUnknown is the zero value: this fact came from nowhere, because
	// nothing determined it.
	TierUnknown Tier = iota
	// TierInferred is terminal-derived: implied by the pane's traffic.
	TierInferred
	// TierAuthoritative is harness-derived: the tool's own record of itself.
	TierAuthoritative
)

// These names are OURS outright — nothing magmux observes writes them, unlike
// the status vocabulary, which mirrors Claude's spelling on purpose. They are
// the two words architecture v4 §4 uses for the two tiers, kept verbatim so
// that the document and the wire read the same.
var tierEnum = enumSpec{"Tier", []string{
	"unknown",
	"inferred",
	"authoritative",
}}

// String returns the wire word.
func (t Tier) String() string { return tierEnum.name(int(t)) }

// MarshalJSON writes null for [TierUnknown] and the value's wire word
// otherwise.
func (t Tier) MarshalJSON() ([]byte, error) { return tierEnum.marshal(int(t)) }

// UnmarshalJSON reads null as [TierUnknown]. An unrecognised name is an error.
func (t *Tier) UnmarshalJSON(b []byte) error {
	i, err := tierEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*t = Tier(i)
	return nil
}

// Conflict is what the tier that LOST said, published beside the winner
// instead of being dropped.
//
// # A conflict is not an error, and nothing here is broken
//
// The obvious reading of a field with this name is "something has gone wrong",
// and that reading is wrong in every case this field exists for. Both tiers
// were working correctly and both reported honestly. They disagree because the
// session is in a state one of them cannot see: a pane that has produced no
// output for ten minutes while its own record says it is working is a pane
// whose screen is telling the truth about the screen and whose record is
// telling the truth about the last thing the tool wrote down. Neither is a
// malfunction. What has happened is that the two windows onto the session
// stopped agreeing, and that is a fact about the SESSION.
//
// # Why the loser is published at all
//
// Resolution without publication would be silent. "Authoritative wins" is the
// right rule and it is not in doubt, but a consumer that only ever sees the
// winner cannot tell an agreed answer from an overruled one — and the case
// where they disagree is precisely the case a human is being asked to look at.
// Publishing the loser is what turns "the screen is lying" from something
// magmux knows into something a consumer can see.
//
// The field is ABSENT, not null, when the tiers agree or when only one of them
// had anything to say. There is no conflict to describe, and a null would
// invite a reader to wonder which conflict was undetermined.
type Conflict struct {
	// Tier is the tier whose answer was NOT published. It is spelled out
	// rather than implied because a reader should not have to know the
	// resolution rule to read the document — and today it is always
	// [TierInferred], since authoritative always wins.
	Tier Tier `json:"tier"`

	// Status is what that tier said. It is the same vocabulary as
	// [Session.Status], so the two can be compared without translating.
	Status Status `json:"status"`

	// Detail says why the two disagree, in magmux's own words. Free text: it
	// is shown to a human and never switched on.
	Detail string `json:"detail,omitempty"`
}
