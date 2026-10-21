package mux

// The magmux side of phase 5: the pane-activity port that feeds the inferred
// tier, and the table that shows a person what the two tiers made of a session.
//
// NOTHING here reads ~/.claude or speaks to tmux. The panes are PTY-less
// control panes carrying a pid nobody signals, the registry is a temporary
// directory, and every instant is written into a pane by hand.

import (
	"bytes"
	"github.com/MadAppGang/magmux/obsview"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/verdict"
)

// stamp writes the two instants the activity port reads, exactly where magmux
// itself writes them — under p.mu, and nowhere near p.dirty.
func stampActivity(t *testing.T, p *Pane, lastOutput, lastInput time.Time) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastOutputAt, p.lastInputAt = lastOutput, lastInput
}

// ── the activity port ───────────────────────────────────────────────────────

// TestTheActivityPortReadsThePaneStamps is the seam: the two instants phase 1
// put in main.go, reaching the observation layer through a port because obs
// cannot import package main.
//
// A WRONG IMPLEMENTATION reads p.dirty, or any of the ~20 other sites that set
// it. Nineteen of those are magmux repainting its OWN chrome, so a staleness
// measured from them can never observe a five-minute silence: something
// repainted three seconds ago, always.
func TestTheActivityPortReadsThePaneStamps(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	loc := newPaneLocator(m)

	out := time.Now().Add(-9 * time.Minute)
	in := time.Now().Add(-40 * time.Minute)
	stampActivity(t, m.paneByID(1), out, in)

	gotOut, gotIn, ok := loc.Activity(1)
	if !ok {
		t.Fatal("Activity reported no such pane for a live one")
	}
	if !gotOut.Equal(out) || !gotIn.Equal(in) {
		t.Fatalf("Activity(1) = (%v, %v), want (%v, %v)", gotOut, gotIn, out, in)
	}
}

// TestAPaneThatHasDoneNothingReportsZeroInstantsAndNotAnAbsence.
//
// The zero instant is a REAL answer — a pane that has printed nothing is the
// most interesting staleness there is — so it must come back as two zeros with
// ok true, not as "there is no such pane".
func TestAPaneThatHasDoneNothingReportsZeroInstantsAndNotAnAbsence(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	out, in, ok := newPaneLocator(m).Activity(0)
	if !ok {
		t.Fatal("a pane that has produced nothing was reported as absent")
	}
	if !out.IsZero() || !in.IsZero() {
		t.Fatalf("Activity = (%v, %v), want two zero instants", out, in)
	}
}

// TestTheActivityPortRefusesAPaneThatIsNotThere.
//
// A WRONG IMPLEMENTATION answers two zero instants with ok true for a pane
// that does not exist, and the inferred tier then reports every id it is asked
// about as having been silent since the epoch.
func TestTheActivityPortRefusesAPaneThatIsNotThere(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	loc := newPaneLocator(m)
	if err := m.ClosePane(1, false); err != nil {
		t.Fatalf("ClosePane(1): %v", err)
	}

	for _, id := range []int{1, 99, -1} {
		if _, _, ok := loc.Activity(id); ok {
			t.Errorf("Activity(%d) answered for a pane that is not there", id)
		}
	}
}

// TestActivityIsSafeForEveryActorAtOnce — ONE locator serves every session's
// stage, and they all run on their own goroutines. This is the rule phase 3's
// -race run found broken the first time it was missed.
func TestActivityIsSafeForEveryActorAtOnce(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	m := newTestMux(t, ctrlPanes(3)...)
	loc := newPaneLocator(m)
	stampActivity(t, m.paneByID(0), time.Now(), time.Time{})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				loc.Activity(0)
				loc.Activity(2)
			}
		}()
	}
	// The pane table changes underneath them, which is what a locator holding
	// state of its own would not survive.
	for i := 0; i < 20; i++ {
		stampActivity(t, m.paneByID(0), time.Now(), time.Time{})
	}
	if err := m.ClosePane(2, false); err != nil {
		t.Fatalf("ClosePane(2): %v", err)
	}
	close(stop)
	wg.Wait()
}

// ── the two tiers, inside a magmux ──────────────────────────────────────────

// newSilentPaneRegistry writes one busy record for pid and returns its
// directory.
func newSilentPaneRegistry(t *testing.T, pid int, status string) string {
	t.Helper()
	dir := t.TempDir()
	body := `{"sessionId":"id-a","pid":` + strconv.Itoa(pid) + `,"name":"wedged","cwd":"/tmp/p",` +
		`"status":"` + status + `","statusUpdatedAt":1789286000000,"updatedAt":1789286000000}`
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	return dir
}

// TestInsideAMagmuxASilentPaneContradictsABusyRecord is the whole phase, end
// to end, through magmux's own ports: a record that says the session is
// working, a pane that has produced nothing for half an hour, and a document
// that publishes both.
//
// The oracle is deliberately nil, so no pid anywhere is probed and the two
// liveness gates stay undetermined — which never rejects, so the attention
// verdict here is the one the CONFLICT rule reaches.
func TestInsideAMagmuxASilentPaneContradictsABusyRecord(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	const pid = 4242
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), pid)
	stampActivity(t, m.paneByID(0), time.Now().Add(-30*time.Minute), time.Time{})

	loc := newPaneLocator(m)
	p, err := pipeline.Start(pipeline.Config{
		Root:     newSilentPaneRegistry(t, pid, "busy"),
		Locator:  loc,
		Activity: loc,
		Now:      time.Now,
	})
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	defer p.Stop()

	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()

	var s obs.Session
	select {
	case d, ok := <-sub.C():
		if !ok || len(d.Doc.Sessions) != 1 {
			t.Fatalf("the document is %+v (ok=%v)", d.Doc.Sessions, ok)
		}
		s = d.Doc.Sessions[0]
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}

	if s.PaneID == nil || *s.PaneID != 0 {
		t.Fatalf("the session was matched to pane %v, want 0", s.PaneID)
	}
	if s.Status != obs.StatusBusy || s.Confidence != obs.TierAuthoritative {
		t.Fatalf("published %v/%v, want busy/authoritative — the record must win",
			s.Status, s.Confidence)
	}
	if s.Conflict == nil {
		t.Fatal("half an hour of silence against a busy record published no conflict; " +
			"the losing tier was dropped")
	}
	if s.Conflict.Tier != obs.TierInferred || s.Conflict.Status != obs.StatusIdle {
		t.Errorf("conflict is %+v, want the inferred tier reporting idle", *s.Conflict)
	}
	if s.Attention.Needed != nil {
		t.Fatalf("attention is %v for a session whose two tiers disagree, want undetermined",
			*s.Attention.Needed)
	}
	if s.Attention.Reason == "" {
		t.Error("an undetermined verdict with no reason is indistinguishable from an unfilled field")
	}
}

// TestALivePaneNeverContradictsItsOwnRecord — the other half of the same
// wiring: output arriving is not a conflict, so a working session does not
// spend its life flagged.
func TestALivePaneNeverContradictsItsOwnRecord(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	const pid = 4243
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), pid)
	stampActivity(t, m.paneByID(0), time.Now(), time.Time{})

	loc := newPaneLocator(m)
	p, err := pipeline.Start(pipeline.Config{
		Root:     newSilentPaneRegistry(t, pid, "busy"),
		Locator:  loc,
		Activity: loc,
		Now:      time.Now,
	})
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	defer p.Stop()

	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	select {
	case d, ok := <-sub.C():
		if !ok || len(d.Doc.Sessions) != 1 {
			t.Fatalf("the document is %+v (ok=%v)", d.Doc.Sessions, ok)
		}
		if c := d.Doc.Sessions[0].Conflict; c != nil {
			t.Fatalf("a pane producing output right now contradicted its busy record: %+v", *c)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}
}

// TestStartObservationWiresTheActivityPort.
//
// A WRONG IMPLEMENTATION builds the pipeline without Activity, so magmux runs
// the same layer `magmux agents` does and the inferred tier is silently absent
// INSIDE the one process that has the evidence for it. Nothing else fails: the
// document still looks complete.
//
// The liveness answer is not asserted on, because startObservation installs
// the real signal oracle and whether pid 4242 exists is a fact about the
// machine running the suite. The CONFLICT does not depend on it.
func TestStartObservationWiresTheActivityPort(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	const pid = 4242
	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), pid)
	stampActivity(t, m.paneByID(0), time.Now().Add(-10*time.Minute), time.Time{})

	if err := m.startObservation(newSilentPaneRegistry(t, pid, "busy")); err != nil {
		t.Fatalf("startObservation: %v", err)
	}
	defer m.stopObservation()

	sub, err := m.obs.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	m.obs.Refresh()
	select {
	case d, ok := <-sub.C():
		if !ok || len(d.Doc.Sessions) != 1 {
			t.Fatalf("the document is %+v (ok=%v)", d.Doc.Sessions, ok)
		}
		if d.Doc.Sessions[0].Conflict == nil {
			t.Fatal("magmux's own pipeline published no conflict for a pane silent ten " +
				"minutes against a busy record: the activity port is not wired in")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}
}

// ── the table ───────────────────────────────────────────────────────────────

func renderTable(sessions ...obs.Session) string {
	var buf bytes.Buffer
	obsview.WriteFleetTable(&buf, obs.Fleet{Epoch: time.Unix(1789545200, 0), Sessions: sessions})
	return buf.String()
}

// TestTheTableSpellsTheThreeAttentionStates is the brief's own requirement:
// the three-state field must be legible AS three states.
//
// A WRONG IMPLEMENTATION prints a tick and a blank. Every reader of every table
// there has ever been takes a blank in a yes/no column for a no, so the one
// session nobody could reach a verdict about is filed under "fine" — which
// undoes the *bool at the last possible moment, after the whole model has
// carried the distinction all the way to the terminal.
func TestTheTableSpellsTheThreeAttentionStates(t *testing.T) {
	out := renderTable(
		obs.Session{PID: 1, Name: "needs-me", Attention: obs.Attention{Needed: obs.Ptr(true), Reason: "it is waiting for you to type something"}},
		obs.Session{PID: 2, Name: "fine", Attention: obs.Attention{Needed: obs.Ptr(false), Reason: "it is working"}},
		obs.Session{PID: 3, Name: "cannot-tell", Attention: obs.Attention{Reason: "nothing is known about this session yet"}},
	)
	t.Logf("\n%s", out)

	rows := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		for pid, name := range map[int]string{1: "needs-me", 2: "fine", 3: "cannot-tell"} {
			if strings.HasSuffix(strings.TrimSpace(line), name) {
				rows[pid] = line
			}
		}
	}
	for pid, want := range map[int]string{1: "YES", 2: "no", 3: "?"} {
		row, ok := rows[pid]
		if !ok {
			t.Fatalf("no row for pid %d in:\n%s", pid, out)
		}
		if !strings.Contains(row, want) {
			t.Errorf("the row for pid %d does not spell its verdict %q: %s", pid, want, row)
		}
	}
	// And the undetermined one is NOT the dash every other column uses, which
	// is the whole point of it being spelled differently.
	if fields := strings.Fields(rows[3]); len(fields) > 0 {
		for _, f := range fields {
			if f == "?" {
				return
			}
		}
		t.Errorf("the undetermined verdict is not a standalone %q: %s", "?", rows[3])
	}
}

// TestTheTableExplainsEveryVerdictWorthActingOn — a YES with no reason is an
// instruction to go and look with no indication of what at.
func TestTheTableExplainsEveryVerdictWorthActingOn(t *testing.T) {
	out := renderTable(
		obs.Session{PID: 1, Name: "a", Attention: obs.Attention{Needed: obs.Ptr(true), Reason: "it is holding a permission prompt open, for 3m 0s"}},
		obs.Session{PID: 2, Name: "b", Attention: obs.Attention{Needed: obs.Ptr(false), Reason: "it is working"}},
		obs.Session{PID: 3, Name: "c", Attention: obs.Attention{Reason: `the tool reported a status this build does not recognise ("compacting")`}},
	)
	t.Logf("\n%s", out)

	if !strings.Contains(out, "why:") {
		t.Fatalf("no reason block:\n%s", out)
	}
	for _, want := range []string{"holding a permission prompt open, for 3m 0s", `("compacting")`} {
		if !strings.Contains(out, want) {
			t.Errorf("the reason %q is not shown:\n%s", want, out)
		}
	}
	// A determined "no" is left out of the block on purpose: its reason is
	// already legible from the row, and thirty of them would bury the two
	// lines that matter.
	if strings.Count(out, "it is working") != 0 {
		t.Errorf("a determined-no reason was printed in the block:\n%s", out)
	}
}

// TestTheTableShowsTheLosingTier — publishing the conflict into JSON and not
// into the one place a person looks would be resolution in silence again, one
// layer further out.
func TestTheTableShowsTheLosingTier(t *testing.T) {
	out := renderTable(obs.Session{
		PID: 1, Name: "wedged", Status: obs.StatusBusy, Confidence: obs.TierAuthoritative,
		Conflict:  &obs.Conflict{Tier: obs.TierInferred, Status: obs.StatusIdle, Detail: "the pane has produced no output for 12m 0s"},
		Attention: obs.Attention{Reason: "its own record says it is working, but the pane has produced no output for 12m 0s"},
	})
	t.Logf("\n%s", out)

	for _, want := range []string{"authoritative", "conflict:", "inferred", "idle", "12m 0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table does not show %q:\n%s", want, out)
		}
	}
}

// TestAConflictIsShownEvenWhenSomethingElseDecidedTheVerdict — a conflict can
// ride along with a verdict reached by a gate or an API error, and then the
// reason line never mentions it. The block is the only place it appears at all.
func TestAConflictIsShownEvenWhenSomethingElseDecidedTheVerdict(t *testing.T) {
	out := renderTable(obs.Session{
		PID: 1, Name: "gone", Status: obs.StatusBusy, Confidence: obs.TierAuthoritative,
		ProcessAlive: obs.LivenessNo,
		Conflict:     &obs.Conflict{Tier: obs.TierInferred, Status: obs.StatusIdle, Detail: "silent for an hour"},
		Attention:    obs.Attention{Needed: obs.Ptr(false), Reason: "its process has exited"},
	})
	t.Logf("\n%s", out)

	if !strings.Contains(out, "conflict:") {
		t.Fatalf("a conflict on a session whose verdict came from a gate was not shown:\n%s", out)
	}
}

// TestATableOfSettledSessionsHasNoWhyBlock — a block that prints on every run
// trains a reader to skip the place the real line will appear.
func TestATableOfSettledSessionsHasNoWhyBlock(t *testing.T) {
	out := renderTable(
		obs.Session{PID: 1, Name: "a", Attention: obs.Attention{Needed: obs.Ptr(false), Reason: "it is working"}},
		obs.Session{PID: 2, Name: "b", Attention: obs.Attention{Needed: obs.Ptr(false), Reason: "nothing outstanding"}},
	)
	if strings.Contains(out, "why:") {
		t.Fatalf("a table with nothing to act on printed a reason block:\n%s", out)
	}
}

// TestTheTierColumnUsesTheWireWords — a second, shorter spelling invented for
// the table is one more thing a reader has to map back to --json.
func TestTheTierColumnUsesTheWireWords(t *testing.T) {
	for _, tt := range []struct {
		tier obs.Tier
		want string
	}{
		{obs.TierAuthoritative, "authoritative"},
		{obs.TierInferred, "inferred"},
	} {
		out := renderTable(obs.Session{PID: 1, Name: "a", Confidence: tt.tier})
		if !strings.Contains(out, tt.want) {
			t.Errorf("the tier column does not spell %v as %q:\n%s", tt.tier, tt.want, out)
		}
	}
	// An undetermined tier is a plain absence of an answer and takes the same
	// dash as every other undetermined column.
	out := renderTable(obs.Session{PID: 1, Name: "a"})
	if strings.Contains(out, "authoritative") || strings.Contains(out, "inferred") {
		t.Errorf("a session resolved by nobody named a tier:\n%s", out)
	}
}

// TestTheHelpDescribesTheThreeStatesAndTheTwoTiers. The table is the only
// place a person sees this layer working, and the help is the only place the
// spelling is explained.
func TestTheHelpDescribesTheThreeStatesAndTheTwoTiers(t *testing.T) {
	var buf bytes.Buffer
	printObsHelp(&buf)
	help := buf.String()
	for _, want := range []string{
		"ATTENTION", "undetermined", "authoritative", "inferred", "conflict", "?",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("the help does not mention %q", want)
		}
	}
}

// TestTheVerdictPolicyIsReachableFromMagmux — a compile-time statement that
// the two thresholds the table's words turn on are the package's, not a second
// copy someone wrote here.
func TestTheVerdictPolicyIsReachableFromMagmux(t *testing.T) {
	if verdict.QuietIdle <= 0 || verdict.QuietConflict <= verdict.QuietIdle {
		t.Fatalf("the verdict thresholds are %s and %s", verdict.QuietIdle, verdict.QuietConflict)
	}
}

// TestTheAlarmDoesNotLeakAGoroutineOrATimer counts goroutines across a whole
// start/stop cycle with the inferred tier live and an alarm armed.
//
// The alarm is the one thing phase 5 added that a shutdown could leak: one
// timer per session, on a machine that starts and finishes agents all day. It
// belongs to the actor's goroutine and is stopped on the way out, and this is
// where that stops being a claim.
func TestTheAlarmDoesNotLeakAGoroutineOrATimer(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	const pid = 4244
	before := runtime.NumGoroutine()

	m := newTestMux(t, ctrlPanes(1)...)
	givePID(t, m.paneByID(0), pid)
	// Silent, but not so silent that the ladder is exhausted: an alarm must
	// actually be armed for this to be measuring anything.
	stampActivity(t, m.paneByID(0), time.Now().Add(-2*time.Minute), time.Time{})

	loc := newPaneLocator(m)
	p, err := pipeline.Start(pipeline.Config{
		Root: newSilentPaneRegistry(t, pid, "busy"), Locator: loc, Activity: loc, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	select {
	case <-sub.C():
	case <-time.After(20 * time.Second):
		t.Fatal("no document arrived")
	}
	if st := p.Stats(); st.Actors != 1 {
		t.Fatalf("stats report %d actors, want 1", st.Actors)
	}
	during := runtime.NumGoroutine()

	p.Stop()

	after := before
	for deadline := time.Now().Add(10 * time.Second); ; {
		after = runtime.NumGoroutine()
		if after <= before || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after > before {
		t.Errorf("goroutines went from %d to %d across a whole start/stop cycle", before, after)
	}
	t.Logf("goroutines: %d before, %d with the pipeline running, %d after", before, during, after)
}
