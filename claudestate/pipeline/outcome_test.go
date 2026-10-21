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
