package pipeline

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

// listing is a fake AgentsListing. calls travels on a channel because List
// runs on its own goroutine.
type listing struct {
	raw   []byte
	err   error
	calls chan struct{}
}

func (l *listing) List(context.Context) (ports.Listing, error) {
	if l.calls != nil {
		l.calls <- struct{}{}
	}
	if l.err != nil {
		return ports.Listing{}, l.err
	}
	return ports.Listing{Raw: l.raw, At: base}, nil
}

// The agents source is reached ONLY through RefreshSources (FR23); its rate
// limit and backoff are timestamps on Config.Now (§3.3); an interactive entry
// no record describes becomes a session.
func TestTheAgentsSourceIsScheduledOnlyByRefreshSources(t *testing.T) {
	now := base
	lst := &listing{
		raw:   []byte(`[{"` + claude.KeySessionID + `":"lonely","` + claude.KeyPID + `":987654,"` + claude.KeyKind + `":"` + claude.ValueKindInteractive + `","` + claude.KeyStatus + `":"` + claude.ValueStatusIdle + `"}]`),
		calls: make(chan struct{}, 16),
	}
	h := start(t, Config{
		Root:   ports.Root{Sessions: mapTree("/s", fstest.MapFS{})},
		Agents: lst, Now: func() time.Time { return now },
	})
	h.cycle()
	h.cycle()
	if len(lst.calls) != 0 {
		t.Fatal("a watch or refresh cycle spawned the listing")
	}
	out, err := h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents})
	if err != nil || !out.Ran || out.Entries != 1 || !out.At.Equal(base) {
		t.Fatalf("%+v %v", out, err)
	}
	d := h.next()
	s, ok := byKey(d, model.Key{SessionID: "lonely", PID: 987654})
	if !ok || s.StatusFrom != model.SourceAgents || sourceRow(d, model.SourceAgents).State != model.SourceOK {
		t.Fatalf("%+v %+v", s, d.Doc.Sources)
	}

	out, _ = h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents})
	if out.Ran || out.Skipped != "min-interval" {
		t.Fatalf("within the minimum interval: %+v", out)
	}
	now = now.Add(time.Minute)
	lst.err = errors.New("claude is not installed")
	out, _ = h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents})
	if out.Ran || out.Err == nil {
		t.Fatalf("a failed List: %+v", out)
	}
	d = h.next()
	if sourceRow(d, model.SourceAgents).State != model.SourceDegraded || !hasProblem(d.Problems, model.ProblemAgentsListing) {
		t.Fatalf("an older listing is still applied: %+v", sourceRow(d, model.SourceAgents))
	}
	out, _ = h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents})
	if out.Skipped != "backoff" {
		t.Fatalf("after a failure: %+v", out)
	}
	now = now.Add(time.Hour)
	lst.err = nil
	if out, _ := h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents}); !out.Ran {
		t.Fatalf("the backoff has passed: %+v", out)
	}
	if h.clk.Created() != 0 {
		t.Fatalf("the agents gates armed %d timers", h.clk.Created())
	}
}

func TestRefreshSourcesWithNoAgentsPortIsSourceOff(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}})
	if _, err := h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents}); !errors.Is(err, ErrSourceOff) {
		t.Fatalf("%v", err)
	}
	if _, err := h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceRegistry}); err != nil {
		t.Fatalf("another source is satisfied by the next cycle: %v", err)
	}
	h.next()
}

// A malformed listing is a FAILED listing, never an empty one (C12): with no
// earlier listing the source reads Failed.
func TestAMalformedListingFailsTheSource(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}, Agents: &listing{raw: []byte("claude: error\n")}})
	out, _ := h.p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents})
	if out.Ran || out.Err == nil {
		t.Fatalf("%+v", out)
	}
	if d := h.next(); sourceRow(d, model.SourceAgents).State != model.SourceFailed {
		t.Fatalf("%+v", sourceRow(d, model.SourceAgents))
	}
}

// held is an AgentsListing that blocks until released or until its context
// ends, and says when each happened.
type held struct {
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
}

func newHeld() *held {
	return &held{started: make(chan struct{}, 1), release: make(chan struct{}), returned: make(chan struct{})}
}

func (h *held) List(ctx context.Context) (ports.Listing, error) {
	defer close(h.returned)
	h.started <- struct{}{}
	select {
	case <-h.release:
		return ports.Listing{Raw: []byte("[]"), At: base}, nil
	case <-ctx.Done():
		return ports.Listing{}, ctx.Err()
	}
}

func awaitStarted(t *testing.T, h *held) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(wait):
		t.Fatal("List never started")
	}
}

// One caller giving up must not fail the source for everyone: the List is the
// pipeline's, not the first caller's (review r1 #6a).
func TestACallerGivingUpDoesNotFailTheAgentsSource(t *testing.T) {
	lst := newHeld()
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}, Agents: lst})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.p.RefreshSources(ctx, []model.SourceID{model.SourceAgents})
		done <- err
	}()
	awaitStarted(t, lst)
	cancel()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("RefreshSources ignored its caller's cancellation")
	}
	close(lst.release)
	for {
		d := h.next()
		switch row := sourceRow(d, model.SourceAgents); row.State {
		case model.SourceStateUnknown:
			continue
		case model.SourceOK:
			if hasProblem(d.Problems, model.ProblemAgentsListing) {
				t.Fatalf("an OK listing with a listing problem: %v", d.Problems)
			}
			return
		default:
			t.Fatalf("a caller's cancellation failed the source for everyone: %+v %v", row, d.Problems)
		}
	}
}

// Stop waits for every goroutine the pipeline owns, the List included: it
// cancels the List's context and waits for it to return (review r1 #6a).
func TestStopEndsAndWaitsForAnInFlightList(t *testing.T) {
	lst := newHeld()
	clk := ports.NewFakeClock(base)
	p, err := Start(Config{Clock: clk, Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}, Agents: lst})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = p.RefreshSources(context.Background(), []model.SourceID{model.SourceAgents}) }()
	awaitStarted(t, lst)
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(wait):
		t.Fatal("Stop hung on an in-flight List")
	}
	select {
	case <-lst.returned:
	default:
		t.Fatal("Stop returned while the List it started was still running")
	}
}
