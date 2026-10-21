package pipeline

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
)

func TestStartRefusesWhatItCannotRun(t *testing.T) {
	clk := ports.NewFakeClock(base)
	if _, err := Start(Config{Clock: clk}); !errors.Is(err, ErrNoRoot) {
		t.Fatalf("no tree: %v", err)
	}
	r := ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}
	if _, err := Start(Config{Clock: clk, Root: r, Watch: true}); !errors.Is(err, ErrUnwatchable) {
		t.Fatalf("Watch over a tree with no Dir: %v", err)
	}
	if _, err := Start(Config{Root: r}); err == nil {
		t.Fatal("a Clock is required")
	}
}

// C9: nothing is published before the first Refresh or event, and every
// Delivery is a deep copy.
func TestNothingIsPublishedBeforeTheFirstRefresh(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})}})
	h.quiet(100 * time.Millisecond)
	a := h.cycle()
	if len(a.Doc.Sessions) != 1 || !a.Doc.Epoch.Equal(base) {
		t.Fatalf("%+v", a.Doc)
	}
	a.Doc.Sessions[0].Name = "mutated"
	b := h.cycle()
	if b.Doc.Sessions[0].Name == "mutated" {
		t.Fatal("a delivery shares memory with the next")
	}
	if len(b.Changes) != 0 {
		t.Fatalf("a quiet machine changed: %+v", b.Changes)
	}
	if len(a.Changes) != 1 || a.Changes[0].Type.String() != "added" {
		t.Fatalf("the first document's changes are additions: %+v", a.Changes)
	}
}

// §3.1's Sources table: exactly four, in order, and a zero tree is Off.
func TestTheSourcesTableIsFourRowsInOrder(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/absent", fstest.MapFS{})}})
	d := h.cycle()
	if len(d.Doc.Sources) != 4 {
		t.Fatalf("%+v", d.Doc.Sources)
	}
	want := []model.SourceID{model.SourceRegistry, model.SourceJobs, model.SourceAgents, model.SourceWatch}
	for i, s := range d.Doc.Sources {
		if s.ID != want[i] {
			t.Fatalf("row %d is %v", i, s.ID)
		}
	}
	if s := sourceRow(d, model.SourceJobs); s.State != model.SourceOff {
		t.Errorf("a zero jobs tree: %v", s.State)
	}
	if s := sourceRow(d, model.SourceAgents); s.State != model.SourceOff {
		t.Errorf("no agents port: %v", s.State)
	}
	if s := sourceRow(d, model.SourceWatch); s.State != model.SourceOff {
		t.Errorf("Watch false: %v", s.State)
	}
	if s := sourceRow(d, model.SourceRegistry); s.State != model.SourceOK {
		t.Errorf("an empty registry: %v %q", s.State, s.Detail)
	}
}

// The corpus, end to end with Watch off (C18): the S-origin roots and tt's.
func TestTheCorpusRootsPublish(t *testing.T) {
	// The corpus holds torn records, so the retry waits on a REAL clock here:
	// a fake one would wait for a Fire nobody sends.
	h := start(t, Config{Root: corpusRoot("magmux-corpus"), Oracle: oracle{60001: model.LivenessNo}, Clock: ports.RealClock(), RetryDelay: time.Millisecond})
	d := h.cycle()
	if n := len(d.Doc.Sessions); n != 19 { // 21 records; 60004 is torn and 60005 empty, with no earlier value
		t.Fatalf("%d sessions from the S-origin registry", n)
	}
	if !hasProblem(d.Problems, model.ProblemTorn) || !hasProblem(d.Problems, model.ProblemPIDMismatch) {
		t.Fatalf("the torn record and the pid mismatch are reported: %v", d.Problems)
	}
	s, _ := byKey(d, model.Key{SessionID: "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b", PID: 98693})
	if s.Transcript == nil || s.Transcript.APIErrors == nil || *s.Transcript.APIErrors != 3 || s.Transcript.Color != nil ||
		s.Fault.APIErrors == nil || *s.Fault.APIErrors != 3 || s.Attention.Needed == nil || !*s.Attention.Needed {
		t.Fatalf("98693: %+v %+v", s.Transcript, s.Attention)
	}
	if st := h.stats(); st.Watching || st.TranscriptGlobs == 0 {
		t.Fatalf("Watch false: %+v", st)
	}

	h2 := start(t, Config{Root: corpusRoot("tt-corpus"), Clock: ports.RealClock(), RetryDelay: time.Millisecond})
	d2 := h2.cycle()
	if sourceRow(d2, model.SourceJobs).State != model.SourceDegraded || !hasProblem(d2.Problems, model.ProblemTorn) {
		t.Fatalf("tt's torn job record degrades the jobs source: %+v", sourceRow(d2, model.SourceJobs))
	}
	if !hasProblem(d2.Problems, model.ProblemOutsideRoot) {
		t.Fatalf("the escaping linkScanPath: %v", d2.Problems)
	}
	bwn, ok := byKey(d2, model.Key{SessionID: "97030e53-3ae6-4b0e-9f5a-1c5d4b2a8e01", Job: "blocked-with-needs"})
	if !ok || bwn.Transcript == nil || bwn.Transcript.LastActivityAt == nil || !bwn.Transcript.Location.Declared {
		t.Fatalf("a job's declared transcript is read: %+v", bwn.Transcript)
	}
}

// C20, NFR13 per key (AQ4): ONE read of each transcript per key per cycle,
// for both windows; two keys on one id each read it once.
func TestTwoProcessesOnOneIDReadTheTranscriptOncePerKey(t *testing.T) {
	projects := newCounting(fstest.MapFS{"p/s.jsonl": {Data: transcriptLines("teal")}})
	sessions := fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}, "2.json": {Data: rec(2, "s", claude.ValueStatusBusy)}}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions), Projects: mapTree("/p", projects)}})
	d := h.cycle()
	if len(d.Doc.Sessions) != 2 {
		t.Fatalf("two processes on one id are two sessions (D12): %d", len(d.Doc.Sessions))
	}
	for _, s := range d.Doc.Sessions {
		if s.Transcript == nil || s.Transcript.Color == nil || *s.Transcript.Color != "teal" {
			t.Fatalf("both are enriched: %+v", s.Transcript)
		}
	}
	if projects.opens()["p/s.jsonl"] != 2 {
		t.Fatalf("%d opens of the transcript for two keys in one cycle", projects.opens()["p/s.jsonl"])
	}
	h.cycle()
	if projects.opens()["p/s.jsonl"] != 4 {
		t.Fatalf("%d opens after two cycles", projects.opens()["p/s.jsonl"])
	}
}

// C6 and D5 under the orchestrator's cache rule: an ambiguous id opens
// nothing; a located single match is kept while one stat confirms it, so the
// glob does not run again; a vanished or moved file is re-globbed and found
// where it now is. Locating arms no timer.
func TestLocationIsByIDAndTheCacheRuleHolds(t *testing.T) {
	projects := fstest.MapFS{"a/s.jsonl": {Data: transcriptLines("red")}, "a/t.jsonl": {Data: transcriptLines("x")}, "b/t.jsonl": {Data: transcriptLines("y")}}
	counting := newCounting(projects)
	sessions := fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}, "2.json": {Data: rec(2, "t", claude.ValueStatusIdle)}}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions), Projects: mapTree("/p", counting)}})
	created := h.clk.Created()

	d := h.cycle()
	amb, _ := byKey(d, model.Key{SessionID: "t", PID: 2})
	if amb.Transcript == nil || amb.Transcript.Location.Matches != 2 || amb.Transcript.Color != nil ||
		amb.Transcript.SubagentEvidence != model.EvidenceAmbiguous || counting.opens()["a/t.jsonl"]+counting.opens()["b/t.jsonl"] != 0 {
		t.Fatalf("ambiguous: %+v, opens %v", amb.Transcript, counting.opens())
	}
	if !hasProblem(d.Problems, model.ProblemAmbiguousTranscript) {
		t.Fatal("no ProblemAmbiguousTranscript")
	}
	if g := h.stats().TranscriptGlobs; g != 2 {
		t.Fatalf("%d globs in the first cycle", g)
	}
	h.cycle()
	if g := h.stats().TranscriptGlobs; g != 3 {
		t.Fatalf("the located id is re-confirmed by a stat and the ambiguous one re-globbed: %d globs", g)
	}

	// The file moves: the stat fails, the glob runs, and it is found anew.
	projects["c/s.jsonl"] = projects["a/s.jsonl"]
	delete(projects, "a/s.jsonl")
	d = h.cycle()
	s, _ := byKey(d, model.Key{SessionID: "s", PID: 1})
	if s.Transcript == nil || s.Transcript.Location.Path != "c/s.jsonl" || *s.Transcript.Color != "red" {
		t.Fatalf("a moved transcript is found where it now is: %+v", s.Transcript)
	}
	if h.clk.Created() != created {
		t.Fatalf("locating armed %d timers", h.clk.Created()-created)
	}
}

// An id outside tt's alphabet, and an empty one, are never globbed.
func TestAnIDOutsideTTsAlphabetIsNeverGlobbed(t *testing.T) {
	sessions := fstest.MapFS{"1.json": {Data: rec(1, "a*b", claude.ValueStatusIdle)}, "2.json": {Data: rec(2, "", claude.ValueStatusIdle)}}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions), Projects: mapTree("/p", fstest.MapFS{"x/a*b.jsonl": {Data: []byte("{}\n")}})}})
	d := h.cycle()
	for _, s := range d.Doc.Sessions {
		if s.Transcript != nil {
			t.Errorf("%s: %+v", s.Key, s.Transcript)
		}
	}
	if g := h.stats().TranscriptGlobs; g != 0 {
		t.Fatalf("%d globs", g)
	}
}

// A lone K1 has no job speaker, so its transcript is located by session id
// (review 3 Issue 7).
func TestALoneBackgroundRecordIsLocatedBySessionID(t *testing.T) {
	h := start(t, Config{Root: corpusRoot("lone-bg-with-transcript")})
	d := h.cycle()
	s, ok := byKey(d, model.Key{SessionID: "30000000-0000-4000-8000-000000000021", Job: "lbt00001"})
	if !ok || s.Job != nil || s.Transcript == nil || s.Transcript.Color == nil || *s.Transcript.Color != "teal" {
		t.Fatalf("%+v", s)
	}
}

// C13: problems OnEffects returns reach every subscriber in the NEXT
// Delivery, which the coordinator triggers itself, once. The effect handed
// over is the stage's own value, paired with the session as published.
func TestOnEffectsProblemsReachEverySubscriberThroughOneSelfWake(t *testing.T) {
	var batches []EffectBatch
	cfg := Config{
		Root:  ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusWaiting)}})},
		Chain: []stage.Factory{func(model.Key) stage.Stage { return &hostStage{name: "host", emit: true} }},
		OnEffects: func(ctx context.Context, b EffectBatch) []model.Problem {
			batches = append(batches, b)
			return []model.Problem{{Scope: model.ScopeHost, Kind: model.ProblemHost, Detail: "the audit trail is unwritable"}}
		},
	}
	h := start(t, cfg)
	other, err := h.p.Subscribe(context.Background(), "other", 4)
	if err != nil {
		t.Fatal(err)
	}
	first := h.cycle()
	if hasProblem(first.Problems, model.ProblemHost) {
		t.Fatal("a host problem arrived before OnEffects ran")
	}
	second := h.next()
	if !hasProblem(second.Problems, model.ProblemHost) {
		t.Fatalf("the self-woken delivery carries the host problem: %v", second.Problems)
	}
	h.quiet(200 * time.Millisecond) // the self-wake cycle never self-wakes
	n := 0
	for range 2 {
		select {
		case d := <-other.C():
			if hasProblem(d.Problems, model.ProblemHost) {
				n++
			}
		case <-time.After(wait):
			t.Fatal("the second subscriber missed a delivery")
		}
	}
	if n != 1 {
		t.Fatalf("the other subscriber saw the host problem %d times", n)
	}
	h.stats() // synchronises with the coordinator, after its last OnEffects
	if len(batches) != 2 {
		t.Fatalf("%d batches", len(batches))
	}
	it := batches[0].Items[0]
	if e, ok := it.Effect.(effect); !ok || e.n != 1 || it.Published.Key != it.Key || it.Published.Attention.Needed == nil {
		t.Fatalf("the effect is the stage's own value, with the session as published: %+v", it)
	}
}

// C15: inside Exec's fn and OnEffects the blocking methods refuse with the
// callback's context; Refresh succeeds.
func TestReentryIsRefusedNotDeadlocked(t *testing.T) {
	check := func(ctx context.Context, p *Pipeline) error {
		if err := p.Exec(ctx, func(context.Context, Ops) error { return nil }); !errors.Is(err, ErrReentrant) {
			return errors.New("Exec: " + errString(err))
		}
		if _, err := p.RefreshSources(ctx, []model.SourceID{model.SourceAgents}); !errors.Is(err, ErrReentrant) {
			return errors.New("RefreshSources: " + errString(err))
		}
		if _, err := p.Stats(ctx); !errors.Is(err, ErrReentrant) {
			return errors.New("Stats: " + errString(err))
		}
		if _, err := p.Subscribe(ctx, "x", 1); !errors.Is(err, ErrReentrant) {
			return errors.New("Subscribe: " + errString(err))
		}
		if err := p.Unsubscribe(ctx, &Subscription{}); !errors.Is(err, ErrReentrant) {
			return errors.New("Unsubscribe: " + errString(err))
		}
		if !p.Refresh() {
			return errors.New("Refresh refused")
		}
		return nil
	}
	var pipe *Pipeline
	fromEffects := make(chan error, 1)
	h := start(t, Config{
		Root:  ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})},
		Chain: []stage.Factory{func(model.Key) stage.Stage { return &hostStage{name: "host", emit: true} }},
		OnEffects: func(ctx context.Context, _ EffectBatch) []model.Problem {
			select {
			case fromEffects <- check(ctx, pipe):
			default: // only the first is kept
			}
			return nil
		},
	})
	pipe = h.p
	h.cycle()
	if err := h.p.Exec(context.Background(), func(ctx context.Context, _ Ops) error { return check(ctx, h.p) }); err != nil {
		t.Fatalf("inside Exec: %v", err)
	}
	select {
	case err := <-fromEffects:
		if err != nil {
			t.Fatalf("inside OnEffects: %v", err)
		}
	case <-time.After(wait):
		t.Fatal("OnEffects never ran")
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// Exec and Call: the frozen session, a stage found by name, on its actor; a
// retired key is ErrNoSuchKey (C14), and its alarm is stopped.
func TestExecCallAndRetirement(t *testing.T) {
	sessions := fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}}
	wk := func(model.Key) stage.Stage { return &waker{d: time.Hour} }
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions)}, Chain: []stage.Factory{wk}})
	h.cycle()
	key := model.Key{SessionID: "s", PID: 1}
	var found bool
	err := h.p.Exec(context.Background(), func(ctx context.Context, x Ops) error {
		if f, ok := x.Latest(); !ok || len(f.Sessions) != 1 {
			return errors.New("Latest")
		}
		return x.Call(ctx, key, func(s model.Session, find func(string) (stage.Stage, bool)) error {
			_, found = find("waker")
			if s.Key != key {
				return errors.New("the frozen session's key")
			}
			return nil
		})
	})
	if err != nil || !found {
		t.Fatalf("%v %v", err, found)
	}
	if h.clk.Outstanding() != 1 {
		t.Fatalf("the waker armed %d alarms", h.clk.Outstanding())
	}
	delete(sessions, "1.json")
	h.cycle()
	st := h.stats()
	if st.ActorsStopped != 1 || h.clk.Outstanding() != 0 {
		t.Fatalf("retirement: %+v, outstanding %d", st, h.clk.Outstanding())
	}
	err = h.p.Exec(context.Background(), func(ctx context.Context, x Ops) error {
		return x.Call(ctx, key, func(model.Session, func(string) (stage.Stage, bool)) error { return nil })
	})
	if !errors.Is(err, ErrNoSuchKey) {
		t.Fatalf("a retired key: %v", err)
	}
}

type waker struct{ d time.Duration }

func (w *waker) Name() string                          { return "waker" }
func (w *waker) Observe(s model.Session) model.Session { return s }
func (w *waker) WakeIn() (time.Duration, bool)         { return w.d, w.d > 0 }

// An actor's alarm drives a cycle with nothing on disk changing.
func TestAWakerAlarmDrivesACycle(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})},
		Chain: []stage.Factory{func(model.Key) stage.Stage { return &waker{d: 5 * time.Second} }}})
	h.cycle()
	var ft *ports.FakeTimer
	select {
	case ft = <-h.clk.Timers():
	case <-time.After(wait):
		t.Fatal("no alarm")
	}
	if ft.Delay() != 5*time.Second {
		t.Fatalf("delay %v", ft.Delay())
	}
	ft.Fire(base.Add(5 * time.Second))
	h.next()
	if st := h.stats(); st.Cycles != 2 {
		t.Fatalf("%d cycles", st.Cycles)
	}
}

// C21: a stage that changes the key is undone and reported, its effects are
// still drained, and the published session keeps its actor's key.
func TestAStageThatChangesTheKeyIsUndoneAndReported(t *testing.T) {
	var items []EffectItem
	bad := func(model.Key) stage.Stage {
		return &hostStage{name: "rekey", emit: true, observe: func(s model.Session) model.Session {
			s.Key = model.Key{SessionID: "someone-else"}
			s.Name = "should not survive"
			return s
		}}
	}
	h := start(t, Config{
		Root:      ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})},
		Chain:     []stage.Factory{bad},
		OnEffects: func(_ context.Context, b EffectBatch) []model.Problem { items = append(items, b.Items...); return nil },
	})
	d := h.cycle()
	s, ok := byKey(d, model.Key{SessionID: "s", PID: 1})
	if !ok || s.Name == "should not survive" || len(d.Doc.Sessions) != 1 {
		t.Fatalf("%+v", d.Doc.Sessions)
	}
	if !hasProblem(d.Problems, model.ProblemHost) {
		t.Fatalf("no ProblemHost: %v", d.Problems)
	}
	h.stats()
	if len(items) != 1 || items[0].Published.Key != s.Key {
		t.Fatalf("the effect is still drained and paired with the actor's key: %+v", items)
	}
}

// C19 through the pipeline: the effective fields are claude.Effective over
// the speakers, and a job-only working + tempo blocked needs a human.
func TestOneEffectiveStatusThroughThePipeline(t *testing.T) {
	h := start(t, Config{Root: corpusRoot("working-tempo-blocked")})
	d := h.cycle()
	s := d.Doc.Sessions[0]
	if s.Status != model.StatusBlocked || !s.WaitingApplies() || s.StatusFrom != model.SourceJobs ||
		s.Attention.Needed == nil || !*s.Attention.Needed || s.Confidence != model.ConfidenceAuthoritative {
		t.Fatalf("%+v", s)
	}
	h2 := start(t, Config{Root: corpusRoot("twin-dead-blocked"), Oracle: oracle{70001: model.LivenessNo}})
	s2 := h2.cycle().Doc.Sessions[0]
	if s2.Status != model.StatusBlocked || s2.ProcessAlive != model.LivenessNo || !*s2.Attention.Needed {
		t.Fatalf("a dead twin over a blocked job: %+v", s2)
	}
	h3 := start(t, Config{Root: corpusRoot("twin-waiting-done"), Oracle: oracle{70002: model.LivenessNo}})
	s3 := h3.cycle().Doc.Sessions[0]
	if s3.Status != model.StatusDone || s3.WaitingApplies() || *s3.Attention.Needed {
		t.Fatalf("a dead twin waiting over a done job: %+v", s3)
	}
}

// §3.4 rows 6 and 14 asserted on the actors: a second claimant keeps the key
// and starts no actor; a spare acquiring a jobId retires one and starts one,
// and no delivery holds both keys.
func TestKeyTransitionsOnTheActors(t *testing.T) {
	sessions := fstest.MapFS{"100.json": {Data: rec(100, "s", claude.ValueStatusIdle, kv(claude.KeyKind, claude.ValueKindBG), kv(claude.KeyJobID, "j"))}}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions)}})
	h.cycle()
	sessions["50.json"] = &fstest.MapFile{Data: rec(50, "s", claude.ValueStatusBusy, kv(claude.KeyKind, claude.ValueKindBG), kv(claude.KeyJobID, "j"))}
	d := h.cycle()
	if st := h.stats(); st.ActorsStarted != 1 || len(d.Doc.Sessions) != 1 || d.Doc.Sessions[0].PID != 50 {
		t.Fatalf("row 6: %+v %+v", st, d.Doc.Sessions)
	}

	spare := fstest.MapFS{"7.json": {Data: rec(7, "t", claude.ValueStatusIdle, kv(claude.KeyKind, claude.ValueKindBG))}}
	h2 := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", spare)}})
	h2.cycle()
	spare["7.json"] = &fstest.MapFile{Data: rec(7, "t", claude.ValueStatusIdle, kv(claude.KeyKind, claude.ValueKindBG), kv(claude.KeyJobID, "j"))}
	d2 := h2.cycle()
	st := h2.stats()
	if st.ActorsStarted != 2 || st.ActorsStopped != 1 || len(d2.Doc.Sessions) != 1 || d2.Doc.Sessions[0].Key.Job != "j" {
		t.Fatalf("row 14: %+v %+v", st, d2.Doc.Sessions)
	}
}

// C10 and step 8: a CycleLocator is asked once per cycle; on error the cycle
// uses UnknownLocator, never the configured locator.
func TestAFailingCycleLocatorMakesEveryPaneUnknown(t *testing.T) {
	loc := &cycleLocator{fail: true}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})}, Locator: loc})
	s := h.cycle().Doc.Sessions[0]
	if s.Pane != "" || s.PaneAlive != model.LivenessUnknown || loc.cycles != 1 {
		t.Fatalf("%+v cycles %d", s, loc.cycles)
	}
	loc.fail = false
	s = h.cycle().Doc.Sessions[0]
	if s.Pane != "7" || s.PaneAlive != model.LivenessYes || loc.cycles != 2 {
		t.Fatalf("%+v cycles %d", s, loc.cycles)
	}
}

// cycleLocator is touched only by the coordinator (ForCycle) and, through the
// snapshot it returns, by actors; the snapshot is immutable.
type cycleLocator struct {
	fail   bool
	cycles int
}

func (l *cycleLocator) PaneFor(model.Session) (model.PaneRef, model.Liveness) {
	return "", model.LivenessNo
}
func (l *cycleLocator) PaneAlive(model.PaneRef) model.Liveness { return model.LivenessNo }
func (l *cycleLocator) ForCycle(context.Context) (ports.PaneLocator, error) {
	l.cycles++
	if l.fail {
		return nil, errors.New("the pane table is unavailable")
	}
	return snapshotLocator{}, nil
}

type snapshotLocator struct{}

func (snapshotLocator) PaneFor(model.Session) (model.PaneRef, model.Liveness) {
	return "7", model.LivenessYes
}
func (snapshotLocator) PaneAlive(model.PaneRef) model.Liveness { return model.LivenessYes }

// The hub never blocks: a full buffer drops the oldest delivery and the
// subscriber is resynced with the newest.
func TestASlowSubscriberIsResyncedNotWaitedFor(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})}})
	slow, _ := h.p.Subscribe(context.Background(), "slow", 1)
	for range 3 {
		h.cycle()
	}
	var d Delivery
	select {
	case d = <-slow.C():
	case <-time.After(wait):
		t.Fatal("the slow sink never received a document; the setup is wrong, not the hub")
	}
	if !d.Resync || d.Dropped != 2 {
		t.Fatalf("resync %v dropped %d", d.Resync, d.Dropped)
	}
	if st := h.stats(); st.HubDropped != 2 || st.Resyncs != 2 || st.Subscribers != 2 {
		t.Fatalf("%+v", st)
	}
	if err := h.p.Unsubscribe(context.Background(), slow); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-slow.C(); ok {
		t.Fatal("Unsubscribe closes the channel")
	}
	if err := h.p.Unsubscribe(context.Background(), slow); err != nil {
		t.Fatal("Unsubscribe is idempotent")
	}
}

func TestStopIsIdempotentAndClosesEverySubscription(t *testing.T) {
	clk := ports.NewFakeClock(base)
	p, err := Start(Config{Clock: clk, Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})}})
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := p.Subscribe(context.Background(), "x", 1)
	p.Refresh()
	<-sub.C()
	p.Stop()
	p.Stop()
	if _, ok := <-sub.C(); ok {
		t.Fatal("Stop closes subscriptions")
	}
	if p.Refresh() {
		t.Fatal("Refresh after Stop")
	}
	if _, err := p.Stats(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("%v", err)
	}
	if clk.Outstanding() != 0 {
		t.Fatalf("%d timers outstanding after Stop", clk.Outstanding())
	}
}

// C2: steady and idle, nothing is outstanding on the clock: no coordinator
// timer, no locating timer, no agents-source timer.
func TestASteadyPipelineHoldsNoTimer(t *testing.T) {
	h := start(t, Config{Root: corpusRoot("k2-duplicate-different-transcripts"), Agents: &listing{raw: []byte("[]")}})
	if _, err := h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents}); err != nil {
		t.Fatal(err)
	}
	h.next()
	for range 3 {
		h.cycle()
	}
	if h.clk.Outstanding() != 0 || h.clk.Created() != 0 {
		t.Fatalf("created %d, outstanding %d", h.clk.Created(), h.clk.Outstanding())
	}
}

// A torn registry record is retried once after RetryDelay on the injected
// clock, then its last good value is kept, with a ProblemTorn (C8).
func TestATornRecordIsRetriedThenKept(t *testing.T) {
	sessions := fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle, kv(claude.KeyName, "good"))}}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions)}, RetryDelay: time.Second})
	h.cycle()
	sessions["1.json"] = &fstest.MapFile{Data: []byte(`{"pid":1,"sess`)}
	h.p.Refresh()
	ft := <-h.clk.Timers()
	if ft.Delay() != time.Second {
		t.Fatalf("retry delay %v", ft.Delay())
	}
	ft.Fire(base.Add(time.Second))
	d := h.next()
	if s := d.Doc.Sessions; len(s) != 1 || s[0].Name != "good" || !hasProblem(d.Problems, model.ProblemTorn) ||
		sourceRow(d, model.SourceRegistry).State != model.SourceDegraded {
		t.Fatalf("%+v %v", s, d.Problems)
	}
	if h.clk.Outstanding() != 0 {
		t.Fatalf("the retry timer is released: %d", h.clk.Outstanding())
	}
}

// A slow registry read never stops the cycle from finishing; this is only the
// sanity check that a cycle with no transcripts reads nothing under projects.
func TestADocumentWithNoProjectsTreeReadsNoTranscript(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})}})
	if s := h.cycle().Doc.Sessions[0]; s.Transcript != nil || h.stats().TranscriptGlobs != 0 {
		t.Fatalf("%+v", s.Transcript)
	}
}

// Once the coordinator has accepted an Exec, fn runs; its answer is the
// answer, even if the caller's context ends while fn is running. Reporting
// ctx.Err() then would tell the caller "not done" about something that was
// done (review r1 #6b).
func TestAnExecThatRanReportsWhatItsFunctionReturned(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}})
	sentinel := errors.New("fn's own answer")
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	err := h.p.Exec(ctx, func(context.Context, Ops) error {
		cancel()
		time.Sleep(50 * time.Millisecond) // the caller's ctx is done well before fn returns
		close(ran)
		return sentinel
	})
	select {
	case <-ran:
	default:
		t.Fatalf("Exec returned %v before fn had finished", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Exec returned %v after fn ran and returned %v", err, sentinel)
	}
}

// Through the pipeline: a wake that changed nothing reads no transcript again.
// The second cycle stats the file and reuses the first cycle's facts; an
// append is read on the next cycle (review r1 #4a).
func TestAWakeReadsOnlyTheTranscriptsThatChanged(t *testing.T) {
	old := base.Add(-time.Hour)
	inner := fstest.MapFS{"p/s.jsonl": {Data: transcriptLines("teal"), ModTime: old}}
	projects := newCounting(inner)
	sessions := fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions), Projects: mapTree("/p", projects)}})
	h.cycle()
	d := h.cycle()
	if n := projects.opens()["p/s.jsonl"]; n != 1 {
		t.Fatalf("%d opens of an unchanged transcript over two cycles, want 1", n)
	}
	if s := d.Doc.Sessions[0]; s.Transcript == nil || s.Transcript.Color == nil || *s.Transcript.Color != "teal" {
		t.Fatalf("the reused facts: %+v", d.Doc.Sessions[0].Transcript)
	}
	inner["p/s.jsonl"] = &fstest.MapFile{Data: append(transcriptLines("teal"), transcriptLines("ruby")...), ModTime: old.Add(time.Minute)}
	d = h.cycle()
	if n := projects.opens()["p/s.jsonl"]; n != 2 || *d.Doc.Sessions[0].Transcript.Color != "ruby" {
		t.Fatalf("an appended transcript: %d opens, %+v", n, d.Doc.Sessions[0].Transcript)
	}
}
