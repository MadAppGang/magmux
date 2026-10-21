package adapters_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
)

// Read only explicitly named JSON test data, never key siblings or live state.
func specRecord(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "testdata", "sessions", name+".json"))
	if e != nil {
		t.Fatal(e)
	}
	var record map[string]json.RawMessage
	if e = json.Unmarshal(b, &record); e != nil {
		t.Fatal(e)
	}
	return record
}
func specString(t *testing.T, r map[string]json.RawMessage, key string) string {
	t.Helper()
	b, ok := r[key]
	if !ok {
		return ""
	}
	var s string
	if e := json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	return s
}

// WRONG: a measured status is misclassified, or a future compacting value is guessed busy.
func TestSpecDecodeRecordedStatus(t *testing.T) {
	for _, tt := range []struct {
		file string
		want obs.Status
		ok   bool
	}{
		{"98198", obs.StatusIdle, true}, {"98693", obs.StatusBusy, true}, {"1724", obs.StatusWaiting, true}, {"15775", obs.StatusShell, true},
		{"60003", obs.StatusUnknown, false}, // SYNTHETIC future status; others REAL (PROVENANCE.md).
	} {
		t.Run(tt.file, func(t *testing.T) {
			raw := specString(t, specRecord(t, tt.file), adapters.KeyStatus)
			got, ok := adapters.DecodeStatus(raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("DecodeStatus(%q)=(%v,%v), want (%v,%v)", raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// WRONG: missing waitingFor defaults to input, or permission and input are collapsed.
func TestSpecDecodeRecordedWaiting(t *testing.T) {
	for _, tt := range []struct {
		file string
		want obs.WaitingReason
		ok   bool
	}{
		{"1724", obs.WaitingReasonInput, true},       // REAL.
		{"60002", obs.WaitingReasonPermission, true}, // SYNTHETIC: never observed in 1920 samples.
		{"60006", obs.WaitingReasonUnknown, false},   // SYNTHETIC omitted key.
	} {
		t.Run(tt.file, func(t *testing.T) {
			raw := specString(t, specRecord(t, tt.file), adapters.KeyWaitingFor)
			got, ok := adapters.DecodeWaitingFor(raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("DecodeWaitingFor(%q)=(%v,%v), want (%v,%v)", raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// WRONG: adapter applies normalization or accepts our wire vocabulary as Claude's schema.
func TestSpecDecodeForeignValues(t *testing.T) {
	for _, raw := range []string{"", "BUSY", " busy", "idle ", "error", "unknown", "working"} {
		t.Run("status/"+raw, func(t *testing.T) {
			got, ok := adapters.DecodeStatus(raw)
			if got != obs.StatusUnknown || ok {
				t.Fatalf("accepted %q as (%v,%v)", raw, got, ok)
			}
		})
	}
	for _, raw := range []string{"", "input", "permission", "none", "unknown", "Input needed", "input needed ", "future reason"} {
		t.Run("waiting/"+raw, func(t *testing.T) {
			got, ok := adapters.DecodeWaitingFor(raw)
			if got != obs.WaitingReasonUnknown || ok {
				t.Fatalf("accepted %q as (%v,%v)", raw, got, ok)
			}
		})
	}
}

// WRONG: encoder lowercases/collapses/trims, counts runes instead of UTF-16 units,
// hashes the sanitized/truncated input, forgets signed int32 overflow, or hashes at length 200.
// Hash suffixes are fixed independent vectors calculated from the specified 31x arithmetic;
// no implementation helper is used to manufacture an expected answer.
func TestSpecEncodeProjectDir(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"real_launch", "/Users/user/dev/magmux", "-Users-user-dev-magmux"},
		{"real_worktree", "/Users/user/dev/magmux/.claude/worktrees/claude-session-control", "-Users-user-dev-magmux--claude-worktrees-claude-session-control"},
		{"empty", "", ""}, {"root", "/", "-"}, {"case", "/AbC123/", "-AbC123-"},
		{"separators", "/foo//bar._ +", "-foo--bar----"}, {"tilde", "~/dev/x", "--dev-x"},
		{"nfc", "/Users/user/dev/café", "-Users-user-dev-caf-"},
		{"non_bmp", "/Users/user/dev/🚀rocket", "-Users-user-dev---rocket"}, {"cjk", "/中文", "---"},
		{"199", strings.Repeat("a", 199), strings.Repeat("a", 199)},
		{"200", strings.Repeat("a", 200), strings.Repeat("a", 200)},
		{"201", strings.Repeat("a", 201), strings.Repeat("a", 200) + "-rkvsv5"},
		{"utf16_200", strings.Repeat("a", 198) + "🚀", strings.Repeat("a", 198) + "--"},
		{"utf16_201", strings.Repeat("a", 199) + "🚀", strings.Repeat("a", 199) + "--rlxqjo"},
		{"original_dot", strings.Repeat("a", 200) + ".", strings.Repeat("a", 200) + "-rkvstq"},
		{"original_underscore", strings.Repeat("a", 200) + "_", strings.Repeat("a", 200) + "-rkvsv3"},
		// Re-derived when the corpus was sanitised: the input used to name a
		// real user, and changing the string changes the hash. Verified by an
		// independent awk implementation of the same arithmetic, not by this
		// package. The five vectors above it are the untouched ones.
		{"documented_negative_hash", "/Users/user/dev/" + strings.Repeat("x", 250), "-Users-user-dev-" + strings.Repeat("x", 184) + "-c9gw6w"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := adapters.EncodeProjectDir(tt.in); got != tt.want {
				t.Fatalf("EncodeProjectDir(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// WRONG: override works without config, admits path traversal/reserved devices,
// rejects a legal boundary value, or caches a previous environment's answer.
func TestSpecProjectDirOverride(t *testing.T) {
	for _, tt := range []struct{ name, config, override, want string }{
		{"disabled", "", "Chosen_1", "-Ab-C"}, {"enabled", "configured", "Chosen_1", "Chosen_1"},
		{"empty", "configured", "", "-Ab-C"}, {"one", "configured", "Z", "Z"},
		{"64", "configured", strings.Repeat("A", 64), strings.Repeat("A", 64)},
		{"65", "configured", strings.Repeat("A", 65), "-Ab-C"},
		{"slash", "configured", "../escape", "-Ab-C"}, {"space", "configured", "a b", "-Ab-C"},
		{"unicode", "configured", "é", "-Ab-C"}, {"newline", "configured", "valid\n", "-Ab-C"},
		{"con", "configured", "CoN", "-Ab-C"}, {"prn", "configured", "PRN", "-Ab-C"},
		{"aux", "configured", "aux", "-Ab-C"}, {"nul", "configured", "NUL", "-Ab-C"},
		{"com0", "configured", "COM0", "-Ab-C"}, {"com9", "configured", "com9", "-Ab-C"},
		{"lpt0", "configured", "lpt0", "-Ab-C"}, {"lpt9", "configured", "LPT9", "-Ab-C"},
		{"not_reserved", "configured", "COM10", "COM10"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tt.config)
			t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", tt.override)
			if got := adapters.ProjectDirName("/Ab.C"); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
