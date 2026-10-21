package watch

// Every test here builds its own directories with t.TempDir(). A watch test
// has to WRITE to be worth anything, so none of them goes near testdata or
// ~/.claude.
//
// The coalescing window runs on a FakeClock: a test decides when the window
// closes instead of sleeping through it. The bounded real waits below are
// FAILURE deadlines (fsnotify is real), and the one that is not, quietWait,
// is how long an ABSENCE is given to disprove itself.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

const (
	armWait   = 2 * time.Second
	eventWait = 2 * time.Second
	quietWait = 200 * time.Millisecond
	settle    = 100 * time.Millisecond // lets every fsnotify event of a write reach the loop
)

var base = time.Unix(1789545200, 0)

// roomy is a budget nothing in these tests approaches, with tt's ceilings.
var roomy = Budget{Descriptors: 1 << 20, Watches: 1 << 20, Files: ceilingFiles, JobDirs: ceilingJobDirs, JobTranscripts: ceilingJobTranscripts}

// rewrite writes a file in place, the registry's own discipline: open,
// truncate, write. No temporary file and no rename.
func rewrite(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s for append: %v", path, err)
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// tempDir is t.TempDir() with symlinks resolved, as a Tree's Dir always is
// (darwin's /var is a symlink to /private/var, and fsnotify names events by
// the path it was given).
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func spine(p string) model.WatchTarget {
	return model.WatchTarget{Path: p, Dir: true, Kind: model.TargetSpine}
}
func jobDir(p string) model.WatchTarget {
	return model.WatchTarget{Path: p, Dir: true, Kind: model.TargetJobDir}
}
func transcript(p string) model.WatchTarget {
	return model.WatchTarget{Path: p, Kind: model.TargetTranscript}
}
func jobTranscript(p string) model.WatchTarget {
	return model.WatchTarget{Path: p, Kind: model.TargetJobTranscript}
}

func start(t *testing.T, b Budget) (*Watcher, *ports.FakeClock) {
	t.Helper()
	clk := ports.NewFakeClock(base)
	w, err := Start(Config{Clock: clk, Budget: b})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(w.Stop)
	return w, clk
}

func nextTimer(t *testing.T, clk *ports.FakeClock) *ports.FakeTimer {
	t.Helper()
	select {
	case ft := <-clk.Timers():
		return ft
	case <-time.After(armWait):
		t.Fatalf("no window alarm within %s: the watcher saw no change", armWait)
		return nil
	}
}

func noTimerWithin(clk *ports.FakeClock, d time.Duration) error {
	select {
	case ft := <-clk.Timers():
		return fmt.Errorf("an alarm was armed for %s, expected none", ft.Delay())
	case <-time.After(d):
		return nil
	}
}

// flush waits for the window's alarm (it cannot exist until the loop has
// processed the event that armed it), lets the write's other events land, and
// fires it.
func flush(t *testing.T, clk *ports.FakeClock, at time.Time) {
	t.Helper()
	ft := nextTimer(t, clk)
	time.Sleep(settle)
	ft.Fire(at)
}

func nextEvent(t *testing.T, w *Watcher) Event {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if !ok {
			t.Fatal("Events closed")
		}
		if ev.Err != nil {
			t.Fatalf("backend error: %v", ev.Err)
		}
		return ev
	case <-time.After(eventWait):
		t.Fatal("no Event")
		return Event{}
	}
}

func noEventWithin(w *Watcher, d time.Duration) (Event, bool) {
	select {
	case ev := <-w.Events():
		return ev, true
	case <-time.After(d):
		return Event{}, false
	}
}

func mustArm(t *testing.T, w *Watcher, ts ...model.WatchTarget) ArmReport {
	t.Helper()
	rep := w.Arm(ts)
	if len(rep.Failed) != 0 {
		t.Fatalf("Arm failed: %v", rep.Failed)
	}
	return rep
}

// --- the claim the package rests on, one layer up from G11 ------------------

// WRONG: a watcher that watches the registry directory the way the kernel
// does, and so never fires for the only write discipline Claude Code uses.
func TestAnInPlaceRewriteOfARecordInTheArmedSpineIsOneEvent(t *testing.T) {
	dir := tempDir(t)
	rec := filepath.Join(dir, "4242.json")
	rewrite(t, rec, `{"status":"idle"}`)
	w, clk := start(t, roomy)
	mustArm(t, w, spine(dir))

	rewrite(t, rec, `{"status":"busy"}`)
	flush(t, clk, base.Add(DefaultWindow))
	ev := nextEvent(t, w)
	if !slices.Contains(ev.Paths, rec) {
		t.Fatalf("Event %v does not name the rewritten record %s", ev.Paths, rec)
	}
	if ev.Dropped != 0 {
		t.Errorf("Dropped = %d", ev.Dropped)
	}
	if err := noTimerWithin(clk, quietWait); err != nil {
		t.Errorf("after the flush: %v (an idle watcher must hold no timer)", err)
	}
}

// --- D8 ---------------------------------------------------------------------

func TestABurstInsideTheWindowIsOneEventWithEachPathOnce(t *testing.T) {
	dir := tempDir(t)
	a, b := filepath.Join(dir, "1.json"), filepath.Join(dir, "2.json")
	rewrite(t, a, "{}")
	rewrite(t, b, "{}")
	w, clk := start(t, roomy)
	mustArm(t, w, spine(dir))

	rewrite(t, a, `{"n":1}`)
	ft := nextTimer(t, clk)
	for i := 0; i < 4; i++ {
		rewrite(t, a, fmt.Sprintf(`{"n":%d}`, i+2))
		rewrite(t, b, fmt.Sprintf(`{"n":%d}`, i+2))
	}
	if err := noTimerWithin(clk, settle); err != nil {
		t.Fatalf("a second alarm inside one window: %v", err)
	}
	ft.Fire(base.Add(DefaultWindow))

	ev := nextEvent(t, w)
	if len(ev.Paths) != 2 || ev.Paths[0] != a || !slices.Contains(ev.Paths, b) {
		t.Fatalf("Paths = %v, want [%s %s] (each once, in arrival order)", ev.Paths, a, b)
	}
	if extra, ok := noEventWithin(w, quietWait); ok {
		t.Fatalf("a second Event for one burst: %+v", extra)
	}
}

// Two writes 13 s apart are two changes (ADR-D8): the window must not merge
// genuine state changes.
func TestTwoWritesThirteenSecondsApartAreTwoEvents(t *testing.T) {
	dir := tempDir(t)
	rec := filepath.Join(dir, "98693.json")
	rewrite(t, rec, `{"n":0}`)
	w, clk := start(t, roomy)
	mustArm(t, w, spine(dir))

	rewrite(t, rec, `{"n":1}`)
	flush(t, clk, base.Add(DefaultWindow))
	if ev := nextEvent(t, w); !slices.Contains(ev.Paths, rec) {
		t.Fatalf("first Event %v", ev.Paths)
	}
	rewrite(t, rec, `{"n":2}`)
	flush(t, clk, base.Add(13*time.Second))
	if ev := nextEvent(t, w); !slices.Contains(ev.Paths, rec) {
		t.Fatalf("second Event %v", ev.Paths)
	}
}

func TestTheWindowIsOneAlarmArmedForTheConfiguredDelay(t *testing.T) {
	for _, c := range []struct {
		window, want time.Duration
	}{{0, DefaultWindow}, {250 * time.Millisecond, 250 * time.Millisecond}} {
		dir := tempDir(t)
		rec := filepath.Join(dir, "1.json")
		rewrite(t, rec, "{}")
		clk := ports.NewFakeClock(base)
		w, err := Start(Config{Clock: clk, Budget: roomy, Window: c.window})
		if err != nil {
			t.Fatal(err)
		}
		mustArm(t, w, spine(dir))
		rewrite(t, rec, `{"n":1}`)
		ft := nextTimer(t, clk)
		if ft.Delay() != c.want {
			t.Errorf("Window %v: alarm armed for %v, want %v", c.window, ft.Delay(), c.want)
		}
		time.Sleep(settle)
		ft.Fire(base.Add(c.want))
		nextEvent(t, w)
		if got := clk.Outstanding(); got != 0 {
			t.Errorf("Outstanding = %d after the window flushed with nothing pending; want 0 (C2)", got)
		}
		w.Stop()
	}
}

// --- arm semantics ----------------------------------------------------------

// tt E5: a re-passed identical set must not close and reopen a held target,
// which would discard what the kernel had queued on it.
func TestArmIsIdempotentOverHeldTargetsAndKeepsAQueuedEvent(t *testing.T) {
	dir := tempDir(t)
	rec := filepath.Join(dir, "1.json")
	rewrite(t, rec, "{}")
	other := mkdir(t, filepath.Join(tempDir(t), "p"))
	tr := filepath.Join(other, "s.jsonl")
	rewrite(t, tr, "{}\n")
	w, clk := start(t, roomy)

	first := mustArm(t, w, spine(dir), transcript(tr))
	if first.Armed != 2 || first.Held != 0 {
		t.Fatalf("first Arm: %+v", first)
	}
	rewrite(t, rec, `{"n":1}`) // queued before the second Arm
	second := mustArm(t, w, spine(dir), transcript(tr), spine(dir))
	if second.Armed != 0 || second.Held != 2 || second.Released != 0 {
		t.Fatalf("an identical re-Arm armed %d, held %d, released %d; want 0, 2, 0", second.Armed, second.Held, second.Released)
	}
	flush(t, clk, base.Add(DefaultWindow))
	if ev := nextEvent(t, w); !slices.Contains(ev.Paths, rec) {
		t.Fatalf("the event queued before the redundant Arm was lost: %v", ev.Paths)
	}
}

// tt E1: an event that happens after the arm and before anyone reads is kept.
func TestAnEventBeforeTheFirstReadIsKept(t *testing.T) {
	dir := tempDir(t)
	rec := filepath.Join(dir, "1.json")
	rewrite(t, rec, "{}")
	w, clk := start(t, roomy)
	mustArm(t, w, spine(dir))
	rewrite(t, rec, `{"n":1}`)
	time.Sleep(quietWait) // nobody reads Events yet
	flush(t, clk, base.Add(DefaultWindow))
	if ev := nextEvent(t, w); !slices.Contains(ev.Paths, rec) {
		t.Fatalf("Paths = %v", ev.Paths)
	}
}

// tt AC10 case 3.
func TestARecordAppearingInTheArmedSpineFires(t *testing.T) {
	dir := tempDir(t)
	w, clk := start(t, roomy)
	mustArm(t, w, spine(dir))
	rec := filepath.Join(dir, "7.json")
	rewrite(t, rec, `{"status":"idle"}`)
	flush(t, clk, base.Add(DefaultWindow))
	if ev := nextEvent(t, w); !slices.Contains(ev.Paths, rec) {
		t.Fatalf("Paths = %v", ev.Paths)
	}
}

// tt AC10 case 4: a job record is published by rename, which the job
// directory's watch sees.
func TestAJobRecordPublishedByRenameFiresTheJobDirectory(t *testing.T) {
	jobs := tempDir(t)
	job := mkdir(t, filepath.Join(jobs, "abcd1234"))
	w, clk := start(t, roomy)
	mustArm(t, w, spine(jobs), jobDir(job))
	tmp := filepath.Join(job, "state.json.tmp")
	rewrite(t, tmp, `{"state":"blocked"}`)
	if err := os.Rename(tmp, filepath.Join(job, "state.json")); err != nil {
		t.Fatal(err)
	}
	flush(t, clk, base.Add(DefaultWindow))
	ev := nextEvent(t, w)
	found := false
	for _, p := range ev.Paths {
		if filepath.Dir(p) == job || p == job {
			found = true
		}
	}
	if !found {
		t.Fatalf("no path under the job directory in %v", ev.Paths)
	}
}

func TestATranscriptAppendFires(t *testing.T) {
	proj := mkdir(t, filepath.Join(tempDir(t), "projects", "-x"))
	tr := filepath.Join(proj, "s.jsonl")
	rewrite(t, tr, "{\"type\":\"user\"}\n")
	w, clk := start(t, roomy)
	mustArm(t, w, transcript(tr))
	appendTo(t, tr, "{\"type\":\"assistant\"}\n")
	flush(t, clk, base.Add(DefaultWindow))
	if ev := nextEvent(t, w); !slices.Contains(ev.Paths, tr) {
		t.Fatalf("Paths = %v", ev.Paths)
	}
}

// --- verify -----------------------------------------------------------------

// tt AC10 case 3 (rename): the path survives, the inode under it does not,
// and the watch is re-armed BEFORE the read.
func TestAReplacedFileIsReArmedByVerify(t *testing.T) {
	proj := mkdir(t, filepath.Join(tempDir(t), "-x"))
	tr := filepath.Join(proj, "s.jsonl")
	rewrite(t, tr, "{}\n")
	w, clk := start(t, roomy)
	mustArm(t, w, transcript(tr))
	before := w.held[tr].id

	tmp := tr + ".tmp"
	rewrite(t, tmp, "{}\n{}\n")
	if err := os.Rename(tmp, tr); err != nil {
		t.Fatal(err)
	}
	flushIfAny(t, w, clk, base.Add(DefaultWindow)) // the replacement itself, if the kernel reports it

	rep := w.Verify()
	if rep.Armed != 1 || len(rep.Failed) != 0 {
		t.Fatalf("Verify after a replace: %+v; want exactly one re-arm", rep)
	}
	if w.held[tr].id == before {
		t.Fatal("the held identity is still the old inode's")
	}
	appendTo(t, tr, "{}\n")
	flush(t, clk, base.Add(2*DefaultWindow))
	if ev := nextEvent(t, w); !slices.Contains(ev.Paths, tr) {
		t.Fatalf("the re-armed watch did not fire: %v", ev.Paths)
	}
	if again := w.Verify(); again.Armed != 0 || again.Held != 1 {
		t.Fatalf("a second Verify with nothing replaced: %+v", again)
	}
}

func TestAVanishedTargetIsDroppedByVerifyAndNamed(t *testing.T) {
	proj := mkdir(t, filepath.Join(tempDir(t), "-x"))
	tr := filepath.Join(proj, "s.jsonl")
	rewrite(t, tr, "{}\n")
	w, _ := start(t, roomy)
	mustArm(t, w, transcript(tr))
	if err := os.Remove(tr); err != nil {
		t.Fatal(err)
	}
	rep := w.Verify()
	if rep.Released != 1 || len(rep.Failed) != 1 || rep.Failed[0].Path != tr || rep.Failed[0].Scope != model.ScopeWatch {
		t.Fatalf("Verify of a vanished target: %+v", rep)
	}
	if _, held := w.held[tr]; held {
		t.Fatal("a vanished target is still held")
	}
}

// The budget is recounted in every Verify: on darwin a directory costs 1 + its
// CURRENT entries, so a registry that grew is counted (D15).
func TestTheCostIsRecountedOnAGrowingDirectory(t *testing.T) {
	dir := tempDir(t)
	rewrite(t, filepath.Join(dir, "1.json"), "{}")
	rewrite(t, filepath.Join(dir, "2.json"), "{}")
	w, _ := start(t, roomy)
	rep := mustArm(t, w, spine(dir))
	want := 1
	if costsEntries {
		want = 3
	}
	if rep.Cost != want {
		t.Fatalf("Cost = %d, want %d", rep.Cost, want)
	}
	rewrite(t, filepath.Join(dir, "3.json"), "{}")
	rewrite(t, filepath.Join(dir, "3.abc.key"), "k")
	if costsEntries {
		want = 5
	}
	if got := w.Verify().Cost; got != want {
		t.Fatalf("Verify Cost = %d after the directory grew, want %d", got, want)
	}
}

// --- D15: shedding ----------------------------------------------------------

// claimOf reads Descriptors on darwin and Watches on linux; every budget in
// these tests sets both, so the expectation is the same on both.
func claimBudget(n int) Budget {
	return Budget{Descriptors: n, Watches: n, Files: ceilingFiles, JobDirs: ceilingJobDirs, JobTranscripts: ceilingJobTranscripts}
}

type tree struct {
	spineDir, jobA, jobB, jtA, jtB, trA, trB string
}

// emptyTree has an empty spine and empty job dirs (each costs 1 on both OSes)
// and four transcript files in an UNWATCHED projects tree.
func emptyTree(t *testing.T) tree {
	root := tempDir(t)
	tr := tree{
		spineDir: mkdir(t, filepath.Join(root, "sessions")),
		jobA:     mkdir(t, filepath.Join(root, "jobs-a")),
		jobB:     mkdir(t, filepath.Join(root, "jobs-b")),
	}
	proj := mkdir(t, filepath.Join(root, "projects", "-x"))
	tr.jtA, tr.jtB = filepath.Join(proj, "ja.jsonl"), filepath.Join(proj, "jb.jsonl")
	tr.trA, tr.trB = filepath.Join(proj, "a.jsonl"), filepath.Join(proj, "b.jsonl")
	for _, p := range []string{tr.jtA, tr.jtB, tr.trA, tr.trB} {
		rewrite(t, p, "{}\n")
	}
	return tr
}

func (tr tree) all() []model.WatchTarget {
	return []model.WatchTarget{
		transcript(tr.trA), jobTranscript(tr.jtB), jobDir(tr.jobB), spine(tr.spineDir),
		transcript(tr.trB), jobTranscript(tr.jtA), jobDir(tr.jobA),
	}
}

func paths(ts []model.WatchTarget) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Path)
	}
	return out
}

// The shedding order is part of the decision: interactive transcripts go
// first, then job transcripts, then job directories; the spine never one by
// one. Arm admits in the reverse order, so a tight claim refuses transcripts
// before anything else.
func TestATightClaimRefusesInSheddingOrder(t *testing.T) {
	tr := emptyTree(t)
	w, _ := start(t, claimBudget(3))
	rep := w.Arm(tr.all())
	if rep.Armed != 3 || rep.Cost != 3 {
		t.Fatalf("Armed %d at Cost %d; want the spine and both job dirs (3)", rep.Armed, rep.Cost)
	}
	want := []string{tr.trB, tr.trA, tr.jtB, tr.jtA}
	if got := paths(rep.Over); !slices.Equal(got, want) {
		t.Fatalf("Over = %v\nwant   %v (transcripts, then job transcripts)", got, want)
	}
	if !slices.Contains(rep.Degraded, DegradedBudget) {
		t.Errorf("Degraded = %v, want budget", rep.Degraded)
	}
	// Pull is per cycle and retried: with room again, the refused targets arm.
	w.claim = 7
	if again := w.Arm(tr.all()); again.Armed != 4 || len(again.Over) != 0 || again.Held != 3 {
		t.Fatalf("the next cycle with room: %+v", again)
	}
}

// R2, review 03-H3: an overrun found by Verify sheds targets that stay armed
// for this cycle's read and are RELEASED in the same cycle, by the release.
func TestAVerifyOverrunIsReleasedInTheSameCycle(t *testing.T) {
	tr := emptyTree(t)
	w, clk := start(t, claimBudget(7))
	if rep := mustArm(t, w, tr.all()...); rep.Armed != 7 {
		t.Fatalf("Arm: %+v", rep)
	}
	w.claim = 4 // the budget the recount now finds (a shrunk limit, a grown registry)

	rep := w.Verify()
	want := []string{tr.trB, tr.trA, tr.jtB}
	if got := paths(rep.Shed); !slices.Equal(got, want) {
		t.Fatalf("Shed = %v\nwant   %v", got, want)
	}
	for _, s := range rep.Shed {
		if s.Kind == model.TargetSpine {
			t.Fatal("the spine was shed")
		}
		if _, held := w.held[s.Path]; !held {
			t.Fatalf("%s was shed by Verify itself; it must stay armed for this cycle's read", s.Path)
		}
	}

	// The cycle's single release keeps everything except the shed set.
	var keep []model.WatchTarget
	for _, h := range w.held {
		if !slices.Contains(paths(rep.Shed), h.t.Path) {
			keep = append(keep, h.t)
		}
	}
	rel := w.Release(keep)
	if rel.Released != 3 || rel.Held != 4 || rel.Cost != 4 {
		t.Fatalf("Release: %+v; want 3 released, 4 held at cost 4", rel)
	}

	// An ACTUAL removal: a write to a shed transcript wakes nothing.
	appendTo(t, tr.trA, "{}\n")
	if err := noTimerWithin(clk, quietWait); err != nil {
		t.Fatalf("a shed target still fires: %v", err)
	}
	// Next cycle it is offered to Arm again and comes back as Pull.
	next := w.Arm(tr.all())
	if !slices.Contains(paths(next.Over), tr.trA) {
		t.Fatalf("the shed target did not come back as Over: %+v", next)
	}
}

// darwin's half of the recount: the overrun comes from a directory that grew.
func TestAGrowingSpineShedsTranscriptsOnDarwin(t *testing.T) {
	if !costsEntries {
		t.Skip("inotify costs one watch per path whatever a directory holds")
	}
	tr := emptyTree(t)
	w, _ := start(t, claimBudget(3))
	mustArm(t, w, spine(tr.spineDir), transcript(tr.trA), transcript(tr.trB))
	rewrite(t, filepath.Join(tr.spineDir, "1.json"), "{}")
	rep := w.Verify()
	if rep.Cost != 4 || !slices.Equal(paths(rep.Shed), []string{tr.trB}) {
		t.Fatalf("Verify after the spine grew: cost %d, shed %v", rep.Cost, paths(rep.Shed))
	}
}

// If the spine alone exceeds the claim the watcher degrades to on-demand: it
// arms nothing new, says so, and the spine is in Over (D15).
func TestASpineOverTheClaimDegradesToOnDemand(t *testing.T) {
	tr := emptyTree(t)
	w, _ := start(t, claimBudget(1))
	root := spine(filepath.Dir(tr.spineDir)) // a second spine directory: the spine is now >= 2
	rep := w.Arm(append(tr.all(), root))
	if rep.Armed != 0 || rep.Cost != 0 {
		t.Fatalf("armed %d at cost %d over a spine that does not fit", rep.Armed, rep.Cost)
	}
	if !slices.Contains(rep.Degraded, DegradedBudget) {
		t.Errorf("Degraded = %v", rep.Degraded)
	}
	if len(rep.Over) != 8 || rep.Over[6].Kind != model.TargetSpine || rep.Over[7].Kind != model.TargetSpine {
		t.Fatalf("Over = %v; want every target, the two spine directories last", paths(rep.Over))
	}
}

func TestTTsCeilingsApplyWithinTheClaim(t *testing.T) {
	tr := emptyTree(t)
	w, _ := start(t, Budget{Descriptors: 1 << 20, Watches: 1 << 20, Files: 2, JobDirs: 1, JobTranscripts: 1})
	rep := w.Arm(tr.all())
	if rep.Armed != 4 {
		t.Fatalf("Armed = %d, want spine + 1 job dir + 1 job transcript + 1 transcript", rep.Armed)
	}
	for _, d := range []Degradation{DegradedJobDirCap, DegradedJobTranscriptCap, DegradedFileCap} {
		if !slices.Contains(rep.Degraded, d) {
			t.Errorf("Degraded %v lacks %v", rep.Degraded, d)
		}
	}
	if want := []string{tr.trB, tr.jtB, tr.jobB}; !slices.Equal(paths(rep.Over), want) {
		t.Fatalf("Over = %v, want %v", paths(rep.Over), want)
	}
}

// --- refusals and failures --------------------------------------------------

// C11: a projects, project or subagents directory is never Added.
func TestNoDirectoryButTheSpineAndJobDirsIsEverAdded(t *testing.T) {
	root := tempDir(t)
	projects := mkdir(t, filepath.Join(root, "projects"))
	project := mkdir(t, filepath.Join(projects, "-x"))
	sub := mkdir(t, filepath.Join(project, "id", "subagents"))
	w, _ := start(t, roomy)
	var added []string
	w.add = func(p string) error { added = append(added, p); return nil }

	rep := w.Arm([]model.WatchTarget{
		spine(projects),
		jobDir(sub),
		{Path: project, Dir: true, Kind: model.TargetTranscript},
		{Path: project, Dir: true, Kind: model.TargetJobTranscript},
		{Path: "relative/dir", Dir: true, Kind: model.TargetSpine},
		{Path: filepath.Join(root, "x"), Kind: model.TargetUnknown},
	})
	if len(added) != 0 {
		t.Fatalf("Added %v", added)
	}
	if rep.Armed != 0 || len(rep.Failed) != 5 { // project appears twice and is de-duplicated
		t.Fatalf("want 5 refusals and nothing armed: %+v", rep)
	}
	for _, p := range rep.Failed {
		if p.Scope != model.ScopeWatch || p.Kind != model.ProblemWatchArm {
			t.Errorf("refusal %v", p)
		}
	}
}

func TestADirFlagThatDisagreesWithThePathIsRefused(t *testing.T) {
	dir := tempDir(t)
	f := filepath.Join(dir, "a.jsonl")
	rewrite(t, f, "{}\n")
	w, _ := start(t, roomy)
	rep := w.Arm([]model.WatchTarget{spine(f), transcript(dir)})
	if rep.Armed != 0 || len(rep.Failed) != 2 {
		t.Fatalf("%+v", rep)
	}
}

// tt: an unarmable target is NAMED, and the rest still arm.
func TestAMissingTargetIsNamedAndTheRestStillArm(t *testing.T) {
	dir := tempDir(t)
	missing := filepath.Join(dir, "nope.jsonl")
	w, _ := start(t, roomy)
	rep := w.Arm([]model.WatchTarget{spine(dir), transcript(missing)})
	if rep.Armed != 1 || len(rep.Failed) != 1 || rep.Failed[0].Path != missing || rep.Failed[0].Detail == "" {
		t.Fatalf("%+v", rep)
	}
}

// D15: ENOSPC or EMFILE from Add is a ProblemWatchLimit that names the limit.
func TestAddLimitErrorsBecomeWatchLimitProblems(t *testing.T) {
	proj := mkdir(t, filepath.Join(tempDir(t), "-x"))
	var ts []model.WatchTarget
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(proj, n+".jsonl")
		rewrite(t, p, "{}\n")
		ts = append(ts, transcript(p))
	}
	w, _ := start(t, roomy)
	errs := map[string]error{
		ts[0].Path: fmt.Errorf("open: %w", syscall.EMFILE),
		ts[1].Path: syscall.ENOSPC,
		ts[2].Path: errors.New("permission denied"),
	}
	w.add = func(p string) error { return errs[p] }
	rep := w.Arm(ts)
	if rep.Armed != 0 || len(rep.Failed) != 3 {
		t.Fatalf("%+v", rep)
	}
	byPath := map[string]model.Problem{}
	for _, p := range rep.Failed {
		byPath[p.Path] = p
	}
	if p := byPath[ts[0].Path]; p.Kind != model.ProblemWatchLimit || !contains(p.Detail, "EMFILE") || !contains(p.Detail, "RLIMIT_NOFILE") {
		t.Errorf("EMFILE: %v", p)
	}
	if p := byPath[ts[1].Path]; p.Kind != model.ProblemWatchLimit || !contains(p.Detail, "ENOSPC") {
		t.Errorf("ENOSPC: %v", p)
	}
	if runtime.GOOS == "linux" && !contains(byPath[ts[1].Path].Detail, "/proc/sys/fs/inotify/max_user_watches") {
		t.Errorf("ENOSPC on linux does not name its /proc path: %v", byPath[ts[1].Path])
	}
	if p := byPath[ts[2].Path]; p.Kind != model.ProblemWatchArm {
		t.Errorf("another error: %v", p)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// --- release ----------------------------------------------------------------

func TestReleaseReleasesEverythingNotKeptAndArmsNothing(t *testing.T) {
	proj := mkdir(t, filepath.Join(tempDir(t), "-x"))
	a, b := filepath.Join(proj, "a.jsonl"), filepath.Join(proj, "b.jsonl")
	rewrite(t, a, "{}\n")
	rewrite(t, b, "{}\n")
	w, clk := start(t, roomy)
	mustArm(t, w, transcript(a), transcript(b))
	var added []string
	w.add = func(p string) error { added = append(added, p); return nil }

	c := filepath.Join(proj, "c.jsonl")
	rep := w.Release([]model.WatchTarget{transcript(a), transcript(c)})
	if rep.Released != 1 || rep.Held != 1 || len(added) != 0 {
		t.Fatalf("Release: %+v, added %v", rep, added)
	}
	appendTo(t, b, "{}\n")
	if err := noTimerWithin(clk, quietWait); err != nil {
		t.Fatalf("a released target still fires: %v", err)
	}
	appendTo(t, a, "{}\n")
	flush(t, clk, base.Add(DefaultWindow))
	if ev := nextEvent(t, w); !slices.Equal(ev.Paths, []string{a}) {
		t.Fatalf("Paths = %v", ev.Paths)
	}
}

// --- lifecycle --------------------------------------------------------------

func TestStartNeedsAClock(t *testing.T) {
	if _, err := Start(Config{Budget: roomy}); err == nil {
		t.Fatal("Start without a Clock succeeded")
	}
}

func TestAZeroBudgetIsTheRuntimeBudget(t *testing.T) {
	want, err := RuntimeBudget()
	if err != nil {
		t.Skipf("no runtime budget here: %v", err)
	}
	w, err := Start(Config{Clock: ports.NewFakeClock(base)})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if w.Budget() != want {
		t.Fatalf("Budget = %+v, want %+v", w.Budget(), want)
	}
}

func TestRuntimeBudgetIsAQuarterOfTheLimit(t *testing.T) {
	b, err := RuntimeBudget()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("a runtime budget on an unsupported OS")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if b.Descriptors != clampInt(lim.Cur)/4 || b.Descriptors <= 0 {
		t.Errorf("Descriptors = %d, want a quarter of the soft limit %d", b.Descriptors, lim.Cur)
	}
	if runtime.GOOS == "linux" && b.Watches <= 0 {
		t.Errorf("Watches = %d on linux", b.Watches)
	}
	if runtime.GOOS == "darwin" && b.Watches != 0 {
		t.Errorf("Watches = %d on darwin, which has no inotify", b.Watches)
	}
	if b.Files != 1024 || b.JobDirs != 64 || b.JobTranscripts != 64 {
		t.Errorf("ceilings = %+v, want tt's 1024/64/64", b)
	}
	t.Logf("runtime budget: %+v (claim on %s: %d)", b, runtime.GOOS, claimOf(b))
}

func TestStopIsIdempotentAndTheWatcherIsThenUnavailable(t *testing.T) {
	dir := tempDir(t)
	w, err := Start(Config{Clock: ports.RealClock(), Budget: roomy})
	if err != nil {
		t.Fatal(err)
	}
	mustArm(t, w, spine(dir))
	w.Stop()
	w.Stop()
	if _, ok := <-w.Events(); ok {
		t.Fatal("Events is not closed after Stop")
	}
	if rep := w.Arm([]model.WatchTarget{spine(dir)}); !slices.Contains(rep.Degraded, DegradedUnavailable) || len(rep.Failed) != 1 {
		t.Fatalf("Arm after Stop: %+v", rep)
	}
	if rep := w.Verify(); !slices.Contains(rep.Degraded, DegradedUnavailable) {
		t.Fatalf("Verify after Stop: %+v", rep)
	}
	if rep := w.Release(nil); rep.Released != 1 || rep.Held != 0 {
		t.Fatalf("Release after Stop: %+v", rep)
	}
}

func TestStopLeavesNoGoroutineBehind(t *testing.T) {
	dir := tempDir(t)
	rewrite(t, filepath.Join(dir, "1.json"), "{}")
	cycle := func() {
		w, err := Start(Config{Clock: ports.RealClock(), Budget: roomy})
		if err != nil {
			t.Fatal(err)
		}
		mustArm(t, w, spine(dir))
		rewrite(t, filepath.Join(dir, "1.json"), `{"n":1}`)
		w.Stop()
	}
	cycle() // warm-up: the first fsnotify watcher starts process-wide machinery
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		cycle()
	}
	var after int
	for i := 0; i < 50; i++ {
		if after = runtime.NumGoroutine(); after <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d goroutines before 20 start/stop cycles, %d after", before, after)
}

// G12. Close alone leaks every descriptor fsnotify opened; draining Events
// until it closes releases them. Descriptors are counted by opening a file and
// reading the number the kernel gave it: Unix assigns the lowest free one, so
// a fresh open after the cycles lands on the same number iff nothing was left
// open in between.
func TestRepeatedStartStopLeaksNoDescriptor(t *testing.T) {
	dir := tempDir(t)
	for _, n := range []string{"1.json", "2.json", "3.json"} {
		rewrite(t, filepath.Join(dir, n), "{}")
	}
	probe := func() uintptr {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		fd := f.Fd()
		f.Close()
		return fd
	}
	cycle := func(i int) {
		w, err := Start(Config{Clock: ports.RealClock(), Budget: roomy})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		mustArm(t, w, spine(dir))
		w.Stop()
	}
	cycle(-1)
	before := probe()
	for i := 0; i < 20; i++ {
		cycle(i)
	}
	var after uintptr
	for i := 0; i < 50; i++ {
		if after = probe(); after <= before {
			t.Logf("lowest free descriptor %d before and %d after 20 cycles: delta 0", before, after)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("a fresh descriptor was %d before 20 start/arm/stop cycles and %d after: each cycle "+
		"leaves descriptors open (a kqueue plus one per entry of the 3-record directory)", before, after)
}

// closeAndDrain's bound is on the Clock (G4): a future fsnotify that never
// closes its channel costs a pause, not a hung Stop.
func TestTheDrainOnCloseIsBoundedByTheClock(t *testing.T) {
	clk := ports.NewFakeClock(base)
	events := make(chan fsnotify.Event)
	errs := make(chan error)
	closed := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		closeAndDrain(func() error { closed = true; return nil }, events, errs, clk)
	}()
	ft := nextTimer(t, clk)
	if ft.Delay() != closeDrainWait {
		t.Errorf("the drain is bounded by %v, want %v", ft.Delay(), closeDrainWait)
	}
	select {
	case <-done:
		t.Fatal("the drain returned before its channel closed or its bound fired")
	case <-time.After(quietWait):
	}
	ft.Fire(base.Add(closeDrainWait))
	select {
	case <-done:
	case <-time.After(eventWait):
		t.Fatal("the drain did not return when its bound fired")
	}
	if !closed {
		t.Error("Close was not called")
	}
	if clk.Outstanding() != 0 {
		t.Errorf("Outstanding = %d", clk.Outstanding())
	}

	// And a channel that does close ends the drain with no timer firing.
	clk2 := ports.NewFakeClock(base)
	ev2 := make(chan fsnotify.Event)
	close(ev2)
	closeAndDrain(func() error { return nil }, ev2, make(chan error), clk2)
	if clk2.Outstanding() != 0 {
		t.Errorf("a closed channel left a live timer: %d", clk2.Outstanding())
	}
}

// A consumer that stops reading loses events and is TOLD how many; the loop
// never blocks on it.
func TestASlowConsumerLosesEventsAndIsToldHowMany(t *testing.T) {
	dir := tempDir(t)
	clk := ports.NewFakeClock(base)
	w, err := Start(Config{Clock: clk, Budget: roomy, Buffer: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	mustArm(t, w, spine(dir))

	// Six records against a buffer of one, with NOTHING reading. However the
	// kernel batches them into windows, at most one Event fits the buffer.
	for i := 0; i < 6; i++ {
		rewrite(t, filepath.Join(dir, fmt.Sprintf("%d.json", i)), "{}")
	}
	// Close every window the loop opens until it stops opening them. That this
	// terminates at all is the first half of the test: a loop blocked on a
	// full channel never processes another event, so never arms another alarm.
	windows := 0
	for {
		select {
		case ft := <-clk.Timers():
			windows++
			ft.Fire(base.Add(time.Duration(windows) * DefaultWindow))
			continue
		case <-time.After(500 * time.Millisecond):
		}
		break
	}
	if windows == 0 {
		t.Fatal("no window opened for six records")
	}
	if first := nextEvent(t, w); first.Dropped != 0 {
		t.Errorf("the first Event reports %d drops, but nothing was dropped when it was published", first.Dropped)
	}
	// The buffer has a slot again, so the next change gets through, and it is
	// the one that must carry the count of what was lost.
	rewrite(t, filepath.Join(dir, "last.json"), "{}")
	flush(t, clk, base.Add(time.Hour))
	if ev := nextEvent(t, w); ev.Dropped != windows-1 {
		t.Errorf("Dropped = %d, want %d (every window after the first, of %d)", ev.Dropped, windows-1, windows)
	}
}

// flushIfAny fires a window if one opens within quietWait and drains its
// Event. It is for a step whose event the kernels report differently (a
// replace-publish of a watched FILE), where the test's claim is about what
// happens next.
func flushIfAny(t *testing.T, w *Watcher, clk *ports.FakeClock, at time.Time) {
	t.Helper()
	select {
	case ft := <-clk.Timers():
		time.Sleep(settle)
		ft.Fire(at)
		nextEvent(t, w)
	case <-time.After(quietWait):
	}
}
