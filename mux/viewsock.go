package mux

// The view socket (--view-sock): a second unix socket, beside the control one,
// on which every connection is a READ-ONLY caller.
//
// The control socket checks no credential — its access control is the
// filesystem's — so "read-only" cannot be a property a client claims about
// itself on it. It is a property of WHERE the client connected: a program that
// was handed only the view path cannot steer the session, whatever it sends.
// That is the whole design, and it is why there is no handshake.
//
// The rule is the --listen view token's, decided by the same auth.Store:
// a built-in op of class read, or a plugin op --view-op granted by name, and
// nothing else. The hub's own Caller.ReadOnly check sits behind it, but the
// legacy socket verbs never reach the hub — they are dispatched straight to
// their handlers — so the gate here runs before ANY verb, on the raw message,
// and is the enforcement point for this transport.
//
// The file is named magmux-{id}.view, NOT magmux-{id}.view.sock: everything
// that discovers sessions — `magmux mcp`, claudish's pane registry — globs
// magmux-*.sock, and a view socket matching it would appear as a second
// session.

import (
	"context"
	"net"
	"os"
	"strings"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/unixsock"
)

// viewSocketPath is the control socket's path with `.sock` replaced by
// `.view`, so it follows --sock-dir and --id with no second rule.
func (m *Magmux) viewSocketPath() string {
	return strings.TrimSuffix(m.socketPath(), ".sock") + ".view"
}

// listenViewSocket binds m.viewSockPath. Like listenSocket it runs before any
// child forks, and the caller treats a failure as fatal: a client told to
// watch a path that never appears would wait forever.
func (m *Magmux) listenViewSocket() (net.Listener, error) {
	// A stale file from a magmux that died is replaced, exactly as the
	// control socket's is. A path that cannot be removed (a directory) makes
	// the bind fail, which is the answer.
	if fi, err := os.Lstat(m.viewSockPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(m.viewSockPath)
	}
	return unixsock.Listen(m.viewSockPath)
}

// serveViewSocket is the view socket's accept loop. Its teardown is the
// control socket's (closeViewSocket, called from serveSocket), so the two
// sockets go away together, after `results` and `shutdown`.
func (m *Magmux) serveViewSocket(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go m.serveSocketConn(conn, true)
	}
}

// closeViewSocket closes the view listener and removes its file. Safe when
// --view-sock was not given.
func (m *Magmux) closeViewSocket() {
	if m.viewLn != nil {
		m.viewLn.Close()
	}
	if m.viewSockPath != "" {
		os.Remove(m.viewSockPath)
	}
}

// errReadOnly is the one answer a viewer gets for anything it may not do.
func errReadOnly(name string) error {
	return protocol.Errr(protocol.CodeForbidden, protocol.ReasonReadOnly,
		"%q is not available on the read-only view socket", name)
}

// viewerGate decides one message from a read-only connection, before
// anything runs it. It answers nil for a message the viewer may send, and
// grant=true for a `call` of a plugin op --view-op granted, which must then
// run with a Caller that is NOT read-only (the hub would otherwise refuse the
// very op the operator granted — see auth.Store.EffectiveReadOnly).
//
// An op the registry does not know is let through to its own unknown-verb
// answer, as auth.Store.Authorize does, so a viewer learns nothing here it
// could not learn from `ops`.
func (m *Magmux) viewerGate(msg sockMsg) (grant bool, err error) {
	if !msg.caller.ReadOnly {
		return false, nil
	}
	name := msg.Type
	switch msg.Type {
	case "watch", "unwatch", "resync":
		// The streaming verbs: class read by definition, and the reason the
		// view socket exists.
		return false, nil
	case "pilot", "agent":
		// Socket-only protocols with no op behind them; both steer what the
		// panel and the status bar report.
		return false, errReadOnly(name)
	case "call":
		name = strings.TrimSpace(msg.Op)
		if name == "" {
			return false, nil // the call's own bad_request
		}
	}
	spec, registered := m.bus().Spec(name)
	if !registered {
		return false, nil
	}
	if err := m.viewGrants.Authorize(auth.View, spec, true); err != nil {
		return false, errReadOnly(name)
	}
	return msg.Type == "call" && !m.viewGrants.EffectiveReadOnly(auth.View, name), nil
}

// viewerGrantedCall runs a granted plugin op for a viewer: the hub directly,
// with a Caller built for this one call, which is what the WebSocket does for
// the same grant. A granted op is always a plugin op, never lane-bound, so
// nothing about ordering changes. No id, no call: `call` is id-path only.
func (m *Magmux) viewerGrantedCall(msg sockMsg, sub *hub.Sub) {
	if len(msg.ID) == 0 {
		return
	}
	c := msg.caller
	c.ReadOnly = false
	op := strings.TrimSpace(msg.Op)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.callWait(op, msg.Args, msg.TimeoutMs))
		defer cancel()
		result, err := m.bus().Call(ctx, c, op, msg.Args)
		m.replyTo(sub, msg.ID, result, err)
	}()
}

// capabilitiesFor is `capabilities` for one caller: whether it is read-only,
// and where the view socket is when there is one.
func (m *Magmux) capabilitiesFor(c hub.Caller) (map[string]any, error) {
	res, err := m.sockCapabilities()
	if err != nil {
		return nil, err
	}
	res["readOnly"] = c.ReadOnly
	if m.viewSockPath != "" {
		res["viewSocket"] = m.viewSockPath
	}
	return res, nil
}
