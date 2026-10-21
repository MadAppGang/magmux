package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Everything here reads internal/obs/testdata/projects, which is READ-ONLY to
// the suite. No test in this file writes into that tree, none touches
// ~/.claude, none probes a live pid, and none speaks to tmux. The two tests
// that need a tree of their own build it in t.TempDir().
//
// See testdata/PROVENANCE.md for what each transcript is and whether it was
// measured or constructed. The three session ids used most below:
//
//	a1478d55  pid 98693  the wrong-directory trap: its record's cwd is a
//	                     worktree, its transcript is filed under the parent
//	                     repository's name, and the cwd-derived directory
//	                     EXISTS with another session's transcript in it.
//	5f1c9a02  pid 98299  the 0-against-23 subagent case.
//	c874de20  pid 98198  the determined zero: an empty subagents directory.
const testdataProjects = "../testdata/projects"

const (
	// The session ids, and the directories they are actually filed under.
	trapSession    = "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b"
	trapDir        = "-Users-user-dev-magmux"
	trapLaunchDir  = "/Users/user/dev/magmux"
	trapLiveCWD    = "/Users/user/dev/magmux/.claude/worktrees/claude-session-control"
	decoySession   = "b0000000-1111-4222-8333-444444444444"
	subagentsMany  = "5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3"
	subagentsZero  = "c874de20-33c1-480a-ba90-8ae3f2f332c4"
	subagentsNone  = "3b90f6cc-1d47-4e20-8a3b-9f5e6c0d2a18"
	recoveredOnly  = "3b90f6cc-1d47-4e20-8a3b-9f5e6c0d2a18"
	bothErrSignals = "8e2d4b17-0c93-4a6f-b512-77c3ad9e4f60"
	colourLastWins = "d0c31e88-4f17-4a52-8e6d-b9a70c55f213"
	colourEarly    = "2f7a8e14-9c63-4051-b7d8-3e1a6f90c4b7"

	// noTranscriptSession has no transcript anywhere in the tree,
	// deliberately. It is the ONLY shape that makes a transcript-derived
	// answer undetermined.
	noTranscriptSession = "6c5b2f09-3a81-4d76-99e4-0f8c1b3a7d52"

	// poison is the agentColor the decoy transcript carries. It exists so that
	// reading the wrong session is LOUD instead of a subtly wrong colour, and
	// no test here may ever see it in an answer.
	poison = "WRONG-SESSION-DO-NOT-REPORT-THIS"
)

func testTranscripts(t *testing.T) Transcripts {
	t.Helper()
	pinTheDirNameOverride(t)
	return Transcripts{Root: testdataProjects}
}

// pinTheDirNameOverride clears Claude Code's own project-directory override for
// the duration of a test.
//
// Locate honours it — see ProjectDirName — which means a developer who has
// CLAUDE_CONFIG_DIR and CLAUDE_CODE_PROJECT_DIR_NAME set in their shell would
// otherwise get a different answer from the same test data. That is not a
// hypothetical environment: CLAUDE_CONFIG_DIR is the documented way to move the
// whole tree. Pinning it here keeps the suite's answer a property of the test
// data rather than of whoever is running it.
func pinTheDirNameOverride(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "")
}

func mustLocate(t *testing.T, tr Transcripts, id, launchDir string) Location {
	t.Helper()
	loc, err := tr.Locate(id, launchDir)
	if err != nil {
		t.Fatalf("Locate(%q, %q): %v", id, launchDir, err)
	}
	if !loc.Found() {
		t.Fatalf("Locate(%q, %q) found nothing", id, launchDir)
	}
	return loc
}

// ---------------------------------------------------------------------------
// The wrong-directory trap. This is the most valuable test in the phase.
// ---------------------------------------------------------------------------

// The trap has to be ARMED for the test below to mean anything, and that is a
// property of the test data rather than of the implementation: the directory
// you get by encoding the session's live working directory must EXIST and must
// hold a DIFFERENT session's transcript. If that ever stops being true, the
// decoy test passes for the wrong reason, so it is asserted separately and
// first.
func TestTheWrongDirectoryTrapIsArmedInTheTestData(t *testing.T) {
	derived := EncodeProjectDir(trapLiveCWD)
	if derived == trapDir {
		t.Fatalf("the live cwd encodes to %q, the same directory the transcript is really in — "+
			"there is no trap left to spring", derived)
	}

	dir := filepath.Join(testdataProjects, derived)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("the cwd-derived directory %s does not exist, so deriving the slug would fail "+
			"LOUDLY here — the whole point is that it does not: %v", dir, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), TranscriptSuffix) {
			found = append(found, e.Name())
		}
	}
	if len(found) == 0 {
		t.Fatal("the cwd-derived directory holds no transcript, so it is an empty decoy")
	}
	for _, name := range found {
		if strings.HasPrefix(name, trapSession) {
			t.Fatalf("the cwd-derived directory holds %s's own transcript (%s); the trap is disarmed",
				trapSession, name)
		}
	}
	t.Logf("trap armed: cwd %q encodes to %q, which exists and holds %v", trapLiveCWD, derived, found)
}

// The decoy directory is never read, and the session resolves to its own
// transcript.
//
// This is the 4% case made runnable. An implementation that derives the
// directory from the session's LIVE working directory does not fail loudly: it
// lands in a directory that exists and holds somebody else's transcript. Here
// that other transcript's colour is a shout, so a wrong answer cannot hide.
//
// Both inputs are tried, because both are ways the mistake happens: no launch
// directory at all (a session magmux did not spawn), and the WRONG launch
// directory (the live cwd, handed in by a caller that believed the record).
func TestDecoyProjectDirectoryIsNeverRead(t *testing.T) {
	tr := testTranscripts(t)
	want := filepath.Join(testdataProjects, trapDir, trapSession+TranscriptSuffix)
	decoyDir := filepath.Join(testdataProjects, EncodeProjectDir(trapLiveCWD))

	for _, c := range []struct {
		name      string
		launchDir string
	}{
		{"no launch directory", ""},
		{"the live working directory, which is the wrong input", trapLiveCWD},
		{"the real launch directory", trapLaunchDir},
	} {
		t.Run(c.name, func(t *testing.T) {
			loc := mustLocate(t, tr, trapSession, c.launchDir)

			if loc.Path != want {
				t.Errorf("resolved to\n got: %s\nwant: %s", loc.Path, want)
			}
			if strings.HasPrefix(loc.Path, decoyDir+string(filepath.Separator)) {
				t.Fatalf("resolved INTO the decoy directory: %s", loc.Path)
			}
			if strings.Contains(loc.Path, decoySession) {
				t.Fatalf("resolved to another session's transcript: %s", loc.Path)
			}

			read, err := tr.Read(loc)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if read.Color != nil && *read.Color == poison {
				t.Fatalf("reported the decoy session's colour %q as this session's", poison)
			}
			if read.Color == nil || *read.Color != "orange" {
				t.Errorf("colour = %v, want the session's own colour", derefColour(read.Color))
			}
		})
	}
}

// The launch directory answers without a scan; the live one falls through to
// the scan and still answers correctly.
//
// The METHOD is the observable part, and it is worth asserting because the two
// paths have different costs and different failure modes: one stat versus one
// directory listing plus a stat per project. A session magmux spawned should
// never pay for the second.
func TestLaunchDirectoryAnswersWithoutAScanAndTheLiveOneDoesNot(t *testing.T) {
	tr := testTranscripts(t)

	byLaunch := mustLocate(t, tr, trapSession, trapLaunchDir)
	if byLaunch.Method != LocatedByLaunchDir {
		t.Errorf("with the launch directory, method = %v, want %v", byLaunch.Method, LocatedByLaunchDir)
	}

	byScan := mustLocate(t, tr, trapSession, trapLiveCWD)
	if byScan.Method != LocatedByScan {
		t.Errorf("with the live cwd, method = %v, want %v — the wrong directory must MISS, "+
			"and a miss must fall through to the scan", byScan.Method, LocatedByScan)
	}
	if byLaunch.Path != byScan.Path {
		t.Errorf("the two paths disagree:\n launch: %s\n   scan: %s", byLaunch.Path, byScan.Path)
	}
}

// A session with no transcript anywhere is a MISS, and a miss is not an error.
func TestASessionWithNoTranscriptIsAMissNotAnError(t *testing.T) {
	tr := testTranscripts(t)
	loc, err := tr.Locate("6c5b2f09-3a81-4d76-99e4-0f8c1b3a7d52", "/Users/user/dev/magmux")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if loc.Found() {
		t.Fatalf("found a transcript for a session that deliberately has none: %s", loc.Path)
	}
	if loc.Matches != 0 {
		t.Errorf("Matches = %d, want 0", loc.Matches)
	}

	// And the consequence, which is the point of the record existing: nobody
	// counted, so nothing is known — not "zero".
	sub, err := tr.Subagents(loc)
	if err != nil {
		t.Fatalf("Subagents: %v", err)
	}
	if sub.Known {
		t.Errorf("a session with no transcript reported a KNOWN subagent count of %d", sub.Count)
	}
}

// Two project directories holding one session id is reported, not guessed at.
//
// api-surface.md raises this as ambiguity A8 and the corpus does not contain
// it, so it is built here — in a temp directory, never in the test data tree.
// The resolver answers deterministically and says how many it saw, which is
// the difference between a caller that can act on the ambiguity and one that
// never learns of it.
func TestTwoDirectoriesHoldingOneSessionAreCountedNotHidden(t *testing.T) {
	root := t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	for _, dir := range []string{"-a-first", "-b-second"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, id+TranscriptSuffix), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tr := Transcripts{Root: root}

	first := mustLocate(t, tr, id, "")
	if first.Matches != 2 {
		t.Errorf("Matches = %d, want 2 — a second copy must be counted, not shadowed", first.Matches)
	}
	for i := 0; i < 5; i++ {
		again := mustLocate(t, tr, id, "")
		if again.Path != first.Path {
			t.Fatalf("run %d resolved to %s, run 0 resolved to %s — the answer is not deterministic",
				i, again.Path, first.Path)
		}
	}
}

// ---------------------------------------------------------------------------
// Containment. Nothing outside the injected root is ever opened.
// ---------------------------------------------------------------------------

// Every path this type opens is Root joined with names it has checked to be
// single path components, so containment is structural rather than a rule
// somebody has to remember. The hostile inputs below are the ones that would
// otherwise escape.
func TestNoPathOutsideTheInjectedRootIsOpened(t *testing.T) {
	tr := testTranscripts(t)

	for _, id := range []string{
		"../sessions/98198",
		"..",
		".",
		"",
		"a/b",
		"/etc/passwd",
		filepath.Join("..", "..", "..", "etc", "passwd"),
	} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			loc, err := tr.Locate(id, "")
			if err == nil && loc.Found() {
				t.Fatalf("Locate(%q) resolved to %s", id, loc.Path)
			}
			if err == nil {
				t.Errorf("Locate(%q) returned no error and no path; a name that cannot be a "+
					"path component is a caller's bug and must be said so", id)
			}
		})
	}

	// And the positive half: a real answer is inside the root.
	loc := mustLocate(t, tr, trapSession, "")
	abs, err := filepath.Abs(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	rootAbs, err := filepath.Abs(tr.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		t.Errorf("resolved outside the root:\n path: %s\n root: %s", abs, rootAbs)
	}
}

// Reading the tree changes nothing in it.
//
// NFR1 says no exceptions, including lockfiles and temp files, and the reason
// is not tidiness: the real tree belongs to thirty live Claude Code processes
// that are writing it at the same time. A cache file dropped beside a
// transcript is a file magmux owns in a directory it does not.
func TestReadingTheWholeTreeChangesNothing(t *testing.T) {
	before := treeDigest(t, testdataProjects)

	tr := testTranscripts(t)
	for _, id := range []string{trapSession, subagentsMany, subagentsZero, subagentsNone,
		bothErrSignals, colourLastWins, colourEarly} {
		loc := mustLocate(t, tr, id, "")
		if _, err := tr.Read(loc); err != nil {
			t.Fatalf("Read(%s): %v", id, err)
		}
		if _, err := tr.Subagents(loc); err != nil {
			t.Fatalf("Subagents(%s): %v", id, err)
		}
		if _, err := tr.Stat(loc); err != nil {
			t.Fatalf("Stat(%s): %v", id, err)
		}
	}

	after := treeDigest(t, testdataProjects)
	if len(before) != len(after) {
		t.Fatalf("the tree has %d entries after reading it, and had %d before", len(after), len(before))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("%s changed:\n before: %s\n  after: %s", path, sum, after[path])
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s did not exist before the read", path)
		}
	}
}

// treeDigest is every file's path, size and content hash.
func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[path] = fmt.Sprintf("%d:%s", len(b), hex.EncodeToString(sum[:8]))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Faults.
// ---------------------------------------------------------------------------

// The error signal is isApiErrorMessage, and this transcript carries one of
// each measured class: transport, auth and quota.
func TestErrorsComeFromIsApiErrorMessage(t *testing.T) {
	tr := testTranscripts(t)
	read, err := tr.Read(mustLocate(t, tr, trapSession, ""))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.APIErrors != 3 {
		t.Errorf("APIErrors = %d, want 3", read.APIErrors)
	}
	if read.LastError == "" {
		t.Error("LastError is empty; a fault a human cannot read is a fault they cannot act on")
	}
}

// A tool result's own error flag is NOT an error signal. Every one of 497
// observed occurrences was recovered from, and counting them reports 29 of 32
// healthy sessions as broken. This transcript is that inversion's guard: it
// has recovered tool errors and no API errors at all.
func TestRecoveredIsErrorIsNotAnError(t *testing.T) {
	tr := testTranscripts(t)
	loc := mustLocate(t, tr, recoveredOnly, "")

	// The test data must actually contain the bait, or this test passes
	// against an implementation that counts the wrong key.
	raw, err := os.ReadFile(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), KeyIsError) {
		t.Fatalf("%s carries no %s records, so it cannot catch an implementation that counts them",
			loc.Path, KeyIsError)
	}

	read, err := tr.Read(loc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.APIErrors != 0 {
		t.Errorf("APIErrors = %d, want 0 — this session recovered from everything and finished cleanly",
			read.APIErrors)
	}
}

// The discriminating case: one transcript carrying BOTH signals. An
// implementation that counts both reports 4, one that counts only the tool
// flag reports 3, and a correct one reports 1. The two single-signal
// transcripts above cannot tell those three apart on their own.
func TestMixedErrorSignalsCountOnlyApiErrors(t *testing.T) {
	tr := testTranscripts(t)
	read, err := tr.Read(mustLocate(t, tr, bothErrSignals, ""))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	switch read.APIErrors {
	case 1:
	case 4:
		t.Error("APIErrors = 4: both signals are being counted")
	case 3:
		t.Errorf("APIErrors = 3: the tool-result flag is being counted instead of %s", KeyIsAPIErrorMessage)
	default:
		t.Errorf("APIErrors = %d, want 1", read.APIErrors)
	}
}

// ---------------------------------------------------------------------------
// Colour.
// ---------------------------------------------------------------------------

// Last record wins, and the key is agentColor — never the plain colour key
// that lives in the same tree with a different meaning. This transcript
// carries a decoy of the second sort BETWEEN two of the first, so an
// implementation reading the wrong key gets a visibly wrong answer instead of
// the right one by luck.
func TestAgentColourIsLastRecordWinsAndTheOtherColourKeyIsIgnored(t *testing.T) {
	tr := testTranscripts(t)
	loc := mustLocate(t, tr, colourLastWins, "")

	raw, err := os.ReadFile(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"`+KeySubagentColor+`"`) {
		t.Fatalf("%s carries no decoy colour key, so it cannot catch an implementation "+
			"that reads the wrong one", loc.Path)
	}

	read, err := tr.Read(loc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	switch derefColour(read.Color) {
	case "yellow":
	case "red":
		t.Errorf("colour = red: the decoy %q key was read instead of %q", KeySubagentColor, KeyAgentColor)
	case "blue":
		t.Error("colour = blue: the FIRST record won; the rule is last-record-wins")
	default:
		t.Errorf("colour = %v, want yellow", derefColour(read.Color))
	}
}

// No colour record at all is nil, and nil is the only spelling of absence:
// the empty string is a VALUE here — a session explicitly coloured with the
// default — so a reader that returns "" for "no record" has thrown away a
// distinction the tool makes.
func TestAbsentAgentColourIsNoOpinion(t *testing.T) {
	tr := testTranscripts(t)
	read, err := tr.Read(mustLocate(t, tr, subagentsNone, ""))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.Color != nil {
		t.Errorf("colour = %q, want nil for a transcript with no colour record", *read.Color)
	}
}

// The empty string survives as a value rather than collapsing to "no record".
func TestAnExplicitlyEmptyColourIsAValueAndNotAnAbsence(t *testing.T) {
	root := t.TempDir()
	const id = "22222222-3333-4444-8555-666666666666"
	dir := filepath.Join(root, "-tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{KeyAgentColor: ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+TranscriptSuffix), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := Transcripts{Root: root}
	read, err := tr.Read(mustLocate(t, tr, id, ""))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.Color == nil {
		t.Fatal("an explicitly empty colour came back as nil, which is the answer for NO record")
	}
	if *read.Color != "" {
		t.Errorf("colour = %q, want the empty string", *read.Color)
	}
}

// ---------------------------------------------------------------------------
// The bound.
// ---------------------------------------------------------------------------

// Transcripts are tailed, never scanned whole.
//
// The assertion is on BYTES READ rather than on elapsed time. A timing
// assertion passes on a 48 KB file and says nothing about the 630.9 MB
// transcript measured on the same machine, which is the file this bound
// exists for.
func TestTranscriptIsTailedNotScannedWhole(t *testing.T) {
	const bound = 8 << 10
	tr := Transcripts{Root: testdataProjects, TailBytes: bound}
	loc := mustLocate(t, tr, colourEarly, "")

	read, err := tr.Read(loc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.Size < 40<<10 {
		t.Fatalf("the test data is only %d bytes; it cannot exercise a %d-byte bound",
			read.Size, bound)
	}
	if !read.Truncated {
		t.Error("Truncated is false for a file larger than the bound")
	}
	if read.BytesRead >= read.Size {
		t.Errorf("read %d bytes of a %d-byte transcript — that is a whole-file scan",
			read.BytesRead, read.Size)
	}
	// Tail plus, at most, one head window.
	if max := int64(2 * bound); read.BytesRead > max {
		t.Errorf("read %d bytes, which is more than the two windows the bound allows (%d)",
			read.BytesRead, max)
	}
}

// A colour set early in a long session is still found, through the bounded
// head fallback — and the fallback is bounded too.
//
// This session's only colour record is in its first 400 bytes and the rest is
// 48 KB of later turns. With the production bound of 200 KB the whole file is
// inside the tail window and the fallback never runs; with a test bound of
// 8 KB it must, or a real session that set its colour once at startup reports
// no colour at all.
func TestColourSetEarlyIsFoundByBoundedFallback(t *testing.T) {
	const bound = 8 << 10
	small := Transcripts{Root: testdataProjects, TailBytes: bound}
	loc := mustLocate(t, small, colourEarly, "")

	read, err := small.Read(loc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !read.HeadFallback {
		t.Error("the head fallback did not run, so the colour below was found by scanning the whole file")
	}
	if got := derefColour(read.Color); got != "purple" {
		t.Errorf("colour = %v, want purple", got)
	}

	// The same file under the production bound: one window, no fallback, same
	// answer. That is the half which says the fallback is not load-bearing on
	// real transcripts.
	big := Transcripts{Root: testdataProjects}
	whole, err := big.Read(loc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if whole.Truncated || whole.HeadFallback {
		t.Errorf("a %d-byte transcript was truncated (%v) or needed the fallback (%v) under the "+
			"%d-byte production bound", whole.Size, whole.Truncated, whole.HeadFallback, DefaultTailBytes)
	}
	if derefColour(whole.Color) != derefColour(read.Color) {
		t.Errorf("the two bounds disagree: %v under the production bound, %v under %d bytes",
			derefColour(whole.Color), derefColour(read.Color), bound)
	}
}

// A window that starts mid-record drops that half-record rather than counting
// it as malformed data or, worse, decoding half of one.
func TestATruncatedWindowDropsItsPartialFirstRecord(t *testing.T) {
	root := t.TempDir()
	const id = "33333333-4444-4555-8666-777777777777"
	dir := filepath.Join(root, "-tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, `{"%s":true,"filler":"%s"}`+"\n", KeyIsAPIErrorMessage, strings.Repeat("x", 200))
	}
	if err := os.WriteFile(filepath.Join(dir, id+TranscriptSuffix), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tr := Transcripts{Root: root, TailBytes: 1000}
	read, err := tr.Read(mustLocate(t, tr, id, ""))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.Malformed != 0 {
		t.Errorf("Malformed = %d; the partial first line of a window is expected and must be "+
			"dropped, not reported", read.Malformed)
	}
	if read.APIErrors == 0 || read.APIErrors >= 40 {
		t.Errorf("APIErrors = %d, want some but not all 40 of them — the window is bounded",
			read.APIErrors)
	}
}

// ---------------------------------------------------------------------------
// Subagents.
// ---------------------------------------------------------------------------

// THE case. The transcript parses as zero subagents; the directory holds 23.
// Both numbers are measured here rather than asserted from memory, because the
// whole deliverable is the gap between them.
func TestSubagentCountComesFromMetaFilesAndNotTheTranscript(t *testing.T) {
	tr := testTranscripts(t)
	loc := mustLocate(t, tr, subagentsMany, "")

	fromFiles, err := tr.Subagents(loc)
	if err != nil {
		t.Fatalf("Subagents: %v", err)
	}
	if !fromFiles.Known {
		t.Fatal("the count is unknown for a session whose subagents directory exists")
	}
	if fromFiles.Count != 23 {
		t.Errorf("counted %d subagents from the metadata files, want 23", fromFiles.Count)
	}

	fromTranscript := agentToolCallsIn(t, loc.Path)
	if fromTranscript != 0 {
		t.Errorf("the transcript names %d %s tool calls; this session is the measured case that "+
			"parses as ZERO, and the test data no longer reproduces it", fromTranscript, ToolNameAgent)
	}
	t.Logf("pid 98299 / session %s: transcript parse = %d, metadata files = %d",
		subagentsMany, fromTranscript, fromFiles.Count)

	if fromFiles.Count == fromTranscript {
		t.Fatal("both methods agree, so this test cannot tell them apart")
	}
}

// agentToolCallsIn counts subagent spawns the way the REJECTED implementation
// would: from the transcript. It is here so the 0-against-23 gap is measured
// in the test rather than asserted from a document.
func agentToolCallsIn(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		msg, _ := entry[KeyMessage].(map[string]any)
		for _, block := range ContentBlocks(msg) {
			if bt, _ := block[KeyRecordType].(string); bt != BlockTypeToolUse {
				continue
			}
			if name, _ := block[KeyToolName].(string); name == ToolNameAgent {
				n++
			}
		}
	}
	return n
}

// A determined zero is not an unknown, and the line between them is whether we
// were in a position to LOOK — not whether the directory was there.
//
// Both of the sessions below are counted, and both count zero. The empty
// directory is the obvious one. The MISSING directory is the one that was read
// the other way until it was measured: Claude Code creates the directory lazily
// on the first spawn, so 94% of live sessions have none, and reporting all of
// them as undetermined meant the count could never be zero for almost anybody.
//
// The undetermined half is a separate test below, because it is the half that
// must not regress.
func TestSubagentCountZeroIsNotUnknown(t *testing.T) {
	tr := testTranscripts(t)

	empty, err := tr.Subagents(mustLocate(t, tr, subagentsZero, ""))
	if err != nil {
		t.Fatalf("Subagents: %v", err)
	}
	if !empty.Known || empty.Count != 0 {
		t.Errorf("an EMPTY subagents directory reported count=%d known=%v, want 0 and true",
			empty.Count, empty.Known)
	}

	absent := mustLocate(t, tr, subagentsNone, "")
	if _, err := os.Stat(filepath.Join(absent.Dir, subagentsNone, SubagentsDirName)); !os.IsNotExist(err) {
		t.Fatalf("the test data grew a subagents directory for %s; this case needs it MISSING",
			subagentsNone)
	}
	missing, err := tr.Subagents(absent)
	if err != nil {
		t.Fatalf("Subagents: %v", err)
	}
	if !missing.Known {
		t.Error("a MISSING subagents directory reported undetermined; we looked where Claude " +
			"Code would have created it, and that is counting")
	}
	if missing.Count != 0 {
		t.Errorf("a missing subagents directory counted %d", missing.Count)
	}
}

// The undetermined half, and the only two situations that produce it: we could
// not look at all.
//
// This is the assertion that keeps nil meaning something. If "no transcript"
// ever starts answering zero, the layer is telling a user that a session it
// never found is healthy.
func TestSubagentCountIsUndeterminedOnlyWhenWeCouldNotLook(t *testing.T) {
	tr := testTranscripts(t)

	t.Run("no transcript was located", func(t *testing.T) {
		loc, err := tr.Locate(noTranscriptSession, "")
		if err != nil {
			t.Fatalf("Locate: %v", err)
		}
		if loc.Found() {
			t.Fatalf("%s is supposed to have no transcript anywhere", noTranscriptSession)
		}
		sub, err := tr.Subagents(loc)
		if err != nil {
			t.Fatalf("Subagents: %v", err)
		}
		if sub.Known {
			t.Errorf("a session whose transcript was never found reported a determined count "+
				"of %d; there was no session directory to look in", sub.Count)
		}
	})

	t.Run("the directory exists and cannot be read", func(t *testing.T) {
		// A permission error is not an observation. Built in a temp tree: the
		// test data is read-only to this suite and nothing here may chmod it.
		root := t.TempDir()
		const id = "55555555-6666-4777-8888-999999999999"
		dir := filepath.Join(root, "-tmp")
		subs := filepath.Join(dir, id, SubagentsDirName)
		if err := os.MkdirAll(subs, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+TranscriptSuffix), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(subs, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(subs, 0o755) })

		if _, err := os.ReadDir(subs); err == nil {
			t.Skip("this user can read a 0000 directory — running as root, most likely — so " +
				"the unreadable case cannot be staged here")
		}

		tmp := Transcripts{Root: root}
		sub, err := tmp.Subagents(mustLocate(t, tmp, id, ""))
		if err == nil {
			t.Error("an unreadable directory returned no error; the caller cannot tell it from " +
				"a directory that was read")
		}
		if sub.Known {
			t.Errorf("an unreadable directory reported a determined count of %d", sub.Count)
		}
	})
}

// The suffix filter, both ways: a notes file beside three metadata files, and
// a placeholder alone in an otherwise empty directory. A counter that counts
// directory entries reports 4 and 1 instead of 3 and 0.
func TestSubagentCountIgnoresNonMetaFiles(t *testing.T) {
	tr := testTranscripts(t)

	withNotes := mustLocate(t, tr, trapSession, "")
	entries, err := os.ReadDir(filepath.Join(withNotes.Dir, trapSession, SubagentsDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("the directory holds %d entries; the test needs the non-metadata one to be there",
			len(entries))
	}
	sub, err := tr.Subagents(withNotes)
	if err != nil {
		t.Fatalf("Subagents: %v", err)
	}
	if sub.Count != 3 {
		t.Errorf("counted %d of 4 entries, want 3 — only %s files are subagents",
			sub.Count, SubagentMetaSuffix)
	}

	placeholder, err := tr.Subagents(mustLocate(t, tr, subagentsZero, ""))
	if err != nil {
		t.Fatalf("Subagents: %v", err)
	}
	if placeholder.Count != 0 {
		t.Errorf("counted %d in a directory holding only a placeholder, want 0", placeholder.Count)
	}
}

// A malformed metadata file is COUNTED, and the fact that it was malformed is
// reported.
//
// api-surface.md raises this as ambiguity A3 and the two readings give 3 and
// 2. It is settled here as 3, and the reasoning is in the doc comment on
// [Subagents]: a file caught half-written describes a subagent that is running
// right now, and the other reading makes a live subagent vanish from the count
// at the moment it starts. The test asserts the settled answer AND the
// reporting, so a future change of mind is a visible change rather than a
// silent one.
func TestMalformedSubagentMetaIsCountedAndReported(t *testing.T) {
	tr := testTranscripts(t)
	sub, err := tr.Subagents(mustLocate(t, tr, trapSession, ""))
	if err != nil {
		t.Fatalf("Subagents: %v", err)
	}
	if sub.Count != 3 {
		t.Errorf("Count = %d, want 3 (counted by file; see the doc comment)", sub.Count)
	}
	if sub.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1 — the truncated file must be visible, not silently "+
			"folded into the count", sub.Malformed)
	}
}

// The depth is under the SESSION ID, not directly under the project slug. One
// level too high finds nothing and reports a confident zero, so the test data
// is checked for the flatter layout: if it ever grows one, this test says so
// rather than letting a wrong implementation pass.
func TestSubagentsLiveUnderTheSessionIDAndNotTheProjectSlug(t *testing.T) {
	tr := testTranscripts(t)
	loc := mustLocate(t, tr, subagentsMany, "")

	deep := filepath.Join(loc.Dir, subagentsMany, SubagentsDirName)
	if _, err := os.Stat(deep); err != nil {
		t.Fatalf("the four-level layout is not in the test data: %v", err)
	}
	shallow := filepath.Join(loc.Dir, SubagentsDirName)
	if _, err := os.Stat(shallow); err == nil {
		t.Fatalf("%s exists, so an implementation looking one level too high would pass", shallow)
	}
}

func derefColour(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
