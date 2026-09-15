package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// okOp is an op that answers with what it was called with, so a test can tell
// that the args and the Caller reached it.
func okOp(class protocol.OpClass) Op {
	return Op{
		Spec: protocol.OpSpec{Name: "x", Description: "d", Class: class},
		Fn: func(ctx context.Context, c Caller, args json.RawMessage) (map[string]any, error) {
			return map[string]any{"caller": c.Conn, "args": string(args)}, nil
		},
	}
}

func named(name string, class protocol.OpClass, fn OpFunc) Op {
	if fn == nil {
		fn = func(context.Context, Caller, json.RawMessage) (map[string]any, error) { return nil, nil }
	}
	return Op{Spec: protocol.OpSpec{Name: name, Description: name, Class: class}, Fn: fn}
}

// TestRegistryNamesAreUniqueAndSourced covers the whole of the registry
// contract that a plugin can break: a name is claimed once, a batch is
// all-or-nothing, the source is magmux's to stamp, and rev moves whenever the
// list a client cached stops being the list magmux has.
func TestRegistryNamesAreUniqueAndSourced(t *testing.T) {
	h := New()
	if _, rev := h.Ops(); rev != 0 {
		t.Fatalf("a fresh hub is at rev %d, want 0", rev)
	}

	if err := h.Register("magmux", named("list", protocol.ClassRead, nil), named("send", protocol.ClassControl, nil)); err != nil {
		t.Fatalf("register built-ins: %v", err)
	}
	specs, rev := h.Ops()
	if rev != 1 {
		t.Errorf("rev = %d after one registration, want 1: rev is the version of the LIST, not a count of ops", rev)
	}
	if len(specs) != 2 || specs[0].Name != "list" || specs[1].Name != "send" {
		t.Fatalf("Ops = %+v, want list then send (ordered by name, because this is a wire payload)", specs)
	}
	for _, s := range specs {
		if s.Source != "magmux" {
			t.Errorf("op %q has source %q; the registry stamps it so a registrant cannot claim to be somebody else", s.Name, s.Source)
		}
		if string(s.Schema) != `{"type":"object"}` {
			t.Errorf("op %q schema = %s, want the filled-in empty object schema (a nil one marshals as null, which is not a schema)", s.Name, s.Schema)
		}
	}

	// A second source cannot take a name that is taken, and says who has it.
	err := h.Register("ticket", named("send", protocol.ClassControl, nil))
	if err == nil {
		t.Fatal("a plugin re-registered an existing op name; that would shadow a built-in verb")
	}
	if !strings.Contains(err.Error(), "magmux") {
		t.Errorf("duplicate error %q does not name the current owner", err)
	}
	if protocol.CodeOf(err) != protocol.CodeBadRequest {
		t.Errorf("duplicate registration code = %q, want bad_request", protocol.CodeOf(err))
	}

	// All-or-nothing: the good op in a rejected batch must not be registered.
	err = h.Register("ticket", named("run_ticket", protocol.ClassControl, nil), named("list", protocol.ClassRead, nil))
	if err == nil {
		t.Fatal("a batch with a duplicate was accepted")
	}
	if _, ok := h.Spec("run_ticket"); ok {
		t.Error("a rejected batch half-registered: run_ticket exists, so the plugin advertises ops magmux disagrees with")
	}
	if _, rev := h.Ops(); rev != 1 {
		t.Errorf("rev = %d after two refused registrations, want 1", rev)
	}

	// Malformed specs.
	for _, bad := range []struct {
		why string
		op  Op
	}{
		{"no name", Op{Spec: protocol.OpSpec{Class: protocol.ClassRead}, Fn: okOp(protocol.ClassRead).Fn}},
		{"no func", Op{Spec: protocol.OpSpec{Name: "nofn", Class: protocol.ClassRead}}},
		{"bad class", named("weird", protocol.OpClass("admin"), nil)},
	} {
		if err := h.Register("ticket", bad.op); err == nil {
			t.Errorf("registered an op with %s", bad.why)
		}
	}

	// Unregister takes exactly one source's ops.
	if err := h.Register("ticket", named("run_ticket", protocol.ClassControl, nil)); err != nil {
		t.Fatalf("register plugin op: %v", err)
	}
	if n := h.UnregisterSource("ticket"); n != 1 {
		t.Errorf("UnregisterSource removed %d ops, want 1", n)
	}
	if _, ok := h.Spec("list"); !ok {
		t.Error("unregistering a plugin took a built-in with it")
	}
	if n := h.UnregisterSource("ticket"); n != 0 {
		t.Errorf("a second UnregisterSource removed %d ops, want 0", n)
	}
	specs, rev = h.Ops()
	if rev != 3 {
		t.Errorf("rev = %d, want 3 (register, register, unregister); an unregister that does nothing must not move it", rev)
	}
	if len(specs) != 2 {
		t.Errorf("Ops = %+v, want the two built-ins", specs)
	}
}

// TestCallMapsErrors pins the one vocabulary. Every refusal the hub itself
// makes is a protocol code, and an error the op returns is passed through
// exactly as it was — which is what keeps a verb's reply bytes on the `call`
// path identical to its reply bytes on the socket.
func TestCallMapsErrors(t *testing.T) {
	h := New()
	boom := protocol.Errf(protocol.CodeNoSuchPane, "no pane 9 (it may have been closed)")
	if err := h.Register("magmux",
		named("capture", protocol.ClassRead, func(context.Context, Caller, json.RawMessage) (map[string]any, error) {
			return nil, boom
		}),
		named("focus", protocol.ClassControl, func(context.Context, Caller, json.RawMessage) (map[string]any, error) {
			return nil, errors.New("something we never classified")
		}),
		okOp(protocol.ClassRead),
	); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	c := Caller{Transport: "socket", Conn: "sock#1"}

	if _, err := h.Call(ctx, c, "nope", nil); protocol.CodeOf(err) != protocol.CodeUnknownVerb {
		t.Errorf("unknown op gave %v (code %q), want unknown_verb", err, protocol.CodeOf(err))
	}
	if _, err := h.Call(ctx, c, "capture", nil); !errors.Is(err, boom) {
		t.Errorf("an op's own error was not passed through: got %v", err)
	}
	if _, err := h.Call(ctx, c, "focus", nil); protocol.CodeOf(err) != protocol.CodeInternal {
		t.Errorf("an unclassified error read as %q, want internal", protocol.CodeOf(err))
	}

	res, err := h.Call(ctx, c, "x", json.RawMessage(`{"pane":3}`))
	if err != nil {
		t.Fatalf("x: %v", err)
	}
	if res["args"] != `{"pane":3}` || res["caller"] != "sock#1" {
		t.Errorf("op saw args=%v caller=%v; both must reach it verbatim", res["args"], res["caller"])
	}

	// After Quiesce every call is the same refusal the socket gives before the
	// layout exists.
	h.Quiesce(10 * time.Millisecond)
	if _, err := h.Call(ctx, c, "x", nil); protocol.CodeOf(err) != protocol.CodeNotReady {
		t.Errorf("call after Quiesce gave %q, want not_ready", protocol.CodeOf(err))
	}
}

// TestReadOnlyCallerIsForbidden is the view token's whole enforcement point. A
// pane is a shell, so anything but read has to be refused on the CLASS rather
// than on a list somebody maintains by hand.
func TestReadOnlyCallerIsForbidden(t *testing.T) {
	h := New()
	if err := h.Register("magmux",
		named("capture", protocol.ClassRead, nil),
		named("send", protocol.ClassControl, nil),
		named("tint", protocol.ClassDisplay, nil),
		named("input", protocol.ClassInput, nil),
	); err != nil {
		t.Fatalf("register: %v", err)
	}
	viewer := Caller{Transport: "ws", Conn: "ws#2", ReadOnly: true}
	full := Caller{Transport: "ws", Conn: "ws#3"}

	if _, err := h.Call(context.Background(), viewer, "capture", nil); err != nil {
		t.Errorf("a read op was refused to a viewer: %v", err)
	}
	for _, op := range []string{"send", "tint", "input"} {
		_, err := h.Call(context.Background(), viewer, op, nil)
		if protocol.CodeOf(err) != protocol.CodeForbidden {
			t.Errorf("viewer calling %q got %q, want forbidden", op, protocol.CodeOf(err))
		}
		if _, err := h.Call(context.Background(), full, op, nil); err != nil {
			t.Errorf("a full caller was refused %q: %v", op, err)
		}
	}
}

// TestQuiesceCancelsInFlightCalls: the run ctx of an op that is already
// running is cancelled, and Quiesce waits for it — bounded, so an op that
// ignores cancellation cannot turn a wrong answer into no answer.
func TestQuiesceCancelsInFlightCalls(t *testing.T) {
	h := New()
	running, cancelled := make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err := h.Register("magmux", named("slow", protocol.ClassControl,
		func(ctx context.Context, _ Caller, _ json.RawMessage) (map[string]any, error) {
			once.Do(func() { close(running) })
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		})); err != nil {
		t.Fatalf("register: %v", err)
	}
	go h.Call(context.Background(), Caller{}, "slow", nil) //nolint:errcheck // the call's own error is not what this test is about
	<-running

	start := time.Now()
	h.Quiesce(2 * time.Second)
	select {
	case <-cancelled:
	default:
		t.Fatal("Quiesce returned without the in-flight op's ctx being cancelled")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Quiesce took %v; it must return as soon as the op does", elapsed)
	}
}

// TestQuiesceIsBounded: an op that ignores its ctx cannot stall teardown.
func TestQuiesceIsBounded(t *testing.T) {
	h := New()
	release := make(chan struct{})
	defer close(release)
	if err := h.Register("magmux", named("stuck", protocol.ClassControl,
		func(context.Context, Caller, json.RawMessage) (map[string]any, error) {
			<-release
			return nil, nil
		})); err != nil {
		t.Fatalf("register: %v", err)
	}
	started := make(chan struct{})
	go func() {
		close(started)
		h.Call(context.Background(), Caller{}, "stuck", nil) //nolint:errcheck // abandoned on purpose
	}()
	<-started
	// Give the call a moment to be in flight; the bound below is what is
	// actually under test, and it holds either way.
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	h.Quiesce(100 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Quiesce waited %v for an op that ignores cancellation; the grace is a bound, not a hope", elapsed)
	}
}
