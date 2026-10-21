package mux

// The magmux side of the write path: the focus port, the one command that uses
// it, and the audit trail both of them leave behind.
//
// NOTHING here reads ~/.claude, probes a live pid, speaks to tmux, or writes
// outside t.TempDir(). The registry is staged, the liveness oracle is nil —
// which registry answers with "undetermined" rather than a probe — and the
// audit trail is pointed at a temporary file, because a default that resolved
// ${XDG_STATE_HOME:-~/.local/state} would have `go test .` appending to the
// developer's own trail.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// stageRegistry writes one session record in the registry's own on-disk shape.
// The instants are MILLISECONDS.
func stageRegistry(t *testing.T, dir string, pid int, id, name string) {
	t.Helper()
	ms := time.Now().Add(-time.Minute).UnixMilli()
	body := fmt.Sprintf(
		`{"sessionId":%q,"pid":%d,"name":%q,"cwd":"/tmp/project","status":"waiting",`+
			`"waitingFor":"input needed","statusUpdatedAt":%d,"updatedAt":%d}`,
		id, pid, name, ms, ms)
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// trailPath is a temporary audit trail. It is never the real one.
func trailPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "magmux", act.AuditFileName)
}

func readTrail(t *testing.T, path string) []act.Record {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	var out []act.Record
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r act.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a trail line is not a record: %v\n%s", err, line)
		}
		out = append(out, r)
	}
	return out
}

// ── the focus port ──────────────────────────────────────────────────────────

// TestFocusMovesFocusAndReportsWhatWasDisplaced — the port itself, against a
// real pane table.
//
// The displaced pane is the whole of the undo information the trail can carry
// for this verb: an Action addresses a SESSION and the thing to put back is a
// PANE, so what was displaced is recorded rather than turned into an undoable
// action nobody could address.
func TestFocusMovesFocusAndReportsWhatWasDisplaced(t *testing.T) {
	m := newTestMux(t, ctrlPanes(3)...)
	loc := newPaneLocator(m)

	m.treeMu.RLock()
	started := m.focused.id
	m.treeMu.RUnlock()

	previous, err := loc.Focus(2)
	if err != nil {
		t.Fatalf("Focus(2): %v", err)
	}
	if previous != started {
		t.Errorf("Focus reported pane %d displaced, want %d", previous, started)
	}
	m.treeMu.RLock()
	now := m.focused.id
	m.treeMu.RUnlock()
	if now != 2 {
		t.Fatalf("focus is on pane %d, want 2", now)
	}

	// Idempotent: focusing the focused pane succeeds and names itself, so a
	// retry after a timeout cannot be mistaken for a failure.
	again, err := loc.Focus(2)
	if err != nil {
		t.Fatalf("Focus(2) a second time: %v", err)
	}
	if again != 2 {
		t.Errorf("a repeated focus reported pane %d displaced, want 2", again)
	}
}

// TestObsFocusRefusesWhatSockFocusRefuses — two doors into the same room, and
// they must refuse the same things.
//
// The hidden case is the one that matters: a hidden pane is ALIVE and merely
// not spliced into the tree, so focusing it would send every later keystroke
// somewhere nobody can see. That is the state hidePanelLocked repairs and
// focusNext skips, and the socket verb already closed the last door into it.
// This is the second door.
func TestObsFocusRefusesWhatSockFocusRefuses(t *testing.T) {
	m := newTestMux(t, ctrlPanes(3)...)
	loc := newPaneLocator(m)

	t.Run("a hidden pane", func(t *testing.T) {
		panel := m.installHiddenPanel()
		m.treeMu.RLock()
		id := panel.id
		m.treeMu.RUnlock()

		if _, err := loc.Focus(id); err == nil {
			t.Fatal("a hidden pane was focused")
		} else if !strings.Contains(err.Error(), "not on screen") {
			t.Errorf("the refusal does not say why: %v", err)
		}
		// And the socket verb agrees, which is the point of asserting both.
		if _, err := m.sockFocus(sockMsg{Type: "focus", Pane: float64(id)}); err == nil {
			t.Fatal("sockFocus focused a hidden pane, so the two doors disagree")
		}
	})

	t.Run("a closed pane", func(t *testing.T) {
		if err := m.ClosePane(2, false); err != nil {
			t.Fatalf("ClosePane: %v", err)
		}
		if _, err := loc.Focus(2); err == nil {
			t.Fatal("a tombstoned pane was focused")
		}
	})

	t.Run("ids outside the table", func(t *testing.T) {
		if _, err := loc.Focus(99); err == nil {
			t.Error("an id past the end of the table was focused")
		}
		if _, err := loc.Focus(-1); err == nil {
			t.Error("a negative id was treated as an address")
		}
	})

	// Focus did not move through any of that.
	m.treeMu.RLock()
	defer m.treeMu.RUnlock()
	if m.focused == nil || m.focused.hidden || m.focused.closed {
		t.Fatalf("focus was left on an unusable pane: %+v", m.focused)
	}
}

// ── the whole path, inside magmux ───────────────────────────────────────────

// TestNavigateInsideMagmuxMovesFocusAndIsAudited is the end-to-end proof:
// a staged registry, the real pipeline, the real gate, the real audit writer
// and magmux's own pane table.
//
// It is the one place the whole of R5.1 and R5.A are exercised together —
// everything else tests a half — and it is what says the layer can actually
// act rather than merely refuse.
func TestNavigateInsideMagmuxMovesFocusAndIsAudited(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")
	trail := trailPath(t)

	m := newTestMux(t, ctrlPanes(3)...)
	// Pane 1 is the session's: the record's pid IS the pane's process, so the
	// ancestry walk finds it on its first hop and no syscall is made. Nothing
	// ever signals this pid.
	givePID(t, m.paneByID(1), 4242)

	m.panelPrivate = true // ADR-X5: observation only runs on a magmux whose panel is on-box
	if err := m.startObservationWith(obsConfig{
		Root: dir,
		// No oracle: liveness stays undetermined, which is honest and is what
		// keeps this test off whatever process happens to be pid 4242.
		Policy:    act.Default(),
		AuditPath: trail,
	}); err != nil {
		t.Fatalf("startObservationWith: %v", err)
	}
	t.Cleanup(m.stopObservation)

	sub, err := m.obs.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	m.obs.Refresh()
	select {
	case d, ok := <-sub.C():
		if !ok {
			t.Fatal("the subscription closed instead of delivering a document")
		}
		if len(d.Doc.Sessions) != 1 {
			t.Fatalf("the document holds %d sessions", len(d.Doc.Sessions))
		}
		if d.Doc.Sessions[0].PaneID == nil || *d.Doc.Sessions[0].PaneID != 1 {
			t.Fatalf("the session was not matched to pane 1: %+v", d.Doc.Sessions[0].PaneID)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}

	// Focus starts somewhere else, so a successful navigate is observable.
	if _, err := newPaneLocator(m).Focus(0); err != nil {
		t.Fatalf("staging focus on pane 0: %v", err)
	}

	report, err := m.obsFocusSession(context.Background(), "id-a", act.RequesterCLI)
	if err != nil {
		t.Fatalf("obsFocusSession: %v", err)
	}
	if !report.Result.Done {
		t.Fatalf("the navigate reported itself not done: %+v", report.Result)
	}

	m.treeMu.RLock()
	focused := m.focused.id
	m.treeMu.RUnlock()
	if focused != 1 {
		t.Fatalf("focus is on pane %d after navigating to the session in pane 1", focused)
	}

	records := readTrail(t, trail)
	if len(records) != 2 {
		t.Fatalf("the trail holds %d records, want an attempt and an outcome", len(records))
	}
	attempt, outcome := records[0], records[1]
	if attempt.Event != act.EventAttempt || attempt.Outcome != nil {
		t.Errorf("the first record is %s with outcome %+v", attempt.Event, attempt.Outcome)
	}
	if outcome.Event != act.EventOutcome || outcome.Outcome == nil || !outcome.Outcome.Done {
		t.Errorf("the second record is %s with outcome %+v", outcome.Event, outcome.Outcome)
	}
	if outcome.Ref != attempt.Seq {
		t.Errorf("the outcome refers to seq %d and the attempt is seq %d, so the two halves "+
			"of one action cannot be correlated", outcome.Ref, attempt.Seq)
	}
	if attempt.Target.SessionID != "id-a" || attempt.Target.PID != 4242 {
		t.Errorf("the record does not say which session: %+v", attempt.Target)
	}
	if attempt.Target.PaneID == nil || *attempt.Target.PaneID != 1 {
		t.Errorf("the record does not say which pane: %+v", attempt.Target.PaneID)
	}
	if attempt.Before == nil || attempt.Before.Status != obs.StatusWaiting {
		t.Errorf("the record does not say what the session was doing: %+v", attempt.Before)
	}
	if !strings.Contains(attempt.Policy.Rule, "default posture") {
		t.Errorf("the record does not say what authorised it: %q", attempt.Policy.Rule)
	}
	if !strings.Contains(outcome.Outcome.Detail, "pane 1") {
		t.Errorf("the outcome does not say what happened: %q", outcome.Outcome.Detail)
	}

	// The trail is readable as a whole, which is what an operator does with
	// it. Logged so that the evidence for this phase is the real file.
	body, err := os.ReadFile(trail)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	t.Logf("audit trail at %s:\n%s", trail, body)
}

// TestNavigateIsRefusedWhenTheZeroPolicyIsInForce — the deliberate break's
// home. A pipeline built without a policy cannot act, even inside a magmux
// where everything else is in place.
func TestNavigateIsRefusedWhenTheZeroPolicyIsInForce(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")
	trail := trailPath(t)

	m := newTestMux(t, ctrlPanes(2)...)
	givePID(t, m.paneByID(1), 4242)
	m.panelPrivate = true // ADR-X5: observation only runs on a magmux whose panel is on-box
	if err := m.startObservationWith(obsConfig{Root: dir, AuditPath: trail}); err != nil {
		t.Fatalf("startObservationWith: %v", err)
	}
	t.Cleanup(m.stopObservation)

	sub, _ := m.obs.Subscribe("test", 0)
	m.obs.Refresh()
	select {
	case <-sub.C():
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}

	_, err := m.obsFocusSession(context.Background(), "id-a", act.RequesterCLI)
	if !errors.Is(err, act.ErrForbidden) {
		t.Fatalf("a zero policy answered a navigate with %v, want a refusal", err)
	}
	records := readTrail(t, trail)
	if len(records) != 1 || records[0].Event != act.EventRefused {
		t.Fatalf("the refusal was not recorded: %+v", records)
	}
	if !strings.Contains(records[0].Policy.Rule, "unset") {
		t.Errorf("the refusal does not say that nothing configured it: %q", records[0].Policy.Rule)
	}
}

// TestAnActionIsRefusedWhenThereIsNoTrailToRecordItIn — R5.A the other way
// round: no trail, no action. The cost is real and accepted, and it is why
// obsConfig.AuditPath being empty is a refusal rather than a silent unaudited
// success.
func TestAnActionIsRefusedWhenThereIsNoTrailToRecordItIn(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")

	m := newTestMux(t, ctrlPanes(2)...)
	givePID(t, m.paneByID(1), 4242)
	m.panelPrivate = true // ADR-X5: observation only runs on a magmux whose panel is on-box
	if err := m.startObservationWith(obsConfig{Root: dir, Policy: act.Default()}); err != nil {
		t.Fatalf("startObservationWith: %v", err)
	}
	t.Cleanup(m.stopObservation)

	sub, _ := m.obs.Subscribe("test", 0)
	m.obs.Refresh()
	select {
	case <-sub.C():
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}

	m.treeMu.RLock()
	before := m.focused.id
	m.treeMu.RUnlock()

	_, err := m.obsFocusSession(context.Background(), "id-a", act.RequesterCLI)
	if !errors.Is(err, act.ErrUnaudited) {
		t.Fatalf("an unauditable action was answered with %v", err)
	}
	m.treeMu.RLock()
	after := m.focused.id
	m.treeMu.RUnlock()
	if after != before {
		t.Fatalf("focus moved from pane %d to pane %d for an action nothing recorded", before, after)
	}
}

// TestObservationRefusesAnAuditPathUnderClaude — NFR1 at the one place magmux
// opens a file for writing on this path. A refused path stops the pipeline
// rather than starting one that cannot act, which is louder.
func TestObservationRefusesAnAuditPathUnderClaude(t *testing.T) {
	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")

	m := newTestMux(t, ctrlPanes(1)...)
	m.panelPrivate = true // ADR-X5: observation only runs on a magmux whose panel is on-box
	err := m.startObservationWith(obsConfig{
		Root:      dir,
		Policy:    act.Default(),
		AuditPath: filepath.Join(t.TempDir(), ".claude", "magmux", act.AuditFileName),
	})
	if err == nil {
		m.stopObservation()
		t.Fatal("magmux started an audit trail under a .claude directory")
	}
	if !strings.Contains(err.Error(), "NFR1") {
		t.Errorf("the refusal does not name the rule: %v", err)
	}
	if m.obs != nil {
		t.Error("a refused audit path left a pipeline behind")
	}
}

// ── the command ─────────────────────────────────────────────────────────────

// TestObsFocusFromOutsideAMagmuxRefusesHonestlyAndRecordsIt.
//
// `magmux agents` is not a magmux: it has no pane table, so it can name a session
// and cannot focus one. The refusal must be "cannot" — nothing declares the
// verb — and not "will not", which would send a user looking for a config file
// that would never help. It is recorded like any other outcome, and the exit
// status is non-zero because the thing that was asked for did not happen.
func TestObsFocusFromOutsideAMagmuxRefusesHonestlyAndRecordsIt(t *testing.T) {
	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")
	trail := trailPath(t)

	out, errOut := &strings.Builder{}, &strings.Builder{}
	code := obsFocus(obsFocusConfig{
		Root:      dir,
		AuditPath: trail,
		Target:    "4242",
		// No oracle. See TestNoTestCanReachTheSignalOracle: only main() and
		// runObs name the real one.
		Policy: act.Default(),
		Out:    out,
		Err:    errOut,
	})
	if code == 0 {
		t.Fatalf("a refused focus exited 0\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(errOut.String(), "focus") {
		t.Errorf("the refusal does not name what was refused:\n%s", errOut)
	}

	records := readTrail(t, trail)
	if len(records) != 1 {
		t.Fatalf("the trail holds %d records, want one refusal", len(records))
	}
	r := records[0]
	if r.Event != act.EventRefused || r.Outcome == nil || !r.Outcome.Refused {
		t.Fatalf("the record is not a refusal: %+v", r)
	}
	if r.Target.SessionID != "id-a" || r.Target.PID != 4242 {
		t.Errorf("the record does not say which session: %+v", r.Target)
	}
	if r.By != act.RequesterCLI {
		t.Errorf("the record says the request came from %s", r.By)
	}
	if strings.TrimSpace(r.Policy.Rule) == "" {
		t.Error("the record does not say what refused it")
	}
	if !strings.Contains(out.String(), "audit trail:") {
		t.Errorf("the command did not show where the trail is:\n%s", out)
	}

	body, _ := os.ReadFile(trail)
	t.Logf("stdout:\n%s\nstderr:\n%s\naudit trail at %s:\n%s", out, errOut, trail, body)
}

// TestObsFocusWithNoTrailRefusesRatherThanActingUnrecorded — the same rule as
// the in-magmux path, at the command.
func TestObsFocusWithNoTrailRefusesRatherThanActingUnrecorded(t *testing.T) {
	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")

	out, errOut := &strings.Builder{}, &strings.Builder{}
	if code := obsFocus(obsFocusConfig{
		Root: dir, Target: "4242", Policy: act.Default(), Out: out, Err: errOut,
	}); code == 0 {
		t.Fatal("a focus with no audit trail exited 0")
	}
}

// TestObsFocusRefusesAnAuditPathUnderClaude — the command's own door, closed
// the same way.
func TestObsFocusRefusesAnAuditPathUnderClaude(t *testing.T) {
	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")

	out, errOut := &strings.Builder{}, &strings.Builder{}
	code := obsFocus(obsFocusConfig{
		Root:      dir,
		AuditPath: filepath.Join(t.TempDir(), ".claude", act.AuditFileName),
		Target:    "4242",
		Policy:    act.Default(),
		Out:       out,
		Err:       errOut,
	})
	if code == 0 {
		t.Fatal("the command wrote its trail under a .claude directory")
	}
	if !strings.Contains(errOut.String(), "NFR1") {
		t.Errorf("the refusal does not name the rule:\n%s", errOut)
	}
}

// TestObsFocusRefusesARelativeAuditPathFromInsideClaude — fix round 1, item 3,
// at the door the review named: `--audit` reaches obsFocus unmodified
// (runObsFocus passes *auditPath straight through), so `magmux agents focus
// --audit trail.jsonl` from a shell whose working directory is inside
// ~/.claude used to create the trail THERE, with exit status 0 on a successful
// refusal-or-focus and nothing on stderr about the path.
//
// The .claude directory is this test's own, under its temporary directory.
func TestObsFocusRefusesARelativeAuditPathFromInsideClaude(t *testing.T) {
	dir := t.TempDir()
	stageRegistry(t, dir, 4242, "id-a", "session-a")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	inside := filepath.Join(t.TempDir(), "home", ".claude", "projects")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(inside)

	out, errOut := &strings.Builder{}, &strings.Builder{}
	code := obsFocus(obsFocusConfig{
		Root:      dir,
		AuditPath: "trail.jsonl",
		Target:    "4242",
		Policy:    act.Default(),
		Out:       out,
		Err:       errOut,
	})
	if code == 0 {
		t.Error("the command accepted a relative audit path from inside a .claude tree")
	}
	if !strings.Contains(errOut.String(), "NFR1") {
		t.Errorf("the refusal does not name the rule:\n%s", errOut)
	}
	entries, err := os.ReadDir(inside)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the command created %s inside the .claude tree", e.Name())
	}
}

// TestResolveObsTargetNamesOneSessionOrRefuses.
//
// An ambiguous target is an ERROR listing the candidates, never a pick. Acting
// on the wrong session of two is the failure this whole layer exists to avoid,
// and a six-character prefix that matches two sessions is exactly the shape
// that produces it.
func TestResolveObsTargetNamesOneSessionOrRefuses(t *testing.T) {
	fleet := obs.Fleet{Sessions: []obs.Session{
		{ID: "abc12300-1111", PID: 100, Name: "alpha"},
		{ID: "abc12399-2222", PID: 200, Name: "beta"},
		{ID: "zz999900-3333", PID: 300, Name: "alpha"},
	}}

	for _, tc := range []struct {
		name    string
		target  string
		wantPID int
		wantErr string
	}{
		{name: "a pid", target: "200", wantPID: 200},
		{name: "an exact id", target: "abc12300-1111", wantPID: 100},
		{name: "an unambiguous prefix", target: "abc123001", wantErr: "no session matches"},
		{name: "a prefix of one", target: "zz9999", wantPID: 300},
		{name: "an ambiguous prefix", target: "abc123", wantErr: "matches 2 sessions"},
		{name: "an exact name", target: "beta", wantPID: 200},
		{name: "an ambiguous name", target: "alpha", wantErr: "matches 2 sessions"},
		{name: "nothing at all", target: "   ", wantErr: "no session named"},
		{name: "a pid nobody has", target: "999", wantErr: "no session matches"},
		{name: "a short prefix is not a prefix", target: "abc", wantErr: "no session matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveObsTarget(fleet, tc.target)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("%q resolved to pid %d, want an error", tc.target, got.PID)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("%q was refused with %v, want %q", tc.target, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.target, err)
			}
			if got.PID != tc.wantPID {
				t.Fatalf("%q resolved to pid %d, want %d", tc.target, got.PID, tc.wantPID)
			}
		})
	}
}
