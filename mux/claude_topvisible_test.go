package mux

// r3 review M3: topVisible decides whether NOTHING older than the visible
// user-prompt entries exists, which is what lets acceptance call every entry
// on a later screen new. A false "true" makes an older identical entry count
// as this send's evidence — a false ok — so it is validated here against
// every real Claude Code 2.1.301 capture mark, not against a synthetic layout.

import (
	"strings"
	"testing"
	"time"
)

var claudeCaptureScenarios = []string{
	"s1-box-history", "s2-permission", "s3-paste", "s4-midturn",
	"s5-insert-plan", "s6-markdown", "s7-bashmode",
}

// TestTopVisibleOnRealCaptures: on a real screen the top of the conversation
// is on screen exactly when Claude's welcome banner is (Claude draws it as the
// first rows of every conversation). topVisible must NEVER say true when the
// banner is off screen (a false ok), on any mark; and on every mark with an
// input box — the screens a verify reads — it must agree. (A dialog screen
// has no box, so no verify ever reads one; there a false "no" is safe.)
func TestTopVisibleOnRealCaptures(t *testing.T) {
	n := 0
	for _, sc := range claudeCaptureScenarios {
		for _, m := range loadClaudeMarks(t, sc) {
			n++
			s := claudeScreenAt(t, sc, m.Label)
			banner := strings.Contains(m.Text, "Claude Code v")
			got := topVisible(s)
			if got && !banner {
				t.Errorf("%s/%s: topVisible = true with the banner off screen (a false ok)", sc, m.Label)
			}
			if s.BoxVisible && got != banner {
				t.Errorf("%s/%s: topVisible = %v, want %v (the banner is on screen: %v)", sc, m.Label, got, banner, banner)
			}
		}
	}
	if n < 31 {
		t.Errorf("validated %d marks, want every one of the 31+ real captures", n)
	}
}

// TestTopVisibleIgnoresBlankPadding: blank rows above the box, or under an
// inline conversation, are not proof that the top is on screen — no real
// 2.1.301 screen has them, and trusting them let a shrinking box reveal an
// older identical entry that then counted as new (review r3 M3).
func TestTopVisibleIgnoresBlankPadding(t *testing.T) {
	conv := cat(entryRows("continue"), []string{"⏺ ack", ""})
	padded := parseClaudeScreen(shortFrame(conv, []string{"❯ "}), frontierCols)
	if topVisible(padded) {
		t.Errorf("a padded conversation without the banner was taken as the top")
	}
	withBanner := parseClaudeScreen(shortFrame(cat(claudeBanner, conv), []string{"❯ "}), frontierCols)
	if !topVisible(withBanner) {
		t.Errorf("a conversation under the banner was not taken as the top")
	}
}

// TestShrinkingBoxRevealingAnOlderEntryIsNoEvidence is the reviewer's
// counterexample: base [A], a lost send of A, then the box shrinks and
// reveals an OLDER A above, giving [A_old, A]. With padding no longer read
// as "the top is on screen", the old A is never this send's evidence.
func TestShrinkingBoxRevealingAnOlderEntryIsNoEvidence(t *testing.T) {
	const a = "continue"
	tail := []string{"⏺ ack", "", "", "", ""}
	before := parseClaudeScreen(shortFrame(cat(entryRows(a), tail), []string{"❯ " + a}), frontierCols)
	b := newAcceptBaseline(before, a, a, 0, time.Now(), 0)
	after := parseClaudeScreen(shortFrame(cat(entryRows(a), []string{"⏺ ack", ""}, entryRows(a), tail), []string{"❯ "}), frontierCols)
	if got := acceptVerdict(after, b, nil); got.State == AcceptTaken {
		t.Errorf("an older entry revealed by a shrinking box was taken as evidence: %+v", got)
	}
}
