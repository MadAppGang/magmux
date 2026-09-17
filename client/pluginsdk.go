package client

// The Go plugin SDK: what a plugin process needs and nothing else.
//
// A plugin is an ordinary socket client that also registers. Everything in this
// file is therefore built on Session — the same connection carries the plugin
// protocol, the ordinary verbs a plugin uses to drive its panes (`open_pane`,
// `send`, `watch`) and the event stream it reads to know what happened. One
// connection, because identity lives on the connection: a plugin's claim to a
// pane (`open_pane {controller:"self"}`) is resolved from the registration made
// on THAT socket, so a plugin that opened a second connection for its verbs
// would find them refused.
//
// What the SDK adds over Session is the direction magmux drives: an `invoke`
// arrives, a handler runs, an `invoke_result` goes back with the same call id.
// Handlers run concurrently and are cancelled when magmux says so.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/protocol"
)

// RegisterTimeout bounds the wait for magmux's answer to plugin.register. It is
// a local round trip against a magmux that has already accepted the connection,
// so a slow one means something is wrong rather than something is busy.
const RegisterTimeout = 10 * time.Second

// PluginOp is one op a plugin offers: how it is advertised, and what runs.
type PluginOp struct {
	// Name is the BARE name ([a-z][a-z0-9_]{0,29}). magmux advertises it
	// qualified, as `<plugin>.<name>`, and invokes it bare.
	Name string
	// Description is what a human or a model reads to decide whether to call
	// it. It is worth writing properly: for an MCP client this is the whole of
	// the tool's documentation.
	Description string
	// Class decides who may call the op. It is a CAPABILITY claim, not a label:
	// declaring a pane-steering op `read` asks magmux to let read-only callers
	// reach it. See protocol.OpClass.
	Class protocol.OpClass
	// Schema is a JSON Schema OBJECT for the args, or nil for "takes nothing".
	Schema json.RawMessage
	// Handler runs the op. ctx is cancelled when magmux stops waiting — the
	// caller's deadline expired, or magmux is shutting down — and a handler
	// that ignores it merely produces an answer nobody reads.
	//
	// A returned error is reported to the caller with its protocol code if it
	// is a *protocol.Error, and as `internal` otherwise.
	Handler func(ctx context.Context, req Invocation) (map[string]any, error)
}

// Invocation is one call as the handler sees it.
type Invocation struct {
	// Op is the bare op name, which matters for a handler shared by several
	// ops.
	Op string
	// Args is the caller's payload, undecoded: it is the plugin's own shape and
	// nothing in between has any business reinterpreting it.
	Args json.RawMessage
	// Caller is who asked. Client is SELF-DECLARED and is a label; nothing may
	// be authorised on it.
	Caller protocol.InvokeCaller
	// Deadline is when magmux stops waiting. It is the same moment ctx is
	// cancelled, given as a time for a handler that wants to budget its work
	// rather than merely be interrupted.
	Deadline time.Time
}

// PluginConfig is everything NewPlugin needs. Name and Ops are required; the
// rest is defaulted from the environment magmux gave the process.
type PluginConfig struct {
	Name    string
	Version string
	Ops     []PluginOp
	// Events every plugin.event must name. An event that is not declared is
	// refused: the declaration is what makes the stream describable to a client
	// that has never heard of this plugin.
	Events []string
	// Sock defaults to MAGMUX_SOCK, Token to MAGMUX_PLUGIN_TOKEN and then
	// MAGMUX_TOKEN — the first for a plugin magmux spawned, the second for one
	// a developer is running by hand against a live session.
	Sock  string
	Token string
	// Dial options, passed through to the underlying Session.
	DialOptions []DialOption
}

// Plugin is a registered plugin's connection to magmux.
type Plugin struct {
	cfg  PluginConfig
	sess *Session
	ops  map[string]PluginOp

	// mu guards the cancel table: one entry per invocation in flight, so an
	// invoke_cancel can reach the handler that is still running. It is a leaf —
	// nothing is called while it is held.
	mu      sync.Mutex
	running map[string]context.CancelFunc

	wg sync.WaitGroup
}

// NewPlugin validates a configuration and fills in its environment defaults. It
// opens nothing: Run is what connects.
func NewPlugin(cfg PluginConfig) (*Plugin, error) {
	if !protocol.ValidPluginName(cfg.Name) {
		return nil, fmt.Errorf("plugin name %q is not usable: it must match [a-z][a-z0-9-]{0,%d}",
			cfg.Name, protocol.MaxPluginNameLen-1)
	}
	if len(cfg.Ops) == 0 {
		return nil, fmt.Errorf("plugin %q registers no ops", cfg.Name)
	}
	ops := make(map[string]PluginOp, len(cfg.Ops))
	for _, op := range cfg.Ops {
		switch {
		case !protocol.ValidOpName(op.Name):
			return nil, fmt.Errorf("op %q is not a usable name ([a-z][a-z0-9_]{0,%d})",
				op.Name, protocol.MaxOpNameLen-1)
		case op.Handler == nil:
			return nil, fmt.Errorf("op %q has no handler", op.Name)
		case !protocol.ValidClass(op.Class):
			return nil, fmt.Errorf("op %q has class %q; it must be read, control, display or input",
				op.Name, op.Class)
		case ops[op.Name].Name != "":
			return nil, fmt.Errorf("op %q is declared twice", op.Name)
		}
		ops[op.Name] = op
	}
	for _, ev := range cfg.Events {
		if !protocol.ValidEventName(ev) {
			return nil, fmt.Errorf("event %q is not a usable name ([a-z][a-z0-9_]{0,%d})",
				ev, protocol.MaxOpNameLen-1)
		}
	}
	if cfg.Sock == "" {
		cfg.Sock = os.Getenv("MAGMUX_SOCK")
	}
	if cfg.Sock == "" {
		return nil, errors.New("no magmux socket: set MAGMUX_SOCK, or PluginConfig.Sock")
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv("MAGMUX_PLUGIN_TOKEN")
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv("MAGMUX_TOKEN")
	}
	if cfg.Token == "" {
		return nil, errors.New("no token: magmux sets MAGMUX_PLUGIN_TOKEN for a plugin it started, " +
			"and MAGMUX_TOKEN is the session token for one you started yourself")
	}
	return &Plugin{cfg: cfg, ops: ops, running: map[string]context.CancelFunc{}}, nil
}

// Session is the underlying connection, for the ordinary verbs: a plugin drives
// its panes with the same `open_pane`, `send` and `watch` any other client
// uses. It is valid only after Connect.
func (p *Plugin) Session() *Session { return p.sess }

// Connect dials magmux and registers. After it returns the plugin's ops are in
// magmux's op table and invocations can arrive, so handlers must be ready
// before it is called — which they are: they were given to NewPlugin.
func (p *Plugin) Connect(ctx context.Context) error {
	opts := append([]DialOption{WithEventHook(p.onLine)}, p.cfg.DialOptions...)
	sess, err := Dial(ctx, p.cfg.Name, p.cfg.Sock, os.Getpid(), opts...)
	if err != nil {
		return fmt.Errorf("connecting to magmux at %s: %w", p.cfg.Sock, err)
	}
	p.sess = sess

	specs := make([]protocol.OpSpec, 0, len(p.cfg.Ops))
	for _, op := range p.cfg.Ops {
		schema := op.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		specs = append(specs, protocol.OpSpec{
			Name:        op.Name,
			Description: op.Description,
			Schema:      schema,
			Class:       op.Class,
		})
	}
	msg := map[string]any{
		"type":    protocol.MsgPluginRegister,
		"token":   p.cfg.Token,
		"name":    p.cfg.Name,
		"version": p.cfg.Version,
		"ops":     specs,
	}
	if len(p.cfg.Events) > 0 {
		msg["events"] = p.cfg.Events
	}
	if _, err := p.sess.Request(ctx, msg, RegisterTimeout); err != nil {
		p.sess.Close()
		return fmt.Errorf("registering as %q: %w", p.cfg.Name, err)
	}
	return nil
}

// Wait blocks until magmux goes away or ctx is cancelled, then waits for every
// handler still running.
//
// A plugin's main is Connect then Wait. The EOF that ends it is magmux's
// teardown: results, then shutdown, then the connection closes — so a plugin
// exits of its own accord, and the SIGTERM magmux sends afterwards is a
// formality it will usually never receive.
func (p *Plugin) Wait(ctx context.Context) {
	select {
	case <-p.sess.closed:
	case <-ctx.Done():
	}
	p.cancelAll()
	p.wg.Wait()
}

// Close ends the connection and cancels every handler in flight.
func (p *Plugin) Close() {
	if p.sess != nil {
		p.sess.Close()
	}
	p.cancelAll()
	p.wg.Wait()
}

// Emit publishes one plugin event to every subscriber. pane may be nil for an
// event about the session as a whole.
//
// Fire and forget: it carries no id, so magmux answers nothing and this cannot
// block. An event that breaks a rule — undeclared, oversize, over the rate cap
// — is refused or dropped by magmux and reported in its debug log rather than
// here, which is the right trade for something on a hot path. Use EmitSync when
// the answer matters.
func (p *Plugin) Emit(event string, pane *int, data any) error {
	msg, err := p.eventMsg(event, pane, data)
	if err != nil {
		return err
	}
	return p.sess.Fire(msg)
}

// EmitSync is Emit with magmux's answer waited for, so a caller learns that an
// event was undeclared, too large, or dropped for exceeding the rate cap.
func (p *Plugin) EmitSync(ctx context.Context, event string, pane *int, data any) error {
	msg, err := p.eventMsg(event, pane, data)
	if err != nil {
		return err
	}
	_, err = p.sess.Request(ctx, msg, ReadTimeout)
	return err
}

func (p *Plugin) eventMsg(event string, pane *int, data any) (map[string]any, error) {
	msg := map[string]any{"type": protocol.MsgPluginEvent, "event": event}
	if pane != nil {
		msg["pane"] = *pane
	}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("encoding the data for event %q: %w", event, err)
		}
		msg["data"] = json.RawMessage(raw)
	}
	return msg, nil
}

// PushSnapshot reports what the tool in one of this plugin's panes is doing.
//
// It is accepted only for a pane this plugin claimed with
// open_pane {controller:"self"}: a plugin may describe the session it is
// driving and no other. The snapshot becomes that pane's state everywhere —
// the live `snapshot` event, `list`, the shutdown `results` — reconciled with
// what the terminal itself saw, so a plugin that stops pushing degrades to
// magmux's own observation rather than freezing.
func (p *Plugin) PushSnapshot(ctx context.Context, snap protocol.ControllerSnapshot) error {
	if snap.Pane == nil {
		return errors.New("a controller snapshot needs a pane: it is an observation about one")
	}
	msg := map[string]any{
		"type":  protocol.MsgControllerSnapshot,
		"pane":  *snap.Pane,
		"state": snap.State,
	}
	for k, v := range map[string]string{
		"response": snap.Response, "tool": snap.Tool, "prompt": snap.Prompt,
		"model": snap.Model, "project": snap.Project, "error": snap.Error,
	} {
		if v != "" {
			msg[k] = v
		}
	}
	_, err := p.sess.Request(ctx, msg, ReadTimeout)
	return err
}

// ── the inbound half ────────────────────────────────────────────────────────

// onLine is the event hook: every line magmux sends, after Session's own
// handling of it. Only the two plugin-protocol types are ours.
func (p *Plugin) onLine(line []byte) {
	var env protocol.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return
	}
	switch env.Type {
	case protocol.MsgInvoke:
		var inv protocol.Invoke
		if err := json.Unmarshal(line, &inv); err != nil {
			return
		}
		p.dispatch(inv)
	case protocol.MsgInvokeCancel:
		var c protocol.InvokeCancel
		if err := json.Unmarshal(line, &c); err != nil {
			return
		}
		p.cancel(c.Call)
	}
}

// dispatch runs one invocation on its own goroutine.
//
// On its own goroutine because this is called from the READER: a handler that
// ran inline would stop the plugin reading its own socket, so it could neither
// receive an invoke_cancel nor get an answer to any request it made — which is
// most of what a handler does.
func (p *Plugin) dispatch(inv protocol.Invoke) {
	op, ok := p.ops[inv.Op]
	if !ok {
		// magmux only ever invokes what this plugin registered, so this is a
		// bug on one side or the other. Answering keeps magmux from waiting out
		// the whole deadline for it.
		p.answer(inv.Call, nil, protocol.Errf(protocol.CodeUnknownVerb,
			"plugin %q has no op %q", p.cfg.Name, inv.Op))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	deadline := time.Now().Add(DefaultProbeConfirmTimeout)
	if inv.DeadlineMs > 0 {
		deadline = time.Now().Add(time.Duration(inv.DeadlineMs) * time.Millisecond)
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
	}

	p.mu.Lock()
	p.running[inv.Call] = cancel
	p.mu.Unlock()
	p.wg.Add(1)

	go func() {
		defer p.wg.Done()
		defer cancel()
		defer func() {
			p.mu.Lock()
			delete(p.running, inv.Call)
			p.mu.Unlock()
		}()
		// A panic in one handler must not take the plugin down: magmux would
		// see the connection drop, unregister every op, and a working plugin
		// would be gone because one call had a bad map index.
		defer func() {
			if r := recover(); r != nil {
				p.answer(inv.Call, nil, protocol.Errf(protocol.CodeInternal,
					"plugin %q panicked handling %s: %v", p.cfg.Name, inv.Op, r))
			}
		}()
		result, err := op.Handler(ctx, Invocation{
			Op:       inv.Op,
			Args:     inv.Args,
			Caller:   inv.Caller,
			Deadline: deadline,
		})
		p.answer(inv.Call, result, err)
	}()
}

// answer sends one invoke_result. It carries no id: magmux matches it by call
// id, and a reply to a reply is not a thing this protocol has.
func (p *Plugin) answer(call string, result map[string]any, err error) {
	msg := map[string]any{"type": protocol.MsgInvokeResult, "call": call, "ok": err == nil}
	if err != nil {
		msg["code"] = protocol.CodeOf(err)
		msg["error"] = err.Error()
	} else if result != nil {
		msg["result"] = result
	}
	_ = p.sess.Fire(msg)
}

func (p *Plugin) cancel(call string) {
	p.mu.Lock()
	cancel := p.running[call]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *Plugin) cancelAll() {
	p.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(p.running))
	for _, c := range p.running {
		cancels = append(cancels, c)
	}
	p.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}
