package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
)

// NOW is the reference instant every time-derived expectation in this file is
// built from, per testdata/PROVENANCE.md rule 4. Nothing here reads the clock.
var NOW = time.Date(2026, 9, 18, 14, 0, 0, 0, time.Local)

// waitingSince builds a session that entered `waiting` at an instant.
func waitingSince(t time.Time) obs.Session {
	return obs.Session{
		ID:          "id-a",
		PID:         4242,
		Name:        "session-a",
		Status:      obs.StatusWaiting,
		Waiting:     obs.WaitingReasonInput,
		StatusSince: &t,
	}
}

// loud is the posture the R6 sketch writes: both triggers, a two-minute delay,
// and one quiet window that crosses midnight.
func loud() act.Policy {
	p := act.Default()
	p.Source = "test policy"
	p.Notify = act.Notify{
		Enabled: true,
		On:      []act.NotifyOn{act.NotifyOnWaiting, act.NotifyOnError},
		Delay:   2 * time.Minute,
		Quiet:   []act.QuietWindow{{From: act.NewDayMinute(22, 0), To: act.NewDayMinute(8, 0)}},
	}
	return p
}

// at builds a clock stuck at one instant, and a handle to move it.
func at(start time.Time) (func() time.Time, func(time.Duration)) {
	now := start
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

// TestTheZeroPolicySaysNothing is the safe-by-default check, and it is the one
// that matters: every unit test in this repository builds its structs as
// literals, so a stage that notified on an unconfigured policy would be
// notifying in every test that never mentions notification.
func TestTheZeroPolicySaysNothing(t *testing.T) {
	s := New(Config{Now: func() time.Time { return NOW }})
	s.Observe(waitingSince(NOW.Add(-time.Hour)))
	if notes := s.Pending(); len(notes) != 0 {
		t.Fatalf("the zero policy produced %d notifications: %+v", len(notes), notes)
	}
	if d, ok := s.WakeIn(); ok {
		t.Errorf("the zero policy armed an alarm for %v; a silent notifier waits for nothing", d)
	}
}

// TestNotifyEnabledFalseSilencesEverything: the master switch beats every other
// key in the block.
func TestNotifyEnabledFalseSilencesEverything(t *testing.T) {
	p := loud()
	p.Notify.Enabled = false
	p.Notify.Delay = 0
	s := New(Config{Policy: p, Now: func() time.Time { return NOW }})
	s.Observe(waitingSince(NOW.Add(-time.Hour)))
	if notes := s.Pending(); len(notes) != 0 {
		t.Fatalf("enabled=false produced %d notifications", len(notes))
	}
}

// TestASessionThatStaysWaitingNotifiesExactlyOnce is R4.4's "not a repeat",
// and it is one of the four deliberate breaks the phase brief names.
//
// Thirty observations of one unchanged waiting session — which is what a busy
// registry produces, because every OTHER session's record changing cycles this
// one too — must produce exactly one notification.
func TestASessionThatStaysWaitingNotifiesExactlyOnce(t *testing.T) {
	now, advance := at(NOW)
	s := New(Config{Policy: loud(), Now: now})
	since := NOW.Add(-10 * time.Minute)

	total := 0
	for i := 0; i < 30; i++ {
		s.Observe(waitingSince(since))
		total += len(s.Pending())
		advance(time.Minute)
	}
	if total != 1 {
		t.Fatalf("a session that sat waiting for half an hour produced %d notifications, want 1", total)
	}
}

// TestANewEpisodeNotifiesAgain is the other half, and the reason the latch is
// keyed on the episode rather than on the session.
//
// A session that goes waiting, is answered, works, and goes waiting again is
// two errands for the human and must be two notifications. A latch keyed on
// "have I ever notified about this session" would announce the first and go
// quiet for the rest of the day.
func TestANewEpisodeNotifiesAgain(t *testing.T) {
	now, advance := at(NOW)
	s := New(Config{Policy: loud(), Now: now})

	first := NOW.Add(-10 * time.Minute)
	s.Observe(waitingSince(first))
	if got := len(s.Pending()); got != 1 {
		t.Fatalf("the first episode produced %d notifications, want 1", got)
	}

	// Answered: the session goes back to work.
	advance(time.Minute)
	busy := obs.Session{ID: "id-a", PID: 4242, Status: obs.StatusBusy, StatusSince: obs.Ptr(now())}
	s.Observe(busy)
	if got := len(s.Pending()); got != 0 {
		t.Fatalf("a busy session produced %d notifications", got)
	}

	// And waits again, ten minutes later.
	advance(10 * time.Minute)
	second := now().Add(-3 * time.Minute)
	s.Observe(waitingSince(second))
	if got := len(s.Pending()); got != 1 {
		t.Fatalf("the second episode produced %d notifications, want 1", got)
	}
}

// TestTheDelayIsServedByAnAlarmAndNotByAPoll is R4.4's "not a poll".
//
// A session that has been waiting 30 seconds against a 2-minute delay produces
// NO notification and asks to be woken in exactly 90 seconds. Nothing here
// loops, and the alarm is armed once.
func TestTheDelayIsServedByAnAlarmAndNotByAPoll(t *testing.T) {
	now, advance := at(NOW)
	s := New(Config{Policy: loud(), Now: now})

	s.Observe(waitingSince(NOW.Add(-30 * time.Second)))
	if got := len(s.Pending()); got != 0 {
		t.Fatalf("a session 30s into a 2m delay produced %d notifications", got)
	}
	d, ok := s.WakeIn()
	if !ok {
		t.Fatal("no alarm was armed; without one the deadline never arrives and the delay " +
			"silently means never")
	}
	if d != 90*time.Second {
		t.Errorf("the alarm is for %v, want 90s — the REMAINDER of the delay, not the whole of it", d)
	}

	// The deadline arrives; the cycle it asked for observes again.
	advance(90 * time.Second)
	s.Observe(waitingSince(NOW.Add(-30 * time.Second)))
	notes := s.Pending()
	if len(notes) != 1 {
		t.Fatalf("at the deadline the session produced %d notifications, want 1", len(notes))
	}
	if notes[0].For != 2*time.Minute {
		t.Errorf("the note says it waited %v, want 2m", notes[0].For)
	}
	// And now there is nothing left to wait for. A stage that re-armed here
	// would be a poll loop with extra steps.
	if d, ok := s.WakeIn(); ok {
		t.Errorf("the alarm was re-armed for %v after the notification; that is a poll loop", d)
	}
}

// TestNothingIsArmedForASessionThatWillNeverQualify is the idle-cost claim, at
// the one place it can be checked: a stage that answered a duration for every
// session would hold thirty timers on a quiet machine.
func TestNothingIsArmedForASessionThatWillNeverQualify(t *testing.T) {
	now, _ := at(NOW)
	for _, tt := range []struct {
		name string
		s    obs.Session
	}{
		{"busy", obs.Session{ID: "id-a", Status: obs.StatusBusy, StatusSince: obs.Ptr(NOW)}},
		{"idle", obs.Session{ID: "id-a", Status: obs.StatusIdle, StatusSince: obs.Ptr(NOW)}},
		{"a shell", obs.Session{ID: "id-a", Status: obs.StatusShell, StatusSince: obs.Ptr(NOW)}},
		{"undetermined", obs.Session{ID: "id-a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{Policy: loud(), Now: now})
			s.Observe(tt.s)
			if d, ok := s.WakeIn(); ok {
				t.Errorf("armed an alarm for %v", d)
			}
			if got := len(s.Pending()); got != 0 {
				t.Errorf("produced %d notifications", got)
			}
		})
	}
}

// TestADelayThatCannotBeMeasuredSaysNothing is NFR4 applied to the one figure
// the delay is compared against.
//
// A record with no statusSince gives no elapsed time. Announcing anyway would
// be asserting the session has waited long enough on no evidence at all; and
// arming an alarm would be worse, because the wait would repeat for ever.
func TestADelayThatCannotBeMeasuredSaysNothing(t *testing.T) {
	now, _ := at(NOW)
	s := New(Config{Policy: loud(), Now: now})
	s.Observe(obs.Session{ID: "id-a", PID: 1, Status: obs.StatusWaiting})
	if got := len(s.Pending()); got != 0 {
		t.Errorf("an unmeasurable wait produced %d notifications", got)
	}
	if d, ok := s.WakeIn(); ok {
		t.Errorf("armed an alarm for %v against a wait it cannot measure", d)
	}

	// With no delay configured there is nothing to measure, so the same
	// session DOES notify — which is what keeps the test above about the
	// measurement rather than about the status.
	p := loud()
	p.Notify.Delay = 0
	s = New(Config{Policy: p, Now: now})
	s.Observe(obs.Session{ID: "id-a", PID: 1, Status: obs.StatusWaiting})
	if got := len(s.Pending()); got != 1 {
		t.Errorf("with delay=0 an unmeasurable wait produced %d notifications, want 1", got)
	}
}

// TestQuietSuppressesAndSaysSo pins the second filter and the decision it
// records, because evidence for a suppressed notification is the record.
func TestQuietSuppressesAndSaysSo(t *testing.T) {
	// 23:14 local, inside the 22:00-08:00 window.
	night := time.Date(2026, 9, 18, 23, 14, 0, 0, time.Local)
	now, _ := at(night)
	s := New(Config{Policy: loud(), Now: now})
	s.Observe(waitingSince(night.Add(-10 * time.Minute)))

	notes := s.Pending()
	if len(notes) != 1 {
		t.Fatalf("quiet hours produced %d notes, want 1 — a suppressed notification is still "+
			"DECIDED, and the record is the evidence that the filter worked", len(notes))
	}
	n := notes[0]
	if n.Decision.Allowed {
		t.Fatalf("the note was allowed at 23:14 inside a 22:00-08:00 quiet window: %+v", n.Decision)
	}
	for _, want := range []string{"notify.quiet", "22:00", "08:00", "23:14"} {
		if !strings.Contains(n.Decision.Rule, want) {
			t.Errorf("the authority is %q and does not mention %q", n.Decision.Rule, want)
		}
	}
	if n.Decision.Stance != act.StanceUnknown {
		t.Errorf("the decision carries stance %v; a notification is governed by [notify] and "+
			"not by an [actions] stance, so the stance must stay unset", n.Decision.Stance)
	}
}

// TestQuietSuppressesRatherThanDefers is the decision spelled out, because the
// alternative is the failure R4.4 names, moved to breakfast.
func TestQuietSuppressesRatherThanDefers(t *testing.T) {
	night := time.Date(2026, 9, 18, 23, 0, 0, 0, time.Local)
	now, advance := at(night)
	s := New(Config{Policy: loud(), Now: now})
	since := night.Add(-10 * time.Minute)

	s.Observe(waitingSince(since))
	notes := s.Pending()
	if len(notes) != 1 || notes[0].Decision.Allowed {
		t.Fatalf("expected one suppressed note at 23:00, got %+v", notes)
	}

	// Morning. The same session, the same episode, still waiting.
	advance(10 * time.Hour) // 09:00, outside the window
	s.Observe(waitingSince(since))
	if got := len(s.Pending()); got != 0 {
		t.Fatalf("the suppressed notification was DELIVERED at 09:00 (%d notes). A night's "+
			"worth arriving at breakfast is exactly the '30 sessions firing unfiltered' "+
			"failure R4.4 names", got)
	}
}

// TestTheDayIsSplitByTheClockAndNotByTheDate walks a whole day against the
// midnight-crossing window, at the interior of each span rather than at a
// boundary.
func TestTheDayIsSplitByTheClockAndNotByTheDate(t *testing.T) {
	for _, tt := range []struct {
		hour    int
		allowed bool
	}{
		{3, false}, {7, false}, {9, true}, {14, true}, {21, true}, {23, false},
	} {
		when := time.Date(2026, 9, 18, tt.hour, 30, 0, 0, time.Local)
		now, _ := at(when)
		s := New(Config{Policy: loud(), Now: now})
		s.Observe(waitingSince(when.Add(-10 * time.Minute)))
		notes := s.Pending()
		if len(notes) != 1 {
			t.Fatalf("%02d:30 produced %d notes", tt.hour, len(notes))
		}
		if notes[0].Decision.Allowed != tt.allowed {
			t.Errorf("%02d:30: allowed=%v, want %v (%s)",
				tt.hour, notes[0].Decision.Allowed, tt.allowed, notes[0].Decision.Rule)
		}
	}
}

// TestOnlyTheConfiguredTriggersFire: `notify.on` is a list the user wrote, and
// a trigger not in it must not raise anything.
func TestOnlyTheConfiguredTriggersFire(t *testing.T) {
	now, _ := at(NOW)
	errored := obs.Session{
		ID: "id-a", PID: 7, Status: obs.StatusBusy,
		StatusSince: obs.Ptr(NOW.Add(-time.Hour)),
		Fault:       obs.Fault{APIErrors: obs.Ptr(3)},
	}

	waitingOnly := loud()
	waitingOnly.Notify.On = []act.NotifyOn{act.NotifyOnWaiting}
	s := New(Config{Policy: waitingOnly, Now: now})
	s.Observe(errored)
	if got := len(s.Pending()); got != 0 {
		t.Errorf("an error raised %d notifications under on=[waiting]", got)
	}

	s = New(Config{Policy: loud(), Now: now})
	s.Observe(errored)
	notes := s.Pending()
	if len(notes) != 1 || notes[0].On != act.NotifyOnError {
		t.Fatalf("on=[waiting,error] produced %+v, want one error note", notes)
	}
	if !strings.Contains(notes[0].Body(), "3 API errors") {
		t.Errorf("the body is %q and does not say what happened", notes[0].Body())
	}

	// Zero errors is a DETERMINED zero and must not fire; nil is nobody
	// counted and must not either.
	for _, faults := range []obs.Fault{{APIErrors: obs.Ptr(0)}, {}} {
		s = New(Config{Policy: loud(), Now: now})
		quiet := errored
		quiet.Fault = faults
		s.Observe(quiet)
		if got := len(s.Pending()); got != 0 {
			t.Errorf("fault %+v raised %d notifications", faults, got)
		}
	}
}

// TestWaitingBeatsErrorWhenBothAreTrue pins the ordering, because a person
// seeing one notification should see the one they can act on now.
func TestWaitingBeatsErrorWhenBothAreTrue(t *testing.T) {
	now, _ := at(NOW)
	s := New(Config{Policy: loud(), Now: now})
	both := waitingSince(NOW.Add(-10 * time.Minute))
	both.Fault = obs.Fault{APIErrors: obs.Ptr(2)}
	s.Observe(both)
	notes := s.Pending()
	if len(notes) != 1 || notes[0].On != act.NotifyOnWaiting {
		t.Fatalf("produced %+v, want one waiting note", notes)
	}
}

// TestTheNoteSaysWhichErrandItIs: "input needed" and "permission prompt" are
// distinct in requirements.md R2 and are different errands for the human, so
// they must not arrive as one sentence.
func TestTheNoteSaysWhichErrandItIs(t *testing.T) {
	now, _ := at(NOW)
	// The expectations are built from the ENUM rather than written out,
	// because that is the property under test: one vocabulary, in the
	// notification and in the document. A table of literals here would also
	// trip adapters.TestOnlySchemaFileDecodesClaudeStrings the moment somebody
	// reached for Claude's own spelling of the second one, which is what
	// happened while this test was being written.
	for _, tt := range []struct {
		reason obs.WaitingReason
		want   string
	}{
		{obs.WaitingReasonInput, "waiting for " + obs.WaitingReasonInput.String()},
		{obs.WaitingReasonPermission, "waiting for " + obs.WaitingReasonPermission.String()},
		{obs.WaitingReasonUnknown, "waiting for you"},
		{obs.WaitingReasonNone, "waiting for you"},
	} {
		s := New(Config{Policy: loud(), Now: now})
		sess := waitingSince(NOW.Add(-10 * time.Minute))
		sess.Waiting = tt.reason
		s.Observe(sess)
		notes := s.Pending()
		if len(notes) != 1 {
			t.Fatalf("%v produced %d notes", tt.reason, len(notes))
		}
		if !strings.Contains(notes[0].Body(), tt.want) {
			t.Errorf("%v reads %q, want it to contain %q", tt.reason, notes[0].Body(), tt.want)
		}
	}
}

// TestTheNoteNamesTheSession, falling back through name, id and pid — because
// a notification that does not say which of thirty sessions it is about is
// worse than none.
func TestTheNoteNamesTheSession(t *testing.T) {
	now, _ := at(NOW)
	for _, tt := range []struct {
		name  string
		sess  obs.Session
		title string
	}{
		{"a named session", obs.Session{Name: "worktree-a"}, "worktree-a"},
		{"an unnamed one", obs.Session{ID: "a1478d55-6e02-4bb9"}, "a1478d55"},
		{"neither", obs.Session{PID: 4242}, "4242"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_ = now
			got := Note{Session: tt.sess}.Title()
			if !strings.Contains(got, tt.title) {
				t.Errorf("title is %q, want it to contain %q", got, tt.title)
			}
		})
	}
}

// TestPendingDrains is what makes the hand-off exactly-once. A Pending that
// returned the same slice twice would deliver every notification twice, and
// the second delivery would look exactly like the repeat this design forbids.
func TestPendingDrains(t *testing.T) {
	now, _ := at(NOW)
	s := New(Config{Policy: loud(), Now: now})
	s.Observe(waitingSince(NOW.Add(-10 * time.Minute)))
	if got := len(s.Pending()); got != 1 {
		t.Fatalf("first Pending returned %d", got)
	}
	if got := len(s.Pending()); got != 0 {
		t.Fatalf("second Pending returned %d; the notes were not drained", got)
	}
}

// TestTheNoteIsAFROZENCopyOfTheSession is freeze point 3 at this boundary.
//
// obs.Session is mostly pointers and the next observation replaces the whole
// fact set, so a note that shared them would describe whichever observation
// happened to be current when the coordinator got round to delivering it.
func TestTheNoteIsAFrozenCopyOfTheSession(t *testing.T) {
	now, _ := at(NOW)
	s := New(Config{Policy: loud(), Now: now})
	in := waitingSince(NOW.Add(-10 * time.Minute))
	in.SubagentCount = obs.Ptr(3)
	s.Observe(in)
	notes := s.Pending()
	if len(notes) != 1 {
		t.Fatalf("produced %d notes", len(notes))
	}
	// Mutate through the caller's own pointer, as a careless middleware would.
	*in.SubagentCount = 99
	*in.StatusSince = NOW
	if got := notes[0].Session.SubagentCount; got == nil || *got != 3 {
		t.Errorf("the note's subagent count followed the caller's mutation to %v", got)
	}
	if notes[0].Session.StatusSince == nil || !notes[0].Session.StatusSince.Equal(NOW.Add(-10*time.Minute)) {
		t.Errorf("the note's statusSince followed the caller's mutation to %v",
			notes[0].Session.StatusSince)
	}
}

// TestObserveChangesNothingAboutTheSession: the notification is not a fact
// about the session and must not reach the wire. architecture-v4 §9 settles
// the document's keys, and a `notified` field would be magmux reporting on
// itself in a document that is entirely about the session.
func TestObserveChangesNothingAboutTheSession(t *testing.T) {
	now, _ := at(NOW)
	s := New(Config{Policy: loud(), Now: now})
	in := waitingSince(NOW.Add(-10 * time.Minute))
	out := s.Observe(in)
	if out.Status != in.Status || out.Waiting != in.Waiting || out.ID != in.ID ||
		out.Attention != in.Attention || out.Confidence != in.Confidence {
		t.Errorf("the stage modified the fact set: in=%+v out=%+v", in, out)
	}
}

// TestTheAuthorityNamesTheClauseThatDecided is R5.A's "by which rule" for a
// notification: the audit record quotes this verbatim, and a line that cannot
// say what raised it does not answer the question the trail exists for.
func TestTheAuthorityNamesTheClauseThatDecided(t *testing.T) {
	now, _ := at(NOW)
	s := New(Config{Policy: loud(), Now: now})
	s.Observe(waitingSince(NOW.Add(-10 * time.Minute)))
	notes := s.Pending()
	if len(notes) != 1 {
		t.Fatalf("produced %d notes", len(notes))
	}
	rule := notes[0].Decision.Rule
	for _, want := range []string{"test policy", "notify.on", "waiting", "notify.delay", "2m"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the authority is %q and does not mention %q", rule, want)
		}
	}
	if strings.TrimSpace(rule) == "" {
		t.Error("an empty authority; act.Writer.Append refuses one, so this would be an " +
			"unrecordable notification")
	}
}

// TestAStaleRecordNeverNotifies is requirements.md R1.3 reaching the notifier.
//
// Claude Code never deletes a record and about half of the ones on the machine
// this was built for are stale, so a `waiting` record whose process exited is
// the COMMON case rather than an edge one. It was found by running the real
// binary against a staged registry of dead pids and watching it decide to
// notify about both of them.
//
// Undetermined is the other half and must NOT reject: a session dropped
// because nobody could tell is the failure the tri-state exists to prevent.
func TestAStaleRecordNeverNotifies(t *testing.T) {
	now, _ := at(NOW)
	for _, tt := range []struct {
		name    string
		process obs.Liveness
		pane    obs.Liveness
		notify  bool
	}{
		{"alive", obs.LivenessYes, obs.LivenessUnknown, true},
		{"its process exited", obs.LivenessNo, obs.LivenessUnknown, false},
		{"its pane was closed", obs.LivenessYes, obs.LivenessNo, false},
		{"nobody could tell", obs.LivenessUnknown, obs.LivenessUnknown, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{Policy: loud(), Now: now})
			sess := waitingSince(NOW.Add(-10 * time.Minute))
			sess.ProcessAlive, sess.PaneAlive = tt.process, tt.pane
			s.Observe(sess)
			got := len(s.Pending()) == 1
			if got != tt.notify {
				t.Errorf("notified=%v, want %v — a DETERMINED no rejects and an undetermined "+
					"answer never does", got, tt.notify)
			}
		})
	}
}
