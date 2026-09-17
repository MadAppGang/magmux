package plugin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// Limits. Each one bounds a resource a plugin could otherwise spend without
// asking, and each is per PLUGIN rather than per magmux: one badly behaved
// plugin must not be able to silence another.
const (
	// MaxEventBytes bounds one plugin.event line. A plugin with something
	// bigger to say has somewhere to put it — its own `status` op, which a
	// client calls when it wants the detail — and an event stream is a notice
	// board, not a transport.
	MaxEventBytes = 64 << 10

	// MaxEventsPerSecond is the event rate. Past it events are DROPPED and
	// counted rather than queued: a queue would turn a loop in a plugin into
	// unbounded memory in magmux, and every subscriber would be reading
	// minutes-old news by the time it drained.
	MaxEventsPerSecond = 50

	// MaxInFlight is how many invocations one plugin may have outstanding.
	// Past it a caller gets `busy`, which is a true statement about the plugin
	// rather than a queue that hides how far behind it is.
	MaxInFlight = 64

	// DefaultTimeout is how long an invoke waits when the caller named no
	// budget; MaxTimeout is the ceiling on one it did name. They are the same
	// two numbers the `call` verb uses, deliberately: a plugin op reached
	// through `call` must not have two different deadlines depending on which
	// layer imposed one.
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 15 * time.Minute
)

// Config is everything the host needs from the process around it. Every field
// is optional except Hub: a host with no spawn directory still serves plugins
// an operator started by hand.
type Config struct {
	// Hub is the op registry and the event bus. Required.
	Hub *hub.Hub

	// Token is the session's own bearer token, which is what a DEVELOPER-RUN
	// plugin authenticates with (it is in the environment as MAGMUX_TOKEN).
	// Empty means only plugins magmux spawned can register, because they are
	// the only ones holding a token magmux issued.
	Token string

	// Env is the base environment for a spawned plugin — magmux passes the same
	// filtered environment a pane gets, so a plugin inherits the developer's
	// PATH and toolchain but none of the session's secrets. The host appends
	// MAGMUX_SOCK, MAGMUX_PLUGIN_TOKEN and MAGMUX_PLUGIN_ID.
	Env []string

	// LogDir and ID decide where a spawned plugin's stdout and stderr go:
	// {LogDir}/magmux-{ID}.plugin-{name}.log. A plugin never writes to magmux's
	// own stdout, which is holding a raw-mode terminal.
	LogDir string
	ID     string

	// Debug is magmux's debug log, or nil. Diagnostics that are nobody's
	// business at runtime — a dropped event, a malformed line — go here.
	Debug io.Writer

	// Snapshot is how a controller.snapshot reaches the pane it describes. The
	// host has already checked that the connection is a registered plugin; the
	// callback checks that THIS plugin owns THAT pane's controller, which is a
	// question only the multiplexer can answer. A nil callback refuses every
	// snapshot with unsupported.
	Snapshot func(plugin string, snap protocol.ControllerSnapshot) error

	// OnExit is called after a plugin's ops have been unregistered and its
	// in-flight calls failed. magmux uses it to degrade that plugin's
	// controllers to terminal-only observation: p.controller is write-once, so
	// the controller stays attached and simply stops being told anything.
	OnExit func(plugin string)
}

// Host is the plugin registry and the router. The zero value is not usable;
// call New. A nil *Host is usable and does nothing, which is what lets the
// multiplexer call into it unconditionally.
type Host struct {
	cfg Config

	// mu is plugMu in the lock order: plugMu -> hub.mu -> sub.mu. It guards the
	// registry below and is NEVER held across a Sink write, a hub call that
	// could reach back here, or a wait for a plugin's answer.
	mu      sync.Mutex
	plugins map[string]*plug
	procs   []*process
	closing bool

	// nextCall numbers invocations. Monotone for the life of the process, so a
	// late invoke_result for an abandoned call can never be matched against a
	// newer one. Atomic rather than guarded by mu, so reserving a call id does
	// not have to take the host lock from inside a plug's — which would be the
	// one edge that could invert plugMu -> plug.mu.
	nextCall atomic.Uint64
}

// New returns a host. cfg.Hub must be non-nil.
func New(cfg Config) *Host {
	return &Host{cfg: cfg, plugins: map[string]*plug{}}
}

// plug is one registered plugin: its identity, its connection, and its
// in-flight calls.
type plug struct {
	host    *Host
	name    string
	version string
	conn    *Conn
	events  map[string]bool
	// proc is the child magmux spawned for this plugin, or nil for one an
	// operator ran by hand and authenticated with the session token. It is how
	// a process exit finds the registration to withdraw.
	proc *process

	// dead is closed when the plugin goes away, so every waiting invoke wakes
	// at once rather than waiting out its own deadline.
	dead     chan struct{}
	deadOnce sync.Once

	// mu guards the call table and the rate window. It is a LEAF below
	// host.mu: the host resolves a plug under host.mu, releases it, and then
	// talks to the plug.
	mu     sync.Mutex
	calls  map[string]chan protocol.InvokeResult
	window time.Time
	inWin  int
	drops  int
}

// ── connections ─────────────────────────────────────────────────────────────

// Conn is one socket connection as the plugin host sees it: an identity for the
// logs, a way to send it a line, and the registration it may or may not have
// made.
//
// The multiplexer creates one per connection and hands the host every
// plugin-protocol line that arrives on it. Identity is RESOLVED through this
// object at every message rather than captured once, because a connection
// registers as a plugin after it — and its hub Sub — already exist.
type Conn struct {
	host *Host
	id   string
	send func([]byte)

	mu sync.Mutex
	p  *plug
}

// Conn opens the host's view of one connection. send queues one whole line to
// that connection and must not block; on the socket it is Sub.Send.
//
// A nil host returns a nil *Conn, and every method below is nil-safe, so the
// caller needs no branch.
func (h *Host) Conn(id string, send func([]byte)) *Conn {
	if h == nil {
		return nil
	}
	return &Conn{host: h, id: id, send: send}
}

// SetSend installs the queue this connection is written through. It is
// separate from Conn because of a chicken and egg: the socket adapter needs the
// Conn to build its hub Sub (the Sub resolves plugin identity through it), and
// the send function IS the Sub's. Called once, before any line is handled.
func (c *Conn) SetSend(send func([]byte)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.send = send
	c.mu.Unlock()
}

// writer returns the send function under the lock.
func (c *Conn) writer() func([]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.send
}

// Plugin is the registered name of this connection, or "" if it never
// registered. It is what Hub.Session's pluginOf resolves to, and it takes only
// this connection's own lock — no host lock and no hub lock — so it is safe to
// call from inside Sub.Caller.
func (c *Conn) Plugin() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p == nil {
		return ""
	}
	return c.p.name
}

// Close releases whatever this connection registered. It is idempotent and is
// called from the connection's own goroutine when it ends.
//
// A plugin's connection going away IS the plugin going away, whether or not its
// process is still alive: magmux cannot invoke anything it cannot reach, so
// leaving the ops registered would advertise ops that answer `plugin_gone`
// forever.
func (c *Conn) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	p := c.p
	c.p = nil
	c.mu.Unlock()
	if p != nil {
		c.host.unregister(p, "connection closed", nil)
	}
}

// Handle routes one plugin-protocol line. It reports whether the line WAS a
// plugin-protocol message, in which case the ordinary verb path must not also
// see it.
//
// It runs synchronously on the connection's reader goroutine, which is what
// makes registration ordered: plugin.register's reply is queued only after the
// registry write, so every later line on this connection already sees the
// registration.
func (c *Conn) Handle(env protocol.Envelope, line []byte) bool {
	if c == nil || !protocol.IsPluginMessage(env.Type) {
		return false
	}
	switch env.Type {
	case protocol.MsgPluginRegister:
		c.handleRegister(env, line)
	case protocol.MsgInvokeResult:
		c.handleResult(env, line)
	case protocol.MsgPluginEvent:
		c.handleEvent(env, line)
	case protocol.MsgControllerSnapshot:
		c.handleSnapshot(env, line)
	}
	return true
}

// plug returns this connection's registration, or nil.
func (c *Conn) plug() *plug {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.p
}

// reply queues one reply line, if the message asked for one. A plugin message
// without an id is answered with nothing at all, exactly like every other
// message on this socket.
func (c *Conn) reply(id json.RawMessage, result map[string]any, err error) {
	send := c.writer()
	if len(id) == 0 || send == nil {
		return
	}
	if line := replyBytes(id, result, err); len(line) > 0 {
		send(line)
	}
}

// write queues one message to this connection. It never blocks: the queue it
// hands to is the connection's Sub, which is bounded and closes a peer that
// stops reading rather than making a writer wait for it.
func (c *Conn) write(v any) error {
	if c == nil {
		return protocol.Errf(protocol.CodePluginGone, "the plugin's connection is gone")
	}
	send := c.writer()
	if send == nil {
		return protocol.Errf(protocol.CodePluginGone, "the plugin's connection is gone")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return protocol.Errf(protocol.CodeInternal, "encoding a message for the plugin: %v", err)
	}
	send(append(data, '\n'))
	return nil
}

// ── registration ────────────────────────────────────────────────────────────

// handleRegister validates a registration and, if it holds up, puts the
// plugin's ops in the op table.
//
// The order is the contract: the registry write happens BEFORE the reply is
// queued, so a plugin that sends `open_pane {controller:"self"}` on the very
// next line is already known to be a plugin. Nothing here is done optimistically
// — a registration that fails any check registers nothing at all, because a
// half-registered plugin would advertise an op list magmux disagrees with.
func (c *Conn) handleRegister(env protocol.Envelope, line []byte) {
	var reg protocol.PluginRegister
	if err := json.Unmarshal(line, &reg); err != nil {
		c.reply(env.ID, nil, protocol.Errf(protocol.CodeBadRequest,
			"plugin.register: the message could not be decoded (%v)", err))
		return
	}
	p, err := c.host.register(c, reg)
	if err != nil {
		c.reply(env.ID, nil, err)
		return
	}
	// Queued after the registry write above, never before it.
	c.reply(env.ID, map[string]any{
		"name": p.name,
		"rev":  c.host.cfg.Hub.Rev(),
	}, nil)
	c.host.publish(map[string]any{
		"type": protocol.EventOpsChanged,
		"rev":  c.host.cfg.Hub.Rev(),
	})
	c.host.debugf("[plugin] %s v%s registered %d op(s) on %s\n", p.name, p.version, len(reg.Ops), c.id)
}

// register is the whole of the admission decision.
func (h *Host) register(c *Conn, reg protocol.PluginRegister) (*plug, error) {
	if !protocol.ValidPluginName(reg.Name) {
		return nil, protocol.Errf(protocol.CodeBadRequest,
			"plugin name %q is not usable: it must match [a-z][a-z0-9-]{0,%d} — no underscore, "+
				"because a qualified op name splits at its first __",
			reg.Name, protocol.MaxPluginNameLen-1)
	}
	if reg.Name == protocol.SourceBuiltin {
		return nil, protocol.Errf(protocol.CodeBadRequest,
			"a plugin cannot be called %q: that name belongs to magmux's own ops", reg.Name)
	}
	if len(reg.Ops) == 0 {
		return nil, protocol.Errf(protocol.CodeBadRequest,
			"plugin %q registered no ops; a plugin that offers nothing has nothing to register", reg.Name)
	}
	for _, ev := range reg.Events {
		if !protocol.ValidEventName(ev) {
			return nil, protocol.Errf(protocol.CodeBadRequest,
				"plugin %q declared event %q, which is not a usable name ([a-z][a-z0-9_]{0,%d})",
				reg.Name, ev, protocol.MaxOpNameLen-1)
		}
	}

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, protocol.Errf(protocol.CodeNotReady, "magmux is shutting down and is not taking registrations")
	}
	pr, tokenOK := h.tokenOKLocked(reg.Token)
	if !tokenOK {
		h.mu.Unlock()
		return nil, protocol.Errf(protocol.CodeUnauthorized,
			"plugin %q presented no usable token; a plugin magmux spawned uses MAGMUX_PLUGIN_TOKEN, "+
				"and one you started yourself uses MAGMUX_TOKEN", reg.Name)
	}
	if prev, dup := h.plugins[reg.Name]; dup {
		h.mu.Unlock()
		return nil, protocol.Errf(protocol.CodeBadRequest,
			"plugin %q is already registered on %s", reg.Name, prev.conn.id)
	}
	if already := c.plug(); already != nil {
		h.mu.Unlock()
		return nil, protocol.Errf(protocol.CodeBadRequest,
			"this connection already registered as plugin %q; one plugin per connection", already.name)
	}

	p := &plug{
		host:    h,
		name:    reg.Name,
		version: reg.Version,
		conn:    c,
		proc:    pr,
		events:  map[string]bool{},
		dead:    make(chan struct{}),
		calls:   map[string]chan protocol.InvokeResult{},
	}
	for _, ev := range reg.Events {
		p.events[ev] = true
	}

	ops, err := h.opsFor(p, reg.Ops)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	// hub.Register is all-or-nothing and refuses a name a built-in or another
	// plugin already holds, so the op table cannot end up half this plugin's.
	if err := h.cfg.Hub.Register(reg.Name, ops...); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	h.plugins[reg.Name] = p
	h.mu.Unlock()

	c.mu.Lock()
	c.p = p
	c.mu.Unlock()
	// Outside the lock: a rename is filesystem work, and the plugin is already
	// registered and callable by the time it happens.
	h.nameLog(pr, p.name)
	return p, nil
}

// opsFor turns a registration's specs into hub ops, checking each name against
// the grammar and qualifying it with the plugin's own name.
func (h *Host) opsFor(p *plug, specs []protocol.OpSpec) ([]hub.Op, error) {
	out := make([]hub.Op, 0, len(specs))
	for _, spec := range specs {
		if !protocol.ValidOpName(spec.Name) {
			return nil, protocol.Errf(protocol.CodeBadRequest,
				"plugin %q declared op %q, which is not a usable name ([a-z][a-z0-9_]{0,%d})",
				p.name, spec.Name, protocol.MaxOpNameLen-1)
		}
		if !protocol.ValidClass(spec.Class) {
			return nil, protocol.Errf(protocol.CodeBadRequest,
				"plugin %q declared op %q with class %q; it must be read, control, display or input",
				p.name, spec.Name, spec.Class)
		}
		bare := spec.Name
		qualified := protocol.QualifiedOp(p.name, bare)
		// Source is stamped by the registry from the source argument, so
		// clearing it here is belt and braces: a plugin cannot claim to be
		// magmux even if it puts "magmux" on the wire.
		spec.Source = ""
		spec.Name = qualified
		out = append(out, hub.Op{
			Spec: spec,
			Fn: func(ctx context.Context, c hub.Caller, args json.RawMessage) (map[string]any, error) {
				return h.invoke(ctx, p.name, bare, c, args)
			},
		})
	}
	return out, nil
}

// tokenOKLocked checks a registration token against the one magmux issued to
// each spawned plugin and against the session token, and reports which child it
// belonged to (nil for the session token: that plugin is one an operator ran by
// hand, and magmux has no process of its own to reap).
//
// Every comparison is made before any result is read, and the loop does not
// break early, so "wrong length" is not measurably cheaper than "nearly right".
//
// Caller holds h.mu.
func (h *Host) tokenOKLocked(tok string) (*process, bool) {
	if tok == "" {
		return nil, false
	}
	ok := 0
	var match *process
	if h.cfg.Token != "" {
		ok |= subtle.ConstantTimeCompare([]byte(tok), []byte(h.cfg.Token))
	}
	for _, pr := range h.procs {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(pr.token)) == 1 {
			match = pr
			ok = 1
		}
	}
	return match, ok == 1
}

// ── death ───────────────────────────────────────────────────────────────────

// unregister is the one path a plugin leaves by, whichever end noticed first:
// its connection closed, or its process exited.
//
// The ORDER is what a client depends on. The ops go first, then ops_changed
// says so, then plugin_exited names the plugin — so a client that re-fetches
// `ops` on ops_changed can never be handed the dead plugin's ops again. The
// in-flight calls are failed last, because a call that returns before its op is
// gone could be retried straight into the same hole.
// code is the process's exit status, or nil when the plugin left by closing its
// connection — a live process with no socket is gone in every way that matters
// here, and reporting a status it does not have would be an invention.
func (h *Host) unregister(p *plug, why string, code *int) {
	if h == nil || p == nil {
		return
	}
	h.mu.Lock()
	if h.plugins[p.name] != p {
		h.mu.Unlock()
		return // already gone
	}
	delete(h.plugins, p.name)
	h.mu.Unlock()

	h.cfg.Hub.UnregisterSource(p.name)
	h.publish(map[string]any{"type": protocol.EventOpsChanged, "rev": h.cfg.Hub.Rev()})
	exited := map[string]any{
		"type":   protocol.EventPluginExited,
		"plugin": p.name,
		"reason": why,
	}
	if code != nil {
		exited["code"] = *code
	}
	h.publish(exited)

	// Every waiter wakes at once: an invoke that is 12 minutes into a 15-minute
	// budget must not sit out the rest of it for a process that has exited.
	p.deadOnce.Do(func() { close(p.dead) })
	h.debugf("[plugin] %s gone (%s)\n", p.name, why)

	if h.cfg.OnExit != nil {
		h.cfg.OnExit(p.name)
	}
}

// Names lists every registered plugin, for diagnostics and tests.
func (h *Host) Names() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.plugins))
	for name := range h.plugins {
		out = append(out, name)
	}
	return out
}

// ── plumbing ────────────────────────────────────────────────────────────────

// publish puts one event on the bus. Called with NO host lock held wherever the
// call site can manage it; the bus is a leaf either way.
func (h *Host) publish(ev map[string]any) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.cfg.Hub.Publish(append(data, '\n'))
}

func (h *Host) debugf(format string, a ...any) {
	if h == nil || h.cfg.Debug == nil {
		return
	}
	fmt.Fprintf(h.cfg.Debug, format, a...)
}

// replyBytes renders one reply line, in exactly the shape the socket's own
// replies take: {"type":"reply","id":…,"ok":…} plus either "result" or
// "code"+"error".
//
// It is a second implementation of that shape rather than a shared one, and
// that is the deliberate cost of the import direction: mux must not be imported
// here. The shape is pinned on both sides by tests.
func replyBytes(id json.RawMessage, result map[string]any, err error) []byte {
	reply := map[string]any{"type": protocol.EventReply, "id": id, "ok": err == nil}
	switch {
	case err != nil:
		reply["code"] = protocol.CodeOf(err)
		reply["error"] = err.Error()
	case result != nil:
		reply["result"] = result
	}
	data, mErr := json.Marshal(reply)
	if mErr != nil {
		return nil
	}
	return append(data, '\n')
}
