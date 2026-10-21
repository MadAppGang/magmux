package pipeline

// Arm, verify, then read: a target is read under a watch only when the LAST
// identity check before the read left it armed (review r2 #4). These drive the
// coordinator's own steps directly, against a real watcher over t.TempDir(),
// with the replacement made exactly where the race puts it — after the arm
// and before the verify — which a whole cycle cannot schedule.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/watch"
)

// bareCoordinator is a coordinator with a real watcher and no loop: the test
// goroutine is its only owner, exactly as the loop would be.
func bareCoordinator(t *testing.T, root ports.Root) *coordinator {
	t.Helper()
	clk := ports.NewFakeClock(base)
	w, err := watch.Start(watch.Config{Clock: clk, Budget: roomy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	p := &Pipeline{in: make(chan command), refresh: make(chan struct{}, 1), wake: make(chan struct{}, 1),
		selfWake: make(chan struct{}, 1), done: make(chan struct{})}
	return newCoordinator(p, Config{Root: root, Clock: clk, Now: clk.Now}, w)
}

// A transcript armed by armTargets and replaced before the second
// verifyIdentity, whose re-arm then FAILS, is not granted: the stage would
// otherwise read a replacement no watch covers. The Watch row says watching
// is degraded.
func TestATargetWhoseReArmFailedIsNotGranted(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(transcript, transcriptLines("teal"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := bareCoordinator(t, ports.Root{Sessions: ports.Tree{Path: dir, Dir: dir, FS: os.DirFS(dir)}})
	k := model.Key{SessionID: "s", PID: 1}
	target := model.WatchTarget{Path: transcript, Kind: model.TargetTranscript}
	cs := &cycleState{sessions: []model.Session{{Key: k}}, targets: map[model.Key][]model.WatchTarget{k: {target}}}

	// armTargets: armed, granted.
	rep := c.watcher.Arm([]model.WatchTarget{target})
	if rep.Armed != 1 || len(rep.Failed) != 0 {
		t.Fatalf("the first arm: %+v", rep)
	}
	cs.over, cs.failed = map[string]bool{}, map[string]bool{}

	// Replaced by something that cannot be armed as a file.
	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(transcript, 0o700); err != nil {
		t.Fatal(err)
	}

	c.verifyIdentity(cs)
	if len(cs.arm.Failed) == 0 {
		t.Fatal("the re-arm did not fail: the test is not testing anything")
	}
	g := c.grant(cs)[k]
	if len(g.Armed) != 0 || len(g.Pull) != 0 {
		t.Fatalf("a target whose re-arm failed was granted: %+v", g)
	}
	for _, r := range c.sources(cs) {
		if r.ID == model.SourceWatch && r.State == model.SourceOK {
			t.Errorf("the Watch source is OK although a target it was asked to watch could not be armed")
		}
	}
}

// The same for a job directory: armed by armJobDirs, replaced before the
// first verifyIdentity, re-arm failed — its record is NOT read this cycle.
func TestAJobDirectoryWhoseReArmFailedIsNotRead(t *testing.T) {
	sessions, jobs := t.TempDir(), t.TempDir()
	jobDir := filepath.Join(jobs, "j1")
	if err := os.Mkdir(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := bareCoordinator(t, ports.Root{
		Sessions: ports.Tree{Path: sessions, Dir: sessions, FS: os.DirFS(sessions)},
		Jobs:     ports.Tree{Path: jobs, Dir: jobs, FS: os.DirFS(jobs)},
	})
	cs := &cycleState{}
	c.armSpine(cs)
	c.discoverJobDirs(cs)
	c.armJobDirs(cs)
	if !cs.jobArmed["j1"] {
		t.Fatalf("the job directory was not armed: %+v", cs.arm)
	}

	// Replaced by a directory the re-arm cannot take as one: a file.
	if err := os.Remove(jobDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	c.verifyIdentity(cs)
	c.readSources(cs)
	name := "j1/" + claude.JobStateFile
	for _, p := range cs.jobsRes.Problems {
		if p.Path == name && p.Kind != model.ProblemNotGranted {
			t.Fatalf("a job directory whose re-arm failed was read anyway: %v", p)
		}
	}
	if !hasProblem(cs.jobsRes.Problems, model.ProblemNotGranted) {
		t.Fatalf("its read was not refused: %v", cs.jobsRes.Problems)
	}
}

// Two claimants sharing one transcript that is not held are ONE target that
// could not be armed, not two: the Watch row counts distinct paths (review r3,
// LOW 3: it counted per grant and overstated the loss).
func TestAnUnarmedPathSharedByTwoSessionsIsCountedOnce(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "shared.jsonl")
	if err := os.WriteFile(transcript, transcriptLines("teal"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := bareCoordinator(t, ports.Root{Sessions: ports.Tree{Path: dir, Dir: dir, FS: os.DirFS(dir)}})
	a, b := model.Key{SessionID: "a", PID: 1}, model.Key{SessionID: "b", PID: 2}
	target := model.WatchTarget{Path: transcript, Kind: model.TargetTranscript}
	cs := &cycleState{
		sessions: []model.Session{{Key: a}, {Key: b}},
		targets:  map[model.Key][]model.WatchTarget{a: {target}, b: {target}},
		over:     map[string]bool{}, failed: map[string]bool{},
	}
	// Never armed, so not held: both grants refuse it.
	for k, g := range c.grant(cs) {
		if len(g.Armed) != 0 || len(g.Pull) != 0 {
			t.Fatalf("%v was granted an unheld target: %+v", k, g)
		}
	}
	for _, r := range c.sources(cs) {
		if r.ID != model.SourceWatch {
			continue
		}
		if r.State != model.SourceDegraded || !strings.Contains(r.Detail, "1 targets could not be armed") {
			t.Fatalf("the Watch row is %+v, want degraded and counting the one shared path once", r)
		}
	}
}
