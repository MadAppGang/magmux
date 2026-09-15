package mux

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/sockdir"
)

// ── Unix Domain Socket IPC ──────────────────────────────────────────────────

type sockMsg struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Pane  any    `json:"pane,omitempty"` // int or "*"
	Color string `json:"color,omitempty"`
	Style string `json:"style,omitempty"`
	// Agent hook event fields (type="agent")
	Event            string `json:"event,omitempty"`             // hook event name
	Tool             string `json:"tool,omitempty"`              // tool name from PreToolUse
	Prompt           string `json:"prompt,omitempty"`            // from UserPromptSubmit
	Project          string `json:"project,omitempty"`           // project name
	NotificationType string `json:"notification_type,omitempty"` // idle_prompt, permission_prompt, etc.
	// Controlled-session fields (type="send" / type="pilot")
	Keys    []string `json:"keys,omitempty"`    // named keys to press after Text
	Enter   *bool    `json:"enter,omitempty"`   // submit after Text; defaults true
	Label   string   `json:"label,omitempty"`   // short tag for the control log ("step 2/5")
	Goal    string   `json:"goal,omitempty"`    // the task the pilot is driving
	Steps   int      `json:"steps,omitempty"`   // planned step count, 0 if open-ended
	Model   string   `json:"model,omitempty"`   // model the pilot itself is running
	Summary string   `json:"summary,omitempty"` // pilot's closing summary
	// Client is the controller's identity for the panel header
	// ("claude-code/2.1"). The ONE field MCP adds to the pilot protocol —
	// everything else an MCP client does arrives as an ordinary socket verb.
	Client string `json:"client,omitempty"`
	// Request/response fields. All optional and purely additive — a message
	// that omits them behaves exactly as it did before they existed.
	//
	// ID is json.RawMessage rather than a string so a numeric or string id
	// round-trips verbatim: a client that sent 7 gets 7 back, not "7".
	// Presence of ID is the *only* thing that makes a message answerable.
	ID        json.RawMessage `json:"id,omitempty"`
	Lines     int             `json:"lines,omitempty"`     // capture: keep the last N rows
	Offset    int             `json:"offset,omitempty"`    // capture: rows of scrollback to reach back through
	Cursor    bool            `json:"cursor,omitempty"`    // capture: caller wants the cursor position
	Cmd       string          `json:"cmd,omitempty"`       // pane lifecycle: command to run
	Cwd       string          `json:"cwd,omitempty"`       // pane lifecycle: working directory
	Dir       string          `json:"dir,omitempty"`       // pane lifecycle: synonym for cwd
	Env       []string        `json:"env,omitempty"`       // pane lifecycle: extra KEY=VALUE entries
	Target    any             `json:"target,omitempty"`    // open_pane: pane to split; absent = focused
	Direction string          `json:"split,omitempty"`     // open_pane: auto | horizontal | vertical
	Ratio     float64         `json:"ratio,omitempty"`     // open_pane: first half's share, 0 = 0.5
	Force     bool            `json:"force,omitempty"`     // pane lifecycle: escalate to SIGKILL
	Focus     *bool           `json:"focus,omitempty"`     // pane lifecycle: focus the result
	TimeoutMs int             `json:"timeoutMs,omitempty"` // per-request budget, 0 = the verb's default
}

// socketDir is where this magmux binds. The field wins over the package
// default so a test can point one Magmux at a t.TempDir() without swapping a
// package var out from under anything else.
func (m *Magmux) socketDir() string {
	if m.sockDir != "" {
		return m.sockDir
	}
	return sockdir.Dir
}

// socketPath is where the IPC socket is bound. The pid-based default is
// documented as stable and stays byte-identical; --id only substitutes the
// name, which is what lets a caller know the path *before* magmux starts and
// poll for it rather than having to discover a pid.
//
// filepath.Join, not concatenation, so a --sock-dir with a trailing slash is
// cleaned. Join("/tmp", "magmux-1234.sock") is exactly "/tmp/magmux-1234.sock",
// which is the byte-for-byte default TestSocketPathDefaultIsUnchanged pins.
func (m *Magmux) socketPath() string {
	id := strconv.Itoa(os.Getpid())
	if m.sockID != "" {
		id = m.sockID
	}
	return filepath.Join(m.socketDir(), "magmux-"+id+".sock")
}

func (m *Magmux) socketServer() {
	sockPath := m.socketPath()
	m.sockPath = sockPath

	// Set env so children inherit it
	os.Setenv("MAGMUX_SOCK", sockPath)

	// Remove the sockets of magmuxes that died without cleaning up after
	// themselves — SIGKILL, a panic, an OOM kill, power loss, and magmux's own
	// second-signal hard exit. All of those skip the teardown below, so nothing
	// else in the system ever removes those files; this sweep is the only
	// recovery there is.
	//
	// SYNCHRONOUS, on this goroutine, and BEFORE the bind. Not a goroutine: an
	// untracked one can be outlived by a short run
	// (`for i in $(seq 1 100); do magmux -w -e true; done`) and then the sweep
	// simply never happens, silently, on exactly the workload that creates the
	// most sockets. It is cheap enough to be synchronous — ~10ms against a
	// 1500-entry directory, against a 50ms deadline — and main()'s 10ms sleep
	// before forking children is explicitly "a delay, not a synchronisation
	// primitive": MAGMUX_SOCK is published synchronously above, and clients
	// retry.
	//
	// Before rather than after net.Listen so the sweep cannot observe our own
	// socket at all; that makes sockdir.ReapStale' path != self rule
	// belt-and-braces rather than load-bearing, because a reaper whose safety
	// depends on correctly recognising itself is one refactor away from
	// deleting its own socket.
	reapStart := time.Now()
	if n := sockdir.ReapStale(filepath.Dir(sockPath), sockPath, sockdir.ReapDeadline); n > 0 && dbgFile != nil {
		// The elapsed time is worth logging: it is the only way to tell a sweep
		// that finished from one the deadline cut short, and a short one leaves
		// work for the next start.
		fmt.Fprintf(dbgFile, "reaped %d stale socket(s) in %v\n", n, time.Since(reapStart))
	}

	// Clean up stale socket
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		// On stderr, not just dbgFile: until now a magmux that could not bind
		// ran with NO socket and said nothing at all, so a caller polling the
		// path it was promised simply hung. Unconditional — stderr is not the
		// frame, and integrators read it.
		fmt.Fprintf(os.Stderr, "magmux: socket %s: %v\n", sockPath, err)
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "socket listen error: %v\n", err)
		}
		m.markSocketDone() // nothing to tear down; don't make main wait
		return
	}

	// Cleanup on exit
	go func() {
		<-m.quit
		// Push a final aggregated results event with the last-known state of
		// every pane. Subscribers (e.g. claudish) use this as the authoritative
		// final state — no file-based fallback needed.
		results := map[string]any{
			"type":    "results",
			"panes":   m.buildPaneResults(),
			"endedAt": time.Now().UTC().Format(time.RFC3339),
		}
		shutdown := map[string]any{"type": "shutdown"}
		// Record before broadcasting. From this point a connection either wins
		// the race and is registered (so the broadcasts below reach it), or it
		// arrives after and is replayed these same events by handleSocketConn.
		// Both paths deliver exactly once, because a client that sees
		// finalEvents is never added to sockClients.
		m.recordFinalEvents(results, shutdown)
		m.broadcastEvent(results)
		// Push a shutdown event so clients know the socket is closing.
		m.broadcastEvent(shutdown)
		// Deterministically flush and close each subscriber connection instead
		// of racing a fixed drain sleep. Closing the connection after the two
		// broadcasts above (which write synchronously under sockClientsMu) gives
		// each subscriber a clean EOF *after* it has received the final results
		// — the ordering guarantee integrators rely on. Bound the whole teardown
		// so a single wedged subscriber can't hang magmux's exit.
		m.closeSockClients(2 * time.Second)
		ln.Close()
		os.Remove(sockPath)
		m.markSocketDone()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go m.handleSocketConn(conn)
	}
}

func (m *Magmux) handleSocketConn(conn net.Conn) {
	// Wait for the layout before serving anything on this connection. The
	// listener is bound before the first child forks, on purpose — MAGMUX_SOCK
	// must be in that child's environment — so connections can and do arrive
	// while m.root and m.allPanes are still nil. Serving one there writes an
	// EMPTY aggregate snapshot as the connection's first line, and a subscriber
	// seeds its whole pane map from that line, so it would then wait forever
	// for per-pane snapshots that only ever fire on change.
	//
	// Done BEFORE sockClientsMu is taken: that lock is what makes the write and
	// the registration below atomic against the shutdown broadcast, and holding
	// it across a wait would block every other client's `results`.
	m.waitLayoutReady(layoutReadyTimeout)

	// Immediately send the current pane-state snapshot so a subscriber that
	// connects *after* some panes have already exited still receives full
	// state (the live exit/snapshot events it missed are folded into this one
	// aggregate event). Written before registering the connection for
	// broadcasts so it is always the first line this subscriber sees.
	snapshot := map[string]any{
		"type":  "snapshot",
		"panes": m.buildPaneResults(),
	}
	// NOTE: distinct from the per-pane live "snapshot" event (which carries a
	// singular "pane" field). This connect-time aggregate carries a "panes"
	// array — subscribers disambiguate on that field.
	//
	// Build the payload BEFORE taking sockClientsMu: buildPaneResults locks
	// each pane, and the established lock order is p.mu -> sockClientsMu
	// (pollControllers releases p.mu before broadcasting). Taking them the
	// other way round here would invert it.
	data, err := json.Marshal(snapshot)
	if err != nil {
		conn.Close()
		return
	}
	data = append(data, '\n')

	// Write the aggregate and register for broadcasts ATOMICALLY, under the
	// same lock broadcastEvent uses. Writing first and registering after left
	// a window in which a shutdown broadcast (results/shutdown) could run
	// against a client list that did not yet contain this connection: the
	// subscriber then saw a clean EOF with no `results` event, violating the
	// ordering guarantee integrators rely on. That was rare but real — it is
	// what made TestSocketSubscriberContract flaky.
	m.sockClientsMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = conn.Write(data)
	_ = conn.SetWriteDeadline(time.Time{})
	if len(m.finalEvents) > 0 {
		// Teardown already began, so this connection will never be broadcast
		// to. Replay the final events it missed, then give it a clean EOF.
		// Registering instead would either lose `results` or leave the
		// connection dangling.
		for _, ev := range m.finalEvents {
			_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			_, _ = conn.Write(ev)
		}
		m.sockClientsMu.Unlock()
		conn.Close()
		return
	}
	m.sockClients = append(m.sockClients, conn)
	m.sockClientsMu.Unlock()

	defer func() {
		conn.Close()
		m.sockClientsMu.Lock()
		for i, c := range m.sockClients {
			if c == conn {
				m.sockClients = append(m.sockClients[:i], m.sockClients[i+1:]...)
				break
			}
		}
		m.sockClientsMu.Unlock()
	}()

	scanner := bufio.NewScanner(conn)
	// Raise the token limit off the 64KB default. An oversized line is not
	// skipped: Scan returns false and this loop ends, so one long message
	// (a pasted instruction, a large payload) silently drops the *whole*
	// client connection rather than one line — the subscriber stops receiving
	// broadcasts with no error anywhere. 4MB matches the transcript scanners
	// in controller_claude.go.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	// Whether this connection ever DROVE anything, as opposed to subscribing
	// and tinting. It is the only thing the panel needs to report a controller
	// arriving and going away: the fd close below is already the disconnect.
	// An operator staring at a frozen panel needs to know the controller went
	// away rather than got slow.
	driving := false
	for scanner.Scan() {
		line := scanner.Text()
		var msg sockMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if m.handleSocketMsg(msg, conn) && !driving {
			driving = true
			m.control.noteController(true)
		}
	}
	if driving {
		m.control.noteController(false)
	}
}

// layoutReadyTimeout bounds how long a connection waits for the layout. It
// exists so a magmux that fails to build one (or dies between binding the
// socket and finishing startup) hands its client an honest `not_ready` instead
// of a connection that never says anything — a hang is the one failure mode an
// agent cannot report on.
const layoutReadyTimeout = 5 * time.Second

// markLayoutReady publishes the layout to the socket. Safe to call repeatedly,
// and called from a defer in main so no failure path can leave a connection
// parked on the channel while magmux exits underneath it.
func (m *Magmux) markLayoutReady() {
	if m.layoutReady == nil {
		return
	}
	m.layoutReadyOnce.Do(func() { close(m.layoutReady) })
}

// layoutIsReady is the non-blocking form, for a verb that has already waited.
func (m *Magmux) layoutIsReady() bool {
	if m.layoutReady == nil {
		return true
	}
	select {
	case <-m.layoutReady:
		return true
	default:
		return false
	}
}

// waitLayoutReady blocks until the layout exists, magmux starts shutting down,
// or the timeout expires; it reports whether the layout is actually there.
//
// Callers must hold NO lock — least of all sockClientsMu, which serialises
// every broadcast: waiting under it would stall the shutdown broadcast that
// delivers `results` behind a client that connected during startup.
func (m *Magmux) waitLayoutReady(timeout time.Duration) bool {
	if m.layoutIsReady() {
		return true
	}
	select {
	case <-m.layoutReady:
		return true
	case <-m.quit:
		// Teardown never waits on a subscriber. The connection is still served:
		// handleSocketConn's finalEvents replay is what gives it results →
		// shutdown → EOF, and that path must stay reachable.
		return false
	case <-time.After(timeout):
		return false
	}
}

// markSocketDone signals that socket teardown is complete (or will never
// happen). Safe to call repeatedly.
func (m *Magmux) markSocketDone() {
	if m.sockDone == nil {
		return
	}
	m.sockDoneOnce.Do(func() { close(m.sockDone) })
}

// waitSocketShutdown blocks until the socket server has flushed its final
// results/shutdown broadcasts and closed every subscriber, or until timeout.
//
// Without this, main can return from inputLoop and exit while the teardown
// goroutine is still running, so subscribers see the connection drop with no
// `results` event at all — the ordering guarantee integrators rely on,
// broken by process exit rather than by anything on the socket path. Bounded
// so a wedged subscriber cannot stop magmux from exiting.
func (m *Magmux) waitSocketShutdown(timeout time.Duration) {
	if m.sockDone == nil {
		return
	}
	select {
	case <-m.sockDone:
	case <-time.After(timeout):
	}
}

// recordFinalEvents marshals the shutdown payloads and stores them so a
// subscriber connecting during or after teardown can still be given them.
// Must be called before the corresponding broadcasts.
func (m *Magmux) recordFinalEvents(events ...any) {
	var encoded [][]byte
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		encoded = append(encoded, append(data, '\n'))
	}
	m.sockClientsMu.Lock()
	m.finalEvents = encoded
	m.sockClientsMu.Unlock()
}

// broadcastEvent serializes an event as JSON and pushes it to all connected
// socket clients. Best-effort: failed writes drop the client silently.
func (m *Magmux) broadcastEvent(event any) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	data = append(data, '\n')

	m.sockClientsMu.Lock()
	defer m.sockClientsMu.Unlock()
	// Iterate in reverse so we can splice out dead clients.
	for i := len(m.sockClients) - 1; i >= 0; i-- {
		c := m.sockClients[i]
		_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.Write(data); err != nil {
			c.Close()
			m.sockClients = append(m.sockClients[:i], m.sockClients[i+1:]...)
		}
	}
}

// closeSockClients flushes and closes every connected subscriber connection so
// each receives a clean EOF *after* the final results/shutdown broadcasts. The
// broadcasts run synchronously under sockClientsMu with a per-write deadline, so
// by the time this runs the payload has already been written to the OS socket
// buffer; closing the fd then signals end-of-stream. Bounded by timeout so a
// wedged peer (never draining its receive buffer) can't block magmux's exit.
func (m *Magmux) closeSockClients(timeout time.Duration) {
	m.sockClientsMu.Lock()
	conns := make([]net.Conn, len(m.sockClients))
	copy(conns, m.sockClients)
	m.sockClients = nil
	m.sockClientsMu.Unlock()

	if len(conns) == 0 {
		return
	}

	done := make(chan struct{})
	go func() {
		for _, c := range conns {
			// A write deadline in the past forces any buffered write to flush or
			// error immediately rather than block, then Close signals EOF.
			_ = c.SetWriteDeadline(time.Now().Add(timeout))
			_ = c.Close()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		// Best effort: force-close whatever is left so nothing lingers.
		for _, c := range conns {
			_ = c.Close()
		}
	}
}

// dispatchSocketMsg runs one verb and reports its outcome. Behaviour when no
// reply was requested is unchanged: a caller that ignores both returns gets
// exactly the old semantics, which is what every legacy client relies on.
func (m *Magmux) dispatchSocketMsg(msg sockMsg) (map[string]any, error) {
	return m.dispatchSocketVerb(msg, nil)
}

// dispatchSocketVerb runs one verb.
//
// done, if non-nil, is how a verb whose work outlives this call answers: it
// returns errReplyDeferred and calls done exactly once, later, on whatever
// goroutine finished the work. Only `send` does that — its writes are paced
// across hundreds of milliseconds and the socket reader must not block on them.
// A nil done means nobody is listening, so those verbs keep their old
// fire-and-forget shape.
func (m *Magmux) dispatchSocketVerb(msg sockMsg, done func(map[string]any, error)) (map[string]any, error) {
	switch msg.Type {
	case "capabilities":
		return m.sockCapabilities()

	case "list":
		return m.sockList()

	case "capture":
		return m.sockCapture(msg)

	case "open_pane":
		return m.sockOpenPane(msg)

	case "close_pane":
		return m.sockClosePane(msg)

	case "focus":
		return m.sockFocus(msg)

	case "status":
		m.treeMu.Lock()
		m.statusText = msg.Text
		// Force a redraw
		for _, p := range m.livePanesLocked(nil) {
			p.mu.Lock()
			p.dirty = true
			p.mu.Unlock()
		}
		m.treeMu.Unlock()
		return nil, nil

	case "tint":
		color := msg.Color
		if color == "reset" {
			color = ""
		}
		paneIdx := m.parsePaneIndex(msg.Pane)
		if paneIdx == paneAll {
			// "*" — apply to every LIVE pane. A tombstoned pane is not on
			// screen, so tinting it would be a write nobody can ever see.
			for _, p := range m.livePanes() {
				p.mu.Lock()
				p.tint = color
				p.dirty = true
				p.mu.Unlock()
			}
			return nil, nil
		}
		p, err := m.paneForMsg(paneIdx)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.tint = color
		p.dirty = true
		p.mu.Unlock()
		return nil, nil

	case "overlay":
		paneIdx := m.parsePaneIndex(msg.Pane)
		p, err := m.paneForMsg(paneIdx)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.overlayText = msg.Text
		p.overlayStyle = msg.Style
		p.dirty = true
		p.mu.Unlock()
		return nil, nil

	case "send":
		// Drive a pane from outside — the inbound half of a controlled
		// session. Defaults to the pilot's target pane so a pilot that has
		// announced itself need not repeat the index on every instruction.
		//
		// Only an *absent* pane takes that default. "*" and an unparseable
		// pane both refuse: there is no such thing as typing an instruction
		// into every pane, and guessing a target types someone's next
		// instruction into the wrong session, which is both expensive and
		// invisible.
		paneIdx := m.parsePaneIndex(msg.Pane)
		switch {
		case paneIdx == paneUnspecified:
			// One route means the single-session case, so this is free. Several
			// open routes means the default would be a guess, and targetPane
			// refuses rather than typing the next instruction into whichever
			// Claude Code session happened to be first.
			var err error
			if paneIdx, err = m.control.targetPane(); err != nil {
				return nil, err
			}
		case paneIdx == paneAll:
			return nil, sockErrf(sockCodeBadRequest, `send has no fan-out: "*" is not a target`)
		case paneIdx < 0:
			return nil, sockErrf(sockCodeBadRequest, "pane is not an index")
		}
		enter := true
		if msg.Enter != nil {
			enter = *msg.Enter
		}
		if done == nil {
			return nil, m.sendToPane(paneIdx, msg.Text, msg.Keys, enter, msg.Label, nil)
		}
		// Delivery outlives this call, so the reply does too: it means "the
		// bytes reached the PTY", which is the failure mode that used to live
		// and die inside sendToPane's goroutine.
		result := map[string]any{"pane": paneIdx, "bytes": len(msg.Text), "keys": len(msg.Keys), "enter": enter}
		if err := m.sendToPane(paneIdx, msg.Text, msg.Keys, enter, msg.Label, func(err error) {
			done(result, err)
		}); err != nil {
			return nil, err
		}
		return nil, errReplyDeferred

	case "pilot":
		return nil, m.dispatchPilotMsg(msg)

	case "agent":
		paneIdx := m.parsePaneIndex(msg.Pane)
		p, err := m.paneForMsg(paneIdx)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()

		oldStatus := p.agentStatus
		if msg.Project != "" {
			p.agentProject = msg.Project
		}

		// State machine: derive status from hook event (matches cctop transitions)
		switch msg.Event {
		case "UserPromptSubmit":
			p.agentStatus = "working"
			p.agentTool = ""
			if msg.Prompt != "" {
				p.agentPrompt = msg.Prompt
			}
		case "PreToolUse":
			p.agentStatus = "working"
			if msg.Tool != "" {
				p.agentTool = msg.Tool
			}
		case "PostToolUse", "PostToolUseFailure":
			p.agentStatus = "working"
		case "Stop":
			p.agentStatus = "waiting_input"
			p.agentTool = ""
		case "Notification":
			switch msg.NotificationType {
			case "idle_prompt":
				p.agentStatus = "waiting_input"
			case "permission_prompt":
				p.agentStatus = "waiting_permission"
			}
		case "PermissionRequest":
			p.agentStatus = "waiting_permission"
		case "PreCompact":
			p.agentStatus = "compacting"
		case "PostCompact":
			p.agentStatus = "idle"
		case "SessionStart":
			p.agentStatus = "idle"
			p.agentTool = ""
			p.agentPrompt = ""
		case "SessionEnd":
			p.agentStatus = ""
			p.agentProject = ""
			p.agentTool = ""
			p.agentPrompt = ""
		}

		newStatus := p.agentStatus
		p.dirty = true

		// Visual feedback for attention-needed states
		if newStatus == "waiting_input" || newStatus == "waiting_permission" {
			label := "INPUT"
			if newStatus == "waiting_permission" {
				label = "PERMISSION"
			}
			p.overlayText = fmt.Sprintf("⚡ AWAITING %s", label)
			p.overlayStyle = "error"
			p.tint = "red"
		} else if oldStatus == "waiting_input" || oldStatus == "waiting_permission" {
			// Clear attention indicators when agent resumes
			p.overlayText = ""
			p.overlayStyle = ""
			p.tint = ""
		}

		p.mu.Unlock()

		// Update aggregated status bar
		if oldStatus != newStatus {
			m.updateAgentStatusBar()
		}
		return nil, nil
	}

	// An unknown verb stays silent without an id, which is the behaviour every
	// client written before replies existed depends on — and is exactly what
	// makes `capabilities` a usable version probe: an older magmux answers a
	// verb it does not know with nothing at all.
	return nil, sockErrf(sockCodeUnknownVerb, "unknown verb %q", msg.Type)
}

// Sentinels parsePaneIndex returns in place of an index. All negative, so the
// existing `idx >= 0 && idx < len(m.allPanes)` bounds checks reject them
// without needing a special case.
const (
	paneAll         = -1 // "*" — fan out to every pane
	paneInvalid     = -2 // the field was present but is not a pane index
	paneUnspecified = -3 // the field was absent; the verb's own default applies
)

// parsePaneIndex resolves a socket message's `pane` field to a pane index or
// to one of the sentinels above.
//
// The string branch used to be fmt.Sscanf with its error dropped, which left
// idx at its zero value on failure — so {"pane":"api"} silently drove *pane 0*,
// the wrong session, with no diagnostic anywhere. strconv.Atoi rejects the
// whole string (Sscanf would have taken the "1" out of "1x"), so anything that
// is neither a bare integer nor "*" is paneInvalid and every caller drops it.
//
// An absent field is distinct from a bad one: it is how a pilot says "the pane
// I already announced", so it gets its own sentinel rather than sharing the one
// that means "refuse this message".
func (m *Magmux) parsePaneIndex(v any) int {
	switch val := v.(type) {
	case nil:
		return paneUnspecified
	case float64:
		return int(val)
	case string:
		s := strings.TrimSpace(val)
		if s == "*" {
			return paneAll
		}
		idx, err := strconv.Atoi(s)
		if err != nil {
			return paneInvalid
		}
		return idx
	}
	return paneInvalid
}

// TODO: Add native macOS kqueue-based file watcher (kqueue_darwin.go) for monitoring
// ~/.cctop/sessions/ as a fallback when agents don't send IPC messages directly.
// Also consider Linux inotify equivalent (inotify_linux.go).

// updateAgentStatusBar rebuilds the status bar from the agent hook state of
// every live pane. Caller must NOT hold treeMu.
func (m *Magmux) updateAgentStatusBar() {
	var parts []string
	needsAttention := 0

	m.treeMu.Lock()
	defer m.treeMu.Unlock()
	live := m.livePanesLocked(nil)
	for _, p := range live {
		p.mu.Lock()
		status := p.agentStatus
		name := p.agentProject
		p.mu.Unlock()

		if status == "" {
			continue // not an agent pane
		}

		if name == "" {
			name = "agent"
		}
		if len(name) > 15 {
			name = name[:15]
		}

		var colorCode, icon string
		switch status {
		case "working":
			colorCode = "G"
			icon = "●"
		case "idle":
			colorCode = "D"
			icon = "○"
		case "waiting_input":
			colorCode = "R"
			icon = "⚡"
			needsAttention++
		case "waiting_permission":
			colorCode = "Y"
			icon = "⚠"
			needsAttention++
		case "compacting":
			colorCode = "C"
			icon = "◐"
		default:
			colorCode = "D"
			icon = "?"
		}
		parts = append(parts, fmt.Sprintf("%s:%s %s", colorCode, icon, name))
	}

	if len(parts) == 0 {
		return
	}

	statusText := strings.Join(parts, "\t")
	if needsAttention > 0 {
		statusText = fmt.Sprintf("R:⚡ %d NEED INPUT\t", needsAttention) + statusText
	}

	m.statusText = statusText
	for _, p := range live {
		p.mu.Lock()
		p.dirty = true
		p.mu.Unlock()
	}
}
