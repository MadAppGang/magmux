package main

// R3 — pane labels and the id guarantee.
//
// The guarantee, stated once so the tests below can pin it:
//
//	Panes created from -e receive ids 0..N-1 in ARGUMENT ORDER. magmux's own
//	panes (the control panel) are appended after them. Ids are permanent:
//	closing a pane leaves a tombstone and never renumbers.
//
// It must hold on BOTH panel paths, and they build the id table by different
// mechanisms: with -c the panel is appended to `commands` and becomes the last
// LEAF; without -c installHiddenPanel appends straight to m.allPanes after the
// layout is built. CLAUDE.md's "-c means start the panel VISIBLE, not add a
// panel" is exactly why only a test that runs both proves they agree.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPaneIDsFollowArgumentOrder is the guarantee, in process, across both
// panel paths and every small N.
func TestPaneIDsFollowArgumentOrder(t *testing.T) {
	for n := 1; n <= 5; n++ {
		for _, visiblePanel := range []bool{false, true} {
			t.Run(fmt.Sprintf("n%d_visiblePanel=%v", n, visiblePanel), func(t *testing.T) {
				m := &Magmux{rows: 40, cols: 160, gridMode: true,
					quit: make(chan struct{}), control: newControlPanel()}

				cfgs := make([]PaneConfig, n)
				for i := range cfgs {
					cfgs[i] = PaneConfig{Control: true, Label: fmt.Sprintf("pane-%d", i)}
				}
				if visiblePanel {
					cfgs = append(cfgs, PaneConfig{Control: true})
				}
				if err := m.buildGrid(cfgs); err != nil {
					t.Fatalf("buildGrid: %v", err)
				}
				if !visiblePanel {
					m.installHiddenPanel()
				}

				if len(m.allPanes) != n+1 {
					t.Fatalf("allPanes has %d entries, want %d sessions + 1 panel",
						len(m.allPanes), n)
				}
				for i := 0; i < n; i++ {
					p := m.allPanes[i]
					if p.id != i {
						t.Errorf("pane at slot %d has id %d", i, p.id)
					}
					if want := fmt.Sprintf("pane-%d", i); p.label != want {
						t.Errorf("pane %d has label %q, want %q (argument order was not preserved)",
							i, p.label, want)
					}
				}
				// The panel is LAST on both paths, which is the whole point.
				panel := m.allPanes[n]
				if panel.id != n {
					t.Errorf("the panel has id %d, want %d", panel.id, n)
				}
				if panel.label != "" {
					t.Errorf("the panel picked up a session label: %q", panel.label)
				}
			})
		}
	}
}

// TestUnlabelledPaneEmitsNoLabelKey. --label is purely additive: absent means
// Label is "", which means no `label` key anywhere. That property is what makes
// the change unable to affect an existing consumer, so it is asserted rather
// than assumed.
func TestUnlabelledPaneEmitsNoLabelKey(t *testing.T) {
	m := newTestMux(t, ctrlPanes(2)...)
	m.treeMu.RLock()
	res := m.buildPaneResultsLocked()
	m.treeMu.RUnlock()
	for _, r := range res {
		if _, ok := r["label"]; ok {
			t.Errorf("an unlabelled pane emitted a label key: %v", r)
		}
	}
}

// ── the live snapshot event (§4.4, the gap) ─────────────────────────────────

// stepController is a ToolController whose Poll answer changes once, so
// pollControllers sees a state transition and emits exactly one snapshot event.
type stepController struct{ polls int }

func (c *stepController) Name() string                  { return "step" }
func (c *stepController) Start(_ context.Context) error { return nil }
func (c *stepController) Stop() error                   { return nil }
func (c *stepController) Poll() (Snapshot, error) {
	c.polls++
	if c.polls == 1 {
		return Snapshot{State: CtrlStarting}, nil
	}
	return Snapshot{State: CtrlWorking, LastResponse: "hello"}, nil
}

// TestSnapshotEventCarriesTheLabel closes the one AC3.1 site that lacked it.
//
// `results` and the `list` verb (buildPaneResults verbatim) already carried the
// label; the live per-pane `snapshot` event did not. A client that resolved a
// pane by name at connect time would then fail to recognise the live events
// about that same pane, which is precisely the indirection --label exists to
// remove.
func TestSnapshotEventCarriesTheLabel(t *testing.T) {
	m := newTestMux(t, PaneConfig{Control: true, Label: "server"}, PaneConfig{Control: true})
	labelled, plain := m.paneByID(0), m.paneByID(1)
	labelled.controller = &stepController{}
	plain.controller = &stepController{}

	// First poll seeds prev; the second is the one that changes and broadcasts.
	m.pollControllers(true)
	events := m.pollControllers(true)
	if len(events) != 2 {
		t.Fatalf("want one snapshot per pane, got %d: %v", len(events), events)
	}

	seen := map[int]any{}
	for _, raw := range events {
		ev, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("event is not a map: %v", raw)
		}
		if ev["type"] != "snapshot" {
			t.Fatalf("unexpected event type %v", ev["type"])
		}
		seen[ev["pane"].(int)] = ev["label"]
	}
	if seen[0] != "server" {
		t.Errorf("the labelled pane's snapshot carries label %v, want \"server\"", seen[0])
	}
	if l, present := seen[1]; present && l != nil {
		t.Errorf("an unlabelled pane's snapshot carries a label key: %v", l)
	}
}

// ── the flag ────────────────────────────────────────────────────────────────

// listPanes sends the `list` verb and returns its pane entries.
//
// The array is under reply["result"]["panes"], not reply["panes"] — a `list`
// reply is an ordinary sockrpc reply envelope. Reading the wrong level yields a
// nil slice and a loop that asserts nothing, which is a test that passes for the
// wrong reason; hence one helper that fatals instead.
func listPanes(t *testing.T, rc *rpcConn, id string) []map[string]any {
	t.Helper()
	rc.send(map[string]any{"type": "list", "id": id})
	reply, _ := rc.awaitReply(id, nil)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("list reply has no result object: %v", reply)
	}
	raw, ok := result["panes"].([]any)
	if !ok {
		t.Fatalf("list result has no panes array: %v", result)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		entry, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("pane entry is not an object: %v", r)
		}
		out = append(out, entry)
	}
	return out
}

// TestLabelReachesResultsAndList runs a real magmux and reads both.
func TestLabelReachesResultsAndList(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless",
		"-e", "sleep 30", "--label", "server",
		"-e", "sleep 30", "--label", "worker two")
	conn := dialASAP(t, h.sock, 10*time.Second)
	rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}

	got := map[int]string{}
	for _, entry := range listPanes(t, rc, "L1") {
		if l, ok := entry["label"].(string); ok {
			got[paneNum(t, entry, "pane")] = l
		}
	}
	if got[0] != "server" {
		t.Errorf("pane 0 label is %q, want \"server\"", got[0])
	}
	if got[1] != "worker two" {
		t.Errorf("pane 1 label is %q, want \"worker two\" (a space must survive)", got[1])
	}
	if len(got) != 2 {
		t.Errorf("%d panes carry a label, want exactly the two that were named: %v", len(got), got)
	}
}

// TestLabelWithNoPrecedingCommandWarns. Warned and dropped, never deferred and
// never fatal — the file's convention for --id and --theme.
func TestLabelWithNoPrecedingCommandWarns(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless", "-w", "--label", "orphan", "-e", "echo hi")
	if code := h.wait(20 * time.Second); code != 0 {
		t.Fatalf("a stray --label must not be fatal; exit %d\nstderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "ignoring --label") {
		t.Errorf("stderr said nothing about the dropped label: %q", h.stderr.String())
	}
	// AC1.4 still: the warning goes to stderr, and stdout stays empty.
	h.requireSilentStdout()
}

// TestLabelAppliesToThePrecedingCommandOnly pins the binding rule at the one
// place it could plausibly be wrong: a labelled -e followed by an unlabelled
// one.
func TestLabelAppliesToThePrecedingCommandOnly(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless",
		"-e", "sleep 30", "--label", "first",
		"-e", "sleep 30")
	conn := dialASAP(t, h.sock, 10*time.Second)
	rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}

	seen := 0
	for _, entry := range listPanes(t, rc, "L1") {
		id := paneNum(t, entry, "pane")
		label, _ := entry["label"].(string)
		switch id {
		case 0:
			seen++
			if label != "first" {
				t.Errorf("pane 0 label is %q, want \"first\"", label)
			}
		case 1:
			seen++
			if label != "" {
				t.Errorf("pane 1 picked up a label it was never given: %q", label)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("only %d of the two session panes were in `list`; the assertions above were vacuous", seen)
	}
}
