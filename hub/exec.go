package hub

import (
	"context"
	"sync"
)

// ExecContext returns the context an op's SIDE EFFECT runs under once the op
// has started it: ctx's values, none of ctx's cancellation or deadline, and
// cancelled by Quiesce and by nothing else. While it is held, Quiesce's grace
// waits for it exactly as it waits for an op in flight. release must be called
// when the side effect is finished; it is idempotent. After Quiesce has begun
// it returns an already-cancelled context.
//
// It exists because a caller giving up must never truncate work that is half
// done. A REST or Firebase call reaches Hub.Call with the CALLER's own ctx, so
// without this a client timeout or a dropped connection would cancel a paced
// PTY delivery between two writes and leave half an instruction typed. A lane
// item's ctx is already Quiesce-only; ExecContext makes every path the same.
//
// The registration happens under h.mu in the same critical section as the
// closing check, exactly as Call's does, so no registration can follow Quiesce
// and the WaitGroup is safe to Wait on.
func (h *Hub) ExecContext(ctx context.Context) (exec context.Context, release func()) {
	exec, cancel := context.WithCancel(context.WithoutCancel(ctx))

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		cancel()
		return exec, func() {}
	}
	id := h.nextCall
	h.nextCall++
	h.calls[id] = cancel
	h.inflight.Add(1)
	h.mu.Unlock()

	var once sync.Once
	return exec, func() {
		once.Do(func() {
			cancel()
			h.mu.Lock()
			delete(h.calls, id)
			h.mu.Unlock()
			h.inflight.Done()
		})
	}
}
