package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/internal/obs/adapters"
)

// The error count's WINDOW — fix round 1, item 6.
//
// Every transcript in this package's testdata is smaller than the 200 KB tail
// the middleware reads, so every test before this one counted errors over a
// whole file and could not see what live validation found on 2026-09-19: 13 of
// 36 real sessions had API errors, every one outside the window, and each was
// published as a determined `apiErrors: 0` with an empty detail. The count is
// now documented as RECENT, and the window is disclosed on a zero exactly as on
// a count. These transcripts are built in t.TempDir() at the production bound,
// so the disclosure says the number the product uses.

// errorRecord is one API-level error, spelled with the schema's own names.
func errorRecord(t *testing.T, text string) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		adapters.KeyRecordType:        adapters.RecordTypeAssistant,
		adapters.KeyIsAPIErrorMessage: true,
		adapters.KeyMessage: map[string]any{
			adapters.KeyContent: []any{map[string]any{
				adapters.KeyRecordType: adapters.BlockTypeText,
				adapters.KeyBlockText:  text,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// fillerRecord is an ordinary turn of about a kilobyte, with no error in it.
func fillerRecord(t *testing.T) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		adapters.KeyRecordType: adapters.RecordTypeUser,
		adapters.KeyMessage:    map[string]any{adapters.KeyContent: strings.Repeat("x", 1000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// stageTranscript writes one session's transcript and returns the projects
// root and the file's size.
func stageTranscript(t *testing.T, id string, body []byte) (string, int64) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "-tmp-window")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+adapters.TranscriptSuffix)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, int64(len(body))
}

// kb is how the detail must spell a size under a megabyte. Written out here
// rather than borrowed from the code under test, so that the expectation is
// not computed by the thing it is checking.
func kb(n int64) string { return fmt.Sprintf("%d KB", n>>10) }

// transcriptOf is an error (or not) at the START, then enough ordinary turns
// to push it well past the production window, then an error (or not) at the
// END.
func transcriptOf(t *testing.T, errorFirst, errorLast bool) []byte {
	t.Helper()
	var b []byte
	if errorFirst {
		b = append(b, errorRecord(t, "API Error: 529 overloaded (long ago)")...)
	}
	for int64(len(b)) < adapters.DefaultTailBytes+64<<10 {
		b = append(b, fillerRecord(t)...)
	}
	if errorLast {
		b = append(b, errorRecord(t, "API Error: 500 internal (just now)")...)
	}
	return b
}

// TestAZeroOverAPartialWindowSaysHowMuchWasExamined is the brief's case
// exactly: a transcript larger than the window, an error BEFORE the window and
// none inside it. The value is a determined 0 — the count is of recent errors,
// and there are none — and the published detail says the window was partial,
// how much was examined, and out of how much.
func TestAZeroOverAPartialWindowSaysHowMuchWasExamined(t *testing.T) {
	const id = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	root, size := stageTranscript(t, id, transcriptOf(t, true, false))

	got := observe(t, newMiddleware(t, Config{Root: root}, id), session(id, 1))

	if got.Fault.APIErrors == nil || *got.Fault.APIErrors != 0 {
		t.Fatalf("Fault.APIErrors = %v, want a determined 0 — the only error is before the window",
			countOf(got.Fault.APIErrors))
	}
	detail := got.Fault.Detail
	if detail == "" {
		t.Fatal("a zero counted over the last 200 KB of a larger transcript was published with no " +
			"detail — a bare determined 0, which is what live validation found for 13 of 36 sessions")
	}
	for _, want := range []string{"last 200 KB", kb(size), "not counted"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail %q does not say %q", detail, want)
		}
	}

	// And it is PUBLISHED: the detail is on the wire beside the count.
	wire, err := json.Marshal(got.Fault)
	if err != nil {
		t.Fatal(err)
	}
	var fault map[string]any
	if err := json.Unmarshal(wire, &fault); err != nil {
		t.Fatal(err)
	}
	if fault["apiErrors"] != float64(0) || fault["detail"] != detail {
		t.Errorf("the published fault is %s", wire)
	}
	t.Logf("published: %s", wire)
}

// TestARecentErrorIsCountedWithTheWindowDisclosed is the positive control: the
// same transcript with a second error INSIDE the window counts exactly that
// one, names it, and discloses the window. Without it, the test above would
// pass against a middleware that counted nothing ever.
func TestARecentErrorIsCountedWithTheWindowDisclosed(t *testing.T) {
	const id = "77777777-8888-4999-aaaa-bbbbbbbbbbbb"
	root, size := stageTranscript(t, id, transcriptOf(t, true, true))

	got := observe(t, newMiddleware(t, Config{Root: root}, id), session(id, 1))

	if got.Fault.APIErrors == nil || *got.Fault.APIErrors != 1 {
		t.Fatalf("Fault.APIErrors = %v, want 1 — the recent error and not the old one",
			countOf(got.Fault.APIErrors))
	}
	for _, want := range []string{"just now", "last 200 KB", kb(size), "not counted"} {
		if !strings.Contains(got.Fault.Detail, want) {
			t.Errorf("the detail %q does not say %q", got.Fault.Detail, want)
		}
	}
	if strings.Contains(got.Fault.Detail, "long ago") {
		t.Errorf("the detail names the error from before the window: %q", got.Fault.Detail)
	}
}

// TestAWholeTranscriptZeroNeedsNoQualification: a transcript that fits in the
// window was examined whole, so its zero is exact and carries no detail —
// qualifying it would train a reader to skip the qualification that matters.
func TestAWholeTranscriptZeroNeedsNoQualification(t *testing.T) {
	const id = "88888888-9999-4aaa-bbbb-cccccccccccc"
	var body []byte
	for i := 0; i < 20; i++ {
		body = append(body, fillerRecord(t)...)
	}
	root, _ := stageTranscript(t, id, body)

	got := observe(t, newMiddleware(t, Config{Root: root}, id), session(id, 1))
	if got.Fault.APIErrors == nil || *got.Fault.APIErrors != 0 {
		t.Fatalf("Fault.APIErrors = %v, want a determined 0", countOf(got.Fault.APIErrors))
	}
	if got.Fault.Detail != "" {
		t.Errorf("a whole-transcript zero carries the detail %q; it is exact", got.Fault.Detail)
	}
}
