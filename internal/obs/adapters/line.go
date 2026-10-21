package adapters

import (
	"encoding/json"
	"strings"
	"time"
)

// The pane controller's two readers of Claude Code's files, moved here so that
// magmux has one decoder of that format and not two.
//
// mux's ClaudeCodeController tails a pane's transcript incrementally and reads
// a few fields of the session registry for its input gate. Both used to spell
// Claude's keys and values themselves — `entry["type"] == "user"`, a struct
// tagged `json:"waitingFor"`, `status == "waiting"` — which made the
// controller the second decoder NFR6 exists to prevent. The controller now
// reads what these return, in this package's vocabulary, and names no Claude
// string at all. Its BEHAVIOUR is unchanged: every function here reproduces
// exactly what the inline decode it replaced did, including its edge cases,
// and the tests in line_test.go hold them to that.

// LineKind is which transcript record a line is, among the ones the pane
// controller acts on. Anything else — permission-mode, a summary, a record
// type Claude adds tomorrow — is LineOther, which the controller ignores.
type LineKind uint8

const (
	LineOther LineKind = iota
	LineUser
	LineAssistant
	LineSystem
	LineQueueOperation
	LineAttachment
)

// QueueOp is a queue-operation record's operation.
type QueueOp uint8

const (
	QueueOpOther QueueOp = iota
	QueueOpEnqueue
	QueueOpRemove
)

// Line is one transcript record, reduced to the facts the pane controller
// reads. Every field is the zero value when the record does not carry it.
type Line struct {
	Kind LineKind

	// CWD is the record's working directory, when it carries a non-empty one.
	CWD string
	// Timestamp is the record's own instant (DecodeRecordTime), zero when
	// missing or malformed.
	Timestamp time.Time

	// A user record: IsPrompt is true when its content is a STRING, which is
	// a real prompt, and Prompt is that string. Array content is a tool result
	// and is not a prompt (PromptText).
	Prompt   string
	IsPrompt bool

	// An assistant record. Model is message.model when it is a non-empty
	// string. Text is the LAST text block whose text is not blank, trimmed,
	// and HasText says one was found. Tool is the name of the LAST tool_use
	// block carrying a string name — which may be empty — and HasTool says one
	// was found.
	Model   string
	Text    string
	HasText bool
	Tool    string
	HasTool bool

	// A system record: TurnEnded is a stop_hook_summary.
	TurnEnded bool

	// A queue-operation record: the operation, its content (the prompt), and
	// whether a remove was the prompt being absorbed into the running turn.
	QueueOp       QueueOp
	QueueContent  string
	QueueAbsorbed bool

	// An attachment record: QueuedCommand is a queued_command attachment, with
	// its prompt and the attachment's OWN instant (the original enqueue time),
	// zero when it carries none.
	QueuedCommand bool
	QueuedPrompt  string
	QueuedAt      time.Time
}

// DecodeLine decodes one transcript line. ok is false when the line is not a
// JSON object — a partially flushed line, which the next read sees whole.
func DecodeLine(line []byte) (Line, bool) {
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		return Line{}, false
	}
	var l Line
	if cwd, ok := entry[KeyCWD].(string); ok && cwd != "" {
		l.CWD = cwd
	}
	l.Timestamp = DecodeRecordTime(entry[KeyTimestamp])

	msg, _ := entry[KeyMessage].(map[string]any)
	switch t, _ := entry[KeyRecordType].(string); t {
	case RecordTypeUser:
		l.Kind = LineUser
		l.Prompt, l.IsPrompt = PromptText(msg)

	case RecordTypeAssistant:
		l.Kind = LineAssistant
		if model, ok := msg[KeyModel].(string); ok && model != "" {
			l.Model = model
		}
		blocks, _ := msg[KeyContent].([]any)
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			switch bt, _ := block[KeyRecordType].(string); bt {
			case BlockTypeText:
				if txt, ok := block[KeyBlockText].(string); ok && strings.TrimSpace(txt) != "" {
					l.Text, l.HasText = strings.TrimSpace(txt), true
				}
			case BlockTypeToolUse:
				if name, ok := block[KeyToolName].(string); ok {
					l.Tool, l.HasTool = name, true
				}
			}
		}

	case RecordTypeSystem:
		l.Kind = LineSystem
		subtype, _ := entry[KeySubtype].(string)
		l.TurnEnded = subtype == SubtypeStopHookSummary

	case RecordTypeQueueOperation:
		l.Kind = LineQueueOperation
		l.QueueContent, _ = entry[KeyContent].(string)
		switch op, _ := entry[KeyOperation].(string); op {
		case OperationEnqueue:
			l.QueueOp = QueueOpEnqueue
		case OperationRemove:
			l.QueueOp = QueueOpRemove
		}
		reason, _ := entry[KeyReason].(string)
		l.QueueAbsorbed = reason == ReasonAbsorbedMidTurn

	case RecordTypeAttachment:
		l.Kind = LineAttachment
		a, _ := entry[KeyAttachment].(map[string]any)
		if t, _ := a[KeyRecordType].(string); t == AttachmentQueuedCommand {
			l.QueuedCommand = true
			l.QueuedPrompt, _ = a[KeyAttachmentPrompt].(string)
			l.QueuedAt = DecodeRecordTime(a[KeyTimestamp])
		}
	}
	return l, true
}

// LooksLikePermission reports whether a registry waitingFor value is about a
// permission prompt. It is a CONTAINS test, deliberately, and that is what the
// pane controller's input gate always did: a reworded permission prompt is
// still a permission dialog, and naming it so beats calling it a question.
func LooksLikePermission(waitingFor string) bool {
	return strings.Contains(waitingFor, permissionWord)
}

// ReadinessFields is the slice of one registry record the pane controller's
// input gate reads, as the RAW strings the tool wrote. The controller decodes
// Status through DecodeStatus where it compares it.
type ReadinessFields struct {
	Status     string
	WaitingFor string
	SessionID  string
}

// DecodeReadinessFields reads the three fields out of a registry record's
// bytes.
//
// ok is false in exactly the cases where the controller's old reader — a
// json.Unmarshal into a struct of three string fields — failed, because "not
// found" is what its cache and its scan act on:
//
//   - the bytes are not one JSON value (torn, or cut at the read bound), or are
//     followed by anything but whitespace;
//   - the value is not an object or null (an array, a number, a string);
//   - one of the three keys is present with a value that is neither a string
//     nor null.
//
// A JSON null record, and null values for the three keys, decode to zero
// fields with ok true, as before. Every other key is ignored whatever its
// type. The one difference from encoding/json's struct decoding is that key
// names match EXACTLY rather than case-insensitively: Claude Code writes them
// in one spelling, and a record that spelled "Status" would be a record from
// something else.
func DecodeReadinessFields(data []byte) (ReadinessFields, bool) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return ReadinessFields{}, false
	}
	var f ReadinessFields
	for _, field := range []struct {
		key string
		dst *string
	}{
		{KeyStatus, &f.Status},
		{KeyWaitingFor, &f.WaitingFor},
		{KeySessionID, &f.SessionID},
	} {
		switch v := raw[field.key].(type) {
		case nil:
		case string:
			*field.dst = v
		default:
			return ReadinessFields{}, false
		}
	}
	return f, true
}
