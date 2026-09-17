package client

// The remote-control half of the client: the op table (`ops`), the generic
// `call`, the two watch verbs, and the per-session ring of plugin events.
//
// These are what a TRANSPORT needs, as opposed to what an agent needs. The nine
// pane verbs in client.go each model one thing an agent does; everything here
// is plumbing for a transport that re-exports magmux's own surface — `magmux
// mcp` turning plugin ops into MCP tools and panes into MCP resources is the
// only caller today, and the HTTP adapter would use the same four verbs if it
// did not already sit inside the process.
//
// One rule runs through all of it: NOTHING HERE DRIVES A SESSION. `ops`,
// `watch` and `unwatch` are outside magmux's isControllerVerb list, and `call`
// is a controller action only when the op's own class says so — so a client
// that reads panes and lists ops never appears in the control panel as a
// controller. That is a property of which verbs this file chose, and it is the
// whole reason a resource read goes through `call {op:"capture"}` rather than
// through the `capture` verb, which magmux does count as driving.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// Plugin-event ring bounds, per plugin per session.
//
// Two bounds rather than one because the two failure shapes are different: 200
// entries stops a chatty plugin from growing the ring without limit, and 256 KB
// stops a plugin emitting near-maximum events (64 KB each, which the host
// allows) from holding 12 MB of them. Whichever is hit first evicts.
const (
	pluginRingEvents = 200
	pluginRingBytes  = 256 << 10
)

// epochSeq numbers Sessions within this process, so a caller can tell a
// re-dialled session's ring from the one it replaced.
//
// It matters because a reconnect does NOT replay: events that happened while
// nobody was connected are gone, by design — they are notices, not a log. A
// consumer that kept `next` from the old connection and applied it to the new
// one would silently skip everything up to that number. The epoch changing is
// how it learns to start over.
var epochSeq atomic.Uint64

// PluginEvent is one `plugin` event as this session recorded it, with the
// session-local sequence number that lets a reader ask for what it has not seen.
//
// Data stays raw. A plugin's payload is the plugin's shape, and a client that
// re-marshalled it through map[string]any would reorder its keys and turn its
// integers into floats on the way to whoever actually reads it.
type PluginEvent struct {
	Seq   uint64          `json:"seq"`
	Event string          `json:"event"`
	Pane  *int            `json:"pane,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	At    string          `json:"at,omitempty"`
}

// pluginRing is one plugin's bounded event history on this connection.
type pluginRing struct {
	evs   []PluginEvent
	bytes int
}

func (r *pluginRing) push(e PluginEvent) {
	size := len(e.Data) + len(e.Event) + len(e.At) + 64
	r.evs = append(r.evs, e)
	r.bytes += size
	for len(r.evs) > pluginRingEvents || (r.bytes > pluginRingBytes && len(r.evs) > 1) {
		old := r.evs[0]
		r.bytes -= len(old.Data) + len(old.Event) + len(old.At) + 64
		// The backing array is dropped rather than shifted: a ring this small is
		// re-sliced far less often than it is read, and holding the old head
		// alive would keep its Data from being collected.
		r.evs = append([]PluginEvent(nil), r.evs[1:]...)
	}
}

// Epoch identifies this connection's ring. See epochSeq.
func (s *Session) Epoch() uint64 { return s.epoch }

// recordPluginEvent files one `plugin` broadcast.
//
// It runs on the reader goroutine, INSIDE ingest and therefore before the raw
// event hook fires. That order is load-bearing for `magmux mcp`: the hook is
// what turns the event into a `notifications/resources/updated`, and a client
// that read the resource the instant it saw the notification would otherwise
// find a ring that did not yet hold the event it was told about.
func (s *Session) recordPluginEvent(ev map[string]any) {
	name, _ := evStr(ev, "plugin")
	if name == "" {
		return
	}
	e := PluginEvent{}
	e.Event, _ = evStr(ev, "event")
	if idx, ok := evInt(ev, "pane"); ok {
		e.Pane = &idx
	}
	e.At, _ = evStr(ev, "at")
	if d, ok := ev["data"]; ok && d != nil {
		if raw, err := json.Marshal(d); err == nil {
			e.Data = raw
		}
	}

	s.evMu.Lock()
	defer s.evMu.Unlock()
	if s.evRings == nil {
		s.evRings = map[string]*pluginRing{}
	}
	r := s.evRings[name]
	if r == nil {
		r = &pluginRing{}
		s.evRings[name] = r
	}
	s.evSeq++
	e.Seq = s.evSeq
	r.push(e)
}

// PluginEvents returns everything this session has recorded for one plugin past
// `since`, and the sequence number to pass as `since` next time.
//
// `next` is derived from the SESSION's counter rather than from the returned
// slice, so a reader that catches up and then reads again cannot be handed the
// same events twice by a ring that has not moved.
func (s *Session) PluginEvents(plugin string, since uint64) ([]PluginEvent, uint64) {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	next := s.evSeq + 1
	r := s.evRings[plugin]
	if r == nil {
		return nil, next
	}
	out := make([]PluginEvent, 0, len(r.evs))
	for _, e := range r.evs {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out, next
}

// ── the transport verbs ─────────────────────────────────────────────────────

// Ops asks magmux for its op table and the revision of it.
//
// The revision is what a cache is keyed on: plugins register and die while
// magmux runs, and `ops_changed` carries the new rev so a client knows the list
// it holds has stopped being the list magmux has without diffing two arrays of
// schemas.
func (s *Session) Ops(ctx context.Context) ([]protocol.OpSpec, int, error) {
	res, err := s.request(ctx, map[string]any{"type": "ops"}, ReadTimeout)
	if err != nil {
		return nil, 0, err
	}
	// Re-marshalled through the generic reply map: `request` decodes every
	// reply into map[string]any, and an op's schema is raw JSON that must reach
	// the caller with its keys in the order the plugin wrote them.
	raw, err := json.Marshal(res["ops"])
	if err != nil {
		return nil, 0, fmt.Errorf("ops: %w", err)
	}
	var specs []protocol.OpSpec
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, 0, fmt.Errorf("ops: %w", err)
	}
	rev, _ := res["rev"].(float64)
	return specs, int(rev), nil
}

// Call runs one registered op — a built-in or a plugin's — and returns its
// result.
//
// The timeout is sent as well as waited on, so magmux's own budget for the call
// matches the caller's patience. Without that the two disagree in the worst
// direction: a client that gave up at 30 s leaves an op running for the
// registry's own default, and the pane keeps changing under a caller that has
// been told the request failed.
func (s *Session) Call(ctx context.Context, op string, args json.RawMessage, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = ReadTimeout
	}
	msg := map[string]any{"type": "call", "op": op, "timeoutMs": int(timeout / time.Millisecond)}
	if len(args) > 0 {
		msg["args"] = args
	}
	return s.request(ctx, msg, timeout)
}

// Watch subscribes this connection to one pane.
//
// Mode "notify" is the cheap half — a `changed` line and nothing else — and is
// what a client with its own way to read the pane wants. It is sent as the
// socket VERB rather than through Call because watch state belongs to the
// connection, and the verb is the shape every other magmux client uses.
func (s *Session) Watch(ctx context.Context, pane int, mode protocol.WatchMode, fps int) (protocol.WatchInfo, error) {
	msg := map[string]any{"type": "watch", "pane": pane}
	if mode != "" {
		msg["mode"] = string(mode)
	}
	if fps > 0 {
		msg["fps"] = fps
	}
	res, err := s.request(ctx, msg, ReadTimeout)
	if err != nil {
		return protocol.WatchInfo{}, err
	}
	var info protocol.WatchInfo
	raw, err := json.Marshal(res)
	if err != nil {
		return protocol.WatchInfo{}, err
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return protocol.WatchInfo{}, fmt.Errorf("watch: %w", err)
	}
	return info, nil
}

// Unwatch drops a watch. A pane that was never watched is not an error: the
// caller's intent — "do not send me this pane" — is satisfied either way.
func (s *Session) Unwatch(ctx context.Context, pane int) error {
	_, err := s.request(ctx, map[string]any{"type": "unwatch", "pane": pane}, ReadTimeout)
	return err
}
