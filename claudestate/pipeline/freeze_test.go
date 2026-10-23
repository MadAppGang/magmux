package pipeline

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
)

// Freeze point 1: a stage that keeps a pointer it set cannot write into a
// session that has been handed out; freeze points 2 and 3 make each delivery
// a copy of its own.
func TestAStageHoldingAPointerCannotReachThePublishedSession(t *testing.T) {
	var kept *string
	keeper := func(model.Key) stage.Stage {
		return &hostStage{name: "keeper", observe: func(s model.Session) model.Session {
			v := "before"
			s.MessagingSocketPath, kept = &v, &v
			return s
		}}
	}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}})},
		Chain: []stage.Factory{keeper}})
	d := h.cycle()
	h.stats() // the actor has finished; kept is safe to touch
	*kept = "after"
	if got := *d.Doc.Sessions[0].MessagingSocketPath; got != "before" {
		t.Fatalf("a stage's pointer reached the published session: %q", got)
	}
	err := h.p.Exec(t.Context(), func(_ context.Context, x Ops) error {
		f, _ := x.Latest()
		if *f.Sessions[0].MessagingSocketPath != "before" {
			t.Errorf("Latest: %q", *f.Sessions[0].MessagingSocketPath)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// C19 through the pipeline with a pane: a record that says nothing (no status
// word at all) lets the inferred tier speak, and it marks itself; a record
// that says something keeps its word.
func TestTheInferredTierIsTheOneWriterAfterTheJoin(t *testing.T) {
	sessions := fstest.MapFS{
		"1.json": {Data: []byte(`{"` + claude.KeyPID + `":1,"` + claude.KeySessionID + `":"quiet"}`)},
		"2.json": {Data: rec(2, "busy", claude.ValueStatusBusy)},
	}
	h := start(t, Config{
		Root:     ports.Root{Sessions: mapTree("/s", sessions)},
		Locator:  everyPane{},
		Activity: silent{since: base.Add(-10 * time.Minute)},
	})
	d := h.cycle()
	q, _ := byKey(d, model.Key{SessionID: "quiet", PID: 1})
	if q.Status != model.StatusIdle || q.Confidence != model.ConfidenceInferred || q.StatusFrom != model.SourceUnknown || q.StatusRaw != "" {
		t.Fatalf("%+v", q)
	}
	b, _ := byKey(d, model.Key{SessionID: "busy", PID: 2})
	if b.Status != model.StatusBusy || b.StatusFrom != model.SourceRegistry || b.Conflict == nil {
		t.Fatalf("a busy record over a silent pane keeps its word, with the conflict published: %+v", b)
	}
}

type everyPane struct{}

func (everyPane) PaneFor(s model.Session) (model.PaneRef, model.Liveness) {
	return model.PaneRef(s.Key.SessionID), model.LivenessYes
}
func (everyPane) PaneAlive(model.PaneRef) model.Liveness { return model.LivenessYes }

type silent struct{ since time.Time }

func (s silent) Activity(model.PaneRef) (time.Time, time.Time, bool) {
	return s.since, time.Time{}, true
}

// Freeze point 3 is "a copy each" for the WHOLE delivery: a sink that edits a
// Problem's Key or PIDs, or a Change's Fields, cannot reach what another sink
// was handed (review r1 #9).
func TestASubscriberMutatingItsProblemsAndChangesCannotReachAnother(t *testing.T) {
	sessions := fstest.MapFS{
		// One id and one pid in two files: a ProblemDuplicateKey carrying Key and PIDs.
		"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)},
		"2.json": {Data: rec(1, "s", claude.ValueStatusIdle)},
	}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions)}})
	other, err := h.p.Subscribe(context.Background(), "other", 16)
	if err != nil {
		t.Fatal(err)
	}
	recv := func() Delivery {
		t.Helper()
		select {
		case d := <-other.C():
			return d
		case <-time.After(wait):
			t.Fatal("the other subscriber got no delivery")
		}
		return Delivery{}
	}
	keyed := func(d Delivery) int {
		for i, p := range d.Problems {
			if p.Key != nil && len(p.PIDs) > 0 {
				return i
			}
		}
		t.Fatalf("no problem with a Key and PIDs: %v", d.Problems)
		return -1
	}

	mine, theirs := h.cycle(), recv()
	i, j := keyed(mine), keyed(theirs)
	mine.Problems[i].Key.SessionID = "mutated"
	mine.Problems[i].PIDs[0] = -1
	if theirs.Problems[j].Key.SessionID != "s" || theirs.Problems[j].PIDs[0] != 1 {
		t.Fatalf("one sink's edit reached another's Problem: key %v pids %v", *theirs.Problems[j].Key, theirs.Problems[j].PIDs)
	}

	sessions["1.json"] = &fstest.MapFile{Data: rec(1, "s", claude.ValueStatusWaiting)}
	sessions["2.json"] = &fstest.MapFile{Data: rec(1, "s", claude.ValueStatusWaiting)}
	mine, theirs = h.cycle(), recv()
	if len(mine.Changes) == 0 || len(mine.Changes[0].Fields) == 0 || len(theirs.Changes) == 0 {
		t.Fatalf("no field change to test with: %+v", mine.Changes)
	}
	want := theirs.Changes[0].Fields[0]
	mine.Changes[0].Fields[0] = "mutated"
	if got := theirs.Changes[0].Fields[0]; got != want {
		t.Fatalf("one sink's edit reached another's Change: %q", got)
	}
}
