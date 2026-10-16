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
//     SEND — a `❯` entry carrying the verified content below the verify's
//     conversation frontier, or a transcript user entry equal to it and written
//     after the verify? An emptied box is never evidence: the measured loss
//     (F3) looks exactly like a submit.
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

// promptMark is the newest transcript user entry: its key, a sequence number
// that grows by one per REAL user entry, and the entry's OWN timestamp — when
// Claude wrote it, not when the render goroutine got round to reading it
// (zero when the entry carried none). Written by applyLine on the render
// goroutine, read lock-free by acceptance.
type promptMark struct {
	key     string
	seq     uint64
	written time.Time
}

// claudeBaseline is the Claude half of an AcceptBaseline.
type claudeBaseline struct {
	// frontier is the conversation's last stable rows on the CheckBox screen
	// (frontierOf); below is how many entries under it already matched. An
	// entry is this send's only if it appears below the frontier's
	// re-location, so an older identical entry a box resize or a scroll
	// brings back into view is never counted (review H3). Empty: no
	// frontier, no history evidence.
	frontier      []string
	below         int
	transcriptSeq uint64 // lastPrompt.seq at the CheckBox read
	// renderedKey of the verified and of the sent text: what a HISTORY entry
	// is compared with, because Claude renders markdown there (F10).
	verifiedRendered, sentRendered string
	// shell: the verified box was in bash mode, so only a `! cmd` entry is
	// evidence, and a `❯` entry never is (and the reverse for a plain send).
	shell bool
}

// notePrompt records a real transcript user entry and the timestamp Claude
// wrote on it.
func (c *ClaudeCodeController) notePrompt(text string, written time.Time) {
	seq := uint64(1)
	if prev := c.lastPrompt.Load(); prev != nil {
		seq = prev.seq + 1
	}
	c.lastPrompt.Store(&promptMark{key: inputKey(text), seq: seq, written: written})
}

func (c *ClaudeCodeController) transcriptSeq() uint64 {
	if lp := c.lastPrompt.Load(); lp != nil {
		return lp.seq
	}
	return 0
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
	r, _ := c.readiness(time.Now(), q, true)
	r.Gated = inputGateOf(c.pane) != nil
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
	if !r.Ready {
		return out
	}
	verdict, verified, paste, detail := judgeBox(sc, want)
	out.Verdict, out.Verified, out.Detail = verdict, verified, detail
	if verdict == BoxVerified {
		sent := sc.BoxText
		if want.Exact != "" {
			sent = want.Exact
		}
		out.Baseline = newAcceptBaseline(sc, sc.BoxText, sent, paste, now, c.transcriptSeq())
	}
	return out
}

// InputAccepted implements InputGate. It re-reads the screen and takes no
// termios sample: it decides no write.
func (c *ClaudeCodeController) InputAccepted(b AcceptBaseline) AcceptVerdict {
	v := c.pane.tailView(0)
	return acceptVerdict(parseClaudeScreen(v.Rows, v.Cols), b, c.lastPrompt.Load())
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
		if paste > 0 {
			return BoxVerified, verifiedPasteToken, paste, ""
		}
	}
	return BoxMismatch, "", 0, fmt.Sprintf("the box holds %d of %d non-space runes",
		utf8.RuneCountInString(sc.BoxKey), utf8.RuneCountInString(inputKey(exact)))
}

// newAcceptBaseline takes the baseline from the screen the verify read.
// verified is the box text the verify read; sent is the text the send put
// there (Exact), or the box text again when that is undetermined.
func newAcceptBaseline(sc claudeScreen, verified, sent string, paste int, at time.Time, seq uint64) AcceptBaseline {
	b := AcceptBaseline{At: at, VerifiedKey: inputKey(verified), SentKey: inputKey(sent), Paste: paste}
	cd := claudeBaseline{transcriptSeq: seq, verifiedRendered: renderedKey(verified), sentRendered: renderedKey(sent), shell: sc.BoxShell}
	b.controllerData = cd // countMatches reads the rendered keys from it
	if row, sig := frontierOf(sc); sig != nil {
		cd.frontier = sig
		cd.below = countMatches(entriesBelow(sc.Prompts, row), b)
	}
	b.controllerData = cd
	return b
}

// ── the history frontier (review H3) ────────────────────────────────────────
//
// A VISIBLE count of matching entries is not evidence: the conversation region
// is whatever the box leaves of the viewport, so a long box hides an older
// identical entry at the verify and the same entry reappears when the box
// empties. Evidence is an entry that appears BELOW the conversation's last
// stable rows as they stood at the verify — the frontier — re-located on the
// new screen. New output and new entries can only appear below it; anything a
// resize or a scroll brings back is above it. Where it cannot be re-located
// (it scrolled off, a resize reflowed it, /clear wiped it) nothing visible is
// attributable and the history gives no evidence; the transcript still can.

// frontierRows is how many stable rows make the frontier's signature: enough
// that a repeated reply ("⏺ ok") does not re-locate it somewhere else.
const frontierRows = 3

// frontierIndentMax: a row indented further than this is right-aligned
// chrome — `◐ medium · /effort`, the token count — that Claude draws above the
// box and moves as the conversation grows (s1, s3). Never conversation.
const frontierIndentMax = 16

// stableRow is one conversation row that can serve as the frontier: its
// index on the screen and its normalised text (frontierText).
type stableRow struct {
	row  int
	text string
}

// frontierText normalises a row for the frontier comparison: the `⏺` glyph
// blinks on a running tool row (s4BlinkOff/On), and NBSPs and the edges carry
// nothing. "" = not a usable row.
func frontierText(r styledRow) string {
	t := strings.ReplaceAll(plain(r), "⏺", " ")
	indent := len(t) - len(strings.TrimLeft(t, " "))
	t = strings.TrimSpace(t)
	if t == "" || indent > frontierIndentMax || allFaint(r) {
		return ""
	}
	return t
}

// stableRows lists the conversation rows that can serve as a frontier, top to
// bottom: every non-blank row above the box except right-aligned chrome,
// faint rows, and the spinner zone (Claude's working row and everything under
// it, which redraws every frame).
func stableRows(sc claudeScreen) []stableRow {
	var out []stableRow
	for i, r := range sc.conv {
		if i >= sc.spinnerRow {
			break
		}
		if t := frontierText(r); t != "" {
			out = append(out, stableRow{row: i, text: t})
		}
	}
	return out
}

// frontierOf is the verify screen's frontier: the last frontierRows stable
// rows ABOVE the trailing run of prompt entries — a queued (mid-turn) entry
// is pinned under the live output, which Claude inserts above it (s4
// queued-c → queued-paste), so it is not a fixed point. row is the
// frontier's last screen row; a nil signature means there is none.
func frontierOf(sc claudeScreen) (row int, sig []string) {
	rows := stableRows(sc)
	end := len(rows)
	for end > 0 && inEntry(sc.Prompts, rows[end-1].row) {
		end--
	}
	if end == 0 {
		return 0, nil
	}
	start := max(0, end-frontierRows)
	for _, r := range rows[start:end] {
		sig = append(sig, r.text)
	}
	return rows[end-1].row, sig
}

// inEntry: screen row i belongs to a prompt entry (its rows, and the queued
// hint under it).
func inEntry(es []promptEntry, i int) bool {
	for _, e := range es {
		if i >= e.row && i <= e.end {
			return true
		}
	}
	return false
}

// locateFrontier finds the frontier on a later screen: the LOWEST run of
// stable rows equal to the signature. The lowest, because the true frontier
// can only be at or above any copy of it that new output repeated, so every
// entry below the lowest match is new.
func locateFrontier(sc claudeScreen, sig []string) (row int, ok bool) {
	rows := stableRows(sc)
	for j := len(rows) - 1; j >= len(sig)-1; j-- {
		match := true
		for k := range sig {
			if rows[j-len(sig)+1+k].text != sig[k] {
				match = false
				break
			}
		}
		if match {
			return rows[j].row, true
		}
	}
	return 0, false
}

// entriesBelow is the entries that start under screen row r.
func entriesBelow(es []promptEntry, r int) []promptEntry {
	for i, e := range es {
		if e.row > r {
			return es[i:]
		}
	}
	return nil
}

// newHistoryEntry: the screen shows a matching entry below the re-located
// frontier that was not there at the verify.
func newHistoryEntry(sc claudeScreen, b AcceptBaseline) bool {
	cd, _ := b.controllerData.(claudeBaseline)
	if len(cd.frontier) == 0 {
		return false
	}
	row, ok := locateFrontier(sc, cd.frontier)
	return ok && countMatches(entriesBelow(sc.Prompts, row), b) > cd.below
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
//	1  a new matching entry below the frontier             Taken / history
//	2  a transcript user entry WRITTEN after it, its key   Taken / transcript
//	3  the box still holds the verified content            InBox
//	—  anything else (an empty box above all)              None
//
// Both kinds of evidence must be ATTRIBUTABLE to this send. History: below
// the frontier (newHistoryEntry). Transcript: the entry's own timestamp is at
// or after the verify — an identical prompt submitted earlier but read late
// (delayed polling, a late discovery) is that earlier submission, and an
// entry with no timestamp proves nothing (review H2). The sequence check
// stays as well: an entry already read at the verify is never new.
func acceptVerdict(sc claudeScreen, b AcceptBaseline, lp *promptMark) AcceptVerdict {
	cd, _ := b.controllerData.(claudeBaseline)
	v := AcceptVerdict{Dialog: sc.Dialog}
	if newHistoryEntry(sc, b) {
		v.State, v.Evidence = AcceptTaken, evidenceHistory
		return v
	}
	if lp != nil && lp.seq > cd.transcriptSeq && writtenSince(lp.written, b.At) &&
		lp.key != "" && (lp.key == b.SentKey || lp.key == b.VerifiedKey) {
		v.State, v.Evidence = AcceptTaken, evidenceTranscript
		return v
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
