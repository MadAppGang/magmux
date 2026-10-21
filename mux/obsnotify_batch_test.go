package mux

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/notify"
)

// stallingPort is a delivery that takes as long as it is allowed to: Send
// returns only when its context ends. Each call is therefore bounded only by
// whatever deadline the caller's context carries.
type stallingPort struct{ calls chan struct{} }

func (p stallingPort) Send(ctx context.Context, n notify.Note) error {
	p.calls <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

// OnEffects runs on the coordinator, which serves every session's next cycle,
// every Exec (agents focus) and every Stats. Each note is bounded by the port,
// but a burst of N notes used to cost N bounds, one after another — at startup
// every waiting session fires at once. The BATCH has one deadline: a note
// reached after it is still recorded, attempt and outcome, and the deadline
// is reported, never silently dropped (review r1 #5).
func TestANotificationBurstIsBoundedAsABatch(t *testing.T) {
	trail := trailPath(t)
	w, err := act.NewWriter(act.WriterConfig{Path: trail})
	if err != nil {
		t.Fatal(err)
	}
	port := stallingPort{calls: make(chan struct{}, 8)}
	acts := newObsActions(act.Default(), w, port, nil)
	acts.batchBudget = 100 * time.Millisecond

	var items []pipeline.EffectItem
	for i, id := range []string{"id-a", "id-b", "id-c"} {
		k := model.Key{SessionID: id, PID: 100 + i}
		s := model.Session{Key: k, PID: 100 + i}
		items = append(items, pipeline.EffectItem{Key: k, Published: s,
			Effect: notify.Note{Session: s, At: time.Now(), Decision: act.Decision{Allowed: true, Rule: "test"}}})
	}
	done := make(chan []model.Problem, 1)
	start := time.Now()
	go func() { done <- acts.onEffects(context.Background(), pipeline.EffectBatch{Items: items}) }()

	var problems []model.Problem
	select {
	case problems = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a burst of three notifications held the coordinator for 5s and counting: the batch has no bound")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the batch took %v against a %v budget", took, acts.batchBudget)
	}
	if n := len(port.calls); n != 1 {
		t.Errorf("%d deliveries were attempted, want 1: the rest came after the budget", n)
	}
	if got := trailCount(trail); got != 6 {
		t.Errorf("the trail holds %d lines, want 6: an attempt and an outcome for every note", got)
	}
	budgeted := 0
	for _, p := range problems {
		if p.Scope == model.ScopeHost && strings.Contains(p.Detail, "budget") {
			budgeted++
		}
	}
	if budgeted != 2 {
		t.Errorf("%d problems name the budget, want 2 (the notes it cut off): %v", budgeted, problems)
	}
}
