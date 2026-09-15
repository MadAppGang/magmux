package firebase

// The inbound half: an SSE subscription on `commands`, six checks, and an
// at-most-once execution.
//
// At-most-once is the whole design, and it is worth saying what it costs and
// what it buys. Before any op that is not class read, magmux writes
// `results/{pushId} = {state:"claimed"}` and WAITS for RTDB to acknowledge it.
// That is a round trip in front of every command, which is the cost. What it
// buys is that there is no state in which magmux runs an op and no durable
// record of it exists: a crash before the claim leaves a command that never
// ran, and a crash after it leaves a claim that reads "this ran at most once
// and will never run again". The alternative — write the result afterwards — has
// a window in which the op has landed and the database has never heard of it,
// and a client that retried into that window would type an instruction twice.
//
// Across a restart nothing replays at all, and that is by construction rather
// than by bookkeeping: `sid` carries the process start time, so a restarted
// magmux listens on a different `commands` node and never reads, runs or
// rewrites the old one.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/sse"
)

const (
	// claimBound is how long the durable claim may take. It is short because
	// nothing has happened yet and a slow claim is a slow database: the command
	// gets `not_ready` and the client retries with a new nonce.
	claimBound = 5 * time.Second
	// executors is the concurrency for ops that are not delivered to a pane.
	// Pane-bound work is ordered by its lane and does not count against it.
	executors = 4
	// resultRing is how many COMPLETED results are kept. A `claimed` entry is
	// never trimmed: it is the only record that a command may have run, and
	// deleting it would turn "outcome unknown" into "never happened".
	resultRing = 100
	// streamSettled is how long a stream must survive before the reconnect
	// backoff resets. Shorter than this and a reconnect loop against a broken
	// endpoint would look like a healthy stream that keeps ending.
	streamSettled = 30 * time.Second
)

// sseEnvelope is the body of an RTDB `put` or `patch` event.
type sseEnvelope struct {
	Path string          `json:"path"`
	Data json.RawMessage `json:"data"`
}

// listen keeps a command stream open, forever, with backoff between attempts.
func (a *Adapter) listen(ctx context.Context) {
	var bo backoff
	for ctx.Err() == nil {
		start := time.Now()
		err := a.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > streamSettled {
			// The stream WORKED and then ended. That is an ordinary
			// long-connection event, not a failing endpoint, so the next
			// attempt starts from one second again.
			bo.reset()
		}
		wait := bo.next()
		if err != nil {
			a.logf("command stream ended (%v); reconnecting in %s", err, wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// listenOnce opens one stream and reads it until it ends.
func (a *Adapter) listenOnce(ctx context.Context) error {
	resp, err := a.c.stream(ctx, a.sess+"/commands")
	if err != nil {
		if he, ok := asHTTPError(err); ok && he.unauthorized() {
			a.c.tokens.Invalidate()
		}
		return err
	}
	defer resp.Body.Close()
	r := sse.NewReader(resp.Body)
	for {
		ev, err := r.Next()
		if err != nil {
			return err
		}
		switch ev.Name {
		case "keep-alive":
			// The stream is alive and nothing changed. RTDB sends these so a
			// proxy does not reap an idle connection.
		case "cancel":
			// RTDB no longer wants to serve this listener — most often because
			// the rules changed under it. Reconnecting is the documented
			// response.
			return errors.New("server cancelled the listener")
		case "auth_revoked":
			// The credential expired or was revoked. Drop the cached token so
			// the reconnect signs a fresh assertion.
			a.c.tokens.Invalidate()
			return errors.New("auth revoked")
		case "put", "patch":
			a.onStreamData(ctx, ev.Data)
		}
	}
}

// onStreamData routes one `put` or `patch` payload.
//
// The initial `put` at `/` is the BACKLOG: everything already sitting under
// `commands` when the listener attached. Later events at `/{pushId}` are single
// commands. A `patch` at `/` is a set of children, which is the same shape as
// the backlog and is handled identically.
func (a *Adapter) onStreamData(ctx context.Context, payload []byte) {
	var env sseEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if isJSONNull(env.Data) {
		// magmux's own delete, echoed back to its own listener. It is ignored,
		// never schema-checked and never logged as forged — an adapter that
		// reported its own housekeeping as an attack would cry wolf on every
		// single command it completed.
		return
	}
	path := strings.Trim(env.Path, "/")
	if path == "" {
		var batch map[string]json.RawMessage
		if err := json.Unmarshal(env.Data, &batch); err != nil {
			return
		}
		// pushId order. Firebase push keys sort lexicographically in creation
		// order, so this is submission order, and it is the order the lanes and
		// the claims inherit.
		ids := make([]string, 0, len(batch))
		for id := range batch {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			a.handle(ctx, id, batch[id])
		}
		return
	}
	if strings.Contains(path, "/") {
		// A write BELOW a command — `commands/{pushId}/sig`, say. A command is
		// created whole by the rules' .validate, so a deeper path is either
		// tampering or a client doing something unsupported. Either way there
		// is no whole command here to verify.
		return
	}
	a.handle(ctx, path, env.Data)
}

// handle verifies one command and places it for execution.
//
// It runs on the LISTENER goroutine and must not block: everything it does is a
// hash, a map write and an enqueue. The claim and the op both happen later, on
// the lane or on an executor.
func (a *Adapter) handle(ctx context.Context, pushId string, raw json.RawMessage) {
	if !sanitizeKey(pushId) {
		// The pushId becomes a path segment in `results/{pushId}`. One that
		// could hold a `/` would write into another node entirely, so this one
		// is not answered at all — answering it would mean composing the very
		// path that is unsafe.
		a.logf("ignoring a command whose key %q is not a legal RTDB key", pushId)
		return
	}
	if isJSONNull(raw) || !a.claimHandled(pushId) {
		// Either magmux's own delete, or a command this process has already
		// dealt with — an SSE reconnect replays the backlog, and a command that
		// was claimed once must never be claimed twice.
		return
	}

	var cmd command
	if err := json.Unmarshal(raw, &cmd); err != nil {
		a.reject(pushId, protocol.Errf(protocol.CodeBadRequest, "the command is not an object"))
		return
	}
	if _, err := a.verify(&cmd, time.Now()); err != nil {
		a.reject(pushId, err)
		return
	}

	args := json.RawMessage(cmd.Args)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	spec, ok := a.hub.Spec(cmd.Op)
	if !ok {
		a.reject(pushId, protocol.Errf(protocol.CodeUnknownVerb, "unknown op %q", cmd.Op))
		return
	}

	pane, lane, err := a.hub.LaneKey(cmd.Op, args)
	if lane {
		if err != nil {
			a.reject(pushId, err)
			return
		}
		a.deliver(pushId, pane, &cmd, spec, args)
		return
	}
	a.execute(ctx, pushId, &cmd, spec, args)
}

// deliver puts a pane-bound command on that pane's lane.
//
// The claim is INSIDE the item, so two commands for one pane claim in the order
// the lane runs them — which is pushId order — and a database that would have
// acknowledged the second claim first cannot reorder anything, because the
// second claim is not even sent until the first item is done.
func (a *Adapter) deliver(pushId string, pane int, cmd *command, spec protocol.OpSpec, args json.RawMessage) {
	c := *cmd
	err := a.sub.Deliver(pane, hub.LaneItem{
		Run: func(runCtx context.Context) {
			a.run(runCtx, pushId, &c, spec, args)
		},
		Discard: func() {
			// Quiesce dropped it before it ran, so it was never claimed and
			// never happened. Saying so is the whole point of Discard.
			a.reject(pushId, protocol.Errf(protocol.CodeNotReady,
				"the command was discarded unrun; magmux is shutting down"))
		},
		Size: len(args) + 64,
	})
	if err != nil {
		a.reject(pushId, err)
	}
}

// execute runs a command that is not delivered to a pane, on one of four
// executor slots.
func (a *Adapter) execute(ctx context.Context, pushId string, cmd *command, spec protocol.OpSpec, args json.RawMessage) {
	c := *cmd
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.sem <- struct{}{}:
		case <-ctx.Done():
			a.reject(pushId, protocol.Errf(protocol.CodeNotReady, "magmux is shutting down"))
			return
		}
		defer func() { <-a.sem }()
		a.run(ctx, pushId, &c, spec, args)
	}()
}

// run is the claim and the op, in that order, for one command.
func (a *Adapter) run(ctx context.Context, pushId string, cmd *command, spec protocol.OpSpec, args json.RawMessage) {
	if spec.Class != protocol.ClassRead {
		if err := a.claim(ctx, pushId); err != nil {
			// NOTHING ran. The command keeps its place in the database and its
			// nonce is spent, so the client retries with a new one rather than
			// wondering whether the first attempt half-happened.
			a.reject(pushId, protocol.Errf(protocol.CodeNotReady,
				"the command could not be claimed, so it was not run: %v", err))
			return
		}
	}
	caller := hub.Caller{Transport: "firebase", Conn: pushId, Client: cmd.UID}
	result, err := a.hub.Call(ctx, caller, cmd.Op, args)
	a.writeResult(pushId, result, err)
}

// claim writes the durable record and waits for RTDB to acknowledge it.
//
// It runs on the item's own ctx, so Quiesce aborts a claim in flight and the op
// it was for never runs. It is its own PATCH rather than a line in the flusher's
// batch because the flusher is asynchronous by design and a claim is the one
// write whose ACKNOWLEDGEMENT is the precondition for a side effect.
func (a *Adapter) claim(ctx context.Context, pushId string) error {
	cctx, cancel := context.WithTimeout(ctx, claimBound)
	defer cancel()
	return a.c.patch(cctx, a.sess, map[string]any{
		"results/" + pushId: map[string]any{"state": "claimed", "at": serverTimestamp()},
	})
}

// reject answers a command that will not run. It is writeResult with a nil
// result, named separately because the two read very differently at the call
// site.
func (a *Adapter) reject(pushId string, err error) {
	a.writeResult(pushId, nil, err)
}

// writeResult records the outcome and deletes the command, in the flusher's
// next batch — which is ONE PATCH, so the result and the delete land together
// or not at all.
//
// A completed result REPLACES the claim that preceded it, which is what makes
// the claim a transient state rather than a growing record.
func (a *Adapter) writeResult(pushId string, result map[string]any, err error) {
	rec := map[string]any{"state": "done", "ok": err == nil, "at": serverTimestamp()}
	if err != nil {
		rec["error"] = err.Error()
		rec["code"] = protocol.CodeOf(err)
	} else if result != nil {
		rec["result"] = jsonString(result)
	}
	a.mirror.SetPath("results/"+pushId, rec)
	a.mirror.SetPath("commands/"+pushId, nil)
	a.trimResults(pushId)
}

// trimResults keeps the completed ring at its bound. Only completed results are
// in the list, so a claim can never be what falls off the end.
func (a *Adapter) trimResults(pushId string) {
	a.mu.Lock()
	a.completed = append(a.completed, pushId)
	var old string
	if len(a.completed) > resultRing {
		old = a.completed[0]
		a.completed = a.completed[1:]
	}
	a.mu.Unlock()
	if old != "" {
		a.mirror.SetPath("results/"+old, nil)
	}
}

// claimHandled records that this process has taken responsibility for a pushId
// and reports whether it was the first to do so.
//
// It is the SAME-PROCESS half of at-most-once: an SSE reconnect replays the
// backlog, and a command already claimed or running must not be started again.
// The cross-process half needs nothing, because a restart gets a new sid.
func (a *Adapter) claimHandled(pushId string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.handled[pushId] {
		return false
	}
	a.handled[pushId] = true
	return true
}

func isJSONNull(b []byte) bool {
	return len(b) == 0 || string(trimSpace(b)) == "null"
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\n' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}
