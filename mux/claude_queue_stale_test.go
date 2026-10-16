package mux

// r3 review M4: a queued prompt that is removed WITHOUT becoming a user entry
// or a queued_command attachment (restored to the box with Up or Esc, or a
// remove reason F11 never measured) must not keep dating the next identical
// prompt as old.

import (
	"testing"
	"time"
)

// freshSubmitIsTaken runs a fresh idle submit of sentFrance after the given
// transcript history and reports the acceptance verdict.
func freshSubmitIsTaken(t *testing.T, history ...[]byte) AcceptVerdict {
	t.Helper()
	p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
	g, _ := gatedClaudeOn(p)
	for _, l := range history {
		g.applyLine(l)
	}
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
	swapScreen(t, p, "s1-box-history", "empty-box-placeholder")
	g.applyLine(userLine(sentFrance, time.Now()))
	return g.InputAccepted(chk.Baseline)
}

func TestRemovedUnsentQueuedPromptDoesNotMisdateTheNextSend(t *testing.T) {
	old := time.Now().Add(-5 * time.Minute)
	for _, c := range []struct {
		name    string
		history [][]byte
	}{
		{"remove reason edited, with content", [][]byte{
			enqueueLine(sentFrance, old), removeLine(sentFrance, "edited", old.Add(time.Second))}},
		{"remove with no content and no reason", [][]byte{
			enqueueLine(sentFrance, old), removeLine("", "", old.Add(time.Second))}},
		{"F11: enqueue, remove, queued_command attachment", [][]byte{
			enqueueLine(sentFrance, old), removeLine("", "", old.Add(time.Second)), queuedAttachLine(sentFrance, old)}},
		{"F11: absorbed mid-turn", [][]byte{
			enqueueLine(sentFrance, old), removeLine(sentFrance, "absorbed_mid_turn", old.Add(time.Second)), queuedAttachLine(sentFrance, old)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := freshSubmitIsTaken(t, c.history...); got.State != AcceptTaken || got.Evidence != evidenceTranscript {
				t.Errorf("a fresh submit after a stale queue entry: %+v, want taken/transcript", got)
			}
		})
	}
}

// TestDequeuedPromptStillDatesItsOwnUserEntry: the r3 M2 rule is kept — a
// prompt dequeued and written as a user entry at once is dated by its
// enqueue, so an OLD queued prompt running now never confirms a new send.
func TestDequeuedPromptStillDatesItsOwnUserEntry(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
	g, _ := gatedClaudeOn(p)
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
	swapScreen(t, p, "s1-box-history", "empty-box-placeholder")
	enq := chk.Baseline.At.Add(-time.Minute) // queued BEFORE this send's verify
	now := time.Now()
	g.applyLine(enqueueLine(sentFrance, enq))
	g.applyLine(removeLine("", "", now))
	g.applyLine(userLine(sentFrance, now.Add(10*time.Millisecond)))
	if got := g.InputAccepted(chk.Baseline); got.State == AcceptTaken {
		t.Errorf("an older queued prompt's dequeued entry confirmed this send: %+v", got)
	}
}
