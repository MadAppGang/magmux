package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/internal/obs"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/notify"
)

// The notification path, end to end through the coordinator: the stage
// decides, the coordinator records, and only then does anything reach a human.
//
// Nothing here reads ~/.claude, probes a pid, or opens a notification centre.
// The registry is staged, the trail is a file under t.TempDir(), and the port
// is a recorder.

// recordingPort is [notify.Port] with the delivery removed.
//
// Send is called on the COORDINATOR's goroutine and the test reads the result
// after a command reply, which is a happens-before edge — the same rule
// countingFocuser follows. The mutex is belt and braces for the one case where
// a test reads while a cycle may still be running.
type recordingPort struct {
	mu   sync.Mutex
	sent []notify.Note
	err  error
}

func (p *recordingPort) Send(ctx context.Context, n notify.Note) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.sent = append(p.sent, n)
	return nil
}

func (p *recordingPort) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

func (p *recordingPort) all() []notify.Note {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]notify.Note(nil), p.sent...)
}

// watchingPort is a port that looks at the world from inside Send, which is
// the one moment at which "recorded before sent" is observable.
//
// onSend runs on the coordinator's goroutine and the test reads what it
// recorded only after waiting for the trail, which is a happens-before edge.
type watchingPort struct {
	recordingPort
	onSend func()
}

func (p *watchingPort) Send(ctx context.Context, n notify.Note) error {
	if p.onSend != nil {
		p.onSend()
	}
	return p.recordingPort.Send(ctx, n)
}

// refusingAuditor is a trail that cannot take a record. It is the unwritable
// state directory, without needing one.
type refusingAuditor struct{ calls int }

func (a *refusingAuditor) Append(r act.Record) (act.Record, error) {
	a.calls++
	return r, errors.New("the state directory is read-only")
}

// notifyPolicy is the R6 sketch's posture with the delay and the quiet windows
// the caller wants.
func notifyPolicy(delay time.Duration, quiet ...act.QuietWindow) act.Policy {
	p := act.Default()
	p.Source = "test policy"
	p.Notify = act.Notify{
		Enabled: true,
		On:      []act.NotifyOn{act.NotifyOnWaiting, act.NotifyOnError},
		Delay:   delay,
		Quiet:   quiet,
	}
	return p
}

// trailCount is how many lines the trail holds, and 0 when it does not exist
// yet. Non-fatal, deliberately: it is polled from waitFor while the file is
// still being created, and act.Writer creates nothing until the first record.
func trailCount(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// decodeTrail reads the audit file into records.
func decodeTrail(t *testing.T, path string) []act.Record {
	t.Helper()
	var out []act.Record
	for _, line := range trailLines(t, path) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r act.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decoding an audit line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// TestANotificationIsRecordedBeforeItIsSent is the phase-6 rule applied to the
// one outward-facing act that nobody requested.
//
// The trail must hold an attempt with a null outcome and then an outcome
// referring back to it — the same two-line shape Gate.Do writes — and the
// attempt must be on disk BEFORE the port is called.
func TestANotificationIsRecordedBeforeItIsSent(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := trailIn(t)

	// The ordering is asserted from INSIDE the port, which is the only place
	// it can be seen: by the time the test reads the file both lines are
	// there, and "the attempt was written first" would be an inference from
	// the sequence numbers rather than an observation.
	var linesAtSend int
	port := &watchingPort{onSend: func() { linesAtSend = trailCount(path) }}

	p := startPipeline(t, Config{
		Root:   dir,
		Policy: notifyPolicy(0),
		Audit:  trail,
		Notify: port,
	})
	firstDoc(t, p)
	waitFor(t, func() bool { return trailCount(path) >= 2 }, "the attempt and its outcome")

	if port.count() != 1 {
		t.Fatalf("the port was called %d times, want once", port.count())
	}
	if linesAtSend < 1 {
		t.Fatalf("the trail held %d lines when the port was called; a notification that cannot "+
			"be recorded must not be sent, so the attempt goes first", linesAtSend)
	}

	recs := decodeTrail(t, path)
	if len(recs) != 2 {
		t.Fatalf("the trail holds %d records, want an attempt and an outcome:\n%s",
			len(recs), strings.Join(trailLines(t, path), "\n"))
	}
	attempt, outcome := recs[0], recs[1]
	if attempt.Event != act.EventAttempt || attempt.Outcome != nil {
		t.Errorf("the first line is %+v, want an attempt with a null outcome", attempt)
	}
	if attempt.Before == nil || attempt.Before.Status != obs.StatusWaiting {
		t.Errorf("the attempt does not carry the state that raised it: %+v", attempt.Before)
	}
	if outcome.Event != act.EventOutcome || outcome.Ref != attempt.Seq {
		t.Errorf("the outcome is %+v, want event=outcome ref=%d", outcome, attempt.Seq)
	}
	if outcome.Outcome == nil || !outcome.Outcome.Done {
		t.Errorf("the outcome says %+v, want done", outcome.Outcome)
	}
	for _, r := range recs {
		if r.Verb != act.VerbNotify {
			t.Errorf("verb is %q, want %q", r.Verb, act.VerbNotify)
		}
		if r.Target.SessionID != "id-a" || r.Target.PID != 4242 {
			t.Errorf("the record aims at %+v, want session id-a / pid 4242", r.Target)
		}
		if strings.TrimSpace(r.Policy.Rule) == "" {
			t.Error("a record with no authority; R5.A's 'by which rule' is not optional")
		}
	}
}

// TestANotifyRecordIsTellableFromAnActionRecord is the two-places-not-one rule
// in act.VerbNotify, checked against a trail that holds both kinds of line.
//
// A reader must be able to tell them apart without knowing how either was
// made, and the two facts that do it — a null tier and a null requestedBy —
// are unreachable on an action line reached from outside this process, because
// Gate.Do refuses an unnamed requester at step 1 and TierUnknown matches no
// capability.
//
// It is NOT unreachable in every case, and this test does not prove that it is.
// One in-process path writes a refusal wearing this shape;
// act.TestAReservedVerbWithNoSurfaceWearsANotificationsShape pins that
// limitation, and act.VerbNotify's doc explains it. What this test checks is the
// shape of the two kinds of line a running pipeline actually produces.
func TestANotifyRecordIsTellableFromAnActionRecord(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := trailIn(t)

	p := startPipeline(t, Config{
		Root:    dir,
		Locator: paneOne{},
		Focuser: &countingFocuser{},
		Policy:  notifyPolicy(0),
		Audit:   trail,
		Notify:  &recordingPort{},
	})
	firstDoc(t, p)
	if _, err := p.Do(context.Background(), focusRequest("id-a")); err != nil {
		t.Fatalf("Do: %v", err)
	}

	var notifies, actions int
	for _, r := range decodeTrail(t, path) {
		switch r.Verb {
		case act.VerbNotify:
			notifies++
			if r.Tier != act.TierUnknown {
				t.Errorf("a notify line carries tier %v; a notification belongs to no action "+
					"tier because it changes nothing on any session", r.Tier)
			}
			if r.By != act.RequesterUnknown {
				t.Errorf("a notify line says it was requested by %v; nothing asked for it", r.By)
			}
		case act.VerbFocus:
			actions++
			if r.Tier != act.TierNavigate {
				t.Errorf("an action line carries tier %v, want navigate", r.Tier)
			}
			if r.By == act.RequesterUnknown {
				t.Error("an action line with no requesting surface; Gate.Do refuses that at step 1")
			}
		default:
			t.Errorf("an unexpected verb %q in the trail", r.Verb)
		}
	}
	if notifies == 0 || actions == 0 {
		t.Fatalf("the trail holds %d notify and %d action lines; this test proves nothing "+
			"unless it holds both", notifies, actions)
	}
}

// TestASuppressedNotificationIsRecordedAndNotSent is the `quiet` half of R6,
// and the evidence a suppressed notification leaves behind.
//
// ONE line, event=refused, no outcome pair — because nothing was attempted —
// and the port is never called.
func TestASuppressedNotificationIsRecordedAndNotSent(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := trailIn(t)
	port := &recordingPort{}

	// A window covering the test's fixed instant, built from that instant's
	// own hour so the suppression is deterministic rather than dependent on
	// the timezone the suite happens to run in. QuietWindow.Contains reads the
	// hour in the instant's own location, and the instant here is `epoch`.
	window := act.QuietWindow{
		From: act.NewDayMinute(epoch.Hour(), 0),
		To:   act.NewDayMinute((epoch.Hour()+2)%24, 0),
	}
	p := startPipeline(t, Config{
		Root:   dir,
		Policy: notifyPolicy(0, window),
		Audit:  trail,
		Notify: port,
		Now:    func() time.Time { return epoch },
	})
	firstDoc(t, p)
	waitFor(t, func() bool { return trailCount(path) >= 1 }, "the suppression to be recorded")

	if got := port.count(); got != 0 {
		t.Fatalf("%d notifications were DELIVERED inside a quiet window", got)
	}
	recs := decodeTrail(t, path)
	if len(recs) != 1 {
		t.Fatalf("the trail holds %d records, want exactly one refusal:\n%s",
			len(recs), strings.Join(trailLines(t, path), "\n"))
	}
	r := recs[0]
	if r.Event != act.EventRefused {
		t.Errorf("the record is %v, want refused", r.Event)
	}
	if r.Outcome == nil || !r.Outcome.Refused {
		t.Errorf("the outcome is %+v, want refused so a reader need not infer it from done=false",
			r.Outcome)
	}
	if !strings.Contains(r.Policy.Rule, "notify.quiet") {
		t.Errorf("the authority is %q and does not name the clause that silenced it", r.Policy.Rule)
	}

	st := p.Stats()
	if st.NotificationsSuppressed != 1 || st.Notifications != 0 {
		t.Errorf("stats say %d suppressed / %d sent, want 1/0", st.NotificationsSuppressed, st.Notifications)
	}
}

// TestANotificationThatCannotBeRecordedIsNotSent is the rule phase 6 stated
// for actions, applied here deliberately rather than by accident.
//
// An unwritable trail silences notifications, the count says so, and the
// failure rides out to every sink as a problem — because a silence nobody was
// told about is indistinguishable from a quiet machine.
func TestANotificationThatCannotBeRecordedIsNotSent(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	auditor := &refusingAuditor{}
	port := &recordingPort{}

	p := startPipeline(t, Config{
		Root:   dir,
		Policy: notifyPolicy(0),
		Audit:  auditor,
		Notify: port,
	})
	sub, err := p.Subscribe("test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	mustReceive(t, sub, 10*time.Second)

	waitFor(t, func() bool { return p.Stats().NotificationsUnrecorded == 1 },
		"the unrecordable notification to be counted")
	if got := port.count(); got != 0 {
		t.Fatalf("%d notifications were sent with no way to record them", got)
	}

	// The next document carries the reason, which is the only place a person
	// can learn why their notifications stopped.
	p.Refresh()
	d := mustReceive(t, sub, 10*time.Second)
	found := false
	for _, pr := range d.Problems {
		if strings.Contains(pr.Error(), "could not be recorded") {
			found = true
		}
	}
	if !found {
		t.Errorf("no sink was told the notification was dropped; problems were %v", d.Problems)
	}
}

// TestNoPortMeansRecordedAndUndelivered is the honest state of a magmux built
// without one — and it must be an OUTCOME, not an attempt left hanging.
func TestNoPortMeansRecordedAndUndelivered(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := trailIn(t)

	p := startPipeline(t, Config{Root: dir, Policy: notifyPolicy(0), Audit: trail})
	firstDoc(t, p)
	waitFor(t, func() bool { return trailCount(path) >= 2 }, "both lines")

	recs := decodeTrail(t, path)
	if len(recs) != 2 {
		t.Fatalf("the trail holds %d records, want an attempt and an outcome", len(recs))
	}
	if recs[1].Outcome == nil || recs[1].Outcome.Done {
		t.Errorf("the outcome is %+v, want not done", recs[1].Outcome)
	}
	if !strings.Contains(recs[1].Outcome.Error, "deliver") {
		t.Errorf("the outcome does not say why: %q", recs[1].Outcome.Error)
	}
}

// TestAFailedSendIsRecordedAsAFailureAndNotRetried.
//
// A retry loop against a broken notification centre is a storm waiting for the
// day it comes back, and a notification that was late is worth less than one
// that was not sent.
func TestAFailedSendIsRecordedAsAFailureAndNotRetried(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := trailIn(t)
	port := &recordingPort{err: errors.New("osascript: not available")}

	p := startPipeline(t, Config{Root: dir, Policy: notifyPolicy(0), Audit: trail, Notify: port})
	firstDoc(t, p)
	waitFor(t, func() bool { return trailCount(path) >= 2 }, "both lines")

	// Several more cycles, none of which must produce another attempt.
	for i := 0; i < 5; i++ {
		p.Refresh()
	}
	waitFor(t, func() bool { return p.Stats().Cycles >= 6 }, "the later cycles")

	recs := decodeTrail(t, path)
	if len(recs) != 2 {
		t.Fatalf("the trail holds %d records after six cycles, want the one attempt and its "+
			"outcome — a failed notification must not be retried:\n%s",
			len(recs), strings.Join(trailLines(t, path), "\n"))
	}
	if recs[1].Outcome == nil || recs[1].Outcome.Done ||
		!strings.Contains(recs[1].Outcome.Error, "osascript") {
		t.Errorf("the outcome is %+v, want the port's own error", recs[1].Outcome)
	}
}

// TestOneWaitingSessionNotifiesOnceAcrossManyCycles is the "not a repeat" rule
// where it actually bites: through the real actor, the real coordinator and a
// registry that keeps being re-read.
//
// This is the deliberate break the brief names. Deleting the episode latch in
// notify.Stage makes this produce one notification per cycle.
func TestOneWaitingSessionNotifiesOnceAcrossManyCycles(t *testing.T) {
	dir := newRegistry(t)
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := trailIn(t)
	port := &recordingPort{}

	p := startPipeline(t, Config{Root: dir, Policy: notifyPolicy(0), Audit: trail, Notify: port})
	firstDoc(t, p)
	for i := 0; i < 10; i++ {
		p.Refresh()
	}
	waitFor(t, func() bool { return p.Stats().Cycles >= 11 }, "eleven cycles")

	if got := port.count(); got != 1 {
		t.Fatalf("eleven cycles over one unchanged waiting session produced %d notifications, "+
			"want 1 — a session that stays waiting for an hour is one errand, not thirty", got)
	}
	if got := len(decodeTrail(t, path)); got != 2 {
		t.Errorf("the trail holds %d records, want one attempt and one outcome", got)
	}
}

// TestTheDelayIsServedByAnAlarmThatAsksForACycle is R4.4's "not a poll",
// through the pipeline: the actor's one-shot alarm is what produces the cycle
// at the deadline, and the wake is counted so the claim is checkable.
func TestTheDelayIsServedByAnAlarmThatAsksForACycle(t *testing.T) {
	dir := newRegistry(t)
	// The record says the session entered `waiting` one minute ago; the delay
	// is two, so 60 seconds are still to serve.
	writeRecord(t, dir, 4242, "id-a", "session-a", "waiting")

	clock := obs.NewFakeClock(epoch)
	port := &recordingPort{}
	trail, _ := trailIn(t)

	// The instant is a separate, MOVING value from the clock, exactly as
	// registry.Config.Now documents: obs.FakeClock's Now is fixed forever and
	// a test that expected it to advance would see one instant for ever.
	var mu sync.Mutex
	current := epoch
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}

	p := startPipeline(t, Config{
		Root:   dir,
		Policy: notifyPolicy(2 * time.Minute),
		Audit:  trail,
		Notify: port,
		Clock:  clock,
		Now:    now,
	})
	firstDoc(t, p)

	if got := port.count(); got != 0 {
		t.Fatalf("%d notifications one minute into a two-minute delay", got)
	}
	timer, err := clock.NextTimer(10 * time.Second)
	if err != nil {
		t.Fatalf("no alarm was armed: %v", err)
	}
	if timer.Delay() != time.Minute {
		t.Fatalf("the alarm is for %v, want the 1m remaining of the 2m delay", timer.Delay())
	}

	// The deadline arrives. Nothing on disk changed; the alarm is the only
	// reason there is a cycle at all.
	mu.Lock()
	current = epoch.Add(time.Minute)
	fireAt := current
	mu.Unlock()
	timer.Fire(fireAt)

	waitFor(t, func() bool { return port.count() == 1 }, "the notification at the deadline")
	if st := p.Stats(); st.Wakes == 0 {
		t.Error("the cycle was not attributed to a wake; if it came from anywhere else, the " +
			"delay is being served by something that is not the alarm")
	}
	if n := port.all()[0]; n.For < 2*time.Minute {
		t.Errorf("the note says it waited %v, want at least the 2m delay", n.For)
	}
}

// waitFor blocks until cond holds, and names what it was waiting for when it
// does not. It exists because every assertion in this file is about something
// that happens on the coordinator's goroutine after a document was published,
// and a bare sleep would be either flaky or slow.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
