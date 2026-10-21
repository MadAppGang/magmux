package obs

// Status is what an observed session is doing.
//
// The zero value is [StatusUnknown] and means magmux has not determined the
// status — never that the session is idle. That distinction is the whole
// point: "idle" is an observation and "unknown" is the absence of one, and a
// model that spells them the same way reports a session as finished when it
// has merely not been looked at yet.
//
// Status is ORTHOGONAL to [Fault]. A session that has hit an API error still
// has a status, and a session with no faults is not thereby idle. Folding one
// into the other loses a fact a user needs; see [Session].
type Status uint8

const (
	// StatusUnknown is the zero value: no opinion.
	StatusUnknown Status = iota
	// StatusIdle is a session waiting for its next instruction, with nothing
	// outstanding.
	StatusIdle
	// StatusBusy is a session working on a turn.
	StatusBusy
	// StatusShell is a session that has handed its terminal to a shell.
	StatusShell
	// StatusWaiting is a session blocked on the user. What it is blocked on is
	// a separate axis; see [WaitingReason].
	StatusWaiting
)

// The names are the WIRE vocabulary — what a Status marshals to and what
// String reports — and they deliberately MIRROR Claude Code's own spelling.
//
// A neutral vocabulary (working / blocked / ready) was considered and
// rejected. Dropping Codex left one harness, and with one harness a
// translation layer buys no insulation: it costs every reader a second set of
// words to hold in their head and protects nothing. The insulation argument is
// indifferent to the spelling anyway — if Claude renames a value,
// adapters/schema.go absorbs the rename and our wire word stays put, whichever
// word we picked.
//
// So "idle" here and "idle" in Claude's registry are HOMOGRAPHS: same
// spelling, different owners, independent lifetimes. That is precisely why
// NFR6's test is decode-scoped. A word in this table is our own output
// vocabulary; only a site that DECODES Claude's string is a second copy of
// Claude's schema.
//
// Index 0 is not a wire value. It is the display spelling for String, and an
// undetermined value marshals to null and to nothing else.
var statusEnum = enumSpec{"Status", []string{
	"unknown",
	"idle",
	"busy",
	"shell",
	"waiting",
}}

// String returns the wire word, so that a value printed in a failing test and
// the same value on the wire read identically. A value outside the enum prints
// its ordinal; see enumSpec.name.
func (s Status) String() string { return statusEnum.name(int(s)) }

// MarshalJSON writes null for [StatusUnknown] and the value's wire word
// otherwise.
func (s Status) MarshalJSON() ([]byte, error) { return statusEnum.marshal(int(s)) }

// UnmarshalJSON reads null as [StatusUnknown]. An unrecognised name is an
// error; see enumSpec.unmarshal.
func (s *Status) UnmarshalJSON(b []byte) error {
	i, err := statusEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*s = Status(i)
	return nil
}

// WaitingReason is what a [StatusWaiting] session is blocked on.
//
// It is a separate axis from [Status] because it has four answers, not two.
// [WaitingReasonUnknown] (a session is waiting and we do not know what for),
// [WaitingReasonNone] (it is waiting on nothing we can name), and the two
// specific reasons are four distinguishable states, and the tempting
// simplification — a bool meaning "blocked on the user" — collapses all four
// into one. Input and permission are different interruptions with different
// urgencies, and the one that matters most is also the one that is hardest to
// observe in the wild.
type WaitingReason uint8

const (
	// WaitingReasonUnknown is the zero value: no opinion. A session can be
	// known to be waiting while what it waits for is undetermined.
	WaitingReasonUnknown WaitingReason = iota
	// WaitingReasonNone is a determined negative: nothing is being waited on.
	WaitingReasonNone
	// WaitingReasonInput is a session waiting for the user to type something.
	WaitingReasonInput
	// WaitingReasonPermission is a session holding a permission prompt open.
	WaitingReasonPermission
)

// These names are NORMALISED, not mirrored, and this is the one axis where
// this package departs from Claude's spelling.
//
// Claude writes "input needed" and "permission prompt" — with spaces, which
// make poor wire values: every jq filter and every shell pipeline downstream
// has to quote them, and half of them will not. Mirror where the word is good,
// normalise where it is not. The mapping from Claude's two strings to these
// two words lives in adapters/schema.go, with every other fact about Claude's
// format, so a rename on their side still moves exactly one file.
var waitingReasonEnum = enumSpec{"WaitingReason", []string{
	"unknown",
	"none",
	"input",
	"permission",
}}

// String returns the wire word.
func (w WaitingReason) String() string { return waitingReasonEnum.name(int(w)) }

// MarshalJSON writes null for [WaitingReasonUnknown] and the value's wire word
// otherwise. Note that [WaitingReasonNone] is NOT null: "waiting for nothing"
// is an answer and "we did not determine it" is not.
func (w WaitingReason) MarshalJSON() ([]byte, error) {
	return waitingReasonEnum.marshal(int(w))
}

// UnmarshalJSON reads null as [WaitingReasonUnknown].
func (w *WaitingReason) UnmarshalJSON(b []byte) error {
	i, err := waitingReasonEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*w = WaitingReason(i)
	return nil
}

// Liveness is a tri-state answer to "does this still exist?" — for a process,
// for a pane, for anything magmux gates a session's inclusion on.
//
// It is not a bool because the third answer is real and common: the oracle may
// not have been asked, or may not be able to answer. A session whose liveness
// was never established must not be reported dead, and must not be reported
// alive either.
type Liveness uint8

const (
	// LivenessUnknown is the zero value: nobody asked, or nobody could answer.
	LivenessUnknown Liveness = iota
	// LivenessYes is a determined yes.
	LivenessYes
	// LivenessNo is a determined no.
	LivenessNo
)

// These names are ours outright: nothing magmux observes writes them. The
// third one is the point — "nobody asked" is not "no".
var livenessEnum = enumSpec{"Liveness", []string{
	"unknown",
	"yes",
	"no",
}}

// LivenessOf converts a determined bool into a [Liveness]. There is
// deliberately no conversion the other way: collapsing LivenessUnknown into a
// bool is the error this type exists to prevent.
func LivenessOf(alive bool) Liveness {
	if alive {
		return LivenessYes
	}
	return LivenessNo
}

// String returns the wire word.
func (l Liveness) String() string { return livenessEnum.name(int(l)) }

// MarshalJSON writes null for [LivenessUnknown] and the value's wire word
// otherwise.
func (l Liveness) MarshalJSON() ([]byte, error) { return livenessEnum.marshal(int(l)) }

// UnmarshalJSON reads null as [LivenessUnknown].
func (l *Liveness) UnmarshalJSON(b []byte) error {
	i, err := livenessEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*l = Liveness(i)
	return nil
}
