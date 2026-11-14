package claude

import (
	"errors"
	"time"
)

// ErrImplausibleInstant is an instant outside 1e12..1e14 epoch milliseconds
// (D4). It is refused, never rescaled.
var ErrImplausibleInstant = errors.New("claude: implausible instant (not epoch milliseconds in 2001..5138)")

// The bounds, expressed as the two mistakes they catch. 1e12 ms is 2001-09-09,
// the instant epoch milliseconds reached thirteen digits: everything below is
// what a SECONDS value looks like, and seconds is the unit this schema was
// documented with until it was re-measured. 1e14 is the year 5138, above which
// a value is microseconds or nanoseconds. Nothing between is judged: a 2019
// date would be a fact about the session, not a unit error.
const (
	minPlausibleMillis = int64(1_000_000_000_000)
	maxPlausibleMillis = int64(100_000_000_000_000)
)

// DecodeInstantMillis decodes one of Claude's epoch-millisecond instants.
//
// ms <= 0 is "no instant" (nil, nil): a zero epoch would report a session in
// its status since 1970, which is a wrong answer where nil is an honest one.
// Outside [1e12, 1e14] it is (nil, ErrImplausibleInstant), and the calling
// decoder reports it in its own shape (C4). A seconds value read as
// milliseconds is 1970; a milliseconds value read as seconds is 58600; both
// would marshal cleanly and be believed, which is why this refuses rather than
// converts.
//
// The result is UTC at millisecond precision, so two machines building the
// same document produce the same bytes.
func DecodeInstantMillis(ms int64) (*time.Time, error) {
	if ms <= 0 {
		return nil, nil
	}
	if ms < minPlausibleMillis || ms > maxPlausibleMillis {
		return nil, ErrImplausibleInstant
	}
	t := time.UnixMilli(ms).UTC()
	return &t, nil
}

// DecodeInstantString decodes an RFC3339 instant (a job record's createdAt, a
// transcript record's timestamp). "" is (nil, nil). A string that does not
// parse is (nil, err). A parsed instant at or before the epoch is (nil, nil),
// for DecodeInstantMillis's reason, and one outside the plausible range is
// (nil, ErrImplausibleInstant), so both of Claude's spellings answer to one
// rule.
func DecodeInstantString(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	ms := t.UnixMilli()
	if ms <= 0 {
		return nil, nil
	}
	if ms < minPlausibleMillis || ms > maxPlausibleMillis {
		return nil, ErrImplausibleInstant
	}
	u := t.UTC().Truncate(time.Millisecond)
	return &u, nil
}
