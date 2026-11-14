package mux

// The notification path, end to end through the claudestate pipeline: the
// notify stage decides on the session's actor, the library hands its Note to
// magmux's OnEffects on the coordinator, and that records and only then
// delivers.
//
// These are the old internal/obs/pipeline notify_test.go, REWRITTEN rather
// than moved (claudestate architecture §2.2): that file drove the old
// coordinator's own fields. The assertions are the same, through the public
// surface — a stage.Factory-built notify stage, OnEffects, Exec for the
// counters — and the one that could not be carried word for word says so. It
// is also the only test in this layer that needs a lock (the recording port is
// read across goroutines), which is why it lives in the host: the library has
// none (C3).
//
// Nothing here reads ~/.claude, probes a pid, or opens a notification centre.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/obsview"
)

// recordingPort is notify.Port with the delivery removed. Send runs on the
// coordinator's goroutine and the test reads across it, hence the lock.
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

// watchingPort looks at the world from inside Send, which is the one moment at
// which "recorded before sent" is observable.
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

// refusingAuditor is a trail that cannot take a record: the unwritable state
// directory, without needing one.
type refusingAuditor struct{}

func (refusingAuditor) Append(r act.Record) (act.Record, error) {
	return r, errors.New("the state directory is read-only")
}

// notifyPolicy is the R6 sketch's posture with the delay and quiet windows the
// caller wants.
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

// trailCount is how many lines the trail holds, 0 when it does not exist yet.
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

func decodeHostTrail(t *testing.T, path string) []act.Record {
	t.Helper()
	var out []act.Record
	for _, line := range hostTrailLines(t, path) {
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

func notifyStats(t *testing.T, p *pipeline.Pipeline, acts *obsActions) obsActionStats {
	t.Helper()
	st, err := acts.statsOf(context.Background(), p)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	return st
}

func cyclesOf(t *testing.T, p *pipeline.Pipeline) int {
	t.Helper()
	st, err := p.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return st.Cycles
}

// TestANotificationIsRecordedBeforeItIsSent: the trail holds an attempt with a
// null outcome and then an outcome referring back to it, and the attempt is on
// disk BEFORE the port is called.
func TestANotificationIsRecordedBeforeItIsSent(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := hostTrail(t)
	var mu sync.Mutex
	linesAtSend := -1
	port := &watchingPort{onSend: func() {
		mu.Lock()
		linesAtSend = trailCount(path)
		mu.Unlock()
	}}
	p, _ := startHost(t, dir, hostOpts{policy: notifyPolicy(0), audit: trail, port: port})
	firstHostDoc(t, p)
	waitUntilObs(t, func() bool { return trailCount(path) >= 2 }, "the attempt and its outcome")

	if port.count() != 1 {
		t.Fatalf("the port was called %d times, want once", port.count())
	}
	mu.Lock()
	seen := linesAtSend
	mu.Unlock()
	if seen < 1 {
		t.Fatalf("the trail held %d lines when the port was called; the attempt goes first", seen)
	}
	recs := decodeHostTrail(t, path)
	if len(recs) != 2 {
		t.Fatalf("the trail holds %d records, want an attempt and an outcome", len(recs))
	}
	attempt, outcome := recs[0], recs[1]
	if attempt.Event != act.EventAttempt || attempt.Outcome != nil {
		t.Errorf("the first line is %+v, want an attempt with a null outcome", attempt)
	}
	if attempt.Before == nil || attempt.Before.Status != obsview.StatusWaiting {
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

// TestANotifyRecordIsTellableFromAnActionRecord: a null tier and a null
// requestedBy on a notify line, a table tier and a surface on an action line.
// act.TestAReservedVerbWithNoSurfaceWearsANotificationsShape pins the one
// in-process path that collides.
func TestANotifyRecordIsTellableFromAnActionRecord(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{
		policy: notifyPolicy(0), audit: trail, port: &recordingPort{},
		locator: paneOne{}, focuser: &countingFocuser{},
	})
	firstHostDoc(t, p)
	waitUntilObs(t, func() bool { return trailCount(path) >= 2 }, "the notification")
	if _, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a")); err != nil {
		t.Fatalf("do: %v", err)
	}

	var notifies, actions int
	for _, r := range decodeHostTrail(t, path) {
		switch r.Verb {
		case act.VerbNotify:
			notifies++
			if r.Tier != act.TierUnknown {
				t.Errorf("a notify line carries tier %v", r.Tier)
			}
			if r.By != act.RequesterUnknown {
				t.Errorf("a notify line says it was requested by %v; nothing asked for it", r.By)
			}
		case act.VerbFocus:
			actions++
			if r.Tier != act.TierNavigate || r.By == act.RequesterUnknown {
				t.Errorf("an action line carries tier %v by %v", r.Tier, r.By)
			}
		default:
			t.Errorf("an unexpected verb %q in the trail", r.Verb)
		}
	}
	if notifies == 0 || actions == 0 {
		t.Fatalf("the trail holds %d notify and %d action lines; this proves nothing unless it holds both", notifies, actions)
	}
}

// TestASuppressedNotificationIsRecordedAndNotSent is the `quiet` half of R6:
// ONE line, event=refused, and the port never called.
func TestASuppressedNotificationIsRecordedAndNotSent(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := hostTrail(t)
	port := &recordingPort{}
	window := act.QuietWindow{
		From: act.NewDayMinute(hostEpoch.Hour(), 0),
		To:   act.NewDayMinute((hostEpoch.Hour()+2)%24, 0),
	}
	p, acts := startHost(t, dir, hostOpts{policy: notifyPolicy(0, window), audit: trail, port: port})
	firstHostDoc(t, p)
	waitUntilObs(t, func() bool { return trailCount(path) >= 1 }, "the suppression to be recorded")

	if got := port.count(); got != 0 {
		t.Fatalf("%d notifications were DELIVERED inside a quiet window", got)
	}
	recs := decodeHostTrail(t, path)
	if len(recs) != 1 {
		t.Fatalf("the trail holds %d records, want exactly one refusal", len(recs))
	}
	r := recs[0]
	if r.Event != act.EventRefused || r.Outcome == nil || !r.Outcome.Refused {
		t.Errorf("the record is %v with outcome %+v, want refused", r.Event, r.Outcome)
	}
	if !strings.Contains(r.Policy.Rule, "notify.quiet") {
		t.Errorf("the authority is %q and does not name the clause that silenced it", r.Policy.Rule)
	}
	if st := notifyStats(t, p, acts); st.NotificationsSuppressed != 1 || st.Notifications != 0 {
		t.Errorf("stats say %d suppressed / %d sent, want 1/0", st.NotificationsSuppressed, st.Notifications)
	}
}

// TestANotificationThatCannotBeRecordedIsNotSent: an unwritable trail silences
// notifications, the count says so, and the failure rides out to every sink as
// a problem on the NEXT document — which the coordinator triggers itself
// (C13), so no outside refresh is needed for it to arrive.
func TestANotificationThatCannotBeRecordedIsNotSent(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	port := &recordingPort{}
	p, acts := startHost(t, dir, hostOpts{policy: notifyPolicy(0), audit: refusingAuditor{}, port: port})
	sub, err := p.Subscribe(context.Background(), "test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	mustReceiveHost(t, sub)

	waitUntilObs(t, func() bool { return notifyStats(t, p, acts).NotificationsUnrecorded == 1 },
		"the unrecordable notification to be counted")
	if got := port.count(); got != 0 {
		t.Fatalf("%d notifications were sent with no way to record them", got)
	}
	d := mustReceiveHost(t, sub)
	found := false
	for _, pr := range d.Problems {
		if pr.Scope == model.ScopeHost && strings.Contains(pr.Error(), "could not be recorded") {
			found = true
		}
	}
	if !found {
		t.Errorf("no sink was told the notification was dropped; problems were %v", d.Problems)
	}
}

// TestNoPortMeansRecordedAndUndelivered is the honest state of a magmux built
// without one — an OUTCOME, not an attempt left hanging.
func TestNoPortMeansRecordedAndUndelivered(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := hostTrail(t)
	p, _ := startHost(t, dir, hostOpts{policy: notifyPolicy(0), audit: trail})
	firstHostDoc(t, p)
	waitUntilObs(t, func() bool { return trailCount(path) >= 2 }, "both lines")
	recs := decodeHostTrail(t, path)
	if len(recs) != 2 || recs[1].Outcome == nil || recs[1].Outcome.Done || !strings.Contains(recs[1].Outcome.Error, "deliver") {
		t.Fatalf("the trail is %+v, want an attempt and a not-done outcome that says why", recs)
	}
}

// TestAFailedSendIsRecordedAsAFailureAndNotRetried: a retry loop against a
// broken notification centre is a storm waiting for the day it comes back.
func TestAFailedSendIsRecordedAsAFailureAndNotRetried(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := hostTrail(t)
	port := &recordingPort{err: errors.New("osascript: not available")}
	p, _ := startHost(t, dir, hostOpts{policy: notifyPolicy(0), audit: trail, port: port})
	firstHostDoc(t, p)
	waitUntilObs(t, func() bool { return trailCount(path) >= 2 }, "both lines")
	start := cyclesOf(t, p)
	for i := 0; i < 5; i++ {
		p.Refresh()
		waitUntilObs(t, func() bool { return cyclesOf(t, p) >= start+i+1 }, "a later cycle")
	}
	recs := decodeHostTrail(t, path)
	if len(recs) != 2 {
		t.Fatalf("the trail holds %d records after more cycles, want the one attempt and its outcome", len(recs))
	}
	if recs[1].Outcome == nil || recs[1].Outcome.Done || !strings.Contains(recs[1].Outcome.Error, "osascript") {
		t.Errorf("the outcome is %+v, want the port's own error", recs[1].Outcome)
	}
}

// TestOneWaitingSessionNotifiesOnceAcrossManyCycles: the episode latch through
// the real actor and a registry that keeps being re-read.
func TestOneWaitingSessionNotifiesOnceAcrossManyCycles(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := hostTrail(t)
	port := &recordingPort{}
	p, _ := startHost(t, dir, hostOpts{policy: notifyPolicy(0), audit: trail, port: port})
	firstHostDoc(t, p)
	start := cyclesOf(t, p)
	for i := 0; i < 10; i++ {
		p.Refresh()
		waitUntilObs(t, func() bool { return cyclesOf(t, p) >= start+i+1 }, "a later cycle")
	}
	if got := port.count(); got != 1 {
		t.Fatalf("eleven cycles over one unchanged waiting session produced %d notifications, want 1", got)
	}
	if got := len(decodeHostTrail(t, path)); got != 2 {
		t.Errorf("the trail holds %d records, want one attempt and one outcome", got)
	}
}

// TestTheDelayIsServedByAnAlarmThatAsksForACycle is R4.4's "not a poll": the
// actor's ONE one-shot alarm is what produces the cycle at the deadline.
//
// The old coordinator counted wakes (Stats.Wakes); the library does not. The
// same claim is made here by elimination: nothing watches, nothing refreshes,
// and the cycle count rises by exactly one when — and only when — the alarm is
// fired.
func TestTheDelayIsServedByAnAlarmThatAsksForACycle(t *testing.T) {
	dir := t.TempDir()
	// The record says the session entered `waiting` one minute before
	// hostEpoch; the delay is two, so one minute is still to serve.
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	clock := ports.NewFakeClock(hostEpoch)
	port := &recordingPort{}
	trail, _ := hostTrail(t)

	var mu sync.Mutex
	current := hostEpoch
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	p, _ := startHost(t, dir, hostOpts{policy: notifyPolicy(2 * time.Minute), audit: trail, port: port, clock: clock, now: now})
	firstHostDoc(t, p)
	if got := port.count(); got != 0 {
		t.Fatalf("%d notifications one minute into a two-minute delay", got)
	}
	var timer *ports.FakeTimer
	select {
	case timer = <-clock.Timers():
	case <-time.After(10 * time.Second):
		t.Fatal("no alarm was armed")
	}
	if timer.Delay() != time.Minute {
		t.Fatalf("the alarm is for %v, want the 1m remaining of the 2m delay", timer.Delay())
	}
	before := cyclesOf(t, p)

	mu.Lock()
	current = hostEpoch.Add(time.Minute)
	fireAt := current
	mu.Unlock()
	timer.Fire(fireAt)

	waitUntilObs(t, func() bool { return port.count() == 1 }, "the notification at the deadline")
	if after := cyclesOf(t, p); after != before+1 {
		t.Errorf("the alarm produced %d cycles, want exactly one; anything else is something other than the alarm serving the delay", after-before)
	}
	if n := port.all()[0]; n.For < 2*time.Minute {
		t.Errorf("the note says it waited %v, want at least the 2m delay", n.For)
	}
}

// TestAnEffectThatIsNotANoteIsAHostProblem: the type assertion in OnEffects IS
// the hand-off, so an effect magmux does not deliver is a programming error,
// returned as a ScopeHost problem rather than ignored.
func TestAnEffectThatIsNotANoteIsAHostProblem(t *testing.T) {
	acts := newObsActions(act.Default(), nil, nil, nil)
	k := model.Key{SessionID: "id-a", PID: 1}
	problems := acts.onEffects(context.Background(), pipeline.EffectBatch{Items: []pipeline.EffectItem{
		{Key: k, Effect: strangeEffect{}},
	}})
	if len(problems) != 1 || problems[0].Scope != model.ScopeHost || problems[0].Kind != model.ProblemHost {
		t.Fatalf("an unknown effect produced %+v, want one ScopeHost problem", problems)
	}
	if problems[0].Key == nil || *problems[0].Key != k || !strings.Contains(problems[0].Detail, "strange") {
		t.Errorf("the problem does not name the session and the effect: %+v", problems[0])
	}
}

type strangeEffect struct{}

func (strangeEffect) EffectName() string { return "strange" }

var _ stage.Effect = strangeEffect{}
