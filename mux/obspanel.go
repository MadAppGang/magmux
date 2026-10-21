package mux

// The control panel's SECOND VIEW: which Claude Code session on this machine
// needs a human.
//
// # It is a view, not a second panel, and that was decided rather than assumed
//
// architecture-v4 decision 8. There is exactly ONE panel pane and ONE set of
// anchor fields (panelAnchor / panelSplit / panelRatio / panelFirst), and a
// second panel would mean duplicating all of them plus a second toggle, a
// second anchor to keep valid across removeLeafLocked, and a second refusal
// path in splitFits. A second VIEW is a frame function and a key, and
// ControlPanel.render already chooses between three frames.
//
// # It is its own view because of what the panel IS
//
// CLAUDE.md: the panel is a WIRE TAP on one exchange — the controller's
// requests (`▶ OUT`) and magmux's own observations of the session (`◀ IN`) —
// and "the moment the panel reports on itself, the provenance model the whole
// instrument rests on is gone". An attention row is neither direction of that
// exchange: nobody asked for it, and it is about sessions the controller may
// never have touched, on panes that may not exist. Putting it in the stream
// would be a third row class, so it is a third VIEW instead, with its own
// frame and no access to `sent`, `observed`, the signal stream or any
// steplog.
//
// The grep-able half of that invariant is unaffected by this file:
//
//	$ grep -n 'observed *=\|observed++\|steplog\[i\].state = ' control.go obspanel.go
//
// still hits only recordObserved and recordStart's run-zeroing.
//
// # Locking
//
// setAttention is called from the obs sink's goroutine and takes cp.mu for
// exactly the assignment. The frame is built from the value copy
// snapshotLocked already makes, so cp.mu is never held across rendering — the
// documented order is treeMu -> cp.mu and the reverse is never legal, so
// nothing here may take treeMu at all. digest() is the same pattern in the
// other direction and is the shape this follows.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/pipeline"
	"github.com/MadAppGang/magmux/obsview"
	"github.com/MadAppGang/magmux/theme"
)

// ctrlView is which frame the panel is painting.
//
// Stated as an ordered list rather than a bool so that a third view is a value
// and not a rewrite, and the zero value is the EXCHANGE — the panel every
// existing test, demo and `test/ui/case3.ts` expects, built from a struct
// literal that says nothing about views.
type ctrlView uint8

const (
	// viewExchange is the wire tap: the control plane magmux has always had.
	viewExchange ctrlView = iota
	// viewAttention is the observation layer's document, as a triage list.
	viewAttention
	// ctrlViewCount bounds the cycle. It is not a view.
	ctrlViewCount
)

// attnState is the three-valued attention verdict, flattened for the panel.
//
// A value and not the *bool the document carries, deliberately: the panel's
// snapshot is a lock-free VALUE COPY, and a pointer in it would be a pointer
// into whatever the sink cloned — safe today, and exactly the kind of sharing
// that stops being safe the first time somebody derives the view somewhere
// else. Three states, three values, no indirection.
type attnState uint8

const (
	attnUnknown attnState = iota // "?" — the evidence supports neither verdict
	attnNo                       // we looked, and it does not need anyone
	attnYes                      // a human should look now
)

// urgency is the SORT rank, and it is a separate function from the enum's own
// order on purpose.
//
// The enum's order is fixed by its zero value: attnUnknown has to be index 0,
// because a row built from a struct literal that says nothing must be
// undetermined rather than a determined "no". The reading order is different —
// the ones that need a human, then the ones nobody could reach a verdict
// about, then the rest — and encoding it in the iota would put those two
// requirements in conflict. "?" sorting with the sessions nobody has to look
// at is exactly the collapse the tri-state exists to prevent.
func (a attnState) urgency() int {
	switch a {
	case attnYes:
		return 2
	case attnUnknown:
		return 1
	default:
		return 0
	}
}

// ctrlAttentionRow is one session, reduced to what the panel paints.
type ctrlAttentionRow struct {
	pid     int
	paneID  int // -1 when the session is not in one of magmux's panes
	name    string
	status  string
	waiting string
	needed  attnState
	reason  string

	inStatus    time.Duration
	hasInStatus bool

	conflict string
}

// ctrlAttention is the whole view's state: a lock-free value copy, taken under
// cp.mu and read with the lock released, exactly as ctrlFrameState is.
type ctrlAttention struct {
	// have is whether a document has arrived. It is NOT len(rows) > 0: "no
	// sessions on this machine" and "the layer has not published yet" are
	// different answers, and the panel says which.
	have  bool
	epoch time.Time
	at    time.Time

	rows []ctrlAttentionRow

	needed  int
	unsure  int
	total   int
	dropped int

	// problems is how many things the source could not read while building
	// this document. Shown as a count, because a source that silently skips a
	// record is indistinguishable from a machine with fewer sessions on it.
	problems int
}

// attentionOf derives the view from one delivery.
//
// It runs on the SINK's goroutine holding no lock at all — that is the whole
// point of deriving here rather than at paint time — and it reads a clone the
// sink already took, so it retains nothing of the shared document.
func attentionOf(doc obs.Fleet, at time.Time, d pipeline.Delivery) ctrlAttention {
	v := ctrlAttention{
		have:     true,
		epoch:    doc.Epoch,
		at:       at,
		total:    len(doc.Sessions),
		dropped:  d.Dropped,
		problems: len(d.Problems),
	}
	for _, s := range doc.Sessions {
		// Every string a row carries is DISPLAY text from here on, and this is
		// the one place registry text enters the panel — so it passes through
		// the one display rule here, before it can reach the panel's own VT
		// parser. A control sequence there would not escape to the real
		// terminal, but it could leave the parser in a changed state (a scroll
		// region, origin mode) that survives every repaint. statusText and
		// waitingText apply the rule themselves; see obsview/display.go.
		r := ctrlAttentionRow{
			pid:     s.PID,
			paneID:  -1,
			name:    obsview.DisplaySafe(s.Name),
			status:  obsview.StatusText(s),
			waiting: obsview.WaitingText(s),
			reason:  obsview.DisplaySafe(s.Attention.Reason),
		}
		if s.PaneID != nil {
			r.paneID = *s.PaneID
		}
		switch {
		case s.Attention.Needed == nil:
			r.needed = attnUnknown
			v.unsure++
		case *s.Attention.Needed:
			r.needed = attnYes
			v.needed++
		default:
			r.needed = attnNo
		}
		if d, ok := s.InStatusFor(doc.Epoch); ok {
			r.inStatus, r.hasInStatus = d, true
		}
		if s.Conflict != nil {
			r.conflict = fmt.Sprintf("the %s tier said %s", s.Conflict.Tier, s.Conflict.Status)
		}
		v.rows = append(v.rows, r)
	}

	// The order is the ANSWER to "which session needs me": the ones that do,
	// then the ones nobody could reach a verdict about, then the rest. Within
	// a group, the longest-waiting first — a session that has been blocked for
	// half an hour is more urgent than one blocked for ten seconds, and on a
	// panel that shows twelve rows of thirty the order decides what is
	// visible at all.
	sort.SliceStable(v.rows, func(i, j int) bool {
		a, b := v.rows[i], v.rows[j]
		if a.needed != b.needed {
			return a.needed.urgency() > b.needed.urgency()
		}
		if a.hasInStatus != b.hasInStatus {
			return a.hasInStatus
		}
		if a.inStatus != b.inStatus {
			return a.inStatus > b.inStatus
		}
		return a.pid < b.pid
	})
	return v
}

// setAttention stores the newest view. Called from the obs sink's goroutine.
//
// It takes cp.mu for the assignment and nothing else. It MUST NOT be called
// with cp.mu held and MUST NOT be reached from anything holding treeMu's write
// lock — the documented order is treeMu -> cp.mu, so the sink, which holds
// neither, is the right place for it and the render loop is not.
func (cp *ControlPanel) setAttention(v ctrlAttention) {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.attention = v
	// Only a repaint when the view is on screen. The panel's whole rendering
	// model is that an idle pane costs nothing, and a document arriving every
	// time any session on the machine changes status would otherwise repaint
	// the exchange view for a change it does not show.
	if cp.view == viewAttention {
		cp.needsPaint = true
	}
	cp.mu.Unlock()
}

// clearAttention forgets the document, for the same reason obsStore.stop drops
// it: a stopped pipeline's last document keeps aging and nothing will correct
// it.
func (cp *ControlPanel) clearAttention() {
	if cp == nil {
		return
	}
	cp.mu.Lock()
	cp.attention = ctrlAttention{}
	if cp.view == viewAttention {
		cp.needsPaint = true
	}
	cp.mu.Unlock()
}

// setView puts the panel on one view. Caller must NOT hold cp.mu; it MAY hold
// treeMu, which is the documented order.
func (cp *ControlPanel) setView(v ctrlView) {
	if cp == nil || v >= ctrlViewCount {
		return
	}
	cp.mu.Lock()
	moved := cp.view != v
	cp.view = v
	if moved {
		cp.scroll = 0
	}
	cp.needsPaint = true
	cp.mu.Unlock()
}

// cycleView steps to the next view and reports whether anything moved.
func (cp *ControlPanel) cycleView(delta int) bool {
	if cp == nil {
		return false
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	n := int(ctrlViewCount)
	cp.view = ctrlView(((int(cp.view)+delta)%n + n) % n)
	// Following the tail again, exactly as setFilter does: a view you have
	// just switched into should show the newest traffic rather than wherever
	// the previous one was scrolled to.
	cp.scroll = 0
	cp.needsPaint = true
	return true
}

// hasAttention reports whether a document has arrived, without painting it. It
// is what the exchange view's hint line asks.
func (cp *ControlPanel) hasAttention() bool {
	if cp == nil {
		return false
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.attention.have
}

// frameAttention paints the triage list.
//
// # What it shows, and what it deliberately does not
//
// Every column here is a column `magmux agents` prints, with the same words and
// the same rules — "-" for undetermined everywhere except the attention
// column, which spells its third state "?" because a dash beside a "no" in the
// same column is read as another no. That is not a coincidence to be
// maintained by hand: both renderings are derived from one obs.Fleet, and
// TestTheThreeSurfacesAgree walks a document through all three.
//
// What it does not show is anything about the controller: no `sent`, no
// `observed`, no step ledger. This view is not part of that exchange.
//
// # It never paints more lines than the pane has, and it scrolls
//
// The header (title, summary, two rules) and the footer (rule, key hint) are
// fixed; the sessions between them scroll, by s.scroll SESSIONS from the top.
// Everything is budgeted in LINES, because a row that needs a human carries a
// detail line under it — and the first version budgeted in rows, so on a
// 24-row pane with 30 sessions it returned 33 lines, paintFrame kept the first
// 24, and the footer went first and then the "+N more": 11 sessions visible
// and nothing to say 19 more existed. It also never read s.scroll, so the j/k
// the footer advertised moved nothing. Fix round 1, item 4; the test is
// TestTheAttentionViewFitsItsPaneAndScrollsToTheLastSession, at 24 rows
// because the live check that missed it ran on 50.
//
// A session scrolled out of sight is COUNTED, above and below: "▲ N above"
// and "▼ N more". A list that quietly shows the first screenful of a triage is
// the failure this view exists to prevent, reintroduced by its own layout.
//
// The guarantee holds from nine rows, which is the fixed six lines plus one
// session and both counts. Below that the footer is still kept, and the lines
// just above it give way; see the end of the function.
func (cp *ControlPanel) frameAttention(s ctrlFrameState, w, h int) []string {
	inner := w - 2
	if inner < 6 {
		inner = w
	}
	pad := " "
	rule := func(c theme.RGB) string { return pad + paint(c, strings.Repeat("─", maxInt(inner, 1))) }

	a := s.attention
	out := []string{
		pad + padBetween(
			paint(theme.Pal.Accent, sgrBold+"ATTENTION"),
			paint(theme.Pal.Subtle, attentionClock(a)),
			inner),
		rule(theme.Pal.Border),
	}
	footer := attentionFooter(pad, inner)

	if !a.have {
		// "Nothing has been published" and "no sessions" are different
		// answers. Saying which is the whole reason ctrlAttention.have exists
		// beside len(rows).
		out = append(out,
			pad+truncANSI(paint(theme.Pal.Subtle, "session observation is not running here"), inner),
			pad+truncANSI(paint(theme.Pal.Subtle, "start magmux with MAGMUX_AGENTS=1"), inner),
		)
		return append(out, footer...)
	}

	out = append(out, pad+truncANSI(attentionSummary(a), inner), rule(theme.Pal.Border))

	if len(a.rows) == 0 {
		out = append(out, pad+truncANSI(paint(theme.Pal.Subtle, "no Claude Code sessions on this machine"), inner))
		return append(out, footer...)
	}

	// The lines left for sessions and the two counts, once the fixed lines
	// above and below have been paid for.
	avail := h - len(out) - len(footer)

	// The offset, clamped now that the lengths are known, and written back so
	// a key pressed past either end does not leave the view stuck there — the
	// arrangement the exchange view already has.
	off := minInt(maxInt(s.scroll, 0), attentionMaxOffset(a.rows, avail))
	if off != s.scroll {
		cp.clampScroll(off)
	}

	budget := avail
	if off > 0 {
		budget-- // "▲ N above"
	}
	var body []string
	end := off
	for i := off; i < len(a.rows); i++ {
		// One line is held back for "▼ N more" whenever anything follows, so
		// the count of what is not shown can never be the thing that is cut.
		reserve := 0
		if i+1 < len(a.rows) {
			reserve = 1
		}
		if len(body)+1+reserve > budget {
			break
		}
		r := a.rows[i]
		body = append(body, pad+attentionRow(r, s.focused, inner))
		// The reason, indented, for the rows a person would have to act on.
		// A determined "no" is left out: its reason is already legible from
		// the row, and thirty of them would bury the two that matter — the
		// same rule `magmux agents`'s why-block follows. A detail that does not
		// fit is dropped before the row it belongs to is; scrolling brings it
		// back.
		if detail := attentionDetail(r); detail != "" && len(body)+1+reserve <= budget {
			body = append(body, pad+"  "+truncANSI(paint(theme.Pal.Subtle, detail), maxInt(inner-2, 1)))
		}
		end = i + 1
	}

	if off > 0 {
		out = append(out, pad+truncANSI(paint(theme.Pal.Subtle, fmt.Sprintf("▲ %d above", off)), inner))
	}
	out = append(out, body...)
	if n := len(a.rows) - end; n > 0 {
		out = append(out, pad+truncANSI(paint(theme.Pal.Subtle, fmt.Sprintf("▼ %d more", n)), inner))
	}
	// A pane under nine rows cannot hold the fixed lines, both counts and one
	// session at once. The footer is what is kept — it is the only place the
	// keys are named — and what gives way is whatever came just before it.
	if keep := h - len(footer); len(out) > keep {
		out = out[:maxInt(keep, 0)]
	}
	return append(out, footer...)
}

// attentionMaxOffset is the furthest the list can usefully be scrolled: the
// smallest offset from which every remaining session fits, detail lines
// included, below a "▲ N above" line. Scrolling further would only empty the
// top of the view.
func attentionMaxOffset(rows []ctrlAttentionRow, avail int) int {
	lines := 0
	for _, r := range rows {
		lines += attentionRowLines(r)
	}
	if lines <= avail {
		return 0 // everything fits from the top; there is nothing to scroll
	}
	used, o := 0, len(rows)
	for o > 0 && used+attentionRowLines(rows[o-1]) <= avail-1 {
		used += attentionRowLines(rows[o-1])
		o--
	}
	if o == len(rows) {
		// Not even the last session fits whole: let the offset reach it anyway,
		// so that it can at least be shown as a row without its detail.
		o = len(rows) - 1
	}
	return o
}

// attentionRowLines is how many lines one session occupies: its row, and the
// detail under it when it has one.
func attentionRowLines(r ctrlAttentionRow) int {
	if attentionDetail(r) != "" {
		return 2
	}
	return 1
}

// attentionClock is the document's own epoch, which is the instant every
// figure in this view is true of. requirements.md R2.8: a consumer comparing
// two reads must be able to compare their epochs, and a panel that showed the
// wall clock instead would be presenting an instant as if it were now.
func attentionClock(a ctrlAttention) string {
	if !a.have {
		return ""
	}
	return a.epoch.Format("15:04:05")
}

// attentionSummary is the one line a person reads first.
func attentionSummary(a ctrlAttention) string {
	parts := []string{}
	if a.needed > 0 {
		parts = append(parts, paint(theme.Pal.Warn, sgrBold+fmt.Sprintf("%d need you", a.needed)))
	} else {
		parts = append(parts, paint(theme.Pal.Subtle, "none need you"))
	}
	if a.unsure > 0 {
		parts = append(parts, paint(theme.Pal.Subtle, fmt.Sprintf("%d unsure", a.unsure)))
	}
	parts = append(parts, paint(theme.Pal.Subtle, fmt.Sprintf("of %d", a.total)))
	if a.problems > 0 {
		parts = append(parts, paint(theme.Pal.Fail, fmt.Sprintf("%d unreadable", a.problems)))
	}
	if a.dropped > 0 {
		// Said out loud rather than swallowed. The document is still correct —
		// it is rebuilt from the directory, not from the events — but a person
		// counting changes would be short.
		parts = append(parts, paint(theme.Pal.Subtle, fmt.Sprintf("%d dropped", a.dropped)))
	}
	return strings.Join(parts, paint(theme.Pal.Subtle, " · "))
}

// attentionRow is one session's line.
func attentionRow(r ctrlAttentionRow, focused, inner int) string {
	glyph, gc := attentionGlyph(r.needed)
	left := paint(gc, glyph) + " " + paint(theme.Pal.Text, attentionName(r))
	if r.paneID >= 0 {
		tag := fmt.Sprintf("¶%d", r.paneID)
		c := theme.Pal.Subtle
		if r.paneID == focused {
			// The pane the keyboard is in. Marked for the same reason the
			// route table marks it: a person looking at this list is about to
			// decide where to go, and "you are already here" is the cheapest
			// thing it can tell them.
			c = theme.Pal.Accent
		}
		left += " " + paint(c, tag)
	}
	right := paint(stateColor(r.status), obsview.OrDash(r.status))
	if r.hasInStatus {
		right += paint(theme.Pal.Subtle, " "+formatDuration(r.inStatus))
	}
	return padBetween(left, right, inner)
}

// attentionName is what to call the session: its name, else its pid.
func attentionName(r ctrlAttentionRow) string {
	if r.name != "" {
		return r.name
	}
	return "pid " + strconv.Itoa(r.pid)
}

// attentionGlyph renders the three-state verdict as THREE marks.
//
// "●", "?" and "·", never a tick against a blank. A blank in a yes/no column
// is read as a no by everyone who has ever read a table, so a session nobody
// could reach a verdict about would be filed under "fine" — which is the exact
// failure the *bool behind this column exists to prevent, reintroduced at the
// last possible moment. `magmux agents` spells the same three states "YES", "no"
// and "?"; the panel is 30 columns wide and uses marks, and the two are
// checked against each other by TestTheThreeSurfacesAgree.
func attentionGlyph(a attnState) (string, theme.RGB) {
	switch a {
	case attnYes:
		return "●", theme.Pal.Warn
	case attnUnknown:
		return "?", theme.Pal.Subtle
	default:
		return "·", theme.Pal.Subtle
	}
}

// attentionDetail is the sentence under a row, or "" for a row that does not
// need one.
func attentionDetail(r ctrlAttentionRow) string {
	switch {
	case r.conflict != "":
		// A conflict rides along even with a verdict decided by something
		// else, and then this is the only place it appears at all.
		if r.reason != "" {
			return r.reason + " — conflict: " + r.conflict
		}
		return "conflict: " + r.conflict
	case r.needed == attnNo:
		return ""
	case r.waiting != "" && r.waiting != "-":
		if r.reason != "" {
			return r.reason
		}
		return "waiting on " + r.waiting
	default:
		return r.reason
	}
}

// attentionFooter is the view's own key hint. A view nobody can find is a view
// nobody has.
func attentionFooter(pad string, inner int) []string {
	return []string{
		pad + paint(theme.Pal.Border, strings.Repeat("─", maxInt(inner, 1))),
		pad + truncANSI(paint(theme.Pal.Subtle, "v: control plane   j/k: scroll"), inner),
	}
}
