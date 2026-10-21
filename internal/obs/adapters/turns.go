package adapters

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// ParseTurns reads a Claude Code JSONL transcript and returns its last
// maxTurns turns, oldest first, holding no more than byteBudget bytes of them.
//
// # Where this came from, and why it moved
//
// It was `parseClaudeTurns` in package main, beside the controller that tails
// the same file. It moved here with [obs.Turn] and [obs.ToolCall], because Go
// forbids importing main and the observation layer needs turns; package main
// keeps a one-line wrapper so that every existing caller compiles untouched.
//
// It is in adapters rather than in obs because it DECODES Claude Code's
// format, which obs may not do, and every string it reads is named in
// schema.go — including the envelope keys, which are conventional rather than
// measured. See the note above them: a shape that is not there yields no turn,
// never a guessed one.
//
// Split out from any file-opening caller so it can be tested against a reader
// with no file, no controller and no discovery — this is the part most likely
// to rot, since the JSONL shape belongs to Claude Code and can change without
// notice.
func ParseTurns(r io.Reader, maxTurns, byteBudget int) ([]obs.Turn, error) {
	sc := bufio.NewScanner(r)
	// Same 4MB budget as magmux's tailer: a single tool result carrying a whole
	// file comfortably exceeds bufio's 64KB default, and an oversized token does
	// not get skipped — Scan returns false and the rest of the transcript
	// silently vanishes.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		turns []obs.Turn
		sizes []int // bytes retained per turn, parallel to turns
		total int
		// open reports whether the last retained turn is an assistant turn
		// still being appended to. Claude Code writes one assistant reply as
		// several entries; they are one turn.
		open bool
	)
	// trim drops the OLDEST turns until the request's bounds are met, always
	// keeping at least one: a caller who asked for history and whose newest
	// turn alone blows the budget still wants that turn.
	trim := func() {
		for len(turns) > 1 && (len(turns) > maxTurns || total > byteBudget) {
			total -= sizes[0]
			turns, sizes = turns[1:], sizes[1:]
			if len(turns) == 0 {
				open = false
			}
		}
	}
	push := func(t obs.Turn, size int) {
		turns = append(turns, t)
		sizes = append(sizes, size)
		total += size
		trim()
	}
	grow := func(i, n int) {
		if i < 0 || i >= len(sizes) || n == 0 {
			return
		}
		sizes[i] += n
		total += n
		trim()
	}

	for sc.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			continue // a partially flushed line; the next poll sees it whole
		}
		msg, _ := entry[KeyMessage].(map[string]any)

		switch t, _ := entry[KeyRecordType].(string); t {
		case RecordTypeUser:
			if text, isPrompt := PromptText(msg); isPrompt {
				open = false
				push(obs.Turn{
					Role:      obs.TurnUser,
					Text:      text,
					Timestamp: DecodeRecordTime(entry[KeyTimestamp]),
				}, len(text))
				continue
			}
			// Not a prompt: a tool RESULT, which Claude Code also files as a
			// user entry. It belongs to the call it answers, not to a turn of
			// its own.
			for _, block := range ContentBlocks(msg) {
				if bt, _ := block[KeyRecordType].(string); bt != BlockTypeToolResult {
					continue
				}
				id, _ := block[KeyToolUseID].(string)
				text := ToolResultText(block[KeyContent])
				if i := obs.AttachToolResult(turns, id, text); i >= 0 {
					grow(i, len(text))
				}
			}

		case RecordTypeAssistant:
			if !open {
				push(obs.Turn{
					Role:      obs.TurnAssistant,
					Timestamp: DecodeRecordTime(entry[KeyTimestamp]),
				}, 0)
				open = true
			}
			cur := &turns[len(turns)-1]
			added := 0
			for _, block := range ContentBlocks(msg) {
				switch bt, _ := block[KeyRecordType].(string); bt {
				case BlockTypeText:
					txt, _ := block[KeyBlockText].(string)
					txt = strings.TrimSpace(txt)
					if txt == "" {
						continue
					}
					if cur.Text != "" {
						cur.Text += "\n\n"
						added += 2
					}
					cur.Text += txt
					added += len(txt)
				case BlockTypeToolUse:
					name, _ := block[KeyToolName].(string)
					id, _ := block[KeyBlockID].(string)
					input := ToolInput(block[KeyToolInput])
					cur.Tools = append(cur.Tools, obs.NewToolCall(name, input, id))
					added += len(name) + len(input)
				}
			}
			grow(len(turns)-1, added)
		}
	}
	if err := sc.Err(); err != nil {
		// Whatever was parsed before the failure is real and is returned with
		// the error: a caller that can show six of eight turns and say why the
		// rest are missing is strictly better off than one handed nothing.
		return turns, err
	}
	return turns, nil
}

// PromptText reports whether a user entry is a REAL prompt, and returns it if
// so.
//
// The rule — string content is a prompt, array content is a tool result — is
// an undocumented shape of Claude Code's record, and both readers of that
// record depend on it for different reasons: a tailer must not treat a tool
// result as a turn boundary, and a history parser must not file one as a turn.
// One function so they cannot drift apart, which is why it is exported rather
// than kept private to [ParseTurns]: magmux's own tailer lives in package main
// and calls this one.
func PromptText(msg map[string]any) (string, bool) {
	content, ok := msg[KeyContent].(string)
	return content, ok
}

// ContentBlocks returns the content blocks of a message, or nil when the
// content is a bare string (a prompt) or missing.
func ContentBlocks(msg map[string]any) []map[string]any {
	raw, _ := msg[KeyContent].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		if block, ok := b.(map[string]any); ok {
			out = append(out, block)
		}
	}
	return out
}

// ToolInput renders a tool-use input as the JSON it was recorded as.
//
// Kept as text rather than as a nested object: it crosses a socket and an MCP
// boundary on its way to a model, and a model reads `{"command":"ls -la"}`
// exactly as well as a decoded map while costing every hop less.
func ToolInput(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ToolResultText flattens a tool result's content, which Claude Code writes as
// a plain string for simple tools and as a block array for the ones that
// return structured output.
func ToolResultText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := block[KeyBlockText].(string); ok && txt != "" {
				parts = append(parts, txt)
				continue
			}
			// A non-text block (an image, say). Naming it beats dropping it:
			// "the tool returned something we did not render" is information.
			if bt, ok := block[KeyRecordType].(string); ok && bt != "" {
				parts = append(parts, "["+bt+"]")
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ToolInput(v)
	}
}

// DecodeRecordTime parses a transcript record's ISO-8601 instant. It returns
// the zero time when the value is missing or malformed.
//
// It is NOT [DecodeInstant]. That one refuses a value that is not plausibly
// epoch milliseconds; this one reads a string, and the two units live in the
// same file precisely so that nobody has to remember which key is which — the
// registry's three timestamps are numbers, a transcript record's is a string.
func DecodeRecordTime(v any) time.Time {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
