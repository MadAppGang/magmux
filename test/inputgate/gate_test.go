package inputgate

// Black-box tests for the Claude input gate. REQ-n / TEST-n ids refer to
// qa/test-plan.md. The pane is the fake Claude child (testdata/fakeclaude);
// what it RECEIVED is read from its own log, never from magmux.

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// ── REQ-3: readiness-gated send ─────────────────────────────────────────────

// TEST-1 (REQ-3, REQ-4): a send issued before the box exists, with a stdin
// reset AFTER the box appears that eats anything typed into it, waits for
// readiness and then succeeds with evidence. If magmux typed during the
// reset, the text would be lost and no submit would ever be logged.
func TestREQ3_EarlySendWaitsForReadinessThenIsConfirmed(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{
		"FAKE_BOX_MS": "1500", "FAKE_RESET_MS": "2300", "FAKE_RESET_LEN_MS": "200",
	})
	if len(ofKind(m.events(t), "box")) > 0 {
		t.Logf("note: the box was already up before the send (slow start); the reset still follows it")
	}
	text := "early hello " + shortID(t)

	r := cn.send(pane, map[string]any{"text": text})

	res := wantGatedOK(t, r)
	wantField(t, res, "accept", "confirmed", r)
	wantField(t, res, "evidence", "history", r)
	wantField(t, res, "verified", "exact", r)
	if w, _ := res["waitedMs"].(float64); w <= 0 {
		t.Errorf("waitedMs = %v; an early send must report time spent waiting", res["waitedMs"])
	}
	evs := m.events(t)
	resetEnd := ofKind(evs, "reset-end")
	if len(resetEnd) == 0 {
		t.Fatalf("fake never finished its boot reset; log:\n%s", rawLog(m.logPath))
	}
	ins := ofKind(evs, "in")
	if len(ins) == 0 {
		t.Fatalf("nothing reached the child")
	}
	if ins[0].T < resetEnd[0].T {
		t.Errorf("first byte reached the child at +%dms, before the stdin reset ended at +%dms", ins[0].T, resetEnd[0].T)
	}
	for _, e := range ins {
		if e.Dropped {
			t.Errorf("input %q arrived during the stdin reset and was lost", e.Data)
		}
	}
	if got := submits(evs); !slices.Equal(got, []string{text}) {
		t.Errorf("child submitted %q; want exactly [%q]", got, text)
	}
}

// TEST-2 (REQ-3): no box ever appears → busy "starting", nothing typed, and
// the reply comes at the requested deadline, not before.
func TestREQ3_NeverReadyIsBusyStartingAndTypesNothing(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_BOX_MS": "-1", "FAKE_RESET_MS": "-1"})

	began := time.Now()
	r := cn.send(pane, map[string]any{"text": "never " + shortID(t), "readyTimeoutMs": 1500})
	took := time.Since(began)

	wantBusy(t, r, "starting")
	if took < 1400*time.Millisecond {
		t.Errorf("busy after %v; want the 1500ms readiness deadline to be honoured", took)
	}
	wantNothingReceived(t, m)
}

// TEST-3 (REQ-3): the box is visible but the terminal never leaves cooked
// mode (another program owns it, or the tool never took raw) → busy.
func TestREQ3_BoxVisibleButTerminalCookedIsBusy(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_COOKED": "1", "FAKE_BOX_MS": "200"})
	m.waitEvent(t, startTimeout, "box", func(e event) bool { return e.Ev == "box" })

	r := cn.send(pane, map[string]any{"text": "cooked " + shortID(t), "readyTimeoutMs": 2500})

	wantBusy(t, r, "starting", "terminal_not_ready")
	wantNothingReceived(t, m)
}

// TEST-4 (REQ-3, REQ-5): a permission dialog without dismissDialogs → busy
// dialog:permission; nothing at all (not even an Escape) is written.
func TestREQ3_PermissionDialogWithoutDismissIsBusy(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_DIALOG": "permission"})
	m.waitBooted(t)

	r := cn.send(pane, map[string]any{"text": "blocked " + shortID(t), "readyTimeoutMs": 4000})

	wantBusy(t, r, "dialog:permission")
	wantNothingReceived(t, m)
}

// TEST-5 (REQ-3): the question dialog draws `❯ 1. Red` between two rules —
// box-shaped — and must still be recognised as a dialog, not a box.
func TestREQ3_QuestionDialogIsNotMistakenForTheBox(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_DIALOG": "question"})
	m.waitBooted(t)

	r := cn.send(pane, map[string]any{"text": "blocked " + shortID(t), "readyTimeoutMs": 4000})

	wantBusy(t, r, "dialog:question")
	wantNothingReceived(t, m)
}

// TEST-6 (REQ-3): the box already holds the human's draft → busy
// box_not_empty; the draft is not appended to.
func TestREQ3_BoxHoldingTextIsBusyBoxNotEmpty(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_PREFILL": "half typed draft"})
	m.waitBooted(t)

	r := cn.send(pane, map[string]any{"text": "mine " + shortID(t), "readyTimeoutMs": 4000})

	wantBusy(t, r, "box_not_empty")
	wantNothingReceived(t, m)
}

// TEST-7 (REQ-3 edge): the child exits while a send waits → the existing
// dead-pane error, never busy.
func TestREQ3_ChildExitWhileWaitingIsPaneDeadNotBusy(t *testing.T) {
	t.Parallel()
	_, cn, pane := declared(t, fake{"FAKE_BOX_MS": "-1", "FAKE_RESET_MS": "-1", "FAKE_EXIT_MS": "2500"})

	r := cn.send(pane, map[string]any{"text": "orphan " + shortID(t), "readyTimeoutMs": 15000})

	if r.OK || r.Code != codePaneDead {
		t.Fatalf("want code %q when the child exits during the wait, got %s", codePaneDead, r)
	}
}

// TEST-8 (REQ-3 edge, lane order): a second send to the same pane while the
// first waits is delivered after it, in order.
func TestREQ3_SecondSendToSamePaneIsOrderedBehindTheFirst(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, nil)
	id := shortID(t)
	a, b := "first "+id, "second "+id

	chA := cn.async(map[string]any{"type": "send", "pane": pane, "text": a})
	chB := cn.async(map[string]any{"type": "send", "pane": pane, "text": b})
	ra := cn.await(chA, "send A")
	rb := cn.await(chB, "send B")

	wantGatedOK(t, ra)
	wantGatedOK(t, rb)
	if got := submits(m.events(t)); !slices.Equal(got, []string{a, b}) {
		t.Errorf("child submitted %q; want [%q %q] in that order", got, a, b)
	}
}

// ── REQ-2: vim mode is verified, not assumed ────────────────────────────────

// TEST-9 (REQ-2): `-- INSERT --` in the footer → busy vim_mode, nothing typed.
func TestREQ2_InsertFooterRefusesWithVimMode(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_VIM": "1"})
	m.waitBooted(t)

	r := cn.send(pane, map[string]any{"text": "vim " + shortID(t), "readyTimeoutMs": 4000})

	wantBusy(t, r, "vim_mode")
	wantNothingReceived(t, m)
}

// ── REQ-4: acceptance check ─────────────────────────────────────────────────

// TEST-10 (REQ-4, F10): markdown delimiters vanish from the history copy;
// the prompt is still confirmed from history.
func TestREQ4_MarkdownPromptIsStillConfirmed(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, nil)
	text := "read `ai-docs/turn-1.md` **now** and *then* ~~later~~ keep_under " + shortID(t)

	r := cn.send(pane, map[string]any{"text": text})

	res := wantGatedOK(t, r)
	wantField(t, res, "accept", "confirmed", r)
	wantField(t, res, "evidence", "history", r)
	wantField(t, res, "verified", "exact", r)
	if got := submits(m.events(t)); !slices.Equal(got, []string{text}) {
		t.Errorf("child submitted %q; want exactly [%q] (the raw text, once)", got, text)
	}
}

// TEST-11 (REQ-4, edge "very long text"): a prompt that wraps over several
// box rows is verified and confirmed, not misread.
func TestREQ4_LongWrappedPromptIsConfirmed(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, nil)
	text := strings.Repeat("the quick brown fox jumps over the lazy dog ", 7) + shortID(t)

	r := cn.send(pane, map[string]any{"text": text})

	res := wantGatedOK(t, r)
	wantField(t, res, "accept", "confirmed", r)
	wantField(t, res, "verified", "exact", r)
	if got := submits(m.events(t)); !slices.Equal(got, []string{text}) {
		t.Errorf("child submitted %q; want exactly [%q]", got, text)
	}
}

// TEST-12 (REQ-4): the first Enter is swallowed, the text stays in the box →
// magmux re-sends Enter ONCE and reports resent_enter.
func TestREQ4_StuckBoxIsRescuedByOneResentEnter(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_IGNORE_ENTERS": "1"})
	text := "stuck once " + shortID(t)

	r := cn.send(pane, map[string]any{"text": text})

	res := wantGatedOK(t, r)
	wantField(t, res, "accept", "resent_enter", r)
	wantField(t, res, "evidence", "history", r)
	evs := m.events(t)
	if n := countEnter(received(evs)); n != 2 {
		t.Errorf("child received %d Enters; want exactly 2 (the submit and one re-send)", n)
	}
	if got := submits(evs); !slices.Equal(got, []string{text}) {
		t.Errorf("child submitted %q; want exactly [%q]", got, text)
	}
}

// TEST-13 (REQ-4): every Enter is swallowed → not_accepted still_in_box,
// typed:true, Enter pressed exactly twice, never more.
func TestREQ4_TextThatStaysInTheBoxIsStillInBox(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_IGNORE_ENTERS": "99"})

	r := cn.send(pane, map[string]any{"text": "stuck " + shortID(t)})

	if r.OK || r.Code != codeNotAccepted || r.Reason != "still_in_box" {
		t.Fatalf("want not_accepted/still_in_box, got %s", r)
	}
	if !r.typedTrue() {
		t.Errorf("still_in_box must carry typed:true; reply: %s", r)
	}
	if n := countEnter(received(m.events(t))); n != 2 {
		t.Errorf("child received %d Enters; want exactly 2 (one re-send, no more)", n)
	}
}

// TEST-14 (REQ-4, contract §5.4): the box empties but no history entry ever
// appears → not_accepted unconfirmed, carrying code, reason, typed and error.
func TestREQ4_NoHistoryEntryIsUnconfirmedWithEveryContractField(t *testing.T) {
	t.Parallel()
	_, cn, pane := declared(t, fake{"FAKE_NO_HISTORY": "1"})

	r := cn.send(pane, map[string]any{"text": "ghost " + shortID(t)})

	if r.OK || r.Code != codeNotAccepted || r.Reason != "unconfirmed" {
		t.Fatalf("want not_accepted/unconfirmed, got %s", r)
	}
	keys := r.topKeys(t)
	for _, k := range []string{"code", "reason", "typed", "error"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("not_accepted reply lacks %q; reply: %s", k, r)
		}
	}
	if !r.typedTrue() {
		t.Errorf("unconfirmed must carry typed:true; reply: %s", r)
	}
	if strings.TrimSpace(r.Error) == "" {
		t.Errorf("not_accepted reply has an empty error message; reply: %s", r)
	}
}

// TEST-15 (REQ-4, amended by review r3 item 5): the child resets stdin as the
// text arrives and the text is LOST, leaving the box completely empty. Nothing
// was submitted and nothing partial is left, so magmux retypes it ONCE; the
// reply is ok only with retyped:true, the text reached the child undropped
// exactly once, one Enter was pressed, and the child submitted exactly the
// text. (Lost twice is text_mismatch — the fake loses only the first chunk,
// so that half is pinned in package mux, TestEmptiedBoxTwiceIsTextMismatch.)
func TestREQ4_LostTextIsRetypedOnceThenConfirmed(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_LOSE_ON_INPUT": "1", "FAKE_LOSE_LEN_MS": "400"})
	text := "lost " + shortID(t)

	r := cn.send(pane, map[string]any{"text": text})

	res := wantGatedOK(t, r)
	wantField(t, res, "retyped", true, r)
	wantField(t, res, "retypeCause", "terminal_reset", r)
	evs := m.events(t)
	var kept []string
	for _, e := range ofKind(evs, "in") {
		if !e.Dropped && e.Data == text {
			kept = append(kept, e.Data)
		}
	}
	if len(kept) != 1 {
		t.Errorf("the text reached the child undropped %d times, want exactly 1 (the retype)", len(kept))
	}
	if n := countEnter(received(evs)); n != 1 {
		t.Errorf("child received %d Enters, want 1", n)
	}
	if got := submits(evs); len(got) != 1 || got[0] != text {
		t.Errorf("child submitted %q, want exactly [%q]", got, text)
	}
}

// TEST-15c (REQ-4, the V1 rerun's silent drop): the child discards the FIRST
// text it receives once the box is up, with NO termios change and NO redraw —
// the box never shows a rune of it. A box blank on every read for the whole
// 3 s verify window is a silent drop, not a slow draw (render lag tops out
// well under a second), so magmux retypes it ONCE: ok with retyped:true and
// retypeCause:"never_drawn", the text dropped silently once and kept once,
// one Enter, and the child submitted exactly the text.
func TestREQ4_SilentlyDroppedTextIsRetypedOnce(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_DROP_SILENT_ONCE": "1"})
	text := "silent " + shortID(t)

	r := cn.send(pane, map[string]any{"text": text})

	res := wantGatedOK(t, r)
	wantField(t, res, "retyped", true, r)
	wantField(t, res, "retypeCause", "never_drawn", r)
	evs := m.events(t)
	var kept, silent int
	for _, e := range ofKind(evs, "in") {
		if e.Data != text {
			continue
		}
		switch {
		case e.Silent:
			silent++
		case !e.Dropped:
			kept++
		}
	}
	if silent != 1 || kept != 1 {
		t.Errorf("the text was silently dropped %d times and kept %d times, want 1 and 1", silent, kept)
	}
	if n := len(ofKind(evs, "reset-start")); n > 1 {
		t.Errorf("%d resets, want only the boot reset: the drop must not be a termios event", n)
	}
	if n := countEnter(received(evs)); n != 1 {
		t.Errorf("child received %d Enters, want 1", n)
	}
	if got := submits(evs); len(got) != 1 || got[0] != text {
		t.Errorf("child submitted %q, want exactly [%q]", got, text)
	}
}

// TEST-15b (REQ-4, the V1 render-lag defect): the child applies the text at
// once but draws it 600 ms late, as Claude Code's frame does under load. The
// box reads empty for a while after the text arrived — a slow frame, not lost
// text — so magmux must wait for it: ok, verified, NO retype, the text
// reached the child exactly once, and the child submitted it exactly once.
func TestREQ4_RenderLagIsWaitedForNotRetyped(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_RENDER_DELAY_MS": "600"})
	text := "slow " + shortID(t)

	r := cn.send(pane, map[string]any{"text": text})

	res := wantGatedOK(t, r)
	if _, ok := res["retyped"]; ok {
		t.Errorf("a slow render was retyped; reply: %s", r)
	}
	evs := m.events(t)
	if n := strings.Count(received(evs), text); n != 1 {
		t.Errorf("the text reached the child %d times, want exactly 1", n)
	}
	if n := countEnter(received(evs)); n != 1 {
		t.Errorf("child received %d Enters, want 1", n)
	}
	if got := submits(evs); len(got) != 1 || got[0] != text {
		t.Errorf("child submitted %q, want exactly [%q]", got, text)
	}
}

// TEST-16 (REQ-4, class TYPE): text with enter:false is typed and verified,
// replies in_box, and nothing is submitted.
func TestREQ4_TypeWithoutEnterRepliesInBox(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, nil)
	text := "draft " + shortID(t)

	r := cn.send(pane, map[string]any{"text": text, "enter": false})

	res := wantGatedOK(t, r)
	wantField(t, res, "accept", "in_box", r)
	wantField(t, res, "verified", "exact", r)
	if _, ok := res["evidence"]; ok {
		t.Errorf("evidence is SUBMIT-only; a TYPE reply carries it: %s", r)
	}
	evs := m.events(t)
	if got := submits(evs); len(got) != 0 {
		t.Errorf("child submitted %q; a TYPE send must not submit", got)
	}
	if !strings.Contains(cn.capture(pane), "❯ "+text) {
		t.Errorf("the box does not show %q; capture:\n%s", text, cn.capture(pane))
	}
}

// ── REQ-5: dialog handling, opt-in ──────────────────────────────────────────

// TEST-17 (REQ-5): with dismissDialogs the dialog is closed with Escape ONLY
// (never approved), the box returns, the text is typed and accepted.
func TestREQ5_DismissDialogsEscapesThenTypesAndIsAccepted(t *testing.T) {
	tests := []struct {
		name   string
		dialog string
	}{
		{name: "permission", dialog: "permission"},
		{name: "question", dialog: "question"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, cn, pane := declared(t, fake{"FAKE_DIALOG": tt.dialog})
			m.waitEvent(t, startTimeout, "dialog shown", func(e event) bool { return e.Ev == "dialog-shown" })
			text := "after dialog " + shortID(t)

			r := cn.send(pane, map[string]any{"text": text, "dismissDialogs": true})

			res := wantGatedOK(t, r)
			wantField(t, res, "accept", "confirmed", r)
			d, _ := res["dismissed"].([]any)
			if len(d) != 1 || d[0] != tt.dialog {
				t.Errorf("result.dismissed = %v; want [%q]", res["dismissed"], tt.dialog)
			}
			evs := m.events(t)
			if a := ofKind(evs, "dialog-approved"); len(a) > 0 {
				t.Fatalf("the dialog was APPROVED (key %q); dismissal must only ever Escape", a[0].Key)
			}
			if len(ofKind(evs, "dialog-closed")) != 1 {
				t.Errorf("want exactly one dialog-closed (by Escape); log:\n%s", rawLog(m.logPath))
			}
			all := received(evs)
			before, _, found := strings.Cut(all, text)
			if !found {
				t.Fatalf("the text never reached the child; received %q", all)
			}
			if before == "" || strings.Trim(before, "\x1b") != "" {
				t.Errorf("bytes before the text = %q; want one or more Escapes and nothing else", before)
			}
			if got := submits(evs); !slices.Equal(got, []string{text}) {
				t.Errorf("child submitted %q; want exactly [%q]", got, text)
			}
		})
	}
}

// ── request validation on a gated pane (contract §5.3) ──────────────────────

// TEST-18 (REQ-10): Enter not last, two Enters, an unknown key, a negative
// readyTimeoutMs → bad_request with nothing written.
func TestREQ10_MalformedSendOnGatedPaneIsBadRequestAndWritesNothing(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, nil)
	m.waitBooted(t)

	tests := []struct {
		name   string
		fields map[string]any
	}{
		{name: "enter not the last key", fields: map[string]any{"keys": []string{"enter", "h"}, "enter": false}},
		{name: "return not the last key", fields: map[string]any{"keys": []string{"h", "return", "i"}, "enter": false}},
		{name: "final enter key plus enter:true", fields: map[string]any{"keys": []string{"h", "i", "enter"}, "enter": true}},
		{name: "final enter key plus default enter", fields: map[string]any{"text": "x", "keys": []string{"enter"}}},
		{name: "unknown key name", fields: map[string]any{"keys": []string{"h", "nosuchkey"}, "enter": false}},
		{name: "negative readyTimeoutMs", fields: map[string]any{"text": "x", "readyTimeoutMs": -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := cn.send(pane, tt.fields)
			if r.OK || r.Code != codeBadRequest {
				t.Fatalf("want %q, got %s", codeBadRequest, r)
			}
		})
	}
	wantNothingReceived(t, m)
}

// TEST-19 (REQ-10, positive control for TEST-18): a final enter key with
// enter:false is a valid SUBMIT and is delivered.
func TestREQ10_FinalEnterKeyIsAValidSubmit(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, nil)

	r := cn.send(pane, map[string]any{"keys": []string{"h", "i", "enter"}, "enter": false})

	res := wantGatedOK(t, r)
	if a := res["accept"]; a != "confirmed" && a != "resent_enter" {
		t.Errorf("result.accept = %v; want a SUBMIT outcome; reply: %s", a, r)
	}
	if got := submits(m.events(t)); !slices.Equal(got, []string{"hi"}) {
		t.Errorf("child submitted %q; want [\"hi\"]", got)
	}
}

// ── KEYS class and noGate are not gated (contract §5.3) ─────────────────────

// TEST-20 (REQ-11): keys-only with enter:false on a dialog is delivered
// immediately — a KEYS send is how a caller answers a dialog itself.
func TestREQ11_KeysOnlyOnDialogIsDeliveredNotGated(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_DIALOG": "permission"})
	m.waitEvent(t, startTimeout, "dialog shown", func(e event) bool { return e.Ev == "dialog-shown" })

	r := cn.send(pane, map[string]any{"keys": []string{"escape"}, "enter": false, "readyTimeoutMs": 3000})

	if !r.OK {
		t.Fatalf("a KEYS send must not be gated; got %s", r)
	}
	m.waitEvent(t, 5*time.Second, "dialog-closed", func(e event) bool { return e.Ev == "dialog-closed" })
	if a := ofKind(m.events(t), "dialog-approved"); len(a) > 0 {
		t.Errorf("escape approved the dialog (key %q)", a[0].Key)
	}
}

// TEST-21 (REQ-11): noGate restores today's behaviour — text is written even
// into a dialog, with no readiness wait.
func TestREQ11_NoGateWritesEvenIntoADialog(t *testing.T) {
	t.Parallel()
	m, cn, pane := declared(t, fake{"FAKE_DIALOG": "permission"})
	m.waitEvent(t, startTimeout, "dialog shown", func(e event) bool { return e.Ev == "dialog-shown" })
	text := "zz" + shortID(t)

	r := cn.send(pane, map[string]any{"text": text, "enter": false, "noGate": true, "readyTimeoutMs": 3000})

	if !r.OK {
		t.Fatalf("noGate send must be delivered; got %s", r)
	}
	m.waitEvent(t, 5*time.Second, "the noGate text", func(e event) bool {
		return e.Ev == "in" && strings.Contains(e.Data, text)
	})
}

// ── REQ-6: panes without a Claude controller are unchanged ──────────────────

// TEST-22 (REQ-6): an undeclared plain `sh` pane: plain ok, the bytes reach
// the shell, no gate fields in the reply, no gate fields in list.
func TestREQ6_UndeclaredShellPaneIsUngated(t *testing.T) {
	t.Parallel()
	m := start(t, opts{cmds: []string{"sh"}})
	cn := dial(t, m)
	p := cn.sessionPane()
	id := shortID(t)
	// The marker is COMPUTED by the shell, so the typed command line can
	// never satisfy the wait by itself.
	want := "IG-42-" + id

	r := cn.send(p.Pane, map[string]any{"text": "echo IG-$((6*7))-" + id})

	if !r.OK {
		t.Fatalf("plain send to an undeclared pane: want ok, got %s", r)
	}
	res := r.result(t)
	for _, k := range gateResultKeys {
		if _, ok := res[k]; ok {
			t.Errorf("ungated reply carries gate key %q: %s", k, r)
		}
	}
	for _, k := range []string{"gateBasis", "input"} {
		if _, ok := cn.row(p.Pane).raw[k]; ok {
			t.Errorf("list row of an undeclared pane carries %q: %v", k, cn.row(p.Pane).raw)
		}
	}
	deadline := time.Now().Add(startTimeout)
	for {
		for _, line := range strings.Split(cn.capture(p.Pane), "\n") {
			if strings.TrimSpace(line) == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell never printed %q; capture:\n%s", want, cn.capture(p.Pane))
		}
		time.Sleep(pollEvery)
	}
}

// TEST-23 (REQ-6): an undeclared non-claude program showing a dialog gets
// today's behaviour — the text is written straight away, no busy.
func TestREQ6_UndeclaredAgentPaneIsNotGatedEvenOnADialog(t *testing.T) {
	t.Parallel()
	m := start(t, opts{cmds: []string{agentBin}, env: fake{"FAKE_DIALOG": "permission"}})
	cn := dial(t, m)
	pane := cn.sessionPane().Pane
	m.waitEvent(t, startTimeout, "dialog shown", func(e event) bool { return e.Ev == "dialog-shown" })
	text := "plain" + shortID(t)

	r := cn.send(pane, map[string]any{"text": text, "enter": false})

	if !r.OK {
		t.Fatalf("undeclared pane: want plain ok, got %s", r)
	}
	res := r.result(t)
	for _, k := range gateResultKeys {
		if _, ok := res[k]; ok {
			t.Errorf("ungated reply carries gate key %q: %s", k, r)
		}
	}
	m.waitEvent(t, 5*time.Second, "the plain text", func(e event) bool {
		return e.Ev == "in" && strings.Contains(e.Data, text)
	})
}

// ── REQ-7: observable state ─────────────────────────────────────────────────

// TEST-24 (REQ-7, CLI position independence): `-e X --controller
// claude-code` declares the pane; list shows controller, gateBasis and an
// input view that becomes ready.
func TestREQ7_DeclaredPaneListsGateStateAndBecomesReady(t *testing.T) {
	t.Parallel()
	m := start(t, opts{cmds: []string{agentBin}, tail: []string{"--controller", "claude-code"}})
	cn := dial(t, m)
	p := cn.sessionPane()

	if p.Controller != "claude-code" {
		t.Errorf("list controller = %q; want claude-code; row: %v", p.Controller, p.raw)
	}
	if p.GateBasis != "declared" {
		t.Errorf("list gateBasis = %q; want declared; row: %v", p.GateBasis, p.raw)
	}
	cn.waitRow(p.Pane, startTimeout, "input.ready", func(r paneRow) bool {
		return r.Input != nil && r.Input.Ready
	})
}

// TEST-25 (REQ-7, REQ-2): the vim footer is visible in the published view.
func TestREQ7_VimFooterIsVisibleInTheInputView(t *testing.T) {
	t.Parallel()
	_, cn, pane := declared(t, fake{"FAKE_VIM": "1"})

	cn.waitRow(pane, startTimeout, "input.reason vim_mode", func(r paneRow) bool {
		return r.Input != nil && !r.Input.Ready && r.Input.Reason == "vim_mode"
	})
}

// ── REQ-1: forced normal editor mode on direct Claude launches ──────────────

// TEST-26 (REQ-1, F1): a direct `claude` launch runs with exactly ONE
// --settings carrying editorMode "normal", with the caller's keys merged in.
func TestREQ1_DirectClaudeLaunchGetsOneMergedNormalEditorSettings(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		wantKeys map[string]any
	}{
		{name: "no caller settings", cmd: directBin},
		{name: "caller settings, separate arg", cmd: directBin + ` --settings '{"theme":"light"}'`, wantKeys: map[string]any{"theme": "light"}},
		{name: "caller settings, = form", cmd: directBin + ` --settings='{"theme":"light"}'`, wantKeys: map[string]any{"theme": "light"}},
		{name: "caller asks for vim", cmd: directBin + ` --settings '{"editorMode":"vim","theme":"light"}'`, wantKeys: map[string]any{"theme": "light"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := start(t, opts{cmds: []string{tt.cmd}})
			argv := m.waitEvent(t, startTimeout, "argv", func(e event) bool { return e.Ev == "argv" }).Args

			vals := settingsArgs(argv)
			if len(vals) != 1 {
				t.Fatalf("claude argv %q carries %d --settings; want exactly 1 (a second one REPLACES the first)", argv, len(vals))
			}
			obj := settingsObject(t, vals[0])
			if obj["editorMode"] != "normal" {
				t.Errorf("effective settings %v: editorMode = %v; want \"normal\"", obj, obj["editorMode"])
			}
			for k, v := range tt.wantKeys {
				if obj[k] != v {
					t.Errorf("effective settings %v lost the caller's %q = %v", obj, k, v)
				}
			}
		})
	}
}

// TEST-27 (REQ-1): a direct claude launch is gated with basis "direct".
func TestREQ1_DirectClaudeLaunchIsGatedDirect(t *testing.T) {
	t.Parallel()
	m := start(t, opts{cmds: []string{directBin}})
	cn := dial(t, m)

	if p := cn.sessionPane(); p.GateBasis != "direct" {
		t.Errorf("list gateBasis = %q; want direct; row: %v", p.GateBasis, p.raw)
	}
}

// TEST-28 (REQ-1): a pane nobody declared and that is not claude is untouched.
func TestREQ1_UndeclaredNonClaudePaneArgvIsUntouched(t *testing.T) {
	t.Parallel()
	m := start(t, opts{cmds: []string{agentBin + " --keep me"}})

	argv := m.waitEvent(t, startTimeout, "argv", func(e event) bool { return e.Ev == "argv" }).Args

	if !slices.Equal(argv, []string{"--keep", "me"}) {
		t.Errorf("argv = %q; want exactly [--keep me]", argv)
	}
}

// ── REQ-9: the --controller flag (contract §5.1) ────────────────────────────

// TEST-29 (REQ-9): unknown controller and no panes are startup errors.
func TestREQ9_ControllerFlagErrorsExitOne(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "unknown value",
			args:    []string{"--controller", "bogus", "-e", "sh"},
			wantErr: `magmux: unknown controller "bogus" (known: claude-code)`,
		},
		{
			name:    "no -e or -g panes",
			args:    []string{"--controller", "claude-code"},
			wantErr: "magmux: --controller applies to -e/-g panes and there are none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"--headless", "--no-status", "--id", shortID(t), "--sock-dir", tmpDir(t, "igs")}, tt.args...)

			code, stderr := runCLI(t, args...)

			if code != 1 {
				t.Errorf("exit code = %d; want 1; stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr does not contain %q:\n%s", tt.wantErr, stderr)
			}
		})
	}
}
