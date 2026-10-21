package adapters

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Every record here is built from schema.go's constants, so a rename on
// Claude's side moves these tests with it, and this file names no Claude
// string itself (TestOnlySchemaFileDecodesClaudeStrings).

func record(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestDecodeLineReadsWhatTheControllerReads(t *testing.T) {
	const ts = "2026-09-18T12:00:00.250Z"
	at, _ := time.Parse(time.RFC3339Nano, ts)
	enqueuedAt, _ := time.Parse(time.RFC3339Nano, "2026-09-18T11:59:00Z")

	cases := []struct {
		name   string
		fields map[string]any
		want   Line
	}{
		{
			name: "a user prompt, with its cwd and instant",
			fields: map[string]any{KeyRecordType: RecordTypeUser, KeyCWD: "/w", KeyTimestamp: ts,
				KeyMessage: map[string]any{KeyContent: "go"}},
			want: Line{Kind: LineUser, CWD: "/w", Timestamp: at, Prompt: "go", IsPrompt: true},
		},
		{
			name: "a tool result filed as a user record is not a prompt",
			fields: map[string]any{KeyRecordType: RecordTypeUser,
				KeyMessage: map[string]any{KeyContent: []any{map[string]any{KeyRecordType: BlockTypeToolResult}}}},
			want: Line{Kind: LineUser},
		},
		{
			name: "an assistant record: last non-blank text wins, trimmed; last tool name wins, even empty",
			fields: map[string]any{KeyRecordType: RecordTypeAssistant, KeyMessage: map[string]any{
				KeyModel: "m-1",
				KeyContent: []any{
					map[string]any{KeyRecordType: BlockTypeText, KeyBlockText: " first "},
					map[string]any{KeyRecordType: BlockTypeToolUse, KeyToolName: "Bash"},
					map[string]any{KeyRecordType: BlockTypeText, KeyBlockText: "  "},
					map[string]any{KeyRecordType: BlockTypeToolUse, KeyToolName: ""},
					map[string]any{KeyRecordType: BlockTypeToolUse, KeyToolName: 7},
				},
			}},
			want: Line{Kind: LineAssistant, Model: "m-1", Text: "first", HasText: true, Tool: "", HasTool: true},
		},
		{
			name: "an assistant record with an empty model and no blocks",
			fields: map[string]any{KeyRecordType: RecordTypeAssistant,
				KeyMessage: map[string]any{KeyModel: ""}},
			want: Line{Kind: LineAssistant},
		},
		{
			name:   "the end of a turn",
			fields: map[string]any{KeyRecordType: RecordTypeSystem, KeySubtype: SubtypeStopHookSummary, KeyTimestamp: ts},
			want:   Line{Kind: LineSystem, Timestamp: at, TurnEnded: true},
		},
		{
			name:   "another system record",
			fields: map[string]any{KeyRecordType: RecordTypeSystem, KeySubtype: "x"},
			want:   Line{Kind: LineSystem},
		},
		{
			name: "an enqueue",
			fields: map[string]any{KeyRecordType: RecordTypeQueueOperation, KeyOperation: OperationEnqueue,
				KeyContent: "next", KeyTimestamp: ts},
			want: Line{Kind: LineQueueOperation, Timestamp: at, QueueOp: QueueOpEnqueue, QueueContent: "next"},
		},
		{
			name: "a remove that was absorbed into the running turn",
			fields: map[string]any{KeyRecordType: RecordTypeQueueOperation, KeyOperation: OperationRemove,
				KeyReason: ReasonAbsorbedMidTurn},
			want: Line{Kind: LineQueueOperation, QueueOp: QueueOpRemove, QueueAbsorbed: true},
		},
		{
			name: "a queued command, carrying its own enqueue instant",
			fields: map[string]any{KeyRecordType: RecordTypeAttachment, KeyTimestamp: ts,
				KeyAttachment: map[string]any{KeyRecordType: AttachmentQueuedCommand,
					KeyAttachmentPrompt: "p", KeyTimestamp: "2026-09-18T11:59:00Z"}},
			want: Line{Kind: LineAttachment, Timestamp: at, QueuedCommand: true, QueuedPrompt: "p", QueuedAt: enqueuedAt},
		},
		{
			name: "some other attachment",
			fields: map[string]any{KeyRecordType: RecordTypeAttachment,
				KeyAttachment: map[string]any{KeyRecordType: "x"}},
			want: Line{Kind: LineAttachment},
		},
		{
			name:   "a record type the controller ignores",
			fields: map[string]any{KeyRecordType: "permission-mode"},
			want:   Line{Kind: LineOther},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := DecodeLine(record(t, c.fields))
			if !ok {
				t.Fatal("a well-formed record did not decode")
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("DecodeLine\n got %+v\nwant %+v", got, c.want)
			}
		})
	}

	if _, ok := DecodeLine([]byte(`{"partial`)); ok {
		t.Error("a partially flushed line decoded")
	}
}

// TestDecodeReadinessFieldsAgreesWithUnmarshal holds the controller's registry
// read to EXACTLY what its old json.Unmarshal into a three-field struct
// accepted and refused, because "not found" drives its cache and its scan.
//
// The reference struct is built with reflect.StructOf, its tags assembled from
// schema.go's constants, so this file carries no struct tag spelling a Claude
// key — which the decoder guard rightly refuses outside schema.go.
func TestDecodeReadinessFieldsAgreesWithUnmarshal(t *testing.T) {
	tag := func(key string) reflect.StructTag { return reflect.StructTag(`json:"` + key + `"`) }
	refType := reflect.StructOf([]reflect.StructField{
		{Name: "Status", Type: reflect.TypeFor[string](), Tag: tag(KeyStatus)},
		{Name: "WaitingFor", Type: reflect.TypeFor[string](), Tag: tag(KeyWaitingFor)},
		{Name: "SessionID", Type: reflect.TypeFor[string](), Tag: tag(KeySessionID)},
	})
	reference := func(b []byte) (ReadinessFields, bool) {
		v := reflect.New(refType)
		if err := json.Unmarshal(b, v.Interface()); err != nil {
			return ReadinessFields{}, false
		}
		e := v.Elem()
		return ReadinessFields{Status: e.Field(0).String(), WaitingFor: e.Field(1).String(),
			SessionID: e.Field(2).String()}, true
	}

	valid := record(t, map[string]any{KeyStatus: statusWaiting, KeyWaitingFor: waitingForPermission,
		KeySessionID: "s", KeyPID: 42, KeyName: map[string]any{"any": "shape"}})
	inputs := map[string][]byte{
		"a whole record":         valid,
		"cut at the read bound":  valid[:len(valid)/2],
		"empty":                  []byte(""),
		"whitespace":             []byte("  \n"),
		"null":                   []byte("null"),
		"an array":               []byte(`[1,2]`),
		"a number":               []byte(`7`),
		"a string":               []byte(`"x"`),
		"trailing garbage":       append(append([]byte{}, valid...), []byte(" x")...),
		"trailing whitespace":    append(append([]byte{}, valid...), []byte(" \n")...),
		"status is a number":     record(t, map[string]any{KeyStatus: 5, KeySessionID: "s"}),
		"waitingFor is a list":   record(t, map[string]any{KeyWaitingFor: []any{"x"}}),
		"sessionId is an object": record(t, map[string]any{KeySessionID: map[string]any{}}),
		"the three are null":     record(t, map[string]any{KeyStatus: nil, KeyWaitingFor: nil, KeySessionID: nil}),
		"other keys only":        record(t, map[string]any{KeyPID: "not a number", KeyCWD: 3}),
		"an unknown status word": record(t, map[string]any{KeyStatus: "compacting"}),
	}
	for name, b := range inputs {
		t.Run(name, func(t *testing.T) {
			got, gotOK := DecodeReadinessFields(b)
			want, wantOK := reference(b)
			if gotOK != wantOK || got != want {
				t.Errorf("DecodeReadinessFields = %+v, %v; json.Unmarshal into the old struct = %+v, %v",
					got, gotOK, want, wantOK)
			}
		})
	}

	// The one stated difference: a key spelled in another case is not the key.
	folded := []byte(`{"STATUS":"busy"}`)
	if got, ok := DecodeReadinessFields(folded); !ok || got.Status != "" {
		t.Errorf("a case-folded key was read as the status: %+v, %v", got, ok)
	}
}

func TestLooksLikePermission(t *testing.T) {
	for in, want := range map[string]bool{
		waitingForPermission:  true,
		waitingForInput:       false,
		"":                    false,
		"a permission dialog": true,
	} {
		if got := LooksLikePermission(in); got != want {
			t.Errorf("LooksLikePermission(%q) = %v, want %v", in, got, want)
		}
	}
}
