package claudecode

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/registry"
)

// Nothing here reads ~/.claude, probes a live pid or speaks to tmux. The
// registry records and the transcript tree both come from
// internal/obs/testdata; the liveness answers come from LIVE-PIDS.txt, because
// a pid in that data could collide with a real process on whatever machine
// runs the suite. The tests that need to MUTATE a transcript build one in
// t.TempDir().
const (
	testdataProjects = "../testdata/projects"
	testdataSessions = "../testdata/sessions"
)

// The sessions this file names, and what each one is for. See
// testdata/PROVENANCE.md for the whole set.
const (
	// trap is the wrong-directory case: its record's working directory is a
	// worktree whose encoded name EXISTS and holds another session's
	// transcript. 3 API errors, 3 subagents, colour orange.
	trapSession = "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b"
	trapPID     = 98693
	trapLiveCWD = "/Users/user/dev/magmux/.claude/worktrees/claude-session-control"
	trapLaunch  = "/Users/user/dev/magmux"

	// many is the 0-against-23 case: a transcript that parses as zero
	// subagents beside a directory holding twenty-three.
	manySession = "5f1c9a02-7b4e-4c31-9d88-2a6f0e11b7c3"
	manyPID     = 98299

	// zero has an empty subagents directory: a DETERMINED zero.
	zeroSession = "c874de20-33c1-480a-ba90-8ae3f2f332c4"
	zeroPID     = 98198

	// none has a transcript but no subagents directory at all, and no colour
	// record. The colour is undetermined; the subagent count is a determined
	// ZERO, because the directory is created on the first spawn and we looked
	// where it would be. See adapters.Transcripts.Subagents.
	noneSession = "3b90f6cc-1d47-4e20-8a3b-9f5e6c0d2a18"
	nonePID     = 15775

	// missing has no transcript anywhere, deliberately. Every harness-derived
	// field must stay undetermined for it — it is the ONLY shape that makes the
	// subagent count undetermined, and the one that must not regress.
	missingSession = "6c5b2f09-3a81-4d76-99e4-0f8c1b3a7d52"
	missingPID     = 1828

	// poison is the colour the decoy transcript carries, so that reading the
	// wrong session is loud rather than subtly wrong.
	poison = "WRONG-SESSION-DO-NOT-REPORT-THIS"
)

// launchDirMap is a LaunchDirs that answers from a table.
//
// It has no mutable state, which is the whole requirement: ONE of these serves
// every session's middleware and each middleware runs on its own actor's
// goroutine, so an implementation that memoised or counted would have to be
// synchronised. -race found exactly that in phase 3's pane locator.
type launchDirMap map[int]string

func (m launchDirMap) LaunchDir(pid int) (string, bool) {
	dir, ok := m[pid]
	return dir, ok
}

func newMiddleware(t *testing.T, cfg Config, sessionID string) pipeline.Middleware {
	t.Helper()
	pinTheDirNameOverride(t)
	if cfg.Root == "" {
		cfg.Root = testdataProjects
	}
	mw := Middleware(cfg)(sessionID)
	if mw == nil {
		t.Fatal("the factory returned no middleware")
	}
	return mw
}

func observe(t *testing.T, mw pipeline.Middleware, s obs.Session) obs.Session {
	t.Helper()
	return mw.Observe(s)
}

func session(id string, pid int) obs.Session {
	return obs.Session{ID: id, PID: pid}
}

// pinTheDirNameOverride clears Claude Code's own project-directory override for
// the duration of a test. Transcript resolution honours it, so a developer with
// CLAUDE_CONFIG_DIR and CLAUDE_CODE_PROJECT_DIR_NAME set in their shell would
// otherwise get a different answer from the same test data.
func pinTheDirNameOverride(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "")
}

// countOf and colourOf render an undetermined value as <nil> rather than as a
// pointer address. A failure message reading "want 3, got 0x14000112038" is a
// failure message nobody can act on.
func countOf(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

func colourOf(c *string) any {
	if c == nil {
		return nil
	}
	return *c
}

// ---------------------------------------------------------------------------
// What the stage adds.
// ---------------------------------------------------------------------------

// The harness-derived facts, one session at a time, with the undetermined
// cases beside the determined ones so that the two cannot be told apart by
// which test they are in.
func TestHarnessFactsReachTheFactSet(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		pid       int
		colour    any // nil for "no record"
		apiErrors any // nil for "nobody counted"
		subagents any // nil for "nobody could look"
	}{
		{"the wrong-directory session", trapSession, trapPID, "orange", 3, 3},
		{"0 against 23", manySession, manyPID, "teal", 0, 23},
		{"an empty subagents directory is a determined zero", zeroSession, zeroPID, "cyan", 0, 0},
		{"no colour record, and no subagents directory to find", noneSession, nonePID, nil, 0, 0},
		{"no transcript at all", missingSession, missingPID, nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mw := newMiddleware(t, Config{}, c.id)
			got := observe(t, mw, session(c.id, c.pid))

			if want, ok := c.colour.(string); ok {
				if got.Color == nil {
					t.Errorf("Color = nil, want %q", want)
				} else if *got.Color != want {
					t.Errorf("Color = %q, want %q", *got.Color, want)
				}
			} else if got.Color != nil {
				t.Errorf("Color = %q, want nil — no record is not a value", *got.Color)
			}

			if want, ok := c.apiErrors.(int); ok {
				if got.Fault.APIErrors == nil {
					t.Errorf("Fault.APIErrors = nil, want %d", want)
				} else if *got.Fault.APIErrors != want {
					t.Errorf("Fault.APIErrors = %d, want %d", *got.Fault.APIErrors, want)
				}
			} else if got.Fault.APIErrors != nil {
				t.Errorf("Fault.APIErrors = %d, want nil — a session nobody could examine has "+
					"no error count, and reporting zero says it is healthy",
					*got.Fault.APIErrors)
			}

			if want, ok := c.subagents.(int); ok {
				if got.SubagentCount == nil {
					t.Errorf("SubagentCount = nil, want %d", want)
				} else if *got.SubagentCount != want {
					t.Errorf("SubagentCount = %d, want %d", *got.SubagentCount, want)
				}
			} else if got.SubagentCount != nil {
				t.Errorf("SubagentCount = %d, want nil", *got.SubagentCount)
			}
		})
	}
}

// A fault is orthogonal to a status, and this stage writes only one of them.
//
// A session that errored an hour ago and is now waiting for the user is
// WAITING, with a fault. Folding the two together — reporting it as "error",
// or clearing the fault because the status moved on — throws away the half the
// user acts on.
func TestAFaultNeverOverwritesAStatus(t *testing.T) {
	since := time.Date(2026, 9, 16, 11, 0, 0, 0, time.UTC)
	in := obs.Session{
		ID:          trapSession,
		PID:         trapPID,
		Status:      obs.StatusWaiting,
		StatusRaw:   "waiting",
		Waiting:     obs.WaitingReasonInput,
		StatusSince: &since,
	}
	mw := newMiddleware(t, Config{}, trapSession)
	got := observe(t, mw, in)

	if got.Status != obs.StatusWaiting {
		t.Errorf("Status = %v, want it untouched at %v", got.Status, obs.StatusWaiting)
	}
	if got.Waiting != obs.WaitingReasonInput {
		t.Errorf("Waiting = %v, want it untouched", got.Waiting)
	}
	if got.StatusSince == nil || !got.StatusSince.Equal(since) {
		t.Errorf("StatusSince = %v, want it untouched at %v", got.StatusSince, since)
	}
	if got.Fault.APIErrors == nil || *got.Fault.APIErrors != 3 {
		t.Fatalf("Fault.APIErrors = %v, want 3 — the session is waiting AND has faults",
			countOf(got.Fault.APIErrors))
	}
	if got.Fault.Detail == "" {
		t.Error("Fault.Detail is empty; a count with no words is a number a user cannot act on")
	}
}

// The stage leaves everything it does not own alone, byte for byte.
func TestTheStageTouchesNothingItDoesNotOwn(t *testing.T) {
	in := obs.Session{
		ID:            manySession,
		PID:           manyPID,
		Name:          "sampleapp-catalog-v3",
		PreviousNames: []string{"older-name"},
		Status:        obs.StatusBusy,
		StatusRaw:     "busy",
		ProcessAlive:  obs.LivenessYes,
		PaneAlive:     obs.LivenessYes,
		PaneID:        obs.Ptr(4),
		Attention:     obs.Attention{Needed: obs.Ptr(true), Reason: "set upstream"},
	}
	got := observe(t, newMiddleware(t, Config{}, manySession), in)

	if got.Name != in.Name || got.Status != in.Status || got.StatusRaw != in.StatusRaw {
		t.Error("an identity or status field was rewritten")
	}
	if got.ProcessAlive != in.ProcessAlive || got.PaneAlive != in.PaneAlive {
		t.Error("a liveness gate was rewritten")
	}
	if got.PaneID == nil || *got.PaneID != 4 {
		t.Error("the pane gate's answer was overwritten")
	}
	if got.Attention.Needed == nil || !*got.Attention.Needed || got.Attention.Reason != "set upstream" {
		t.Error("an attention decision made upstream was overwritten")
	}
}

// ---------------------------------------------------------------------------
// One instance per session.
// ---------------------------------------------------------------------------

// The per-session rule is not an optimisation, and this test says what it
// costs to break it.
//
// The first half is the contract: the factory builds a fresh instance per
// session and each answers its own. The second half is the reason the contract
// exists — an instance that HAS answered once remembers the file it found, so
// handing it a second session answers with the first session's memory, with
// nothing anywhere to say so. That is a silent wrong answer in exactly the
// cases this feature exists for, which is why there is no API through which
// two sessions can be given one instance.
func TestOneInstancePerSessionIsNotAnOptimisation(t *testing.T) {
	factory := Middleware(Config{Root: testdataProjects})

	t.Run("a fresh instance per session answers each correctly", func(t *testing.T) {
		a := factory(trapSession).Observe(session(trapSession, trapPID))
		b := factory(manySession).Observe(session(manySession, manyPID))

		if a.Color == nil || *a.Color != "orange" {
			t.Errorf("first session's colour = %v, want orange", colourOf(a.Color))
		}
		if b.Color == nil || *b.Color != "teal" {
			t.Errorf("second session's colour = %v, want teal", colourOf(b.Color))
		}
		if a.SubagentCount == nil || *a.SubagentCount != 3 {
			t.Errorf("first session's subagents = %v, want 3", countOf(a.SubagentCount))
		}
		if b.SubagentCount == nil || *b.SubagentCount != 23 {
			t.Errorf("second session's subagents = %v, want 23", countOf(b.SubagentCount))
		}
	})

	t.Run("a shared instance would answer with the first session's memory", func(t *testing.T) {
		shared := factory(trapSession)
		first := shared.Observe(session(trapSession, trapPID))
		second := shared.Observe(session(manySession, manyPID))

		if second.Color == nil || first.Color == nil {
			t.Fatal("expected both observations to produce a colour")
		}
		if *second.Color != *first.Color {
			t.Skipf("the instance no longer remembers across sessions (%q then %q); if that is "+
				"deliberate, this test's reason has moved", *first.Color, *second.Color)
		}
		if *second.Color == "teal" {
			t.Fatal("the shared instance answered the second session correctly, so the " +
				"per-session rule is no longer load-bearing and its documentation is wrong")
		}
	})
}

// ---------------------------------------------------------------------------
// The wrong-directory trap, at the level a user would hit it.
// ---------------------------------------------------------------------------

// A launch-directory source that answers with the LIVE working directory — the
// mistake the whole of §3.1 is about — must not produce another session's
// facts.
//
// The encoded name of that directory EXISTS in the test data and holds a
// different session's transcript, whose colour is a shout. If the resolver
// ever starts choosing a file by anything other than the session id, this
// fails loudly instead of reporting somebody else's colour, error count and
// subagents as this session's.
func TestTheWrongLaunchDirectoryCannotProduceAnotherSessionsFacts(t *testing.T) {
	for _, c := range []struct {
		name string
		dirs LaunchDirs
	}{
		{"no source at all", nil},
		{"the live working directory, which is the wrong input", launchDirMap{trapPID: trapLiveCWD}},
		{"the real launch directory", launchDirMap{trapPID: trapLaunch}},
		{"a directory that does not exist", launchDirMap{trapPID: "/nowhere/at/all"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mw := newMiddleware(t, Config{LaunchDirs: c.dirs}, trapSession)
			got := observe(t, mw, session(trapSession, trapPID))

			if got.Color == nil {
				t.Fatal("no colour at all; the transcript was not found")
			}
			if *got.Color == poison {
				t.Fatalf("reported the decoy session's colour %q", poison)
			}
			if *got.Color != "orange" {
				t.Errorf("Color = %q, want orange", *got.Color)
			}
			if got.Fault.APIErrors == nil || *got.Fault.APIErrors != 3 {
				t.Errorf("Fault.APIErrors = %v, want 3", countOf(got.Fault.APIErrors))
			}
			if got.SubagentCount == nil || *got.SubagentCount != 3 {
				t.Errorf("SubagentCount = %v, want 3", countOf(got.SubagentCount))
			}
		})
	}
}

// What the port answers is recorded on the fact set, because it is the input
// to the tool's own on-disk layout and no consumer can re-derive it.
func TestTheLaunchDirectoryIsRecordedWhenTheSourceKnowsIt(t *testing.T) {
	mw := newMiddleware(t, Config{LaunchDirs: launchDirMap{trapPID: trapLaunch}}, trapSession)
	got := observe(t, mw, session(trapSession, trapPID))
	if got.LaunchDir != trapLaunch {
		t.Errorf("LaunchDir = %q, want %q", got.LaunchDir, trapLaunch)
	}

	// And a source that does not know this session leaves the field alone
	// rather than inventing a directory.
	quiet := newMiddleware(t, Config{LaunchDirs: launchDirMap{}}, trapSession)
	if got := observe(t, quiet, session(trapSession, trapPID)); got.LaunchDir != "" {
		t.Errorf("LaunchDir = %q, want it left empty", got.LaunchDir)
	}
}

// An empty root adds nothing, and — the point — reads nothing. A default that
// silently fell back to the developer's home directory would make the rule
// that no test may touch ~/.claude unenforceable.
func TestAnEmptyRootAddsNothingRatherThanReadingTheHomeDirectory(t *testing.T) {
	pinTheDirNameOverride(t)
	mw := Middleware(Config{})(trapSession)
	got := mw.Observe(session(trapSession, trapPID))
	if got.Color != nil || got.Fault.APIErrors != nil || got.SubagentCount != nil {
		t.Error("a middleware with no root answered a question; it can only have done that by " +
			"choosing a root of its own")
	}
}

// ---------------------------------------------------------------------------
// Re-reading.
// ---------------------------------------------------------------------------

// A transcript that has changed is read again; one that has not is not.
//
// The second half is asserted by rewriting the file to the SAME size and
// restoring its modification time, which is precisely the cache key. That is
// not a realistic edit — it is the only way to observe the skip from outside —
// and it pins what the cache is keyed on rather than merely that a cache
// exists.
//
// Nothing here is an activity inference. The file's timestamp says "read it
// again", never "the session is busy": some record types are written with no
// conversational turn behind them, which is how one session looked 1.37 hours
// stale while being correctly idle.
func TestAChangedTranscriptIsReReadAndAnUnchangedOneIsNot(t *testing.T) {
	root := t.TempDir()
	const id = "44444444-5555-4666-8777-888888888888"
	dir := filepath.Join(root, "-tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+adapters.TranscriptSuffix)

	write := func(colour string) {
		t.Helper()
		line, err := json.Marshal(map[string]any{adapters.KeyAgentColor: colour})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("green")
	mw := newMiddleware(t, Config{Root: root}, id)
	if got := observe(t, mw, session(id, 1)); got.Color == nil || *got.Color != "green" {
		t.Fatalf("first reading = %v, want green", colourOf(got.Color))
	}

	// A real change: different content, different size.
	write("magenta-and-longer")
	if got := observe(t, mw, session(id, 1)); got.Color == nil || *got.Color != "magenta-and-longer" {
		t.Errorf("after a change, colour = %v, want magenta-and-longer — a cached reading is "+
			"serving a file that has moved on", colourOf(got.Color))
	}

	// The cache key, held constant on purpose: same size, same modification
	// time, different bytes.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	write("cyan-and-identical")
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) {
		t.Fatalf("the test failed to hold the cache key constant: size %d->%d, mtime %v->%v",
			st.Size(), after.Size(), st.ModTime(), after.ModTime())
	}
	if got := observe(t, mw, session(id, 1)); got.Color == nil || *got.Color != "magenta-and-longer" {
		t.Errorf("colour = %v, want the previous reading — an unchanged size and modification "+
			"time must not cost a re-read", colourOf(got.Color))
	}
}

// ---------------------------------------------------------------------------
// Inside the pipeline.
// ---------------------------------------------------------------------------

// The stage in its real position: an actor's chain, fed by the registry
// source, published through the hub.
//
// This is where the three deliverables meet the rest of the layer, and it is
// the only place the whole path is exercised. It also demonstrates the two
// traps at document level: session 98299 carries 23 subagents against a
// transcript that parses as 0, and session 98693 carries its OWN colour rather
// than the one in the directory its working directory encodes to.
func TestTheMiddlewareRunsInsideThePipeline(t *testing.T) {
	pinTheDirNameOverride(t)
	p, err := pipeline.Start(pipeline.Config{
		Root:   testdataSessions,
		Oracle: loadOracle(t),
		Now:    func() time.Time { return time.Unix(1789545200, 0).UTC() },
		Chain: []pipeline.Factory{
			Middleware(Config{
				Root: testdataProjects,
				// The port answers for one session, with the value magmux
				// would have passed to the child. Every other session is
				// resolved by scanning, which is what a session magmux did not
				// start looks like.
				LaunchDirs: launchDirMap{manyPID: "/Users/user/dev/sampleapp"},
			}),
		},
	})
	if err != nil {
		t.Fatalf("starting the pipeline: %v", err)
	}
	defer p.Stop()

	sub, err := p.Subscribe("test", 4)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if !p.Refresh() {
		t.Fatal("the pipeline refused a refresh")
	}

	var doc obs.Fleet
	select {
	case d := <-sub.C():
		doc = d.Doc
	case <-time.After(10 * time.Second):
		t.Fatal("no document arrived")
	}

	byID := map[string]obs.Session{}
	for _, s := range doc.Sessions {
		byID[s.ID] = s
		if s.Color != nil && *s.Color == poison {
			t.Fatalf("session %s (pid %d) was given the decoy session's colour", s.ID, s.PID)
		}
	}

	many := byID[manySession]
	if many.SubagentCount == nil || *many.SubagentCount != 23 {
		t.Errorf("pid %d: SubagentCount = %v, want 23", manyPID, countOf(many.SubagentCount))
	}
	trap := byID[trapSession]
	if trap.Fault.APIErrors == nil || *trap.Fault.APIErrors != 3 {
		t.Errorf("pid %d: Fault.APIErrors = %v, want 3", trapPID, countOf(trap.Fault.APIErrors))
	}
	if trap.Color == nil || *trap.Color != "orange" {
		t.Errorf("pid %d: Color = %v, want orange", trapPID, colourOf(trap.Color))
	}
	if trap.Status != obs.StatusBusy {
		t.Errorf("pid %d: Status = %v, want busy — the registry's own answer, untouched",
			trapPID, trap.Status)
	}
	missing := byID[missingSession]
	if missing.PID != missingPID {
		t.Fatalf("the session with no transcript is not in the document at all")
	}
	if missing.Fault.APIErrors != nil || missing.SubagentCount != nil || missing.Color != nil {
		t.Errorf("pid %d has no transcript, and answered %v errors / %v subagents / %v colour",
			missingPID, countOf(missing.Fault.APIErrors), countOf(missing.SubagentCount),
			colourOf(missing.Color))
	}

	stats := p.Stats()
	if stats.ObservationsDropped != 0 {
		t.Errorf("%d observations were dropped", stats.ObservationsDropped)
	}
}

// loadOracle answers liveness from LIVE-PIDS.txt.
//
// A pid that is not in the file is undetermined rather than dead: the file's
// header says nothing else is live, but a test asserting "dead" must name the
// pid it means, and an oracle answering "no" for everything unlisted would
// make a typo look like a correctly detected dead process.
func loadOracle(t *testing.T) registry.ProcessOracle {
	t.Helper()
	f, err := os.Open(filepath.Join(testdataSessions, "LIVE-PIDS.txt"))
	if err != nil {
		t.Fatalf("opening the liveness oracle: %v", err)
	}
	defer f.Close()

	answers := map[int]obs.Liveness{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed oracle line %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("malformed pid in oracle line %q", line)
		}
		switch fields[1] {
		case "alive":
			answers[pid] = obs.LivenessYes
		case "dead":
			answers[pid] = obs.LivenessNo
		default:
			t.Fatalf("malformed answer in oracle line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading the liveness oracle: %v", err)
	}
	return fileOracle(answers)
}

// fileOracle is a read-only table, which is what makes it safe to share across
// every actor without a lock.
type fileOracle map[int]obs.Liveness

func (o fileOracle) Alive(pid int) obs.Liveness { return o[pid] }

// ---------------------------------------------------------------------------
// The three rejection heuristics, inverted.
// ---------------------------------------------------------------------------

// magmux's existing ClaudeCodeController is built to watch ONE pane that it
// spawned, and it rejects everything else three ways: a transcript older than
// the controller's own start, a first-scan baseline that marks everything
// already on disk as somebody else's, and a claim that stops two panes locking
// onto one file. A machine-wide observer must have none of the three, and this
// test is that absence made runnable.
//
// It is also why this is a parallel observer and not a ToolController: Poll()
// is contracted as cheap and runs at 4 Hz on magmux's render goroutine, and a
// scan of thirty transcripts does not belong there.
func TestTheThreeRejectionHeuristicsAreInverted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-one-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const (
		first  = "aaaaaaaa-1111-4111-8111-111111111111"
		second = "bbbbbbbb-2222-4222-8222-222222222222"
	)
	write := func(id, colour string) {
		t.Helper()
		line, err := json.Marshal(map[string]any{adapters.KeyAgentColor: colour})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, id+adapters.TranscriptSuffix)
		if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		// Older than anything that will observe it, by a wide margin, and
		// never touched again. The controller's first heuristic is
		// info.ModTime().Before(spawnedAt), which rejects exactly this.
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// BOTH transcripts exist before any middleware does. For the controller
	// that is the `preexisting` baseline: everything already on disk when it
	// first scanned belongs to some other session.
	write(first, "first-session")
	write(second, "second-session")

	cfg := Config{Root: root}
	a := newMiddleware(t, cfg, first)
	gotA := observe(t, a, session(first, 111))
	if gotA.Color == nil || *gotA.Color != "first-session" {
		t.Errorf("a transcript written before the observer existed was not read: colour = %v",
			colourOf(gotA.Color))
	}

	// The same project directory, a second session, observed after the first
	// has already locked onto its own file. claimSession is what would stop
	// this one.
	b := newMiddleware(t, cfg, second)
	gotB := observe(t, b, session(second, 222))
	if gotB.Color == nil || *gotB.Color != "second-session" {
		t.Errorf("a second session in the same project directory was rejected: colour = %v",
			colourOf(gotB.Color))
	}

	// And the first one still answers with its own, after the second has been
	// resolved: two observers of one directory, and no claim between them.
	if again := observe(t, a, session(first, 111)); again.Color == nil || *again.Color != "first-session" {
		t.Errorf("the first session's answer changed to %v once a second session appeared",
			colourOf(again.Color))
	}
}
