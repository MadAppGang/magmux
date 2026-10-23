package mux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// stuckFocuser gives up on its caller's behalf: Focus cancels the request's
// context and then blocks until the test releases it, so the action is
// accepted by the actor and is still running when the caller stops waiting.
type stuckFocuser struct {
	cancel  context.CancelFunc
	release chan struct{}
}

func (f *stuckFocuser) Focus(int) (int, error) {
	f.cancel()
	<-f.release
	return 0, nil
}

// TestAnOutcomeUnknownWearsANotDoneShape PINS A LIMITATION, so that closing it
// FAILS a test rather than passing unnoticed (the same device as
// act's TestAReservedVerbWithNoSurfaceWearsANotificationsShape).
//
// When the caller's context ends after the actor has the focus, Ops.Call
// returns pipeline.ErrOutcomeUnknown, and Gate.Do files it as it files every
// actuator error: an outcome line with done:false and the error text, which
// says "it may have run". A reader keying on done reads "not done" about a
// focus that then completed. The trail has no third outcome, and giving it one
// is a schema decision (the orchestrator's, gates.log 2026-10-21: no schema
// change now). It is unreachable in production today: `magmux agents focus`, the one
// production caller, passes context.Background(), so its context never ends.
func TestAnOutcomeUnknownWearsANotDoneShape(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	focuser := &stuckFocuser{cancel: cancel, release: make(chan struct{})}
	trail, path := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{policy: act.Default(), audit: trail, locator: paneOne{}, focuser: focuser})
	t.Cleanup(func() { close(focuser.release) }) // before startHost's Stop
	firstHostDoc(t, p)

	_, err := acts.do(ctx, p, focusRequest("id-a"), keyByID("id-a"))
	if !errors.Is(err, pipeline.ErrOutcomeUnknown) {
		t.Fatalf("do returned %v, want pipeline.ErrOutcomeUnknown", err)
	}
	waitUntilObs(t, func() bool { return trailCount(path) >= 2 }, "the attempt and its outcome")
	lines := hostTrailLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("the trail holds %d lines, want 2:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], `"event":"outcome"`) || !strings.Contains(lines[1], `"done":false`) ||
		!strings.Contains(lines[1], "may have run") {
		t.Fatalf("an unknown outcome is no longer filed as done:false with \"may have run\" in its error. "+
			"If the trail gained a way to say unknown, that was the schema decision this test pins: update "+
			"CLAUDE.md's actuation invariants and this test together.\n%s", lines[1])
	}
}
