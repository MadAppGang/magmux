package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// FactsTailBytes is the facts window: the last 200 KB. Measured for colour, 0
// misses across 35 live transcripts (worst case 44.1 KB) against a largest
// transcript of 630.9 MB, so a whole-file scan is not slow but impossible.
// It was NOT measured for errors and does miss them (13 of 36 sessions had
// errors only outside it), which is why every count discloses its Window
// (D10).
const FactsTailBytes = 200 << 10

// TimingTailBytes is the timing window, a suffix of the same read: the newest
// timestamped record sits at most 1,912 B from EOF on the measured machine.
const TimingTailBytes = 64 << 10

// MaxRecordBytes bounds a registry or job record. A larger file is
// ProblemUnreadable and is never read whole.
const MaxRecordBytes = 1 << 20

// maxPlausibleTokens refuses a context count that is not a token count at all
// (about 100x the largest measured window), and keeps the sum far from
// overflow.
const maxPlausibleTokens = 100_000_000

// Facts is what the facts window established.
type Facts struct {
	// APIErrors counts isApiErrorMessage records, never is_error (which fires
	// on RECOVERED failures: 29 of 32 healthy sessions would read broken).
	APIErrors    int
	LastAPIError string // raw text; the host decides exposure (D13)
	// Color is the LAST type:"agent-color" record's agentColor (D2), raw;
	// nil = no colour record in the window. "" is a value, not an absence.
	Color *string
	// ContextTokens is input + cache_creation + cache_read from the newest
	// main-thread assistant usage, or a newer compaction's postTokens (D3).
	ContextTokens *int64
	Model         string // that record's message.model, raw
	Compactions   int    // compact_boundary records in the window
	// ImplausibleInstants counts timestamps outside 2001..5138 in the window,
	// and ImplausibleField names the first offending key (C4).
	ImplausibleInstants int
	ImplausibleField    string
}

// DecodeFacts decodes the facts window. tail is the last w.TailBytes of the
// file; when w.Partial its first line was cut by the boundary and is
// discarded (D10), so a fragment that happens to parse is never counted. A
// malformed line is skipped, never fatal. It opens nothing.
func DecodeFacts(tail []byte, w model.Window) Facts {
	var f Facts
	lines := windowLines(tail, w)
	var (
		assistantAt   []int
		lastCompactAt = -1
		postTokens    *int64
	)
	for i, line := range lines {
		rec := lineObject(line)
		if rec == nil {
			continue
		}
		f.notice(rec)
		if isTrue(rec[KeyIsAPIErrorMessage]) {
			f.APIErrors++
			if text := errorText(rec); text != "" {
				f.LastAPIError = text
			}
		}
		switch typ, _ := rawString(rec[KeyRecordType]); typ {
		case ValueRecordAgentColor:
			if c, ok := rawString(rec[KeyAgentColor]); ok {
				f.Color = &c
			}
		case ValueRecordAssistant:
			if !isTrue(rec[KeyIsSidechain]) {
				assistantAt = append(assistantAt, i)
			}
		case ValueRecordSystem:
			if st, _ := rawString(rec[KeySubtype]); st == ValueSubtypeCompactBoundary {
				f.Compactions++
				lastCompactAt = i
				postTokens = compactPostTokens(rec[KeyCompactMetadata])
			}
		}
	}
	usedAt := f.context(lines, assistantAt)
	if lastCompactAt > usedAt {
		// The newest compaction replaced the context the usage describes;
		// without a postTokens the honest answer is "not measurable".
		f.ContextTokens = postTokens
	}
	return f
}

// notice counts an implausible timestamp on one record.
func (f *Facts) notice(rec map[string]json.RawMessage) {
	if badTimestamp(rec) {
		f.ImplausibleInstants++
		if f.ImplausibleField == "" {
			f.ImplausibleField = KeyTimestamp
		}
	}
}

func badTimestamp(rec map[string]json.RawMessage) bool {
	ts, ok := rawString(rec[KeyTimestamp])
	if !ok || ts == "" {
		return false
	}
	_, err := DecodeInstantString(ts)
	return errors.Is(err, ErrImplausibleInstant)
}

// context walks the main-thread assistant records NEWEST FIRST and takes the
// first that qualifies: a model that is not the synthetic placeholder, and a
// usage carrying all three input counts as non-negative integers summing to a
// plausible, non-zero total. It returns that line's index, or -1.
func (f *Facts) context(lines [][]byte, assistantAt []int) int {
	for k := len(assistantAt) - 1; k >= 0; k-- {
		idx := assistantAt[k]
		rec := lineObject(lines[idx])
		var msg map[string]json.RawMessage
		if json.Unmarshal(rec[KeyMessage], &msg) != nil {
			continue
		}
		model, isString := rawString(msg[KeyModel])
		if isString && model == ValueModelSynthetic {
			continue
		}
		used, ok := contextUsed(msg[KeyUsage])
		if !ok {
			continue
		}
		f.ContextTokens = &used
		if isString {
			f.Model = model
		}
		return idx
	}
	return -1
}

func contextUsed(raw json.RawMessage) (int64, bool) {
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil || usage == nil {
		return 0, false
	}
	var used int64
	for _, key := range []string{KeyInputTokens, KeyCacheCreationTokens, KeyCacheReadTokens} {
		n, ok := tokenCount(usage[key])
		if !ok {
			return 0, false
		}
		used += n
	}
	if used <= 0 || used > maxPlausibleTokens {
		return 0, false
	}
	return used, true
}

func compactPostTokens(raw json.RawMessage) *int64 {
	var meta map[string]json.RawMessage
	if json.Unmarshal(raw, &meta) != nil || meta == nil {
		return nil
	}
	n, ok := tokenCount(meta[KeyPostTokens])
	if !ok || n == 0 {
		return nil
	}
	return &n
}

func tokenCount(raw json.RawMessage) (int64, bool) {
	n, ok := rawInt64(raw)
	if !ok || n < 0 || n > maxPlausibleTokens {
		return 0, false
	}
	return n, true
}

// errorText is the human text of an API-error record: its message's text
// blocks, trimmed and joined. Called only for records already flagged.
func errorText(rec map[string]json.RawMessage) string {
	var msg map[string]json.RawMessage
	if json.Unmarshal(rec[KeyMessage], &msg) != nil {
		return ""
	}
	var parts []string
	for _, b := range decodeBlocks(msg[KeyContent]) {
		if b.Kind == BlockText && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, " ")
}

// Timing is what the timing window established.
type Timing struct {
	// LastActivityAt is the newest record carrying a plausible timestamp.
	// Eight record types carry none, and on 4 of 5 measured job transcripts
	// the LAST LINE is one of them, so the scan walks backwards past them.
	LastActivityAt *time.Time
	// LatestText is the newest main-thread assistant record's last non-blank
	// text block, raw (FR7), and LatestTextAt that record's instant.
	LatestText          string
	LatestTextAt        *time.Time
	ImplausibleInstants int
	ImplausibleField    string
}

// DecodeTiming decodes the timing window, by DecodeFacts's rules for the
// window. It opens nothing.
func DecodeTiming(tail []byte, w model.Window) Timing {
	var t Timing
	lines := windowLines(tail, w)
	for i := len(lines) - 1; i >= 0; i-- {
		rec := lineObject(lines[i])
		if rec == nil {
			continue
		}
		if badTimestamp(rec) {
			t.ImplausibleInstants++
			t.ImplausibleField = KeyTimestamp
		}
		ts, _ := rawString(rec[KeyTimestamp])
		at, err := DecodeInstantString(ts)
		if err != nil {
			at = nil
		}
		if t.LastActivityAt == nil && at != nil {
			t.LastActivityAt = at
		}
		if t.LatestTextAt == nil && t.LatestText == "" {
			if typ, _ := rawString(rec[KeyRecordType]); typ == ValueRecordAssistant && !isTrue(rec[KeyIsSidechain]) {
				var msg map[string]json.RawMessage
				if json.Unmarshal(rec[KeyMessage], &msg) == nil {
					blocks := decodeBlocks(msg[KeyContent])
					for j := len(blocks) - 1; j >= 0; j-- {
						if blocks[j].Kind == BlockText && strings.TrimSpace(blocks[j].Text) != "" {
							t.LatestText, t.LatestTextAt = blocks[j].Text, at
							break
						}
					}
				}
			}
		}
	}
	return t
}

// windowLines splits a window into lines, dropping the first when the window
// was cut mid-file.
func windowLines(tail []byte, w model.Window) [][]byte {
	lines := bytes.Split(tail, []byte{'\n'})
	if w.Partial && len(lines) > 0 {
		lines = lines[1:]
	}
	return lines
}

func lineObject(line []byte) map[string]json.RawMessage {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	var rec map[string]json.RawMessage
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	return rec
}
