package mux

// A resize resets the scroll margins to the full screen. The bug this pins:
// Screen.resize clamped scrollBot DOWN on a shrink and never raised it on a
// grow, so after any grow — a client's resize, a bigger window, the panel
// hiding — CUD and CNL stopped at the OLD bottom row. Claude Code redraws with
// relative moves after SIGWINCH, so its input box, footer and status line all
// collapsed onto one row and its turns never settled. A child that positions
// rows absolutely (CUP) never noticed, which is why the resize e2e passed.

import "testing"

func curY(p *Pane) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.screen.curY
}

// cudToBottom homes the cursor and moves down far past any screen, the way a
// relative redraw reaches its last row.
func cudToBottom(p *Pane) int {
	feed(p, "\x1b[1;1H\x1b[999B")
	return curY(p)
}

func TestCUDReachesTheNewBottomAfterAGrow(t *testing.T) {
	p := newScrollPane(20, 80)
	if got := cudToBottom(p); got != 19 {
		t.Fatalf("before any resize CUD stopped on row %d, want 19", got)
	}
	p.resize(0, 0, 30, 80)
	if got := cudToBottom(p); got != 29 {
		t.Fatalf("after 20->30 rows CUD stopped on row %d, want 29 (the old bottom is 19)", got)
	}
	// CNL clamps the same way.
	feed(p, "\x1b[1;1H\x1b[999E")
	if got := curY(p); got != 29 {
		t.Fatalf("after 20->30 rows CNL stopped on row %d, want 29", got)
	}
}

// The sequence claudish reported: grow, shrink, grow again.
func TestCUDFollowsGrowShrinkGrow(t *testing.T) {
	p := newScrollPane(22, 80)
	for _, c := range []struct{ rows, want int }{{30, 29}, {20, 19}, {28, 27}} {
		p.resize(0, 0, c.rows, 80)
		if got := cudToBottom(p); got != c.want {
			t.Fatalf("after resize to %d rows CUD stopped on row %d, want %d", c.rows, got, c.want)
		}
	}
}

func TestCUDReachesTheNewBottomOnTheAltScreen(t *testing.T) {
	p := newScrollPane(20, 80)
	feed(p, "\x1b[?1049h")
	if !p.altMode {
		t.Fatalf("DEC 1049 did not put the pane on the alternate screen")
	}
	p.resize(0, 0, 30, 80)
	if got := cudToBottom(p); got != 29 {
		t.Fatalf("alt screen: after 20->30 rows CUD stopped on row %d, want 29", got)
	}
	// And the primary the shell returns to was reset too.
	feed(p, "\x1b[?1049l")
	if got := cudToBottom(p); got != 29 {
		t.Fatalf("primary after leaving alt: CUD stopped on row %d, want 29", got)
	}
}

// A region the child set does not survive a resize, in either direction —
// xterm's behaviour. Keeping it on a grow is the bug; keeping it on a shrink
// would apply margins chosen for a screen that no longer exists.
func TestResizeResetsAChildsMargins(t *testing.T) {
	for _, rows := range []int{30, 15} {
		p := newScrollPane(20, 80)
		feed(p, "\x1b[5;10r")
		p.mu.Lock()
		top, bot := p.screen.scrollTop, p.screen.scrollBot
		p.mu.Unlock()
		if top != 4 || bot != 10 {
			t.Fatalf("DECSTBM 5;10 gave margins [%d,%d), want [4,10)", top, bot)
		}
		p.resize(0, 0, rows, 80)
		p.mu.Lock()
		top, bot = p.screen.scrollTop, p.screen.scrollBot
		p.mu.Unlock()
		if top != 0 || bot != rows {
			t.Fatalf("after resize to %d rows margins are [%d,%d), want [0,%d)", rows, top, bot, rows)
		}
	}
}
