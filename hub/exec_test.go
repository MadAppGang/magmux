package hub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

type ctxKey struct{}

// TestExecContextCancelledOnlyByQuiesce is the whole contract of ExecContext:
// a side effect an op has STARTED is not the caller's to cut short. The
// caller's values reach it; the caller's cancel and deadline do not; Quiesce
// does; Quiesce's grace waits for one that is still held; and one asked for
// after Quiesce is born cancelled.
func TestExecContextCancelledOnlyByQuiesce(t *testing.T) {
	t.Run("the caller's values reach it, its cancel and deadline do not", func(t *testing.T) {
		h := New()
		caller, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxKey{}, "v"), 10*time.Millisecond)
		exec, release := h.ExecContext(caller)
		defer release()

		cancel()
		<-caller.Done()
		time.Sleep(20 * time.Millisecond) // past the caller's deadline too
		if err := exec.Err(); err != nil {
			t.Fatalf("the exec context died with its caller: %v", err)
		}
		if _, ok := exec.Deadline(); ok {
			t.Error("the exec context inherited the caller's deadline")
		}
		if exec.Value(ctxKey{}) != "v" {
			t.Error("the exec context lost the caller's values")
		}
	})

	t.Run("Quiesce cancels it", func(t *testing.T) {
		h := New()
		exec, release := h.ExecContext(context.Background())
		go func() {
			<-exec.Done()
			release()
		}()
		h.Quiesce(2 * time.Second)
		if !errors.Is(exec.Err(), context.Canceled) {
			t.Fatalf("after Quiesce the exec context is %v, want cancelled", exec.Err())
		}
	})

	t.Run("Quiesce's grace waits for an unreleased one", func(t *testing.T) {
		h := New()
		exec, release := h.ExecContext(context.Background())
		hold := 150 * time.Millisecond
		go func() {
			<-exec.Done()
			time.Sleep(hold) // the side effect winding down after its cancel
			release()
		}()
		start := time.Now()
		h.Quiesce(5 * time.Second)
		if elapsed := time.Since(start); elapsed < hold {
			t.Errorf("Quiesce returned after %v, before the held exec context was released (%v)", elapsed, hold)
		}
	})

	t.Run("Quiesce's grace still bounds a release that never comes", func(t *testing.T) {
		h := New()
		_, release := h.ExecContext(context.Background())
		defer release()
		start := time.Now()
		h.Quiesce(100 * time.Millisecond)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("Quiesce waited %v for an exec context that was never released", elapsed)
		}
	})

	t.Run("after Quiesce it is born cancelled", func(t *testing.T) {
		h := New()
		h.Quiesce(time.Second)
		exec, release := h.ExecContext(context.Background())
		defer release()
		if !errors.Is(exec.Err(), context.Canceled) {
			t.Fatalf("an exec context asked for after Quiesce is %v, want cancelled", exec.Err())
		}
	})

	t.Run("release is idempotent and leaves nothing registered", func(t *testing.T) {
		h := New()
		_, release := h.ExecContext(context.Background())
		release()
		release()
		h.mu.RLock()
		n := len(h.calls)
		h.mu.RUnlock()
		if n != 0 {
			t.Errorf("%d cancel funcs still registered after release", n)
		}
		// A double Done on the WaitGroup would have panicked above; a missing
		// one would hang here.
		done := make(chan struct{})
		go func() { h.Quiesce(time.Second); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Quiesce hung on a released exec context")
		}
	})
}

// TestDefaultWaitIsMaxOfBaseAndBudget: an op with a Budget is waited for at
// least as long as its own bound, and never less than the transport's default.
// An op without one, an unknown op, and a Budget that returns 0 all get the
// transport's default unchanged.
func TestDefaultWaitIsMaxOfBaseAndBudget(t *testing.T) {
	h := New()
	noop := func(ctx context.Context, c Caller, args json.RawMessage) (map[string]any, error) { return nil, nil }
	var seen json.RawMessage
	err := h.Register(protocol.SourceBuiltin,
		Op{Spec: protocol.OpSpec{Name: "plain", Class: protocol.ClassRead}, Fn: noop},
		Op{Spec: protocol.OpSpec{Name: "long", Class: protocol.ClassControl}, Fn: noop,
			Budget: func(args json.RawMessage) time.Duration {
				seen = args
				var a struct {
					Ms int `json:"ms"`
				}
				_ = json.Unmarshal(args, &a)
				return time.Duration(a.Ms) * time.Millisecond
			}},
	)
	if err != nil {
		t.Fatal(err)
	}
	base := 30 * time.Second
	cases := []struct {
		name string
		op   string
		args string
		want time.Duration
	}{
		{"no budget", "plain", `{}`, base},
		{"unknown op", "nosuch", `{}`, base},
		{"budget below base", "long", `{"ms":1000}`, base},
		{"budget of zero", "long", `{"ms":0}`, base},
		{"budget above base", "long", `{"ms":45000}`, 45 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := h.DefaultWait(c.op, json.RawMessage(c.args), base); got != c.want {
				t.Errorf("DefaultWait(%s, %s) = %v, want %v", c.op, c.args, got, c.want)
			}
		})
	}
	if string(seen) != `{"ms":45000}` {
		t.Errorf("the Budget hook saw args %s; it must be handed the call's own args", seen)
	}
	// The hook is hub-internal: it is not part of the wire spec.
	specs, _ := h.Ops()
	for _, s := range specs {
		b, _ := json.Marshal(s)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for k := range m {
			if k == "budget" || k == "Budget" {
				t.Errorf("op %s advertises its budget hook on the wire: %s", s.Name, b)
			}
		}
	}
}

// TestInFlightCapIsBusyInFlight: the per-Sub call cap is a busy too, and says
// which one.
func TestInFlightCapIsBusyInFlight(t *testing.T) {
	h := New()
	block := make(chan struct{})
	defer close(block)
	if err := h.Register(protocol.SourceBuiltin, Op{
		Spec: protocol.OpSpec{Name: "slow", Class: protocol.ClassRead},
		Fn: func(ctx context.Context, c Caller, args json.RawMessage) (map[string]any, error) {
			<-block
			return nil, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	sub := h.Session(Caller{Conn: "a"}, newFakeSink(), nil)
	sub.Start(nil)
	defer sub.Close("done")

	errs := make(chan error, maxInFlightCalls+1)
	for i := 0; i < maxInFlightCalls+1; i++ {
		sub.Call(context.Background(), "slow", nil, func(_ map[string]any, err error) []byte {
			errs <- err
			return nil
		})
	}
	select {
	case err := <-errs:
		if protocol.CodeOf(err) != protocol.CodeBusy || protocol.ReasonOf(err) != protocol.ReasonInFlight {
			t.Fatalf("the call past the cap got %v (code %q, reason %q), want busy/in_flight",
				err, protocol.CodeOf(err), protocol.ReasonOf(err))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call past the cap was never refused")
	}
}
