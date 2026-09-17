package plugin

// Invocation: magmux asking a plugin to run one of its ops, and everything that
// can go wrong on the way back.
//
// The shape is a request/response over a stream that carries other traffic, so
// every invocation is a `call` id plus a channel, and EVERY exit from the wait
// removes the entry. There are four ways out and they mean four different
// things to the caller:
//
//	answered  — the plugin replied; its own code and message pass through
//	timeout   — the budget expired; the plugin is told to stop (invoke_cancel)
//	not_ready — magmux is shutting down, or the caller gave up; also cancelled
//	plugin_gone — the process or its connection went away with the call in flight
//
// The distinction between the last two matters more than it looks: `timeout`
// invites a retry with a longer budget, `plugin_gone` invites a look at the
// plugin's log, and `not_ready` invites neither because magmux is leaving.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// invoke runs one of a plugin's ops and waits for its answer.
//
// It holds NO lock while waiting — not the host's, not the plug's — so a plugin
// that takes fifteen minutes blocks nothing but its own caller's goroutine, and
// a second plugin's registration during that wait is unaffected.
func (h *Host) invoke(ctx context.Context, name, op string, c hub.Caller, args json.RawMessage) (map[string]any, error) {
	h.mu.Lock()
	p := h.plugins[name]
	closing := h.closing
	h.mu.Unlock()
	switch {
	case p == nil:
		return nil, protocol.Errf(protocol.CodePluginGone,
			"plugin %q is no longer connected, so %s cannot run", name, protocol.QualifiedOp(name, op))
	case closing:
		return nil, protocol.Errf(protocol.CodeNotReady, "magmux is shutting down and is not taking new requests")
	}

	budget := budgetOf(ctx)
	call, ch, err := p.begin()
	if err != nil {
		return nil, err
	}
	defer p.end(call)

	if err := p.conn.write(protocol.Invoke{
		Type: protocol.MsgInvoke,
		Call: call,
		Op:   op,
		Args: args,
		Caller: protocol.InvokeCaller{
			Transport: c.Transport,
			Conn:      c.Conn,
			Client:    c.Client,
			ReadOnly:  c.ReadOnly,
		},
		DeadlineMs: int(budget / time.Millisecond),
	}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case res := <-ch:
		return resultOf(name, op, res)
	case <-timer.C:
		p.cancel(call)
		return nil, protocol.Errf(protocol.CodeTimeout,
			"plugin op %s did not answer within %s", protocol.QualifiedOp(name, op), budget)
	case <-ctx.Done():
		// The caller stopped waiting, or Quiesce cancelled the run. Either way
		// the plugin is told, because work nobody will read is work a plugin
		// should be allowed to abandon.
		p.cancel(call)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, protocol.Errf(protocol.CodeTimeout,
				"plugin op %s did not answer in time", protocol.QualifiedOp(name, op))
		}
		return nil, protocol.Errf(protocol.CodeNotReady,
			"plugin op %s was cancelled before it answered", protocol.QualifiedOp(name, op))
	case <-p.dead:
		return nil, protocol.Errf(protocol.CodePluginGone,
			"plugin %q went away while %s was in flight", name, protocol.QualifiedOp(name, op))
	}
}

// budgetOf resolves how long to wait: the caller's remaining deadline, clamped
// to MaxTimeout, or DefaultTimeout when it named none.
//
// A caller whose deadline has already passed still gets a floor of one
// millisecond rather than a zero timer, so the invoke goes out and is cancelled
// rather than being refused by arithmetic — the plugin then learns the call
// existed at all, which is what makes its own logs make sense.
func budgetOf(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return DefaultTimeout
	}
	d := time.Until(deadline)
	switch {
	case d <= 0:
		return time.Millisecond
	case d > MaxTimeout:
		return MaxTimeout
	}
	return d
}

// resultOf turns a plugin's answer into an op result or a protocol error.
//
// A failure with no code is reported as `internal`: the plugin failed and did
// not say how, which is not something the caller can branch on, and inventing a
// more specific code would be magmux guessing on the plugin's behalf.
func resultOf(name, op string, res protocol.InvokeResult) (map[string]any, error) {
	if res.OK {
		return res.Result, nil
	}
	code := res.Code
	if code == "" {
		code = protocol.CodeInternal
	}
	msg := res.Error
	if msg == "" {
		msg = fmt.Sprintf("plugin op %s failed and reported no reason", protocol.QualifiedOp(name, op))
	}
	return res.Result, &protocol.Error{Code: code, Msg: msg}
}

// begin reserves a call slot and returns its id and answer channel.
func (p *plug) begin() (string, chan protocol.InvokeResult, error) {
	// Numbered before the lock: the counter is atomic precisely so this does
	// not have to reach for the host lock from inside the plug's.
	id := fmt.Sprintf("c%d", p.host.nextCall.Add(1))

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) >= MaxInFlight {
		return "", nil, protocol.Errf(protocol.CodeBusy,
			"plugin %q already has %d invocations in flight", p.name, MaxInFlight)
	}
	// Buffered: deliver must never block on a caller that has already given up,
	// and the entry it delivers into is removed by end() on every path.
	ch := make(chan protocol.InvokeResult, 1)
	p.calls[id] = ch
	return id, ch, nil
}

func (p *plug) end(call string) {
	p.mu.Lock()
	delete(p.calls, call)
	p.mu.Unlock()
}

// cancel tells the plugin to stop working on a call magmux has stopped waiting
// for. Best effort by design: a plugin that ignores it is not killed, it simply
// produces an answer nobody reads.
func (p *plug) cancel(call string) {
	_ = p.conn.write(protocol.InvokeCancel{Type: protocol.MsgInvokeCancel, Call: call})
}

// deliver hands one invoke_result to whoever is waiting for it.
func (p *plug) deliver(res protocol.InvokeResult) bool {
	p.mu.Lock()
	ch, ok := p.calls[res.Call]
	p.mu.Unlock()
	if !ok {
		return false // a late answer for an abandoned call
	}
	select {
	case ch <- res:
	default:
	}
	return true
}

// handleResult routes a plugin's answer.
func (c *Conn) handleResult(env protocol.Envelope, line []byte) {
	p := c.plug()
	if p == nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeForbidden,
			"this connection has not registered as a plugin, so it has no invocations to answer"))
		return
	}
	var res protocol.InvokeResult
	if err := json.Unmarshal(line, &res); err != nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBadRequest,
			"invoke_result: the message could not be decoded (%v)", err))
		return
	}
	if res.Call == "" {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBadRequest, "invoke_result needs a call id"))
		return
	}
	if !p.deliver(res) {
		// Not an error the plugin can act on — the caller gave up, or the call
		// timed out — but worth a line when someone is debugging a slow plugin.
		c.host.debugf("[plugin] %s answered call %s, which nobody was waiting for\n", p.name, res.Call)
	}
	c.reply(env.ID, map[string]any{"call": res.Call}, nil)
}

// ── events ──────────────────────────────────────────────────────────────────

// handleEvent forwards one plugin event to every subscriber, after three checks
// that each exist for a different reason.
//
//   - DECLARED. An event a plugin never declared is refused, because the
//     declaration is what makes the stream describable to a client that has
//     never heard of this plugin. It is a bad_request rather than a silent drop
//     so a plugin author finds out in development.
//   - SIZE. One line over MaxEventBytes is too_large. The plugin's own op is
//     where big payloads belong.
//   - RATE. Past MaxEventsPerSecond events are DROPPED and counted. This is the
//     only one that is not the plugin's mistake in kind — a busy plugin is
//     allowed to be busy — so it is answered `busy` rather than refused, and the
//     connection stays up in every case.
func (c *Conn) handleEvent(env protocol.Envelope, line []byte) {
	p := c.plug()
	if p == nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeForbidden,
			"this connection has not registered as a plugin, so it cannot emit plugin events"))
		return
	}
	if len(line) > MaxEventBytes {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeTooLarge,
			"plugin %q sent a %d byte event; the limit is %d — put the detail behind one of your own ops",
			p.name, len(line), MaxEventBytes))
		return
	}
	var ev protocol.PluginEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBadRequest,
			"plugin.event: the message could not be decoded (%v)", err))
		return
	}
	if !p.events[ev.Event] {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBadRequest,
			"plugin %q did not declare an event called %q; declare it in plugin.register",
			p.name, ev.Event))
		return
	}
	if !p.allowEvent() {
		c.host.debugf("[plugin] %s event %q dropped: over %d/s\n", p.name, ev.Event, MaxEventsPerSecond)
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBusy,
			"plugin %q is emitting more than %d events a second; this one was dropped",
			p.name, MaxEventsPerSecond))
		return
	}

	out := map[string]any{
		"type":   protocol.EventPlugin,
		"plugin": p.name,
		"event":  ev.Event,
		"at":     time.Now().UTC().Format(time.RFC3339),
	}
	if ev.Pane != nil {
		out["pane"] = *ev.Pane
	}
	if len(ev.Data) > 0 {
		out["data"] = ev.Data
	}
	c.host.publish(out)
	c.reply(env.ID, map[string]any{"event": ev.Event}, nil)
}

// allowEvent is the rate window: a plain fixed window rather than a token
// bucket, because the limit exists to stop a runaway loop rather than to shape
// traffic, and a window is one comparison and one counter.
func (p *plug) allowEvent() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if now.Sub(p.window) >= time.Second {
		p.window = now
		p.inWin = 0
	}
	if p.inWin >= MaxEventsPerSecond {
		p.drops++
		return false
	}
	p.inWin++
	return true
}

// ── controller snapshots ────────────────────────────────────────────────────

// handleSnapshot hands one observation to the pane it describes.
//
// Two checks, in two places, because they are two different questions. HERE:
// is this connection a registered plugin at all. THERE (Config.Snapshot, which
// is the multiplexer): does this plugin own that pane's controller. The second
// cannot be answered without the pane table, and the first must not be skipped
// on the way to it.
func (c *Conn) handleSnapshot(env protocol.Envelope, line []byte) {
	p := c.plug()
	if p == nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeForbidden,
			"this connection has not registered as a plugin, so it cannot report on a pane"))
		return
	}
	var snap protocol.ControllerSnapshot
	if err := json.Unmarshal(line, &snap); err != nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBadRequest,
			"controller.snapshot: the message could not be decoded (%v)", err))
		return
	}
	if snap.Pane == nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBadRequest,
			"controller.snapshot needs a pane: it is an observation ABOUT one"))
		return
	}
	if c.host.cfg.Snapshot == nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeUnsupported,
			"this magmux cannot take controller snapshots"))
		return
	}
	if err := c.host.cfg.Snapshot(p.name, snap); err != nil {
		c.reply(env.ID, nil, err)
		return
	}
	c.reply(env.ID, map[string]any{"pane": *snap.Pane, "state": snap.State}, nil)
}
