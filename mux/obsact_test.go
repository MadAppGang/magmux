package mux

// The write path, end to end through the claudestate pipeline: the gate and the
// audit trail on the coordinator (Pipeline.Exec), the navigate stage reached on
// the session's actor (Ops.Call), and the one live verb.
//
// These are the old internal/obs/pipeline act_test.go, REWRITTEN rather than
// moved (claudestate architecture §2.2): that file was package pipeline and
// drove fields the move removed. The assertions are the same; where one could
// not be re-expressed through the library's public surface it says so.
//
// Nothing here reads ~/.claude, probes a pid or writes outside a temporary
// directory. The registry is staged, the focuser is a fake, and the audit trail
// is a file under t.TempDir().

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/pipeline"
	"github.com/MadAppGang/magmux/claudestate/ports"
	"github.com/MadAppGang/magmux/claudestate/stage"
	"github.com/MadAppGang/magmux/internal/obs/act"
	"github.com/MadAppGang/magmux/internal/obs/navigate"
	"github.com/MadAppGang/magmux/internal/obs/notify"
	"github.com/MadAppGang/magmux/obsview"
)

// hostEpoch is a fixed instant, so a published document is comparable.
var hostEpoch = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

// writeHostRecord writes one <pid>.json in the registry's own on-disk shape,
// its instants a minute before hostEpoch, in MILLISECONDS.
func writeHostRecord(t *testing.T, dir string, pid int, id, name, status string) {
	t.Helper()
	ms := hostEpoch.Add(-time.Minute).UnixMilli()
	body := `{"sessionId":"` + id + `","pid":` + strconv.Itoa(pid) + `,"name":"` + name + `","cwd":"/tmp/project",` +
		`"status":"` + status + `","statusUpdatedAt":` + strconv.FormatInt(ms, 10) + `,"updatedAt":` + strconv.FormatInt(ms, 10) + `}`
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
}

// countingFocuser is magmux's pane table, reduced to what a navigate needs.
// calls is appended to on an ACTOR's goroutine and read by the test after the
// action returned, which is a happens-before edge.
type countingFocuser struct {
	mu      sync.Mutex
	focused int
	calls   []int
}

func (f *countingFocuser) Focus(paneID int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, paneID)
	previous := f.focused
	f.focused = paneID
	return previous, nil
}

func (f *countingFocuser) called() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.calls...)
}

// paneOne answers the pane gate: every session is in pane 1, which is alive.
type paneOne struct{}

func (paneOne) PaneFor(model.Session) (model.PaneRef, model.Liveness) {
	return obsview.PaneRef(1), model.LivenessYes
}
func (paneOne) PaneAlive(model.PaneRef) model.Liveness { return model.LivenessYes }

// hostOpts is one test host: the ports and host stages a pipeline is built
// with, exactly as startObservationWith builds them, minus a Magmux.
type hostOpts struct {
	policy  act.Policy
	audit   act.Auditor
	port    notify.Port
	locator ports.PaneLocator
	focuser navigate.PaneFocuser
	// noNavigate builds a chain with no navigate stage: a read-only process.
	noNavigate bool
	extra      []stage.Factory // before navigate
	clock      ports.Clock     // nil: the real clock
	now        func() time.Time
}

// startHost starts one host pipeline over a registry directory and stops it
// at the end of the test.
func startHost(t *testing.T, dir string, o hostOpts) (*pipeline.Pipeline, *obsActions) {
	t.Helper()
	now := o.now
	if now == nil {
		now = func() time.Time { return hostEpoch }
	}
	acts := newObsActions(o.policy, o.audit, o.port, now)
	clock := o.clock
	if clock == nil {
		clock = ports.RealClock()
	}
	chain := append([]stage.Factory(nil), o.extra...)
	if !o.noNavigate {
		chain = append(chain, navigate.Factory(navigate.Config{Focuser: o.focuser}))
	}
	chain = append(chain, notify.Factory(notify.Config{Policy: o.policy, Now: now}))
	root, err := claudeRoot(dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.Start(pipeline.Config{
		Root: root, Locator: o.locator, Chain: chain, OnEffects: acts.onEffects,
		Clock: clock, Now: now,
	})
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	t.Cleanup(p.Stop)
	return p, acts
}

// firstHostDoc subscribes, asks for a cycle and returns the document.
func firstHostDoc(t *testing.T, p *pipeline.Pipeline) pipeline.Delivery {
	t.Helper()
	sub, err := p.Subscribe(context.Background(), "test", 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	p.Refresh()
	return mustReceiveHost(t, sub)
}

func mustReceiveHost(t *testing.T, sub *pipeline.Subscription) pipeline.Delivery {
	t.Helper()
	select {
	case d, ok := <-sub.C():
		if !ok {
			t.Fatal("the subscription closed")
		}
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("no document within 10s")
	}
	return pipeline.Delivery{}
}

// hostTrail builds a real audit writer under a temporary directory, so that
// the on-disk shape is exercised rather than a fake's idea of it.
func hostTrail(t *testing.T) (*act.Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "magmux", act.AuditFileName)
	w, err := act.NewWriter(act.WriterConfig{Path: path, Now: func() time.Time { return hostEpoch }, PID: 7777})
	if err != nil {
		t.Fatalf("act.NewWriter: %v", err)
	}
	return w, path
}

func hostTrailLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

func focusRequest(sessionID string) act.Request {
	return act.Request{
		Action: act.Action{Verb: act.VerbFocus, Tier: act.TierNavigate, Session: sessionID},
		By:     act.RequesterCLI,
	}
}

// TestANavigateReachesTheChainThroughTheGateAndIsRecorded is the whole write
// path in one test: a request arrives, Exec resolves the session's key on the
// coordinator, the gate decides and records, the session's actor runs the
// navigate stage, and the trail on disk says what happened.
func TestANavigateReachesTheChainThroughTheGateAndIsRecorded(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")

	focuser := &countingFocuser{}
	trail, path := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{policy: act.Default(), audit: trail, locator: paneOne{}, focuser: focuser})
	firstHostDoc(t, p)

	rep, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a"))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if !rep.Result.Done {
		t.Fatalf("the action reported itself not done: %+v", rep.Result)
	}
	if calls := focuser.called(); len(calls) != 1 || calls[0] != 1 {
		t.Fatalf("the focuser was called %v, want exactly pane 1", calls)
	}
	if rep.Decision.Stance != act.StanceAlways {
		t.Errorf("the decision was %+v; R6.1 gives navigate `always`", rep.Decision)
	}

	lines := hostTrailLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("the trail holds %d lines, want an attempt and an outcome:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], `"event":"attempt"`) || !strings.Contains(lines[0], `"outcome":null`) {
		t.Errorf("the first line is not an attempt with a null outcome:\n%s", lines[0])
	}
	if !strings.Contains(lines[1], `"event":"outcome"`) || !strings.Contains(lines[1], `"done":true`) {
		t.Errorf("the second line does not record what happened:\n%s", lines[1])
	}
	for _, want := range []string{`"id":"id-a"`, `"pid":4242`, `"verb":"focus"`,
		`"tier":"navigate"`, `"requestedBy":"cli"`, `"rule":"the built-in default posture`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the attempt line does not carry %s:\n%s", want, lines[0])
		}
	}

	st, err := acts.statsOf(context.Background(), p)
	if err != nil || st.Actions != 1 || st.ActionsRefused != 0 {
		t.Errorf("stats say %d actions and %d refused (%v)", st.Actions, st.ActionsRefused, err)
	}
}

// TestThePipelineRefusesEveryDeferredTier — the act package's proof, made again
// through the real path, because what could go wrong here is different: a host
// that asked the actor FIRST and the gate second would run a restart and then
// record that it had refused it.
func TestThePipelineRefusesEveryDeferredTier(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "busy")

	focuser := &countingFocuser{}
	trail, path := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{
		policy: act.Policy{
			Source: "a policy that says always to everything",
			Actions: act.Actions{
				Navigate: act.StanceAlways, Approve: act.StanceAlways,
				Send: act.StanceAlways, Lifecycle: act.StanceAlways,
			},
		},
		audit: trail, locator: paneOne{}, focuser: focuser,
	})
	firstHostDoc(t, p)

	for _, tc := range []struct {
		verb string
		tier act.Tier
		want string
	}{
		{"approve", act.TierApprove, "R5.2"},
		{"send_input", act.TierSend, "R5.3"},
		{"restart", act.TierLifecycle, "R5.4"},
	} {
		req := act.Request{Action: act.Action{Verb: tc.verb, Tier: tc.tier, Session: "id-a"}, By: act.RequesterMCP}
		_, err := acts.do(context.Background(), p, req, keyByID("id-a"))
		if !errors.Is(err, act.ErrDeferred) {
			t.Fatalf("%s was answered with %v, want a deferral", tc.verb, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("the refusal does not name %s: %v", tc.want, err)
		}
	}
	if calls := focuser.called(); len(calls) != 0 {
		t.Fatalf("a deferred tier reached the pane table: %v", calls)
	}
	lines := hostTrailLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("three refusals produced %d lines", len(lines))
	}
	for _, line := range lines {
		if !strings.Contains(line, `"event":"refused"`) || !strings.Contains(line, "hard-disabled in code") {
			t.Errorf("a deferred tier produced a line that is not a hard-disabled refusal:\n%s", line)
		}
	}
	if st, _ := acts.statsOf(context.Background(), p); st.ActionsRefused != 3 {
		t.Errorf("stats say %d refused, want 3", st.ActionsRefused)
	}
}

// TestASessionWithNoPaneDeclaresNothing — `magmux agents` outside a magmux, and
// every session on a machine magmux did not start. The refusal is "cannot", not
// "will not": nothing declares the verb, because there is no pane to focus.
func TestASessionWithNoPaneDeclaresNothing(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "busy")

	trail, path := hostTrail(t)
	// No locator and no focuser: the honest shape of a process that is not a
	// magmux.
	p, acts := startHost(t, dir, hostOpts{policy: act.Default(), audit: trail})
	firstHostDoc(t, p)

	_, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a"))
	if !errors.Is(err, act.ErrNotCapable) {
		t.Fatalf("a session with no pane was answered with %v, want `cannot`", err)
	}
	if errors.Is(err, act.ErrForbidden) {
		t.Fatal("a session with no pane was refused as a policy matter")
	}
	if lines := hostTrailLines(t, path); len(lines) != 1 || !strings.Contains(lines[0], `"event":"refused"`) {
		t.Errorf("the refusal was not recorded: %v", lines)
	}
}

// TestAnActionForASessionNobodyHasObservedIsRefusedAndRecorded — a typo, a
// session that has exited, or a document a client is holding from ten minutes
// ago. There is no session, so nothing declares anything, and the refusal
// names the session that was asked for rather than silently doing nothing.
func TestAnActionForASessionNobodyHasObservedIsRefusedAndRecorded(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "busy")

	trail, path := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{policy: act.Default(), audit: trail, locator: paneOne{}, focuser: &countingFocuser{}})
	firstHostDoc(t, p)

	_, err := acts.do(context.Background(), p, focusRequest("id-that-never-was"), keyByID("id-that-never-was"))
	if !errors.Is(err, act.ErrNotCapable) {
		t.Fatalf("an unknown session was answered with %v", err)
	}
	lines := hostTrailLines(t, path)
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":"id-that-never-was"`) {
		t.Errorf("the refusal does not say what was asked for:\n%s", strings.Join(lines, "\n"))
	}
}

// TestAPipelineThatWasNeverGivenAPolicyCannotAct is the zero-value rule at the
// level a caller actually builds: a config literal that says nothing about
// policy or audit. Two cases, and the FIRST is the load-bearing half: given a
// working trail, the policy is the only thing that can refuse.
func TestAPipelineThatWasNeverGivenAPolicyCannotAct(t *testing.T) {
	t.Run("no policy, but a working trail", func(t *testing.T) {
		dir := t.TempDir()
		writeHostRecord(t, dir, 4242, "id-a", "session-a", "busy")
		focuser := &countingFocuser{}
		trail, _ := hostTrail(t)
		p, acts := startHost(t, dir, hostOpts{audit: trail, locator: paneOne{}, focuser: focuser})
		firstHostDoc(t, p)
		_, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a"))
		if !errors.Is(err, act.ErrForbidden) {
			t.Fatalf("a host whose config says nothing about policy answered with %v, want a refusal from the policy", err)
		}
		if calls := focuser.called(); len(calls) != 0 {
			t.Fatalf("the pane table was reached: %v", calls)
		}
	})
	t.Run("an allowing policy, and no trail", func(t *testing.T) {
		dir := t.TempDir()
		writeHostRecord(t, dir, 4242, "id-a", "session-a", "busy")
		focuser := &countingFocuser{}
		p, acts := startHost(t, dir, hostOpts{policy: act.Default(), locator: paneOne{}, focuser: focuser})
		firstHostDoc(t, p)
		_, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a"))
		if !errors.Is(err, act.ErrUnaudited) {
			t.Fatalf("a host with no audit trail answered with %v", err)
		}
		if calls := focuser.called(); len(calls) != 0 {
			t.Fatalf("the pane table was reached: %v", calls)
		}
	})
}

// blockingStage holds its chain inside Observe until released, once it is
// armed, so a test can put an action behind a chain run that is in flight.
type blockingStage struct {
	armed   chan struct{}
	entered chan struct{}
	release chan struct{}
}

func (b *blockingStage) Name() string { return "blocker" }
func (b *blockingStage) Observe(s model.Session) model.Session {
	select {
	case <-b.armed:
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-b.release
	default:
	}
	return s
}

// TestAnActionIsOrderedWithTheObservationsAroundIt — host code runs on the
// coordinator BETWEEN cycles, and a session's actor serialises its own
// messages, so an action cannot overtake a chain run that is in flight: the
// fact set an action is decided against is the one the session had when the
// gate looked.
func TestAnActionIsOrderedWithTheObservationsAroundIt(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	block := &blockingStage{armed: make(chan struct{}), entered: make(chan struct{}, 1), release: make(chan struct{})}
	trail, _ := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{
		policy: act.Default(), audit: trail, locator: paneOne{}, focuser: &countingFocuser{},
		extra: []stage.Factory{func(model.Key) stage.Stage { return block }},
	})
	firstHostDoc(t, p)

	close(block.armed)
	p.Refresh()
	select {
	case <-block.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the chain never ran")
	}

	done := make(chan error, 1)
	go func() {
		_, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the action overtook a chain run that was still in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(block.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the action failed once the chain was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the action never completed")
	}
}

// countingStage is a read-only stage that counts its runs.
type countingStage struct {
	mu   sync.Mutex
	runs int
}

func (c *countingStage) Name() string { return "reader" }
func (c *countingStage) Observe(s model.Session) model.Session {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
	return s
}
func (c *countingStage) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs
}

// TestOnlyTheStagesThatOptInAreAskedToAct — the host finds the navigate stage
// BY NAME on the actor and asks nothing else: a read-only stage beside it is
// not run by an action. (The old actor's own Acted counter has no library
// counterpart; the focuser's call count is the evidence instead.)
func TestOnlyTheStagesThatOptInAreAskedToAct(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	reader := &countingStage{}
	focuser := &countingFocuser{}
	trail, _ := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{
		policy: act.Default(), audit: trail, locator: paneOne{}, focuser: focuser,
		extra: []stage.Factory{func(model.Key) stage.Stage { return reader }},
	})
	firstHostDoc(t, p)
	before := reader.count()

	if _, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a")); err != nil {
		t.Fatalf("do: %v", err)
	}
	if calls := focuser.called(); len(calls) != 1 {
		t.Fatalf("the navigate stage ran %d times", len(calls))
	}
	if reader.count() != before {
		t.Fatal("a read-only stage was run by an action")
	}
}

// TestAnActorWithNoWriteHalfDeclaresNothing — every chain built without the
// navigate stage. The answer is an empty declaration and a recorded refusal,
// never a panic and never a silent success.
func TestAnActorWithNoWriteHalfDeclaresNothing(t *testing.T) {
	dir := t.TempDir()
	writeHostRecord(t, dir, 4242, "id-a", "session-a", "waiting")
	trail, path := hostTrail(t)
	p, acts := startHost(t, dir, hostOpts{policy: act.Default(), audit: trail, locator: paneOne{}, noNavigate: true})
	firstHostDoc(t, p)
	_, err := acts.do(context.Background(), p, focusRequest("id-a"), keyByID("id-a"))
	if !errors.Is(err, act.ErrNotCapable) {
		t.Fatalf("a chain with no write half answered an action with %v", err)
	}
	if lines := hostTrailLines(t, path); len(lines) != 1 {
		t.Errorf("the refusal was not recorded: %v", lines)
	}
}

// ── the audit line across the move ──────────────────────────────────────────

// auditCorpus is the magmux-origin corpus the C0.6 audit line was recorded
// over, and auditGolden the line itself.
const (
	auditCorpus = "../internal/obs/testdata"
	auditGolden = "../obsview/testdata/golden/magmux/audit-focus.jsonl"
)

// auditPanes is the pane table the recorded focus runs against: the target
// session's pid is in pane 3, and pane 1 held focus before. Nothing else is in
// any pane, and no process is involved: the pids are the corpus's.
type auditPanes struct{ pid int }

func (a auditPanes) PaneFor(s model.Session) (model.PaneRef, model.Liveness) {
	if s.PID == a.pid {
		return obsview.PaneRef(3), model.LivenessYes
	}
	return "", model.LivenessNo
}

func (auditPanes) PaneAlive(ref model.PaneRef) model.Liveness {
	if id, ok := obsview.PaneID(ref); ok && id == 3 {
		return model.LivenessYes
	}
	return model.LivenessNo
}

func (auditPanes) Focus(id int) (int, error) {
	if id != 3 {
		return -1, errors.New("no such pane")
	}
	return 1, nil
}

// TestAnAuditLineIsByteIdenticalAcrossTheMove replays the focus C0.6 recorded —
// the attempt line and its outcome — through the library's pipeline, magmux's
// gate on its coordinator and the real writer, over the same corpus, with the
// same fixed clock and pid, and holds the trail to the recorded BYTES. act now
// takes the library's session and spells its `before` state through
// obsview.FromModelSession; this is what says the trail it writes did not move
// (claudestate architecture §2.2).
func TestAnAuditLineIsByteIdenticalAcrossTheMove(t *testing.T) {
	const targetPID = 1724
	const targetID = "8e2d4b17-0c93-4a6f-b512-77c3ad9e4f60"
	epoch := time.UnixMilli(1789545200000).UTC()

	path := filepath.Join(t.TempDir(), "trail.jsonl")
	w, err := act.NewWriter(act.WriterConfig{Path: path, Now: func() time.Time { return epoch }, PID: 4242})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	acts := newObsActions(act.Default(), w, nil, func() time.Time { return epoch })
	panes := auditPanes{pid: targetPID}
	tree := func(name string) ports.Tree {
		return ports.Tree{Path: "/testdata/" + name, FS: os.DirFS(filepath.Join(auditCorpus, name))}
	}
	p, err := pipeline.Start(pipeline.Config{
		Root:    ports.Root{Sessions: tree("sessions"), Projects: tree("projects")},
		Locator: panes,
		Chain:   []stage.Factory{navigate.Factory(navigate.Config{Focuser: panes})},
		Clock:   ports.RealClock(), RetryDelay: time.Millisecond,
		Now: func() time.Time { return epoch },
	})
	if err != nil {
		t.Fatalf("pipeline.Start: %v", err)
	}
	defer p.Stop()
	firstHostDoc(t, p)

	if _, err := acts.do(context.Background(), p, focusRequest(targetID), keyByID(targetID)); err != nil {
		t.Fatalf("the recorded focus was refused: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	want, err := os.ReadFile(auditGolden)
	if err != nil {
		t.Fatalf("reading %s: %v", auditGolden, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("the audit trail of a recorded focus changed:\n got %s\nwant %s", got, want)
	}
}
