package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
)

// Every test here builds its own registry in a temporary directory. NOTHING in
// this suite reads ~/.claude: those records belong to running agents, they are
// rewritten in place while anything reading them is reading, and a test whose
// answer depends on which agents happen to be running is not a test.

// epoch is a fixed instant, so a published document is comparable.
var epoch = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

// writeRecord writes one <pid>.json in the registry's own on-disk shape. The
// instants are MILLISECONDS; see findings/registry-timestamps-are-milliseconds.
func writeRecord(t *testing.T, dir string, pid int, id, name, status string) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%d.json", pid))
	body := fmt.Sprintf(
		`{"sessionId":%q,"pid":%d,"name":%q,"cwd":"/tmp/project","status":%q,`+
			`"statusUpdatedAt":%d,"updatedAt":%d}`,
		id, pid, name, status, epoch.Add(-time.Minute).UnixMilli(), epoch.Add(-time.Minute).UnixMilli())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func newRegistry(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// startPipeline starts one and stops it at the end of the test.
func startPipeline(t *testing.T, cfg Config) *Pipeline {
	t.Helper()
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return epoch }
	}
	p, err := Start(cfg)
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

// firstDoc subscribes, asks for a cycle and returns the document.
func firstDoc(t *testing.T, p *Pipeline) Delivery {
	t.Helper()
	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	return mustReceive(t, sub, 10*time.Second)
}

func sessionByPID(t *testing.T, f obs.Fleet, pid int) obs.Session {
	t.Helper()
	for _, s := range f.Sessions {
		if s.PID == pid {
			return s
		}
	}
	t.Fatalf("no session with pid %d in a document of %d", pid, len(f.Sessions))
	return obs.Session{}
}

// TestTheDocumentIsWhatTheChainsProduced is the end-to-end shape of the whole
// phase: a directory of records goes in, one document comes out, and the pane
// facts in it were added by a middleware running on a per-session goroutine.
func TestTheDocumentIsWhatTheChainsProduced(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "busy")
	writeRecord(t, dir, 4343, "id-b", "session-b", "idle")

	loc := staticLocator{
		pane:  map[int]int{4242: 0, 4343: 2},
		alive: map[int]obs.Liveness{0: obs.LivenessYes, 2: obs.LivenessNo},
	}
	p := startPipeline(t, Config{Root: dir, Locator: loc})

	d := firstDoc(t, p)
	if len(d.Doc.Sessions) != 2 {
		t.Fatalf("the document has %d sessions, want 2: %+v", len(d.Doc.Sessions), d.Doc.Sessions)
	}
	if !d.Doc.Epoch.Equal(epoch) {
		t.Errorf("epoch is %v, want the injected instant %v", d.Doc.Epoch, epoch)
	}

	a := sessionByPID(t, d.Doc, 4242)
	if a.PaneID == nil || *a.PaneID != 0 || a.PaneAlive != obs.LivenessYes {
		t.Errorf("session a: pane %v, alive %v — want pane 0, alive yes", a.PaneID, a.PaneAlive)
	}
	if a.Status != obs.StatusBusy {
		t.Errorf("session a: status %v — the record's own facts must survive the chain", a.Status)
	}

	b := sessionByPID(t, d.Doc, 4343)
	if b.PaneID == nil || *b.PaneID != 2 || b.PaneAlive != obs.LivenessNo {
		t.Errorf("session b: pane %v, alive %v — want pane 2, alive no", b.PaneID, b.PaneAlive)
	}

	st := p.Stats()
	if st.Actors != 2 || st.ActorsStarted != 2 {
		t.Errorf("stats report %d actors (%d started) for 2 sessions", st.Actors, st.ActorsStarted)
	}
	if st.Cycles != 1 || st.Published != 1 {
		t.Errorf("stats report %d cycles and %d publishes for one Refresh", st.Cycles, st.Published)
	}
}

// TestASessionWithNoPaneIsStillInTheDocument — discovery sees sessions magmux
// did not start, and throwing them away at the source removes a decision that
// belongs to whatever is showing them.
func TestASessionWithNoPaneIsStillInTheDocument(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-mine", "mine", "busy")
	writeRecord(t, dir, 9999, "id-theirs", "somebody-elses", "idle")

	loc := staticLocator{
		pane:  map[int]int{4242: 1},
		alive: map[int]obs.Liveness{1: obs.LivenessYes},
	}
	d := firstDoc(t, startPipeline(t, Config{Root: dir, Locator: loc}))

	theirs := sessionByPID(t, d.Doc, 9999)
	if theirs.PaneID != nil {
		t.Errorf("a session magmux did not start was given pane %d", *theirs.PaneID)
	}
	if theirs.PaneAlive != obs.LivenessUnknown {
		t.Errorf("a session with no pane has PaneAlive %v, want undetermined", theirs.PaneAlive)
	}
}

// TestAnActorAppearsWithItsSessionAndRetiresWithIt is where a goroutine leak
// would live: one per session, on a machine that starts and finishes agents
// all day.
func TestAnActorAppearsWithItsSessionAndRetiresWithIt(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "a", "busy")
	path := writeRecord(t, dir, 4343, "id-b", "b", "busy")

	p := startPipeline(t, Config{Root: dir})
	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	p.Refresh()
	mustReceive(t, sub, 10*time.Second)
	if st := p.Stats(); st.Actors != 2 {
		t.Fatalf("%d actors for 2 sessions", st.Actors)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing a record: %v", err)
	}
	p.Refresh()
	d := mustReceive(t, sub, 10*time.Second)

	if len(d.Doc.Sessions) != 1 {
		t.Fatalf("the document still has %d sessions after one record was removed", len(d.Doc.Sessions))
	}
	st := p.Stats()
	if st.Actors != 1 || st.ActorsStopped != 1 {
		t.Fatalf("stats report %d actors and %d stopped after one session vanished",
			st.Actors, st.ActorsStopped)
	}
}

// TestAChangedRecordDrivesACycleAndAnIdleRegistryDrivesNone is the no-polling
// decision, from the outside.
//
// The first half is the wiring: a record rewritten IN PLACE — the shape
// measured in the architecture round, and the one a kqueue directory watch
// does not report on its own — produces a document. The second half is the
// property a ticker would break: nothing happening produces nothing, and an
// idle registry costs a few blocked goroutines and no wake-ups at all.
func TestAChangedRecordDrivesACycleAndAnIdleRegistryDrivesNone(t *testing.T) {
	dir := newRegistry(t)
	path := writeRecord(t, dir, 4242, "id-a", "a", "busy")

	p := startPipeline(t, Config{Root: dir, Watch: true})
	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	mustReceive(t, sub, 10*time.Second)

	baseline := p.Stats().Cycles
	// Long enough that any plausible poll interval would have fired. There is
	// no interval to wait for, which is the point.
	time.Sleep(750 * time.Millisecond)
	if idle := p.Stats().Cycles; idle != baseline {
		t.Errorf("%d cycles happened while nothing changed — something is on a clock",
			idle-baseline)
	}

	// Rewritten in place, which is how these records are actually updated.
	writeRecord(t, dir, 4242, "id-a", "a", "waiting")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("reopening the record: %v", err)
	}
	f.Close()
	writeRecord(t, dir, 4242, "id-a", "a", "waiting")

	d := mustReceive(t, sub, 10*time.Second)
	got := sessionByPID(t, d.Doc, 4242)
	if got.Status != obs.StatusWaiting {
		t.Fatalf("the document after the rewrite reports %v, want waiting", got.Status)
	}
}

// TestProblemsRideWithTheDocument — a source that silently skips a record it
// could not read is indistinguishable from a machine with fewer sessions on
// it, so the problems travel to every sink alongside the document rather than
// being logged by whoever happened to build it.
func TestProblemsRideWithTheDocument(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "a", "busy")
	if err := os.WriteFile(filepath.Join(dir, "4343.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing the broken record: %v", err)
	}

	d := firstDoc(t, startPipeline(t, Config{Root: dir}))
	if len(d.Problems) == 0 {
		t.Fatal("a record that could not be read produced no problem")
	}
	if len(d.Doc.Sessions) != 1 {
		t.Fatalf("the readable session was lost: %d sessions", len(d.Doc.Sessions))
	}
}

// TestTwoRecordsClaimingOneIdentityBothSurvive — the actor table is keyed by
// session identity, so a duplicate would otherwise be answered with the first
// one's facts and silently replace a session with a copy of its namesake.
func TestTwoRecordsClaimingOneIdentityBothSurvive(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "same-id", "first", "busy")
	writeRecord(t, dir, 4343, "same-id", "second", "idle")

	p := startPipeline(t, Config{Root: dir})
	d := firstDoc(t, p)

	if len(d.Doc.Sessions) != 2 {
		t.Fatalf("%d sessions survived two records sharing an id, want 2", len(d.Doc.Sessions))
	}
	if st := p.Stats(); st.DuplicateSessions != 1 {
		t.Errorf("DuplicateSessions is %d, want 1 — the collision must be counted, not hidden",
			st.DuplicateSessions)
	}
	first := sessionByPID(t, d.Doc, 4242)
	second := sessionByPID(t, d.Doc, 4343)
	if first.Name != "first" || second.Name != "second" {
		t.Errorf("a session was replaced by its namesake: %q and %q", first.Name, second.Name)
	}
}

// TestStopIsTheShutdownBarrier — the whole start/stop cycle, counted.
//
// A leaked actor is the failure this design is most likely to have, and the
// watcher and hub goroutines are two more. Stop must return only once every
// one of them is gone and every subscription is closed, because a sink still
// being written to when the process exits is the socket layer's issue #2
// wearing different clothes.
func TestStopIsTheShutdownBarrier(t *testing.T) {
	before := runtime.NumGoroutine()

	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "a", "busy")
	writeRecord(t, dir, 4343, "id-b", "b", "idle")
	writeRecord(t, dir, 4444, "id-c", "c", "waiting")

	p, err := Start(Config{
		Root:  dir,
		Watch: true,
		Now:   func() time.Time { return epoch },
	})
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	mustReceive(t, sub, 10*time.Second)

	if running := runtime.NumGoroutine(); running <= before {
		t.Fatalf("goroutines went from %d to %d with three sessions running — "+
			"the fixture is not exercising anything", before, running)
	}
	t.Logf("goroutines: %d before, %d with 3 sessions running", before, runtime.NumGoroutine())

	p.Stop()
	p.Stop() // idempotent

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Error("a document arrived after Stop returned")
		}
	case <-time.After(5 * time.Second):
		t.Error("Stop returned without closing the subscription; a sink would block forever")
	}

	if p.Refresh() {
		t.Error("Refresh on a stopped pipeline reported success")
	}
	if st := p.Stats(); st.Cycles != 0 {
		t.Errorf("a stopped pipeline answered a stats command: %+v", st)
	}

	after := settledGoroutines(before, 5*time.Second)
	if after > before {
		t.Errorf("goroutines went from %d to %d across a whole start/stop cycle", before, after)
	}
	t.Logf("goroutines: %d before, %d after the full cycle", before, after)
}

// TestARootIsRequired — everything else about a pipeline has a default; the
// directory it reads does not, and a zero Config that started successfully
// would read whatever the process's working directory happens to be.
func TestARootIsRequired(t *testing.T) {
	if _, err := Start(Config{}); err == nil {
		t.Fatal("a pipeline with no registry root started")
	}
}

// TestAMissingDirectoryIsRefusedByTheWatchAndNotBySilence — Start reports it,
// rather than running with a watcher that will never fire.
func TestAMissingDirectoryIsRefusedByTheWatchAndNotBySilence(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	if _, err := Start(Config{Root: missing, Watch: true}); err == nil {
		t.Fatal("a watch on a directory that does not exist started successfully")
	}
	// Without a watch the same root is legal: the directory can appear later,
	// and the source reports its absence as a problem on each read rather than
	// refusing to exist.
	p, err := Start(Config{Root: missing})
	if err != nil {
		t.Fatalf("an unwatched pipeline refused a missing root: %v", err)
	}
	defer p.Stop()
	d := firstDoc(t, p)
	if len(d.Problems) == 0 {
		t.Error("a missing directory produced no problem on the document")
	}
}
