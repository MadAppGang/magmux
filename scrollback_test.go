package main

// Scrollback: the history a pane keeps behind its visible screen, who can reach
// it, and the one rule that decides how far the feature reaches at all.
//
// That rule is the alternate screen. A terminal does not record the alt screen
// into history — it is why quitting vim does not leave its buffer in your
// scrollback — and magmux must not either. The consequence is worth stating
// where the tests are, because it is the first thing anybody using this will
// hit: Claude Code, vim, htop and every other full-screen app run on the alt
// screen, so those panes have little or no scrollback and their own transcript
// remains the authoritative history. Scrollback is for shell panes: builds,
// test runs, dev servers.

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// feed pushes bytes through the pane's real VT parser, under p.mu exactly as
// readLoop does. Driving the parser rather than poking Screen.cells is the
// point: scrollUp is reached the way a child reaches it, so a test cannot pass
// against a scroll path the parser never takes.
func feed(p *Pane, s string) {
	p.mu.Lock()
	p.vt.write([]byte(s))
	p.dirty = true
	p.mu.Unlock()
}

// feedLines writes `prefix1 … prefixN`, one per line, CRLF-terminated.
func feedLines(p *Pane, prefix string, n int) {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s%d\r\n", prefix, i)
	}
	feed(p, b.String())
}

// scrollPane is a bare pane with a screen and a parser and no process: enough
// to exercise every scrollback path, with no PTY timing in the way.
func newScrollPane(rows, cols int) *Pane {
	p := &Pane{y: 0, x: 0, h: rows, w: cols, charsetG0: 'B', charsetG1: 'B'}
	p.screen = newScreen(rows, cols)
	p.primaryScreen = p.screen
	p.vt.node = p
	return p
}

// captureLines is a captureAt with the blank-tail strip off, so a row that is
// genuinely blank still occupies a position and the caller's indexing matches
// the screen's.
func captureLines(p *Pane, offset int) []string {
	return strings.Split(p.captureAt(offset, 0, false).Text, "\n")
}

// ── the ring itself ─────────────────────────────────────────────────────────

// TestScrollbackKeepsEvictedLinesInOrder is the base claim: a line that scrolls
// off the top of a primary screen is retrievable afterwards, in the order it was
// printed. Before this change scrollUp recycled the evicted row to the bottom
// and blanked it, so every one of these reads came back as the visible screen.
func TestScrollbackKeepsEvictedLinesInOrder(t *testing.T) {
	p := newScrollPane(10, 40)
	feedLines(p, "L", 30)

	// 30 lines through a 10-row screen: the first nine fill rows 0-8, and every
	// line from the tenth on scrolls one row off the top. That is 21 evictions,
	// L1 through L21, leaving L22-L30 on rows 0-8 and row 9 blank.
	p.mu.Lock()
	have := p.screen.sbLen
	p.mu.Unlock()
	if have != 21 {
		t.Fatalf("scrollback holds %d rows after 30 lines through a 10-row screen, want 21", have)
	}

	live := captureLines(p, 0)
	if live[0] != "L22" || live[8] != "L30" {
		t.Errorf("the live screen is %q, want L22 … L30", live)
	}

	// One screenful back is the ten lines immediately above the live screen.
	back := captureLines(p, 10)
	for i, want := range []string{"L12", "L13", "L14", "L15", "L16", "L17", "L18", "L19", "L20", "L21"} {
		if back[i] != want {
			t.Errorf("offset 10, row %d = %q, want %q (full window %q)", i, back[i], want, back)
		}
	}

	// All the way back is the oldest line magmux still holds.
	top := p.captureAt(21, 0, false)
	oldest := strings.Split(top.Text, "\n")
	if oldest[0] != "L1" {
		t.Errorf("the oldest screenful starts at %q, want L1 (window %q)", oldest[0], oldest)
	}
	if !top.AtTop {
		t.Errorf("a capture showing the oldest line did not report AtTop")
	}

	// Every line ever printed is reachable at some offset, which is the claim a
	// caller actually relies on.
	seen := map[string]bool{}
	for off := 0; off <= 21; off++ {
		for _, line := range captureLines(p, off) {
			seen[line] = true
		}
	}
	for i := 1; i <= 30; i++ {
		if !seen[fmt.Sprintf("L%d", i)] {
			t.Errorf("L%d is not reachable at any offset", i)
		}
	}
}

// TestScrollbackDropsTheOldestPastItsBound. The ring is bounded, and the bound
// is a promise about memory rather than about content: past it the OLDEST lines
// go, because the newest are the ones anybody is looking for.
func TestScrollbackDropsTheOldestPastItsBound(t *testing.T) {
	p := newScrollPane(10, 40)
	p.screen.sbCap = 5
	feedLines(p, "L", 30)

	p.mu.Lock()
	have, cap := p.screen.sbLen, p.screen.sbCap
	p.mu.Unlock()
	if have != 5 {
		t.Fatalf("a 5-row ring holds %d rows after 21 evictions, want 5", have)
	}
	if cap != 5 {
		t.Fatalf("the ring's capacity moved to %d", cap)
	}

	// The five survivors are the five most recent evictions: L17 … L21.
	top := p.captureAt(5, 0, false)
	rows := strings.Split(top.Text, "\n")
	for i, want := range []string{"L17", "L18", "L19", "L20", "L21"} {
		if rows[i] != want {
			t.Errorf("the oldest window row %d = %q, want %q (window %q)", i, rows[i], want, rows)
		}
	}
	if !top.AtTop || top.Scrollback != 5 {
		t.Errorf("capture at the top reported scrollback=%d atTop=%v, want 5/true",
			top.Scrollback, top.AtTop)
	}
	// L16 and everything before it is gone, and asking for more history than
	// exists says so rather than inventing it.
	if strings.Contains(top.Text, "L16") {
		t.Errorf("a line past the bound survived: %q", top.Text)
	}
	if far := p.captureAt(500, 0, false); far.Offset != 5 || far.Text != top.Text {
		t.Errorf("an offset past the top returned offset %d rather than clamping to 5", far.Offset)
	}
}

// TestScrollbackIsAllocationFreeOnceFull pins the hot-path constraint. scrollUp
// runs on every newline at the bottom of every pane; a per-line allocation there
// would be paid by every busy pane forever. The ring is filled by SWAPPING —
// the row dropped from the ring becomes the new bottom of the screen — so once
// it is full the steady state allocates nothing at all.
func TestScrollbackIsAllocationFreeOnceFull(t *testing.T) {
	s := newScreen(10, 80)
	s.sbCap = 64
	for i := 0; i < s.sbCap+2; i++ {
		s.scrollUp(0, s.rows)
	}
	if n := testing.AllocsPerRun(200, func() { s.scrollUp(0, s.rows) }); n != 0 {
		t.Errorf("scrollUp allocates %.1f times per line once the ring is full, want 0", n)
	}
}

// TestScreenAllocatesOnlyItsViewport. The dead `rows + scrollbackLines` tail is
// gone: it was never written and never read, and keeping it beside a real ring
// would be worse than either alone.
func TestScreenAllocatesOnlyItsViewport(t *testing.T) {
	s := newScreen(10, 40)
	if len(s.cells) != 10 {
		t.Errorf("a 10-row screen allocates %d rows of cells, want exactly 10", len(s.cells))
	}
	if s.sb != nil {
		t.Errorf("a screen that has never scrolled allocated its ring up front")
	}
	s.resize(12, 30)
	if len(s.cells) != 12 {
		t.Errorf("after resize the screen holds %d rows of cells, want 12", len(s.cells))
	}
}

// ── the alt-screen rule ─────────────────────────────────────────────────────

// TestAltScreenNeverEntersScrollback is the load-bearing test of the whole
// change.
//
// A full-screen app scrolls constantly — every redraw of a list, every page of
// a pager — and none of it is output that "scrolled off". Recording it would
// flush the shell history a human actually wants behind a thousand rows of vim
// frames, and it is not what a terminal does. So the alternate screen keeps no
// history, and a round trip through it must leave the primary's untouched.
func TestAltScreenNeverEntersScrollback(t *testing.T) {
	p := newScrollPane(10, 40)
	feedLines(p, "SHELL", 20)

	p.mu.Lock()
	before := p.screen.sbLen
	p.mu.Unlock()
	if before == 0 {
		t.Fatalf("the fixture produced no primary scrollback to protect")
	}
	liveBefore := p.captureAt(0, 0, false).Text
	histBefore := p.captureAt(before, 0, false).Text

	// Into the alt screen, a great deal of scrolling, and back out — the shape
	// of every vim or htop session.
	feed(p, "\x1b[?1049h")
	if !p.altMode {
		t.Fatalf("DEC 1049 did not put the pane on the alternate screen")
	}
	feedLines(p, "ALT", 60)

	p.mu.Lock()
	altLen, altCap := p.screen.sbLen, p.screen.sbCap
	p.mu.Unlock()
	if altCap != 0 {
		t.Errorf("the alternate screen has scrollback capacity %d, want 0", altCap)
	}
	if altLen != 0 {
		t.Errorf("the alternate screen recorded %d rows of scrollback", altLen)
	}
	// And it cannot be reached through capture either, which is where an agent
	// would notice.
	if shot := p.captureAt(30, 0, false); shot.Scrollback != 0 || shot.Offset != 0 {
		t.Errorf("capture on an alt-screen pane offered scrollback %d at offset %d, want 0/0",
			shot.Scrollback, shot.Offset)
	}

	feed(p, "\x1b[?1049l")
	if p.altMode {
		t.Fatalf("DEC 1049 reset did not return the pane to the primary screen")
	}

	p.mu.Lock()
	after := p.screen.sbLen
	p.mu.Unlock()
	if after != before {
		t.Errorf("the primary's scrollback is %d rows after an alt round trip, was %d", after, before)
	}
	if got := p.captureAt(0, 0, false).Text; got != liveBefore {
		t.Errorf("the primary screen changed across an alt round trip:\n got %q\nwant %q", got, liveBefore)
	}
	if got := p.captureAt(before, 0, false).Text; got != histBefore {
		t.Errorf("the primary's history changed across an alt round trip:\n got %q\nwant %q", got, histBefore)
	}
	// The decisive assertion: not one row of what the alt screen did is in the
	// history the shell will scroll back through.
	for off := 0; off <= before; off++ {
		if text := p.captureAt(off, 0, false).Text; strings.Contains(text, "ALT") {
			t.Fatalf("alt-screen output reached the primary's scrollback at offset %d: %q", off, text)
		}
	}
}

// TestScrollRegionScrollDoesNotRecord. A child that set DECSTBM is animating
// part of its own frame — a pager's body under a fixed header, a TUI's log
// area — and those rows never left the screen in the sense scrollback means.
// Only a full-screen scroll writes history, which is what every terminal does.
func TestScrollRegionScrollDoesNotRecord(t *testing.T) {
	p := newScrollPane(10, 40)
	feed(p, "\x1b[1;5r") // scroll region = rows 1..5, i.e. not the whole screen
	feedLines(p, "R", 30)

	p.mu.Lock()
	have := p.screen.sbLen
	p.mu.Unlock()
	if have != 0 {
		t.Errorf("a scroll inside a DECSTBM region recorded %d rows of scrollback, want 0", have)
	}
}

// ── resize ──────────────────────────────────────────────────────────────────

// TestScrollbackSurvivesResizeUnreflowed pins the choice, because both answers
// are defensible and the one that is implemented should be the one that is
// asserted.
//
// History SURVIVES a resize and is NOT reflowed. Dropping it was rejected
// because a resize happens every time the human reveals the control panel, and
// history that evaporates on a window nudge is history nobody trusts. Reflowing
// was rejected because magmux does not record which rows were soft-wrapped
// continuations, so re-wrapping would invent line breaks the child never wrote.
// A history row therefore comes back at the width it was printed at.
func TestScrollbackSurvivesResizeUnreflowed(t *testing.T) {
	p := newScrollPane(10, 40)
	long := strings.Repeat("x", 30)
	feed(p, long+"\r\n")
	feedLines(p, "L", 30)

	p.mu.Lock()
	before := p.screen.sbLen
	p.mu.Unlock()

	p.mu.Lock()
	p.screen.resize(10, 20) // the terminal is halved
	after := p.screen.sbLen
	p.mu.Unlock()

	if after != before {
		t.Errorf("a resize took the scrollback from %d rows to %d; it must survive", before, after)
	}
	// The 30-character line was evicted from a 40-column screen and comes back
	// whole, on a screen that is now 20 columns wide.
	found := false
	for off := 0; off <= after; off++ {
		if strings.Contains(p.captureAt(off, 0, false).Text, long) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("a %d-character history line did not survive a narrowing resize intact", len(long))
	}
	// And the live screen still paints: a recycled row of the wrong width
	// reaching the viewport would be an out-of-range panic in renderPane.
	feedLines(p, "AFTER", 30)
	if got := captureLines(p, 0)[0]; !strings.HasPrefix(got, "AFTER") {
		t.Errorf("the pane stopped printing after a resize: first row %q", got)
	}
}

// ── the capture verb ────────────────────────────────────────────────────────

// TestCaptureReportsHowMuchHistoryExists. An offset is only useful with a bound
// beside it: a caller walking backwards has to know how far it can go and when
// it has arrived, and inferring either from a repeated answer is how a driver
// ends up in a loop.
func TestCaptureReportsHowMuchHistoryExists(t *testing.T) {
	p := newScrollPane(10, 40)

	if fresh := p.captureAt(0, 0, true); fresh.Scrollback != 0 || !fresh.AtTop {
		t.Errorf("a pane that has never scrolled reports scrollback=%d atTop=%v, want 0/true",
			fresh.Scrollback, fresh.AtTop)
	}

	feedLines(p, "L", 30)
	live := p.captureAt(0, 0, true)
	if live.Scrollback != 21 {
		t.Errorf("capture reports %d rows of scrollback, want 21", live.Scrollback)
	}
	if live.Offset != 0 || live.AtTop {
		t.Errorf("the live screen reports offset=%d atTop=%v, want 0/false", live.Offset, live.AtTop)
	}

	mid := p.captureAt(10, 0, true)
	if mid.Offset != 10 || mid.AtTop {
		t.Errorf("a mid-history capture reports offset=%d atTop=%v, want 10/false", mid.Offset, mid.AtTop)
	}
	if mid.Scrollback != 21 {
		t.Errorf("the history depth changed with the offset: %d", mid.Scrollback)
	}
	// lastN still means "the last N rows of THIS window", so the two controls
	// compose rather than fighting.
	if cut := p.captureAt(10, 3, true); strings.Count(cut.Text, "\n") != 2 || !cut.Truncated {
		t.Errorf("offset with lines:3 returned %q truncated=%v, want three rows", cut.Text, cut.Truncated)
	}
	// capture() is captureAt(0), byte for byte: every existing caller is
	// unaffected by the new argument.
	if a, b := p.capture(0, true).Text, p.captureAt(0, 0, true).Text; a != b {
		t.Errorf("capture and captureAt(0) disagree:\n %q\n %q", a, b)
	}
}

// TestSockCaptureServesAnOffset covers the wire shape an agent sees: the fields
// are always present, the offset the reply reports is the one magmux SETTLED on,
// and the rows are the ones a human scrolled to that offset would be reading.
func TestSockCaptureServesAnOffset(t *testing.T) {
	m := newTestMux(t, ctrlPanes(1)...)
	p := m.paneByID(0)
	feedLines(p, "L", 200)

	live, err := m.sockCapture(sockMsg{Pane: float64(0)})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	depth, _ := live["scrollback"].(int)
	if depth <= 0 {
		t.Fatalf("capture reported %v rows of scrollback; the fixture never scrolled", live["scrollback"])
	}
	if live["offset"] != 0 || live["atTop"] != false {
		t.Errorf("a live capture reported offset=%v atTop=%v, want 0/false", live["offset"], live["atTop"])
	}

	back, err := m.sockCapture(sockMsg{Pane: float64(0), Offset: 5})
	if err != nil {
		t.Fatalf("capture with an offset: %v", err)
	}
	if back["offset"] != 5 {
		t.Errorf("capture offset:5 reported offset %v", back["offset"])
	}
	liveText, _ := live["text"].(string)
	backText, _ := back["text"].(string)
	if liveText == backText {
		t.Errorf("capture at offset 5 returned the live screen unchanged: %q", backText)
	}
	if !strings.Contains(backText, "L") {
		t.Errorf("capture at offset 5 returned nothing recognisable: %q", backText)
	}

	// Past the top clamps and says so, so a caller knows to stop walking.
	top, err := m.sockCapture(sockMsg{Pane: float64(0), Offset: 100000})
	if err != nil {
		t.Fatalf("capture past the top: %v", err)
	}
	if top["atTop"] != true {
		t.Errorf("a capture past the oldest line did not report atTop: %v", top)
	}
	if got, _ := top["offset"].(int); got != depth {
		t.Errorf("an over-large offset reported %v rather than clamping to %d", top["offset"], depth)
	}

	// And the capability probe advertises the ceiling rather than 0.
	caps, err := m.sockCapabilities()
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if caps["scrollback"] != scrollbackLimit || scrollbackLimit == 0 {
		t.Errorf("capabilities reports scrollback %v, want the real limit %d",
			caps["scrollback"], scrollbackLimit)
	}
}

// ── human scrolling ─────────────────────────────────────────────────────────

// scrollInputMux is a mux with one focused, non-control pane whose "child" is a
// pipe, so a test can watch what does and does not reach it.
func scrollInputMux(t *testing.T) (*Magmux, *Pane, *os.File) {
	t.Helper()
	m := &Magmux{rows: 40, cols: 120, quit: make(chan struct{}), control: newControlPanel()}
	p := newScrollPane(20, 60)
	childR, childW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { childR.Close(); childW.Close() })
	p.ptmx = childW
	m.root = p
	m.focused = p
	m.allPanes = []*Pane{p}
	m.stampPaneIDs()
	return m, p, childR
}

func scrollOff(p *Pane) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.screen.sbOff
}

// TestScrollModeIsEnteredMovedAndLeft drives the REAL inputLoop, because every
// other test here calls the helpers directly and a chord that was never wired
// up would let all of them pass.
//
// The mechanism is deliberately not a bare key: arrows, PageUp/PageDown and the
// wheel have to keep reaching a full-screen TUI, so scrolling is a mode entered
// from the Ctrl-G prefix (tmux's copy-mode binding) and left by q/Enter/Esc.
func TestScrollModeIsEnteredMovedAndLeft(t *testing.T) {
	defer deadlineWatchdog(t, 20*time.Second)()

	m, p, child := scrollInputMux(t)
	feedLines(p, "L", 200) // plenty of history to move through
	drain(t, child)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pw.Close()
	m.stdin = pr

	done := make(chan struct{})
	go func() { m.inputLoop(); close(done) }()
	defer func() {
		m.quitOnce.Do(func() { close(m.quit) })
		<-done
	}()

	type keyCase struct {
		send []byte
		want func(int) bool
		what string
	}
	page := scrollPage(p.h)
	cases := []keyCase{
		{[]byte{0x07, '['}, func(o int) bool { return o == page }, "ctrl-g [ scrolls back a page"},
		{[]byte("k"), func(o int) bool { return o == page+1 }, "k moves one line further back"},
		{[]byte("j"), func(o int) bool { return o == page }, "j moves one line toward live"},
		{[]byte("\x1b[A"), func(o int) bool { return o == page+1 }, "the up arrow moves back"},
		{[]byte("\x1b[5~"), func(o int) bool { return o == 2*page+1 }, "PgUp moves a page back"},
		{[]byte("\x1b[6~"), func(o int) bool { return o == page+1 }, "PgDn moves a page forward"},
		{[]byte("g"), func(o int) bool { return o > 2*page }, "g goes to the oldest line"},
		{[]byte("q"), func(o int) bool { return o == 0 }, "q returns to live output"},
	}
	for _, c := range cases {
		if _, err := pw.Write(c.send); err != nil {
			t.Fatalf("write %q: %v", c.send, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		ok := false
		for time.Now().Before(deadline) {
			if c.want(scrollOff(p)) {
				ok = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !ok {
			t.Fatalf("%s: offset stayed at %d", c.what, scrollOff(p))
		}
	}

	// Nothing that moved the viewport was typed at the child. A scroll key that
	// leaked would land in whatever prompt the pane is showing.
	if leaked := drain(t, child); leaked != "" {
		t.Errorf("scroll-mode keys reached the child: %q", leaked)
	}
}

// TestKeysReachTheChildWhenNotInScrollMode is the other half of the contract,
// and the one that protects every existing user: outside scroll mode the keys
// scroll mode claims — j, k, q, the arrows, PageUp — go to the child exactly as
// they always did. A full-screen TUI needs every one of them.
func TestKeysReachTheChildWhenNotInScrollMode(t *testing.T) {
	defer deadlineWatchdog(t, 20*time.Second)()

	m, p, child := scrollInputMux(t)
	feedLines(p, "L", 200)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pw.Close()
	m.stdin = pr

	done := make(chan struct{})
	go func() { m.inputLoop(); close(done) }()
	defer func() {
		m.quitOnce.Do(func() { close(m.quit) })
		<-done
	}()

	if _, err := pw.Write([]byte("kjq\x1b[A\x1b[5~")); err != nil {
		t.Fatalf("write: %v", err)
	}
	want := "kjq\x1b[A\x1b[5~"
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) && len(got) < len(want) {
		got += drain(t, child)
		time.Sleep(2 * time.Millisecond)
	}
	if got != want {
		t.Errorf("the child received %q, want %q — scroll mode is stealing keys it was not given", got, want)
	}
	if off := scrollOff(p); off != 0 {
		t.Errorf("keys typed outside scroll mode moved the viewport to %d", off)
	}
}

// drain reads whatever is waiting on the child end of the pipe without blocking.
func drain(t *testing.T, f *os.File) string {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	return string(buf[:n])
}

// TestWheelScrollsAPrimaryPaneAndNotAnAltScreenOne. The wheel is the natural
// gesture and it is safe on a primary screen — but it is already forwarded to
// alt-screen apps, which is how a TUI's own scrolling works, and taking it back
// would break every one of them.
func TestWheelScrollsAPrimaryPaneAndNotAnAltScreenOne(t *testing.T) {
	m, p, child := scrollInputMux(t)
	feedLines(p, "L", 200)
	drain(t, child)

	wheelUp := []byte("\x1b[<64;5;5M")
	if n, ok := m.parseSGRMouse(wheelUp); !ok || n != len(wheelUp) {
		t.Fatalf("the wheel report was not parsed: n=%d ok=%v", n, ok)
	}
	if off := scrollOff(p); off != 3 {
		t.Errorf("the wheel scrolled a primary pane to offset %d, want 3", off)
	}
	if leaked := drain(t, child); leaked != "" {
		t.Errorf("a wheel event over a primary pane was forwarded to the child: %q", leaked)
	}

	// Back to live, then onto the alternate screen.
	m.scrollFocusedTo(0)
	feed(p, "\x1b[?1049h")
	if !p.altMode {
		t.Fatalf("the pane did not enter the alternate screen")
	}
	if n, ok := m.parseSGRMouse(wheelUp); !ok || n != len(wheelUp) {
		t.Fatalf("the wheel report was not parsed on the alt pane: n=%d ok=%v", n, ok)
	}
	p.mu.Lock()
	altOff, primaryOff := p.screen.sbOff, p.primaryScreen.sbOff
	p.mu.Unlock()
	if altOff != 0 || primaryOff != 0 {
		t.Errorf("the wheel scrolled an alt-screen pane (alt=%d primary=%d), want neither", altOff, primaryOff)
	}
	deadline := time.Now().Add(2 * time.Second)
	var fwd string
	for time.Now().Before(deadline) && fwd == "" {
		fwd = drain(t, child)
	}
	if !strings.Contains(fwd, "\x1b[<64;") {
		t.Errorf("the wheel was not forwarded to the alt-screen app: %q", fwd)
	}
}

// TestScrollModeRefusesAPaneWithNoHistoryAndSaysWhy. A keystroke that appears
// to do nothing is indistinguishable from a broken one, and on an alt-screen
// pane — which is every coding agent — this key can never do anything. The
// refusal has to name the reason.
func TestScrollModeRefusesAPaneWithNoHistoryAndSaysWhy(t *testing.T) {
	m, p, _ := scrollInputMux(t)

	if m.scrollFocusedBy(5) {
		t.Errorf("a pane with no history reported a successful scroll")
	}
	m.treeMu.RLock()
	note := m.chromeNoteLocked()
	m.treeMu.RUnlock()
	if note == "" {
		t.Errorf("a refused scroll said nothing in the status bar")
	}

	feed(p, "\x1b[?1049h")
	if m.scrollFocusedBy(5) {
		t.Errorf("an alt-screen pane reported a successful scroll")
	}
	m.treeMu.RLock()
	note = m.chromeNoteLocked()
	m.treeMu.RUnlock()
	if !strings.Contains(note, "alternate screen") {
		t.Errorf("the refusal on an alt-screen pane does not name the reason: %q", note)
	}
}

// TestScrollHintAppearsOnlyWhenThereIsHistory. The Ctrl-G row is where this
// feature is discovered, and it must not advertise a key that does nothing on
// the pane being looked at — which on an alt-screen pane it always would.
func TestScrollHintAppearsOnlyWhenThereIsHistory(t *testing.T) {
	m, p, _ := scrollInputMux(t)

	m.treeMu.RLock()
	before := keyHintList(m.keyHintLocked(), 0)
	m.treeMu.RUnlock()
	if strings.Contains(before, "[") {
		t.Errorf("the chord offers scrolling on a pane with no history: %q", before)
	}

	feedLines(p, "L", 200)
	m.treeMu.RLock()
	after := keyHintList(m.keyHintLocked(), 0)
	m.treeMu.RUnlock()
	if !strings.Contains(after, "[") {
		t.Errorf("the chord does not offer scrolling on a pane with history: %q", after)
	}
}

// TestScrollBadgeSaysWhereYouAreAndHowToLeave. The badge is the only thing on
// screen that says a pane is not showing live output, and it carries the way
// out for the same reason a modal dialog carries a Cancel.
func TestScrollBadgeSaysWhereYouAreAndHowToLeave(t *testing.T) {
	m, p, _ := scrollInputMux(t)
	feedLines(p, "L", 200)
	m.scrollFocusedTo(12)

	m.treeMu.RLock()
	m.markAllDirtyLocked()
	_, frame, _ := m.renderLocked()
	m.treeMu.RUnlock()

	if !strings.Contains(frame, "SCROLL 12/") {
		t.Errorf("a scrolled pane does not say how far back it is:\n%q", frame)
	}
	if !strings.Contains(frame, "q live") {
		t.Errorf("a scrolled pane does not say how to get back to live output")
	}

	m.scrollFocusedTo(0)
	m.treeMu.RLock()
	m.markAllDirtyLocked()
	_, frame, _ = m.renderLocked()
	m.treeMu.RUnlock()
	if strings.Contains(frame, "SCROLL") {
		t.Errorf("the badge outlived scroll mode:\n%q", frame)
	}
}

// TestScrolledPaneStaysParkedWhileOutputArrives. Output does not drag the
// viewport out from under a reader: the offset follows the content, exactly as
// tmux's copy-mode does, until the line being read is the one falling out of the
// ring.
func TestScrolledPaneStaysParkedWhileOutputArrives(t *testing.T) {
	p := newScrollPane(10, 40)
	feedLines(p, "L", 30)

	p.mu.Lock()
	p.screen.sbOff = 5
	p.mu.Unlock()
	parked := p.captureAt(5, 0, false).Text

	feedLines(p, "MORE", 4)

	p.mu.Lock()
	off := p.screen.sbOff
	p.mu.Unlock()
	if off != 9 {
		t.Errorf("four new lines moved the viewport to offset %d, want 9 (parked on the same rows)", off)
	}
	if got := p.captureAt(off, 0, false).Text; got != parked {
		t.Errorf("the parked view drifted while output arrived:\n got %q\nwant %q", got, parked)
	}
}
