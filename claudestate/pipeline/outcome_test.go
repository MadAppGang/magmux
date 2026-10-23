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
	"github.com/MadAppGang/magmux/claudestate/stage"
)

// within runs f on its own goroutine and FAILS, rather than hanging the suite,
// when it has not returned in time. The goroutine is left to the test's
// cleanup, which releases whatever f is blocked behind.
func within(t *testing.T, d time.Duration, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("%s had not returned %v after its caller's context ended: it blocks the caller for as long as "+
			"the host code does (review r2 #2/#3)", what, d)
	}
	return nil
}

// release is a gate host code blocks behind. It is opened by the test's
// cleanup, which runs before the harness's Stop (cleanups run last-first), so
// the pipeline can always stop.
func release(t *testing.T) chan struct{} {
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	return gate
}

// An Exec the coordinator has ACCEPTED, whose caller's context then ends,
// returns at once with ErrOutcomeUnknown — wrapping the context's own error —
// and never a plain context error: fn has run or is running, so "cancelled"
// would claim it did not happen, and waiting for fn would hand the caller's
// goroutine to host code that ignores its context (review r2 #2; the r1 #6b
// fix waited for ever).
func TestAnAcceptedExecWhoseCallerGivesUpReportsAnUnknownOutcome(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}})
	gate := release(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() { <-started; cancel() }()
	err := within(t, 2*time.Second, "Exec", func() error {
		return h.p.Exec(ctx, func(context.Context, Ops) error {
			close(started)
			<-gate // host code that does not honour its context
			return nil
		})
	})
	if !errors.Is(err, ErrOutcomeUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec returned %v, want ErrOutcomeUnknown wrapping context.Canceled", err)
	}
}

// Before acceptance, a context that ended is a plain refusal, as it always
// was: fn never runs, so there is nothing whose outcome is unknown.
func TestAnExecRefusedBeforeAcceptanceIsAPlainContextError(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := h.p.Exec(ctx, func(context.Context, Ops) error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrOutcomeUnknown) || ran {
		t.Fatalf("a cancelled Exec: err %v, ran %v; want a plain context.Canceled and fn never run", err, ran)
	}

	// The same while the coordinator is busy: the second Exec is never
	// accepted, so its caller giving up is a refusal.
	gate := release(t)
	busy := make(chan struct{})
	go func() {
		_ = h.p.Exec(context.Background(), func(context.Context, Ops) error { close(busy); <-gate; return nil })
	}()
	<-busy
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	ran2 := make(chan struct{}, 1)
	err = within(t, 2*time.Second, "a second Exec", func() error {
		return h.p.Exec(ctx2, func(context.Context, Ops) error { ran2 <- struct{}{}; return nil })
	})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("an Exec never accepted returned %v, want a plain context.DeadlineExceeded", err)
	}
	select {
	case <-ran2:
		t.Fatal("an Exec refused before acceptance ran anyway")
	default:
	}
}

// Ops.Call follows the same rule, one level down: once the actor has the
// callback, a caller whose context ends is told the outcome is unknown, never
// that it failed — the callback may be completing (a focus that moved the
// keyboard) while the host records "cancelled" (review r2 #3).
func TestAnAcceptedCallWhoseCallerGivesUpReportsAnUnknownOutcome(t *testing.T) {
	sessions := fstest.MapFS{"1.json": {Data: rec(1, "s", claude.ValueStatusIdle)}}
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", sessions)}})
	d := h.cycle()
	if len(d.Doc.Sessions) != 1 {
		t.Fatalf("want one session, got %d", len(d.Doc.Sessions))
	}
	key := d.Doc.Sessions[0].Key
	gate := release(t)
	ctx, cancel := context.WithCancel(context.Background())
	var callErr error
	execErr := within(t, 2*time.Second, "Exec around Ops.Call", func() error {
		return h.p.Exec(context.Background(), func(_ context.Context, x Ops) error {
			callErr = x.Call(ctx, key, func(model.Session, func(string) (stage.Stage, bool)) error {
				cancel() // the caller gives up while the callback is running
				<-gate
				return nil
			})
			return nil
		})
	})
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	if !errors.Is(callErr, ErrOutcomeUnknown) || !errors.Is(callErr, context.Canceled) {
		t.Fatalf("Ops.Call returned %v, want ErrOutcomeUnknown wrapping context.Canceled", callErr)
	}

	// Before the actor has it, an ended context is a plain refusal.
	ran := false
	execErr = h.p.Exec(context.Background(), func(_ context.Context, x Ops) error {
		callErr = x.Call(ctx, key, func(model.Session, func(string) (stage.Stage, bool)) error { ran = true; return nil })
		return nil
	})
	if execErr != nil || !errors.Is(callErr, context.Canceled) || errors.Is(callErr, ErrOutcomeUnknown) || ran {
		t.Fatalf("a Call on an ended context: exec %v, call %v, ran %v; want a plain refusal", execErr, callErr, ran)
	}
}

// lateCancel is a context that ends exactly between the coordinator's
// acceptance of a command and its reply: Done is open the first time ask
// selects on it (the offer) and closed every time after, and Err agrees. Only
// the asking goroutine calls Done and Err — the coordinator never reads a
// Subscribe's, Unsubscribe's or Stats' context — so the counter needs no lock.
type lateCancel struct {
	context.Context
	calls        int
	open, closed chan struct{}
}

func newLateCancel() *lateCancel {
	c := &lateCancel{Context: context.Background(), open: make(chan struct{}), closed: make(chan struct{})}
	close(c.closed)
	return c
}

func (c *lateCancel) Done() <-chan struct{} {
	c.calls++
	if c.calls == 1 {
		return c.open
	}
	return c.closed
}

func (c *lateCancel) Err() error {
	if c.calls >= 2 {
		return context.Canceled
	}
	return nil
}

// A Subscribe the coordinator has ACCEPTED is registered, whatever the
// caller's context does next. A caller told ctx.Err() at that point believes
// there is no subscription while the coordinator keeps one, filling its
// buffer every cycle until Stop (review r3 #2). So the coordinator never ends
// up with a subscriber nobody holds: the caller either gets the subscription
// or nothing was registered. Unsubscribe is the mirror: told "cancelled", a
// caller would believe a subscription it had just lost was still live.
func TestASubscribeWhoseCallerGivesUpAfterAcceptanceLeavesNoOrphan(t *testing.T) {
	h := start(t, Config{Root: ports.Root{Sessions: mapTree("/s", fstest.MapFS{})}})
	want := h.stats().Subscribers
	for i := 0; i < 200; i++ {
		sub, err := h.p.Subscribe(newLateCancel(), "late", 1)
		if err != nil {
			if got := h.stats().Subscribers; got != want {
				t.Fatalf("attempt %d: Subscribe returned %v and the coordinator holds %d subscribers, want %d: "+
					"a subscriber nobody holds", i, err, got, want)
			}
			continue
		}
		if got := h.stats().Subscribers; got != want+1 {
			t.Fatalf("attempt %d: Subscribe succeeded and the coordinator holds %d subscribers, want %d", i, got, want+1)
		}
		err = h.p.Unsubscribe(newLateCancel(), sub)
		open := true // no cycle runs here, so an open channel is empty
		select {
		case _, ok := <-sub.C():
			open = ok
		default:
		}
		if err != nil && !open {
			t.Fatalf("attempt %d: Unsubscribe returned %v, but the subscription was removed and closed", i, err)
		}
		if open {
			t.Fatalf("attempt %d: Unsubscribe returned %v and the subscription still delivers", i, err)
		}
		if got := h.stats().Subscribers; got != want {
			t.Fatalf("attempt %d: after Unsubscribe the coordinator holds %d subscribers, want %d", i, got, want)
		}
	}
}
