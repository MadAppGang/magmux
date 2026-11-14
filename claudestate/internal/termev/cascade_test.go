package termev

import (
	"testing"
	"time"
)

// base is the instant every cascade test measures from. It is deliberately not
// time.Now(): nothing here reads a real clock, so the tests are the same test
// on every machine at every hour.
var base = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return base.Add(d) }

// TestDefaultLadderIsTheSettledSevenRungs pins the ladder itself.
//
// A WRONG IMPLEMENTATION that quietly changed a rung — 15s instead of 10s, or
// dropped the 30m rung because six felt like enough — would pass every other
// test in this file, because every other test builds its own short ladder. The
// shipped ladder is the one thing no other test looks at.
func TestDefaultLadderIsTheSettledSevenRungs(t *testing.T) {
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 30 * time.Second,
		time.Minute, 5 * time.Minute, 30 * time.Minute, time.Hour,
	}
	got := DefaultLadder()
	if len(got) != len(want) {
		t.Fatalf("ladder has %d rungs, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rung %d is %s, want %s", i, got[i], want[i])
		}
	}
}

// TestDefaultLadderCannotBeMutatedByACaller proves the ladder is handed out as
// a fresh copy.
//
// A WRONG IMPLEMENTATION exporting `var DefaultLadder = []time.Duration{...}`
// would let one caller's in-place edit change the ladder for every pane in the
// process — a bug that would appear in a completely unrelated component and
// have no path back to the line that caused it.
func TestDefaultLadderCannotBeMutatedByACaller(t *testing.T) {
	first := DefaultLadder()
	first[0] = 999 * time.Hour
	if DefaultLadder()[0] != 5*time.Second {
		t.Fatalf("mutating a returned ladder changed the next one: %s", DefaultLadder()[0])
	}
}

// TestNewCascadeRejectsAMalformedLadder covers the three shapes that would
// make "the next rung" ambiguous or nonsensical.
//
// A WRONG IMPLEMENTATION that accepted them would not fail loudly. An empty
// ladder produces a cascade that never says anything; a repeated rung fires
// twice at the same instant and looks like the at-most-once rule being broken;
// a non-positive rung comes due before the output that armed it, so a pane
// reads as stale the moment it prints.
func TestNewCascadeRejectsAMalformedLadder(t *testing.T) {
	cases := []struct {
		name   string
		ladder []time.Duration
	}{
		{"empty", nil},
		{"zero rung", []time.Duration{0}},
		{"negative rung", []time.Duration{-time.Second}},
		{"repeated rung", []time.Duration{time.Second, time.Second}},
		{"decreasing rung", []time.Duration{2 * time.Second, time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCascade(tc.ladder...); err == nil {
				t.Fatalf("NewCascade(%v) accepted a malformed ladder", tc.ladder)
			}
		})
	}
}

// TestANewCascadeIsDisarmed proves a cascade says nothing before it is told
// about any activity.
//
// A WRONG IMPLEMENTATION whose zero `since` counted as "armed at the epoch"
// would report every rung as due on the first look, against a quiet period
// that began in 1970.
func TestANewCascadeIsDisarmed(t *testing.T) {
	c := mustCascade(t, time.Second, 2*time.Second)
	if c.Armed() {
		t.Fatal("a fresh cascade is armed")
	}
	if _, ok := c.Deadline(); ok {
		t.Fatal("a fresh cascade has a deadline")
	}
	if _, ok := c.Advance(at(time.Hour)); ok {
		t.Fatal("a fresh cascade fired a rung")
	}
}

// TestEachThresholdFiresAtMostOncePerQuietPeriod is the core rule.
//
// A WRONG IMPLEMENTATION that compared `now - since >= threshold` without
// advancing its position would re-report the 5s rung on every look for the
// rest of the silence: seven rungs would become an unbounded stream, and a
// consumer counting "this pane has gone stale" would count thousands.
func TestEachThresholdFiresAtMostOncePerQuietPeriod(t *testing.T) {
	ladder := DefaultLadder()
	c := mustCascade(t, ladder...)
	c.Activity(base)

	seen := map[time.Duration]int{}
	// Look many times, at instants well past every rung. The repetition is the
	// point: a correct cascade is silent on all but the first look past each
	// rung, however often it is asked.
	for _, look := range []time.Duration{
		time.Second, 5 * time.Second, 6 * time.Second, 9 * time.Second,
		10 * time.Second, 11 * time.Second, 30 * time.Second, 45 * time.Second,
		time.Minute, 2 * time.Minute, 5 * time.Minute, 20 * time.Minute,
		30 * time.Minute, time.Hour, 2 * time.Hour, 10 * time.Hour,
	} {
		for {
			ev, ok := c.Advance(at(look))
			if !ok {
				break
			}
			seen[ev.Threshold]++
		}
	}

	if len(seen) != len(ladder) {
		t.Fatalf("saw %d distinct thresholds, want %d: %v", len(seen), len(ladder), seen)
	}
	for _, d := range ladder {
		if seen[d] != 1 {
			t.Errorf("threshold %s fired %d times, want exactly 1", d, seen[d])
		}
	}
	if c.Armed() {
		t.Error("an exhausted cascade still reports itself armed")
	}
}

// TestALateLookCrossesEveryDueRungOnceEach covers the case a real timer makes
// unavoidable: the process was descheduled, or the machine slept, and several
// rungs came due between two looks.
//
// A WRONG IMPLEMENTATION that reported only the LAST rung crossed would lose
// the intermediate ones, so a consumer watching for "quiet 10s" would never
// see it on a loaded machine. One that reported them with different At values
// would be inventing observations it never made.
func TestALateLookCrossesEveryDueRungOnceEach(t *testing.T) {
	c := mustCascade(t, 5*time.Second, 10*time.Second, 30*time.Second, time.Minute)
	c.Activity(base)

	look := at(35 * time.Second)
	var got []Event
	for {
		ev, ok := c.Advance(look)
		if !ok {
			break
		}
		got = append(got, ev)
	}

	want := []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("one late look produced %d events, want %d: %v", len(got), len(want), got)
	}
	for i, ev := range got {
		if ev.Threshold != want[i] {
			t.Errorf("event %d crossed %s, want %s", i, ev.Threshold, want[i])
		}
		if ev.Index != i {
			t.Errorf("event %d has index %d", i, ev.Index)
		}
		if !ev.At.Equal(look) {
			t.Errorf("event %d observed at %s, want the single look instant %s", i, ev.At, look)
		}
		if !ev.Since.Equal(base) {
			t.Errorf("event %d is since %s, want %s", i, ev.Since, base)
		}
		if ev.Quiet() != 35*time.Second {
			t.Errorf("event %d reports quiet %s, want 35s", i, ev.Quiet())
		}
	}
	// The 1m rung is not yet due and must still be pending.
	if !c.Armed() {
		t.Fatal("the cascade disarmed itself with a rung still to come")
	}
	if dl, _ := c.Deadline(); !dl.Equal(at(time.Minute)) {
		t.Errorf("next deadline is %s, want %s", dl, at(time.Minute))
	}
}

// TestActivityReArmsFromTheNewInstant proves output restarts the silence.
//
// A WRONG IMPLEMENTATION that re-armed but left its ladder position alone
// would never fire the early rungs again, so a pane that produced one line
// every four seconds forever would report "quiet 1h" — the opposite of the
// truth. One that re-armed from the OLD instant would report a silence that
// included output it had been told about.
func TestActivityReArmsFromTheNewInstant(t *testing.T) {
	c := mustCascade(t, 5*time.Second, 10*time.Second)
	c.Activity(base)

	ev, ok := c.Advance(at(5 * time.Second))
	if !ok || ev.Threshold != 5*time.Second {
		t.Fatalf("first rung did not fire: %v %v", ev, ok)
	}

	c.Activity(at(6 * time.Second))
	if !c.Since().Equal(at(6 * time.Second)) {
		t.Fatalf("Since is %s, want %s", c.Since(), at(6*time.Second))
	}
	if _, ok := c.Advance(at(10 * time.Second)); ok {
		t.Fatal("a rung fired 4s into a fresh quiet period")
	}
	ev, ok = c.Advance(at(11 * time.Second))
	if !ok {
		t.Fatal("the 5s rung did not fire again in the new quiet period")
	}
	if ev.Threshold != 5*time.Second {
		t.Errorf("crossed %s, want the 5s rung again", ev.Threshold)
	}
	if !ev.Since.Equal(at(6 * time.Second)) {
		t.Errorf("Since is %s, want the new activity instant", ev.Since)
	}
}

// TestActivityRefusesTheZeroAndOutOfOrderInstants pins the two instants that
// are reports about a quiet period already accounted for.
//
// A WRONG IMPLEMENTATION accepting the zero time would arm from the epoch and
// fire the whole ladder at once for a pane that had simply never printed —
// exactly the pane for which a staleness report matters most, reported as
// nonsense. One accepting an earlier instant would restart a quiet period that
// had already fired rungs, and the at-most-once rule would silently stop
// holding.
func TestActivityRefusesTheZeroAndOutOfOrderInstants(t *testing.T) {
	c := mustCascade(t, 5*time.Second)

	if c.Activity(time.Time{}) {
		t.Fatal("the zero instant was accepted")
	}
	if c.Armed() {
		t.Fatal("the zero instant armed the cascade")
	}

	if !c.Activity(at(10 * time.Second)) {
		t.Fatal("a first real instant was refused")
	}
	if c.Activity(at(9 * time.Second)) {
		t.Fatal("an earlier instant was accepted")
	}
	if c.Activity(at(10 * time.Second)) {
		t.Fatal("a repeated instant was accepted")
	}
	if !c.Since().Equal(at(10 * time.Second)) {
		t.Fatalf("Since moved to %s", c.Since())
	}
	if !c.Activity(at(11 * time.Second)) {
		t.Fatal("a later instant was refused")
	}
}

// TestInputCancelsTheCascade proves typing into a pane silences the ladder.
//
// A WRONG IMPLEMENTATION that treated input as activity would report a pane as
// stale when what it is really doing is waiting for the program to answer
// something a human just typed — a notification every five seconds at a prompt
// somebody is sitting at. One that ignored input entirely would keep the
// silence running from before the keystroke, so the rungs would fire against a
// quiet period the user had already ended.
func TestInputCancelsTheCascade(t *testing.T) {
	c := mustCascade(t, 5*time.Second, 10*time.Second)
	c.Activity(base)
	if !c.Armed() {
		t.Fatal("output did not arm the cascade")
	}

	c.Input(at(time.Second))

	if c.Armed() {
		t.Fatal("the cascade is still armed after input")
	}
	if !c.Since().IsZero() {
		t.Fatalf("Since is %s after input, want the zero instant", c.Since())
	}
	if _, ok := c.Deadline(); ok {
		t.Fatal("a cancelled cascade has a deadline")
	}
	// Look far past every rung. A cancelled cascade says nothing, ever.
	for _, look := range []time.Duration{time.Second, 5 * time.Second, time.Hour, 24 * time.Hour} {
		if ev, ok := c.Advance(at(look)); ok {
			t.Fatalf("a cancelled cascade fired %v at +%s", ev, look)
		}
	}
	if !c.LastInput().Equal(at(time.Second)) {
		t.Errorf("LastInput is %s, want %s", c.LastInput(), at(time.Second))
	}
}

// TestOutputAfterInputRevivesTheCascade is the other half of cancellation: the
// silence that matters starts when the program next speaks.
//
// A WRONG IMPLEMENTATION that made cancellation permanent — a latch rather
// than a state — would go quiet forever after the first keystroke, and every
// pane a human had ever touched would stop being observed. That failure is
// invisible in normal use, because the pane looks fine; it just never reports.
func TestOutputAfterInputRevivesTheCascade(t *testing.T) {
	c := mustCascade(t, 5*time.Second)
	c.Activity(base)
	c.Input(at(time.Second))

	if !c.Activity(at(2 * time.Second)) {
		t.Fatal("output after input was refused")
	}
	if !c.Armed() {
		t.Fatal("output after input did not re-arm the cascade")
	}
	ev, ok := c.Advance(at(7 * time.Second))
	if !ok {
		t.Fatal("the revived cascade did not fire")
	}
	if !ev.Since.Equal(at(2 * time.Second)) {
		t.Errorf("the revived quiet period is since %s, want %s", ev.Since, at(2*time.Second))
	}
}

// TestDeadlineIsSinceePlusTheNextRung pins the arithmetic the driver arms its
// timer from.
//
// A WRONG IMPLEMENTATION computing the deadline from the previous rung, or
// from the time of the last look rather than from Since, would drift: each
// rung would be armed relative to when the last one happened to be noticed, so
// a loaded machine would push the whole ladder later and later.
func TestDeadlineIsSinceePlusTheNextRung(t *testing.T) {
	c := mustCascade(t, 5*time.Second, 10*time.Second, 30*time.Second)
	c.Activity(base)

	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second} {
		dl, ok := c.Deadline()
		if !ok {
			t.Fatalf("no deadline with the %s rung still to come", want)
		}
		if !dl.Equal(at(want)) {
			t.Fatalf("deadline is %s, want %s", dl, at(want))
		}
		// Look LATE on purpose: the next deadline must still be measured from
		// Since, not from this look.
		if _, ok := c.Advance(at(want + 3*time.Second)); !ok {
			t.Fatalf("the %s rung did not fire", want)
		}
	}
	if _, ok := c.Deadline(); ok {
		t.Fatal("an exhausted cascade still offers a deadline")
	}
}

// TestLadderIsCopiedIntoTheCascade proves a caller cannot reach in afterwards.
//
// A WRONG IMPLEMENTATION retaining the caller's slice would let the ladder
// change underneath a running cascade, so "the 5s rung" could become the 5h
// rung between two looks and the at-most-once bookkeeping would be indexing a
// table that had moved.
func TestLadderIsCopiedIntoTheCascade(t *testing.T) {
	ladder := []time.Duration{5 * time.Second, 10 * time.Second}
	c := mustCascade(t, ladder...)
	ladder[0] = time.Hour

	c.Activity(base)
	ev, ok := c.Advance(at(5 * time.Second))
	if !ok {
		t.Fatal("the 5s rung did not fire after the caller edited its slice")
	}
	if ev.Threshold != 5*time.Second {
		t.Errorf("crossed %s, want 5s", ev.Threshold)
	}
	if got := c.Ladder(); got[0] != 5*time.Second {
		t.Errorf("Ladder reports %s, want 5s", got[0])
	}
	out := c.Ladder()
	out[1] = time.Hour
	if c.Ladder()[1] != 10*time.Second {
		t.Error("Ladder hands out the cascade's own slice")
	}
}

func mustCascade(t *testing.T, ladder ...time.Duration) *Cascade {
	t.Helper()
	c, err := NewCascade(ladder...)
	if err != nil {
		t.Fatalf("NewCascade(%v): %v", ladder, err)
	}
	return c
}
