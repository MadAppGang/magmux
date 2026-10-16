package mux

// ── the gated send (architecture §3) ─────────────────────────────────────────
//
// A `send` to a gated pane does not type until the tool can receive it, does
// not press Enter until the box holds exactly what it typed, and does not
// answer ok until a USER PROMPT carrying that content appears. Every other
// send — an ungated pane, a KEYS send, `noGate` — never reaches this code and
// is today's, byte for byte (R6).
//
// The rules that keep it honest, in the order deliverGated meets them:
//
//   - Deadlines are VALUES, never contexts [E]. gateDeadline is computed when
//     the delivery starts and is read by the loops; the ctx a delivery pauses
//     on is the lane item's or an ExecContext, which only Quiesce cancels, so a
//     caller giving up never truncates a delivery half-typed.
//   - No lock is held across a wait. paneByID takes and drops treeMu; p.mu is
//     taken only inside injectPTY, tailView and liveness; the gate's own
//     methods take their own leaf locks.
//   - Every write is preceded by a SYNCHRONOUS readiness decision [D]: the
//     post-token InputReadiness before the first byte, the between-writes
//     check before every later one, CheckBox before the Enter and before a
//     re-sent Enter.
//   - Enter never reaches a dialog, an unverified box or a non-final
//     position [B]; dismissal presses Escape and nothing else (R5).

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/protocol"
)

// Gate timing. The acceptance window is protocol's, because a client's
// default wait (protocol.SendBudget) counts it.
const (
	gateStep     = 50 * time.Millisecond
	dismissMax   = 3 // per send, gate and pre-Enter together
	dismissGap   = 400 * time.Millisecond
	acceptWindow = protocol.SendAcceptWindow // per window; at most three
	acceptStep   = 50 * time.Millisecond

	// The pre-Enter verify's settle wait (settleBox); protocol's, because
	// SendBudget counts them.
	verifyStep   = protocol.SendVerifyStep
	verifySettle = protocol.SendVerifySettle
	verifyMax    = protocol.SendVerifyMax
	verifyMin    = protocol.SendVerifyMin
)

// accept values (the reply's `accept`).
const (
	acceptConfirmed = "confirmed"
	acceptResent    = "resent_enter"
	acceptInBox     = "in_box"
	// acceptSubmittedByOther: the text left the box through ANOTHER writer's
	// Enter (a human keystroke, a noGate send, a remote `input` op), and
	// evidence attributable to this text appeared. magmux pressed no Enter, so
	// the reply's `enter` is false.
	acceptSubmittedByOther = "submitted_by_other"
)

// retypeCause values (the reply's `retypeCause`, present only with
// `retyped:true`): which loss signal licensed the one retype.
const (
	// retypeTerminalReset: the box stayed empty for the settle interval and
	// the pty's termios changed after the first typed byte (the late startup
	// stdin reset).
	retypeTerminalReset = "terminal_reset"
	// retypeNeverDrawn: the box was completely empty on every read for the
	// whole verify window (verifyMax) with no reset — Claude never drew any of
	// the text. Render lag is well under a second, so that is a silent drop.
	retypeNeverDrawn = "never_drawn"
)

// sendClassification is admit's verdict on a send.
type sendClassification struct {
	g     InputGate
	gated bool
	class SendClass
	keys  []string // after normalisation: a trailing Enter key moved onto enter
	enter bool
	text  string // after normalisation: on a gated pane every CR / CRLF is "\n"
}

// gatedText is the text a gated send delivers, or why it cannot be delivered.
//
// A CR in the text is a NEWLINE, and becomes "\n" (CRLF too). Written raw it
// would be a keystroke Return in the middle of the instruction: an unverified
// submit of everything before it, after which the box check sees only the
// rest and the reply says nothing was submitted (review H1). Normalised, a
// multi-line auto text takes the bracketed path (query().Paste, rule i), typed
// mode is unchanged (typedWrites already maps every spelling to one ctrl-j),
// and inside a bracketed paste a CR was a newline already. So normalising is
// the meaning the caller wrote, in every mode, and refusing would only turn
// CRLF text from Windows clients into errors.
//
// Every OTHER control rune but tab is refused: ctrl-c interrupts, Escape
// dismisses a dialog or ends a bracketed paste early, DEL and ctrl-u edit the
// box. None of them is text the box check could account for, and a send that
// half-ran cannot be verified.
func gatedText(idx int, text string) (string, error) {
	for i, r := range text {
		if r == '\r' || r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return "", sockErrf(sockCodeBadRequest,
				"pane %d is gated: the text holds control character %U at byte %d; nothing was sent (send keys for it, or noGate)", idx, r, i)
		}
	}
	if !strings.Contains(text, "\r") {
		return text, nil
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}

// isEnterKey reports whether a key submits: anything whose bytes are Return,
// so `enter`, `return` and a literal "\r" alike.
func isEnterKey(k string) bool {
	b, ok := keyBytes(k)
	return ok && string(b) == "\r"
}

// printableKey reports whether a key types exactly one printable rune into
// the box (`a`, `2`, `space`); every other key is an editing key.
func printableKey(k string) (rune, bool) {
	b, ok := keyBytes(k)
	if !ok {
		return 0, false
	}
	r, size := utf8.DecodeRune(b)
	if size != len(b) || r == utf8.RuneError || !unicode.IsPrint(r) {
		return 0, false
	}
	return r, true
}

// classifySend applies the Enter-position rule and classifies the send (§3.2).
// On a pane that is not gated, or with noGate, it changes nothing.
//
//	KEYS    no text, no paste, no submit (any keys, printable or not): ungated
//	TYPE    text or paste, no submit                                  : gated
//	SUBMIT  a final Enter — enter:true, or a trailing enter key       : gated
//
// Printable keys alone never make a TYPE (r3 H1): `{"keys":["2"],"enter":false}`
// answers a numbered dialog deliberately, ungated.
func classifySend(p *Pane, r sendRequest) (sendClassification, error) {
	out := sendClassification{keys: r.keys, enter: r.enter, text: r.text}
	g := inputGateOf(p)
	if g == nil || r.noGate {
		return out, nil
	}
	text, err := gatedText(r.idx, r.text)
	if err != nil {
		return out, err
	}
	n := len(r.keys)
	for i, k := range r.keys {
		if _, ok := keyBytes(k); !ok {
			return out, sockErrf(sockCodeBadRequest,
				"pane %d is gated: unknown key %q; nothing was sent (a send that half-ran could not be verified)", r.idx, k)
		}
		if isEnterKey(k) && i != n-1 {
			return out, sockErrf(sockCodeBadRequest,
				"on a gated pane Enter may only be the final key of a send; split the request (key %d of %d is %q)", i+1, n, k)
		}
	}
	trailing := n > 0 && isEnterKey(r.keys[n-1])
	if trailing && r.enter {
		return out, sockErrf(sockCodeBadRequest,
			`on a gated pane a send presses Enter once: the last key is %q and "enter" is true; set "enter":false or drop the key`, r.keys[n-1])
	}
	keys, submit := r.keys, r.enter
	if trailing {
		keys, submit = keys[:n-1:n-1], true
	}
	cls := ClassKeys
	switch {
	case submit:
		cls = ClassSubmit
	case r.text != "" || r.mode == sendPaste:
		cls = ClassType
	}
	if cls == ClassKeys {
		return out, nil
	}
	return sendClassification{g: g, gated: true, class: cls, keys: keys, enter: submit, text: text}, nil
}

// sendReport is what a gated delivery observed, for the reply and the ack.
// Gated is false for every other send, and then nothing here is meaningful.
type sendReport struct {
	Gated       bool
	keys        int  // keys as delivered (a trailing Enter key became the submit)
	enter       bool // a submit
	waited      time.Duration
	dismissed   []string
	verified    string
	accept      string
	evidence    string
	resentEnter bool
	// retyped: the box was found completely empty after typing and the text
	// was typed once more (the auto-retype). Additive in the reply.
	retyped bool
	// retypeCause: the loss signal that licensed the retype
	// (retypeTerminalReset / retypeNeverDrawn). Reported only with retyped.
	retypeCause string
	lastEsc     time.Time
}

// addTo puts the gated reply keys on a send's result (§5.4). An ungated
// report adds nothing, so an ungated reply is byte-identical to today's.
func (r sendReport) addTo(result map[string]any) {
	if !r.Gated || result == nil {
		return
	}
	result["keys"] = r.keys
	result["enter"] = r.enter
	result["waitedMs"] = r.waited.Milliseconds()
	if len(r.dismissed) > 0 {
		result["dismissed"] = r.dismissed
	}
	if r.verified != "" {
		result["verified"] = r.verified
	}
	if r.accept != "" {
		result["accept"] = r.accept
	}
	if r.evidence != "" {
		result["evidence"] = r.evidence
	}
	if r.retyped {
		result["retyped"] = true
		if r.retypeCause != "" {
			result["retypeCause"] = r.retypeCause
		}
	}
}

// gatedAckText is the ⇦ continuation for a gated send:
//
//	waited 2.4s · dismissed dialog:permission · 118 bytes + enter · verified exact · accepted (history)
func gatedAckText(bytes int, r sendReport, err error) string {
	parts := []string{fmt.Sprintf("waited %.1fs", r.waited.Seconds())}
	for _, k := range r.dismissed {
		parts = append(parts, "dismissed "+protocol.ReasonDialogPrefix+k)
	}
	parts = append(parts, deliveryParts(bytes, r.keys, r.enter))
	if r.retyped {
		switch r.retypeCause {
		case retypeNeverDrawn:
			parts = append(parts, "retyped (the box was empty: never drawn)")
		case retypeTerminalReset:
			parts = append(parts, "retyped (the box was empty: terminal reset)")
		default:
			parts = append(parts, "retyped (the box was empty)")
		}
	}
	if r.verified != "" {
		parts = append(parts, "verified "+strings.ReplaceAll(r.verified, "_", " "))
	}
	if err != nil {
		parts = append(parts, gatedOutcome(err))
		return strings.Join(parts, " · ")
	}
	switch r.accept {
	case acceptConfirmed:
		parts = append(parts, "accepted ("+r.evidence+")")
	case acceptResent:
		parts = append(parts, "accepted after re-sent Enter ("+r.evidence+")")
	case acceptInBox:
		parts = append(parts, "in box, not submitted")
	case acceptSubmittedByOther:
		parts = append(parts, "submitted by another writer, not by magmux ("+r.evidence+")")
	}
	return strings.Join(parts, " · ")
}

// gatedOutcome names a failed gated send in the panel's words (§4.4), then
// the error itself.
func gatedOutcome(err error) string {
	head := ""
	switch protocol.ReasonOf(err) {
	case protocol.ReasonTextMismatch:
		head = "not submitted: text mismatch"
	case protocol.ReasonEmptyBox:
		head = "not submitted: box empty"
	case protocol.ReasonStillInBox:
		head = "not accepted: still in box"
	case protocol.ReasonUnconfirmed:
		head = "not accepted: unconfirmed"
	default:
		if verbErrCode(err) == protocol.CodeBusy {
			head = "refused: busy (" + protocol.ReasonOf(err) + ")"
		}
	}
	if head == "" {
		return err.Error()
	}
	return head + ": " + err.Error()
}

// gateErr is a gate error with its reason and typed flag.
func gateErr(code, reason string, typed bool, format string, a ...any) *protocol.Error {
	e := protocol.Errr(code, reason, format, a...)
	e.Typed = typed
	return e
}

// typingToken is the pane's cap-1 typing token (D12). Lazily created; never
// removed (Magmux.typingTokens).
func (m *Magmux) typingToken(p *Pane) chan struct{} {
	if v, ok := m.typingTokens.Load(p); ok {
		return v.(chan struct{})
	}
	v, _ := m.typingTokens.LoadOrStore(p, make(chan struct{}, 1))
	return v.(chan struct{})
}

// acceptWindower lets a TEST gate shorten the acceptance windows; a real
// controller does not implement it and gets acceptWindow.
type acceptWindower interface{ acceptWindowFor() time.Duration }

func (d *sendDelivery) acceptWindow() time.Duration {
	if w, ok := d.g.(acceptWindower); ok {
		if v := w.acceptWindowFor(); v > 0 {
			return v
		}
	}
	return acceptWindow
}

// query is what this delivery will do to the box (ReadinessQuery).
//
// Paste is true for a paste and for an AUTO text holding a newline: that is
// the text pasteWrap brackets, and on a gated pane it waits for Claude to
// enable bracketed paste (rule i) — unbracketed, its first newline would land
// as a keystroke in the middle of the prompt.
func (d *sendDelivery) query() ReadinessQuery {
	types := d.text != "" || d.mode == sendPaste
	for _, k := range d.keys {
		if _, ok := printableKey(k); ok {
			types = true
		}
	}
	return ReadinessQuery{
		Class:    d.class,
		Types:    types,
		Paste:    d.mode == sendPaste || (d.mode == sendAuto && strings.Contains(d.text, "\n")),
		Baseline: d.knowableContent(),
	}
}

// knowableContent is what this delivery will put into an EMPTY box when
// every key is printable: the text plus the keys' runes — the Exact the box
// check will expect. "" when an editing key makes it unknowable.
func (d *sendDelivery) knowableContent() string {
	var b strings.Builder
	b.WriteString(d.text)
	for _, k := range d.keys {
		r, ok := printableKey(k)
		if !ok {
			return ""
		}
		b.WriteRune(r)
	}
	return b.String()
}

// liveness is the per-iteration check every gate loop makes: the pane still
// exists and its child can still be written to.
func (d *sendDelivery) liveness(typed bool) *protocol.Error {
	p := d.m.paneByID(d.idx)
	if p == nil {
		return gateErr(protocol.CodeNoSuchPane, "", typed, "no pane %d (it was closed during the send)", d.idx)
	}
	p.mu.Lock()
	gone := p.dead || p.ptmx == nil
	p.mu.Unlock()
	if gone {
		return gateErr(protocol.CodePaneDead, "", typed, "pane %d: its child exited during the send", d.idx)
	}
	return nil
}

// tryDismiss presses Escape on a dismissable dialog when the request asked for
// it (R5): only Escape, only when the controller says Escape DECLINES what is
// showing, at most dismissMax per send and dismissGap apart. It reports
// whether it pressed.
//
// The Escape is a write, so the caller holds the pane's typing token: the
// step-6 loop already does, and the gate presses only after acquiring it
// (dismissDue), so an Escape can never land between another delivery's
// chunks (review M5b).
func (d *sendDelivery) tryDismiss(r InputReadiness, rep *sendReport) (bool, *protocol.Error) {
	if !d.dismissDue(r, rep) {
		return false, nil
	}
	if !d.p.injectPTY([]byte{0x1b}) {
		return false, gateErr(protocol.CodePaneDead, "", false,
			"pane %d rejected the Escape: its child has exited or its PTY is closed", d.idx)
	}
	rep.lastEsc = time.Now()
	rep.dismissed = append(rep.dismissed, strings.TrimPrefix(r.Reason, protocol.ReasonDialogPrefix))
	return true, nil
}

// dismissDue reports whether tryDismiss would press Escape on r now: asked
// for, dismissable, under the per-send count and past the gap.
func (d *sendDelivery) dismissDue(r InputReadiness, rep *sendReport) bool {
	if !d.dismiss || !r.Dismissable || len(rep.dismissed) >= dismissMax {
		return false
	}
	return rep.lastEsc.IsZero() || time.Since(rep.lastEsc) >= dismissGap
}

// notReadyErr is the gate's refusal: nothing was typed.
func (d *sendDelivery) notReadyErr(r InputReadiness, start time.Time) *protocol.Error {
	return gateErr(protocol.CodeBusy, r.Reason, false,
		"pane %d: claude-code is not ready for input (%s: %s) after %s; nothing was typed",
		d.idx, r.Reason, r.Detail, time.Since(start).Round(10*time.Millisecond))
}

// waitReady is step 1, the gate: ask until ready, or until a dismissal is due,
// until deadline. It writes NOTHING: a due Escape is pressed by the caller
// after it holds the typing token (review M5b).
func (d *sendDelivery) waitReady(ctx context.Context, q ReadinessQuery, start, deadline time.Time, rep *sendReport) error {
	for {
		if err := d.liveness(false); err != nil {
			return err
		}
		r := d.g.InputReadiness(q)
		if r.Ready || d.dismissDue(r, rep) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return d.notReadyErr(r, start)
		}
		if !pause(ctx, gateStep) {
			return d.cancelled("nothing")
		}
	}
}

// acquireToken is step 2's wait for the typing token, bounded by the same
// deadline and by Quiesce. Tried once without blocking first, so a passed
// deadline never loses a coin toss to a free token.
func (d *sendDelivery) acquireToken(ctx context.Context, tok chan struct{}, start, deadline time.Time) error {
	select {
	case tok <- struct{}{}:
		return nil
	default:
	}
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case tok <- struct{}{}:
		return nil
	case <-ctx.Done():
		return d.cancelled("nothing")
	case <-t.C:
		return gateErr(protocol.CodeBusy, protocol.ReasonTypingInProgress, false,
			"pane %d: another delivery is typing into the pane, still after %s; nothing was typed",
			d.idx, time.Since(start).Round(10*time.Millisecond))
	}
}

// deliverGated is the gated delivery, steps 1–9 of §3.3.
func (d *sendDelivery) deliverGated(ctx context.Context, done func(sendReport, error)) {
	rep := &sendReport{Gated: true, keys: len(d.keys), enter: d.enter}
	start := time.Now()
	deadline := start.Add(d.readyWait)
	tok := d.m.typingToken(d.p)
	held := false
	gateWaited := time.Duration(-1) // the gate and the token
	var waitedPre time.Duration     // the pre-Enter re-wait

	// Step 9, on every path: release the token, record the ack, answer.
	finish := func(err error) {
		if held {
			<-tok
			held = false
		}
		if gateWaited < 0 {
			gateWaited = time.Since(start)
		}
		rep.waited = gateWaited + waitedPre
		d.m.control.recordAck(d.seq, err == nil, verbErrCode(err), ackText(d.text, d.keys, d.enter, rep, err))
		if done != nil {
			done(*rep, err)
		}
	}
	fail := func(err *protocol.Error) { finish(err) }
	if ctx.Err() != nil {
		finish(d.cancelled("nothing"))
		return
	}

	// Steps 1 and 2: the gate, the token, then the post-token re-ask — the
	// AUTHORITY for the first byte [D]. Between it and the first injectPTY
	// there is only NotifyInput, an atomic flip: no pause, no I/O, no lock wait.
	q := d.query()
	// gate runs steps 1 and 2 until the post-token re-ask is ready, and
	// returns holding the token. It is entered again through regate.
	gate := func() (InputReadiness, error) {
		for {
			if err := d.waitReady(ctx, q, start, deadline, rep); err != nil {
				return InputReadiness{}, err
			}
			if err := d.acquireToken(ctx, tok, start, deadline); err != nil {
				return InputReadiness{}, err
			}
			held = true
			r := d.g.InputReadiness(q)
			if r.Ready {
				return r, nil
			}
			// Not ready: a due Escape is pressed now, with the token held,
			// and the token is released before waiting for the box to come
			// back.
			if _, err := d.tryDismiss(r, rep); err != nil {
				return InputReadiness{}, err
			}
			<-tok
			held = false
			if !time.Now().Before(deadline) {
				return InputReadiness{}, d.notReadyErr(r, start)
			}
			if !pause(ctx, gateStep) {
				return InputReadiness{}, d.cancelled("nothing")
			}
		}
	}
	// regate is the gate entered again, token released, to the SAME
	// deadline: when the first byte's own re-ask (the keys loop) finds the
	// pane not ready, and before a retype. Its time is the gate's, so
	// waitedMs counts it (r3 LOW).
	regate := func() (InputReadiness, error) {
		if held {
			<-tok
			held = false
		}
		t0 := time.Now()
		r, err := gate()
		gateWaited += time.Since(t0)
		return r, err
	}
	r, err := gate()
	if err != nil {
		finish(err)
		return
	}
	gateWaited = time.Since(start)

	// Step 3: the turn starts at the FIRST byte of the delivery, and only
	// then (D9, r3 M1). announce runs inside write and before the Enter —
	// after the last readiness check that authorised that byte — so a send
	// that writes nothing (busy at the first key's re-ask, cancelled) never
	// announces a turn, and a retype does not announce a second one.
	announced := false
	announce := func() {
		if !announced {
			announced = true
			notifyInput(d.p)
		}
	}

	// Step 4: typing. Before every write after an attempt's first, a
	// screen-and-termios check: a dialog stops the typing (busy, typed);
	// anything else not ready stops it and goes to the box check, which
	// re-waits and then verifies. wrote is the whole delivery's (the typed
	// flag); this is the current attempt's.
	wrote, this := false, false
	between := func() (stop bool, err *protocol.Error) {
		if !this {
			return false, nil
		}
		c := d.g.InputReadiness(ReadinessQuery{Class: ClassSubmit})
		if c.Ready {
			return false, nil
		}
		if strings.HasPrefix(c.Reason, protocol.ReasonDialogPrefix) {
			return true, gateErr(protocol.CodeBusy, c.Reason, true,
				"pane %d: a %s dialog appeared while the text was being typed; typing stopped and Enter was not pressed",
				d.idx, strings.TrimPrefix(c.Reason, protocol.ReasonDialogPrefix))
		}
		return true, nil
	}
	// typedAt is the current attempt's first byte; attemptN the attempt.
	var typedAt time.Time
	attemptN := 0
	write := func(b []byte, what string) *protocol.Error {
		announce()
		if !this {
			typedAt = time.Now()
		}
		if !d.p.injectPTY(b) {
			return gateErr(protocol.CodePaneDead, "", wrote,
				"pane %d rejected the %s: its child has exited or its PTY is closed", d.idx, what)
		}
		wrote, this = true, true
		if attemptN > 0 {
			rep.retyped = true // only once a retype byte has actually gone in
		}
		return nil
	}
	cancelledTyped := func(before string) error {
		err := d.cancelled(before)
		if e, ok := err.(*protocol.Error); ok {
			e.Typed = wrote
		}
		return err
	}

	var (
		chk       BoxCheck
		expect    BoxExpect
		settle    boxSettle
		sawExcess string // the detail of a read that held MORE than typed, on any attempt
		noLoss    bool   // the box stayed empty with neither loss signal: not retyped
		windowCut bool   // ... and the deadline cut the verify window short
		lostAgain bool   // the box stayed empty after the one retype
		pasteLost bool   // a paste's box stayed empty: never retyped
		leftBox   bool   // the box showed the text, then emptied without our Enter
	)
	for attempt := 0; ; attempt++ {
		attemptN = attempt
		boxWasEmpty, pasteAbove := !r.BoxHasText, r.MaxPaste
		pre := r.Pre // the pre-typing acceptance baseline, from the read that authorised the first byte
		typedText, pasted, stopped := false, false, false
		this, typedAt = false, time.Time{}

		switch d.mode {
		case sendTyped:
			for i, w := range typedWrites(d.text) {
				if i > 0 && !pause(ctx, typedChunkGap) {
					finish(cancelledTyped("rest of the typed text"))
					return
				}
				stop, err := between()
				if err != nil {
					fail(err)
					return
				}
				if stop {
					stopped = true
					break
				}
				if err := write(w, "text"); err != nil {
					fail(err)
					return
				}
				typedText = true
			}
		case sendPaste:
			// Written even when empty: the markers alone are the gesture.
			if err := write([]byte("\x1b[200~"+d.text+"\x1b[201~"), "text"); err != nil {
				fail(err)
				return
			}
			typedText, pasted = d.text != "", true
		default:
			if d.text != "" {
				b, bracketed := d.p.pasteWrapped(d.text)
				if err := write(b, "text"); err != nil {
					fail(err)
					return
				}
				typedText = true
				pasted = bracketed || utf8.RuneCountInString(d.text) > pasteCollapseRunes || len(d.text) > pasteCollapseRunes
			}
		}
		var exactKeys []rune
		allPrintable := true
		for _, k := range d.keys {
			kr, printable := printableKey(k)
			if printable {
				exactKeys = append(exactKeys, kr)
			} else {
				allPrintable = false
			}
			if stopped {
				continue
			}
			if !pause(ctx, keyGap) {
				finish(cancelledTyped("key " + k))
				return
			}
			if !this {
				// The attempt's FIRST byte is this key, and the readiness that
				// authorised it is a keyGap old now: a reset or a dialog in
				// that pause would be missed (review H4). Re-ask with the
				// gate's own query; not ready sends the delivery back to the
				// gate, nothing written.
				rr := d.g.InputReadiness(q)
				if !rr.Ready {
					if rr, err = regate(); err != nil {
						finish(err)
						return
					}
				}
				boxWasEmpty, pasteAbove, pre = !rr.BoxHasText, rr.MaxPaste, rr.Pre
			}
			stop, err := between()
			if err != nil {
				fail(err)
				return
			}
			if stop {
				stopped = true
				continue
			}
			b, _ := keyBytes(k)
			if err := write(b, "key "+k); err != nil {
				fail(err)
				return
			}
			if printable {
				typedText = true
			}
		}

		// Step 5: let the TUI draw the last write.
		if !pause(ctx, pilotSendDelay) {
			finish(cancelledTyped("box check"))
			return
		}

		// Step 6: the box check — a bounded settle wait (settleBox), to the
		// SAME deadline, run at least once.
		expect = BoxExpect{Class: d.class, TypedText: typedText, Pasted: pasted, PasteAbove: pasteAbove, TypedAt: typedAt}
		if boxWasEmpty && allPrintable && (d.text != "" || len(exactKeys) > 0) {
			// The box was empty when the first byte went in and nothing edited
			// it, so its content is knowable. Pastes included: Claude shows them
			// expanded in history, so the SENT key is what confirms them (phase 3).
			expect.Exact = d.text + string(exactKeys)
		}
		var ok bool
		if settle, ok = d.settleBox(ctx, expect, deadline, wrote, rep, &waitedPre, fail, func() {
			finish(cancelledTyped("enter"))
		}); !ok {
			return
		}
		chk = settle.chk
		if settle.excess != "" {
			sawExcess = settle.excess
		}

		// An empty box after typing knowable content: before anything else,
		// ask whether the text was SUBMITTED. A human keystroke, a noGate
		// send or a remote `input` op takes no typing token, so somebody
		// else's Enter can have submitted it while it sat in the box (r3
		// review M2). Evidence against the pre-typing baseline — a new user
		// entry or a fresh transcript record for this text — is the answer:
		// the text was taken, so the send is ok and is NEVER retyped. The
		// reply says WHO submitted it (r4 M3): accept:"submitted_by_other",
		// and `enter` false, because magmux pressed no Enter — for a TYPE send
		// as much as for a SUBMIT, since the text is no longer in the box.
		if chk.Verdict == BoxEmpty && typedText && expect.Exact != "" && !pre.At.IsZero() {
			if v := d.g.InputAccepted(pre); v.State == AcceptTaken {
				rep.accept, rep.evidence, rep.enter = acceptSubmittedByOther, v.Evidence, false
				finish(nil)
				return
			}
		}

		// The auto-retype, ONCE, and only on POSITIVE evidence that the text
		// was swallowed. The box stayed COMPLETELY empty (the placeholder
		// only) on every read for the WHOLE settle interval, and ONE of:
		//
		//   terminal_reset  the pty's termios changed after the attempt's
		//                   first byte — the late startup stdin reset IS a
		//                   termios event (F3);
		//   never_drawn     every read of the WHOLE verify window (verifyMax,
		//                   not cut short by the deadline) was ready and empty
		//                   — Claude never drew a rune of it (the V1 rerun's
		//                   silent drop). Render lag tops out well under a
		//                   second, so a 3 s blank is not a slow draw.
		//
		// A box that is merely slow to draw shows neither (V1: retyping slow
		// text put it in the box twice). The pre-typing baseline must exist
		// and show nothing submitted (above). Nothing was submitted and
		// nothing partial is left, so typing again cannot duplicate anything.
		// Back through the gate to the same deadline, then the same typing and
		// the same settle wait. A box holding PARTIAL text, a box still
		// changing when the window closed, or one that ever held MORE than
		// typed, is never retyped; nor is a box that showed ANY of the text
		// and then emptied (the text left it, perhaps through somebody
		// else's Enter, r4 M3c). Nor is a send that may have collapsed into a
		// `[Pasted text #N]` token (expect.Pasted: paste mode, a bracketed
		// auto text, an auto text over the collapse threshold): a paste drawn
		// late plus its retype are two tokens, and Enter would submit both
		// (r4 M1).
		lost := settle.termiosChanged || settle.neverDrawn
		if attempt == 0 && settle.emptyHeld && lost && sawExcess == "" && !settle.sawText &&
			!expect.Pasted && typedText && expect.Exact != "" && !pre.At.IsZero() {
			rep.retypeCause = retypeNeverDrawn
			if settle.termiosChanged {
				rep.retypeCause = retypeTerminalReset
			}
			if r, err = regate(); err != nil {
				if e, ok := err.(*protocol.Error); ok && e.Code == protocol.CodeBusy {
					msg := "pane %d: the input box was empty after typing (the text was lost) and the pane was not ready to retype it (%s); Enter was not pressed, nothing was submitted"
					if e.Reason == protocol.ReasonBoxNotEmpty {
						msg = "pane %d: the input box was empty after typing, then showed text before it could be retyped (%s); nothing was retyped, Enter was not pressed, and the box may hold the text"
					}
					err = gateErr(protocol.CodeNotAccepted, protocol.ReasonTextMismatch, true, msg, d.idx, e.Msg)
				} else if ok {
					e.Typed = true
				}
				finish(err)
				return
			}
			continue
		}
		if chk.Verdict == BoxEmpty && typedText {
			switch {
			case settle.sawText:
				leftBox = true
			case !settle.emptyHeld:
			case attempt > 0:
				lostAgain = true
			case expect.Pasted:
				pasteLost = true
			case !lost:
				noLoss, windowCut = true, settle.windowCut
			}
		}
		break
	}
	if chk.Verdict != BoxVerified {
		reason := boxVerdictReason(chk.Verdict, expect)
		if reason == protocol.ReasonEmptyBox {
			fail(gateErr(protocol.CodeNotAccepted, reason, wrote,
				"pane %d: the input box is empty, so there is nothing to submit; Enter was not pressed", d.idx))
			return
		}
		detail := chk.Detail
		if sawExcess != "" {
			detail = sawExcess
		}
		if detail == "" {
			detail = "it differs from what was sent"
		}
		switch {
		case noLoss && windowCut:
			detail += fmt.Sprintf("; it stayed empty but the terminal showed no stdin reset, and the deadline left less than the %s verify window that tells a silent drop from a slow draw, so the text was not retyped", verifyMax)
		case noLoss:
			detail += fmt.Sprintf("; it stayed empty but the terminal showed no stdin reset, and the box was not seen empty on every read for the whole %s verify window, so the text was not retyped", verifyMax)
		case lostAgain:
			detail += fmt.Sprintf("; the text was already retyped once (%s) and the box stayed empty again, so it was not retyped a second time", strings.ReplaceAll(rep.retypeCause, "_", " "))
		case pasteLost:
			detail += "; a paste is never retyped, because a late-drawn paste and its retype would be two tokens and Enter would submit both"
		}
		if settle.unsettled > 0 {
			detail += fmt.Sprintf("; it was still changing when the verify window closed after %s",
				settle.unsettled.Round(10*time.Millisecond))
		}
		if leftBox {
			// The text was seen in the box and is gone, and magmux pressed no
			// Enter: another writer may have submitted it with no evidence on
			// screen or in the transcript yet. "Nothing was submitted" would
			// invite a retry into a duplicate (r4 M3c).
			fail(gateErr(protocol.CodeNotAccepted, reason, true,
				"pane %d: the input box showed the text and then emptied without magmux's Enter (%s); it is unknown whether another Enter submitted it, so check the conversation before sending it again; magmux pressed no Enter and did not retype it",
				d.idx, detail))
			return
		}
		fail(gateErr(protocol.CodeNotAccepted, reason, true,
			"pane %d: the input box does not hold what was typed (%s); Enter was not pressed, nothing was submitted, and the box may hold partial text",
			d.idx, detail))
		return
	}
	rep.verified = chk.Verified
	if d.class == ClassType {
		rep.accept = acceptInBox
		finish(nil)
		return
	}

	// Step 7: Enter, with the baseline already taken from step 6's read. A
	// submit that typed nothing (Enter on text already in the box) starts
	// its turn here.
	announce()
	if !d.p.injectPTY([]byte("\r")) {
		fail(gateErr(protocol.CodePaneDead, "", true,
			"pane %d rejected the enter: its child has exited or its PTY is closed", d.idx))
		return
	}

	// Step 8: acceptance.
	if err := d.waitAccepted(ctx, expect, chk.Baseline, rep); err != nil {
		fail(err)
		return
	}
	finish(nil)
}

// boxSettle is what one verify's settle wait concluded.
type boxSettle struct {
	chk BoxCheck // the last ready read: verified, or the mismatch it settled on
	// emptyHeld: the box was COMPLETELY empty, and ready, on every read for
	// the whole settle interval at the end of the wait — the one fact that
	// licenses the auto-retype. Never true on a single read.
	emptyHeld bool
	excess    string // the detail of a read that held MORE than typed
	// termiosChanged: some read in the wait saw the termios change after
	// the attempt's first byte (BoxCheck.TermiosChanged).
	termiosChanged bool
	// neverDrawn: every read from the first ready read to the end of the
	// WHOLE verify window (verifyMax) was ready and COMPLETELY empty, with no
	// termios change — Claude never drew any of the text. Never true when the
	// deadline cut the window short (windowCut).
	neverDrawn bool
	// windowCut: the wait ended on an empty box with no termios change, and
	// the deadline fell before the full verify window could pass.
	windowCut bool
	// sawText: some ready read showed the box holding something (part of the
	// text, all of it, or more). A box that then empties is never retyped:
	// the text LEFT it, perhaps through another writer's Enter.
	sawText   bool
	unsettled time.Duration // >0: the window closed with the box still changing
}

// sameBox reports whether two ready reads saw the same box.
func sameBox(a, b BoxCheck) bool {
	return a.Verdict == b.Verdict && a.Box == b.Box && a.Detail == b.Detail
}

// settleBox is step 6: the pre-Enter verify as a bounded SETTLE WAIT, not one
// read. Claude can take several hundred milliseconds to draw typed text into
// its box (an Ink frame, worse under load), so a read that finds the box
// empty or short may be reading a frame that has not landed yet (V1). The box
// is read every verifyStep — each a synchronous CheckBox, termios sample and
// fresh screen — until:
//
//	verified (exact, or the paste-token rule)              → Enter may follow
//	the content is unchanged for verifySettle, mismatching → mismatch (settled)
//	verifyMax after the first ready read, or the deadline  → mismatch (unsettled)
//
// A not-ready read (a dialog, a reset) is not the box: it gets the old
// pre-Enter treatment — a due Escape, the deadline — and restarts the
// stability clock, so a box is "empty for the whole settle interval" only
// when every read in it was ready and empty. No lock is held across a pause.
//
// One exception to "settled ends the wait": a box that has been COMPLETELY
// empty on every read since the window opened, with no termios change, for
// typed knowable content, is NOT concluded at the settle interval when the
// whole verify window fits before the deadline. The wait runs on to
// verifyMax: text drawn late (inside the window) verifies as usual, and a box
// still blank at the window's end is neverDrawn — a silent drop, the second
// retype signal. A termios change during the extension ends it at the next
// settled read (the reset signal). If the deadline would cut the window
// short, the wait ends at the settle interval as before, windowCut.
//
// fail and cancelled answer the send; ok=false means it has been answered.
func (d *sendDelivery) settleBox(ctx context.Context, expect BoxExpect, deadline time.Time, typed bool,
	rep *sendReport, waitedPre *time.Duration, fail func(*protocol.Error), cancelled func()) (out boxSettle, ok bool) {
	var (
		first      time.Time // the first ready read: the window's start
		last       BoxCheck
		have       bool // last is the previous read, and it was ready
		stableFrom time.Time
		allEmpty   = true // every read since first was ready and BoxEmpty
	)
	// The verify's OWN minimum window (r4 M2): the readiness deadline never
	// cuts it below verifyMin from here, so a send that became ready in the
	// deadline's last moments, or a caller with a small readyTimeoutMs, still
	// waits out the measured render lag instead of judging one read 150 ms
	// after typing (V1). SendBudget counts it per attempt.
	if floor := time.Now().Add(verifyMin); deadline.Before(floor) {
		deadline = floor
	}
	// Only typed knowable content that is never a paste token can be "never
	// drawn": anything else has no retype to license, and waiting longer
	// would only delay the answer.
	drawable := expect.TypedText && expect.Exact != "" && !expect.Pasted
	for {
		if err := d.liveness(typed); err != nil {
			fail(err)
			return out, false
		}
		chk := d.g.CheckBox(expect)
		now := time.Now()
		if chk.TermiosChanged {
			out.termiosChanged = true
		}
		if !chk.Readiness.Ready {
			have = false
			if !first.IsZero() {
				allEmpty = false // a read that did not observe the box
			}
			cr := chk.Readiness
			pressed, err := d.tryDismiss(cr, rep)
			if err != nil {
				fail(err)
				return out, false
			}
			if !pressed && !now.Before(deadline) {
				fail(gateErr(protocol.CodeBusy, cr.Reason, typed,
					"pane %d: claude-code was not ready for the Enter (%s: %s) by the deadline; the text was typed and Enter was not pressed",
					d.idx, cr.Reason, cr.Detail))
				return out, false
			}
			t0 := time.Now()
			if !pause(ctx, gateStep) {
				cancelled()
				return out, false
			}
			*waitedPre += time.Since(t0)
			continue
		}
		out.chk = chk
		if chk.Verdict == BoxVerified {
			return out, true
		}
		if chk.Excess && out.excess == "" {
			out.excess = chk.Detail
		}
		if first.IsZero() {
			first = now
		}
		if chk.Verdict != BoxEmpty {
			allEmpty, out.sawText = false, true
		}
		if !have || !sameBox(last, chk) {
			last, have, stableFrom = chk, true, now
		}
		full := first.Add(verifyMax)
		cut := deadline.Before(full) // the deadline falls inside the window
		// blank: so far this is a box Claude never drew, with no reset.
		blank := drawable && allEmpty && !out.termiosChanged
		if blank && !cut && !now.Before(full) {
			out.emptyHeld, out.neverDrawn = true, true
			return out, true
		}
		if now.Sub(stableFrom) >= verifySettle && (!blank || cut) {
			out.emptyHeld = chk.Verdict == BoxEmpty
			out.windowCut = blank && cut
			return out, true
		}
		end := full
		if cut {
			end = deadline
		}
		if !now.Before(end) {
			out.unsettled = now.Sub(first)
			return out, true
		}
		if !pause(ctx, verifyStep) {
			cancelled()
			return out, false
		}
	}
}

// waitAccepted is step 8 (§4.3): up to three windows of evidence. Taken is
// the only ok; an empty box is never evidence.
//
//	window 1  Taken → confirmed · InBox → one guarded Enter → window 2 · None → keep polling
//	window 2  Taken → ok · InBox → resent? still_in_box : guarded Enter → window 3 · None → unconfirmed
//	window 3  Taken → resent_enter · InBox → still_in_box · None → unconfirmed
func (d *sendDelivery) waitAccepted(ctx context.Context, expect BoxExpect, b AcceptBaseline, rep *sendReport) *protocol.Error {
	win := d.acceptWindow()
	window, resent := 1, false
	end := time.Now().Add(win)
	for {
		if err := d.liveness(true); err != nil {
			return err
		}
		v := d.g.InputAccepted(b)
		if v.State == AcceptTaken {
			rep.evidence = v.Evidence
			rep.accept = acceptConfirmed
			if rep.resentEnter {
				rep.accept = acceptResent
			}
			return nil
		}
		if time.Now().Before(end) {
			if !pause(ctx, acceptStep) {
				return gateErr(protocol.CodeNotAccepted, protocol.ReasonUnconfirmed, true,
					"pane %d: magmux is shutting down; delivery outcome unknown (Enter was pressed)", d.idx)
			}
			continue
		}
		switch {
		case v.State == AcceptInBox && !resent:
			resent = true
			if err := d.resendEnter(expect, rep); err != nil {
				return err
			}
		case v.State == AcceptNone && window == 1:
			// No Enter: keep polling one more window.
		case v.State == AcceptInBox:
			times := "once"
			if rep.resentEnter {
				times = "twice"
			}
			return gateErr(protocol.CodeNotAccepted, protocol.ReasonStillInBox, true,
				"pane %d: Enter was pressed %s and the text is still in the input box; it was not submitted — do not retype it", d.idx, times)
		default:
			return gateErr(protocol.CodeNotAccepted, protocol.ReasonUnconfirmed, true,
				"pane %d: Enter was pressed, but no user-prompt entry or transcript entry appeared within %s; the prompt may have been lost or may have been taken without a visible sign",
				d.idx, time.Duration(window)*win)
		}
		window++
		end = time.Now().Add(win)
	}
}

// resendEnter is the guarded re-send: a write, so it gets the pre-Enter
// treatment — one CheckBox, and only a ready, verified box lets the \r go.
// Refused, it sends nothing and the window plays out.
func (d *sendDelivery) resendEnter(expect BoxExpect, rep *sendReport) *protocol.Error {
	c := d.g.CheckBox(expect)
	if !c.Readiness.Ready || c.Verdict != BoxVerified {
		return nil
	}
	if !d.p.injectPTY([]byte("\r")) {
		return gateErr(protocol.CodePaneDead, "", true,
			"pane %d rejected the re-sent enter: its child has exited or its PTY is closed", d.idx)
	}
	rep.resentEnter = true
	return nil
}

// sendBudget is THE upper bound on how long a gated send can take from the
// moment its delivery runs (protocol.SendBudget, §3.4). Ungated sends are not
// budgeted: their timings are today's.
func sendBudget(r sendRequest) time.Duration {
	submit := r.enter || (len(r.keys) > 0 && isEnterKey(r.keys[len(r.keys)-1]))
	s := protocol.SendShape{ReadyTimeoutMs: r.readyTimeoutMs, Keys: len(r.keys), Submit: submit}
	if r.mode == sendTyped {
		s.Typed, s.TypedWrites = true, len(typedWrites(r.text))
	}
	return protocol.SendBudget(s)
}
