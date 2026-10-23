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
	"strings"
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

// While jobs/ is absent the Claude root itself is armed (M12), and only so that
// creating jobs/ fires. A write to anything else directly under the root —
// history.jsonl, settings.json — changes no record and must not cost a cycle
// that re-reads every source (review r1 #4b). Creating jobs/ still does, and so
// does a record written into sessions/.
func TestAWriteBesideTheTreesUnderTheArmedRootIsNoCycle(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Sessions.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root.Base, "history.jsonl"), []byte("{}\n"))
	h := watched(t, root, roomy)
	h.cycle()
	h.settle()
	before := h.stats()

	f, err := os.OpenFile(filepath.Join(root.Base, "history.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root.Base, "settings.json"), []byte("{}"))
	h.settle()
	after := h.stats()
	if after.WatchEvents == before.WatchEvents {
		t.Fatal("no watch event reached the coordinator: the setup is wrong, not the filter")
	}
	if after.Cycles != before.Cycles {
		t.Fatalf("%d cycles for writes beside the Claude trees, want 0", after.Cycles-before.Cycles)
	}

	// The positive controls: the reason Base is armed, and the registry.
	if err := os.MkdirAll(root.Jobs.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	d := h.await()
	for sourceRow(d, model.SourceJobs).Detail == "absent" {
		d = h.await()
	}
	write(t, filepath.Join(root.Sessions.Path, "7.json"), rec(7, "s", claude.ValueStatusIdle))
	for len(d.Doc.Sessions) == 0 {
		d = h.await()
	}
}

// What one watch event makes due, in isolation (review r1 #1 and #4b): a loss
// is due whatever paths ride with it, a .key sibling is not, a direct child of
// the armed Claude root is due only when it is the sessions or jobs directory,
// and anything deeper always is.
func TestWhatOneWatchEventMakesDue(t *testing.T) {
	root, err := ports.RootAt("/claude")
	if err != nil {
		t.Fatal(err)
	}
	c := &coordinator{cfg: Config{Root: root}}
	key := filepath.Join(root.Sessions.Dir, "1"+claude.KeyFileSuffix)
	for _, tc := range []struct {
		name string
		ev   watch.Event
		due  bool
	}{
		{"a .key sibling", watch.Event{Paths: []string{key}}, false},
		{"a loss riding on a .key sibling", watch.Event{Paths: []string{key}, Dropped: 2}, true},
		{"a bare loss", watch.Event{Dropped: 1}, true},
		{"a record", watch.Event{Paths: []string{filepath.Join(root.Sessions.Dir, "1.json")}}, true},
		{"history.jsonl beside the trees", watch.Event{Paths: []string{filepath.Join(root.Base, "history.jsonl")}}, false},
		{"the jobs directory appearing", watch.Event{Paths: []string{root.Jobs.Dir}}, true},
		{"the sessions directory appearing", watch.Event{Paths: []string{root.Sessions.Dir}}, true},
		{"an armed transcript under projects", watch.Event{Paths: []string{filepath.Join(root.Projects.Dir, "p", "s.jsonl")}}, true},
	} {
		if got := c.fold(tc.ev); got != tc.due {
			t.Errorf("%s: due = %v, want %v", tc.name, got, tc.due)
		}
	}
	if c.stats.WatchDropped != 3 {
		t.Errorf("WatchDropped = %d, want 3", c.stats.WatchDropped)
	}
	// Trees given separately: no Base, so nothing is filtered but .key.
	c = &coordinator{}
	if !c.fold(watch.Event{Paths: []string{"/elsewhere/history.jsonl"}}) {
		t.Error("with no Base, a path is due")
	}
}

// A watcher whose loop has ended holds nothing, so no read is ever granted
// "under a watch" again. The coordinator must notice and pull instead: every
// record, job record and transcript is read on every cycle, the stop is itself
// a cycle, and the Watch row says the watcher stopped (review r3 #1, a
// regression from b07a866, which refused every watched read for the rest of
// the run).
func TestAStoppedWatcherDegradesToAPullAndStillReads(t *testing.T) {
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root.Sessions.Path, "1.json"), rec(1, "s", claude.ValueStatusIdle))
	write(t, filepath.Join(root.Projects.Path, "p", "s.jsonl"), transcriptLines("teal"))
	link := filepath.Join(root.Projects.Path, "p", "j.jsonl")
	write(t, link, transcriptLines("ruby"))
	write(t, filepath.Join(root.Jobs.Path, "c", claude.JobStateFile), jobRec("j", "c", claude.ValueStateWorking, "", link))
	interactive, job := model.Key{SessionID: "s", PID: 1}, model.Key{SessionID: "j", Job: "c"}

	read := func(when string, d Delivery) {
		t.Helper()
		for k, colour := range map[model.Key]string{interactive: "teal", job: "ruby"} {
			s, ok := byKey(d, k)
			if !ok {
				t.Fatalf("%s: %v is not in the document: %+v", when, k, d.Doc.Sessions)
			}
			if s.Transcript == nil || s.Transcript.Color == nil || *s.Transcript.Color != colour {
				t.Fatalf("%s: %v's transcript was not read: %+v", when, k, s.Transcript)
			}
		}
	}

	h := watched(t, root, roomy)
	d := h.cycle()
	read("watched", d)
	if w := sourceRow(d, model.SourceWatch); w.State != model.SourceOK {
		t.Fatalf("watched: %+v", w)
	}
	h.settle()

	// The loop ends under the coordinator, which is not stopping.
	if err := h.p.Exec(context.Background(), func(_ context.Context, x Ops) error {
		x.(ops).c.watcher.Stop()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stopped := func(when string, d Delivery) {
		t.Helper()
		read(when, d)
		w := sourceRow(d, model.SourceWatch)
		if w.State != model.SourceDegraded || !strings.Contains(w.Detail, "watcher has stopped") {
			t.Fatalf("%s: the Watch row is %+v, want degraded and saying the watcher stopped", when, w)
		}
		said := false
		for _, p := range d.Problems {
			if p.Scope == model.ScopeWatch && strings.Contains(p.Detail, "watcher has stopped") {
				said = true
			}
		}
		if !said {
			t.Fatalf("%s: no ScopeWatch problem names the stop: %v", when, d.Problems)
		}
	}
	stopped("the cycle the stop raised", h.await())
	stopped("a later cycle", h.cycle())
	if st := h.stats(); st.Watching {
		t.Fatalf("Stats.Watching is true with the watcher stopped: %+v", st)
	}
}
