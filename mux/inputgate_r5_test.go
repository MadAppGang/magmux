package mux

// r5 review M1: somebody else's Enter (a human key, a noGate send, a remote
// `input` op) inside the ~150 ms before the first verify read submits the
// text before Claude ever draws it. Every read then shows an empty box, so
// the box alone looks exactly like the `never_drawn` silent drop — and the
// one guard left, acceptance evidence against the pre-typing baseline, is
// blind in the history blind spot with a transcript that is lagging or was
// never found. A submit always CHANGES something the silent drop does not:
// the conversation above the box gains a `❯` entry (or a queued one), or a
// turn starts. So `never_drawn` additionally requires the conversation to be
// unchanged since the pre-typing baseline, and the turn not to have started;
// a change is text_mismatch, named, and never retyped.

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// emptyConvChanged is an empty, ready box whose conversation changed since
// the pre-typing baseline.
func emptyConvChanged(what string) BoxCheck {
	c := boxEmptyCheck()
	c.ConvChanged = what
	return c
}

// TestForeignSubmitBeforeFirstDrawIsNotRetyped: the box is empty on every
// read for the whole verify window with no terminal reset — never_drawn's
// shape — but the conversation changed since the text was typed. Somebody
// else may have submitted it: no retype, no Enter, text_mismatch, and the
// error names the change and does not claim nothing was submitted.
func TestForeignSubmitBeforeFirstDrawIsNotRetyped(t *testing.T) {
	const entry = "the conversation above the input box changed"
	for _, tc := range []struct {
		name string
		what string
		from time.Duration // the change is visible from here on
		to   time.Duration // ... and until here (0: to the end)
	}{
		{"entry-from-the-first-read", entry, 0, 0},
		{"spinner-from-the-first-read", "a turn started (Claude's spinner appeared above the input box)", 0, 0},
		{"registry-went-busy", "a turn started (Claude's session registry went busy)", 0, 0},
		{"entry-late-in-the-window", entry, 2 * time.Second, 0},
		// A change seen once and gone again still counts: a short turn
		// that came and went is still a submit.
		{"spinner-seen-then-gone", "a turn started (Claude's spinner appeared above the input box)", 300 * time.Millisecond, 900 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clk lagClock
			g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
				el := clk.since()
				if el >= tc.from && (tc.to == 0 || el < tc.to) {
					return emptyConvChanged(tc.what)
				}
				return boxEmptyCheck()
			}}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, sendRequest{text: "hello", enter: true})
			wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
			if got := log.settle(); got != "hello" {
				t.Errorf("bytes %q, want the text once and no Enter", got)
			}
			noRetype(t, o)
			msg := o.err.Error()
			for _, want := range []string{tc.what, "another writer", "not retyped"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not say %q", msg, want)
				}
			}
			if strings.Contains(msg, "nothing was submitted") {
				t.Errorf("error %q claims nothing was submitted", msg)
			}
		})
	}
}

// TestSilentDropWithAnUnchangedConversationIsStillRetyped is the positive
// control: the same empty box with the conversation unchanged is the genuine
// silent drop, and still gets its one retype — and the verify was handed the
// pre-typing baseline it compares the conversation against.
func TestSilentDropWithAnUnchangedConversationIsStillRetyped(t *testing.T) {
	g := &fakeGate{}
	g.checkFn = func(BoxExpect, int) BoxCheck {
		if strings.Count(g.log.String(), "hello") >= 2 {
			return verifiedBox(verifiedExact)
		}
		return boxEmptyCheck() // ConvChanged "": nothing above the box moved
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hellohello\r" {
		t.Errorf("bytes %q, want one retype and one Enter", got)
	}
	if res := replyOf(o); res["retyped"] != true || res["retypeCause"] != retypeNeverDrawn {
		t.Errorf("reply %v: want retyped:true, retypeCause:%q", res, retypeNeverDrawn)
	}
	g.mu.Lock()
	first := g.expects[0]
	g.mu.Unlock()
	if first.Pre.At.IsZero() || first.Pre.SentKey != fakePreKey {
		t.Errorf("the verify's BoxExpect.Pre = %+v, want the pre-typing baseline", first.Pre)
	}
}

// TestTerminalResetRetypeKeepsItsRules: the conversation check is
// never_drawn's alone. A reset is a terminal event, not a submit, and the
// terminal_reset retype is decided by its own rules, as before.
func TestTerminalResetRetypeKeepsItsRules(t *testing.T) {
	g := &fakeGate{}
	g.checkFn = func(BoxExpect, int) BoxCheck {
		if strings.Count(g.log.String(), "hello") >= 2 {
			return verifiedBox(verifiedExact)
		}
		c := lostBoxCheck()
		c.ConvChanged = "the conversation above the input box changed"
		return c
	}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if res := replyOf(o); res["retyped"] != true || res["retypeCause"] != retypeTerminalReset {
		t.Errorf("reply %v: want retyped:true, retypeCause:%q", res, retypeTerminalReset)
	}
}

// convCheck is the real controller's verify on the screen at `after`,
// against a pre-typing baseline taken on the screen at `before`.
func convCheck(t *testing.T, scenario, before, after string) BoxCheck {
	t.Helper()
	p := claudePaneAt(t, scenario, before)
	c, _ := gatedClaudeOn(p)
	r := c.InputReadiness(ReadinessQuery{Class: ClassSubmit, Types: true, Baseline: "hello"})
	if !r.Ready || r.Pre.At.IsZero() {
		t.Fatalf("%s/%s: not ready or no pre-typing baseline: %+v", scenario, before, r)
	}
	swapScreen(t, p, scenario, after)
	chk := c.CheckBox(BoxExpect{Class: ClassSubmit, Exact: "hello", TypedText: true, Pre: r.Pre})
	if !chk.Readiness.Ready || chk.Verdict != BoxEmpty {
		t.Fatalf("%s/%s: want a ready empty box, got %+v", scenario, after, chk)
	}
	return chk
}

// TestConversationChangeOnRealCaptures pins the classification on real
// Claude Code 2.1.301 screens. The conversation is every row above the box's
// top rule; the parser's spinner row (spinnerRe, in the rows just above the
// rule — the same rows that set Working) and faint runes are its volatile
// part and are not compared, and a spinner APPEARING is a turn start, which
// is.
func TestConversationChangeOnRealCaptures(t *testing.T) {
	for _, tc := range []struct {
		scenario, before, after string
		changed                 bool
	}{
		// Idle and still: the genuine silent drop's screen.
		{"s1-box-history", "empty-box-placeholder", "empty-box-placeholder", false},
		// Somebody's Enter: the `❯` entry is drawn and the turn starts.
		{"s1-box-history", "empty-box-placeholder", "submitted-working", true},
		// Mid-turn, only the spinner row animates (glyph, seconds, token
		// count, "thinking"); the row under it is identical in all three.
		// A spinner-only frame change is not a submit.
		{"s4-midturn", "turn-running-a", "turn-running-b", false},
		{"s4-midturn", "turn-running-a", "turn-running-c", false},
		// Mid-turn, somebody's Enter queues the prompt: a queued entry.
		{"s4-midturn", "turn-running-c", "queued-a", true},
	} {
		chk := convCheck(t, tc.scenario, tc.before, tc.after)
		if got := chk.ConvChanged != ""; got != tc.changed {
			t.Errorf("%s: %s → %s: ConvChanged %q, want changed=%v", tc.scenario, tc.before, tc.after, chk.ConvChanged, tc.changed)
		}
	}
}

// stripFrame is an idle 39-row frame: the banner, then blank rows down to
// the strip just above the box's top rule, then an empty box.
func stripFrame(strip ...string) []styledRow {
	const height = 39
	conv := cat(claudeBanner, []string{"⏺ Ready."})
	tail := append(append([]string{}, strip...), frontierRule(), "❯ ", frontierRule(), "  ⏵⏵ auto mode on (shift+tab to cycle)")
	rows := []styledRow{{}}
	for _, s := range conv {
		rows = append(rows, styledRow{Text: s})
	}
	for len(rows)+len(tail) < height {
		rows = append(rows, styledRow{})
	}
	for _, s := range tail {
		rows = append(rows, styledRow{Text: s})
	}
	return rows
}

// TestTurnStartIsAConversationChange: from an idle baseline, a spinner
// appearing is a turn start even though the spinner row itself is volatile
// — and Claude's registry going busy is one too, when the registry is there.
func TestTurnStartIsAConversationChange(t *testing.T) {
	const status = "                                       ◐ medium · /effort"
	idle := parseClaudeScreen(stripFrame("", status), frontierCols)
	pre := newAcceptBaseline(idle, "hello", "hello", 0, time.Now(), 0)
	working := parseClaudeScreen(stripFrame("· Germinating… (0s)", status), frontierCols)
	if !working.Working || idle.Working {
		t.Fatalf("Working idle=%v working=%v, want false/true", idle.Working, working.Working)
	}
	if got := convChange(idle, pre, regEntry{}); got != "" {
		t.Errorf("idle → idle: %q, want unchanged", got)
	}
	if got := convChange(working, pre, regEntry{}); !strings.Contains(got, "spinner") {
		t.Errorf("idle → spinner: %q, want a turn start", got)
	}

	// The registry, when present.
	withReg := func(status string) AcceptBaseline {
		b := pre
		cd := b.controllerData.(claudeBaseline)
		cd.regStatus = status
		b.controllerData = cd
		return b
	}
	for _, tc := range []struct {
		pre, now string
		changed  bool
	}{
		{"idle", "busy", true},
		{"", "busy", true}, // found busy where nothing was found: a turn is running
		{"busy", "busy", false},
		{"idle", "idle", false},
		{"", "", false}, // no registry: the screen decides alone
	} {
		got := convChange(idle, withReg(tc.pre), regEntry{Status: tc.now})
		if (got != "") != tc.changed || (tc.changed && !strings.Contains(got, "registry")) {
			t.Errorf("registry %q → %q: %q, want changed=%v", tc.pre, tc.now, got, tc.changed)
		}
	}
}

// TestConversationKeyIgnoresFaintRunesAndBlankRows: a faint rune (a tip, a
// placeholder) and the blank rows that pad the region are not content.
func TestConversationKeyIgnoresFaintRunesAndBlankRows(t *testing.T) {
	a := stripFrame("", "status")
	b := stripFrame("", "status")
	b[6] = styledRow{Text: "tip", Faint: []bool{true, true, true}}
	if ka, kb := convKey(parseClaudeScreen(a, frontierCols)), convKey(parseClaudeScreen(b, frontierCols)); ka != kb {
		t.Errorf("a faint row changed the key: %q vs %q", ka, kb)
	}
	c := stripFrame("", "status")
	c[6] = styledRow{Text: "❯ hello"}
	if ka, kc := convKey(parseClaudeScreen(a, frontierCols)), convKey(parseClaudeScreen(c, frontierCols)); ka == kc {
		t.Errorf("a new entry did not change the key %q", ka)
	}
}
