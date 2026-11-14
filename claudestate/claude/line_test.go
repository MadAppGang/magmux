package claude

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every record here is built from schema.go's constants, so a rename on
// Claude's side moves these tests with it. Re-expressed from magmux's
// TestDecodeLineReadsWhatTheControllerReads, in the new Line's shape, plus
// Line.CWD, Line.Model and QueueOp.AbsorbedMidTurn for the controller (review
// 4 Issue 7).

func record(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeLineReadsWhatTheControllerReads(t *testing.T) {
	const ts = "2026-09-18T12:00:00.250Z"
	at := time.Date(2026, 9, 18, 12, 0, 0, 250e6, time.UTC)
	enq := time.Date(2026, 9, 18, 11, 59, 0, 0, time.UTC)

	cases := []struct {
		name   string
		fields map[string]any
		want   Line
	}{
		{"a user prompt, with its cwd and instant",
			map[string]any{KeyRecordType: ValueRecordUser, KeyCWD: "/w", KeyTimestamp: ts, KeyMessage: map[string]any{KeyContent: "go"}},
			Line{Kind: LineUser, KindRaw: ValueRecordUser, CWD: "/w", Time: &at, prompt: "go", hasPrompt: true}},
		{"a tool result filed as a user record is not a prompt",
			map[string]any{KeyRecordType: ValueRecordUser, KeyMessage: map[string]any{KeyContent: []any{
				map[string]any{KeyRecordType: ValueBlockToolResult, KeyToolUseID: "t1", KeyContent: "out"}}}},
			Line{Kind: LineUser, KindRaw: ValueRecordUser, Blocks: []Block{{Kind: BlockToolResult, ToolID: "t1", Text: "out"}}}},
		{"an assistant record with its model and blocks in order",
			map[string]any{KeyRecordType: ValueRecordAssistant, KeyMessage: map[string]any{KeyModel: "m-1", KeyContent: []any{
				map[string]any{KeyRecordType: ValueBlockText, KeyBlockText: " first "},
				map[string]any{KeyRecordType: ValueBlockToolUse, KeyToolName: "Bash", KeyBlockID: "t1", KeyToolInput: map[string]any{"command": "ls"}},
				map[string]any{KeyRecordType: "image"},
			}}},
			Line{Kind: LineAssistant, KindRaw: ValueRecordAssistant, Model: "m-1", Blocks: []Block{
				{Kind: BlockText, Text: " first "},
				{Kind: BlockToolUse, ToolName: "Bash", ToolID: "t1", ToolInput: `{"command":"ls"}`},
				{Kind: BlockOther, Text: "image"},
			}}},
		{"the end of a turn",
			map[string]any{KeyRecordType: ValueRecordSystem, KeySubtype: ValueSubtypeStopHookSummary, KeyTimestamp: ts},
			Line{Kind: LineStopHookSummary, KindRaw: ValueRecordSystem, Time: &at}},
		{"a compaction",
			map[string]any{KeyRecordType: ValueRecordSystem, KeySubtype: ValueSubtypeCompactBoundary},
			Line{Kind: LineCompaction, KindRaw: ValueRecordSystem}},
		{"another system record",
			map[string]any{KeyRecordType: ValueRecordSystem, KeySubtype: "x"},
			Line{Kind: LineSystem, KindRaw: ValueRecordSystem}},
		{"an enqueue",
			map[string]any{KeyRecordType: ValueRecordQueueOperation, KeyOperation: ValueOperationEnqueue, KeyContent: "next", KeyTimestamp: ts},
			Line{Kind: LineQueueOperation, KindRaw: ValueRecordQueueOperation, Time: &at,
				Queue: &QueueOp{Op: QueueEnqueue, OpRaw: ValueOperationEnqueue, Content: "next"}}},
		{"a remove absorbed into the running turn",
			map[string]any{KeyRecordType: ValueRecordQueueOperation, KeyOperation: ValueOperationRemove, KeyReason: ValueReasonAbsorbedMidTurn},
			Line{Kind: LineQueueOperation, KindRaw: ValueRecordQueueOperation,
				Queue: &QueueOp{Op: QueueRemove, OpRaw: ValueOperationRemove, Reason: ValueReasonAbsorbedMidTurn, AbsorbedMidTurn: true}}},
		{"an enqueue whose reason says absorbed is not a removal",
			map[string]any{KeyRecordType: ValueRecordQueueOperation, KeyOperation: ValueOperationEnqueue, KeyReason: ValueReasonAbsorbedMidTurn},
			Line{Kind: LineQueueOperation, KindRaw: ValueRecordQueueOperation,
				Queue: &QueueOp{Op: QueueEnqueue, OpRaw: ValueOperationEnqueue, Reason: ValueReasonAbsorbedMidTurn}}},
		{"a queued command carrying its own enqueue instant",
			map[string]any{KeyRecordType: ValueRecordAttachment, KeyTimestamp: ts, KeyAttachment: map[string]any{
				KeyRecordType: ValueAttachmentQueued, KeyAttachmentPrompt: "p", KeyTimestamp: "2026-09-18T11:59:00Z"}},
			Line{Kind: LineAttachment, KindRaw: ValueRecordAttachment, Time: &at,
				Attachment: &Attachment{Kind: AttachmentQueuedCommand, KindRaw: ValueAttachmentQueued, Prompt: "p", Time: &enq}}},
		{"the colour record",
			map[string]any{KeyRecordType: ValueRecordAgentColor, KeyAgentColor: "teal"},
			Line{Kind: LineAgentColor, KindRaw: ValueRecordAgentColor, AgentColor: "teal"}},
		{"an API error and a side conversation",
			map[string]any{KeyRecordType: ValueRecordAssistant, KeyIsAPIErrorMessage: true, KeyIsSidechain: true},
			Line{Kind: LineAssistant, KindRaw: ValueRecordAssistant, APIError: true, Sidechain: true}},
		{"is_error is not an API error",
			map[string]any{KeyRecordType: ValueRecordUser, KeyIsError: true, KeyIsAPIErrorMessage: "true"},
			Line{Kind: LineUser, KindRaw: ValueRecordUser}},
		{"a type this package does not act on",
			map[string]any{KeyRecordType: ValueRecordPermissionMode},
			Line{Kind: LineOther, KindRaw: ValueRecordPermissionMode}},
		{"no type at all", map[string]any{KeyCWD: "/x"}, Line{Kind: LineUnknown, CWD: "/x"}},
		{"an implausible instant is named, not believed",
			map[string]any{KeyRecordType: ValueRecordUser, KeyTimestamp: "1970-01-02T00:00:00Z"},
			Line{Kind: LineUser, KindRaw: ValueRecordUser, BadInstant: KeyTimestamp}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := DecodeLine(record(t, c.fields))
			if !ok {
				t.Fatal("a well-formed record did not decode")
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
	for _, bad := range []string{`{"partial`, `[]`, `null`, ``} {
		if _, ok := DecodeLine([]byte(bad)); ok {
			t.Errorf("%q decoded", bad)
		}
	}
}

func TestPromptTextIsAStringContentOnly(t *testing.T) {
	l, _ := DecodeLine(record(t, map[string]any{KeyRecordType: ValueRecordUser, KeyMessage: map[string]any{KeyContent: ""}}))
	if s, ok := PromptText(l); !ok || s != "" {
		t.Fatalf("an empty prompt is still a prompt: %q %v", s, ok)
	}
	l, _ = DecodeLine(record(t, map[string]any{KeyRecordType: ValueRecordAssistant, KeyMessage: map[string]any{KeyContent: "x"}}))
	if _, ok := PromptText(l); ok {
		t.Fatal("an assistant record is never a prompt")
	}
}

// ParseTurns, re-expressed over DecodeLine: one assistant reply split across
// records is one turn, a tool result belongs to its call, and the bounds keep
// the newest turns, at least one.
func TestParseTurnsJoinsRepliesAndAttachesResults(t *testing.T) {
	lines := []map[string]any{
		{KeyRecordType: ValueRecordUser, KeyTimestamp: "2026-09-18T01:00:00Z", KeyMessage: map[string]any{KeyContent: "list files"}},
		{KeyRecordType: ValueRecordAssistant, KeyMessage: map[string]any{KeyContent: []any{
			map[string]any{KeyRecordType: ValueBlockText, KeyBlockText: "Sure."},
			map[string]any{KeyRecordType: ValueBlockToolUse, KeyToolName: "Bash", KeyBlockID: "t1", KeyToolInput: map[string]any{"command": "ls"}},
		}}},
		{KeyRecordType: ValueRecordUser, KeyMessage: map[string]any{KeyContent: []any{
			map[string]any{KeyRecordType: ValueBlockToolResult, KeyToolUseID: "t1", KeyContent: []any{map[string]any{KeyRecordType: ValueBlockText, KeyBlockText: "a\nb"}}},
		}}},
		{KeyRecordType: ValueRecordAssistant, KeyMessage: map[string]any{KeyContent: []any{
			map[string]any{KeyRecordType: ValueBlockText, KeyBlockText: " Two files. "},
		}}},
		{KeyRecordType: ValueRecordUser, KeyMessage: map[string]any{KeyContent: "thanks"}},
	}
	var b strings.Builder
	for _, l := range lines {
		b.Write(record(t, l))
		b.WriteString("\n{partial\n")
	}
	turns, err := ParseTurns(strings.NewReader(b.String()), 10, 1<<20)
	if err != nil || len(turns) != 3 {
		t.Fatalf("%d turns, %v", len(turns), err)
	}
	if turns[0].Text != "list files" || turns[0].Timestamp.IsZero() {
		t.Errorf("turn 0 %+v", turns[0])
	}
	if turns[1].Text != "Sure.\n\nTwo files." || len(turns[1].Tools) != 1 || turns[1].Tools[0].Result != "a\nb" ||
		turns[1].Tools[0].Input != `{"command":"ls"}` {
		t.Errorf("turn 1 %+v", turns[1])
	}
	last, _ := ParseTurns(strings.NewReader(b.String()), 1, 1<<20)
	if len(last) != 1 || last[0].Text != "thanks" {
		t.Errorf("maxTurns 1: %+v", last)
	}
	tiny, _ := ParseTurns(strings.NewReader(b.String()), 10, 1)
	if len(tiny) != 1 {
		t.Errorf("a byte budget keeps at least the newest turn: %d", len(tiny))
	}
}
