package act

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// auditNow is the fixed instant this file's records are stamped with. Time is
// injected, never read from the clock; see testdata/PROVENANCE.md rule 4.
var auditNow = time.Unix(1789545200, 0).UTC()

// testRecord is a well-formed record: an event, and a rule naming what
// authorised it. Both are required, and both are required for reasons the
// tests below assert directly.
func testRecord() Record {
	return Record{
		Event:  EventRefused,
		Verb:   VerbFocus,
		Tier:   TierNavigate,
		By:     RequesterCLI,
		Target: Target{SessionID: "id-a", PID: 4242},
		Policy: Decision{Stance: StanceAlways, Rule: "the test's policy: actions.navigate = always"},
	}
}

func newTestWriter(t *testing.T, path string) *Writer {
	t.Helper()
	w, err := NewWriter(WriterConfig{
		Path: path,
		Now:  func() time.Time { return auditNow },
		PID:  7777,
	})
	if err != nil {
		t.Fatalf("NewWriter(%s): %v", path, err)
	}
	return w
}

// TestTheTrailLivesInTheStateDirectoryAndNeverUnderClaude.
//
// NFR1 has no exceptions, including lockfiles and temporary files, because
// thirty live Claude Code processes own that tree and none of them expects a
// foreign writer. A WRONG IMPLEMENTATION puts the trail beside the data it
// describes — ~/.claude/magmux/sessions-audit.jsonl, which the user did
// separately approve as a directory magmux may own, and which is still the
// wrong default: a log that needs an exception to a rule stated as absolute is
// worse than one that does not.
//
// Nothing here opens anything. The resolution is over environment variables
// and the refusal is over a string, so the guard cannot itself touch the tree
// it protects.
func TestTheTrailLivesInTheStateDirectoryAndNeverUnderClaude(t *testing.T) {
	t.Run("XDG_STATE_HOME wins", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/var/state")
		got, err := DefaultAuditPath()
		if err != nil {
			t.Fatalf("DefaultAuditPath: %v", err)
		}
		if want := filepath.Join("/var/state", "magmux", AuditFileName); got != want {
			t.Fatalf("DefaultAuditPath is %s, want %s", got, want)
		}
	})

	t.Run("otherwise ~/.local/state", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "/home/someone")
		got, err := DefaultAuditPath()
		if err != nil {
			t.Fatalf("DefaultAuditPath: %v", err)
		}
		if want := filepath.Join("/home/someone", ".local", "state", "magmux", AuditFileName); got != want {
			t.Fatalf("DefaultAuditPath is %s, want %s", got, want)
		}
	})

	t.Run("a path under .claude is refused, however it is reached", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		for _, p := range []string{
			"/home/someone/.claude/sessions-audit.jsonl",
			"/home/someone/.claude/magmux/sessions-audit.jsonl",
			"/home/someone/.claude/../.claude/state/magmux/sessions-audit.jsonl",
			// Fix round 1, item 3: the case variants. On a Mac's default
			// case-insensitive APFS volume these ARE ~/.claude, and until the
			// round they passed. Path strings only — nothing is opened.
			"/Users/user/.Claude/magmux/trail.jsonl",
			"/Users/user/.CLAUDE/trail.jsonl",
		} {
			if err := CheckAuditPath(p); err == nil {
				t.Errorf("%s was accepted as an audit path", p)
			} else if !strings.Contains(err.Error(), "NFR1") {
				t.Errorf("%s was refused without naming the rule: %v", p, err)
			}
			if _, err := NewWriter(WriterConfig{Path: p}); err == nil {
				t.Errorf("NewWriter accepted %s", p)
			}
		}
	})

	t.Run("a relocated config dir is refused too", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "/opt/claude-config")
		if err := CheckAuditPath("/opt/claude-config/magmux/audit.jsonl"); err == nil {
			t.Error("a path under CLAUDE_CONFIG_DIR was accepted; that directory has no " +
				".claude in its name and is still Claude's")
		}
		// A sibling whose name merely starts the same way is not inside it.
		if err := CheckAuditPath("/opt/claude-config-backup/audit.jsonl"); err != nil {
			t.Errorf("a sibling directory was refused: %v", err)
		}
	})

	t.Run("the state directory itself is accepted", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		if err := CheckAuditPath("/home/someone/.local/state/magmux/" + AuditFileName); err != nil {
			t.Errorf("the default location was refused: %v", err)
		}
	})
}

// TestTheClaudeGuardPlacesARelativePathBeforeJudgingIt — fix round 1, item 3.
//
// `magmux agents focus --audit trail.jsonl` reaches NewWriter unmodified, and a
// relative path resolves against the working directory. Somebody who changed
// into ~/.claude to read a transcript and ran the command from there got a file
// created inside it, with no error: the guard cleaned the path and never made
// it absolute, so `trail.jsonl` had no `.claude` element to find.
//
// Nothing here goes near the real ~/.claude. The ".claude" below is a
// directory this test creates inside its own temporary directory, which is
// exactly as good a witness: the guard judges a path's ELEMENTS, and a
// temporary one has the same elements as the real one.
func TestTheClaudeGuardPlacesARelativePathBeforeJudgingIt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	t.Run("from a working directory inside a .claude tree", func(t *testing.T) {
		inside := filepath.Join(t.TempDir(), "home", ".claude", "projects", "-Users-someone-repo")
		if err := os.MkdirAll(inside, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(inside)

		// The brief's first two inputs, and one with a directory in it.
		for _, p := range []string{AuditFileName, "./trail.jsonl", "magmux/trail.jsonl"} {
			if err := CheckAuditPath(p); err == nil {
				t.Errorf("%q, from inside a .claude tree, was accepted as an audit path", p)
			} else if !strings.Contains(err.Error(), "NFR1") {
				t.Errorf("%q was refused without naming the rule: %v", p, err)
			}
			w, err := NewWriter(WriterConfig{Path: p})
			if err == nil {
				// Append is what creates the file; prove the refusal matters by
				// showing what it would have done.
				t.Errorf("NewWriter accepted %q and would write to %s", p, w.Path())
			}
		}
		entries, err := os.ReadDir(inside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("the guard's working directory is no longer empty: %d entries", len(entries))
		}
	})

	t.Run("a relative CLAUDE_CONFIG_DIR is placed the same way", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		t.Setenv("CLAUDE_CONFIG_DIR", "claude-config")
		if err := CheckAuditPath(filepath.Join(dir, "claude-config", "magmux", "audit.jsonl")); err == nil {
			t.Error("a path under a relative CLAUDE_CONFIG_DIR was accepted")
		}
		if err := CheckAuditPath("claude-config/audit.jsonl"); err == nil {
			t.Error("a relative path under a relative CLAUDE_CONFIG_DIR was accepted")
		}
	})

	t.Run("CLAUDE_CONFIG_DIR is compared without regard to case", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "/opt/Claude-Config")
		if err := CheckAuditPath("/opt/claude-config/magmux/audit.jsonl"); err == nil {
			t.Error("a path under CLAUDE_CONFIG_DIR spelled in another case was accepted")
		}
		if err := CheckAuditPath("/opt/claude-config-backup/audit.jsonl"); err != nil {
			t.Errorf("a sibling directory was refused: %v", err)
		}
	})

	t.Run("an accepted relative path is kept ABSOLUTE", func(t *testing.T) {
		// The path that was checked is the path that is opened. A writer that
		// kept the relative spelling would resolve it again at every Append,
		// against whatever the working directory had become by then.
		checked := t.TempDir()
		t.Chdir(checked)
		w, err := NewWriter(WriterConfig{Path: "trail.jsonl", Now: func() time.Time { return auditNow }})
		if err != nil {
			t.Fatalf("a relative path outside any .claude tree was refused: %v", err)
		}
		if !filepath.IsAbs(w.Path()) {
			t.Fatalf("the writer kept %q; it must keep the absolute path it checked", w.Path())
		}

		elsewhere := filepath.Join(t.TempDir(), "home", ".claude")
		if err := os.MkdirAll(elsewhere, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(elsewhere)
		if _, err := w.Append(testRecord()); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if _, err := os.Stat(filepath.Join(checked, "trail.jsonl")); err != nil {
			t.Errorf("the record is not where the path was checked: %v", err)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Errorf("after a chdir into a .claude tree, Append wrote there: %d entries", len(entries))
		}
	})
}

// TestARelativeXDGStateHomeIsIgnored — the XDG Base Directory specification:
// "If an implementation encounters a relative path in any of these variables it
// should consider the path invalid and ignore it." Honouring one would put the
// trail wherever the shell happened to be standing.
func TestARelativeXDGStateHomeIsIgnored(t *testing.T) {
	for _, rel := range []string{"state", "./state", "../somewhere/state"} {
		t.Setenv("XDG_STATE_HOME", rel)
		t.Setenv("HOME", "/home/someone")
		got, err := DefaultAuditPath()
		if err != nil {
			t.Fatalf("DefaultAuditPath: %v", err)
		}
		if want := filepath.Join("/home/someone", ".local", "state", "magmux", AuditFileName); got != want {
			t.Errorf("XDG_STATE_HOME=%q gave %s, want the default %s", rel, got, want)
		}
	}
}

// TestTheDirectoryAppearsOnlyWithTheFirstRecord — a magmux that never actuates
// leaves no trace, and a state directory that appears on every start is one
// nobody can tell the meaning of. It is also what makes the "create nothing"
// promise in NewWriter's doc comment checkable.
func TestTheDirectoryAppearsOnlyWithTheFirstRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "magmux")
	path := filepath.Join(dir, AuditFileName)
	w := newTestWriter(t, path)

	if _, err := os.Stat(dir); err == nil {
		t.Fatal("NewWriter created the state directory before anything was recorded")
	}
	if _, err := w.Append(testRecord()); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the first record did not create the trail: %v", err)
	}
}

// TestTheTrailIsAppendOnlyAndOneRecordPerLine.
//
// The requirement is that a record survives the process dying mid-write
// without corrupting the record before it. That is a property of HOW it is
// written — one marshalled line, one write, O_APPEND — so it is asserted three
// ways: earlier bytes are never revisited, each line is independently
// decodable, and a torn last line leaves everything before it readable.
func TestTheTrailIsAppendOnlyAndOneRecordPerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "magmux", AuditFileName)
	w := newTestWriter(t, path)

	first, err := w.Append(testRecord())
	if err != nil {
		t.Fatalf("first Append: %v", err)
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}

	second := testRecord()
	second.Event = EventAttempt
	second.Outcome = nil
	if _, err := w.Append(second); err != nil {
		t.Fatalf("second Append: %v", err)
	}
	afterSecond, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}

	if !strings.HasPrefix(string(afterSecond), string(afterFirst)) {
		t.Fatal("the second record rewrote the bytes of the first; the trail is not append-only")
	}
	lines := strings.Split(strings.TrimSuffix(string(afterSecond), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("two records produced %d lines", len(lines))
	}
	for i, line := range lines {
		var back Record
		if err := json.Unmarshal([]byte(line), &back); err != nil {
			t.Fatalf("line %d is not a record on its own: %v", i+1, err)
		}
	}

	// The sequence numbers are the writer's and they count from one, so an
	// attempt and its outcome can be correlated and a double fire is visible.
	if first.Seq != 1 || first.PID != 7777 {
		t.Fatalf("the first record is seq %d pid %d", first.Seq, first.PID)
	}
	if !first.At.Equal(auditNow) {
		t.Fatalf("the record was stamped %s, not from the injected clock", first.At)
	}

	// A torn final line — the shape a process killed mid-write leaves — does
	// not damage what came before it.
	torn := append([]byte(nil), afterSecond...)
	torn = append(torn, []byte(`{"at":"2026`)...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatalf("staging a torn line: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the torn trail: %v", err)
	}
	complete := strings.Split(string(body), "\n")
	var decoded int
	for _, line := range complete {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var back Record
		if json.Unmarshal([]byte(line), &back) == nil {
			decoded++
		}
	}
	if decoded != 2 {
		t.Fatalf("%d complete records survived a torn final line, want 2", decoded)
	}
}

// TestARecordThatCannotSayWhatAuthorisedItIsRefused — R5.A's "by which rule"
// is not optional, and a line that cannot answer it does not answer the
// question the trail exists for. Refusing loudly is how the defect gets found;
// the gate turns the refusal into a refused ACTION rather than an unaudited
// one.
func TestARecordThatCannotSayWhatAuthorisedItIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuditFileName)
	w := newTestWriter(t, path)

	noRule := testRecord()
	noRule.Policy.Rule = "   "
	if _, err := w.Append(noRule); err == nil {
		t.Error("a record with no rule was written")
	}

	noEvent := testRecord()
	noEvent.Event = EventUnknown
	if _, err := w.Append(noEvent); err == nil {
		t.Error("a record that does not say what it is a record of was written")
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("a refused record still created the trail")
	}
}

// TestTheRecordCarriesTheFourThingsR5ARequires — what was done, to which
// session, why, and by which rule. Asserted against the ENCODED form, because
// the trail is read by whatever comes later and the keys are the contract.
func TestTheRecordCarriesTheFourThingsR5ARequires(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuditFileName)
	w := newTestWriter(t, path)

	since := auditNow.Add(-90 * time.Second)
	session := obs.Session{
		ID: "id-a", PID: 4242, Name: "circl", PaneID: obs.Ptr(2),
		Status: obs.StatusWaiting, Waiting: obs.WaitingReasonInput,
		StatusSince: &since, Confidence: obs.TierAuthoritative,
		Attention: obs.Attention{Needed: obs.Ptr(true)},
	}
	before := StateOf(session, auditNow)

	r := testRecord()
	r.Event = EventOutcome
	r.Target = Target{SessionID: session.ID, PID: session.PID, Name: session.Name, PaneID: obs.Ptr(2)}
	r.Before = &before
	r.Outcome = &Outcome{Done: true, Detail: "focus moved from magmux pane 1 to pane 2"}
	if _, err := w.Append(r); err != nil {
		t.Fatalf("Append: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}
	for _, key := range []string{"at", "seq", "pid", "event", "verb", "tier", "target",
		"requestedBy", "policy", "before", "outcome"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("the record has no %q", key)
		}
	}
	policy, _ := doc["policy"].(map[string]any)
	if rule, _ := policy["rule"].(string); rule == "" {
		t.Error("the record does not say by which rule")
	}
	state, _ := doc["before"].(map[string]any)
	if got, _ := state["inStatusFor"].(string); got != "1m30s" {
		t.Errorf("the `before` state says inStatusFor %q, want 1m30s", got)
	}
	// Compared against the enum's own word rather than a literal: the status
	// vocabulary mirrors Claude's spelling on purpose, and a literal here
	// would read as this file decoding Claude's format — which is a rule
	// adapters.TestOnlySchemaFileDecodesClaudeStrings enforces by reading the
	// source.
	if got, _ := state["status"].(string); got != obs.StatusWaiting.String() {
		t.Errorf("the `before` state says status %q", got)
	}
}
