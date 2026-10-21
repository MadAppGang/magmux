package verdict

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/adapters"
)

// TestAWaitingWordThisBuildDoesNotKnowStaysUnknown is fix round 2, item 2 —
// which is a NON-fix, and this test exists so that it is not "fixed" by
// accident.
//
// On 2026-09-19 Claude Code 2.1.276 was found writing a third waitingFor
// value, "dialog open" (~/.claude/sessions/26188.json). The requirements named
// exactly two, "input needed" and "permission prompt". The service handled it
// the way the lenient-adapter rule says it must, and this pins that behaviour
// through the decoder AND the verdict, with the value that was measured:
//
//   - it is not guessed at: waitingOn stays null. Mapping it to "input" would
//     have been right for the 55 samples the original rule was written from,
//     and would label this session with total confidence and no evidence;
//   - the raw word survives, so a consumer can still act on it;
//   - attention still fires — the session IS waiting, whatever for — and the
//     reason says the cause is unrecognised rather than inventing one.
//
// Recognising "dialog open" means a new obs.WaitingReason, which is a change to
// the published wire vocabulary (architecture-v4 §9) and the user's decision.
// When it is made, this test is the thing that has to change with it, and it
// should: that is the point of it.
func TestAWaitingWordThisBuildDoesNotKnowStaysUnknown(t *testing.T) {
	const measured = "dialog open"
	rec := `{"sessionId":"s","pid":26188,"status":"waiting","waitingFor":"` + measured + `",` +
		`"statusUpdatedAt":1789545192100}`
	s, notes, err := adapters.ParseRecord([]byte(rec))
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if s.Waiting != obs.WaitingReasonUnknown {
		t.Fatalf("%q decoded as %s; recognising it is a vocabulary change nobody has decided, "+
			"and a guess is indistinguishable from an observation", measured, s.Waiting)
	}
	if s.WaitingRaw != measured {
		t.Errorf("waitingOnRaw is %q, want the word the tool wrote, %q", s.WaitingRaw, measured)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], measured) {
		t.Errorf("notes are %q, want one naming the unrecognised word — a format change must be "+
			"discoverable", notes)
	}

	got := Resolve(s, Screen{}, NOW)
	if got.Attention.Needed == nil || !*got.Attention.Needed {
		t.Errorf("attention is %s; the session IS waiting, and not knowing what for is no reason "+
			"to stay quiet about it", needed(got.Attention))
	}
	if !strings.Contains(got.Attention.Reason, `"`+measured+`"`) ||
		!strings.Contains(got.Attention.Reason, "does not recognise") {
		t.Errorf("the reason %q does not name the unrecognised word as unrecognised", got.Attention.Reason)
	}
	// Logged beside the live one, which read: it is waiting, for something
	// this build does not recognise ("dialog open"), for 7.9s. The record
	// above is staged 7.9s before NOW so the two can be compared by eye.
	t.Logf("reason: %s (the session entered waiting %s before NOW)", got.Attention.Reason,
		7900*time.Millisecond)
}
