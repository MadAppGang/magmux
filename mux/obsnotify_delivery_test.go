package mux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/notify"
)

// oneNote is a batch of one allowed notification for session id-a.
func oneNote() pipeline.EffectBatch {
	k := model.Key{SessionID: "id-a", PID: 100}
	s := model.Session{Key: k, PID: 100}
	return pipeline.EffectBatch{Items: []pipeline.EffectItem{{Key: k, Published: s,
		Effect: notify.Note{Session: s, At: time.Now(), Decision: act.Decision{Allowed: true, Rule: "test"}}}}}
}

// hostProblemsNaming counts the ScopeHost problems for id-a whose detail
// contains want.
func hostProblemsNaming(ps []model.Problem, want string) int {
	n := 0
	for _, p := range ps {
		if p.Scope == model.ScopeHost && p.Key != nil && p.Key.SessionID == "id-a" && strings.Contains(p.Detail, want) {
			n++
		}
	}
	return n
}

// A notification that was decided, recorded and then NOT delivered is a
// problem every sink sees, not only a line in the trail: the episode is
// latched, so nobody would ever be told otherwise. The budget-cut notes
// already said so; the delivery that failed — often the one that spent the
// budget — did not (review r2 #5).
func TestAFailedDeliveryIsAProblemEverySinkSees(t *testing.T) {
	trail := trailPath(t)
	w, err := act.NewWriter(act.WriterConfig{Path: trail})
	if err != nil {
		t.Fatal(err)
	}
	acts := newObsActions(act.Default(), w, &recordingPort{err: errors.New("osascript: not available")}, nil)
	problems := acts.onEffects(context.Background(), oneNote())
	if hostProblemsNaming(problems, "osascript: not available") != 1 {
		t.Fatalf("a failed delivery raised no problem naming its error: %v", problems)
	}
	if got := trailCount(trail); got != 2 {
		t.Errorf("the trail holds %d lines, want the attempt and its not-done outcome", got)
	}
}

// The same for a process with no port at all: decided, recorded, not
// delivered, and said out loud.
func TestNoPortIsAProblemEverySinkSees(t *testing.T) {
	trail := trailPath(t)
	w, err := act.NewWriter(act.WriterConfig{Path: trail})
	if err != nil {
		t.Fatal(err)
	}
	acts := newObsActions(act.Default(), w, nil, nil)
	problems := acts.onEffects(context.Background(), oneNote())
	if hostProblemsNaming(problems, "deliver") != 1 {
		t.Fatalf("an undeliverable notification raised no problem: %v", problems)
	}
}
