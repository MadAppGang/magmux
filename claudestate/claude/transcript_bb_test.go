package claude_test

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
)

// Inferred wire shapes: message.content accepts a string or text-block array;
// tool_use uses id/name/input; tool_result uses tool_use_id/content. The contract
// names the decoded fields but does not enumerate those JSON layouts. No queue,
// attachment or positive compaction layout is invented in this suite.
func bbWindow(b []byte) model.Window {
	return model.Window{TailBytes: len(b), FileSize: int64(len(b))}
}

func TestBBDecodeLineAssistantBlocksAndRawText(t *testing.T) {
	line, ok := claude.DecodeLine([]byte(`{"type":"assistant","timestamp":"2023-11-14T22:13:20Z","cwd":"/testdata/live","isSidechain":true,"isApiErrorMessage":true,"message":{"model":"synthetic-model","content":[{"type":"text","text":"raw\u001b]52;c;x\u0007"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"/testdata/a"}}]}}`))
	if !ok || line.Kind != claude.LineAssistant || line.KindRaw != "assistant" || !line.Sidechain || !line.APIError || line.CWD != "/testdata/live" || line.Model != "synthetic-model" || len(line.Blocks) != 2 {
		t.Fatalf("assistant line = %+v, ok=%v", line, ok)
	}
	bbInstant(t, line.Time, time.UnixMilli(1700000000000))
	if line.Blocks[0].Kind != claude.BlockText || line.Blocks[0].Text != "raw\x1b]52;c;x\a" {
		t.Errorf("text sanitized or lost: %+v", line.Blocks[0])
	}
	if b := line.Blocks[1]; b.Kind != claude.BlockToolUse || b.ToolName != "Read" || b.ToolID != "tool-1" || !strings.Contains(b.ToolInput, "file_path") || !strings.Contains(b.ToolInput, "/testdata/a") {
		t.Errorf("tool block = %+v", b)
	}
	if _, ok := claude.PromptText(line); ok {
		t.Error("assistant content counted as user prompt")
	}
}

func TestBBDecodeLineNonObjectsUnknownKindsAndAPIErrorKey(t *testing.T) {
	for _, raw := range []string{"", "{", "[]", "null", "true", `"text"`} {
		if _, ok := claude.DecodeLine([]byte(raw)); ok {
			t.Errorf("nonobject %q accepted", raw)
		}
	}
	got, ok := claude.DecodeLine([]byte(`{"type":"future-line","cwd":"/testdata/raw"}`))
	if !ok || got.KindRaw != "future-line" || got.CWD != "/testdata/raw" {
		t.Errorf("unknown line lost raw fields: %+v, %v", got, ok)
	}
	got, ok = claude.DecodeLine([]byte(`{"type":"assistant","is_error":true,"message":{"content":[{"type":"text","text":"not an API error"}]}}`))
	if !ok || got.APIError {
		t.Errorf("is_error treated as isApiErrorMessage: %+v, %v", got, ok)
	}
	// Contract §4.10 B: transcript timestamps are RFC3339 strings, and C4's window
	// applies to the value. Both ends of the window are refused.
	for _, ts := range []string{"1970-01-01T00:00:01Z", "5138-11-16T09:46:40.001Z"} {
		got, ok = claude.DecodeLine([]byte(`{"type":"assistant","timestamp":"` + ts + `"}`))
		if !ok || got.Time != nil || got.BadInstant != "timestamp" {
			t.Errorf("implausible instant %s = %+v, %v", ts, got, ok)
		}
	}
	// A plausible millisecond NUMBER is the other JSON type: never coerced (§4.10 B).
	// DecodeLine has no Problem channel, so only the absence of a Time is asserted.
	got, ok = claude.DecodeLine([]byte(`{"type":"assistant","timestamp":1700000000000}`))
	if !ok || got.Time != nil {
		t.Errorf("numeric timestamp was coerced into an instant: %+v, %v", got, ok)
	}
}

func TestBBPromptTextFromUserLine(t *testing.T) {
	// Only the string-content shape is asserted. The contract does not say whether
	// a user line whose content is an array of text blocks is a prompt (array content
	// is also the tool_result shape), so that row was dropped as an inferred
	// expectation; it is an open question in the failure analysis, not a test.
	for _, raw := range []string{
		`{"type":"user","message":{"content":"synthetic prompt"}}`,
	} {
		line, ok := claude.DecodeLine([]byte(raw))
		if !ok || line.Kind != claude.LineUser {
			t.Fatalf("user decode = %+v, %v", line, ok)
		}
		text, ok := claude.PromptText(line)
		if !ok || text != "synthetic prompt" {
			t.Errorf("PromptText = %q, %v", text, ok)
		}
	}
}

func TestBBDecodeAgentColorUsesTypeNotIncidentalKey(t *testing.T) {
	b := []byte("{\"type\":\"agent-color\",\"agentColor\":\"red\"}\n" +
		"{\"type\":\"user\",\"agentColor\":\"incidental\",\"message\":{\"content\":\"prompt\"}}\n" +
		"{\"type\":\"agent-color\",\"agentColor\":\"blue\"}\n")
	if got := claude.DecodeFacts(b, bbWindow(b)); got.Color == nil || *got.Color != "blue" {
		t.Errorf("last typed color = %v; want blue", got.Color)
	}
	incidental := []byte("{\"type\":\"user\",\"agentColor\":\"incidental\"}\n")
	if got := claude.DecodeFacts(incidental, bbWindow(incidental)); got.Color != nil {
		t.Errorf("incidental agentColor became color: %v", got.Color)
	}
	line, ok := claude.DecodeLine([]byte(`{"type":"agent-color","agentColor":"blue"}`))
	if !ok || line.Kind != claude.LineAgentColor || line.AgentColor != "blue" {
		t.Errorf("agent-color line = %+v, %v", line, ok)
	}
}

func TestBBDecodeFactsAPIErrorsAndNewestMainContext(t *testing.T) {
	b := []byte(strings.Join([]string{
		`{"type":"assistant","timestamp":"2023-11-14T22:13:20Z","is_error":true,"message":{"model":"older-model","content":[{"type":"text","text":"ordinary result"}],"usage":{"input_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}`,
		`{"type":"assistant","timestamp":"2023-11-14T22:13:21Z","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"first error"}]}}`,
		`{"type":"assistant","timestamp":"2023-11-14T22:13:22Z","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"last error\u001b"}]}}`,
		`{"type":"assistant","timestamp":"2023-11-14T22:13:23Z","message":{"model":"main-model","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30}}}`,
		`{"type":"assistant","timestamp":"2023-11-14T22:13:24Z","isSidechain":true,"message":{"model":"side-model","usage":{"input_tokens":1000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":3000}}}`,
	}, "\n") + "\n")
	got := claude.DecodeFacts(b, bbWindow(b))
	if got.APIErrors != 2 || got.LastAPIError != "last error\x1b" {
		t.Errorf("API errors = %d, %q; want 2, raw last error", got.APIErrors, got.LastAPIError)
	}
	if got.ContextTokens == nil || *got.ContextTokens != 60 {
		t.Errorf("context = %v; want latest main usage sum 60", got.ContextTokens)
	}
	if got.Compactions != 0 {
		t.Errorf("compactions = %d without compaction record", got.Compactions)
	}
}

func TestBBDecodeTimingNewestTimestampAndAssistantText(t *testing.T) {
	b := []byte(strings.Join([]string{
		`{"type":"assistant","timestamp":"2023-11-14T22:13:20Z","message":{"content":[{"type":"text","text":"older answer"}]}}`,
		`{"type":"assistant","timestamp":"2023-11-14T22:13:21Z","message":{"content":[{"type":"text","text":"newest answer\u001b"}]}}`,
		`{"type":"user","timestamp":"2023-11-14T22:13:22Z","message":{"content":"new user activity"}}`,
	}, "\n") + "\n")
	got := claude.DecodeTiming(b, bbWindow(b))
	bbInstant(t, got.LastActivityAt, time.UnixMilli(1700000002000))
	bbInstant(t, got.LatestTextAt, time.UnixMilli(1700000001000))
	if got.LatestText != "newest answer\x1b" {
		t.Errorf("latest text = %q; want raw newest assistant text", got.LatestText)
	}
}

func TestBBFactsAndTimingDiscardTailCutFirstLine(t *testing.T) {
	first := `{"type":"assistant","timestamp":"2023-11-14T22:13:29Z","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"cut line must be ignored"}]}}`
	last := `{"type":"assistant","timestamp":"2023-11-14T22:13:20Z","message":{"content":[{"type":"text","text":"complete line"}]}}`
	b := []byte(first[30:] + "\n" + last + "\n")
	w := model.Window{TailBytes: len(b), FileSize: int64(len(b) + 100), Partial: true}
	facts := claude.DecodeFacts(b, w)
	if facts.APIErrors != 0 || facts.LastAPIError != "" {
		t.Errorf("cut first line contributed facts: %+v", facts)
	}
	timing := claude.DecodeTiming(b, w)
	if timing.LatestText != "complete line" {
		t.Errorf("cut first line contributed text: %+v", timing)
	}
	bbInstant(t, timing.LastActivityAt, time.UnixMilli(1700000000000))
}

func TestBBFactsAndTimingCountImplausibleInstants(t *testing.T) {
	// Contract §4.10 B: RFC3339 transcript timestamps, one below and one above C4's
	// 1e12..1e14 ms window. Each line counts once.
	b := []byte("{\"type\":\"assistant\",\"timestamp\":\"1970-01-01T00:00:01Z\",\"message\":{\"content\":\"one\"}}\n" +
		"{\"type\":\"assistant\",\"timestamp\":\"5138-11-16T09:46:40.001Z\",\"message\":{\"content\":\"two\"}}\n")
	facts := claude.DecodeFacts(b, bbWindow(b))
	timing := claude.DecodeTiming(b, bbWindow(b))
	if facts.ImplausibleInstants != 2 || facts.ImplausibleField != "timestamp" {
		t.Errorf("facts instant diagnostics = %+v", facts)
	}
	if timing.ImplausibleInstants != 2 || timing.ImplausibleField != "timestamp" || timing.LastActivityAt != nil {
		t.Errorf("timing instant diagnostics = %+v", timing)
	}
}

func TestBBEmptyTranscriptFactsAreDeterminedWithoutInventedEvidence(t *testing.T) {
	facts := claude.DecodeFacts(nil, model.Window{})
	if facts.APIErrors != 0 || facts.LastAPIError != "" || facts.Color != nil || facts.ContextTokens != nil || facts.Model != "" || facts.Compactions != 0 || facts.ImplausibleInstants != 0 {
		t.Errorf("empty facts = %+v", facts)
	}
	timing := claude.DecodeTiming(nil, model.Window{})
	if timing.LastActivityAt != nil || timing.LatestTextAt != nil || timing.LatestText != "" || timing.ImplausibleInstants != 0 {
		t.Errorf("empty timing = %+v", timing)
	}
}

func TestBBParseTurnsBoundsOrderingAndToolPairing(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2023-11-14T22:13:20Z","message":{"content":"first prompt"}}`,
		`{"type":"assistant","timestamp":"2023-11-14T22:13:21Z","message":{"content":[{"type":"text","text":"first answer"},{"type":"tool_use","id":"call-a","name":"Read","input":{"file_path":"/testdata/a"}}]}}`,
		`{"type":"user","timestamp":"2023-11-14T22:13:22Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-a","content":"tool output"}]}}`,
		`{"type":"user","timestamp":"2023-11-14T22:13:23Z","message":{"content":"last prompt"}}`,
		`{"type":"assistant","timestamp":"2023-11-14T22:13:24Z","message":{"content":[{"type":"text","text":"last answer"}]}}`,
	}, "\n") + "\n"
	turns, err := claude.ParseTurns(strings.NewReader(input), 20, 1<<20)
	if err != nil {
		t.Fatalf("ParseTurns: %v", err)
	}
	found := false
	for i, turn := range turns {
		if i > 0 && turn.Timestamp.Before(turns[i-1].Timestamp) {
			t.Errorf("turns not oldest first: %+v", turns)
		}
		for _, tool := range turn.Tools {
			if tool.Name == "Read" {
				found = true
				if tool.Result != "tool output" || !strings.Contains(tool.Input, "/testdata/a") {
					t.Errorf("tool use/result not paired: %+v", tool)
				}
			}
		}
	}
	if !found || len(turns) == 0 {
		t.Fatalf("turns/tool call lost: %+v", turns)
	}
	if turns[0].Role != model.TurnUser || turns[0].Text != "first prompt" || turns[len(turns)-1].Text != "last answer" {
		t.Errorf("turn text/roles = %+v", turns)
	}
	limited, err := claude.ParseTurns(strings.NewReader(input), 2, 1<<20)
	if err != nil || len(limited) > 2 {
		t.Errorf("maxTurns not respected: %+v, %v", limited, err)
	}
	// Contract §4.10 D: older turns are dropped oldest first until the rest fit the
	// budget, and the newest turn is always kept whole, even when it alone exceeds it.
	// Roles alternate so that each line is its own turn (consecutive lines of one
	// role are one turn).
	assistant := func(text string) string {
		return `{"type":"assistant","message":{"content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	user := func(text string) string {
		return `{"type":"user","message":{"content":"` + text + `"}}` + "\n"
	}
	small := user(strings.Repeat("a", 10)) + assistant(strings.Repeat("b", 10)) + user(strings.Repeat("c", 10))
	budgeted, err := claude.ParseTurns(strings.NewReader(small), 20, 24)
	if err != nil {
		t.Fatalf("bounded parse: %v", err)
	}
	if len(budgeted) != 2 || budgeted[0].Text != strings.Repeat("b", 10) || budgeted[1].Text != strings.Repeat("c", 10) {
		t.Errorf("budget 24 over three 10-byte turns = %+v; want the newest two, oldest first", budgeted)
	}
	bytes := 0
	for _, turn := range budgeted {
		bytes += len(turn.Text)
	}
	if bytes > 24 {
		t.Errorf("several small turns use %d bytes with budget 24: %+v", bytes, budgeted)
	}
	big := strings.Repeat("x", 100)
	lone, err := claude.ParseTurns(strings.NewReader(assistant(big)), 20, 24)
	if err != nil {
		t.Fatalf("bounded parse of a lone oversize turn: %v", err)
	}
	if len(lone) != 1 || lone[0].Text != big {
		t.Errorf("lone 100-byte turn at budget 24 = %+v; want it kept whole", lone)
	}
	two, err := claude.ParseTurns(strings.NewReader(user(strings.Repeat("y", 100))+assistant(big)), 20, 24)
	if err != nil {
		t.Fatalf("bounded parse of two oversize turns: %v", err)
	}
	if len(two) != 1 || two[0].Text != big {
		t.Errorf("two 100-byte turns at budget 24 = %+v; want only the newest, whole", two)
	}
}
