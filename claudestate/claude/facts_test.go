package claude

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
)

func factsOf(t *testing.T, n int, rel ...string) Facts {
	t.Helper()
	tail, w := tailOf(readCorpus(t, rel...), n)
	return DecodeFacts(tail, w)
}

func mmTranscript(dir, id string) []string {
	return []string{"roots", "magmux-corpus", "projects", dir, id + TranscriptSuffix}
}

func ttTranscript(dir, id string) []string {
	return []string{"roots", "tt-corpus", "projects", dir, id + TranscriptSuffix}
}

func colourFile(id string) []string {
	return []string{"roots", "agent-color", "projects", "-Users-user-dev-colour", id + TranscriptSuffix}
}

// Errors come from isApiErrorMessage only, never from is_error: every one of
// 497 observed is_error was RECOVERED. Re-expressed from magmux's
// TestErrorsComeFromIsApiErrorMessage, TestRecoveredIsErrorIsNotAnError and
// TestMixedErrorSignalsCountOnlyApiErrors, plus tt's AC5 pair.
func TestAPIErrorsCountOnlyIsApiErrorMessage(t *testing.T) {
	cases := []struct {
		rel  []string
		want int
	}{
		{mmTranscript("-Users-user-dev-magmux", "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b"), 3},
		{mmTranscript("-Users-user-dev-agent-fleet-sessions", "3b90f6cc-1d47-4e20-8a3b-9f5e6c0d2a18"), 0},
		{mmTranscript("-Users-user-dev-sampleapp", "8e2d4b17-0c93-4a6f-b512-77c3ad9e4f60"), 1},
		{ttTranscript("-Users-user-proj-d", "dddddddd-dddd-4ddd-8ddd-dddddddddddd"), 0},
		{ttTranscript("-Users-user-proj-e", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"), 2},
	}
	for _, c := range cases {
		if f := factsOf(t, FactsTailBytes, c.rel...); f.APIErrors != c.want {
			t.Errorf("%s: %d API errors, want %d", c.rel[len(c.rel)-1], f.APIErrors, c.want)
		}
	}
	f := factsOf(t, FactsTailBytes, mmTranscript("-Users-user-dev-magmux", "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b")...)
	if f.LastAPIError == "" {
		t.Error("the last API error's text is kept raw for the host (D13)")
	}
}

// D2: colour is the LAST type:"agent-color" record's agentColor and nothing
// else. A record of another type carrying agentColor, and a bare color key,
// are ignored (tt's AC6, re-expressed over new files with real records).
func TestColourIsTheLastAgentColorRecordOnly(t *testing.T) {
	cases := []struct {
		rel  []string
		n    int
		want *string
	}{
		{colourFile("c0100001-0000-4000-8000-000000000001"), FactsTailBytes, ptr("yellow")},
		{colourFile("c0100002-0000-4000-8000-000000000002"), FactsTailBytes, ptr("purple")},
		{colourFile("c0100002-0000-4000-8000-000000000002"), 8 << 10, nil}, // no head fallback (D2)
		{colourFile("c0100003-0000-4000-8000-000000000003"), FactsTailBytes, ptr("orange")},
		{colourFile("c0100004-0000-4000-8000-000000000004"), FactsTailBytes, ptr("")},
		{ttTranscript("-Users-user-proj-c", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"), FactsTailBytes, ptr("green")},
		{ttTranscript("-Users-user-proj-b", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), FactsTailBytes, ptr("cyan")},
		{ttTranscript("-Users-user-proj-b", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"), 2 << 10, nil},
	}
	for _, c := range cases {
		f := factsOf(t, c.n, c.rel...)
		if !samePtr(f.Color, c.want) {
			t.Errorf("%s at %d bytes: colour %v, want %v", c.rel[len(c.rel)-1], c.n, show(f.Color), show(c.want))
		}
	}
}

// The S-origin transcripts carry agentColor only on records whose type is not
// agent-color, so under D2 they have NO colour. That is the named D2 wire
// change, pinned so it cannot happen silently in the other direction.
func TestTheSOriginColoursAreTheNamedD2Change(t *testing.T) {
	for _, id := range []string{
		"a1478d55-6e02-4bb9-93cf-01d2e8f47a6b", "c874de20-33c1-480a-ba90-8ae3f2f332c4",
		"d0c31e88-4f17-4a52-8e6d-b9a70c55f213", "2f7a8e14-9c63-4051-b7d8-3e1a6f90c4b7",
	} {
		if f := factsOf(t, FactsTailBytes, mmTranscript("-Users-user-dev-magmux", id)...); f.Color != nil {
			t.Errorf("%s: colour %q from a record that is not agent-color", id, *f.Color)
		}
	}
	b := readCorpus(t, mmTranscript("-Users-user-dev-magmux", "d0c31e88-4f17-4a52-8e6d-b9a70c55f213")...)
	if !strings.Contains(string(b), `"`+KeyAgentColor+`"`) {
		t.Fatal("the S-origin file no longer carries agentColor; the D2 case is gone")
	}
}

// D10: a window cut mid-file discards its partial first line, so a fragment
// that happens to parse is never counted.
func TestAPartialWindowDropsItsFirstLine(t *testing.T) {
	line := `{"type":"assistant","isApiErrorMessage":true}` + "\n"
	b := []byte(line + line + line)
	tail, w := tailOf(b, len(line)*2+5) // starts 5 bytes before the second line ends... mid-record
	if !w.Partial {
		t.Fatal("the window is not partial")
	}
	if f := DecodeFacts(tail, w); f.APIErrors != 2 {
		t.Fatalf("%d errors in a window holding two whole records", f.APIErrors)
	}
	full, w := tailOf(b, len(b))
	if f := DecodeFacts(full, w); w.Partial || f.APIErrors != 3 {
		t.Fatalf("a whole-file window: %d", f.APIErrors)
	}
}

// D3: the context is input + cache_creation + cache_read from the NEWEST
// main-thread assistant usage; a synthetic or sidechain record does not count,
// and a newer compaction replaces it with postTokens.
func TestContextTokensComeFromTheNewestUsageOrACompaction(t *testing.T) {
	usage := func(model string, side bool, in, cc, cr int) string {
		s := `{"type":"assistant","message":{"model":"` + model + `","usage":{"input_tokens":` + itoa(in) +
			`,"cache_creation_input_tokens":` + itoa(cc) + `,"cache_read_input_tokens":` + itoa(cr) + `}}}`
		if side {
			s = strings.Replace(s, `{"type"`, `{"isSidechain":true,"type"`, 1)
		}
		return s + "\n"
	}
	b := usage("claude-a", false, 1, 2, 3) + usage("claude-b", false, 10, 20, 30) +
		usage(ValueModelSynthetic, false, 0, 0, 0) + usage("claude-c", true, 100, 100, 100)
	f := DecodeFacts([]byte(b), model.Window{TailBytes: len(b), FileSize: int64(len(b))})
	if f.ContextTokens == nil || *f.ContextTokens != 60 || f.Model != "claude-b" {
		t.Fatalf("context %v model %q", f.ContextTokens, f.Model)
	}
	compact := `{"type":"system","subtype":"compact_boundary","compactMetadata":{"postTokens":21149}}` + "\n"
	f = DecodeFacts([]byte(b+compact), model.Window{TailBytes: 1 << 20})
	if f.ContextTokens == nil || *f.ContextTokens != 21149 || f.Compactions != 1 {
		t.Fatalf("after a compaction: %v %d", f.ContextTokens, f.Compactions)
	}
	bare := `{"type":"system","subtype":"compact_boundary","compactMetadata":{}}` + "\n"
	f = DecodeFacts([]byte(b+bare), model.Window{TailBytes: 1 << 20})
	if f.ContextTokens != nil {
		t.Fatalf("a compaction with no postTokens leaves the context unmeasurable: %v", *f.ContextTokens)
	}
	f = DecodeFacts([]byte(compact+b), model.Window{TailBytes: 1 << 20})
	if f.ContextTokens == nil || *f.ContextTokens != 60 {
		t.Fatalf("a usage newer than the compaction wins: %v", f.ContextTokens)
	}
}

// C4: an implausible timestamp inside the window is COUNTED and named.
func TestImplausibleInstantsInTheWindowAreCounted(t *testing.T) {
	b := []byte(`{"type":"user","timestamp":"1970-01-02T00:00:00Z"}` + "\n" + `{"type":"user","timestamp":"2026-09-18T00:00:00Z"}` + "\n")
	w := model.Window{TailBytes: len(b)}
	f := DecodeFacts(b, w)
	tm := DecodeTiming(b, w)
	if f.ImplausibleInstants != 1 || f.ImplausibleField != KeyTimestamp || tm.ImplausibleInstants != 1 {
		t.Fatalf("%+v %+v", f, tm)
	}
	if tm.LastActivityAt == nil || tm.LastActivityAt.Year() != 2026 {
		t.Fatalf("the plausible instant is still the activity: %v", tm.LastActivityAt)
	}
}

// The timing scan walks BACKWARDS past the untimestamped record types: on 4 of
// 5 real job transcripts the last line is one of them.
func TestTheTimingScanSkipsUntimestampedRecords(t *testing.T) {
	b := readCorpus(t, ttTranscript("-Users-user-proj-f", "ffffffff-ffff-4fff-8fff-ffffffffffff")...)
	tail, w := tailOf(b, TimingTailBytes)
	tm := DecodeTiming(tail, w)
	want := time.Date(2026, 9, 18, 2, 35, 12, 345e6, time.UTC)
	if tm.LastActivityAt == nil || !tm.LastActivityAt.Equal(want) {
		t.Fatalf("%v, want %v", tm.LastActivityAt, want)
	}
	// 78 KB of untimestamped records behind the only timestamp: at the
	// production bound the window holds none.
	g := readCorpus(t, ttTranscript("-Users-user-proj-g", "99999999-9999-4999-8999-999999999999")...)
	tail, w = tailOf(g, TimingTailBytes)
	if tm := DecodeTiming(tail, w); tm.LastActivityAt != nil || !w.Partial {
		t.Fatalf("proj-g at 64 KB: %v", tm.LastActivityAt)
	}
}

func TestLatestTextIsTheNewestAssistantText(t *testing.T) {
	b := []byte(`{"type":"assistant","timestamp":"2026-09-18T01:00:00Z","message":{"content":[{"type":"text","text":"first"}]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-09-18T01:01:00Z","message":{"content":[{"type":"text","text":"  second\n"},{"type":"tool_use","name":"Bash"},{"type":"text","text":"  "}]}}` + "\n" +
		`{"type":"assistant","isSidechain":true,"timestamp":"2026-09-18T01:02:00Z","message":{"content":[{"type":"text","text":"side"}]}}` + "\n" +
		`{"type":"last-prompt"}` + "\n")
	tm := DecodeTiming(b, model.Window{TailBytes: len(b)})
	if tm.LatestText != "  second\n" || tm.LatestTextAt == nil || tm.LatestTextAt.Minute() != 1 {
		t.Fatalf("%q at %v: the newest main-thread text, raw", tm.LatestText, tm.LatestTextAt)
	}
	if tm.LastActivityAt == nil || tm.LastActivityAt.Minute() != 2 {
		t.Fatalf("activity %v", tm.LastActivityAt)
	}
}

func ptr(s string) *string { return &s }

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func show(p *string) string {
	if p == nil {
		return "nil"
	}
	return "\"" + *p + "\""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
