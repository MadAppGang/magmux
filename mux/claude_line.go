package mux

import (
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// ctrlLine is one transcript record reduced to what the pane controller's
// state machine reads (applyLine, noteQueued, readFirstUserPrompt).
//
// The DECODING is claude.DecodeLine's, the one reader of Claude Code's
// transcript format, in the claudestate library; this is only the controller's
// view of it — which text block and which tool call of an assistant record it
// keeps — so that function names none of Claude's keys or values and the
// controller's state machine reads plain fields. It replaced the controller's
// own decoder in internal/obs/adapters when that package was deleted.
type ctrlLine struct {
	Kind claude.LineKind
	// CWD is the record's top-level cwd, on any record (the snapshot's
	// Project); Model is an assistant record's message.model.
	CWD, Model string
	// Timestamp is the record's own instant; zero when absent or implausible.
	Timestamp time.Time

	// Prompt is a user record's STRING content; IsPrompt says it was one. A
	// user record with array content is a tool RESULT, not a prompt.
	Prompt   string
	IsPrompt bool

	// Text is an assistant record's last non-blank text block, trimmed; Tool
	// its last tool call's name. Has* says there was one.
	Text    string
	HasText bool
	Tool    string
	HasTool bool

	// Queue is a queue-operation record's operation.
	Queue *claude.QueueOp

	// QueuedCommand is a queued_command attachment, with its prompt and the
	// attachment's OWN instant (the original enqueue time), zero when it
	// carries none.
	QueuedCommand bool
	QueuedPrompt  string
	QueuedAt      time.Time
}

// decodeCtrlLine decodes one transcript line for the controller. ok is false
// when the line is not a JSON object — a partially flushed line, which the
// next read sees whole.
func decodeCtrlLine(b []byte) (ctrlLine, bool) {
	l, ok := claude.DecodeLine(b)
	if !ok {
		return ctrlLine{}, false
	}
	c := ctrlLine{Kind: l.Kind, CWD: l.CWD, Model: l.Model, Queue: l.Queue}
	if l.Time != nil {
		c.Timestamp = *l.Time
	}
	c.Prompt, c.IsPrompt = claude.PromptText(l)
	if l.Kind == claude.LineAssistant {
		for _, b := range l.Blocks {
			switch b.Kind {
			case claude.BlockText:
				if t := strings.TrimSpace(b.Text); t != "" {
					c.Text, c.HasText = t, true
				}
			case claude.BlockToolUse:
				c.Tool, c.HasTool = b.ToolName, true
			}
		}
	}
	if a := l.Attachment; a != nil && a.Kind == claude.AttachmentQueuedCommand {
		c.QueuedCommand, c.QueuedPrompt = true, a.Prompt
		if a.Time != nil {
			c.QueuedAt = *a.Time
		}
	}
	return c, true
}
