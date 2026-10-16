package mux

// Claude readiness, the pre-submit box check and acceptance evidence —
// observed only (Phase 3: nothing gates yet). Screens are the real captures
// (claude_screen_test.go); terminal timelines are fake ttyStates, because the
// F3 timelines are a property of the decision, not of a live PTY.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/pty"
)

var (
	modeRaw    = pty.Mode{}
	modeCooked = pty.Mode{Canonical: true, Echo: true}
)

// settledTTY is a ttyWatch whose history says: raw since `since`, boot reset
// seen, nothing changed since. Its read returns *mode (raw unless changed).
func settledTTY(mode *atomic.Pointer[pty.Mode], since time.Time) *ttyWatch {
	w := newTTYWatch(func() (pty.Mode, error) { return *mode.Load(), nil }, since)
	w.have, w.last = true, modeRaw
	w.changedAt, w.firstRawAt, w.sampledAt = since, since, since
	w.sawReset = true
	return w
}

// gatedClaudeOn is a declared, settled Claude controller over p: everything the
// readiness rules need is old enough to pass, so the SCREEN decides.
func gatedClaudeOn(p *Pane) (*ClaudeCodeController, *atomic.Pointer[pty.Mode]) {
	var mode atomic.Pointer[pty.Mode]
	m := modeRaw
	mode.Store(&m)
	past := time.Now().Add(-time.Minute)
	c := newClaudeCodeController(p)
	c.tty = settledTTY(&mode, past)
	c.boxSince.Store(past.UnixNano())
	c.everReady.Store(true)
	p.gateBasis = gateDeclared
	p.controller = c
	return c, &mode
}

// swapScreen replaces p's screen with the one at a labelled moment, as if
// Claude had drawn it next.
func swapScreen(t *testing.T, p *Pane, scenario, label string) {
	t.Helper()
	src := claudePaneAt(t, scenario, label)
	p.mu.Lock()
	p.screen, p.primaryScreen = src.screen, src.primaryScreen
	p.mu.Unlock()
}

// ── the precedence table ────────────────────────────────────────────────────

func TestReadinessPrecedence(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Minute)
	settled := ttyState{Raw: true, ChangedAt: old, FirstRawAt: old, SawReset: true, SampledAt: now}
	box := claudeScreenAt(t, "s1-box-history", "empty-box-placeholder")
	boxText := claudeScreenAt(t, "s1-box-history", "box-wrapped-text")
	perm := claudeScreenAt(t, "s2-permission", "dialog-permission")
	question := claudeScreenAt(t, "s2-permission", "dialog-question")
	insert := claudeScreenAt(t, "s5-insert-plan", "insert-footer-empty")
	blank := parseClaudeScreen(newScrollPane(39, 100).tailRows(0), 100)
	typeQ := ReadinessQuery{Class: ClassType, Types: true}
	submitQ := ReadinessQuery{Class: ClassSubmit}

	base := readinessInput{now: now, tt: settled, sc: box, everReady: true, boxSince: old, bracketPaste: true, q: typeQ}
	with := func(f func(*readinessInput)) readinessInput { in := base; f(&in); return in }

	for _, c := range []struct {
		name   string
		in     readinessInput
		ready  bool
		reason string
		dism   bool
	}{
		{"ready", base, true, "", false},
		{"a: not raw, booting", with(func(i *readinessInput) { i.tt.Raw = false; i.everReady = false }), false, protocol.ReasonStarting, false},
		{"a: not raw, was ready", with(func(i *readinessInput) { i.tt.Raw = false }), false, protocol.ReasonTerminalNotReady, false},
		{"a: read error", with(func(i *readinessInput) { i.tt.Err = os.ErrClosed }), false, protocol.ReasonTerminalNotReady, false},
		{"a beats b", with(func(i *readinessInput) { i.tt.Raw = false; i.sc = perm }), false, protocol.ReasonTerminalNotReady, false},
		{"b: permission dialog", with(func(i *readinessInput) { i.sc = perm }), false, "dialog:permission", true},
		{"b: question dialog", with(func(i *readinessInput) { i.sc = question }), false, "dialog:question", true},
		{"c: registry waiting on permission", with(func(i *readinessInput) {
			i.sc = blank
			i.reg = regEntry{Status: "waiting", WaitingFor: "permission prompt"}
		}), false, "dialog:permission", false},
		{"c: registry waiting on input", with(func(i *readinessInput) {
			i.sc = blank
			i.reg = regEntry{Status: "waiting", WaitingFor: "input needed"}
		}), false, "dialog:question", false},
		{"c: registry ignored while a box shows", with(func(i *readinessInput) { i.reg = regEntry{Status: "waiting"} }), true, "", false},
		{"d: vim INSERT", with(func(i *readinessInput) { i.sc = insert }), false, protocol.ReasonVimMode, false},
		{"e: no box, booting", with(func(i *readinessInput) { i.sc = blank; i.everReady = false }), false, protocol.ReasonStarting, false},
		{"e: no box, was ready", with(func(i *readinessInput) { i.sc = blank }), false, protocol.ReasonNoInputBox, false},
		{"f: no reset, floor not passed", with(func(i *readinessInput) {
			i.everReady = false
			i.tt.SawReset = false
			i.tt.FirstRawAt = now.Add(-2 * time.Second)
		}), false, protocol.ReasonStarting, false},
		{"f: no reset, floor passed", with(func(i *readinessInput) {
			i.everReady = false
			i.tt.SawReset = false
			i.tt.FirstRawAt = now.Add(-bootFloor)
		}), true, "", false},
		{"f: only before the first ready", with(func(i *readinessInput) {
			i.tt.SawReset = false
			i.tt.FirstRawAt = now.Add(-time.Second)
		}), true, "", false},
		{"g: termios changed 300ms ago", with(func(i *readinessInput) { i.tt.ChangedAt = now.Add(-300 * time.Millisecond) }), false, protocol.ReasonTerminalNotReady, false},
		{"g: box appeared 300ms ago, booting", with(func(i *readinessInput) {
			i.everReady = false
			i.boxSince = now.Add(-300 * time.Millisecond)
		}), false, protocol.ReasonStarting, false},
		{"h: box holds text, TYPE", with(func(i *readinessInput) { i.sc = boxText }), false, protocol.ReasonBoxNotEmpty, false},
		{"h: only with Types", with(func(i *readinessInput) { i.sc = boxText; i.q = submitQ }), true, "", false},
		{"i: paste before 2004", with(func(i *readinessInput) {
			i.bracketPaste = false
			i.q.Paste = true
		}), false, protocol.ReasonTerminalNotReady, false},
		{"i: only with Paste", with(func(i *readinessInput) { i.bracketPaste = false }), true, "", false},
	} {
		got := decideReadiness(c.in)
		if got.Ready != c.ready || got.Reason != c.reason || got.Dismissable != c.dism {
			t.Errorf("%s: got ready=%v reason=%q dismissable=%v (%s); want %v %q %v",
				c.name, got.Ready, got.Reason, got.Dismissable, got.Detail, c.ready, c.reason, c.dism)
		}
		if !got.Ready && got.Detail == "" {
			t.Errorf("%s: a refusal with no detail", c.name)
		}
	}

	if got := decideReadiness(with(func(i *readinessInput) { i.sc = boxText; i.q = submitQ })); !got.BoxHasText {
		t.Errorf("BoxHasText not reported")
	}
	auto := claudeScreenAt(t, "s3-paste", "auto-long-in-box")
	if got := decideReadiness(with(func(i *readinessInput) { i.sc = auto; i.q = submitQ })); got.MaxPaste != 2 {
		t.Errorf("MaxPaste = %d, want 2", got.MaxPaste)
	}
}

// TestReadinessF3Timelines walks the measured boot timelines (research F3 and
// architecture §2.4.3) through the decision and checks the moment READY first
// holds. The screen is the real idle box; the terminal and the box clock are
// the timeline's.
func TestReadinessF3Timelines(t *testing.T) {
	box := claudeScreenAt(t, "s1-box-history", "empty-box-placeholder")
	blank := parseClaudeScreen(newScrollPane(39, 100).tailRows(0), 100)
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }

	type timeline struct {
		name                  string
		firstRaw, boxAt       int // ms after spawn
		resetFrom, resetUntil int // cooked in [from, until); 0,0 = no reset
		readyAt               int // the expected first READY, ms
	}
	for _, tl := range []timeline{
		{"A quiet", 17, 1729, 1284, 1506, 2729},
		{"B quiet", 86, 1471, 882, 1313, 2471},
		{"loaded, reset after the box", 50, 1500, 2500, 2700, 3700},
		{"loaded, no reset", 50, 1500, 0, 0, 3550},
		{"wrapped (claudish)", 850, 2400, 2100, 2300, 3400},
	} {
		t0 := time.Unix(1_700_000_000, 0)
		at := func(n int) time.Time { return t0.Add(ms(n)) }
		stateAt := func(n int) ttyState {
			st := ttyState{SampledAt: at(n), FirstRawAt: at(tl.firstRaw), ChangedAt: at(tl.firstRaw), Raw: n >= tl.firstRaw}
			if tl.resetUntil > 0 && n >= tl.resetFrom {
				st.SawReset = true
				st.ChangedAt = at(tl.resetFrom)
				if n < tl.resetUntil {
					st.Raw = false
				} else {
					st.ChangedAt = at(tl.resetUntil)
				}
			}
			return st
		}
		first := -1
		for n := 0; n <= 6000; n += 10 {
			sc, since := blank, time.Time{}
			if n >= tl.boxAt {
				sc, since = box, at(tl.boxAt)
			}
			r := decideReadiness(readinessInput{now: at(n), tt: stateAt(n), sc: sc, boxSince: since, bracketPaste: true,
				q: ReadinessQuery{Class: ClassType, Types: true}})
			if r.Ready {
				first = n
				break
			}
			if r.Reason != protocol.ReasonStarting {
				t.Errorf("%s @%dms: a booting pane refused as %q", tl.name, n, r.Reason)
			}
		}
		if first < tl.readyAt || first > tl.readyAt+10 {
			t.Errorf("%s: first READY at %dms, want %dms", tl.name, first, tl.readyAt)
		}
	}
}

// TestReadinessNeedsNoTranscript: a Claude pane whose transcript was never
// found (no projectDir at all) is judged on the box and the terminal alone,
// through the real method and its synchronous sample.
func TestReadinessNeedsNoTranscript(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "empty-box-placeholder")
	c, mode := gatedClaudeOn(p)
	if c.projectDir != "" || c.published.Load() != nil {
		t.Fatalf("the controller should know no transcript")
	}
	r := c.InputReadiness(ReadinessQuery{Class: ClassType, Types: true})
	if !r.Ready {
		t.Fatalf("not ready without a transcript: %+v", r)
	}
	// The demand samples synchronously: a reset that has just begun is seen
	// at once, whatever the history says.
	m := modeCooked
	mode.Store(&m)
	if r := c.InputReadiness(ReadinessQuery{Class: ClassType, Types: true}); r.Ready || r.Reason != protocol.ReasonTerminalNotReady {
		t.Errorf("a cooked terminal at the demand: %+v", r)
	}
}

// TestDemandSamplesAndHeats: every demand is exactly one synchronous read and
// extends the hot window; Poll takes no synchronous read of its own beyond the
// cold sample.
func TestDemandSamplesAndHeats(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "empty-box-placeholder")
	c, _ := gatedClaudeOn(p)
	var reads atomic.Int32
	inner := c.tty.read
	c.tty.read = func() (pty.Mode, error) { reads.Add(1); return inner() }
	c.tty.close() // no goroutine may add reads behind the test's back

	c.InputReadiness(ReadinessQuery{Class: ClassType, Types: true})
	c.CheckBox(BoxExpect{Class: ClassSubmit})
	if n := reads.Load(); n != 2 {
		t.Errorf("two demands made %d reads, want 2", n)
	}
	c.InputAccepted(AcceptBaseline{})
	if n := reads.Load(); n != 2 {
		t.Errorf("InputAccepted sampled the terminal (%d reads); it decides no write", n)
	}
}

// ── CheckBox ────────────────────────────────────────────────────────────────

func TestCheckBox(t *testing.T) {
	for _, c := range []struct {
		name     string
		scenario string
		label    string
		want     BoxExpect
		ready    bool
		reason   string // readiness reason when !ready
		verdict  BoxVerdict
		verified string
		paste    int
		mapsTo   string // boxVerdictReason
	}{
		{"equal", "s1-box-history", "box-wrapped-text",
			BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true}, true, "", BoxVerified, verifiedExact, 0, ""},
		{"a chunk missing", "s1-box-history", "box-wrapped-text",
			BoxExpect{Class: ClassSubmit, Exact: strings.Replace(sentFrance, "deliberately ", "deliberately, and a chunk the box lost, ", 1), TypedText: true},
			true, "", BoxMismatch, "", 0, protocol.ReasonTextMismatch},
		{"undetermined, non-empty", "s1-box-history", "box-wrapped-text",
			BoxExpect{Class: ClassSubmit}, true, "", BoxVerified, verifiedNonEmpty, 0, ""},
		{"a new paste token", "s3-paste", "auto-long-in-box",
			BoxExpect{Class: ClassSubmit, Exact: sentAutoLong, TypedText: true, Pasted: true, PasteAbove: 1},
			true, "", BoxVerified, verifiedPasteToken, 2, ""},
		{"a stale paste token", "s3-paste", "auto-long-in-box",
			BoxExpect{Class: ClassSubmit, Exact: sentAutoLong, TypedText: true, Pasted: true, PasteAbove: 2},
			true, "", BoxMismatch, "", 0, protocol.ReasonTextMismatch},
		{"a token without Pasted", "s3-paste", "auto-long-in-box",
			BoxExpect{Class: ClassSubmit, Exact: sentAutoLong, TypedText: true},
			true, "", BoxMismatch, "", 0, protocol.ReasonTextMismatch},
		{"empty after typing", "s1-box-history", "empty-box-placeholder",
			BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true}, true, "", BoxEmpty, "", 0, protocol.ReasonTextMismatch},
		{"empty, Enter only", "s1-box-history", "empty-box-placeholder",
			BoxExpect{Class: ClassSubmit}, true, "", BoxEmpty, "", 0, protocol.ReasonEmptyBox},
		// The queued-message placeholder is faint: an empty box mid-turn.
		{"placeholder mid-turn", "s4-midturn", "queued-a",
			BoxExpect{Class: ClassSubmit}, true, "", BoxEmpty, "", 0, protocol.ReasonEmptyBox},
		{"a dialog", "s2-permission", "dialog-permission",
			BoxExpect{Class: ClassSubmit, Exact: "x", TypedText: true}, false, "dialog:permission", 0, "", 0, ""},
		{"vim INSERT", "s5-insert-plan", "insert-footer-empty",
			BoxExpect{Class: ClassSubmit}, false, protocol.ReasonVimMode, 0, "", 0, ""},
	} {
		p := claudePaneAt(t, c.scenario, c.label)
		g, _ := gatedClaudeOn(p)
		got := g.CheckBox(c.want)
		if got.Readiness.Ready != c.ready {
			t.Errorf("%s: ready=%v (%+v), want %v", c.name, got.Readiness.Ready, got.Readiness, c.ready)
			continue
		}
		if !c.ready {
			if got.Readiness.Reason != c.reason {
				t.Errorf("%s: reason %q, want %q", c.name, got.Readiness.Reason, c.reason)
			}
			continue
		}
		if got.Verdict != c.verdict || got.Verified != c.verified || got.Baseline.Paste != c.paste {
			t.Errorf("%s: verdict=%v verified=%q paste=%d (%s); want %v %q %d",
				c.name, got.Verdict, got.Verified, got.Baseline.Paste, got.Detail, c.verdict, c.verified, c.paste)
		}
		if r := boxVerdictReason(got.Verdict, c.want); r != c.mapsTo {
			t.Errorf("%s: maps to %q, want %q", c.name, r, c.mapsTo)
		}
		if got.Verdict == BoxVerified {
			b := got.Baseline
			if b.At.IsZero() || b.VerifiedKey == "" {
				t.Errorf("%s: no baseline: %+v", c.name, b)
			}
			if c.want.Exact != "" && b.SentKey != inputKey(c.want.Exact) {
				t.Errorf("%s: SentKey is not the sent text's key", c.name)
			}
			if c.want.Exact == "" && b.SentKey != b.VerifiedKey {
				t.Errorf("%s: undetermined SentKey should be the verified key", c.name)
			}
		}
	}
}

// ── InputAccepted ───────────────────────────────────────────────────────────

// baselineAt builds the baseline CheckBox would take on a screen for a send of
// `sent`, regardless of whether that screen's box holds it (the negative cases
// need a baseline for content that never reached the box).
func baselineAt(t *testing.T, c *ClaudeCodeController, sc claudeScreen, sent string) AcceptBaseline {
	t.Helper()
	return newAcceptBaseline(sc, sent, sent, 0, time.Now(), c.transcriptSeq())
}

func TestInputAccepted(t *testing.T) {
	type step struct{ scenario, label string }
	for _, c := range []struct {
		name     string
		before   step // the screen the baseline is taken on
		sent     string
		after    step
		state    AcceptState
		evidence string
	}{
		{"text still in the box", step{"s1-box-history", "box-wrapped-text"}, sentFrance,
			step{"s1-box-history", "box-wrapped-text"}, AcceptInBox, ""},
		{"box emptied, no entry", step{"s1-box-history", "box-wrapped-text"}, sentFrance,
			step{"s1-box-history", "empty-box-placeholder"}, AcceptNone, ""},
		{"a new matching entry", step{"s1-box-history", "box-wrapped-text"}, sentFrance,
			step{"s1-box-history", "submitted-working"}, AcceptTaken, evidenceHistory},
		// Mid-turn: the queued entry (F9) is the evidence.
		{"a new queued entry mid-turn", step{"s4-midturn", "turn-running-d"}, sentQueued,
			step{"s4-midturn", "queued-a"}, AcceptTaken, evidenceHistory},
		// A lost send whose conversation did not change: placeholder box, the
		// same entries.
		{"unchanged mid-turn", step{"s4-midturn", "queued-a"}, sentQueued3,
			step{"s4-midturn", "queued-c"}, AcceptNone, ""},
		// The same prompt again: the old entry is in the baseline; only a NEW
		// one counts.
		{"an old identical entry", step{"s4-midturn", "queued-a"}, sentQueued,
			step{"s4-midturn", "queued-c"}, AcceptNone, ""},
		// [A] negative 1: the assistant's reply begins with the lost prompt's
		// words (`⏺ Respond with a single line, as you asked.`).
		{"assistant prose with the prompt's words", step{"s1-box-history", "history-long-prompt"},
			"Respond with a single line, as you asked.",
			step{"s1-box-history", "assistant-prose-same-words"}, AcceptNone, ""},
		// [A] negative 2: a different prompt's NEW entry shares a 60-rune
		// prefix with the lost send.
		{"a different prompt sharing a long prefix", step{"s1-box-history", "history-long-prompt"},
			"Respond with a single line that starts with the exact words: something else entirely",
			step{"s1-box-history", "assistant-prose-same-words"}, AcceptNone, ""},
		{"the assistant quoting ❯", step{"s1-box-history", "assistant-prose-same-words"}, "not a prompt.",
			step{"s1-box-history", "assistant-quotes-prompt-glyph-and-dialog"}, AcceptNone, ""},
		{"a slash command", step{"s1-box-history", "assistant-quotes-prompt-glyph-and-dialog"}, "/clear",
			step{"s1-box-history", "after-slash-clear"}, AcceptTaken, evidenceHistory},
	} {
		p := claudePaneAt(t, c.before.scenario, c.before.label)
		g, _ := gatedClaudeOn(p)
		b := baselineAt(t, g, claudeScreenAt(t, c.before.scenario, c.before.label), c.sent)
		swapScreen(t, p, c.after.scenario, c.after.label)
		got := g.InputAccepted(b)
		if got.State != c.state || got.Evidence != c.evidence {
			t.Errorf("%s: %+v, want state %v evidence %q", c.name, got, c.state, c.evidence)
		}
	}
}

// TestPasteAcceptedByItsFullText: the box shows `[Pasted text #2]`, the
// history entry shows the WHOLE text (captures, fact 1). The entry matches the
// SENT key, which is why matches compares SentKey as well as VerifiedKey.
func TestPasteAcceptedByItsFullText(t *testing.T) {
	p := claudePaneAt(t, "s3-paste", "auto-long-in-box")
	g, _ := gatedClaudeOn(p)
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentAutoLong, TypedText: true, Pasted: true, PasteAbove: 1})
	if chk.Verdict != BoxVerified || chk.Verified != verifiedPasteToken {
		t.Fatalf("check: %+v", chk)
	}
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptInBox {
		t.Errorf("before the Enter the token is in the box: %+v", got)
	}
	swapScreen(t, p, "s3-paste", "auto-long-submitted")
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptTaken || got.Evidence != evidenceHistory {
		t.Errorf("after the Enter: %+v", got)
	}
}

// TestMatchesShortPasteBySentKey: a collapsed paste whose full text is SHORTER
// than historyMatchMin (four short lines collapse too: captures, fact 1) can
// only be matched by SentKey equality, because the prefix branch needs 48
// runes. No capture holds such a paste, so this is a value-level check of the
// rule the long captures above rely on.
func TestMatchesShortPasteBySentKey(t *testing.T) {
	sent := "a\nb\nc\nd"
	b := AcceptBaseline{VerifiedKey: inputKey("[Pasted text #3 +3 lines]"), SentKey: inputKey(sent), Paste: 3}
	e := promptEntry{Text: sent, Key: inputKey(sent)}
	if !matches(e, b) {
		t.Errorf("the expanded history entry of a short paste does not match")
	}
	if matches(promptEntry{Text: "a b c", Key: "abc"}, b) {
		t.Errorf("a different short entry matched")
	}
	if matches(promptEntry{}, AcceptBaseline{}) {
		t.Errorf("an empty entry matched an empty baseline")
	}
}

// TestTakenBeatsInBox: a new matching entry AND the text still in the box is
// Taken — re-pressing Enter would submit it twice. No capture shows both at
// once, so the screen is SPLICED from two real frames: the conversation of
// submitted-working over the box of box-wrapped-text.
func TestTakenBeatsInBox(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
	g, _ := gatedClaudeOn(p)
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
	if chk.Verdict != BoxVerified {
		t.Fatalf("check: %+v", chk)
	}
	conv := claudePaneAt(t, "s1-box-history", "submitted-working").tailRows(0)
	boxRows := claudePaneAt(t, "s1-box-history", "box-wrapped-text").tailRows(0)
	spliced := append(append([]styledRow(nil), conv[:33]...), boxRows[33:]...)
	sc := parseClaudeScreen(spliced, 100)
	if !sc.BoxVisible || sc.BoxKey != inputKey(sentFrance) || len(sc.Prompts) != 1 {
		t.Fatalf("the splice is not entry+box: %+v", sc)
	}
	if got := acceptVerdict(sc, chk.Baseline, nil); got.State != AcceptTaken {
		t.Errorf("entry and box: %+v, want Taken", got)
	}
}

func TestAcceptedByTranscript(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
	g, _ := gatedClaudeOn(p)
	// An EQUAL transcript entry from before the baseline is not evidence: it
	// is an earlier submission of the same text.
	g.notePrompt(sentFrance)
	chk := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: sentFrance, TypedText: true})
	if chk.Verdict != BoxVerified {
		t.Fatalf("check: %+v", chk)
	}
	swapScreen(t, p, "s1-box-history", "empty-box-placeholder") // no entry on screen
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptNone {
		t.Fatalf("a transcript entry older than the baseline counted: %+v", got)
	}

	g.notePrompt("a different prompt")
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptNone {
		t.Errorf("a transcript entry with a different key counted: %+v", got)
	}
	// Claude files the prompt as typed; the key ignores whitespace.
	g.notePrompt(strings.ReplaceAll(sentFrance, " ", "  "))
	if got := g.InputAccepted(chk.Baseline); got.State != AcceptTaken || got.Evidence != evidenceTranscript {
		t.Errorf("an equal transcript entry after the baseline: %+v", got)
	}
}

// TestApplyLineRecordsLastPrompt: only a REAL user prompt advances lastPrompt;
// a tool result filed as a "user" entry does not.
func TestApplyLineRecordsLastPrompt(t *testing.T) {
	c := newClaudeCodeController(&Pane{})
	c.applyLine([]byte(`{"type":"user","message":{"role":"user","content":"hello there"}}`))
	c.applyLine([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`))
	lp := c.lastPrompt.Load()
	if lp == nil || lp.seq != 1 || lp.key != inputKey("hello there") {
		t.Fatalf("lastPrompt = %+v", lp)
	}
	c.applyLine([]byte(`{"type":"user","message":{"role":"user","content":"again"}}`))
	if lp := c.lastPrompt.Load(); lp.seq != 2 || lp.key != "again" {
		t.Errorf("lastPrompt = %+v", lp)
	}
}

// ── the registry ────────────────────────────────────────────────────────────

func writeRegistry(t *testing.T, dir string, pid int, v map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", pid)), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRegistryCorroboratesADialog: with no box and no recognised dialog on
// screen, Claude's own session registry saying `waiting` names the dialog; it
// never makes a pane ready.
func TestRegistryCorroboratesADialog(t *testing.T) {
	dir := t.TempDir()
	p := newScrollPane(39, 100) // a blank screen: no box, no dialog
	p.cmd = &exec.Cmd{Env: []string{"CLAUDE_CONFIG_DIR=" + dir}, Process: &os.Process{Pid: 424242}}
	g, _ := gatedClaudeOn(p)

	if r := g.InputReadiness(ReadinessQuery{Class: ClassSubmit}); r.Reason != protocol.ReasonNoInputBox {
		t.Fatalf("no registry: %+v", r)
	}
	writeRegistry(t, dir, 424242, map[string]any{"pid": 424242, "sessionId": "s-1", "status": "waiting", "waitingFor": "permission prompt"})
	g.reg.invalidate()
	if r := g.InputReadiness(ReadinessQuery{Class: ClassSubmit}); r.Reason != "dialog:permission" || r.Dismissable {
		t.Errorf("pid registry: %+v", r)
	}
}

// TestRegistryFallsBackToTheSessionID: a wrapper's pid has no registry file;
// the transcript's session id finds the entry.
func TestRegistryFallsBackToTheSessionID(t *testing.T) {
	dir := t.TempDir()
	p := newScrollPane(39, 100)
	p.cmd = &exec.Cmd{Env: []string{"CLAUDE_CONFIG_DIR=" + dir}, Process: &os.Process{Pid: 515151}}
	g, _ := gatedClaudeOn(p)
	writeRegistry(t, dir, 616161, map[string]any{"pid": 616161, "sessionId": "abc-123", "status": "waiting", "waitingFor": "input needed"})
	path := filepath.Join(dir, "projects", "x", "abc-123.jsonl")
	g.published.Store(&path)
	if r := g.InputReadiness(ReadinessQuery{Class: ClassSubmit}); r.Reason != "dialog:question" {
		t.Errorf("session-id registry: %+v", r)
	}
}

// ── Poll, the published view, list/results ──────────────────────────────────

func TestPollPublishesInputOnlyWhenGated(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "empty-box-placeholder")
	c, _ := gatedClaudeOn(p)
	snap, _ := c.Poll()
	if !snap.Input.Gated || !snap.Input.Ready {
		t.Errorf("gated pane: Input = %+v", snap.Input)
	}

	p.gateBasis = gateNone
	snap, _ = c.Poll()
	if snap.Input != (InputReadiness{}) {
		t.Errorf("an ungated pane published Input %+v", snap.Input)
	}
}

func TestResultsCarryInputFields(t *testing.T) {
	gated := newScrollPane(10, 40)
	gated.id = 0
	gated.gateBasis = gateDeclared
	gated.editorOverride = overrideDelegated
	gated.controllerSnap.Input = InputReadiness{Gated: true, Reason: protocol.ReasonStarting, Detail: "no box yet"}
	plain := newScrollPane(10, 40)
	plain.id = 1
	m := &Magmux{allPanes: []*Pane{gated, plain}}

	res := m.buildPaneResults()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	_ = json.Unmarshal(b, &got)
	if got[0]["gateBasis"] != "declared" || got[0]["editorOverride"] != "delegated" {
		t.Errorf("gated entry: %v", got[0])
	}
	in, _ := got[0]["input"].(map[string]any)
	if in == nil || in["ready"] != false || in["reason"] != "starting" || in["detail"] != "no box yet" {
		t.Errorf("gated entry input: %v", got[0]["input"])
	}
	for _, k := range []string{"input", "gateBasis", "editorOverride"} {
		if _, ok := got[1][k]; ok {
			t.Errorf("an ungated pane carries %q: %v", k, got[1])
		}
	}
}

// TestInputChangeDoesNotRecordObserved (D10): a readiness flip is not a turn.
// The pane's Input changes between two polls while its State does not; no
// snapshot event goes out and the control panel observes nothing, even with an
// instruction outstanding on the route.
func TestInputChangeDoesNotRecordObserved(t *testing.T) {
	m := newTestMux(t, PaneConfig{Control: true})
	p := m.paneByID(0)
	c, mode := gatedClaudeOn(p)
	_ = c
	m.control.recordSend(0, "", "do something")

	m.pollControllers(true) // seeds prev
	before := p.controllerSnap.Input
	cooked := modeCooked
	mode.Store(&cooked)
	c.tty.sampleCold(time.Now())
	events := m.pollControllers(true)
	after := p.controllerSnap.Input
	if before == after {
		t.Fatalf("the test did not flip Input: %+v", before)
	}
	if len(events) != 0 {
		t.Errorf("a readiness flip emitted %v", events)
	}
	m.control.mu.Lock()
	r := m.control.routes[0]
	observed := r.observed
	m.control.mu.Unlock()
	if observed != 0 {
		t.Errorf("a readiness flip was recorded as observed (%d)", observed)
	}
}

// TestStartBuildsAndHeatsTheWatch: a gated pane's Start gives it a ttyWatch
// and heats it for the boot (Phase 2: newTTYWatch starts nothing by itself);
// an ungated pane gets none.
func TestStartBuildsAndHeatsTheWatch(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	p := &Pane{ptmx: r, gateBasis: gateDeclared, startedAt: time.Now()}
	c := newClaudeCodeController(p)
	_ = c.Start(t.Context())
	if c.tty == nil {
		t.Fatalf("a gated pane has no ttyWatch after Start")
	}
	if until := c.tty.hotUntilValue(); time.Until(until) < ttyHotStartup-time.Second {
		t.Errorf("the watch is not heated for the boot: hot until %v", until)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return !c.tty.isRunning() }) {
		t.Errorf("Stop left the sampler running")
	}

	q := newClaudeCodeController(&Pane{ptmx: r})
	_ = q.Start(t.Context())
	if q.tty != nil {
		t.Errorf("an ungated pane got a ttyWatch")
	}
}
