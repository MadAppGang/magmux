package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

// DefaultTranscriptRetention is Claude's documented default transcript
// retention, 30 days, in force when settings.json has no cleanupPeriodDays.
const DefaultTranscriptRetention = 30 * 24 * time.Hour

// maxRetentionDays keeps the conversion to a Duration far from overflow.
const maxRetentionDays = 100_000

// TimelineLine is one jobs/<dir>/timeline.jsonl line, for claude/doctor's two
// timeline checks. Nothing at runtime reads the timeline (O10): it is an
// incomplete log, and one record's current state does not appear in its own.
type TimelineLine struct {
	At *time.Time
	// BadInstant: the at value was present and implausible.
	BadInstant bool
	// Keys lists the line's top-level keys, sorted, for jobs-timeline-shape.
	Keys []string
}

// DecodeTimelineLine decodes one timeline line. false => not a JSON object.
func DecodeTimelineLine(b []byte) (TimelineLine, bool) {
	keys, ok := TopLevelKeys(b)
	if !ok {
		return TimelineLine{}, false
	}
	l := TimelineLine{Keys: keys}
	var rec map[string]json.RawMessage
	_ = json.Unmarshal(bytes.TrimSpace(b), &rec)
	if s, ok := rawString(rec[KeyTimelineAt]); ok {
		t, err := DecodeInstantString(s)
		if errors.Is(err, ErrImplausibleInstant) {
			l.BadInstant = true
		} else if err == nil {
			l.At = t
		}
	}
	return l, true
}

// DecodeRetention reads settings.json's cleanupPeriodDays by tt's rule
// (tt:adapters/transcriptfs/retention.go:102-150). ok is false when the file
// is not an object, the key is absent, the value is QUOTED (a vendor writing
// it as a string has changed the format), or it is not a positive whole
// number; the caller then uses DefaultTranscriptRetention. A zero is refused
// rather than honoured: it would make every record "beyond retention".
func DecodeRetention(settings []byte) (time.Duration, bool) {
	o, err := decodeObject(SettingsFile, settings)
	if err != nil {
		return 0, false
	}
	days, ok := rawInt64(o.m[KeyCleanupPeriodDays])
	if !ok || days <= 0 || days > maxRetentionDays {
		return 0, false
	}
	return time.Duration(days) * 24 * time.Hour, true
}
