package firebase

// The adapter: one hub.Sub, one flusher, one listener, and the startup order
// that makes the rest of the package safe.
//
// The order is the point. Everything that can be checked without the network is
// checked first (New), everything that needs the network happens later (Start),
// and the teardown is bounded by the same D the socket's Finalize uses so the
// mirror cannot be the thing that makes magmux slow to exit.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/hub"
)

// Options are the things the adapter cannot know for itself.
//
// Aggregate and Ops are FUNCTIONS rather than values because both are read
// after the layout exists and change afterwards, and a value captured at
// construction would be a picture of a magmux that had not started yet.
type Options struct {
	// Hub is the registry and the bus. Required.
	Hub *hub.Hub
	// Aggregate returns the connect-time aggregate as one line-JSON message,
	// exactly as the socket and HTTP adapters build it.
	Aggregate func() []byte
	// SID is this session's id, `{sockid|pid}-{startUnix}`. Use MakeSID.
	SID string
	// PID and Version go into the session's `meta` node, which is what a reader
	// lists sessions from.
	PID     int
	Version string
	// Geometry is the terminal size, read at Start rather than taken as a
	// value: the adapter is BUILT before mux.init(), which is where the size is
	// resolved, so a value captured at construction is always 0x0. It may be
	// nil.
	Geometry func() (rows, cols int)
	// Log receives one line per diagnostic. It must never write to stdout: a
	// headless magmux emits zero bytes there, and an attached one has an
	// alternate screen in the way.
	Log func(string)
	// HTTPClient replaces the one the adapter would build. Only tests set it,
	// and a test that does must install the same redirect rule.
	HTTPClient *http.Client
}

// Adapter is the whole Firebase surface for one session.
type Adapter struct {
	cfg    *Config
	c      *client
	hub    *hub.Hub
	sub    *hub.Sub
	mirror *mirror

	sid  string
	sess string
	opts Options
	log  func(string)

	nonces *nonceLRU
	sem    chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	handled   map[string]bool
	completed []string
	started   bool
	closeOnce sync.Once
}

// MakeSID is the session id: the socket id (or the pid) and the process start
// time.
//
// The start time is not decoration. It is what makes a restarted magmux listen
// on a DIFFERENT `commands` node, which is the whole of the cross-crash
// at-most-once guarantee: the old session's commands are never read, never run
// and never rewritten, so nothing can replay.
func MakeSID(id string, start time.Time) string {
	if id == "" {
		id = "magmux"
	}
	return id + "-" + strconv.FormatInt(start.Unix(), 10)
}

// New validates everything that can be validated without the network and builds
// the adapter. It performs NO I/O.
//
// It is called before mux.init(), so every error here is a plain line on stderr
// and an exit 1 while magmux still owns a normal terminal.
func New(cfg *Config, opts Options) (*Adapter, error) {
	if cfg == nil {
		return nil, fmt.Errorf("firebase: no config")
	}
	if opts.Hub == nil {
		return nil, fmt.Errorf("firebase: no hub")
	}
	if opts.SID == "" {
		return nil, fmt.Errorf("firebase: no session id")
	}
	c, err := newClient(cfg, opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	a := &Adapter{
		cfg:     cfg,
		c:       c,
		hub:     opts.Hub,
		sid:     opts.SID,
		sess:    sessionPath(cfg.Root, cfg.Host, opts.SID),
		opts:    opts,
		log:     opts.Log,
		nonces:  newNonceLRU(nonceCap),
		sem:     make(chan struct{}, executors),
		handled: make(map[string]bool),
	}
	a.mirror = newMirror(c, a.sess, cfg, a.log)
	return a, nil
}

// URL is the session node, for the one line magmux prints at startup. The query
// is not in it and neither is the credential.
func (a *Adapter) URL() string { return redact(a.c.nodeURL(a.sess, nil)) }

// CommandsEnabled reports whether the inbound half is on. Printed at startup,
// because "magmux is taking commands from the internet" is not a thing to
// discover from a config file later.
func (a *Adapter) CommandsEnabled() bool { return a.cfg.Commands.Enabled }

// Start opens the subscription and brings up the flusher and the listener.
//
// It is called AFTER the layout is ready, for the same reason the socket's
// aggregate is: a snapshot of panes that do not exist yet is a mirror of
// nothing. Nothing here blocks on the network — the owner list and the first
// meta write go through the flusher and the retry loop like everything else.
func (a *Adapter) Start() {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	a.started = true
	a.mu.Unlock()

	a.ctx, a.cancel = context.WithCancel(context.Background())

	// The session's own meta. `alive` is true from here and is only ever set
	// false by a CLEAN shutdown; the heartbeat is what a reader actually uses.
	meta := map[string]any{
		"pid":         a.opts.PID,
		"version":     a.opts.Version,
		"alive":       true,
		"startedAt":   serverTimestamp(),
		"heartbeatAt": serverTimestamp(),
	}
	if a.opts.Geometry != nil {
		// Read HERE and not in New: the size is settled by mux.init(), which
		// runs after the adapter is built, so a value captured at construction
		// is always 0x0 — which is what the first smoke run against a real
		// emulator showed.
		meta["rows"], meta["cols"] = a.opts.Geometry()
	}
	a.mirror.SetSessionMeta(meta)
	a.mirror.MarkOpsChanged()

	// Step 1 of the subscribe cut: registered and BUFFERING. Nothing published
	// from here is lost, and nothing is written before the aggregate.
	a.sub = a.hub.Session(hub.Caller{
		Transport: "firebase",
		Conn:      "firebase",
		Client:    "mirror",
	}, a.mirror, nil)
	var head []byte
	if a.opts.Aggregate != nil {
		head = a.opts.Aggregate()
	}
	a.sub.Start(head)
	// One WatchAll instead of tracking pane lifecycle: the streamer attaches
	// panes opened later, so a mirror never misses a pane it was not told
	// about.
	if err := a.sub.WatchAll(a.cfg.FrameFPS); err != nil {
		a.logf("frames are not available: %v", err)
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.ensureOwners(a.ctx)
	}()
	go a.mirror.Run(a.ctx, a.opsNode)
	if a.cfg.Commands.Enabled {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.listen(a.ctx)
		}()
	}
}

// ensureOwners writes the owner list, wholesale, above the session.
//
// Wholesale and above the session because the SECURITY RULES read it: a
// session-scoped owner list would let a forged session name its own owners, and
// a merge rather than a replace would leave a removed owner in place forever.
// It retries because it is the one write whose absence silently refuses every
// legitimate client.
func (a *Adapter) ensureOwners(ctx context.Context) {
	owners := make(map[string]any, len(a.cfg.Commands.Owners))
	for _, uid := range a.cfg.Commands.Owners {
		if sanitizeKey(uid) {
			owners[uid] = true
		}
	}
	if len(owners) == 0 {
		return
	}
	var bo backoff
	for ctx.Err() == nil {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := a.c.put(wctx, ownersPath(a.cfg.Root, a.cfg.Host), owners)
		cancel()
		if err == nil {
			return
		}
		wait := bo.next()
		a.logf("could not write the owner list (%v); retrying in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// opsNode renders the hub's op list as the `ops` node: one JSON STRING per op,
// keyed by the RTDB-safe spelling of its name.
//
// A string and not a subtree because an op's schema is arbitrary JSON: a
// plugin's `$ref` is a legal JSON key and an illegal RTDB one, and a schema
// nested past 32 levels would be refused outright. One string per op means no
// plugin can make magmux's mirror unwritable by describing itself.
func (a *Adapter) opsNode() map[string]any {
	specs, rev := a.hub.Ops()
	out := make(map[string]any, len(specs)+1)
	for _, s := range specs {
		out[opKey(s.Name)] = jsonString(s)
	}
	out["rev"] = rev
	return out
}

// Finalize is the last write: whatever is still pending, plus `alive:false`,
// under the deadline the rest of teardown uses.
//
// It runs AFTER hub.Finalize, which has already handed the mirror `results`
// through the ordinary Write path, so the final flush carries the authoritative
// end state of every pane. The listener and the flusher are stopped first, so
// there is no request in flight to race it.
func (a *Adapter) Finalize(d time.Duration) {
	a.mu.Lock()
	started := a.started
	a.mu.Unlock()
	if !started {
		return
	}
	// Stop the periodic flusher and wait for the request in flight. The wait is
	// bounded by d: a mirror whose database has gone away must not be what
	// keeps magmux on screen.
	a.mirror.Stop()
	select {
	case <-a.mirror.Done():
	case <-time.After(d):
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	a.mirror.FinalFlush(ctx, a.opsNode)
}

// Close ends everything and waits for the goroutines it started.
//
// Idempotent: it is reached from a defer, from the failure paths, and from the
// normal end of Main.
func (a *Adapter) Close() {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		started := a.started
		a.mu.Unlock()
		if !started {
			return
		}
		a.cancel()
		a.mirror.Stop()
		if a.sub != nil {
			a.sub.Close("magmux is shutting down")
		}
		done := make(chan struct{})
		go func() {
			a.wg.Wait()
			<-a.mirror.Done()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
}

func (a *Adapter) logf(format string, args ...any) {
	if a.log != nil {
		a.log("firebase: " + fmt.Sprintf(format, args...))
	}
}
