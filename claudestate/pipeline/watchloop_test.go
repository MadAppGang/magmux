package pipeline

// The loop-level scenarios of tt's own watch-protocol tests, re-expressed
// against this coordinator (§8.3: watcher parity against tt is NOT VERIFIED
// by construction, and specification conformance is the substitute). Each
// cites the tt test it re-expresses. They run over a t.TempDir() with real
// fsnotify; the coalescing window is a FakeClock timer the test fires.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/watch"
)

var roomy = watch.Budget{Descriptors: 4096, Watches: 4096, Files: 1024, JobDirs: 64, JobTranscripts: 64}

// await returns the next delivery, firing every coalescing window the watcher
// arms on the way. Test goroutine only, as FakeClock requires.
func (h *harness) await() Delivery {
	h.t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case d, ok := <-h.sub.C():
			if !ok {
				h.t.Fatal("the subscription closed")
			}
			return d
		case ft := <-h.clk.Timers():
			ft.Fire(base.Add(ft.Delay()))
		case <-deadline:
			h.t.Fatal("no delivery")
		}
	}
}

// settle fires windows until none is outstanding and no delivery is pending.
func (h *harness) settle() {
	h.t.Helper()
	for {
		select {
		case <-h.sub.C():
		case ft := <-h.clk.Timers():
			ft.Fire(base.Add(ft.Delay()))
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

func watched(t *testing.T, root ports.Root, budget watch.Budget) *harness {
	t.Helper()
	return start(t, Config{Root: root, Watch: true, Budget: budget})
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// tt: watchloop_test.go's arm-before-read case and AC10 case 1 (a record
// created while the loop is idle is published by the event it raises).
func TestARecordWrittenUnderWatchIsPublishedByItsEvent(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Sessions.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	h := watched(t, root, roomy)
	if d := h.cycle(); len(d.Doc.Sessions) != 0 || sourceRow(d, model.SourceWatch).State != model.SourceOK {
		t.Fatalf("%+v", d.Doc.Sources)
	}
	write(t, filepath.Join(root.Sessions.Path, "42.json"), rec(42, "s", claude.ValueStatusIdle))
	d := h.await()
	for len(d.Doc.Sessions) == 0 {
		d = h.await()
	}
	if d.Doc.Sessions[0].Key != (model.Key{SessionID: "s", PID: 42}) {
		t.Fatalf("%+v", d.Doc.Sessions)
	}
	h.settle()
	if st := h.stats(); !st.Watching || st.WatchEvents == 0 {
		t.Fatalf("%+v", st)
	}
	if h.clk.Outstanding() != 0 {
		t.Fatalf("steady and idle, %d timers outstanding (C2)", h.clk.Outstanding())
	}
}

// tt's M12: while jobs/ does not exist the Claude root itself is armed, so
// creating jobs/ raises an event; a job directory created afterwards is armed
// by the fixed point and its record is read once it appears.
func TestCreatingTheJobsTreeIsSeenThroughTheArmedRoot(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Sessions.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	h := watched(t, root, roomy)
	d := h.cycle()
	if js := sourceRow(d, model.SourceJobs); js.State != model.SourceOK || js.Detail != "absent" {
		t.Fatalf("an absent jobs tree is OK and absent: %+v", js)
	}
	if err := os.MkdirAll(root.Jobs.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	d = h.await()
	for sourceRow(d, model.SourceJobs).Detail == "absent" {
		d = h.await()
	}
	write(t, filepath.Join(root.Jobs.Path, "j1", claude.JobStateFile), jobRec("s", "j1", claude.ValueStateWorking, "", ""))
	for len(d.Doc.Sessions) == 0 {
		d = h.await()
	}
	if s := d.Doc.Sessions[0]; s.Key != (model.Key{SessionID: "s", Job: "j1"}) || s.Status != model.StatusWorking {
		t.Fatalf("%+v", s)
	}
	h.settle()
	if h.clk.Outstanding() != 0 {
		t.Fatalf("%d timers outstanding", h.clk.Outstanding())
	}
}

// tt's job fixed point: every job directory present at the first cycle is
// armed and read in that cycle, and a watched transcript is read under its
// grant.
func TestEveryJobDirectoryIsArmedAndReadInOneCycle(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Sessions.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root.Projects.Path, "p", "s3.jsonl")
	write(t, link, transcriptLines("teal"))
	for i, d := range []string{"a", "b", "c"} {
		id := []string{"s1", "s2", "s3"}[i]
		l := ""
		if id == "s3" {
			l = link
		}
		write(t, filepath.Join(root.Jobs.Path, d, claude.JobStateFile), jobRec(id, d, claude.ValueStateWorking, "", l))
	}
	h := watched(t, root, roomy)
	d := h.cycle()
	if len(d.Doc.Sessions) != 3 {
		t.Fatalf("%d sessions", len(d.Doc.Sessions))
	}
	s, _ := byKey(d, model.Key{SessionID: "s3", Job: "c"})
	if s.Transcript == nil || s.Transcript.Color == nil || *s.Transcript.Color != "teal" {
		t.Fatalf("the job's transcript is armed and read: %+v", s.Transcript)
	}
	if hasProblem(d.Problems, model.ProblemNotGranted) || sourceRow(d, model.SourceJobs).State != model.SourceOK {
		t.Fatalf("%v %+v", d.Problems, sourceRow(d, model.SourceJobs))
	}
}

// tt's non-convergence: job directories that keep appearing during the arm
// wave leave the fixed point unconverged; the cycle says so and does not read
// what it did not arm.
func TestAJobTreeThatNeverConvergesIsReportedAndNotReadUnarmed(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Jobs.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	root.Jobs.FS = &growing{FS: root.Jobs.FS, dir: root.Jobs.Path}
	h := watched(t, root, roomy)
	d := h.cycle()
	if !hasProblem(d.Problems, model.ProblemNotConverged) || sourceRow(d, model.SourceJobs).State != model.SourceDegraded ||
		sourceRow(d, model.SourceWatch).State != model.SourceDegraded {
		t.Fatalf("%v %+v", d.Problems, d.Doc.Sources)
	}
	if !hasProblem(d.Problems, model.ProblemNotGranted) {
		t.Fatalf("the unarmed directory's record is refused, not read: %v", d.Problems)
	}
	h.settle()
}

// growing creates one more job directory, with a record, each of the first
// five times the jobs root is listed: more rounds than the fixed point has.
// Only the coordinator goroutine calls it.
type growing struct {
	fs.FS
	dir string
	n   int
}

func (g *growing) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "." && g.n < 5 {
		g.n++
		d := filepath.Join(g.dir, "g"+string(rune('a'+g.n)))
		if err := os.MkdirAll(d, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(d, claude.JobStateFile), jobRec("id"+string(rune('a'+g.n)), "", claude.ValueStateWorking, "", ""), 0o644)
		}
	}
	return fs.ReadDir(g.FS, name)
}

// D15: when the spine alone exceeds the claim the watcher degrades to
// on-demand, loudly, and every cycle is an unwatched pull: transcripts are
// still read.
func TestASpineOverTheClaimDegradesToOnDemandAndStillReads(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root.Sessions.Path, "1.json"), rec(1, "s", claude.ValueStatusIdle))
	write(t, filepath.Join(root.Sessions.Path, "2.json"), rec(2, "t", claude.ValueStatusIdle))
	write(t, filepath.Join(root.Projects.Path, "p", "s.jsonl"), transcriptLines("teal"))
	h := watched(t, root, watch.Budget{Descriptors: 1, Watches: 1, Files: 1024, JobDirs: 64, JobTranscripts: 64})
	d := h.cycle()
	if w := sourceRow(d, model.SourceWatch); w.State != model.SourceDegraded || !hasProblem(d.Problems, model.ProblemOverBudget) {
		t.Fatalf("%+v %v", w, d.Problems)
	}
	if h.stats().Watching {
		t.Fatal("Stats.Watching is false on demand")
	}
	s, _ := byKey(d, model.Key{SessionID: "s", PID: 1})
	if s.Transcript == nil || s.Transcript.Color == nil {
		t.Fatalf("an unwatched pull still reads: %+v", s.Transcript)
	}
}

// C11 at the pipeline: the projects tree is the watcher's Never list, so no
// projects directory is ever armed, whatever a stage asks.
func TestTheProjectsTreeIsNeverArmed(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root.Sessions.Path, "1.json"), rec(1, "s", claude.ValueStatusIdle))
	write(t, filepath.Join(root.Projects.Path, "p", "s.jsonl"), transcriptLines("teal"))
	h := watched(t, root, roomy)
	h.cycle()
	st := h.stats()
	// sessions/ (1 + 1 entry) and the root (1 + its 2 entries: jobs/ is
	// absent) and one transcript file. A projects directory would add more.
	if st.LastArm.Cost > 1+1+1+2+1 {
		t.Fatalf("cost %d: a directory of the projects tree was armed", st.LastArm.Cost)
	}
	_ = context.Background
}
