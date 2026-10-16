package mux

// Phase 5: the gated send. Classes and the Enter-position rule [B], the
// pre-submit verify [A](1), the synchronous decisions [D], dialogs (R5) and
// the plumbing (lanes, Quiesce, the typing token, NotifyInput). Acceptance
// and the deadline/budget rules [E] are in inputgate_accept_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/pty"
)

func hubCaller() hub.Caller { return hub.Caller{Transport: "test"} }

// ── R6: an ungated send is today's, byte for byte ───────────────────────────

// TestUngatedSendIsByteIdentical: on a pane nobody gates, on a gated pane with
// noGate, and for a KEYS send on a gated pane, the PTY bytes, the reply line
// and the panel's ack are exactly what they were before the gate existed.
func TestUngatedSendIsByteIdentical(t *testing.T) {
	const (
		wantBytes = "ls -la\t\r"
		wantReply = `{"id":"1","ok":true,"result":{"bytes":6,"enter":true,"keys":1,"pane":0},"type":"reply"}` + "\n"
		wantAck   = "6 bytes + 1 keys + enter"
	)
	never := &fakeGate{
		readyFn: func(ReadinessQuery, int) InputReadiness { t.Error("the gate was consulted"); return readyNow() },
		checkFn: func(BoxExpect, int) BoxCheck { t.Error("the box was checked"); return verifiedBox(verifiedExact) },
	}
	for _, c := range []struct {
		name  string
		gate  bool
		extra map[string]any
	}{
		{"ungated pane", false, nil},
		{"gated pane, noGate", true, map[string]any{"noGate": true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, child, dial := sockMux(t)
			p := m.allPanes[0]
			if c.gate {
				gateSockPane(m, p, never)
			}
			conn := dial()
			rd := newSockReader(t, conn)
			msg := map[string]any{"type": "send", "pane": 0, "text": "ls -la", "keys": []string{"tab"}, "id": "1"}
			for k, v := range c.extra {
				msg[k] = v
			}
			writeLines(t, conn, msg)
			_, line := rd.reply("1", 5*time.Second)
			if line != wantReply {
				t.Errorf("reply line\n got %q\nwant %q", line, wantReply)
			}
			if got := childQuiet(t, child, 100*time.Millisecond); got != wantBytes {
				t.Errorf("PTY bytes %q, want %q", got, wantBytes)
			}
			if got := lastAck(m); got != wantAck {
				t.Errorf("ack %q, want %q", got, wantAck)
			}
		})
	}
}

// TestKeysOnlySendIsNotGated (r3 H1): keys with no text and enter:false are
// KEYS whatever the keys, printable included — so a numbered dialog can be
// answered deliberately, and nothing is verified.
func TestKeysOnlySendIsNotGated(t *testing.T) {
	for _, state := range []InputReadiness{dialogUp("permission", true), notReady(protocol.ReasonBoxNotEmpty)} {
		for _, keys := range [][]string{{"2"}, {"a", "b"}} {
			t.Run(fmt.Sprintf("%s/%v", state.Reason, keys), func(t *testing.T) {
				g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return state }}
				m, _, log := gatedMux(t, g)
				o := runSend(t, m, sendRequest{keys: keys})
				if o.err != nil || o.rep.Gated {
					t.Fatalf("err=%v gated=%v, want an ungated ok", o.err, o.rep.Gated)
				}
				if got, want := log.settle(), strings.Join(keys, ""); got != want {
					t.Errorf("bytes %q, want %q", got, want)
				}
				if r, c, a := g.counts(); r+c+a != 0 {
					t.Errorf("the gate was consulted: ready=%d check=%d accept=%d", r, c, a)
				}
			})
		}
	}
}

// ── [B] the Enter-position rule ─────────────────────────────────────────────

func TestEnterNotFinalIsBadRequest(t *testing.T) {
	for _, r := range []sendRequest{
		{keys: []string{"enter", "x"}, enter: false},
		{keys: []string{"a", "enter"}, enter: true}, // two Enters
		{keys: []string{"return", "a"}, enter: false},
		{text: "hi", keys: []string{"frobnicate"}, enter: true}, // unknown key
	} {
		t.Run(fmt.Sprintf("%v/%v", r.keys, r.enter), func(t *testing.T) {
			g := &fakeGate{}
			m, _, log := gatedMux(t, g)
			o := runSend(t, m, r)
			wantErr(t, o, protocol.CodeBadRequest, "", false)
			if got := log.settle(); got != "" {
				t.Errorf("a refused send wrote %q", got)
			}
			if n := outRows(m); n != 0 {
				t.Errorf("a refused send filed %d OUT rows", n)
			}
			if r, c, a := g.counts(); r+c+a != 0 || g.notified.Load() != 0 {
				t.Errorf("a refused send reached the gate")
			}
		})
	}
}

// TestTrailingEnterKeyIsTheSubmit: a trailing enter key takes enter:true's
// path — the settle, CheckBox, the one guarded \r, acceptance.
func TestTrailingEnterKeyIsTheSubmit(t *testing.T) {
	g := &fakeGate{}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{keys: []string{"h", "i", "enter"}, enter: false})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hi\r" {
		t.Errorf("bytes %q, want %q (one \\r)", got, "hi\r")
	}
	evs, seen := g.eventLog()
	for i, ev := range evs {
		if ev == "check" && strings.Contains(seen[i], "\r") {
			t.Errorf("the \\r was written before the box check: %q", seen[i])
		}
	}
	if !o.rep.enter || o.rep.keys != 2 || o.rep.accept != acceptConfirmed {
		t.Errorf("report %+v, want a submit of 2 keys, confirmed", o.rep)
	}
}

// TestKeysThenEnterVerifiedOnBoxAfterKeys: the verify judges the box AFTER
// the keys, with Exact = their runes, and the baseline comes from that read.
func TestKeysThenEnterVerifiedOnBoxAfterKeys(t *testing.T) {
	g := &fakeGate{}
	m, _, _ := gatedMux(t, g)
	o := runSend(t, m, sendRequest{keys: []string{"h", "i"}, enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	w := g.lastExpect()
	if w.Class != ClassSubmit || w.Exact != "hi" || !w.TypedText {
		t.Errorf("expect %+v, want a submit verifying exactly \"hi\"", w)
	}
	evs, seen := g.eventLog()
	for i, ev := range evs {
		if ev == "check" && seen[i] != "hi" {
			t.Errorf("the box was checked on %q, want after both keys", seen[i])
		}
	}
	if o.rep.verified != verifiedExact {
		t.Errorf("verified %q", o.rep.verified)
	}
}

func TestEnterOnlyOnEmptyBoxIsEmptyBox(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		return BoxCheck{Readiness: readyNow(), Verdict: BoxEmpty}
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonEmptyBox, false)
	if got := log.settle(); got != "" {
		t.Errorf("an Enter-only send on an empty box wrote %q", got)
	}
	if q := g.queries[0]; q.Types || q.Class != ClassSubmit {
		t.Errorf("query %+v, want a submit that types nothing", q)
	}
	if w := g.lastExpect(); w.Exact != "" || w.TypedText {
		t.Errorf("expect %+v, want undetermined", w)
	}
}

// TestSubmitIntoDialogRefused: {"keys":["2"]} with the default enter is a
// SUBMIT, and Enter on a permission dialog would approve option 1.
func TestSubmitIntoDialogRefused(t *testing.T) {
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return dialogUp("permission", true) }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{keys: []string{"2"}, enter: true, readyTimeoutMs: 200})
	wantErr(t, o, protocol.CodeBusy, "dialog:permission", false)
	if got := log.settle(); got != "" {
		t.Errorf("bytes %q, want none", got)
	}
}

// ── [A](1) the pre-submit verify ────────────────────────────────────────────

func TestPreSubmitMismatchWithholdsEnter(t *testing.T) {
	text := strings.Repeat("abcdefgh ", 12)
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		return BoxCheck{Readiness: readyNow(), Verdict: BoxMismatch, Detail: "the box holds 70 of 96 non-space runes"}
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	got := log.settle()
	if strings.Contains(got, "\r") {
		t.Errorf("Enter was pressed on a mismatched box: %q", got)
	}
	if !strings.Contains(o.err.Error(), "70 of 96") {
		t.Errorf("the message drops the detail: %v", o.err)
	}
	if _, _, a := g.counts(); a != 0 {
		t.Errorf("acceptance ran after a withheld Enter")
	}
	if ack := lastAck(m); !strings.Contains(ack, "not submitted: text mismatch") {
		t.Errorf("ack %q", ack)
	}
}

func TestTypeOnlyVerifiedInBox(t *testing.T) {
	t.Run("verified", func(t *testing.T) {
		g := &fakeGate{}
		m, _, log := gatedMux(t, g)
		o := runSend(t, m, sendRequest{text: "/compact", enter: false})
		if o.err != nil {
			t.Fatalf("send: %v", o.err)
		}
		if o.rep.accept != acceptInBox || o.rep.verified != verifiedExact {
			t.Errorf("report %+v, want in_box / exact", o.rep)
		}
		if got := log.settle(); got != "/compact" {
			t.Errorf("bytes %q", got)
		}
		if w := g.lastExpect(); w.Class != ClassType || w.Exact != "/compact" {
			t.Errorf("expect %+v", w)
		}
		if _, _, a := g.counts(); a != 0 {
			t.Errorf("a TYPE ran acceptance")
		}
	})
	t.Run("lost", func(t *testing.T) {
		g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return BoxCheck{Readiness: readyNow(), Verdict: BoxEmpty} }}
		m, _, _ := gatedMux(t, g)
		o := runSend(t, m, sendRequest{text: "/compact", enter: false})
		wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	})
}

// TestPasteVerifiedByNewToken: a paste is expected to collapse, verified by a
// token above the post-token MaxPaste — and, per the phase 3 finding, it still
// carries Exact, because history shows a paste expanded.
func TestPasteVerifiedByNewToken(t *testing.T) {
	text := "line one\nline two\nline three"
	g := &fakeGate{
		readyFn: func(ReadinessQuery, int) InputReadiness { r := readyNow(); r.MaxPaste = 4; return r },
		checkFn: func(BoxExpect, int) BoxCheck { return verifiedBox(verifiedPasteToken) },
	}
	m, p, log := gatedMux(t, g)
	p.bracketPaste = false // the gate, not admit, owns the bracketed-paste rule now
	o := runSend(t, m, sendRequest{mode: sendPaste, text: text, enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	w := g.lastExpect()
	if !w.Pasted || w.PasteAbove != 4 || w.Exact != text {
		t.Errorf("expect %+v, want Pasted above 4 with the full text", w)
	}
	if q := g.queries[0]; !q.Paste || !q.Types {
		t.Errorf("query %+v, want Paste and Types", q)
	}
	if got := log.settle(); got != "\x1b[200~"+text+"\x1b[201~\r" {
		t.Errorf("bytes %q", got)
	}
	if o.rep.verified != verifiedPasteToken {
		t.Errorf("verified %q", o.rep.verified)
	}
}

// TestLongAutoSendVerifiedByToken (r3 H2): a 1,000-rune single-line AUTO
// send is one unbracketed write over pasteCollapseRunes, which Claude shows as
// `[Pasted text #N`. Against the REAL controller and the real captures: a new
// token verifies (paste_token) and the expanded history entry confirms; the
// same box showing the full text verifies exact.
func TestLongAutoSendVerifiedByToken(t *testing.T) {
	if n := len([]rune(sentAutoLong)); n <= pasteCollapseRunes {
		t.Fatalf("the capture's send is %d runes, not over the threshold", n)
	}
	t.Run("token", func(t *testing.T) {
		start := claudePaneAt(t, "s1-box-history", "empty-box-placeholder")
		m, s, log := claudeMux(t, start)
		inBox := claudePaneAt(t, "s3-paste", "auto-long-in-box")
		submitted := claudePaneAt(t, "s3-paste", "auto-long-submitted")
		s.beforeCheck = func(int) { setScreen(start, inBox) }
		s.beforeAccept = func(int) { setScreen(start, submitted) }
		o := runSend(t, m, sendRequest{text: sentAutoLong, enter: true})
		if o.err != nil {
			t.Fatalf("send: %v (ack %q)", o.err, lastAck(m))
		}
		if o.rep.verified != verifiedPasteToken || o.rep.accept != acceptConfirmed || o.rep.evidence != evidenceHistory {
			t.Errorf("report %+v, want paste_token / confirmed / history", o.rep)
		}
		if got := log.settle(); got != sentAutoLong+"\r" {
			t.Errorf("the auto send was not one unbracketed write and one Enter (%d bytes)", len(got))
		}
	})
	t.Run("full text", func(t *testing.T) {
		long := strings.Repeat("word ", 170) + "end"
		p := claudeLines(30, 100, nil, long, "")
		g, _ := gatedClaudeOn(p)
		c := g.CheckBox(BoxExpect{Class: ClassSubmit, Exact: long, TypedText: true, Pasted: true})
		if c.Verdict != BoxVerified || c.Verified != verifiedExact {
			t.Errorf("a literal long box: %+v, want exact", c)
		}
	})
}

// ── [D] every decision samples synchronously ────────────────────────────────

// TestEveryDecisionSamplesSynchronously: with the sampler closed (no
// goroutine), every termios read is a decision's own — one per gate
// iteration, one at the post-token check, one per CheckBox, and none in
// acceptance, which decides no write.
func TestEveryDecisionSamplesSynchronously(t *testing.T) {
	for _, cookedFirst := range []int{0, 2} {
		t.Run(fmt.Sprintf("cooked for %d", cookedFirst), func(t *testing.T) {
			p := claudePaneAt(t, "s1-box-history", "empty-box-placeholder")
			inBox := claudePaneAt(t, "s1-box-history", "box-wrapped-text")
			taken := claudePaneAt(t, "s1-box-history", "submitted-working")
			m, s, _ := claudeMux(t, p)
			n := 0
			var mu sync.Mutex
			s.tty.read = func() (pty.Mode, error) {
				mu.Lock()
				defer mu.Unlock()
				n++
				if n <= cookedFirst {
					return modeCooked, nil // a reset in progress at the demand
				}
				return modeRaw, nil
			}
			s.tty.close() // no hot sampler: every read below is a decision's
			reads := func() int { mu.Lock(); defer mu.Unlock(); return n }
			var atCheck, readiesAtCheck, atAccept int
			s.beforeCheck = func(int) {
				atCheck = reads()
				s.mu.Lock()
				readiesAtCheck = s.nReady
				s.mu.Unlock()
				setScreen(p, inBox)
			}
			s.beforeAccept = func(k int) {
				if k == 0 {
					atAccept = reads()
				}
				setScreen(p, taken)
			}
			o := runSend(t, m, sendRequest{text: sentFrance, enter: true})
			if o.err != nil {
				t.Fatalf("send: %v", o.err)
			}
			// One read per readiness demand: every gate iteration (the cooked
			// ones, the settle after the reset, the ready one) and the
			// post-token re-ask. Then one for the CheckBox, none for acceptance.
			if atCheck != readiesAtCheck || readiesAtCheck < cookedFirst+2 {
				t.Errorf("reads before the box check = %d for %d readiness demands (want equal, at least %d)",
					atCheck, readiesAtCheck, cookedFirst+2)
			}
			if atAccept != atCheck+1 || reads() != atCheck+1 {
				t.Errorf("reads at acceptance = %d, total %d; want %d (CheckBox 1, acceptance 0)", atAccept, reads(), atCheck+1)
			}
		})
	}
}

// TestResetBeforeFirstByteReturnsToGate: a cooked terminal at the post-token
// check sends the delivery back to the gate with zero bytes written.
func TestResetBeforeFirstByteReturnsToGate(t *testing.T) {
	g := &fakeGate{readyFn: func(_ ReadinessQuery, n int) InputReadiness {
		if n == 1 {
			return notReady(protocol.ReasonTerminalNotReady)
		}
		return readyNow()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	evs, seen := g.eventLog()
	readies := 0
	for i, ev := range evs {
		if ev != "ready" {
			continue
		}
		readies++
		if readies <= 3 && seen[i] != "" {
			t.Errorf("ready call %d saw bytes %q; nothing may be written before the post-token check passes", readies, seen[i])
		}
	}
	if readies < 4 {
		t.Errorf("%d ready calls; want gate, refused re-ask, gate, re-ask", readies)
	}
	if got := log.settle(); got != "hello\r" {
		t.Errorf("bytes %q", got)
	}
}

func TestPreEnterNonRawRewaitsThenVerifies(t *testing.T) {
	var first time.Time
	g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck {
		if n == 0 {
			first = time.Now()
		}
		if time.Since(first) < 300*time.Millisecond {
			return checkNotReady(notReady(protocol.ReasonTerminalNotReady))
		}
		return BoxCheck{Readiness: readyNow(), Verdict: BoxMismatch, Detail: "mangled"}
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	wantErr(t, o, protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true)
	if _, c, _ := g.counts(); c < 3 {
		t.Errorf("CheckBox ran %d times; the not-raw box should have been waited out", c)
	}
	if strings.Contains(log.settle(), "\r") {
		t.Errorf("Enter was pressed")
	}
	if o.rep.waited < 250*time.Millisecond {
		t.Errorf("waitedMs %v does not count the pre-Enter re-wait", o.rep.waited)
	}
}

func TestPreEnterDeadlineIsBusyTyped(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck {
		return checkNotReady(notReady(protocol.ReasonTerminalNotReady))
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 300})
	wantErr(t, o, protocol.CodeBusy, protocol.ReasonTerminalNotReady, true)
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text and no Enter", got)
	}
}

// ── dialogs (R5) ────────────────────────────────────────────────────────────

func TestPreEnterDialogWithholdsEnter(t *testing.T) {
	g := &fakeGate{checkFn: func(BoxExpect, int) BoxCheck { return checkNotReady(dialogUp("permission", true)) }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 200})
	wantErr(t, o, protocol.CodeBusy, "dialog:permission", true)
	if got := log.settle(); got != "hello" {
		t.Errorf("bytes %q, want the text and no Enter", got)
	}
}

func TestPreEnterDialogDismissedThenVerified(t *testing.T) {
	g := &fakeGate{checkFn: func(_ BoxExpect, n int) BoxCheck {
		if n == 0 {
			return checkNotReady(dialogUp("permission", true))
		}
		return verifiedBox(verifiedExact)
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, dismiss: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if got := log.settle(); got != "hello\x1b\r" {
		t.Errorf("bytes %q, want text, ONE Escape, Enter", got)
	}
	if len(o.rep.dismissed) != 1 || o.rep.dismissed[0] != "permission" {
		t.Errorf("dismissed %v", o.rep.dismissed)
	}
	if ack := lastAck(m); !strings.Contains(ack, "dismissed dialog:permission") || !strings.Contains(ack, "verified exact") {
		t.Errorf("ack %q", ack)
	}
}

func TestDialogBetweenChunksStopsTyping(t *testing.T) {
	text := strings.Repeat("x", 100) // four typed chunks
	g := &fakeGate{readyFn: func(_ ReadinessQuery, n int) InputReadiness {
		// 0 gate, 1 post-token, 2 before chunk 2, 3 before chunk 3
		if n >= 3 {
			return dialogUp("question", true)
		}
		return readyNow()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{mode: sendTyped, text: text, enter: true})
	wantErr(t, o, protocol.CodeBusy, "dialog:question", true)
	if got := log.settle(); got != text[:64] {
		t.Errorf("bytes %q (%d), want the first two chunks only", got, len(got))
	}
	if _, c, _ := g.counts(); c != 0 {
		t.Errorf("the box was checked after typing was stopped by a dialog")
	}
}

// TestDismissPressesOnlyEscape: whatever dismissal does, the only byte it ever
// writes is 0x1b.
func TestDismissPressesOnlyEscape(t *testing.T) {
	g := &fakeGate{readyFn: func(_ ReadinessQuery, n int) InputReadiness {
		if n < 20 {
			return dialogUp("permission", true)
		}
		return readyNow()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "go", enter: true, dismiss: true, readyTimeoutMs: 5000})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	got := log.settle()
	pre := strings.TrimSuffix(got, "go\r")
	if pre == got || strings.Trim(pre, "\x1b") != "" || len(pre) == 0 || len(pre) > dismissMax {
		t.Errorf("bytes %q: before the text only 1..%d Escapes may be written", got, dismissMax)
	}
}

func TestNoDismissOnBootDialog(t *testing.T) {
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return dialogUp("trust", false) }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "go", enter: true, dismiss: true, readyTimeoutMs: 300})
	wantErr(t, o, protocol.CodeBusy, "dialog:trust", false)
	if got := log.settle(); got != "" {
		t.Errorf("bytes %q; Escape on the trust dialog exits Claude", got)
	}
}

func TestDismissRespectsDeadline(t *testing.T) {
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return dialogUp("select", true) }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "go", enter: true, dismiss: true, readyTimeoutMs: 700})
	wantErr(t, o, protocol.CodeBusy, "dialog:select", false)
	if o.elapsed > 1500*time.Millisecond {
		t.Errorf("took %v; dismissal must stay inside the 700ms deadline", o.elapsed)
	}
	got := log.settle()
	if strings.Trim(got, "\x1b") != "" || len(got) > 2 {
		t.Errorf("bytes %q: at 400ms apart, a 700ms deadline allows 2 Escapes", got)
	}
}

func TestDismissCountIsPerSend(t *testing.T) {
	stuck := func(BoxExpect, int) BoxCheck { return checkNotReady(dialogUp("permission", true)) }
	start := time.Now()
	g := &fakeGate{
		readyFn: func(ReadinessQuery, int) InputReadiness {
			if time.Since(start) < 600*time.Millisecond { // two Escapes' worth at the gate
				return dialogUp("permission", true)
			}
			return readyNow()
		},
		checkFn: stuck,
	}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "go", enter: true, dismiss: true, readyTimeoutMs: 3000})
	wantErr(t, o, protocol.CodeBusy, "dialog:permission", true)
	got := log.settle()
	if n := strings.Count(got, "\x1b"); n != dismissMax {
		t.Errorf("%d Escapes across the gate and the pre-Enter check (%q), want %d", n, got, dismissMax)
	}
	// The next send has its own count.
	g.mu.Lock()
	g.readyFn, g.checkFn = nil, stuck
	g.mu.Unlock()
	o = runSend(t, m, sendRequest{text: "go", enter: true, dismiss: true, readyTimeoutMs: 1500})
	wantErr(t, o, protocol.CodeBusy, "dialog:permission", true)
	if n := strings.Count(log.settle(), "\x1b"); n != 2*dismissMax {
		t.Errorf("%d Escapes after the second send, want %d", n, 2*dismissMax)
	}
}

// ── plumbing ────────────────────────────────────────────────────────────────

func TestGateWaitsThenTypes(t *testing.T) {
	start := time.Now()
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness {
		if time.Since(start) < 300*time.Millisecond {
			return notReady(protocol.ReasonStarting)
		}
		return readyNow()
	}}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true})
	if o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
	if o.rep.waited < 250*time.Millisecond {
		t.Errorf("waitedMs %v, want the ~300ms the gate held", o.rep.waited)
	}
	if got := log.settle(); got != "hello\r" {
		t.Errorf("bytes %q", got)
	}
}

func TestGateBusyAfterDeadlineWritesNothing(t *testing.T) {
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return notReady(protocol.ReasonVimMode) }}
	m, _, log := gatedMux(t, g)
	o := runSend(t, m, sendRequest{text: "hello", enter: true, readyTimeoutMs: 250})
	wantErr(t, o, protocol.CodeBusy, protocol.ReasonVimMode, false)
	if !strings.Contains(o.err.Error(), "nothing was typed") {
		t.Errorf("message %q", o.err)
	}
	if got := log.settle(); got != "" {
		t.Errorf("bytes %q", got)
	}
	if g.notified.Load() != 0 {
		t.Errorf("a refused send announced a turn")
	}
	if ack := lastAck(m); !strings.Contains(ack, "refused: busy (vim_mode)") {
		t.Errorf("ack %q", ack)
	}
}

// TestGateWaitHoldsNoTreeMu: while a send waits in the gate, a writer gets
// treeMu and p.mu at once.
func TestGateWaitHoldsNoTreeMu(t *testing.T) {
	release := make(chan struct{})
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness {
		select {
		case <-release:
			return readyNow()
		default:
			return notReady(protocol.ReasonStarting)
		}
	}}
	m, p, _ := gatedMux(t, g)
	ch := startSend(t, m, context.Background(), sendRequest{text: "x", enter: true})
	time.Sleep(150 * time.Millisecond)
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		m.treeMu.Lock()
		p.mu.Lock()
		p.mu.Unlock()
		m.treeMu.Unlock()
		if d := time.Since(t0); d > 40*time.Millisecond {
			t.Errorf("treeMu+p.mu waited %v during the gate", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	if o := awaitSend(t, ch, 10*time.Second); o.err != nil {
		t.Fatalf("send: %v", o.err)
	}
}

func TestGatePaneExitRepliesPaneDead(t *testing.T) {
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return notReady(protocol.ReasonStarting) }}
	m, p, log := gatedMux(t, g)
	ch := startSend(t, m, context.Background(), sendRequest{text: "x", enter: true})
	time.Sleep(120 * time.Millisecond)
	p.mu.Lock()
	p.dead = true
	p.mu.Unlock()
	o := awaitSend(t, ch, 5*time.Second)
	wantErr(t, o, protocol.CodePaneDead, "", false)
	if got := log.settle(); got != "" {
		t.Errorf("bytes %q", got)
	}
}

// TestGateQuiesceCancels: a delivery waiting in the gate stops within a step
// when its (Quiesce-only) ctx is cancelled, with nothing typed.
func TestGateQuiesceCancels(t *testing.T) {
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return notReady(protocol.ReasonStarting) }}
	m, _, log := gatedMux(t, g)
	exec, release := m.bus().ExecContext(context.Background())
	defer release()
	ch := startSend(t, m, exec, sendRequest{text: "x", enter: true})
	time.Sleep(120 * time.Millisecond)
	t0 := time.Now()
	go m.bus().Quiesce(500 * time.Millisecond)
	o := awaitSend(t, ch, 5*time.Second)
	wantErr(t, o, protocol.CodeNotReady, "", false)
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Errorf("Quiesce took %v to stop the gate", d)
	}
	if got := log.settle(); got != "" {
		t.Errorf("bytes %q", got)
	}
}

func TestTypingTokenSerialisesConnections(t *testing.T) {
	g := &fakeGate{}
	m, _, log := gatedMux(t, g)
	a := strings.Repeat("A", 96)
	b := strings.Repeat("B", 96)
	ca := startSend(t, m, context.Background(), sendRequest{mode: sendTyped, text: a, enter: true})
	cb := startSend(t, m, context.Background(), sendRequest{mode: sendTyped, text: b, enter: true})
	for _, ch := range []<-chan sendOutcome{ca, cb} {
		if o := awaitSend(t, ch, 10*time.Second); o.err != nil {
			t.Fatalf("send: %v", o.err)
		}
	}
	got := log.settle()
	if got != a+"\r"+b+"\r" && got != b+"\r"+a+"\r" {
		t.Errorf("two deliveries interleaved: %q", got)
	}
}

func TestTokenDeadlineIsTypingInProgress(t *testing.T) {
	g := &fakeGate{}
	m, p, log := gatedMux(t, g)
	tok := m.typingToken(p)
	tok <- struct{}{} // another delivery holds it
	o := runSend(t, m, sendRequest{text: "x", enter: true, readyTimeoutMs: 200})
	wantErr(t, o, protocol.CodeBusy, protocol.ReasonTypingInProgress, false)
	if got := log.settle(); got != "" {
		t.Errorf("bytes %q", got)
	}
	<-tok
}

// TestNotifyInputOnlyAfterGatePasses (D9): gated, the turn is announced after
// the post-token check and before the first byte; refused, never; ungated, at
// admit as before.
func TestNotifyInputOnlyAfterGatePasses(t *testing.T) {
	t.Run("gated", func(t *testing.T) {
		g := &fakeGate{}
		m, _, _ := gatedMux(t, g)
		if o := runSend(t, m, sendRequest{text: "hi", enter: true}); o.err != nil {
			t.Fatalf("send: %v", o.err)
		}
		evs, seen := g.eventLog()
		if len(evs) < 3 || evs[0] != "ready" || evs[1] != "ready" || evs[2] != "notify" || seen[2] != "" {
			t.Errorf("events %v (bytes %q): want gate, post-token, notify before any byte", evs, seen)
		}
	})
	t.Run("refused", func(t *testing.T) {
		g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness { return notReady(protocol.ReasonStarting) }}
		m, _, _ := gatedMux(t, g)
		runSend(t, m, sendRequest{text: "hi", enter: true, readyTimeoutMs: 100})
		if g.notified.Load() != 0 {
			t.Errorf("a busy send announced a turn")
		}
	})
	t.Run("ungated keys", func(t *testing.T) {
		g := &fakeGate{}
		m, _, _ := gatedMux(t, g)
		if err := m.sendRequestVia(nil, nil, sendRequest{keys: []string{"2"}}, nil); err != nil {
			t.Fatal(err)
		}
		if g.notified.Load() != 1 {
			t.Errorf("an ungated send must announce at admit")
		}
	})
}

// TestPasteGatedUntilBracketed: on a gated pane a paste is not refused at
// admit for a missing bracketed paste; the gate (rule i, the REAL controller)
// waits for Claude to enable it.
func TestPasteGatedUntilBracketed(t *testing.T) {
	p := claudePaneAt(t, "s1-box-history", "empty-box-placeholder")
	m, s, log := claudeMux(t, p)
	p.mu.Lock()
	p.bracketPaste = false
	p.mu.Unlock()
	s.beforeCheck = func(int) {
		p.mu.Lock()
		p.bracketPaste = true
		p.mu.Unlock()
		setScreen(p, claudePaneAt(t, "s3-paste", "paste-in-box"))
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		p.mu.Lock()
		p.bracketPaste = true
		p.mu.Unlock()
	}()
	o := runSend(t, m, sendRequest{mode: sendPaste, text: "a\nb", enter: false, readyTimeoutMs: 3000})
	if o.err != nil && verbErrCode(o.err) == protocol.CodeBadRequest {
		t.Fatalf("the paste was refused at admit: %v", o.err)
	}
	if o.rep.waited < 250*time.Millisecond {
		t.Errorf("waited %v; the gate should have held until bracketed paste was on", o.rep.waited)
	}
	if got := log.settle(); !strings.HasPrefix(got, "\x1b[200~") {
		t.Errorf("bytes %q", got)
	}
	t.Run("auto multi-line asks for Paste", func(t *testing.T) {
		d := &sendDelivery{mode: sendAuto, text: "a\nb", class: ClassSubmit}
		if !d.query().Paste {
			t.Errorf("an auto multi-line text must wait for bracketed paste")
		}
		d = &sendDelivery{mode: sendTyped, text: "a\nb", class: ClassSubmit}
		if d.query().Paste {
			t.Errorf("a typed text is never bracketed")
		}
	})
}

// ── the op path ─────────────────────────────────────────────────────────────

func TestOpSendHasNoSub(t *testing.T) {
	msg, err := opMsg("send", hubCaller(), json.RawMessage(`{"pane":0,"text":"x","sub":1,"runCtx":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if msg.sub != nil || msg.runCtx != nil {
		t.Fatalf("an op's message carries a sub or a run ctx")
	}
	// And the gated op runs to completion through the hub: no self-deadlock.
	g := &fakeGate{}
	m, _, log := gatedMux(t, g)
	res, err := m.bus().Call(context.Background(), hubCaller(), "send", json.RawMessage(`{"pane":0,"text":"hi"}`))
	if err != nil {
		t.Fatalf("call send: %v", err)
	}
	if res["accept"] != acceptConfirmed || res["verified"] != verifiedExact {
		t.Errorf("result %v", res)
	}
	if got := log.settle(); got != "hi\r" {
		t.Errorf("bytes %q", got)
	}
}

// TestOpSendGateOnLane: `call {op:"send"}` on the socket waits in the gate on
// its (connection, pane) lane, and a second call to the same pane runs after.
func TestOpSendGateOnLane(t *testing.T) {
	m, child, dial := sockMux(t)
	start := time.Now()
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness {
		if time.Since(start) < 400*time.Millisecond {
			return notReady(protocol.ReasonStarting)
		}
		return readyNow()
	}}
	p := m.allPanes[0]
	gateSockPane(m, p, g)
	log := logChild(t, child)
	conn := dial()
	rd := newSockReader(t, conn)
	writeLines(t, conn,
		map[string]any{"type": "call", "op": "send", "id": "a", "args": map[string]any{"pane": 0, "text": "first"}},
		map[string]any{"type": "call", "op": "send", "id": "b", "args": map[string]any{"pane": 0, "text": "second"}})
	ra, _ := rd.reply("a", 10*time.Second)
	rb, _ := rd.reply("b", 10*time.Second)
	if ra["ok"] != true || rb["ok"] != true {
		t.Fatalf("replies %v / %v", ra, rb)
	}
	if got := log.settle(); got != "first\rsecond\r" {
		t.Errorf("bytes %q, want the lane's order", got)
	}
}

// TestGateBlocksOnlyItsOwnLane: a send held in the gate on pane 0 does not
// hold up the same connection's send to pane 1.
func TestGateBlocksOnlyItsOwnLane(t *testing.T) {
	m, child0, dial := sockMux(t)
	hold := make(chan struct{})
	g := &fakeGate{readyFn: func(ReadinessQuery, int) InputReadiness {
		select {
		case <-hold:
			return readyNow()
		default:
			return notReady(protocol.ReasonStarting)
		}
	}}
	p0 := m.allPanes[0]
	gateSockPane(m, p0, g)
	p1 := newScrollPane(20, 60)
	child1 := attachPipe(t, p1)
	m.treeMu.Lock()
	m.allPanes = append(m.allPanes, p1)
	m.stampPaneIDs()
	m.treeMu.Unlock()
	log0, log1 := logChild(t, child0), logChild(t, child1)

	conn := dial()
	rd := newSockReader(t, conn)
	writeLines(t, conn,
		map[string]any{"type": "send", "pane": 0, "text": "held", "id": "a"},
		map[string]any{"type": "send", "pane": 1, "text": "free", "id": "b"})
	if rb, _ := rd.reply("b", 3*time.Second); rb["ok"] != true {
		t.Fatalf("pane 1's send: %v", rb)
	}
	if got := log1.settle(); got != "free\r" {
		t.Errorf("pane 1 bytes %q", got)
	}
	if got := log0.String(); got != "" {
		t.Errorf("pane 0 typed while held: %q", got)
	}
	close(hold)
	if ra, _ := rd.reply("a", 5*time.Second); ra["ok"] != true {
		t.Fatalf("pane 0's send: %v", ra)
	}
}

// TestReplyKeysOnAGatedSend: the socket reply carries §5.4's keys.
func TestReplyKeysOnAGatedSend(t *testing.T) {
	m, _, dial := sockMux(t)
	g := &fakeGate{}
	p := m.allPanes[0]
	gateSockPane(m, p, g)
	conn := dial()
	rd := newSockReader(t, conn)
	writeLines(t, conn, map[string]any{"type": "send", "pane": 0, "keys": []string{"o", "k", "enter"}, "enter": false, "id": "1"})
	r, _ := rd.reply("1", 5*time.Second)
	res, _ := r["result"].(map[string]any)
	for k, want := range map[string]any{"keys": float64(2), "enter": true, "verified": "exact", "accept": "confirmed", "evidence": "history"} {
		if res[k] != want {
			t.Errorf("result[%q] = %v, want %v (%v)", k, res[k], want, res)
		}
	}
	if _, ok := res["waitedMs"]; !ok {
		t.Errorf("no waitedMs: %v", res)
	}
}

func TestReadyTimeoutClamp(t *testing.T) {
	g := &fakeGate{}
	m, _, _ := gatedMux(t, g)
	for ms, want := range map[int]time.Duration{0: 10 * time.Second, 2500: 2500 * time.Millisecond, 120000: 60 * time.Second} {
		d, err := m.admitSend(sendRequest{text: "x", enter: true, readyTimeoutMs: ms})
		if err != nil {
			t.Fatal(err)
		}
		if d.readyWait != want {
			t.Errorf("readyTimeoutMs %d → %v, want %v", ms, d.readyWait, want)
		}
	}
	if _, err := m.dispatchSocketMsg(sockMsg{Type: "send", Pane: 0, Text: "x", ReadyTimeoutMs: -1}); verbErrCode(err) != sockCodeBadRequest {
		t.Errorf("readyTimeoutMs -1: %v, want bad_request", err)
	}
}
