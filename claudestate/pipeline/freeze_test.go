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
