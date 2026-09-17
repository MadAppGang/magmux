package mux

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MadAppGang/magmux/buildinfo"
)

// magmuxLabel returns the status-bar app label, e.g. "magmux v3.4.2".
// Falls back to "magmux" when the version is the unreleased "dev" value.
func magmuxLabel() string {
	if buildinfo.Version == "" || buildinfo.Version == "dev" {
		return "magmux"
	}
	if strings.HasPrefix(buildinfo.Version, "v") {
		return "magmux " + buildinfo.Version
	}
	return "magmux v" + buildinfo.Version
}

// approxStatusWidth estimates the on-screen width of a tab-separated
// "CODE:text" status-bar string. Used to decide whether the status bar
// has enough room for the attribution tail. Not exact — overestimates
// slightly to stay on the safe side.
func approxStatusWidth(s string) int {
	segments := strings.Split(s, "\t")
	w := 1 // leading padding
	for i, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		parts := strings.SplitN(seg, ":", 2)
		txt := seg
		if len(parts) == 2 {
			txt = strings.TrimSpace(parts[1])
		}
		if i > 0 {
			w += 3 // " │ " divider
		}
		w += utf8.RuneCountInString(txt)
		if len(parts) == 2 {
			code := strings.TrimSpace(parts[0])
			switch code {
			case "P", "Pr", "Py":
				w += 2 // the pill's padding spaces
			case "*":
				w += 2 // renderStatusBar writes "* " in front of the label
			}
		}
	}
	return w
}

// fitNote cuts a plain-text status-bar segment to w painted columns, marking
// the cut with an ellipsis. Returns "" when there is no room at all.
//
// Deliberately NOT control.go's truncANSI, which is the panel's: that one wraps
// its ellipsis in SGR and closes with the panel's own ground state, and the bar
// has a different one (barBase), so the panel's colour would leak into every
// segment after it. approxStatusWidth counts runes, so the escape bytes would
// be counted as columns on top of that.
func fitNote(s string, w int) string {
	if w <= 0 {
		return ""
	}
	width := 0
	for _, r := range s {
		width += runeWidth(r)
	}
	if width <= w {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		cw := runeWidth(r)
		if n+cw > w-1 { // leave the last column for the ellipsis
			break
		}
		b.WriteRune(r)
		n += cw
	}
	b.WriteRune('…')
	return b.String()
}

// chromeNoteTTL is how long a refusal stays in the status bar. It clears on
// the next repaint after that; an idle magmux paints nothing, which is the
// whole rendering model, so the note can outstay this on a still screen.
const chromeNoteTTL = 4 * time.Second

// statusRowsLocked is how many rows the bottom status bar takes off the
// layout: 1 normally, 0 when it is hidden (--no-status / Ctrl-G s). Every
// place that used to write a bare `statusH := 1` goes through it, so showing
// and hiding the bar is one number rather than three that can drift apart.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) statusRowsLocked() int {
	if m.hideStatus {
		return 0
	}
	return 1
}

// reflowLocked resizes the whole tree to the current terminal minus the status
// row. It is the SIGWINCH path, reused verbatim by the two chrome toggles:
// showing or hiding either the panel or the status bar is a reflow and nothing
// more, and a second copy of this arithmetic is how the two would drift.
//
// Caller holds treeMu.Lock.
func (m *Magmux) reflowLocked() {
	if m.root == nil {
		return
	}
	m.root.resize(0, 0, maxInt(0, m.rows-m.statusRowsLocked()), maxInt(0, m.cols))
}

// panelLocked returns the control-panel pane whether it is on screen or
// hidden, or nil if this magmux has none or an agent closed it.
// Caller holds at least treeMu.RLock.
func (m *Magmux) panelLocked() *Pane {
	if m.panel == nil || m.panel.closed {
		return nil
	}
	return m.panel
}

// panelHiddenLocked reports whether the panel exists and is currently out of
// the tree. Caller holds at least treeMu.RLock.
func (m *Magmux) panelHiddenLocked() bool {
	p := m.panelLocked()
	return p != nil && p.hidden
}

// nodeInTreeLocked reports whether n is still reachable from m.root. The panel
// anchor is a raw pointer to a node that may since have been collapsed away by
// a close_pane, and splicing onto a detached node would attach the panel to a
// subtree nothing paints — invisible and undismissable, the same failure
// OpenPane re-verifies against.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) nodeInTreeLocked(n *Pane) bool {
	if n == nil {
		return false
	}
	var walk func(*Pane) bool
	walk = func(p *Pane) bool {
		if p == nil {
			return false
		}
		if p == n {
			return true
		}
		return walk(p.child1) || walk(p.child2)
	}
	return walk(m.root)
}

// markAllDirtyLocked forces a full repaint. Geometry changed under every pane,
// and the dirty-flag model means a frame is only painted when some pane says
// its CONTENT changed — so without this a reflow can sit unpainted until the
// next keystroke. Caller holds at least treeMu.RLock.
func (m *Magmux) markAllDirtyLocked() {
	for _, p := range m.livePanesLocked(nil) {
		if p.hidden {
			continue
		}
		p.mu.Lock()
		p.dirty = true
		p.mu.Unlock()
	}
}

// noteChromeLocked parks a transient message for the status bar.
// Caller holds treeMu.Lock.
func (m *Magmux) noteChromeLocked(s string) {
	m.chromeNote = s
	m.chromeNoteAt = time.Now()
}

// noteRowLocked puts a live refusal note in front of the status row's own text.
//
// The note rides in FRONT of the run's summary rather than being written into
// it: it belongs to the keystroke that caused it. If the two together would
// overrun, the keystroke wins — the run's summary is back on the next frame,
// the refusal is not.
//
// Caller holds at least treeMu.RLock. Split out of renderLocked so the width
// can be measured by a test; renderStatusBar pads its segments and never
// truncates them, so nothing downstream will catch an overrun.
func (m *Magmux) noteRowLocked(text string) string {
	n := m.chromeNoteLocked()
	if n == "" {
		return text
	}
	// The note itself was the one thing on this row nobody measured, and the two
	// that exist are both longer than the terminals that produce them:
	// panelTooNarrow is 41 runes and splitFits refuses at exactly the widths it
	// cannot fit, and the alternate-screen note is 50 and fires on Ctrl-G [
	// against any Claude Code or vim pane. renderStatusBar pads its segments and
	// never truncates them, so an unbounded note wraps onto the pane above and
	// corrupts the session's output. The budget is m.cols minus the bar's own
	// leading pad, which is the only column renderStatusBar spends before this.
	note := "R: " + fitNote(n, m.cols-1)
	if text != "" && approxStatusWidth(note+"\t"+text) <= m.cols {
		note += "\t" + text
	}
	return note
}

// chromeNoteLocked returns the live refusal message, or "" once it has expired.
// Caller holds at least treeMu.RLock.
func (m *Magmux) chromeNoteLocked() string {
	if m.chromeNote == "" || time.Since(m.chromeNoteAt) > chromeNoteTTL {
		return ""
	}
	return m.chromeNote
}

// installHiddenPanel creates the control panel and gives it an id, but leaves
// it OUT of the layout tree.
//
// It is deliberately not a config appended to the layout builders. Handing them
// an extra command would change the shape they build — buildLayout's 3+ branch
// only ever builds three panes and would silently drop it — and the panel would
// then have to be surgically removed again to start hidden. Building the
// session layout exactly as it has always been built and adding the panel to
// the id table afterwards means the visible layout of a hidden-panel magmux is
// byte-identical to a magmux that has no panel at all.
//
// The panel still lands LAST in m.allPanes, so session panes keep the indices a
// controller would naturally use (pane 0 is the first -e command).
//
// Caller must NOT hold treeMu.
func (m *Magmux) installHiddenPanel() *Pane {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	if p := m.panelLocked(); p != nil {
		return p
	}
	// Plausible geometry so nothing reads a zero-sized screen before the first
	// Ctrl-G p; showPanelLocked resizes it for real through reshapeChildren.
	h := maxInt(1, m.rows-m.statusRowsLocked())
	w := maxInt(1, m.cols/2)
	p := newControlPane(0, maxInt(0, m.cols-w), h, w, "")
	p.hidden = true
	p.gridMode = m.gridMode
	m.allPanes = append(m.allPanes, p)
	m.panel = p
	m.stampPaneIDs()
	return p
}

// ── Chrome: the control panel and the status bar ─────────────────────────────
//
// magmux's default is now to show nothing of itself. `magmux -e claude` is a
// bare terminal running Claude Code: one leaf, no border (renderBorder only
// ever paints a SPLIT node, so a lone leaf has nothing to draw), the whole
// terminal minus the status row. Ctrl-G p reveals the panel, Ctrl-G s drops
// the status row, and -c means "start with the panel already visible" so every
// invocation that predates this looks exactly as it did.

// panelTooNarrow is what the status bar says when the terminal cannot carry
// both a session and a panel at the minimum a pane is usable at.
const panelTooNarrow = "no room for the panel — widen the terminal"

// splitFits reports whether splitting t leaves both halves usable. Same
// arithmetic and the same floor as OpenPane, deliberately: "usable" cannot
// mean one thing for an agent opening a pane and another for the panel.
func splitFits(t *Pane, st SplitType, ratio float64) bool {
	if t == nil {
		return false
	}
	if st == SplitHorizontal {
		w1 := int(float64(t.w) * ratio)
		return w1 >= minPaneCols && t.w-w1-1 >= minPaneCols && t.h >= minPaneRows
	}
	h1 := int(float64(t.h) * ratio)
	return h1 >= minPaneRows && t.h-h1-1 >= minPaneRows && t.w >= minPaneCols
}

// hidePanelLocked takes the panel out of the layout and gives its space back.
//
// This is NOT a close. `closed` is a tombstone and is permanent; `dead` is a
// child that exited. A hidden panel is alive, keeps every row of its OUT/IN
// ledger, keeps its id, and is still reported by buildPaneResults as
// state:"panel" — it is merely not in the tree, so it costs no columns and the
// sessions reflow over it.
//
// Caller holds treeMu.Lock.
func (m *Magmux) hidePanelLocked(p *Pane) {
	if par := p.parent; par != nil {
		sib := par.child1
		if sib == p {
			sib = par.child2
		}
		m.panelAnchor = sib
		m.panelSplit = par.splitType
		m.panelRatio = par.ratio
		m.panelFirst = par.child1 == p
	} else {
		// The panel was the whole tree (magmux -c with no -e). There is no
		// sibling to anchor to; showing it again makes it the root.
		m.panelAnchor, m.panelSplit, m.panelRatio, m.panelFirst = nil, SplitHorizontal, 0.5, false
	}
	m.removeLeafLocked(p)
	p.hidden = true
	// A selection anchored in a pane that is no longer on screen would paint
	// its highlight over whatever grew into that space.
	if sel.pane == p {
		m.selClear()
	}
	// Focus must not be left on something invisible: every keystroke would go
	// to a pane nobody can see. firstLiveLeaf prefers a real session over the
	// panel, which is exactly what is wanted here.
	if m.focused == p {
		m.focused = firstLiveLeaf(m.root)
	}
}

// showPanelLocked splices the panel back where it was, or reports that there
// is no room. It does NOT touch focus: the human is typing into their agent,
// and revealing an instrument must not take the keyboard away from them —
// which is the same reason main() moves focus off the panel at startup.
//
// Caller holds treeMu.Lock.
func (m *Magmux) showPanelLocked(p *Pane) bool {
	st, ratio := m.panelSplit, m.panelRatio
	if st == SplitNone {
		st = SplitHorizontal
	}
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.5
	}

	t := m.panelAnchor
	if !m.nodeInTreeLocked(t) {
		// The node the panel was taken from is gone (an agent closed the pane
		// under it). The root is the honest fallback: the panel comes back as a
		// column beside everything else, which is where the layout builders put
		// it in the first place.
		t = m.root
	}
	if t == nil {
		// Empty tree — the panel is the layout.
		p.hidden = false
		p.parent = nil
		m.root = p
		m.reflowLocked()
		return true
	}
	if !splitFits(t, st, ratio) {
		// Refuse rather than reshape into a 0-column pane. reshapeChildren
		// clamps at zero, so the layout would survive — as a panel with no
		// columns in it, which is worse than not showing it at all.
		return false
	}
	p.hidden = false
	m.splitNodeLocked(t, p, st, ratio, m.panelFirst)
	return true
}

// togglePanel reveals or hides the control panel. Caller must NOT hold treeMu.
func (m *Magmux) togglePanel() {
	m.treeMu.Lock()
	p := m.panelLocked()
	if p == nil {
		m.treeMu.Unlock()
		return
	}
	shown, refocus := false, -1
	if p.hidden {
		if shown = m.showPanelLocked(p); !shown {
			m.noteChromeLocked(panelTooNarrow)
		}
	} else {
		m.hidePanelLocked(p)
		if m.focused != nil {
			refocus = m.focused.id
		}
	}
	m.markAllDirtyLocked()
	m.treeMu.Unlock()

	// cp.mu is taken with treeMu released. The documented order treeMu -> cp.mu
	// would allow it above, but nothing here needs the two held together and
	// the shorter treeMu hold is free.
	if refocus >= 0 {
		m.control.setFocused(refocus)
	}
	if shown {
		// The panel has no child process to redraw it, so a reflow leaves it
		// blank unless it is told — the same reason SIGWINCH marks it dirty.
		m.control.markDirty()
	}
}

// toggleStatusBar shows or hides the bottom status row, giving the row to the
// layout or taking it back. Caller must NOT hold treeMu.
func (m *Magmux) toggleStatusBar() {
	m.treeMu.Lock()
	m.hideStatus = !m.hideStatus
	m.reflowLocked()
	m.markAllDirtyLocked()
	m.treeMu.Unlock()
	m.control.markDirty()
}

// ── the status bar's panel digest ────────────────────────────────────────────
//
// The panel starts hidden, so the status bar is the only place a run announces
// itself until somebody asks for the panel. What it carries is the panel's own
// two counters — `▶` what the controller asked for, `◀` what magmux observed —
// plus the newest signal, degrading to the counters alone as the terminal
// narrows. It invents no third number: a status bar that reconciled the two
// would be a second, quieter provenance model beside the panel's.
//
// The done/running counts stay. They answer a different question ("how much of
// this grid has finished") from the counters ("how many instructions has the
// controller issued, and how many turns has magmux seen close"), and on a run
// with several panes and one controller they routinely disagree — which is
// exactly the disagreement worth seeing.

// statusSeg joins two tab-separated status-bar segments.
func statusSeg(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\t" + b
}

// digestStateSeg is "p0 working · Bash 14s" — which pane, doing what, since
// when. Empty when the controller has not touched a pane yet.
func digestStateSeg(d ctrlDigest) string {
	if d.pane < 0 || d.state == "" {
		return ""
	}
	s := fmt.Sprintf("p%d %s", d.pane, d.state)
	if d.tool != "" {
		s += " · " + d.tool
	}
	if !d.stateAt.IsZero() {
		s += " " + formatDuration(time.Since(d.stateAt))
	}
	return "M: " + s
}

// digestSignalSeg is the newest row of the panel's stream, cut to `budget`
// columns. Empty when there is no room for a useful amount of it — a signal
// truncated to three characters and an ellipsis is noise, not information.
func digestSignalSeg(d ctrlDigest, budget int) string {
	if d.sigVerb == "" && d.sigText == "" {
		return ""
	}
	glyph := "•"
	switch d.sigDir {
	case "out":
		glyph = "▶"
	case "in":
		glyph = "◀"
	}
	head := glyph + " " + d.sigVerb
	if d.sigVerb == "" {
		head = glyph
	}
	body := oneLine(d.sigText, maxInt(0, budget-utf8.RuneCountInString(head)-4))
	if body != "" {
		head += ` "` + body + `"`
	}
	if utf8.RuneCountInString(head) < 6 {
		return ""
	}
	return "D: " + head
}

// appendPanelDigestLocked adds as much of the digest as fits in m.cols, widest
// part first out. Caller holds at least treeMu.RLock; d was taken with cp.mu
// already released.
func (m *Magmux) appendPanelDigestLocked(segs string, d ctrlDigest, reserve int) string {
	if !d.active {
		return segs
	}
	fits := func(s string) bool { return approxStatusWidth(s) <= m.cols }

	// The counters are the digest, so they are the LAST thing dropped — but
	// they are still dropped. A status bar that overruns m.cols wraps onto the
	// pane above it, and corrupting a session's output to announce that magmux
	// is here is the exact opposite of what this change is for.
	out := statusSeg(segs, fmt.Sprintf("W: ▶%d ◀%d", d.sent, d.observed))
	if !fits(out) {
		return segs
	}

	// The key hints are what make a hidden panel discoverable, so their room is
	// reserved before the state and the signal are measured rather than
	// competing with them. The caller appends the hints themselves — see
	// appendKeyHint — once the digest has taken what it can.
	if st := digestStateSeg(d); st != "" && approxStatusWidth(statusSeg(out, st))+reserve <= m.cols {
		out = statusSeg(out, st)
	}
	if budget := m.cols - approxStatusWidth(out) - 3 - reserve; budget >= 14 {
		// Re-measured rather than trusted: digestSignalSeg cuts the signal's
		// TEXT to the budget, but its head (the direction glyph and the verb)
		// is whatever the verb is, so a long verb can overrun a budget that was
		// only ever applied to the quoted body — and it would overrun it into
		// the hints' floor.
		if sig := digestSignalSeg(d, budget); sig != "" {
			if cand := statusSeg(out, sig); approxStatusWidth(cand)+reserve <= m.cols {
				out = cand
			}
		}
	}
	return out
}

// ── the status bar's key hints ───────────────────────────────────────────────
//
// magmux is invisible by default: no panel, no border, one status row. That row
// is therefore the ONLY place a user can find out that a control panel exists
// at all — the panel is not on screen to advertise itself, and `--help` is not
// on screen either. So `p` is the discovery-critical hint and outranks
// everything on the bar except `q`, the way out.
//
// Survival order as the terminal narrows, last dropped first: q, p, Tab, s.
// `Tab` only matters with more than one pane on screen, and `s` is the least
// useful column magmux can spend — hiding the bar hides its own hint, so the
// one person who wants it gone can find it in `--help`.
//
// The hints are not appended after everything else has taken its room: the run
// summary yields to them (see statusForms / fitStatusBase) and the digest
// reserves their floor before it measures its optional parts. A hint that only
// showed up on a wide terminal would be a hint for the people who least need
// it.

// hintItem is one key hint and the rank at which it is dropped: the lowest rank
// goes first, and rank 0 is reserved for "keep everything".
type hintItem struct {
	text string
	rank int
}

const (
	hintRankStatus = iota + 1 // s: hiding the bar hides this hint with it
	hintRankScroll            // [: only offered when there is history to reach
	hintRankTab               // Tab: nothing to switch to with one pane on screen
	hintRankPanel             // p: the surface nothing else can announce
	hintRankQuit              // q: the way out
)

// keyHintItems is the chord's full hint set, in reading order. panelVisible
// flips "p panel" to "p hide" so the key's effect is unambiguous in both
// states; multiPane drops Tab when there is nothing to switch to; canScroll
// offers "[ back" only when the focused pane actually has something behind it.
//
// The scroll hint is conditional rather than permanent because most of what
// magmux runs is on the ALTERNATE screen — Claude Code, vim, htop — and those
// panes keep no scrollback at all by design. Advertising a key that does
// nothing on the pane you are looking at teaches the wrong thing about the
// feature; offering it the moment a shell pane has scrolled teaches the right
// one.
//
// It ranks below Tab so the floor the rest of the bar yields to (panel + quit)
// is unchanged: the hint appears when there is room, and is the second thing to
// go when there is not.
func keyHintItems(panelVisible, multiPane, canScroll bool) []hintItem {
	panel := "p panel"
	if panelVisible {
		panel = "p hide"
	}
	items := []hintItem{{panel, hintRankPanel}}
	if multiPane {
		items = append(items, hintItem{"Tab", hintRankTab})
	}
	if canScroll {
		items = append(items, hintItem{"[ back", hintRankScroll})
	}
	return append(items, hintItem{"s bar", hintRankStatus}, hintItem{"q quit", hintRankQuit})
}

// keyHintLocked is the hint set for the current layout, or nil when the chord
// is inert — a grid whose panes have all EXITED swallows every key but
// q/Esc/Ctrl-C, so advertising p there would be advertising a key that does
// nothing.
//
// The predicate is allPanesDead, not allPanesDone, and the difference is the
// whole of issue #333: an idle pane is a live agent between turns, the chord
// still works on it, and blanking the hints told a person their keyboard was
// gone at the exact moment it was not.
//
// Caller holds at least treeMu.RLock.
func (m *Magmux) keyHintLocked() []hintItem {
	if m.gridMode && m.allPanesDeadLocked() {
		return nil
	}
	onScreen := 0
	for _, p := range m.livePanesLocked(nil) {
		if !p.hidden {
			onScreen++
		}
	}
	// treeMu -> p.mu is the legal order, so reading the focused pane's history
	// depth from here is fine.
	canScroll := false
	if f := m.focused; f != nil && !f.isControl && f.screen != nil {
		f.mu.Lock()
		canScroll = f.screen.sbLen > 0
		f.mu.Unlock()
	}
	return keyHintItems(!m.panelHiddenLocked(), onScreen > 1, canScroll)
}

// keyHintList joins the hints, dropping everything ranked at or below `cut`.
func keyHintList(items []hintItem, cut int) string {
	var parts []string
	for _, it := range items {
		if it.rank > cut {
			parts = append(parts, it.text)
		}
	}
	return strings.Join(parts, " · ")
}

// hintFloor is the form the rest of the bar yields to: the panel and the way
// out. Everything above it (Tab, s) competes for leftovers like any other
// segment.
func hintFloor(items []hintItem) string { return keyHintList(items, hintRankTab) }

// hintFloorWidth is what hintFloor costs on the bar, divider included. Zero
// when there are no hints to place.
func hintFloorWidth(items []hintItem) int {
	f := hintFloor(items)
	if f == "" {
		return 0
	}
	return utf8.RuneCountInString("ctrl-g "+f) + 3
}

// appendKeyHint puts as much of the hint set on the bar as `cols` allows,
// dropping items in reverse priority. It never returns a bar wider than cols:
// a status row that overruns wraps onto the pane above it, which is the one
// thing magmux's chrome must never do to a session's output.
func appendKeyHint(segs string, items []hintItem, cols int) string {
	for cut := 0; cut <= hintRankQuit; cut++ {
		list := keyHintList(items, cut)
		if list == "" {
			break
		}
		if out := statusSeg(segs, "D: ctrl-g "+list); approxStatusWidth(out) <= cols {
			return out
		}
	}
	// Last resort. A terminal too narrow for "ctrl-g q quit" is still a
	// terminal somebody has to be able to get out of.
	if len(items) > 0 {
		if out := statusSeg(segs, "D: ^g q"); approxStatusWidth(out) <= cols {
			return out
		}
	}
	return segs
}

// fitStatusBase picks the widest run summary that still leaves the hints their
// floor. The forms are widest first, and each drops the least valuable segment
// of the one before it: the running count is derivable from the done pill, the
// timer is a nice-to-have, and magmux's own name is the first thing a narrow
// terminal should stop spending columns on.
func fitStatusBase(budget int, forms ...string) string {
	for _, f := range forms {
		if approxStatusWidth(f) <= budget {
			return f
		}
	}
	return ""
}

// chordMenuLocked is what the bar says while Ctrl-G has been pressed and magmux
// is waiting for the second key: the chord teaching itself, for one keystroke,
// on the one row magmux already owns. Empty when there is nothing to offer.
// Caller holds at least treeMu.RLock.
func (m *Magmux) chordMenuLocked(items []hintItem) string {
	for cut := 0; cut <= hintRankQuit; cut++ {
		list := keyHintList(items, cut)
		if list == "" {
			break
		}
		if s := "C: ctrl-g …\tD: " + list; approxStatusWidth(s) <= m.cols {
			return s
		}
	}
	return ""
}
