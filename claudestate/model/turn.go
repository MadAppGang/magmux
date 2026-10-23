package model

import "time"

// Turn roles. The same two words every tool in this space uses. They are
// HOMOGRAPHS of Claude Code's record types, not copies: these are the
// library's own, and if Claude renames its record types the decoder absorbs
// it and these do not move.
const (
	TurnUser      = "user"
	TurnAssistant = "assistant"
)

// Turn is one message in a tool's own record of a session: a user prompt, or
// the assistant's whole reply to it however many entries the tool split that
// across.
type Turn struct {
	Role      string // TurnUser or TurnAssistant
	Text      string // complete, never truncated
	Tools     []ToolCall
	Timestamp time.Time // zero when the record carried none
}

// ToolCall is one tool invocation and its outcome.
type ToolCall struct {
	Name   string
	Input  string // the arguments, as JSON, exactly as recorded
	Result string // "" if it had not returned yet

	// id pairs a call with its result inside the tool's record. Unexported:
	// it is a parser's bookkeeping, and the two functions below are the whole
	// of what a parser needs it for.
	id string
}

// NewToolCall builds a call with its record's own correlation id.
func NewToolCall(name, input, id string) ToolCall {
	return ToolCall{Name: name, Input: input, id: id}
}

// AttachToolResult files a tool result against the call it answers, in place,
// and returns the index of the turn it landed in, or -1.
//
// Matching is by correlation id, searching backwards because a result answers
// a recent call. With no id, the most recent unanswered call takes it, so a
// result is never dropped silently.
func AttachToolResult(turns []Turn, id, text string) int {
	for i := len(turns) - 1; i >= 0; i-- {
		for j := len(turns[i].Tools) - 1; j >= 0; j-- {
			call := &turns[i].Tools[j]
			if id != "" {
				if call.id != id {
					continue
				}
			} else if call.Result != "" {
				continue
			}
			call.Result = text
			return i
		}
	}
	return -1
}
