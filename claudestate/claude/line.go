package claude

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// LineKind is which transcript record a line is.
type LineKind uint8

const (
	LineUnknown LineKind = iota // not a record type at all (no type key)
	LineUser
	LineAssistant
	LineSystem
	LineAttachment
	LineStopHookSummary // a system record: the turn's Stop hook ran, so the turn is over
	LineAgentColor
	LineCompaction // a system record: compact_boundary
	LineQueueOperation
	LineOther // a type this package does not act on; KindRaw keeps it
)

// BlockKind is a content block's type.
type BlockKind uint8

const (
	BlockUnknown BlockKind = iota
	BlockText
	BlockToolUse
	BlockToolResult
	BlockOther
)

// Block is one content block; a record may carry many.
type Block struct {
	Kind BlockKind
	// Text is a text block's text, or a tool result's flattened content.
	Text string
	// ToolName and ToolInput (the arguments as JSON, exactly as recorded) are
	// a tool_use's; ToolID is its id, or the tool_use_id a result answers.
	ToolName, ToolInput, ToolID string
}

// QueueOpKind is a queue-operation record's operation.
type QueueOpKind uint8

const (
	QueueUnknown QueueOpKind = iota
	QueueEnqueue
	QueueRemove
)

// QueueOp is a queue-operation record.
type QueueOp struct {
	Op      QueueOpKind
	OpRaw   string
	Content string
	Reason  string
	// AbsorbedMidTurn: a remove whose reason is ValueReasonAbsorbedMidTurn.
	// The prompt reached the model inside the running turn and will never
	// become a user record.
	AbsorbedMidTurn bool
}

// AttachmentKind is an attachment record's own type.
type AttachmentKind uint8

const (
	AttachmentUnknown AttachmentKind = iota
	AttachmentQueuedCommand
)

// Attachment is an attachment record. A queued command carries its prompt and
// its ORIGINAL enqueue instant.
type Attachment struct {
	Kind    AttachmentKind
	KindRaw string
	Prompt  string
	Time    *time.Time
}

// Line is one transcript record, reduced to the facts something reads.
type Line struct {
	Kind    LineKind
	KindRaw string
	// Time is nil when absent OR implausible; BadInstant names the key of an
	// implausible one (C4).
	Time       *time.Time
	BadInstant string
	Sidechain  bool
	APIError   bool // isApiErrorMessage: true, never is_error
	Blocks     []Block
	Queue      *QueueOp
	Attachment *Attachment
	AgentColor string // LineAgentColor only
	// CWD is the line's top-level cwd, on any line; Model is message.model on
	// an assistant line.
	CWD   string
	Model string

	// prompt is a user record's STRING content. A user record with ARRAY
	// content is a tool RESULT, not a prompt, and treating it as a turn
	// boundary resets a turn mid-flight (PromptText).
	prompt    string
	hasPrompt bool
}

// DecodeLine decodes one transcript line. ok is false when the line is not a
// JSON object: a partially flushed line, which the next read sees whole.
func DecodeLine(b []byte) (Line, bool) {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(b, &rec); err != nil || rec == nil {
		return Line{}, false
	}
	var l Line
	l.KindRaw, _ = rawString(rec[KeyRecordType])
	if ts, ok := rawString(rec[KeyTimestamp]); ok && ts != "" {
		t, err := DecodeInstantString(ts)
		if errors.Is(err, ErrImplausibleInstant) {
			l.BadInstant = KeyTimestamp
		} else if err == nil {
			l.Time = t
		}
	}
	l.Sidechain = isTrue(rec[KeyIsSidechain])
	l.APIError = isTrue(rec[KeyIsAPIErrorMessage])
	l.CWD, _ = rawString(rec[KeyCWD])

	var msg map[string]json.RawMessage
	_ = json.Unmarshal(rec[KeyMessage], &msg)

	switch l.KindRaw {
	case "":
		l.Kind = LineUnknown
	case ValueRecordUser:
		l.Kind = LineUser
		if s, ok := rawString(msg[KeyContent]); ok {
			l.prompt, l.hasPrompt = s, true
		} else {
			l.Blocks = decodeBlocks(msg[KeyContent])
		}
	case ValueRecordAssistant:
		l.Kind = LineAssistant
		l.Model, _ = rawString(msg[KeyModel])
		l.Blocks = decodeBlocks(msg[KeyContent])
	case ValueRecordSystem:
		switch st, _ := rawString(rec[KeySubtype]); st {
		case ValueSubtypeStopHookSummary:
			l.Kind = LineStopHookSummary
		case ValueSubtypeCompactBoundary:
			l.Kind = LineCompaction
		default:
			l.Kind = LineSystem
		}
	case ValueRecordAgentColor:
		l.Kind = LineAgentColor
		l.AgentColor, _ = rawString(rec[KeyAgentColor])
	case ValueRecordQueueOperation:
		l.Kind = LineQueueOperation
		q := &QueueOp{}
		q.OpRaw, _ = rawString(rec[KeyOperation])
		switch q.OpRaw {
		case ValueOperationEnqueue:
			q.Op = QueueEnqueue
		case ValueOperationRemove:
			q.Op = QueueRemove
		}
		q.Content, _ = rawString(rec[KeyContent])
		q.Reason, _ = rawString(rec[KeyReason])
		q.AbsorbedMidTurn = q.Op == QueueRemove && q.Reason == ValueReasonAbsorbedMidTurn
		l.Queue = q
	case ValueRecordAttachment:
		l.Kind = LineAttachment
		var a map[string]json.RawMessage
		_ = json.Unmarshal(rec[KeyAttachment], &a)
		att := &Attachment{}
		att.KindRaw, _ = rawString(a[KeyRecordType])
		if att.KindRaw == ValueAttachmentQueued {
			att.Kind = AttachmentQueuedCommand
			att.Prompt, _ = rawString(a[KeyAttachmentPrompt])
			if ts, ok := rawString(a[KeyTimestamp]); ok {
				att.Time, _ = DecodeInstantString(ts)
			}
		}
		l.Attachment = att
	default:
		l.Kind = LineOther
	}
	return l, true
}

// PromptText reports whether a line is a REAL prompt (a user record whose
// content is a string) and returns it. A tool result is filed as a user record
// too, with array content, and is not a prompt.
func PromptText(l Line) (string, bool) {
	return l.prompt, l.hasPrompt
}

func decodeBlocks(raw json.RawMessage) []Block {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]Block, 0, len(items))
	for _, it := range items {
		var b map[string]json.RawMessage
		if json.Unmarshal(it, &b) != nil || b == nil {
			continue
		}
		bt, _ := rawString(b[KeyRecordType])
		switch bt {
		case ValueBlockText:
			txt, _ := rawString(b[KeyBlockText])
			out = append(out, Block{Kind: BlockText, Text: txt})
		case ValueBlockToolUse:
			name, _ := rawString(b[KeyToolName])
			id, _ := rawString(b[KeyBlockID])
			out = append(out, Block{Kind: BlockToolUse, ToolName: name, ToolID: id, ToolInput: toolInput(b[KeyToolInput])})
		case ValueBlockToolResult:
			id, _ := rawString(b[KeyToolUseID])
			out = append(out, Block{Kind: BlockToolResult, ToolID: id, Text: toolResultText(b[KeyContent])})
		default:
			out = append(out, Block{Kind: BlockOther, Text: bt})
		}
	}
	return out
}

// toolInput renders a tool_use input as compact JSON, as it was recorded.
func toolInput(raw json.RawMessage) string {
	if isNull(raw) {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// toolResultText flattens a tool result's content: a plain string, or a block
// array whose text blocks are joined by newlines and whose other blocks are
// named in brackets (naming beats dropping).
func toolResultText(raw json.RawMessage) string {
	if s, ok := rawString(raw); ok {
		return s
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return toolInput(raw)
	}
	var parts []string
	for _, it := range items {
		var b map[string]json.RawMessage
		if json.Unmarshal(it, &b) != nil {
			continue
		}
		if txt, ok := rawString(b[KeyBlockText]); ok && txt != "" {
			parts = append(parts, txt)
			continue
		}
		if bt, ok := rawString(b[KeyRecordType]); ok && bt != "" {
			parts = append(parts, "["+bt+"]")
		}
	}
	return strings.Join(parts, "\n")
}
