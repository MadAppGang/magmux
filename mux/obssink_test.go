package mux

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/internal/obs/registry"
	"github.com/MadAppGang/magmux/obsview"
)

// The sinks: the document magmux keeps, the three surfaces that render it, and
// the two lock rules that make a sink safe to attach to a hub at all.
//
// Nothing here reads ~/.claude, probes a pid or speaks to a real magmux.

// stagedFleet is one document with every interesting shape in it: a session
// that needs a human, one that does not, one nobody could reach a verdict
// about, one in a magmux pane and two not, a determined zero beside an
// undetermined count, and a conflict.
//
// It is built as a value rather than read from a registry because the subject
// under test is the RENDERING, and a staged document is the only way all three
// surfaces can be shown the identical input.
func stagedFleet() obs.Fleet {
	since := func(d time.Duration) *time.Time {
		t := obsEpoch.Add(-d)
		return &t
	}
	return obs.Fleet{
		Epoch: obsEpoch,
		Sessions: []obs.Session{
			{
				ID: "a1478d55-6e02-4bb9-93cf-01d2e8f47a6b", PID: 4242, Name: "worktree-a",
				PaneID:        obs.Ptr(1),
				Status:        obs.StatusWaiting,
				Waiting:       obs.WaitingReasonInput,
				StatusSince:   since(9 * time.Minute),
				Confidence:    obs.TierAuthoritative,
				ProcessAlive:  obs.LivenessYes,
				PaneAlive:     obs.LivenessYes,
				SubagentCount: obs.Ptr(3),
				Fault:         obs.Fault{APIErrors: obs.Ptr(0)},
				Attention:     obs.Attention{Needed: obs.Ptr(true), Reason: "it is waiting for input"},
			},
			{
				ID: "c874de20-33c1-480a-ba90-8ae3f2f332c4", PID: 98198, Name: "worktree-b",
				Status:       obs.StatusBusy,
				StatusSince:  since(31 * time.Minute),
				Confidence:   obs.TierAuthoritative,
				ProcessAlive: obs.LivenessYes,
				Attention:    obs.Attention{Needed: obs.Ptr(false), Reason: "it is working"},
			},
			{
				ID: "d0c31e88-4f17-4a52-8e6d-b9a70c55f213", PID: 1724,
				Status:       obs.StatusBusy,
				StatusSince:  since(45 * time.Minute),
				Confidence:   obs.TierAuthoritative,
				ProcessAlive: obs.LivenessUnknown,
				Conflict: &obs.Conflict{
					Tier:   obs.TierInferred,
					Status: obs.StatusIdle,
					Detail: "the pane has produced nothing for 45m",
				},
				Attention: obs.Attention{Reason: "the two tiers disagree"},
			},
		},
	}
}

// TestTheThreeSurfacesAgree is requirements.md R4.1 and R4.3 as one assertion:
// the socket verb, the MCP tool and the panel are three renderings of ONE
// document, and none of them may say something about a session that another
// contradicts.
//
// It compares FACTS rather than bytes, deliberately. The three surfaces are a
// JSON object, a text table and a 34-column ANSI frame; a byte comparison is
// impossible and a comparison of three hand-written expectations would prove
// only that somebody copied the same table three times. What is asserted is
// that each surface's own rendering of each session round-trips back to the
// same values.
func TestTheThreeSurfacesAgree(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()
	fleet := stagedFleet()

	// ── surface 1: the socket verb ──────────────────────────────────────
	m := newTestMux(t, ctrlPanes(2)...)
	m.obsDoc.start()
	m.obsDoc.set(fleet.Clone(), obsEpoch, 0)
	payload, err := m.sockObs()
	if err != nil {
		t.Fatalf("the obs verb: %v", err)
	}
	// The payload IS the document: it decodes back into an obs.Fleet whose
	// every field equals the one that went in. That is the property R4.1's
	// "identical document" needs, and it is what a hand-built map would break.
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("re-encoding the reply: %v", err)
	}
	var overWire obs.Fleet
	if err := json.Unmarshal(raw, &overWire); err != nil {
		t.Fatalf("decoding the reply as a document: %v", err)
	}
	if !reflect.DeepEqual(overWire, fleet) {
		t.Fatalf("the socket document differs from the one published:\n got %+v\nwant %+v",
			overWire, fleet)
	}
	// And it carries NOTHING else. An extra key here is a second schema in the
	// one place R4.1 names.
	if len(payload) != 2 || payload["epoch"] == nil || payload["sessions"] == nil {
		t.Errorf("the reply carries %d keys (%v); the document has exactly epoch and sessions",
			len(payload), keysOfPayload(payload))
	}

	// ── surface 2: the MCP tool's rendering ─────────────────────────────
	// The tool decodes the wire into obs.Fleet and renders it with the CLI's
	// own writeFleetTable, so what is exercised here is that decode-and-render
	// path over the document that actually crossed the wire.
	var mcpOut strings.Builder
	obsview.WriteFleetTable(&mcpOut, overWire)
	mcpText := mcpOut.String()

	// ── surface 3: the panel ────────────────────────────────────────────
	view := attentionOf(fleet.Clone(), obsEpoch, pipeline.Delivery{})
	cp := newControlPanel()
	cp.setAttention(view)
	cp.mu.Lock()
	snap := cp.snapshotLocked()
	cp.mu.Unlock()
	panelText := stripANSI(strings.Join(cp.frameAttention(snap, 60, 40), "\n"))

	// Logged, not just asserted. `go test -v -run TestTheThreeSurfacesAgree`
	// then shows all three renderings of one document side by side, which is
	// the only way a person can check the agreement this test checks
	// mechanically — and the only way a reviewer can see that the panel is
	// legible at all.
	t.Logf("the socket verb replies with:\n%s", string(raw))
	t.Logf("the MCP tool renders:\n%s", mcpText)
	t.Logf("the panel renders:\n%s", panelText)

	// ── the agreement ───────────────────────────────────────────────────
	for _, s := range fleet.Sessions {
		name := s.Name
		if name == "" {
			name = "pid " + strconv.Itoa(s.PID)
		}
		t.Run(name, func(t *testing.T) {
			// Every surface identifies the session. The two do it with
			// different keys and that is not a disagreement: the table has a
			// PID column and the panel is 30 columns wide and leads with the
			// name, falling back to the pid for a session that has none.
			pidText := strconv.Itoa(s.PID)
			if !strings.Contains(mcpText, pidText) {
				t.Errorf("the MCP rendering does not name pid %s", pidText)
			}
			if !strings.Contains(panelText, name) {
				t.Errorf("the panel does not name %q:\n%s", name, panelText)
			}

			// The STATUS word is one word in all three.
			want := obsview.StatusText(s)
			if !strings.Contains(mcpText, want) {
				t.Errorf("the MCP rendering does not say %q for %s", want, name)
			}
			if !strings.Contains(panelText, want) {
				t.Errorf("the panel does not say %q for %s", want, name)
			}

			// The ATTENTION verdict is the same verdict, in each surface's own
			// three spellings. The panel uses marks because it is 30 columns
			// wide; what must not differ is WHICH of the three it is.
			row := panelRowFor(panelText, name)
			mcpRow := tableRowFor(mcpText, pidText)
			switch {
			case s.Attention.Needed == nil:
				if !strings.Contains(mcpRow, "?") {
					t.Errorf("the MCP rendering does not spell %s undetermined: %q", name, mcpRow)
				}
				if !strings.HasPrefix(strings.TrimSpace(row), "?") {
					t.Errorf("the panel does not spell %s undetermined: %q", name, row)
				}
			case *s.Attention.Needed:
				if !strings.Contains(mcpRow, "YES") {
					t.Errorf("the MCP rendering does not say %s needs a human: %q", name, mcpRow)
				}
				if !strings.HasPrefix(strings.TrimSpace(row), "●") {
					t.Errorf("the panel does not mark %s as needing a human: %q", name, row)
				}
			default:
				if !strings.Contains(mcpRow, " no ") {
					t.Errorf("the MCP rendering does not say %s is fine: %q", name, mcpRow)
				}
				if !strings.HasPrefix(strings.TrimSpace(row), "·") {
					t.Errorf("the panel does not mark %s as fine: %q", name, row)
				}
			}

			// A conflict appears in every surface that shows reasons at all.
			if s.Conflict != nil {
				if !strings.Contains(mcpText, "conflict") {
					t.Errorf("the MCP rendering hides %s's conflict", name)
				}
				if !strings.Contains(panelText, "conflict") {
					t.Errorf("the panel hides %s's conflict:\n%s", name, panelText)
				}
			}
		})
	}

	// The one number all three agree on, said out loud: how many need a human.
	if view.needed != 1 || view.unsure != 1 || view.total != 3 {
		t.Errorf("the panel counted %d needed / %d unsure / %d total, want 1/1/3",
			view.needed, view.unsure, view.total)
	}
}

// TestTheAttentionViewLeadsWithWhatNeedsAHuman: the panel shows a dozen rows of
// thirty, so the ORDER decides what is visible at all.
func TestTheAttentionViewLeadsWithWhatNeedsAHuman(t *testing.T) {
	fleet := stagedFleet()
	view := attentionOf(fleet, obsEpoch, pipeline.Delivery{})
	if len(view.rows) != 3 {
		t.Fatalf("%d rows, want 3", len(view.rows))
	}
	if view.rows[0].needed != attnYes {
		t.Errorf("the first row is %v, want the session that needs a human", view.rows[0].needed)
	}
	if view.rows[1].needed != attnUnknown {
		t.Errorf("the second row is %v, want the undetermined one — \"?\" is not a no and must "+
			"not sort with the sessions nobody has to look at", view.rows[1].needed)
	}
	if view.rows[2].needed != attnNo {
		t.Errorf("the third row is %v, want the one that is fine", view.rows[2].needed)
	}
}

// busyFleet is thirty sessions: ten that need a human, each with a reason and
// therefore a detail line under its row, five nobody could decide about, and
// fifteen that are fine. It is the machine the attention view is FOR, and the
// shape that overflowed it.
func busyFleet() obs.Fleet {
	since := obsEpoch.Add(-time.Hour)
	var sessions []obs.Session
	for i := 0; i < 30; i++ {
		s := obs.Session{
			ID:          fmt.Sprintf("id-%02d", i),
			PID:         71000 + i,
			Name:        fmt.Sprintf("sess-%02d", i),
			Status:      obs.StatusIdle,
			StatusSince: obs.Ptr(since.Add(time.Duration(i) * time.Minute)),
			Attention:   obs.Attention{Needed: obs.Ptr(false), Reason: "its own record says it is idle"},
		}
		switch {
		case i < 10:
			s.Status, s.Waiting = obs.StatusWaiting, obs.WaitingReasonInput
			s.Attention = obs.Attention{Needed: obs.Ptr(true), Reason: "it is waiting for input"}
		case i < 15:
			s.Attention = obs.Attention{Reason: "nothing known about it"}
		}
		sessions = append(sessions, s)
	}
	return obs.Fleet{Epoch: obsEpoch, Sessions: sessions}
}

// attentionScreen is what a person sees: the frame, clipped exactly as
// paintFrame clips it, as plain text.
func attentionScreen(cp *ControlPanel, w, h int) []string {
	cp.mu.Lock()
	snap := cp.snapshotLocked()
	cp.mu.Unlock()
	lines := cp.frameAttention(snap, w, h)
	if len(lines) > h {
		lines = lines[:h]
	}
	for i := range lines {
		lines[i] = stripANSI(lines[i])
	}
	return lines
}

// visibleSessions is which of busyFleet's sessions are on screen.
func visibleSessions(screen []string) map[string]bool {
	out := map[string]bool{}
	for _, line := range screen {
		for i := 0; i < 30; i++ {
			if name := fmt.Sprintf("sess-%02d", i); strings.Contains(line, name) {
				out[name] = true
			}
		}
	}
	return out
}

// countAfter reads the number in a "▲ N above" / "▼ N more" line, or -1.
func countAfter(screen []string, glyph string) int {
	for _, line := range screen {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, glyph+" ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if n, err := strconv.Atoi(fields[1]); err == nil {
			return n
		}
	}
	return -1
}

// TestTheAttentionViewFitsItsPaneAndScrollsToTheLastSession — fix round 1,
// item 4.
//
// Thirty sessions on a 24-row pane. The view budgeted in ROWS while every row
// that needs a human paints a second line, so it returned 33 lines; paintFrame
// kept 24, the footer went first and the "+N more" after it, and the pane
// showed eleven sessions with nothing to say nineteen more existed. It never
// read the scroll offset either, so the j/k its own footer advertised could not
// reach them.
//
// Live validation missed it because it ran on a 50-row pane with 36 sessions,
// which happens to fit. So this is at 24 rows, and the keys are pressed through
// consumeControlKey — the handler a person's keystrokes reach — not by setting
// the offset.
func TestTheAttentionViewFitsItsPaneAndScrollsToTheLastSession(t *testing.T) {
	const w, h = 60, 24
	m := newTestMux(t, ctrlPanes(1)...)
	cp := m.control
	view := attentionOf(busyFleet(), obsEpoch, pipeline.Delivery{})
	cp.setAttention(view)
	cp.setView(viewAttention)
	first, last := view.rows[0].name, view.rows[len(view.rows)-1].name
	press := func(key string) {
		t.Helper()
		if n := m.consumeControlKey([]byte(key)); n == 0 {
			t.Fatalf("the panel did not consume %q", key)
		}
	}
	check := func(screen []string, where string) (above, shown, below int) {
		t.Helper()
		if len(screen) == 0 || !strings.Contains(screen[0], "ATTENTION") {
			t.Fatalf("%s: the header is not on screen:\n%s", where, strings.Join(screen, "\n"))
		}
		if !strings.Contains(screen[len(screen)-1], "j/k: scroll") {
			t.Fatalf("%s: the footer is not on screen — its last line is %q:\n%s",
				where, screen[len(screen)-1], strings.Join(screen, "\n"))
		}
		above, below = maxInt(countAfter(screen, "▲"), 0), maxInt(countAfter(screen, "▼"), 0)
		shown = len(visibleSessions(screen))
		if above+shown+below != len(view.rows) {
			t.Fatalf("%s: %d above + %d shown + %d below = %d, want all %d sessions accounted for:\n%s",
				where, above, shown, below, above+shown+below, len(view.rows), strings.Join(screen, "\n"))
		}
		return above, shown, below
	}

	// The frame itself, before paintFrame clips anything.
	cp.mu.Lock()
	snap := cp.snapshotLocked()
	cp.mu.Unlock()
	if n := len(cp.frameAttention(snap, w, h)); n > h {
		t.Fatalf("the attention view built %d lines for a %d-row pane; everything past %d is cut, "+
			"footer first", n, h, h)
	}

	top := attentionScreen(cp, w, h)
	above, shown, below := check(top, "at the top")
	if above != 0 || below == 0 || !visibleSessions(top)[first] {
		t.Fatalf("at the top: %d above, %d below, first session shown %v", above, below,
			visibleSessions(top)[first])
	}
	t.Logf("at the top, %d shown and %d counted below:\n%s", shown, below, strings.Join(top, "\n"))

	// j, until the last session is on screen, and every session on the way is
	// seen at least once.
	seen := visibleSessions(top)
	reached := false
	for i := 0; i < 2*len(view.rows); i++ {
		press("j")
		screen := attentionScreen(cp, w, h)
		check(screen, fmt.Sprintf("after %d j", i+1))
		for name := range visibleSessions(screen) {
			seen[name] = true
		}
		if visibleSessions(screen)[last] {
			reached = true
			if countAfter(screen, "▼") > 0 {
				t.Errorf("the last session is on screen and a count below it says more follow")
			}
			t.Logf("after %d j:\n%s", i+1, strings.Join(screen, "\n"))
			break
		}
	}
	if !reached {
		t.Fatalf("j never brought the last session (%s) on screen", last)
	}
	if len(seen) != len(view.rows) {
		t.Errorf("scrolling showed %d of %d sessions", len(seen), len(view.rows))
	}

	// Past the end is clamped, not an empty view.
	for i := 0; i < 50; i++ {
		press("j")
	}
	if !visibleSessions(attentionScreen(cp, w, h))[last] {
		t.Error("scrolling past the end moved the last session off screen")
	}

	// k goes back up, g to the top, G to the bottom.
	press("k")
	check(attentionScreen(cp, w, h), "after k")
	press("g")
	if screen := attentionScreen(cp, w, h); !visibleSessions(screen)[first] || countAfter(screen, "▲") > 0 {
		t.Errorf("g did not return to the top:\n%s", strings.Join(screen, "\n"))
	}
	press("G")
	if screen := attentionScreen(cp, w, h); !visibleSessions(screen)[last] {
		t.Errorf("G did not reach the last session:\n%s", strings.Join(screen, "\n"))
	}
}

// TestTheAttentionViewNeverOverrunsItsPane is the same property at every height
// from the smallest that holds one session and both counts, at every offset:
// the frame fits, the footer is its last line, and every session is either on
// screen or counted.
func TestTheAttentionViewNeverOverrunsItsPane(t *testing.T) {
	view := attentionOf(busyFleet(), obsEpoch, pipeline.Delivery{})
	for h := 9; h <= 60; h++ {
		cp := newControlPanel()
		cp.setAttention(view)
		cp.setView(viewAttention)
		for off := 0; off <= len(view.rows)+2; off++ {
			cp.mu.Lock()
			cp.scroll = off
			snap := cp.snapshotLocked()
			cp.mu.Unlock()
			lines := cp.frameAttention(snap, 60, h)
			if len(lines) > h {
				t.Fatalf("h=%d offset=%d: %d lines", h, off, len(lines))
			}
			screen := make([]string, len(lines))
			for i := range lines {
				screen[i] = stripANSI(lines[i])
			}
			if !strings.Contains(screen[len(screen)-1], "j/k: scroll") {
				t.Fatalf("h=%d offset=%d: the footer is not the last line", h, off)
			}
			above, below := maxInt(countAfter(screen, "▲"), 0), maxInt(countAfter(screen, "▼"), 0)
			if shown := len(visibleSessions(screen)); shown == 0 || above+shown+below != len(view.rows) {
				t.Fatalf("h=%d offset=%d: %d above + %d shown + %d below, want %d with at least one shown:\n%s",
					h, off, above, shown, below, len(view.rows), strings.Join(screen, "\n"))
			}
		}
	}
	// Below nine rows the footer is still kept.
	for h := 3; h < 9; h++ {
		cp := newControlPanel()
		cp.setAttention(view)
		cp.setView(viewAttention)
		cp.mu.Lock()
		snap := cp.snapshotLocked()
		cp.mu.Unlock()
		lines := cp.frameAttention(snap, 60, h)
		if len(lines) > h || !strings.Contains(stripANSI(lines[len(lines)-1]), "j/k: scroll") {
			t.Errorf("h=%d: %d lines, last %q", h, len(lines), stripANSI(lines[len(lines)-1]))
		}
	}
}

// TestAnEmptyDocumentAndNoDocumentAreDifferentAnswers, in the panel. "No
// sessions on this machine" and "the layer has not published anything" send a
// person to opposite places, and a view that showed one blank list for both
// would be the collapse this whole layer refuses everywhere else.
func TestAnEmptyDocumentAndNoDocumentAreDifferentAnswers(t *testing.T) {
	cp := newControlPanel()

	cp.mu.Lock()
	snap := cp.snapshotLocked()
	cp.mu.Unlock()
	never := stripANSI(strings.Join(cp.frameAttention(snap, 60, 20), "\n"))
	if !strings.Contains(never, "not running") {
		t.Errorf("a panel that has received nothing does not say so:\n%s", never)
	}

	cp.setAttention(attentionOf(obs.Fleet{Epoch: obsEpoch}, obsEpoch, pipeline.Delivery{}))
	cp.mu.Lock()
	snap = cp.snapshotLocked()
	cp.mu.Unlock()
	empty := stripANSI(strings.Join(cp.frameAttention(snap, 60, 20), "\n"))
	if !strings.Contains(empty, "no Claude Code sessions") {
		t.Errorf("a panel holding an empty document does not say so:\n%s", empty)
	}
	if strings.Contains(empty, "not running") {
		t.Errorf("an empty document was reported as the layer being off:\n%s", empty)
	}
}

// TestTheAttentionViewIsNotPartOfTheWireTap is CLAUDE.md's provenance rule,
// asserted rather than trusted.
//
// The panel records ONE exchange: what a controller asked of a session (▶ OUT)
// and what magmux observed the session do (◀ IN). An attention document is
// neither, and a sink that recorded it into the stream would make `done` count
// magmux's own bookkeeping — the exact defect recordObserved exists to prevent.
func TestTheAttentionViewIsNotPartOfTheWireTap(t *testing.T) {
	cp := newControlPanel()
	cp.recordStart(0, "a goal", "model", "client", 2)
	before := cp.digest()
	cp.mu.Lock()
	signalsBefore := len(cp.signals)
	cp.mu.Unlock()

	for i := 0; i < 5; i++ {
		cp.setAttention(attentionOf(stagedFleet(), obsEpoch, pipeline.Delivery{}))
	}
	after := cp.digest()

	if after.sent != before.sent || after.observed != before.observed {
		t.Errorf("five documents moved the panel's counters from %d/%d to %d/%d; the attention "+
			"view is not part of the controller↔session exchange and must not touch them",
			before.sent, before.observed, after.sent, after.observed)
	}
	cp.mu.Lock()
	signals, routes := len(cp.signals), len(cp.routes)
	cp.mu.Unlock()
	if routes != 1 {
		t.Errorf("the documents opened %d routes, want only the controller's 1", routes)
	}
	if signals != signalsBefore {
		t.Errorf("the documents appended to the signal stream: %d entries, was %d. `▶ OUT` is "+
			"what a controller asked for and `◀ IN` is what magmux observed a session do; an "+
			"attention row is neither.", signals, signalsBefore)
	}
}

// TestTheSinkNeverHoldsALockAcrossTheHub is the deliberate break the phase
// brief names first, as a test rather than as a comment.
//
// architecture-v4 §2: the hub never blocks on a subscriber, which is what
// makes it safe for a subscriber to block on the hub. A sink that holds cp.mu
// while parked on its subscription breaks that from the other side — cp.mu is
// taken by the render loop under treeMu.RLock, so every frame magmux draws
// stops until the next document arrives, which on a quiet machine is never.
//
// The assertion is therefore about LATENCY under a sink that is parked: a
// digest, which is exactly what renderLocked takes, must complete immediately
// while the sink is waiting for its next document.
func TestTheSinkNeverHoldsALockAcrossTheHub(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	hub := pipeline.StartHub()
	defer hub.Stop()
	sub, err := hub.Subscribe("chrome", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	m := newTestMux(t, ctrlPanes(1)...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.obsSinkLoop(sub)
	}()

	// One document, so the sink has run its body at least once and is now
	// parked on the receive — which is the state the break would poison.
	hub.Publish(stagedFleet(), nil)
	deadline := time.Now().Add(10 * time.Second)
	for m.control.digest().active || !m.control.hasAttention() {
		if time.Now().After(deadline) {
			t.Fatal("the sink never stored a document")
		}
		time.Sleep(time.Millisecond)
	}

	// The render loop's own read, twice, while the sink is parked. With cp.mu
	// held across the receive this blocks for ever and the watchdog above
	// names the goroutine holding it.
	for i := 0; i < 2; i++ {
		start := time.Now()
		_ = m.control.digest()
		_ = m.control.hasAttention()
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("reading the panel took %v while the sink was waiting on the hub; the sink "+
				"is holding cp.mu across a channel receive", took)
		}
	}

	hub.Unsubscribe(sub)
	<-done
}

// TestTheSinkNeverTakesTreeMu is the second lock rule, and the one that is a
// DEADLOCK rather than a stall.
//
// CLAUDE.md: the documented order is treeMu -> cp.mu and the reverse is never
// legal. renderLocked holds treeMu.RLock for its whole body and takes cp.mu
// inside it; a sink that took cp.mu and then wanted treeMu would close the
// cycle, and the failure is a silent hang with no race report.
//
// A source scan is the only way to assert an ABSENCE like this: a behavioural
// test passes on a build where the sink takes treeMu and simply never
// deadlocks because no writer queued during the window.
//
// It walks the AST rather than grepping, which is the lesson termev's ticker
// detector already learned twice in this repository: these two files are full
// of PROSE about treeMu — the whole point of them is that they explain why
// they do not take it — and a text search flags every sentence.
func TestTheSinkNeverTakesTreeMu(t *testing.T) {
	for _, file := range []string{"obssink.go", "obspanel.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0) // comments dropped
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != "treeMu" {
				return true
			}
			found = true
			t.Errorf("%s names treeMu in CODE. The sink runs with cp.mu and nothing else: "+
				"taking treeMu from a goroutine that holds cp.mu closes the cycle "+
				"renderLocked opens, and the failure is a silent hang with no race report.",
				fset.Position(id.Pos()))
			return true
		})
		if found {
			continue
		}
		// Non-vacuity: the files must still be the ones this is about. A test
		// that scanned an empty or renamed file would pass and prove nothing.
		if len(f.Decls) == 0 {
			t.Errorf("%s has no declarations; this scan is checking a file that is not there", file)
		}
	}
}

// TestTheStoreTellsOffApartFromNothingYet: two states with different remedies —
// restart magmux with MAGMUX_AGENTS, versus ask again in a moment.
func TestTheStoreTellsOffApartFromNothingYet(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)

	if _, err := m.sockObs(); verbErrCode(err) != sockCodeObsOff {
		t.Errorf("a magmux with observation off answered %v (%q), want %s",
			err, verbErrCode(err), sockCodeObsOff)
	}

	m.obsDoc.start()
	if _, err := m.sockObs(); verbErrCode(err) != sockCodeObsPending {
		t.Errorf("a magmux with observation running and nothing published answered %v (%q), "+
			"want %s", err, verbErrCode(err), sockCodeObsPending)
	}

	m.obsDoc.set(stagedFleet(), obsEpoch, 0)
	if _, err := m.sockObs(); err != nil {
		t.Errorf("a magmux with a document answered %v", err)
	}

	// And stopping DROPS it: a stopped pipeline's last document keeps ageing
	// and nothing will ever correct it.
	m.obsDoc.stop()
	if _, err := m.sockObs(); verbErrCode(err) != sockCodeObsOff {
		t.Errorf("after stopping, the verb answered %v (%q), want %s",
			err, verbErrCode(err), sockCodeObsOff)
	}
}

// TestTheSinkKeepsItsOwnCopy is freeze point 3 at magmux's boundary: the
// obs.Fleet in a Delivery is shared with every other subscriber of that
// broadcast, so a sink that retained it would be holding something another
// sink may be reading — and would see it change underneath the socket verb.
func TestTheSinkKeepsItsOwnCopy(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	hub := pipeline.StartHub()
	defer hub.Stop()
	sub, _ := hub.Subscribe("chrome", 0)
	m := newTestMux(t, ctrlPanes(1)...)
	done := make(chan struct{})
	go func() { defer close(done); m.obsSinkLoop(sub) }()

	fleet := stagedFleet()
	hub.Publish(fleet, nil)
	waitUntilObs(t, func() bool { return m.obsDoc.snapshot().Have }, "the document to arrive")

	// Mutate through the publisher's own pointers, as a careless middleware
	// would. Publish already deep-copied once; this proves the SINK's copy is
	// independent of that copy too.
	*fleet.Sessions[0].Attention.Needed = false
	fleet.Sessions[0].Name = "renamed"

	got := m.obsDoc.snapshot().Doc
	if got.Sessions[0].Name != "worktree-a" {
		t.Errorf("the stored document followed a mutation: name is %q", got.Sessions[0].Name)
	}
	if got.Sessions[0].Attention.Needed == nil || !*got.Sessions[0].Attention.Needed {
		t.Errorf("the stored document's verdict followed a mutation to %v",
			got.Sessions[0].Attention.Needed)
	}

	hub.Unsubscribe(sub)
	<-done
}

// TestTheSinkSurvivesADocumentItCannotKeepUpWith: the hub drops for a slow
// sink rather than blocking, and the drop is carried on the next delivery. The
// sink must report it rather than swallow it — a consumer counting changes
// would otherwise be short and would never know.
func TestTheSinkCarriesTheDropCount(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	m.obsDoc.start()
	m.obsDoc.set(stagedFleet(), obsEpoch, 4)
	if got := m.obsDoc.snapshot().Dropped; got != 4 {
		t.Errorf("the store forgot %d drops", 4-got)
	}
	view := attentionOf(stagedFleet(), obsEpoch, pipeline.Delivery{Dropped: 4})
	cp := newControlPanel()
	cp.setAttention(view)
	cp.mu.Lock()
	snap := cp.snapshotLocked()
	cp.mu.Unlock()
	if !strings.Contains(stripANSI(strings.Join(cp.frameAttention(snap, 60, 20), "\n")), "4 dropped") {
		t.Error("the panel does not say it fell behind")
	}
}

// TestTheViewCyclesAndTheZeroValueIsTheWireTap. Every test in this repository
// builds a ControlPanel as a literal, so the zero value has to be the panel
// that predates the second view.
func TestTheViewCyclesAndTheZeroValueIsTheWireTap(t *testing.T) {
	cp := newControlPanel()
	cp.mu.Lock()
	zero := cp.view
	cp.mu.Unlock()
	if zero != viewExchange {
		t.Fatalf("the zero view is %v, want the exchange", zero)
	}

	cp.cycleView(1)
	cp.mu.Lock()
	next := cp.view
	cp.mu.Unlock()
	if next != viewAttention {
		t.Fatalf("v moved to %v, want the attention view", next)
	}
	cp.cycleView(1)
	cp.mu.Lock()
	back := cp.view
	cp.mu.Unlock()
	if back != viewExchange {
		t.Fatalf("v again moved to %v, want back to the exchange", back)
	}
	// And backwards, without going out of range.
	cp.cycleView(-1)
	cp.mu.Lock()
	rev := cp.view
	cp.mu.Unlock()
	if rev != viewAttention {
		t.Fatalf("v backwards moved to %v", rev)
	}
}

// TestCtrlGAIsOnlyAdvertisedWhenThereIsSomethingToShow: the observation layer
// is opt-in, so on most magmuxes the key would reveal a panel saying it has
// nothing. Advertising a key that does nothing teaches the wrong thing about
// the feature — the same rule "[ back" already follows.
func TestCtrlGAIsOnlyAdvertisedWhenThereIsSomethingToShow(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	m.treeMu.RLock()
	before := keyHintList(m.keyHintLocked(), 0)
	m.treeMu.RUnlock()
	if strings.Contains(before, "attention") {
		t.Errorf("the attention key is advertised with no document: %q", before)
	}

	m.control.setAttention(attentionOf(stagedFleet(), obsEpoch, pipeline.Delivery{}))
	m.treeMu.RLock()
	after := keyHintList(m.keyHintLocked(), 0)
	m.treeMu.RUnlock()
	if !strings.Contains(after, "attention") {
		t.Errorf("the attention key is not advertised with a document: %q", after)
	}
}

// TestTheAttentionKeyShowsThePanelWithoutStealingFocus.
//
// CLAUDE.md: showing the panel must not steal focus — a human is typing into
// their agent, and an instrument that takes the keyboard mid-sentence is worse
// than not having the instrument.
func TestTheAttentionKeyShowsThePanelWithoutStealingFocus(t *testing.T) {
	m := newTestMux(t, PaneConfig{Control: true}, PaneConfig{Control: true})
	// Make the second pane the panel and hide it, which is what a magmux
	// started without -c looks like.
	panel := m.paneByID(1)
	panel.isControl = true
	m.control.attach(panel)
	m.treeMu.Lock()
	m.panel = panel
	m.hidePanelLocked(panel)
	m.focused = m.paneByIDLocked(0)
	m.treeMu.Unlock()

	m.showAttentionView()

	m.treeMu.RLock()
	hidden, focused := panel.hidden, m.focused
	m.treeMu.RUnlock()
	if hidden {
		t.Error("ctrl-g a left the panel hidden")
	}
	if focused == nil || focused.id != 0 {
		t.Errorf("ctrl-g a moved focus to pane %v; showing the panel must not take the keyboard",
			focused)
	}
	m.control.mu.Lock()
	view := m.control.view
	m.control.mu.Unlock()
	if view != viewAttention {
		t.Errorf("ctrl-g a left the panel on view %v", view)
	}
}

// TestTheSinkReportsProblemsRatherThanSwallowingThem: a source that silently
// skips a record it could not read is indistinguishable from a machine with
// fewer sessions on it, and the difference between those two is the entire
// value of the report.
func TestTheSinkReportsProblemsRatherThanSwallowingThem(t *testing.T) {
	view := attentionOf(stagedFleet(), obsEpoch, pipeline.Delivery{
		Problems: []registry.Problem{{Path: "/tmp/x.json", Kind: registry.ProblemUnreadable}},
	})
	if view.problems != 1 {
		t.Fatalf("the view counted %d problems, want 1", view.problems)
	}
	cp := newControlPanel()
	cp.setAttention(view)
	cp.mu.Lock()
	snap := cp.snapshotLocked()
	cp.mu.Unlock()
	if !strings.Contains(stripANSI(strings.Join(cp.frameAttention(snap, 60, 20), "\n")), "unreadable") {
		t.Error("the panel does not say a record could not be read")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func keysOfPayload(p map[string]any) []string {
	var out []string
	for k := range p {
		out = append(out, k)
	}
	return out
}

// panelRowFor finds the line of a rendered panel that names a session.
func panelRowFor(frame, name string) string {
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	return ""
}

// tableRowFor is the same for a tabwriter table.
func tableRowFor(table, name string) string {
	for _, line := range strings.Split(table, "\n") {
		if strings.Contains(line, name) {
			return line
		}
	}
	return ""
}

// waitUntil blocks until cond holds, naming what it waited for when it does
// not.
func waitUntilObs(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// stripANSI removes SGR sequences so a frame can be asserted on as text.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' && s[i] != 'H' && s[i] != 'J' {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TestThePanelIsSafeUnderASinkAndARenderLoopAtOnce is the -race exercise the
// two lock rules are really about — and the THIRD goroutine in it is the whole
// reason it works.
//
// The sink writing while the render loop reads is not enough to catch a lock
// inversion. cp.mu -> treeMu.RLock and treeMu.RLock -> cp.mu do not deadlock
// on their own: two read locks do not block each other, so the cycle never
// closes. It closes the moment a treeMu WRITER queues between them — Go's
// RWMutex puts later readers behind a waiting writer, which is the exact
// mechanism CLAUDE.md names ("a second RLock on one goroutine deadlocks if a
// writer queued between them, and the failure mode is a silent HANG, not a
// race report").
//
// magmux has such a writer on every keystroke, every SIGWINCH and every socket
// verb that moves a pane, so a test without one is testing a magmux nobody
// runs. This was found by deliberately inverting the order in setAttention and
// watching this test PASS.
func TestThePanelIsSafeUnderASinkAndARenderLoopAtOnce(t *testing.T) {
	defer deadlineWatchdog(t, 60*time.Second)()

	m := newTestMux(t, ctrlPanes(2)...)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The treeMu WRITER. Without it the inversion below cannot close.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.treeMu.Lock()
			m.markAllDirtyLocked()
			m.treeMu.Unlock()
		}
	}()

	// The sink.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.control.setAttention(attentionOf(stagedFleet(), obsEpoch, pipeline.Delivery{}))
		}
	}()

	// The render loop's two reads of the panel, in the order renderLocked
	// makes them: under treeMu.RLock, which is the legal direction.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			m.treeMu.RLock()
			_ = m.control.digest()
			_ = m.keyHintLocked()
			m.treeMu.RUnlock()
		}
	}()

	// And a frame being built from a snapshot, which is what render() does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			m.control.mu.Lock()
			snap := m.control.snapshotLocked()
			m.control.mu.Unlock()
			_ = m.control.frame(snap, nil, 60, 20)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	_ = fmt.Sprint(m.obsDoc.snapshot().Running)
}

// TestTheConfigFileIsActuallyInForce is the wiring R6.3's tests in
// internal/obs/act cannot see: a file on disk, read by the code magmux's entry
// points call, producing the policy the gate decides with.
//
// It exists because a deliberate break — a loader that kept what it read
// before a parse error — was caught by four tests in internal/obs/act and by
// NOTHING here. A config that is parsed correctly and then not used is
// indistinguishable, from inside that package, from one that is.
func TestTheConfigFileIsActuallyInForce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "magmux"), 0o700); err != nil {
		t.Fatalf("staging the config directory: %v", err)
	}
	path := filepath.Join(dir, "magmux", act.ConfigFileName)

	// 1. No file at all: the default posture, and NOT an error. An absent
	//    config is the normal state of an unconfigured machine.
	policy, err := loadObsPolicy()
	if err != nil {
		t.Fatalf("with no config file: %v", err)
	}
	if policy.Actions != act.Default().Actions {
		t.Errorf("with no config file the posture is %+v, want the default", policy.Actions)
	}

	// 2. A file the user wrote: in force, and naming itself as the authority.
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing the config: %v", err)
		}
	}
	write(`{"actions":{"navigate":"never"},"notify":{"enabled":true,"on":["waiting"],"delay":"90s"}}`)
	policy, err = loadObsPolicy()
	if err != nil {
		t.Fatalf("with a valid config: %v", err)
	}
	if policy.Actions.Navigate != act.StanceNever {
		t.Errorf("navigate is %v, want never — the file is not in force", policy.Actions.Navigate)
	}
	if !policy.Notify.Enabled || policy.Notify.Delay != 90*time.Second {
		t.Errorf("the notify block is %+v, want enabled with a 90s delay", policy.Notify)
	}
	if !strings.Contains(policy.Source, act.ConfigFileName) {
		t.Errorf("the policy's authority is %q and does not name the file", policy.Source)
	}

	// 3. A file the user mistyped: the DEFAULT posture, never a more
	//    permissive one, and the failure is reported.
	write(`{"actions":{"navigate":"always","approve":"always","send":"always"` + "\n")
	policy, err = loadObsPolicy()
	if err == nil {
		t.Fatal("an unparseable config was accepted silently; a config that is not in force " +
			"must say so, or nobody ever finds out their rules never applied")
	}
	if policy.Actions != act.Default().Actions {
		t.Fatalf("an unparseable config left the posture at %+v, want the default. R6.3 forbids "+
			"a MORE PERMISSIVE outcome and this file's permissive stanza parses cleanly before "+
			"the error", policy.Actions)
	}
}
