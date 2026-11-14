package claude_test

import (
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

func bbContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestBBVocabularyExposesDecoderOwnedKeysAndValues(t *testing.T) {
	v := claude.Vocabulary()
	// These constant identifiers are explicitly exemplified in section 4.5; none
	// was inferred from the spelling of a private decoder symbol.
	for _, key := range []string{claude.KeySessionID, claude.KeyStatus, claude.KeyWaitingFor} {
		if !bbContains(v.Registry.Universal, key) && !bbContains(v.Registry.Optional, key) {
			t.Errorf("registry vocabulary omits decoder key %q", key)
		}
	}
	if !bbContains(v.Jobs.Universal, claude.KeyLinkScanPath) {
		t.Errorf("jobs union omits linkScanPath: %v", v.Jobs.Universal)
	}
	if len(v.Jobs.Universal) != 19 {
		t.Errorf("jobs universal union has %d keys; want measured 19-key union", len(v.Jobs.Universal))
	}
	// Contract §4.10 C: Universal is the measured ALWAYS-PRESENT set; a key measured
	// as optional is not in it even when it is also deliberately unread. So every
	// JobsNeverRead key is in exactly one of Universal and Optional, and
	// suggestedReply in particular is Optional and not Universal.
	for _, key := range v.JobsNeverRead {
		inU, inO := bbContains(v.Jobs.Universal, key), bbContains(v.Jobs.Optional, key)
		if inU == inO {
			t.Errorf("deliberately unread job key %q: in Universal=%v, in Optional=%v; want exactly one", key, inU, inO)
		}
	}
	if !bbContains(v.JobsNeverRead, "suggestedReply") || !bbContains(v.Jobs.Optional, "suggestedReply") || bbContains(v.Jobs.Universal, "suggestedReply") {
		t.Errorf("suggestedReply must be never-read and Optional, not Universal: neverRead=%v optional=%v universal=%v", v.JobsNeverRead, v.Jobs.Optional, v.Jobs.Universal)
	}
	if !bbContains(v.TranscriptKeys, claude.KeyAgentColor) {
		t.Errorf("transcript vocabulary omits agentColor: %v", v.TranscriptKeys)
	}
	for _, set := range []struct {
		name   string
		values []string
		want   []string
	}{
		{"registry status", v.RegistryStatus, []string{"idle", "busy", "shell", "waiting"}},
		{"waiting", v.WaitingFor, []string{"input needed", "permission prompt", "dialog open"}},
		{"job state", v.JobState, []string{"working", "blocked", "done", "failed"}},
		{"tempo", v.Tempo, []string{"blocked"}},
		{"timeline keys", append(append([]string(nil), v.Timeline.Universal...), v.Timeline.Optional...), []string{"at", "detail", "state", "text"}},
	} {
		for _, word := range set.want {
			if !bbContains(set.values, word) {
				t.Errorf("%s vocabulary omits %q: %v", set.name, word, set.values)
			}
		}
		seen := make(map[string]bool)
		for _, word := range set.values {
			if seen[word] {
				t.Errorf("%s repeats %q", set.name, word)
			}
			seen[word] = true
		}
	}
	// §4.10 A, the other side: the bare enum names are not Claude's words, so they
	// are not known waitingFor values.
	for _, word := range []string{"input", "permission"} {
		if bbContains(v.WaitingFor, word) {
			t.Errorf("waiting vocabulary lists bare %q, which §4.10 A says is Unknown: %v", word, v.WaitingFor)
		}
	}
}

func TestBBTimelineLineKeysAndInstantDiagnostics(t *testing.T) {
	// Contract §4.10 B: timeline instants are RFC3339 strings.
	got, ok := claude.DecodeTimelineLine([]byte(`{"at":"2023-11-14T22:13:20Z","detail":"d","state":"working","text":"t"}`))
	if !ok || got.BadInstant || len(got.Keys) != 4 {
		t.Fatalf("timeline line = %+v, %v", got, ok)
	}
	bbInstant(t, got.At, time.UnixMilli(1700000000000))
	for _, key := range []string{"at", "detail", "state", "text"} {
		if !bbContains(got.Keys, key) {
			t.Errorf("timeline keys omit %q: %v", key, got.Keys)
		}
	}
	for _, at := range []string{"1970-01-01T00:00:01Z", "5138-11-16T09:46:40.001Z"} {
		got, ok = claude.DecodeTimelineLine([]byte(`{"at":"` + at + `"}`))
		if !ok || got.At != nil || !got.BadInstant {
			t.Errorf("implausible timeline instant %s = %+v, %v", at, got, ok)
		}
	}
	// The other JSON type is never coerced into an instant (§4.10 B).
	got, ok = claude.DecodeTimelineLine([]byte(`{"at":1700000000000}`))
	if !ok || got.At != nil {
		t.Errorf("numeric timeline at was coerced: %+v, %v", got, ok)
	}
	for _, raw := range []string{"", "{", "[]", "null", `"text"`} {
		if _, ok := claude.DecodeTimelineLine([]byte(raw)); ok {
			t.Errorf("nonobject timeline line %q accepted", raw)
		}
	}
}

func TestBBRetentionOnlyAcceptsPositiveUnquotedWholeDays(t *testing.T) {
	// Contract §4.10 Retention: a positive, unquoted, WHOLE number of days; 1.5 and
	// 2.0 are refused like zero, negatives, quoted numbers and non-numbers.
	for _, tt := range []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{`{"cleanupPeriodDays":7}`, 7 * 24 * time.Hour, true},
		{`{"cleanupPeriodDays":1}`, 24 * time.Hour, true},
		{`{"cleanupPeriodDays":2}`, 48 * time.Hour, true},
		{`{"cleanupPeriodDays":1.5}`, 0, false},
		{`{"cleanupPeriodDays":2.0}`, 0, false},
		{`{}`, 0, false}, {`{"cleanupPeriodDays":"7"}`, 0, false},
		{`{"cleanupPeriodDays":0}`, 0, false}, {`{"cleanupPeriodDays":-7}`, 0, false},
		{`{"cleanupPeriodDays":null}`, 0, false}, {`{"cleanupPeriodDays":true}`, 0, false},
		{"{", 0, false},
	} {
		got, ok := claude.DecodeRetention([]byte(tt.raw))
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("DecodeRetention(%s) = (%v, %v); want (%v, %v)", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

func TestBBPublicTailAndRecordConstants(t *testing.T) {
	if claude.FactsTailBytes != 200<<10 || claude.TimingTailBytes != 64<<10 || claude.MaxRecordBytes != 1<<20 || claude.RecordSuffix != ".json" {
		t.Errorf("public bounds changed: facts=%d timing=%d record=%d suffix=%q", claude.FactsTailBytes, claude.TimingTailBytes, claude.MaxRecordBytes, claude.RecordSuffix)
	}
}
