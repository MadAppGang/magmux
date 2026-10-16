package mux

// ── Claude readiness, the pre-submit box check and acceptance evidence ────────
//
// The ClaudeCodeController's InputGate. Three questions, one screen reader:
//
//   - InputReadiness: would a delivery land NOW? A demand: one synchronous
//     termios sample (the authority; ttyWatch's history only feeds the settle
//     and boot rules) and one fresh screen read.
//   - CheckBox: immediately before the final Enter, does the box hold what
//     this send put there? The same sample + read, and the acceptance baseline
//     is taken from that SAME read, so nothing can happen between the verify
//     and the baseline.
//   - InputAccepted: after the Enter, is there evidence attributable to THIS
//     SEND — a NEW `❯` entry carrying the verified content after the verify's
//     own user-prompt entries, uniquely placed, or a transcript prompt record
//     equal to it and submitted after the verify? An emptied box is never
//     evidence: the measured loss (F3) looks exactly like a submit.
//
// Everything here is safe from any goroutine: the pane's content is read
// through tailView (p.mu, taken and released inside), the terminal through the
// ttyWatch (a leaf lock), and the controller's own state is atomics. The
// transcript path is read ONLY through `published`, never sessionPath, which
// belongs to the render goroutine. No magmux lock is ever held here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/pty"
)

const (
	// inputSettle: the terminal and the box must both have been still this
	// long (R3's "~1 s"; F3's reset lasts 100–400 ms).
	inputSettle = time.Second
	// bootFloor: without an observed boot reset, the first READY waits this
	// long from the first RAW sample — the loaded reset was measured ~2.5 s
	// after spawn. Measured from the first raw sample, not the fork, because
	// for a wrapper (claudish) the fork starts the wrapper, and the first raw
	// sample is the first moment Claude itself owns the terminal.
	bootFloor = 3500 * time.Millisecond
	// regMaxAge / regScanEvery bound the registry's file reads.
	regMaxAge    = 250 * time.Millisecond
	regScanEvery = time.Second
)

// errNoTTY is the terminal state of a controller with no ttyWatch (an ungated
// pane, or a pane with no PTY): not raw, so never ready.
var errNoTTY = errors.New("no termios watch for this pane")

// promptMark is one transcript PROMPT record: its key, a sequence number that
// grows by one per record, and when the prompt was SUBMITTED — the record's
// own timestamp, or for a dequeued prompt its enqueue's (zero when the record
// carried none). Never when the render goroutine got round to reading it.
type promptMark struct {
	key       string
	seq       uint64
	submitted time.Time
}

// promptLog is the published, immutable list of the most recent marks, oldest
// first. applyLine (the render goroutine) replaces it; acceptance reads it
// lock-free.
type promptLog struct{ marks []promptMark }

// promptLogMax bounds the log. Acceptance only looks at marks newer than its
// verify, and a send's window is seconds long.
const promptLogMax = 64

// queuedPrompt is an enqueued prompt not yet dequeued (render goroutine only).
// A dequeued one keeps its enqueue time in `at` until the user entry it
// becomes, if any, is written — and only for dequeueMaxLag after its remove.
type queuedPrompt struct {
	key       string
	at        time.Time
	dequeued  bool
	removedAt time.Time // the remove record's time (when it was read, if it carried none)
}

// queuedPromptMax bounds the render goroutine's queue list.
const queuedPromptMax = 64

// dequeueMaxLag bounds how long after its `remove` a dequeued prompt may
// still date a user entry. Claude writes the user entry a dequeued prompt
// becomes immediately after the remove (F11, milliseconds); a prompt removed
// WITHOUT becoming one — restored to the box with Up or Esc, or any reason
// F11 has not measured — must not date the next identical prompt, possibly
// minutes later, as old (r3 review M4).
const dequeueMaxLag = 2 * time.Second

// claudeBaseline is the Claude half of an AcceptBaseline.
type claudeBaseline struct {
	// entries is the ordered list of user-prompt entries on the CheckBox
	// screen, topSeen whether the top of the conversation was on it, and
	// lastRows the last entry's rows as drawn: what newEntriesFrom places the
	// later screen's entries against (review H3, r3 H1).
	entries       []entryID
	topSeen       bool
	lastRows      []string
	transcriptSeq uint64 // the newest promptMark's seq at the CheckBox read
	// renderedKey of the verified and of the sent text: what a HISTORY entry
	// is compared with, because Claude renders markdown there (F10).
	verifiedRendered, sentRendered string
	// shell: the verified box was in bash mode, so only a `! cmd` entry is
	// evidence, and a `❯` entry never is (and the reverse for a plain send).
	shell bool
}

// notePrompt records a transcript prompt and when it was submitted.
func (c *ClaudeCodeController) notePrompt(text string, submitted time.Time) {
	var marks []promptMark
	seq := uint64(1)
	if prev := c.prompts.Load(); prev != nil {
		marks = prev.marks
		if len(marks) > 0 {
			seq = marks[len(marks)-1].seq + 1
		}
	}
	if len(marks) >= promptLogMax {
		marks = marks[len(marks)-promptLogMax+1:]
	}
	next := append(slices.Clip(marks), promptMark{key: inputKey(text), seq: seq, submitted: submitted})
	c.prompts.Store(&promptLog{marks: next})
}

// lastMark is the newest prompt record, or nil.
func (c *ClaudeCodeController) lastMark() *promptMark {
	if l := c.prompts.Load(); l != nil && len(l.marks) > 0 {
		return &l.marks[len(l.marks)-1]
	}
	return nil
}

func (c *ClaudeCodeController) transcriptSeq() uint64 {
	if m := c.lastMark(); m != nil {
		return m.seq
	}
	return 0
}

// noteQueued reads the two records Claude writes for a queued (mid-turn)
// prompt — research F11, measured on 2.1.301:
//
//	{"type":"queue-operation","operation":"enqueue","timestamp":T0,"content":P}   at queue time
//	{"type":"queue-operation","operation":"remove","timestamp":T1,"content":P}    when it is taken
//	{"type":"attachment","attachment":{"type":"queued_command","prompt":P,"timestamp":T0}}
//
// The attachment is WRITTEN after the remove but carries the ORIGINAL enqueue
// time, so an earlier identical prompt's late attachment is not fresh. The
// enqueue is a queued send's own evidence, the moment it is queued. A remove
// marks the oldest matching enqueue dequeued, so that a user entry the
// dequeued prompt becomes is dated by its enqueue (dequeuedAt), not by when
// it ran: the run time of an older queued `continue` must never confirm a new
// one (r3 internal M2).
func (c *ClaudeCodeController) noteQueued(kind string, entry map[string]any) {
	at := parseClaudeTimestamp(entry["timestamp"])
	if kind == "attachment" {
		a, _ := entry["attachment"].(map[string]any)
		if t, _ := a["type"].(string); t != "queued_command" {
			return
		}
		prompt, _ := a["prompt"].(string)
		if own := parseClaudeTimestamp(a["timestamp"]); !own.IsZero() {
			at = own
		}
		if prompt != "" {
			c.notePrompt(prompt, at)
			// The queued prompt reached the model as this attachment: it is
			// consumed, and never becomes a user entry to be dated.
			c.dropQueued(inputKey(prompt))
		}
		return
	}
	content, _ := entry["content"].(string)
	key := inputKey(content)
	switch op, _ := entry["operation"].(string); op {
	case "enqueue":
		if content == "" {
			return
		}
		c.notePrompt(content, at)
		if len(c.queue) >= queuedPromptMax {
			c.queue = c.queue[1:]
		}
		c.queue = append(c.queue, queuedPrompt{key: key, at: at})
	case "remove":
		// The oldest pending enqueue of this content; with no content, the
		// oldest pending one of any — the conservative reading, since it
		// makes a later user entry OLDER, never fresher. A prompt removed
		// as absorbed_mid_turn (F11) reached the model inside the running
		// turn and never becomes a user entry, so it is dropped rather than
		// left to misdate the next identical send's own entry.
		reason, _ := entry["reason"].(string)
		for i := range c.queue {
			if !c.queue[i].dequeued && (content == "" || c.queue[i].key == key) {
				if reason == "absorbed_mid_turn" {
					c.queue = slices.Delete(c.queue, i, i+1)
				} else {
					c.queue[i].dequeued = true
					c.queue[i].removedAt = at
					if at.IsZero() {
						c.queue[i].removedAt = time.Now()
					}
				}
				break
			}
		}
	}
}

// dequeuedAt dates a real user entry: a dequeued enqueue of the same content
// is this entry, and its submit time is the enqueue's. Otherwise the entry's
// own timestamp.
//
// A dequeued entry dates only a user entry written within dequeueMaxLag of
// its remove; older dequeued entries are dropped here, of any key, so a
// prompt removed without becoming a user entry cannot shadow a new one.
func (c *ClaudeCodeController) dequeuedAt(text string, written time.Time) time.Time {
	key := inputKey(text)
	now := written
	if now.IsZero() {
		now = time.Now()
	}
	c.queue = slices.DeleteFunc(c.queue, func(q queuedPrompt) bool {
		return q.dequeued && now.Sub(q.removedAt) > dequeueMaxLag
	})
	for i, q := range c.queue {
		if q.dequeued && q.key == key {
			c.queue = slices.Delete(c.queue, i, i+1)
			return q.at
		}
	}
	return written
}

// dropQueued removes the oldest queue entry of key, pending or dequeued.
func (c *ClaudeCodeController) dropQueued(key string) {
	for i, q := range c.queue {
		if q.key == key {
			c.queue = slices.Delete(c.queue, i, i+1)
			return
		}
	}
}

// startTTYWatch builds and heats the termios watch for a GATED pane. It cannot
// fail, which is why it runs first in Start: a declared pane always has a
// working gate even when transcript discovery is dormant.
func (c *ClaudeCodeController) startTTYWatch() {
	p := c.pane
	if c.tty != nil || p == nil || p.gateBasis == gateNone || p.ptmx == nil {
		return
	}
	ptmx := p.ptmx
	started := p.startedAt
	if started.IsZero() {
		started = time.Now()
	}
	c.tty = newTTYWatch(func() (pty.Mode, error) { return pty.ReadMode(ptmx) }, started)
	// newTTYWatch starts nothing: the boot is covered by heating it here.
	c.tty.heat(started.Add(ttyHotStartup))
}

// ── the decision ────────────────────────────────────────────────────────────

// readinessInput is everything one readiness decision reads, so the decision
// itself is a pure function the F3 timelines can be walked through.
type readinessInput struct {
	now          time.Time
	tt           ttyState
	sc           claudeScreen
	reg          regEntry
	everReady    bool
	boxSince     time.Time // start of the box's current continuous visibility
	bracketPaste bool
	q            ReadinessQuery
}

// decideReadiness applies the rules in order; the first that matches decides.
//
//	a  terminal not raw (or unreadable)   starting | terminal_not_ready
//	b  a dialog in the dialog region      dialog:<kind>
//	c  no box, registry says waiting      dialog:permission | dialog:question
//	d  -- INSERT -- under the box         vim_mode
//	e  no box                             starting | no_input_box
//	f  never ready, boot not over         starting
//	g  box or termios changed < 1 s ago   starting | terminal_not_ready
//	h  Types, and the box holds text      box_not_empty
//	i  Paste, and bracketed paste is off  starting | terminal_not_ready
//
// "starting" is the answer while the pane has never been ready; afterwards the
// same condition is a regression and says so.
func decideReadiness(in readinessInput) InputReadiness {
	sc, tt, now := in.sc, in.tt, in.now
	r := InputReadiness{BoxHasText: sc.BoxText != "", MaxPaste: sc.MaxPaste}
	notReady := starting
	if in.everReady {
		notReady = protocol.ReasonTerminalNotReady
	}
	refuse := func(reason, detail string) InputReadiness {
		r.Reason, r.Detail = reason, detail
		return r
	}
	switch {
	case tt.Err != nil:
		return refuse(notReady, "termios unreadable: "+tt.Err.Error())
	case !tt.Raw:
		return refuse(notReady, "the terminal is not in raw mode")
	case sc.Dialog != "":
		r.Dismissable = sc.Dismissable
		return refuse(protocol.ReasonDialogPrefix+sc.Dialog, "a "+sc.Dialog+" dialog is showing")
	case !sc.BoxVisible && in.reg.Status == "waiting":
		kind := "question"
		if strings.Contains(in.reg.WaitingFor, "permission") {
			kind = "permission"
		}
		return refuse(protocol.ReasonDialogPrefix+kind,
			fmt.Sprintf("Claude's session registry says waiting (%s)", in.reg.WaitingFor))
	case sc.Insert:
		return refuse(protocol.ReasonVimMode, "-- INSERT -- is showing: vim editing mode is on")
	case !sc.BoxVisible:
		if in.everReady {
			return refuse(protocol.ReasonNoInputBox, "no input box and no recognised dialog on screen")
		}
		return refuse(starting, "no input box yet")
	}
	if !in.everReady && !bootDone(tt, now) {
		left := bootFloor - now.Sub(tt.FirstRawAt)
		return refuse(starting, fmt.Sprintf("no boot reset seen yet; floor in %s", left.Round(100*time.Millisecond)))
	}
	since := in.boxSince
	if since.IsZero() {
		since = now
	}
	if d := now.Sub(laterOf(since, tt.ChangedAt)); d < inputSettle {
		what := "the input box appeared"
		if tt.ChangedAt.After(since) {
			what = "termios changed"
		}
		return refuse(notReady, fmt.Sprintf("%s %s ago", what, d.Round(10*time.Millisecond)))
	}
	if in.q.Types && sc.BoxText != "" {
		return refuse(protocol.ReasonBoxNotEmpty, "the input box already holds text")
	}
	if in.q.Paste && !in.bracketPaste {
		return refuse(notReady, "bracketed paste is not enabled yet")
	}
	r.Ready = true
	return r
}

const starting = protocol.ReasonStarting

// bootDone: a boot reset was observed, or the floor has passed since the first
// raw sample.
func bootDone(tt ttyState, now time.Time) bool {
	return tt.SawReset || (!tt.FirstRawAt.IsZero() && now.Sub(tt.FirstRawAt) >= bootFloor)
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// readiness is the ONE function behind InputReadiness, CheckBox and Poll. A
// demand takes a synchronous sample and heats the watch; Poll reads history
// only (its answer is a published view, never the authority for a write).
func (c *ClaudeCodeController) readiness(now time.Time, q ReadinessQuery, demand bool) (InputReadiness, claudeScreen) {
	var tt ttyState
	switch {
	case c.tty == nil:
		tt.Err = errNoTTY
	case demand:
		tt = c.tty.sampleNow()
		c.tty.heat(now.Add(ttyDemandHold))
	default:
		tt = c.tty.state()
	}

	v := c.pane.tailView(0)
	sc := parseClaudeScreen(v.Rows, v.Cols)
	if sc.BoxVisible {
		c.boxSince.CompareAndSwap(0, now.UnixNano())
	} else {
		c.boxSince.Store(0)
	}
	var since time.Time
	if n := c.boxSince.Load(); n != 0 {
		since = time.Unix(0, n)
	}

	var reg regEntry
	if !sc.BoxVisible && sc.Dialog == "" {
		reg = c.registry(now) // corroboration only, and only where rule c can use it
	}
	r := decideReadiness(readinessInput{
		now: now, tt: tt, sc: sc, reg: reg,
		everReady: c.everReady.Load(), boxSince: since,
		bracketPaste: v.BracketPaste, q: q,
	})
	if r.Ready {
		c.everReady.Store(true)
	}
	return r, sc
}

// ── InputGate ───────────────────────────────────────────────────────────────

var _ InputGate = (*ClaudeCodeController)(nil)

// InputReadiness implements InputGate.
func (c *ClaudeCodeController) InputReadiness(q ReadinessQuery) InputReadiness {
	now := time.Now()
	r, sc := c.readiness(now, q, true)
	r.Gated = inputGateOf(c.pane) != nil
	if q.Baseline != "" && r.Ready {
		// The pre-typing baseline for the content about to be typed, from
		// this same read: any user entry or transcript record for it that
		// appears later was submitted after this moment.
		r.Pre = newAcceptBaseline(sc, q.Baseline, q.Baseline, 0, now, c.transcriptSeq())
	}
	return r
}

// CheckBox implements InputGate: readiness with Types:false (the box holds the
// send's own text, so rule h must not refuse it), then the box against want,
// both on ONE read.
func (c *ClaudeCodeController) CheckBox(want BoxExpect) BoxCheck {
	now := time.Now()
	r, sc := c.readiness(now, ReadinessQuery{Class: want.Class}, true)
	r.Gated = inputGateOf(c.pane) != nil
	out := BoxCheck{Readiness: r}
	if c.tty != nil && !want.TypedAt.IsZero() {
		// History, and the read above has just been stored into it.
		out.TermiosChanged = c.tty.state().ChangedAt.After(want.TypedAt)
	}
	if !r.Ready {
		return out
	}
	verdict, verified, paste, detail := judgeBox(sc, want)
	out.Verdict, out.Verified, out.Detail = verdict, verified, detail
	out.Box = sc.BoxKey
	out.Excess = verdict == BoxMismatch && (boxHoldsMore(sc, want) || newPasteTokens(sc, want) > 1)
	if verdict == BoxVerified {
		out.Baseline = newAcceptBaseline(sc, sc.BoxText, sentOf(sc, want.Exact), paste, now, c.transcriptSeq())
	}
	return out
}

// sentOf is the text a verified send put into the box, as acceptance compares
// it: Exact, or the box text when Exact is undetermined. In bash mode the
// leading `!` of Exact was the mode switch (s7) — it is the entry's glyph, not
// its text, in the box and in history alike — so it is not part of the sent
// text, on every path: a collapsed paste is confirmed by its sent key alone
// (r3 gemini L1). judgeBox has already refused a shell box whose Exact did
// not start with `!`.
func sentOf(sc claudeScreen, exact string) string {
	if exact == "" {
		return sc.BoxText
	}
	if sc.BoxShell {
		return strings.TrimPrefix(exact, "!")
	}
	return exact
}

// InputAccepted implements InputGate. It re-reads the screen and takes no
// termios sample: it decides no write.
func (c *ClaudeCodeController) InputAccepted(b AcceptBaseline) AcceptVerdict {
	v := c.pane.tailView(0)
	var marks []promptMark
	if l := c.prompts.Load(); l != nil {
		marks = l.marks
	}
	return acceptVerdict(parseClaudeScreen(v.Rows, v.Cols), b, marks)
}

// judgeBox is the pre-submit verify on one parsed screen.
//
//	Exact set, inputKey(box) == inputKey(Exact)        verified exact
//	Exact set, Pasted, a token N > PasteAbove in box   verified paste_token
//	Exact set, anything else non-empty                 mismatch
//	Exact unset, box non-empty                         verified non_empty
//	box empty                                          empty
//
// In bash mode (BoxShell) Exact must begin with the `!` that switched the
// mode, and the rows above are judged on the rest; otherwise mismatch.
func judgeBox(sc claudeScreen, want BoxExpect) (v BoxVerdict, verified string, paste int, detail string) {
	if sc.BoxText == "" {
		return BoxEmpty, "", 0, "the input box is empty"
	}
	if want.Exact == "" {
		return BoxVerified, verifiedNonEmpty, 0, ""
	}
	exact := want.Exact
	if sc.BoxShell {
		// Bash mode (s7): the leading `!` was consumed as the mode switch, so
		// it is compared as the switch and the rest as the text. A shell box
		// the send did NOT switch into is a mismatch whatever it holds:
		// submitting it would run a shell command, not send a prompt.
		rest, switched := strings.CutPrefix(exact, "!")
		if !switched {
			return BoxMismatch, "", 0, "the input box is in bash mode (`!`) and the send did not start with `!`"
		}
		exact = rest
	}
	if sc.BoxKey == inputKey(exact) {
		return BoxVerified, verifiedExact, 0, ""
	}
	if want.Pasted {
		for _, n := range sc.BoxPastes {
			if n > want.PasteAbove && n > paste {
				paste = n
			}
		}
		if fresh := newPasteTokens(sc, want); fresh > 1 {
			// One send pastes once. Two NEW tokens are a paste drawn late
			// plus a second copy of it (r4 M1): Enter would submit both.
			return BoxMismatch, "", 0, fmt.Sprintf("the box holds more than was typed: %d new paste tokens where one was pasted", fresh)
		}
		if paste > 0 {
			return BoxVerified, verifiedPasteToken, paste, ""
		}
	}
	got, typed := utf8.RuneCountInString(sc.BoxKey), utf8.RuneCountInString(inputKey(exact))
	if got > typed {
		return BoxMismatch, "", 0, fmt.Sprintf("the box holds more than was typed: %d non-space runes where %d were typed", got, typed)
	}
	return BoxMismatch, "", 0, fmt.Sprintf("the box holds %d of %d non-space runes", got, typed)
}

// newPasteTokens counts the box's `[Pasted text #N]` tokens newer than the
// baseline (N > PasteAbove), for a send that may have collapsed into one.
func newPasteTokens(sc claudeScreen, want BoxExpect) int {
	if !want.Pasted {
		return 0
	}
	n := 0
	for _, p := range sc.BoxPastes {
		if p > want.PasteAbove {
			n++
		}
	}
	return n
}

// boxHoldsMore reports whether the box holds more non-space runes than the
// send's known content: text drawn twice, never text still being drawn.
// False when the content is undetermined (no Exact).
func boxHoldsMore(sc claudeScreen, want BoxExpect) bool {
	if want.Exact == "" {
		return false
	}
	exact := want.Exact
	if sc.BoxShell {
		exact = strings.TrimPrefix(exact, "!")
	}
	return utf8.RuneCountInString(sc.BoxKey) > utf8.RuneCountInString(inputKey(exact))
}

// newAcceptBaseline takes the baseline from the screen the verify read.
// verified is the box text the verify read; sent is the text the send put
// there (Exact), or the box text again when that is undetermined.
func newAcceptBaseline(sc claudeScreen, verified, sent string, paste int, at time.Time, seq uint64) AcceptBaseline {
	b := AcceptBaseline{At: at, VerifiedKey: inputKey(verified), SentKey: inputKey(sent), Paste: paste}
	cd := claudeBaseline{transcriptSeq: seq, verifiedRendered: renderedKey(verified), sentRendered: renderedKey(sent), shell: sc.BoxShell,
		entries: entryIDs(sc.Prompts), topSeen: topVisible(sc)}
	if n := len(sc.Prompts); n > 0 {
		cd.lastRows = entryRowsAt(sc, sc.Prompts[n-1])
	}
	b.controllerData = cd
	return b
}

// ── the entry frontier (review H3, r3 gpt H1 / internal M3) ─────────────────
//
// A VISIBLE count of matching entries is not evidence: the conversation region
// is whatever the box leaves of the viewport, so a long box hides an older
// identical entry at the verify and the same entry reappears when the box
// empties. And no assistant, tool or spinner row is an anchor either: they
// stream, redraw and repeat ("⏺ ok"), so a signature taken from them can
// vanish (a correct queued send goes unconfirmed) or re-locate onto an older
// identical copy (an old entry counts as new — gpt's S → ❯ retry → S).
//
// The anchor is the ordered list of USER-PROMPT entries, which are immutable
// once drawn and are only ever appended (a queued entry keeps its place in
// that order while live output is inserted above it, s4). The verify records
// that list; a later screen's list must EXTEND it — the baseline is located
// in it as an aligned run ending where it ended — and only the entries after
// that end are new. Entries can scroll off the top (the run is then a
// prefix of the later list that equals a suffix of the baseline), and a
// shrinking box can bring older entries back above it (the whole baseline
// appears after them) unless the top of the conversation was on screen at the
// verify, in which case nothing older exists. If the baseline's end cannot be
// placed UNIQUELY — no candidate, or more than one (a run of identical
// entries) — nothing visible is attributable and the history gives no
// evidence; the transcript still can.

// entryID is an entry's identity in the ordered list: its kind and its key.
// Queued is not part of it: the hint goes when the queued prompt runs.
type entryID struct {
	shell bool
	key   string
}

func entryIDs(es []promptEntry) []entryID {
	out := make([]entryID, len(es))
	for i, e := range es {
		out[i] = entryID{shell: e.Shell, key: e.Key}
	}
	return out
}

// bannerRe is Claude's welcome banner row (s1): the top of the document.
var bannerRe = regexp.MustCompile(`▐▛.*Claude Code v[0-9]`)

// topVisible: the conversation's first row is on screen, so nothing older
// than the visible entries exists. The ONE proof is Claude's welcome banner as
// the region's first non-blank row (within its first three rows): Claude
// draws it at the top of every conversation.
//
// There used to be a second rule — the conversation "fits": content from the
// region's top plus three blank rows above the box or under the content. It
// was tuned to a synthetic layout and matched NONE of the real 2.1.301
// capture marks (every one draws the effort row directly above the rule, so
// there is never a blank tail), and where it did fire it was unsafe: a box
// that shrinks reveals an older identical entry, which then counted as new —
// a false ok (r3 review M3). Without the banner there is no proof, and a
// screen with no user-prompt entry at the verify gives no history evidence;
// the transcript still confirms. TestTopVisibleOnRealCaptures pins it.
func topVisible(sc claudeScreen) bool {
	blank := func(r styledRow) bool { return strings.TrimSpace(plain(r)) == "" }
	first := -1
	for i, r := range sc.conv {
		if !blank(r) {
			first = i
			break
		}
	}
	if first < 0 {
		return false // a box so tall it leaves no conversation proves nothing
	}
	return first < 3 && bannerRe.MatchString(plain(sc.conv[first]))
}

// entryRowsAt is an entry's own rows as drawn (its queued hint excluded),
// right-trimmed: what a partly scrolled-off copy of it is compared with.
func entryRowsAt(sc claudeScreen, e promptEntry) []string {
	end := e.end
	if e.Queued {
		end--
	}
	var out []string
	for i := e.row; i <= end && i < len(sc.conv); i++ {
		out = append(out, strings.TrimRight(plain(sc.conv[i]), " "))
	}
	return out
}

// tailAtTop: the region's first rows are the last rows of the baseline's last
// entry — its `❯` row scrolled off the top, the rest still showing, row for
// row. That is the proof the baseline's end sits just above the first visible
// entry; without it an entry list that shares nothing with the baseline is
// unplaceable.
func tailAtTop(sc claudeScreen, last []string) bool {
	if len(last) < 2 || len(sc.conv) == 0 || strings.TrimSpace(plain(sc.conv[0])) == "" {
		return false
	}
	first := len(sc.conv)
	if len(sc.Prompts) > 0 {
		first = sc.Prompts[0].row
	}
	k := 0
	for k < first && continuesEntry(sc.conv[k]) {
		k++
	}
	if k == 0 || k >= len(last) {
		return false
	}
	for i := range k {
		if strings.TrimRight(plain(sc.conv[i]), " ") != last[len(last)-k+i] {
			return false
		}
	}
	return true
}

// newEntriesFrom places the baseline's entry list in the later screen's and
// returns the index of the first NEW entry. ok is false when the place is not
// unique: no candidate, or several.
func newEntriesFrom(sc claudeScreen, cd claudeBaseline) (start int, ok bool) {
	post, base := entryIDs(sc.Prompts), cd.entries
	n, found := len(base), 0
	for e := 0; n > 0 && e < len(post); e++ { // e: where the baseline's last entry is
		l := min(n, e+1)
		if !slices.Equal(post[e-l+1:e+1], base[n-l:]) {
			continue
		}
		if cd.topSeen && e-n+1 > 0 {
			continue // entries above the whole baseline, yet nothing older exists
		}
		found, start = found+1, e+1
	}
	// The baseline's last entry is off screen: every visible entry is new,
	// provably only when there was no entry and the top was on screen, or the
	// last entry's tail is still showing at the top.
	if (n == 0 && cd.topSeen) || (n > 0 && tailAtTop(sc, cd.lastRows)) {
		found, start = found+1, 0
	}
	return start, found == 1
}

// newHistoryEntry: a NEW entry, after the uniquely placed baseline, carries
// this send's content.
func newHistoryEntry(sc claudeScreen, b AcceptBaseline) bool {
	cd, _ := b.controllerData.(claudeBaseline)
	start, ok := newEntriesFrom(sc, cd)
	return ok && countMatches(sc.Prompts[start:], b) > 0
}

// matches says whether an entry carries the baselined content. The entry's
// kind must be the send's first: a bash-mode (`! cmd`) entry for a send that
// switched into bash mode, a `❯` entry for any other (s7). Then:
//
//   - its key equals the VERIFIED key (what the box showed), or the SENT key.
//     The second is not in the design's sketch and is what the captures
//     demand: Claude shows a collapsed paste as `[Pasted text #N]` in the box
//     but EXPANDED in history (s3 paste-submitted, auto-long-submitted, s4
//     queued-paste), so a paste_token send's entry carries the sent text, not
//     the token;
//   - its RENDERED key equals the verified or sent text's rendered key.
//     Claude renders markdown in history and not in the box (F10, s6), so a
//     prompt with a backticked path — claudish's launch line — never has
//     equal raw keys; the rendered form folds '`', '*' and '~' on both sides;
//   - it begins with the baselined paste token;
//   - its key is at least historyMatchMin runes and the verified or sent key
//     starts with it (a history row Claude abbreviated; unseen in 2.1.301),
//     in either form.
func matches(e promptEntry, b AcceptBaseline) bool {
	cd, _ := b.controllerData.(claudeBaseline)
	if e.Key == "" || e.Shell != cd.shell {
		return false
	}
	if e.Key == b.VerifiedKey || e.Key == b.SentKey ||
		(e.RenderedKey != "" && (e.RenderedKey == cd.verifiedRendered || e.RenderedKey == cd.sentRendered)) {
		return true
	}
	if b.Paste > 0 && e.Paste == b.Paste {
		return true
	}
	prefixOf := func(full, part string) bool {
		return full != "" && part != "" && utf8.RuneCountInString(part) >= historyMatchMin && strings.HasPrefix(full, part)
	}
	return prefixOf(b.VerifiedKey, e.Key) || prefixOf(b.SentKey, e.Key) ||
		prefixOf(cd.verifiedRendered, e.RenderedKey) || prefixOf(cd.sentRendered, e.RenderedKey)
}

func countMatches(es []promptEntry, b AcceptBaseline) int {
	n := 0
	for _, e := range es {
		if matches(e, b) {
			n++
		}
	}
	return n
}

// acceptVerdict evaluates the evidence in order; the first match wins. Taken
// is checked BEFORE InBox: if a new entry proves the submission AND the box
// still shows the text, pressing Enter again would submit it twice.
//
//	1  a new matching entry after the placed baseline        Taken / history
//	2  a transcript prompt SUBMITTED after it, its key       Taken / transcript
//	3  the box still holds the verified content              InBox
//	—  anything else (an empty box above all)                None
//
// Both kinds of evidence must be ATTRIBUTABLE to this send. History: a new
// entry after the baseline's uniquely placed entry list (newHistoryEntry).
// Transcript: a record read after the verify whose SUBMIT time is at or after
// it — the record's own timestamp, the original enqueue time for a queued
// prompt's attachment, and the enqueue's for a dequeued prompt (F11). An
// identical prompt submitted earlier but read late (delayed polling, a late
// discovery, a queue that drained late) is that earlier submission, and a
// record with no timestamp proves nothing (review H2, r3 M2).
func acceptVerdict(sc claudeScreen, b AcceptBaseline, marks []promptMark) AcceptVerdict {
	cd, _ := b.controllerData.(claudeBaseline)
	v := AcceptVerdict{Dialog: sc.Dialog}
	if newHistoryEntry(sc, b) {
		v.State, v.Evidence = AcceptTaken, evidenceHistory
		return v
	}
	for _, m := range marks {
		if m.seq > cd.transcriptSeq && writtenSince(m.submitted, b.At) &&
			m.key != "" && (m.key == b.SentKey || m.key == b.VerifiedKey) {
			v.State, v.Evidence = AcceptTaken, evidenceTranscript
			return v
		}
	}
	if sc.BoxVisible && b.VerifiedKey != "" &&
		(sc.BoxKey == b.VerifiedKey || (b.Paste > 0 && slices.Contains(sc.BoxPastes, b.Paste))) {
		v.State = AcceptInBox
	}
	return v
}

// writtenSince: a transcript entry stamped `written` was written at or after
// the verify at `at`. Claude stamps to the millisecond, so the verify's
// instant is truncated to it; a missing stamp is never fresh. Both clocks are
// this machine's wall clock.
func writtenSince(written, at time.Time) bool {
	return !written.IsZero() && !written.Before(at.Truncate(time.Millisecond))
}

// ── Claude's session registry (corroboration only) ──────────────────────────
//
// ${CLAUDE_CONFIG_DIR:-~/.claude}/sessions/<pid>.json is Claude Code's own,
// undocumented record of a session's status (F4). It is treated like
// encodeProjectDir: optional, never the only source. It can name a dialog the
// screen did not recognise (rule c); it never makes a pane ready and is never
// acceptance evidence.

// regEntry is the part of a registry file readiness reads.
type regEntry struct {
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
	SessionID  string `json:"sessionId"`
}

type regCached struct {
	e  regEntry
	at time.Time
}

// claudeRegistry caches the last registry read. Atomics only: Poll (render
// goroutine) and demands (lane goroutines) both read it.
type claudeRegistry struct {
	cur      atomic.Pointer[regCached]
	lastScan atomic.Int64 // unix nanos of the last directory scan
}

// invalidate forgets the cache (tests).
func (r *claudeRegistry) invalidate() {
	r.cur.Store(nil)
	r.lastScan.Store(0)
}

// registry returns the pane's registry entry, re-read at most every
// regMaxAge. Lookup: <pid>.json for the pane's own process (zsh and bash exec
// the last simple command of -c); otherwise the entry whose sessionId is the
// transcript's, by a directory scan at most once per regScanEvery. A claudish
// pane's pid is the wrapper's, so only the second can hit, and only once the
// transcript is found. Missing anything: the zero entry.
func (c *ClaudeCodeController) registry(now time.Time) regEntry {
	if cached := c.reg.cur.Load(); cached != nil && now.Sub(cached.at) < regMaxAge {
		return cached.e
	}
	dir := filepath.Join(c.claudeConfigDir(), "sessions")
	var e regEntry
	found := false
	if p := c.pane; p != nil && p.cmd != nil && p.cmd.Process != nil {
		e, found = readRegEntry(filepath.Join(dir, fmt.Sprintf("%d.json", p.cmd.Process.Pid)))
	}
	if !found {
		if prev := c.reg.cur.Load(); prev != nil {
			e = prev.e // a throttled scan keeps the last answer
		}
		if path := c.published.Load(); path != nil && *path != "" &&
			now.Sub(time.Unix(0, c.reg.lastScan.Load())) >= regScanEvery {
			c.reg.lastScan.Store(now.UnixNano())
			e = scanRegistry(dir, strings.TrimSuffix(filepath.Base(*path), ".jsonl"))
		}
	}
	c.reg.cur.Store(&regCached{e: e, at: now})
	return e
}

// claudeConfigDir is CLAUDE_CONFIG_DIR as the pane's child sees it (the last
// occurrence in the pane's env, as os/exec resolves it), else magmux's own,
// else ~/.claude.
func (c *ClaudeCodeController) claudeConfigDir() string {
	if p := c.pane; p != nil && p.cmd != nil {
		for i := len(p.cmd.Env) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(p.cmd.Env[i], "CLAUDE_CONFIG_DIR="); ok {
				return v
			}
		}
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude")
	}
	return ""
}

// regFileMax bounds one registry read; the real files are a few hundred bytes.
const regFileMax = 64 << 10

func readRegEntry(path string) (regEntry, bool) {
	var e regEntry
	f, err := os.Open(path)
	if err != nil {
		return e, false
	}
	defer f.Close()
	// Whole, up to the bound: a short read cannot half-parse, and a file past
	// regFileMax is cut and fails to unmarshal.
	b, err := io.ReadAll(io.LimitReader(f, regFileMax))
	if err != nil || json.Unmarshal(b, &e) != nil {
		return regEntry{}, false
	}
	return e, true
}

func scanRegistry(dir, sessionID string) regEntry {
	if sessionID == "" {
		return regEntry{}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return regEntry{}
	}
	for _, de := range ents {
		if !de.Type().IsRegular() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		if e, ok := readRegEntry(filepath.Join(dir, de.Name())); ok && e.SessionID == sessionID {
			return e
		}
	}
	return regEntry{}
}
