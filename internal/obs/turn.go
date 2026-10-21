package obs

import "time"

// Turn is one entry in a tool's own record of a session.
//
// The granularity is one MESSAGE, not one exchange: a user prompt is a Turn,
// and the assistant's whole reply to it — however many entries the tool split
// that across in its record — is the next Turn. That is what makes Role
// meaningful, and what makes "the last four turns" read as a conversation
// rather than as a fragment of one.
//
// # Why this lives here and not in package main
//
// It was declared in package main, beside the controller that first produced
// it, and it is part of the public TranscriptReader contract there. Go forbids
// importing main, so an observation layer that wants turns cannot have them
// while they live there — and a second, parallel Turn type in this package
// would be two vocabularies for one thing, diverging the first time either
// side gained a field.
//
// The move is by TYPE ALIAS: package main keeps `type Turn = obs.Turn` and
// `type ToolCall = obs.ToolCall`, which is the SAME type rather than a
// conversion, so the TranscriptReader contract is unchanged, the unexported
// field below travels with the type, and no existing caller notices.
type Turn struct {
	// Role is who spoke: TurnUser or TurnAssistant.
	Role string

	// Text is the complete text of the turn, never truncated. A turn that was
	// nothing but tool calls has none, which is normal and is not a failure.
	Text string

	// Tools are the tool calls made during this turn, in the order the tool
	// recorded them, each with the result it produced.
	Tools []ToolCall

	// Timestamp is when the tool recorded the turn's first entry. Zero when
	// the record carried none.
	Timestamp time.Time
}

// ToolCall is one tool invocation and its outcome, as the tool itself recorded
// them. Input and Result are the whole point: a status surface has only ever
// carried a tool's NAME, which tells a reader that something ran and nothing
// whatever about what it did or what it found.
type ToolCall struct {
	Name   string // e.g. "Bash", "Edit"
	Input  string // the arguments, as JSON, exactly as the tool recorded them
	Result string // what the tool returned; empty if it had not returned yet

	// id pairs a call with its result inside the tool's record. Unexported
	// because it is a PARSER's bookkeeping and means nothing to a caller:
	// nobody outside this package can read it, and the two functions below are
	// the whole of what a parser needs it for.
	id string
}

// Turn roles. Deliberately the same two words every tool in this space uses,
// so a consumer needs no translation table — and, for the same reason, the
// same two words Claude Code writes in its transcript. They are HOMOGRAPHS,
// not copies: these are ours, they marshal into our own documents, and if
// Claude renames its record types the adapter absorbs it and these do not
// move. See adapters.RecordTypeUser, which is the other side of the pair.
const (
	TurnUser      = "user"
	TurnAssistant = "assistant"
)

// NewToolCall builds a call with its record's own correlation id.
//
// The id is write-only from outside this package: it goes in here and comes
// back out only as [AttachToolResult]'s matching, which is the only thing it
// is for. A caller that wants to know which tool ran reads Name.
func NewToolCall(name, input, id string) ToolCall {
	return ToolCall{Name: name, Input: input, id: id}
}

// AttachToolResult files a tool result against the call it answers, in place,
// and returns the index of the turn it landed in, or -1.
//
// Matching is by the record's own correlation id, searching backwards because
// a result is always answering a recent call. The id-less fallback exists
// because the id is the observed tool's field and not ours: without it a
// result would be dropped silently, and "the tool ran and returned nothing" is
// exactly the wrong thing to tell a driver.
//
// It is here, beside the type, rather than in whichever parser needed it
// first: it is an operation on this data structure and it names no tool's
// strings at all, so a second tool's parser gets the same behaviour for free
// instead of a second copy of the backwards walk.
func AttachToolResult(turns []Turn, id, text string) int {
	for i := len(turns) - 1; i >= 0; i-- {
		for j := len(turns[i].Tools) - 1; j >= 0; j-- {
			call := &turns[i].Tools[j]
			if id != "" {
				if call.id != id {
					continue
				}
			} else if call.Result != "" {
				continue // answered already; the open call is the one we want
			}
			call.Result = text
			return i
		}
	}
	return -1
}
