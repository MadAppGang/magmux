package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/watch"
)

const bbCoreSID = "00000000-0000-4000-8000-000000000001"
const bbCoreSID2 = "00000000-0000-4000-8000-000000000002"

func bbCoreContext(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Skip("asynchronous black-box tests require a runner deadline")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

func bbCoreClock() *ports.FakeClock {
	return ports.NewFakeClock(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
}

func bbCoreStart(t *testing.T, ctx context.Context, cfg pipeline.Config) *pipeline.Pipeline {
	t.Helper()
	p, err := pipeline.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bbCoreStop(t, ctx, p) })
	return p
}

func bbCoreStop(t *testing.T, ctx context.Context, p *pipeline.Pipeline) {
	t.Helper()
	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Errorf("Stop did not finish before runner-derived deadline: %v", ctx.Err())
	}
}

func bbCoreSubscribe(t *testing.T, ctx context.Context, p *pipeline.Pipeline, name string) *pipeline.Subscription {
	t.Helper()
	s, err := p.Subscribe(ctx, name, 4)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bbCoreDelivery(t *testing.T, ctx context.Context, s *pipeline.Subscription) pipeline.Delivery {
	t.Helper()
	select {
	case d, ok := <-s.C():
		if !ok {
			t.Fatal("subscription closed before delivery")
		}
		return d
	case <-ctx.Done():
		t.Fatalf("delivery did not arrive: %v", ctx.Err())
		return pipeline.Delivery{}
	}
}

func bbCoreRefresh(t *testing.T, ctx context.Context, p *pipeline.Pipeline, s *pipeline.Subscription) pipeline.Delivery {
	t.Helper()
	if !p.Refresh() {
		t.Fatal("live pipeline refused Refresh")
	}
	return bbCoreDelivery(t, ctx, s)
}

func bbCoreExec(t *testing.T, ctx context.Context, p *pipeline.Pipeline, fn func(pipeline.Ops)) {
	t.Helper()
	if err := p.Exec(ctx, func(_ context.Context, ops pipeline.Ops) error { fn(ops); return nil }); err != nil {
		t.Fatal(err)
	}
}

func bbCoreStats(t *testing.T, ctx context.Context, p *pipeline.Pipeline) pipeline.Stats {
	t.Helper()
	stats, err := p.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func bbCoreProblem(d pipeline.Delivery, kind model.ProblemKind) bool {
	for _, p := range d.Problems {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

func bbCoreRecord(id string, pid int, name string) []byte {
	return []byte(fmt.Sprintf(`{"sessionId":%q,"pid":%d,"status":"busy","name":%q,"cwd":"/synthetic/live"}`, id, pid, name))
}

func bbCoreTranscript() []byte {
	return []byte("{\"type\":\"agent-color\",\"agentColor\":\"blue\"}\n" +
		"{\"type\":\"assistant\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"isApiErrorMessage\":true,\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"synthetic error\"}]}}\n")
}

type bbCoreRead struct {
	path string
	n    int
}

// Stat/ReadDir capabilities ensure metadata discovery does not count as content.
// The test changes data only between cycles through Exec; reads are journalled
// through a channel, never a concurrently mutated map or a lock.
type bbCoreFS struct {
	data  fstest.MapFS
	reads chan bbCoreRead
}

func bbCoreMemory(data fstest.MapFS) *bbCoreFS {
	return &bbCoreFS{data: data, reads: make(chan bbCoreRead, 4096)}
}
func (f *bbCoreFS) Stat(name string) (fs.FileInfo, error)      { return fs.Stat(f.data, name) }
func (f *bbCoreFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(f.data, name) }
func (f *bbCoreFS) Open(name string) (fs.File, error) {
	file, err := f.data.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.IsDir() {
		return file, nil
	}
	return &bbCoreFile{File: file, owner: f, path: name}, nil
}

type bbCoreFile struct {
	fs.File
	owner *bbCoreFS
	path  string
}

func (f *bbCoreFile) Read(b []byte) (int, error) {
	n, err := f.File.Read(b)
	if n > 0 {
		select {
		case f.owner.reads <- bbCoreRead{path: f.path, n: n}:
		default:
			panic("synthetic read ledger exhausted")
		}
	}
	return n, err
}
func (f *bbCoreFS) bytesRead() map[string]int {
	out := make(map[string]int)
	for {
		select {
		case r := <-f.reads:
			out[r.path] += r.n
		default:
			return out
		}
	}
}
func bbCoreTree(fsys fs.FS, name string) ports.Tree {
	return ports.Tree{Path: "/synthetic/" + name, FS: fsys}
}

func TestBBPipelineCoreNoPublishBeforeRefreshAndIdleHasNoTimers(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	registry := bbCoreMemory(fstest.MapFS{"101.json": {Data: bbCoreRecord(bbCoreSID, 101, "original")}})
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions")}, Clock: clock})
	sub := bbCoreSubscribe(t, ctx, p, "first")
	bbCoreExec(t, ctx, p, func(ops pipeline.Ops) {
		if _, ok := ops.Latest(); ok {
			t.Error("Start/Subscribe published before Refresh or event")
		}
	})
	select {
	case d := <-sub.C():
		t.Fatalf("premature delivery: %#v", d)
	default:
	}
	d := bbCoreRefresh(t, ctx, p, sub)
	if len(d.Doc.Sessions) != 1 || !d.Doc.Epoch.Equal(clock.Now()) {
		t.Fatalf("first refresh did not publish one document at supplied epoch: %#v", d.Doc)
	}
	stats := bbCoreStats(t, ctx, p)
	if stats.Published == 0 || stats.Watching || clock.Outstanding() != 0 || clock.Created() != 0 {
		t.Errorf("idle on-demand pipeline armed a timer/watcher: stats=%#v created=%d outstanding=%d", stats, clock.Created(), clock.Outstanding())
	}
}

func TestBBPipelineCoreDeliveryAndOpsCopiesAreIndependent(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	registry := bbCoreMemory(fstest.MapFS{"101.json": {Data: bbCoreRecord(bbCoreSID, 101, "original")}})
	projects := bbCoreMemory(fstest.MapFS{"project/" + bbCoreSID + ".jsonl": {Data: bbCoreTranscript()}})
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions"), Projects: bbCoreTree(projects, "projects")}, Clock: clock})
	a, b := bbCoreSubscribe(t, ctx, p, "a"), bbCoreSubscribe(t, ctx, p, "b")
	first := bbCoreRefresh(t, ctx, p, a)
	second := bbCoreDelivery(t, ctx, b)
	if len(first.Doc.Sessions) != 1 || first.Doc.Sessions[0].Transcript == nil || first.Doc.Sessions[0].Transcript.Color == nil {
		t.Fatalf("positive control did not enrich: %#v", first.Doc)
	}
	key := first.Doc.Sessions[0].Key
	first.Doc.Sessions[0].Name = "changed by a"
	*first.Doc.Sessions[0].Transcript.Color = "red"
	first.Doc.Sessions[0].Registry.StatusRaw = "changed by a"
	if second.Doc.Sessions[0].Name != "original" || *second.Doc.Sessions[0].Transcript.Color != "blue" || second.Doc.Sessions[0].Registry.StatusRaw != "busy" {
		t.Fatalf("subscribers shared mutable document storage: %#v", second.Doc)
	}
	bbCoreExec(t, ctx, p, func(ops pipeline.Ops) {
		doc, ok := ops.Latest()
		if !ok {
			t.Error("Latest missing after publish")
			return
		}
		doc.Sessions[0].Name = "changed through Latest"
		*doc.Sessions[0].Transcript.Color = "green"
		session, ok := ops.Session(key)
		if !ok || session.Name != "original" || session.Transcript == nil || session.Transcript.Color == nil || *session.Transcript.Color != "blue" {
			t.Errorf("Ops retained caller's mutated copy: %#v", session)
			return
		}
		session.Name = "changed through Session"
		again, _ := ops.Session(key)
		if again.Name != "original" {
			t.Error("Ops.Session returned shared storage")
		}
	})
}

func TestBBPipelineCoreBothProcessesAreEnrichedAndReadOncePerKey(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	registry := bbCoreMemory(fstest.MapFS{
		"101.json": {Data: bbCoreRecord(bbCoreSID, 101, "first")},
		"102.json": {Data: bbCoreRecord(bbCoreSID, 102, "second")},
	})
	content := bbCoreTranscript()
	path := "project/" + bbCoreSID + ".jsonl"
	projects := bbCoreMemory(fstest.MapFS{path: {Data: content}})
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions"), Projects: bbCoreTree(projects, "projects")}, Clock: clock})
	sub := bbCoreSubscribe(t, ctx, p, "same-id")
	d := bbCoreRefresh(t, ctx, p, sub)
	if len(d.Doc.Sessions) != 2 {
		t.Fatalf("processes collapsed: %#v", d.Doc)
	}
	seen := make(map[int]bool)
	for _, s := range d.Doc.Sessions {
		seen[s.PID] = true
		if s.Key != (model.Key{SessionID: bbCoreSID, PID: s.PID}) || s.Registry == nil || s.Registry.File != fmt.Sprintf("%d.json", s.PID) {
			t.Errorf("process lost own identity/facts: %#v", s)
		}
		facts := s.Transcript
		if facts == nil || facts.Color == nil || *facts.Color != "blue" || facts.APIErrors == nil || *facts.APIErrors != 1 || facts.Subagents == nil || *facts.Subagents != 0 || facts.SubagentEvidence != model.EvidenceAbsent {
			t.Errorf("process not fully enriched: %#v", s)
		}
	}
	if !seen[101] || !seen[102] {
		t.Errorf("wrong process identities: %v", seen)
	}
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {})
	if got := projects.bytesRead()[path]; got != 2*len(content) {
		t.Errorf("facts/timing read %d bytes for two keys, want %d (one small-file read per key)", got, 2*len(content))
	}
}

// Contract §4.10 F (C6 as built, the user's decision in gates.log): a transcript
// located once is kept while one stat confirms it still exists, and the by-id glob
// runs only for an id that is new, has vanished, or is already ambiguous. So a
// second copy that appears while the first still exists is NOT seen, and the
// session stays located and read. Once the first copy vanishes the next cycle
// globs, finds the remaining copies, and goes ambiguous without reading any.
func TestBBPipelineCoreSecondCopyIsNotSeenUntilTheFirstVanishes(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	registry := bbCoreMemory(fstest.MapFS{"101.json": {Data: bbCoreRecord(bbCoreSID, 101, "original")}})
	path := "project-a/" + bbCoreSID + ".jsonl"
	pathB := "project-b/" + bbCoreSID + ".jsonl"
	pathC := "project-c/" + bbCoreSID + ".jsonl"
	projects := bbCoreMemory(fstest.MapFS{path: {Data: bbCoreTranscript()}})
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions"), Projects: bbCoreTree(projects, "projects")}, Clock: clock})
	sub := bbCoreSubscribe(t, ctx, p, "ambiguity")
	first := bbCoreRefresh(t, ctx, p, sub)
	if len(first.Doc.Sessions) != 1 || first.Doc.Sessions[0].Transcript == nil || first.Doc.Sessions[0].Transcript.Color == nil {
		t.Fatal("unique transcript positive control failed")
	}

	// A second copy appears while the first still exists: not seen.
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {
		projects.bytesRead()
		projects.data[pathB] = &fstest.MapFile{Data: bbCoreTranscript()}
	})
	second := bbCoreRefresh(t, ctx, p, sub)
	if len(second.Doc.Sessions) != 1 || second.Doc.Sessions[0].Transcript == nil {
		t.Fatalf("located session lost its transcript facts: %#v", second.Doc)
	}
	kept := second.Doc.Sessions[0].Transcript
	if kept.Location.Matches != 1 || kept.Location.Path != path || kept.Color == nil || *kept.Color != "blue" || kept.APIErrors == nil || *kept.APIErrors != 1 || kept.SubagentEvidence == model.EvidenceAmbiguous {
		t.Errorf("a second copy beside a still-present first was seen: %#v", kept)
	}
	if bbCoreProblem(second, model.ProblemAmbiguousTranscript) {
		t.Errorf("ambiguity reported while the first copy still exists: %#v", second.Problems)
	}
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {})
	reads := projects.bytesRead()
	if reads[path] == 0 {
		t.Errorf("the kept location was not read this cycle: %v", reads)
	}
	if reads[pathB] != 0 {
		t.Errorf("the unseen second copy was read: %v", reads)
	}

	// The first copy vanishes, leaving two others: the next cycle globs and goes
	// ambiguous, and reads neither.
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {
		projects.bytesRead()
		projects.data[pathC] = &fstest.MapFile{Data: bbCoreTranscript()}
		delete(projects.data, path)
	})
	third := bbCoreRefresh(t, ctx, p, sub)
	if len(third.Doc.Sessions) != 1 || third.Doc.Sessions[0].Transcript == nil {
		t.Fatalf("ambiguous location was not disclosed: %#v", third.Doc)
	}
	facts := third.Doc.Sessions[0].Transcript
	if facts.Location.Matches != 2 || facts.Location.Path != "" || facts.Color != nil || facts.Subagents != nil || facts.APIErrors != nil || facts.SubagentEvidence != model.EvidenceAmbiguous {
		t.Errorf("stale unique enrichment survived ambiguity: %#v", facts)
	}
	if !bbCoreProblem(third, model.ProblemAmbiguousTranscript) {
		t.Errorf("ambiguity has no problem: %#v", third.Problems)
	}
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {})
	if reads := projects.bytesRead(); len(reads) != 0 {
		t.Errorf("ambiguous transcripts were read: %v", reads)
	}
}

// C6, unchanged by §4.10 F: an id that is ambiguous on its FIRST location publishes
// no transcript facts and no transcript byte is read.
func TestBBPipelineCoreAmbiguousAtFirstLocationPublishesNoFactsAndReadsNothing(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	registry := bbCoreMemory(fstest.MapFS{"101.json": {Data: bbCoreRecord(bbCoreSID, 101, "original")}})
	projects := bbCoreMemory(fstest.MapFS{
		"project-a/" + bbCoreSID + ".jsonl": {Data: bbCoreTranscript()},
		"project-b/" + bbCoreSID + ".jsonl": {Data: bbCoreTranscript()},
	})
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions"), Projects: bbCoreTree(projects, "projects")}, Clock: clock})
	sub := bbCoreSubscribe(t, ctx, p, "ambiguous-first")
	d := bbCoreRefresh(t, ctx, p, sub)
	if len(d.Doc.Sessions) != 1 || d.Doc.Sessions[0].Transcript == nil {
		t.Fatalf("ambiguous location was not disclosed: %#v", d.Doc)
	}
	facts := d.Doc.Sessions[0].Transcript
	if facts.Location.Matches != 2 || facts.Location.Path != "" || facts.Color != nil || facts.Subagents != nil || facts.APIErrors != nil || facts.SubagentEvidence != model.EvidenceAmbiguous {
		t.Errorf("ambiguous-at-first transcript published facts: %#v", facts)
	}
	if facts.ContextTokens != nil || facts.Compactions != nil || facts.LastActivityAt != nil || facts.LatestText != "" || facts.LastAPIError != "" {
		t.Errorf("ambiguous-at-first transcript published content-derived facts: %#v", facts)
	}
	if !bbCoreProblem(d, model.ProblemAmbiguousTranscript) {
		t.Errorf("ambiguity has no problem: %#v", d.Problems)
	}
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {})
	if reads := projects.bytesRead(); len(reads) != 0 {
		t.Errorf("ambiguous transcripts were read: %v", reads)
	}
}

type bbCoreFailLocator struct{}

func (bbCoreFailLocator) PaneFor(model.Session) (model.PaneRef, model.Liveness) {
	return "wrong", model.LivenessNo
}
func (bbCoreFailLocator) PaneAlive(model.PaneRef) model.Liveness { return model.LivenessNo }
func (bbCoreFailLocator) ForCycle(context.Context) (ports.PaneLocator, error) {
	return nil, errors.New("synthetic cycle-locator failure")
}

func TestBBPipelineCoreNilPortsAndFailedCycleLocatorStayUnknown(t *testing.T) {
	for _, tt := range []struct {
		name    string
		locator ports.PaneLocator
	}{
		{"nil", nil}, {"failed cycle snapshot", bbCoreFailLocator{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, clock := bbCoreContext(t), bbCoreClock()
			registry := bbCoreMemory(fstest.MapFS{"101.json": {Data: bbCoreRecord(bbCoreSID, 101, "original")}})
			p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions")}, Clock: clock, Locator: tt.locator})
			sub := bbCoreSubscribe(t, ctx, p, "unknown")
			d := bbCoreRefresh(t, ctx, p, sub)
			if len(d.Doc.Sessions) != 1 {
				t.Fatalf("missing session: %#v", d.Doc)
			}
			s := d.Doc.Sessions[0]
			if s.ProcessAlive != model.LivenessUnknown || s.PaneAlive != model.LivenessUnknown || s.Pane != "" {
				t.Errorf("nil/failing port invented negative facts: %#v", s)
			}
		})
	}
}

func TestBBPipelineCoreWatchOffDiscoversJobsAndBlockedTempoNeedsAttention(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	jobs := bbCoreMemory(fstest.MapFS{
		"short/state.json":        {Data: []byte(`{"sessionId":"` + bbCoreSID + `","state":"working","tempo":"blocked"}`)},
		"short/deeper/state.json": {Data: []byte(`{"sessionId":"must-not-be-a-job","state":"done"}`)},
	})
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Jobs: bbCoreTree(jobs, "jobs")}, Clock: clock, Watch: false})
	sub := bbCoreSubscribe(t, ctx, p, "jobs")
	d := bbCoreRefresh(t, ctx, p, sub)
	if len(d.Doc.Sessions) != 1 {
		t.Fatalf("off-watch job discovery/depth = %#v", d.Doc)
	}
	s := d.Doc.Sessions[0]
	if s.Key != (model.Key{SessionID: bbCoreSID, Job: "short"}) || s.Status != model.StatusBlocked || s.StatusFrom != model.SourceJobs || !s.WaitingApplies() || s.Job == nil || s.Job.State != model.StatusWorking || s.Attention.Needed == nil || !*s.Attention.Needed {
		t.Errorf("blocked job effective status/attention = %#v", s)
	}
	watchOff := false
	if len(d.Doc.Sources) != 4 {
		t.Errorf("Sources has %d entries, want always four", len(d.Doc.Sources))
	}
	for _, source := range d.Doc.Sources {
		if source.ID == model.SourceWatch {
			watchOff = source.State == model.SourceOff
		}
	}
	stats := bbCoreStats(t, ctx, p)
	if !watchOff || stats.Watching || stats.LastArm.Held != 0 || clock.Outstanding() != 0 {
		t.Errorf("Watch:false did not disclose off/zero held resources: %#v; sources=%#v", stats, d.Doc.Sources)
	}
}

func TestBBPipelineCoreIncompleteRecordsArePublishedWithProblems(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	registry := bbCoreMemory(fstest.MapFS{"idless.json": {Data: []byte(`{"status":"idle"}`)}})
	jobs := bbCoreMemory(fstest.MapFS{"idless/state.json": {Data: []byte(`{"state":"blocked"}`)}})
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions"), Jobs: bbCoreTree(jobs, "jobs")}, Clock: clock})
	sub := bbCoreSubscribe(t, ctx, p, "incomplete")
	d := bbCoreRefresh(t, ctx, p, sub)
	if len(d.Doc.Sessions) != 2 || !bbCoreProblem(d, model.ProblemMissingField) {
		t.Fatalf("incomplete records dropped or problems hidden: %#v", d)
	}
	keys := make(map[model.Key]bool)
	for _, s := range d.Doc.Sessions {
		keys[s.Key] = true
	}
	if !keys[model.Key{Orphan: "idless.json"}] || !keys[model.Key{Orphan: "jobs/idless"}] {
		t.Errorf("incomplete records lost separate orphan identities: %#v", keys)
	}
}

func TestBBPipelineCoreTornRetryOnceRetainsLastGoodAndRecovers(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	registry := bbCoreMemory(fstest.MapFS{"101.json": {Data: bbCoreRecord(bbCoreSID, 101, "last good")}})
	retry := 17 * time.Millisecond
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: bbCoreTree(registry, "sessions")}, Clock: clock, RetryDelay: retry})
	sub := bbCoreSubscribe(t, ctx, p, "torn")
	initial := bbCoreRefresh(t, ctx, p, sub)
	if len(initial.Doc.Sessions) != 1 {
		t.Fatal("last-good positive control missing")
	}
	key := initial.Doc.Sessions[0].Key
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {
		registry.bytesRead()
		registry.data["101.json"] = &fstest.MapFile{Data: []byte("{")}
	})
	if !p.Refresh() {
		t.Fatal("Refresh refused")
	}
	var timer *ports.FakeTimer
	select {
	case timer = <-clock.Timers():
	case <-ctx.Done():
		t.Fatalf("torn read never armed retry: %v", ctx.Err())
	}
	if timer.Delay() != retry {
		t.Fatalf("retry delay = %v, want %v", timer.Delay(), retry)
	}
	timer.Fire(clock.Now().Add(retry))
	var d pipeline.Delivery
	for {
		d = bbCoreDelivery(t, ctx, sub)
		if bbCoreProblem(d, model.ProblemTorn) {
			break
		}
	}
	if len(d.Doc.Sessions) != 1 || d.Doc.Sessions[0].Key != key || d.Doc.Sessions[0].Name != "last good" {
		t.Errorf("torn read did not keep last-good value: %#v", d.Doc)
	}
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {})
	if got := registry.bytesRead()["101.json"]; got != 2 {
		t.Errorf("torn one-byte record read %d bytes, want first attempt + one retry", got)
	}
	if clock.Created() != 1 || clock.Outstanding() != 0 {
		t.Errorf("retry was not bounded/one-shot: created=%d outstanding=%d", clock.Created(), clock.Outstanding())
	}
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {
		registry.data["101.json"] = &fstest.MapFile{Data: bbCoreRecord(bbCoreSID, 101, "repaired")}
	})
	repaired := bbCoreRefresh(t, ctx, p, sub)
	if len(repaired.Doc.Sessions) != 1 || repaired.Doc.Sessions[0].Name != "repaired" || repaired.Doc.Sessions[0].Key != key {
		t.Errorf("record did not recover under same identity: %#v", repaired.Doc)
	}
}

func bbCoreWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
func bbCoreSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[rel] = "directory"
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBBPipelineCoreLeavesItsInputTreeUnchanged(t *testing.T) {
	ctx, clock := bbCoreContext(t), bbCoreClock()
	// The root is built straight from t.TempDir() (guard G8 accepts only that
	// shape), and dir is recovered from it: Sessions.Path is the configured
	// directory joined with "sessions", made absolute and cleaned, so its
	// parent is the same temporary directory.
	root, err := ports.RootAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(root.Sessions.Path)
	for _, name := range []string{"sessions", "jobs", "projects"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bbCoreWrite(t, filepath.Join(dir, "sessions", "101.json"), bbCoreRecord(bbCoreSID, 101, "original"))
	bbCoreWrite(t, filepath.Join(dir, "sessions", "101.synthetic.key"), []byte("must remain untouched"))
	bbCoreWrite(t, filepath.Join(dir, "projects", "project", bbCoreSID+".jsonl"), bbCoreTranscript())
	before := bbCoreSnapshot(t, dir)
	p := bbCoreStart(t, ctx, pipeline.Config{Root: root, Clock: clock, Watch: false})
	sub := bbCoreSubscribe(t, ctx, p, "read-only")
	bbCoreRefresh(t, ctx, p, sub)
	bbCoreExec(t, ctx, p, func(pipeline.Ops) {})
	bbCoreStop(t, ctx, p)
	if after := bbCoreSnapshot(t, dir); !reflect.DeepEqual(after, before) {
		t.Errorf("library changed test-owned input tree: before=%#v after=%#v", before, after)
	}
}

func TestBBPipelineCoreFileCapIsLoudWithoutBlanketProjectWatches(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("fsnotify's first-class platforms")
	}
	ctx, clock := bbCoreContext(t), bbCoreClock()
	dir := t.TempDir()
	sessionsDir, projectsDir := filepath.Join(dir, "sessions"), filepath.Join(dir, "projects")
	for i, id := range []string{bbCoreSID, bbCoreSID2} {
		bbCoreWrite(t, filepath.Join(sessionsDir, fmt.Sprintf("%d.json", 101+i)), bbCoreRecord(id, 101+i, "original"))
		bbCoreWrite(t, filepath.Join(projectsDir, "project", id+".jsonl"), bbCoreTranscript())
	}
	bbCoreWrite(t, filepath.Join(projectsDir, "project", bbCoreSID, "subagents", "a.meta.json"), []byte("name only, not opened"))
	sessions, err := ports.TreeAt(sessionsDir)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := ports.TreeAt(projectsDir)
	if err != nil {
		t.Fatal(err)
	}
	budget := watch.Budget{Descriptors: 512, Watches: 512, Files: 1, JobDirs: 64, JobTranscripts: 64}
	p := bbCoreStart(t, ctx, pipeline.Config{Root: ports.Root{Sessions: sessions, Projects: projects}, Clock: clock, Watch: true, Budget: budget})
	sub := bbCoreSubscribe(t, ctx, p, "cap")
	d := bbCoreRefresh(t, ctx, p, sub)
	if len(d.Doc.Sessions) != 2 || !bbCoreProblem(d, model.ProblemOverBudget) {
		t.Fatalf("cap silently lost a session/degradation: %#v", d)
	}
	degraded := false
	for _, source := range d.Doc.Sources {
		if source.ID == model.SourceWatch && source.State == model.SourceDegraded {
			degraded = true
		}
	}
	if !degraded {
		t.Errorf("file cap has no degraded watch source: %#v", d.Doc.Sources)
	}
	stats := bbCoreStats(t, ctx, p)
	if stats.LastArm.Held != 2 {
		t.Errorf("held %d objects, want registry spine + one transcript, no projects/subagents dirs: %#v", stats.LastArm.Held, stats.LastArm)
	}
	wantCost := 2
	if runtime.GOOS == "darwin" {
		wantCost = 4
	} // registry dir + two entries + one transcript.
	if stats.LastArm.Cost != wantCost {
		t.Errorf("cost = %d, want %d without blanket directory arms", stats.LastArm.Cost, wantCost)
	}
	if len(stats.LastArm.Over) != 1 || stats.LastArm.Over[0].Dir || stats.LastArm.Over[0].Kind != model.TargetTranscript || !strings.HasPrefix(stats.LastArm.Over[0].Path, projects.Dir+string(filepath.Separator)) {
		t.Errorf("file-cap refused targets = %#v", stats.LastArm.Over)
	}
	for _, session := range d.Doc.Sessions {
		if session.Transcript == nil || session.Transcript.Color == nil || *session.Transcript.Color != "blue" {
			t.Errorf("budget pull grant did not permit degraded read: %#v", session)
		}
	}
}
