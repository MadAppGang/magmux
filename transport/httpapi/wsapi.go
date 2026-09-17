package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/ws"
)

// Keepalive. A WebSocket through a proxy or a laptop lid has no other way to
// find out the peer is gone: TCP will sit on a half-open connection for
// minutes, and a magmux holding a framer for a watcher that no longer exists is
// burning a goroutine per pane.
const (
	pingInterval = 30 * time.Second
	pongTimeout  = 60 * time.Second
	// wsCallTimeout bounds how long a caller waits for one op's ANSWER. A lane
	// item that outlives it keeps running and keeps its place; only the wait
	// ends. It is the socket's own default.
	wsCallTimeout = 30 * time.Second
	wsCallMax     = 15 * time.Minute
)

// wsSink is one WebSocket as the hub sees it: one text frame per message.
//
// Over plain TCP it can report n == 0 honestly, because ws.Conn counts the
// bytes that actually reached the connection. Under TLS it cannot: after a
// tls.Conn write times out "the TLS state is corrupt and all future writes will
// return the same error", so any failure has to be treated as torn or a final
// could be spliced onto a half-encrypted record.
type wsSink struct {
	c    *ws.Conn
	tls  bool
	once sync.Once
}

func (s *wsSink) Write(b []byte) (int, error) {
	n, err := s.c.WriteMessage(ws.OpText, bytes.TrimRight(b, "\n"))
	if err != nil && s.tls && n == 0 {
		// See the type comment: a TLS write that reports nothing written may
		// still have put a record on the wire, so it is reported torn.
		n = 1
	}
	return n, err
}

func (s *wsSink) SetWriteDeadline(t time.Time) error { return s.c.SetWriteDeadline(t) }

// Close sends the close frame the reason deserves and then drops the
// connection.
//
// A torn or failed write gets NO frame: the peer is already looking at a
// half-written one, and adding another is not going to be read correctly. The
// TCP close is the honest end there.
func (s *wsSink) Close(reason string) {
	s.once.Do(func() {
		if code := closeCodeFor(reason); code != 0 && !s.c.ReceivedClose() {
			_ = s.c.SetWriteDeadline(time.Now().Add(time.Second))
			_ = s.c.WriteClose(code, closeReason(reason))
		}
		_ = s.c.Close()
	})
}

// closeCodeFor maps the hub's own reason strings onto RFC 6455 close codes.
//
// No auth failure appears here, and none can: every credential decision is made
// before the upgrade, as an HTTP status.
func closeCodeFor(reason string) int {
	switch {
	case strings.Contains(reason, "slow_consumer"):
		return ws.CloseTryAgainLater
	case strings.Contains(reason, "torn"), strings.Contains(reason, "write failed"):
		return 0
	case strings.HasPrefix(reason, "shutdown"):
		return ws.CloseGoingAway
	case strings.Contains(reason, "too large"), strings.Contains(reason, "exceeds"):
		return ws.CloseMessageTooBig
	}
	return ws.CloseNormal
}

func closeReason(r string) string {
	if len(r) > 100 {
		return r[:100]
	}
	return r
}

// handleWS serves GET /v1/ws.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Everything that can refuse does so as an HTTP status, BEFORE the upgrade.
	// That is the rule the whole auth design rests on: a client never has to
	// read a close code to learn its credential was wrong, and a browser — which
	// cannot read one reliably — sees an ordinary 401.
	cred, ok := s.authenticate(w, r, useTicket)
	if !ok {
		return
	}
	if err := ws.CheckUpgrade(r); err != nil {
		writeErr(w, protocol.Errf(protocol.CodeBadRequest, "%v", err))
		return
	}
	if s.cfg.Hub.Closing() {
		writeErr(w, protocol.Errf(protocol.CodeNotReady, "magmux is shutting down"))
		return
	}
	if !s.cfg.Ready(LayoutWait) {
		writeErr(w, protocol.Errf(protocol.CodeNotReady, "magmux has no layout yet"))
		return
	}
	hj, canHijack := w.(http.Hijacker)
	if !canHijack {
		// Only reachable if net/http served this over HTTP/2, which the server
		// disables in two places precisely so this cannot happen.
		writeErr(w, protocol.Errf(protocol.CodeUnsupported, "this connection cannot be upgraded"))
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		writeErr(w, protocol.Errf(protocol.CodeInternal, "hijack: %v", err))
		return
	}

	echo := ""
	if ws.OffersSubprotocol(r.Header) {
		// Echo magmux.v1 and NOTHING else. The `magmux.auth.<token>` entry
		// beside it is a credential, and echoing it would put the token in a
		// response header and therefore in every proxy log on the way back.
		echo = ws.Subprotocol
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteHandshake(conn, strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key")), echo); err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})

	c := ws.NewConn(conn, brw.Reader, MaxMessage)
	sink := &wsSink{c: c, tls: r.TLS != nil}
	sess := &wsSession{
		srv:    s,
		conn:   c,
		sink:   sink,
		kind:   cred.kind,
		caller: s.caller(r, "ws", cred, cred.kind.ReadOnly()),
	}
	sess.sub = s.cfg.Hub.Session(sess.caller, sink, nil)
	sess.seen.Store(time.Now().UnixNano())

	// The aggregate is the first frame, exactly as it is the first line on the
	// socket: a client seeds its whole pane map from it and is never told that
	// state again except on change.
	sess.sub.Start(s.cfg.Aggregate())

	stop := make(chan struct{})
	go sess.keepalive(stop)
	sess.read()
	close(stop)

	sess.sub.Close("client disconnected")
	<-sess.sub.Done()
	sink.Close("client disconnected")
}

// wsSession is one connection's reader: it turns text frames into Sub calls and
// answers ping, pong and close.
type wsSession struct {
	srv    *Server
	conn   *ws.Conn
	sink   *wsSink
	sub    *hub.Sub
	kind   auth.Kind
	caller hub.Caller
	seen   atomic.Int64
}

// wsRequest is the one inbound shape. It is deliberately the socket's `call`
// with the envelope flattened: an id, an op and its args.
type wsRequest struct {
	ID        json.RawMessage `json:"id"`
	Op        string          `json:"op"`
	Args      json.RawMessage `json:"args"`
	TimeoutMs int             `json:"timeoutMs"`
}

func (s *wsSession) read() {
	for {
		// The read deadline is the other half of the keepalive: without it a
		// half-open connection parks this goroutine forever and the pinger's
		// own writes would be the only thing that ever noticed.
		_ = s.conn.SetReadDeadline(time.Now().Add(pongTimeout + pingInterval))
		op, msg, err := s.conn.ReadMessage()
		if err != nil {
			if code := ws.CloseCodeOf(err); code != 0 {
				_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
				_ = s.conn.WriteClose(code, err.Error())
			}
			return
		}
		s.seen.Store(time.Now().UnixNano())
		switch op {
		case ws.OpPing:
			_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := s.conn.WriteControl(ws.OpPong, msg); err != nil {
				return
			}
		case ws.OpPong:
			// Liveness, already recorded above.
		case ws.OpClose:
			code, _, perr := ws.ParseClose(msg)
			if perr != nil {
				_ = s.conn.WriteClose(ws.CloseCodeOf(perr), perr.Error())
				return
			}
			if code == ws.CloseNoStatus {
				code = ws.CloseNormal
			}
			// The close is echoed, as §5.5.1 requires, and nothing follows it.
			_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = s.conn.WriteClose(code, "")
			return
		case ws.OpBinary:
			_ = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
			_ = s.conn.WriteClose(ws.CloseUnsupportedData, "magmux speaks text JSON")
			return
		case ws.OpText:
			s.dispatch(msg)
		}
	}
}

// keepalive pings on an interval and drops a peer that has gone quiet.
func (s *wsSession) keepalive(stop <-chan struct{}) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.sub.Done():
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, s.seen.Load())) > pongTimeout {
				s.sink.Close("no pong within " + pongTimeout.String())
				return
			}
			_ = s.conn.SetWriteDeadline(now.Add(5 * time.Second))
			if err := s.conn.WriteControl(ws.OpPing, nil); err != nil {
				return
			}
		}
	}
}

// dispatch runs one request. It never blocks the reader: Sub.Call places the
// work — inline for the watch verbs, on the pane's lane for input and send, on
// its own goroutine otherwise — and returns.
func (s *wsSession) dispatch(msg []byte) {
	var req wsRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		s.send(nil, nil, protocol.Errf(protocol.CodeBadRequest, "a message must be a JSON object (%v)", err))
		return
	}
	req.Op = strings.TrimSpace(req.Op)
	if req.Op == "" {
		s.send(req.ID, nil, protocol.Errf(protocol.CodeBadRequest,
			`a message needs an op: {"id":1,"op":"list"}`))
		return
	}

	// hello is adapter-level: it is not a registered op, it never appears in
	// /v1/ops or in a tool list, and it exists only so a client can name itself
	// for the control panel after the connection is already up.
	if req.Op == "hello" {
		s.hello(req)
		return
	}

	readOnly, err := s.srv.authorizeOp(s.kind, req.Op)
	if err != nil {
		s.send(req.ID, nil, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), wsTimeout(req.TimeoutMs))
	id := req.ID

	if s.kind.ReadOnly() && !readOnly {
		// --view-op granted this op to viewers, and the Sub's Caller is
		// read-only for the life of the connection. The grant is carried by
		// calling the hub directly with a Caller built for this ONE call, which
		// is exactly what Sub.Call would do for an ordinary op — a granted op
		// is always a plugin op (the flag requires a `plugin.op` name), so it
		// is never lane-bound and nothing about ordering changes.
		c := s.caller
		c.ReadOnly = false
		go func() {
			defer cancel()
			result, callErr := s.srv.cfg.Hub.Call(ctx, c, req.Op, req.Args)
			s.send(id, result, callErr)
		}()
		return
	}

	s.sub.Call(ctx, req.Op, req.Args, func(result map[string]any, err error) []byte {
		// Exactly once per call on every path, including the timeout, so the
		// budget is released neither early nor never.
		cancel()
		return replyBytes(id, result, err)
	})
}

func (s *wsSession) hello(req wsRequest) {
	var a struct {
		Client string `json:"client"`
	}
	if len(req.Args) > 0 {
		_ = json.Unmarshal(req.Args, &a)
	}
	if name := clientLabel(a.Client); name != "" {
		s.sub.SetClient(name)
	}
	s.send(req.ID, map[string]any{
		"protocol":  protocol.Version,
		"client":    s.sub.Caller().Client,
		"readOnly":  s.kind.ReadOnly(),
		"transport": "ws",
	}, nil)
}

func (s *wsSession) send(id json.RawMessage, result map[string]any, err error) {
	if b := replyBytes(id, result, err); len(b) > 0 {
		s.sub.Send(b)
	}
}

func wsTimeout(ms int) time.Duration {
	switch {
	case ms <= 0:
		return wsCallTimeout
	case time.Duration(ms)*time.Millisecond > wsCallMax:
		return wsCallMax
	}
	return time.Duration(ms) * time.Millisecond
}

// replyBytes renders one reply as the message it goes out as, or nil when the
// request carried no id and therefore asked for no answer.
//
// The shape is byte-for-byte the socket's (mux.replyBytes): a client that works
// against one transport works against the other, and a reply is a reply
// wherever it is read. It is written out here rather than shared because the
// transport packages must not import mux.
func replyBytes(id json.RawMessage, result map[string]any, err error) []byte {
	if len(id) == 0 {
		return nil
	}
	reply := map[string]any{"type": "reply", "id": id, "ok": err == nil}
	switch {
	case err != nil:
		reply["code"] = protocol.CodeOf(err)
		reply["error"] = err.Error()
	case result != nil:
		reply["result"] = result
	}
	data, mErr := json.Marshal(reply)
	if mErr != nil {
		data, mErr = json.Marshal(map[string]any{
			"type": "reply", "id": id, "ok": false,
			"code": protocol.CodeInternal, "error": "reply payload could not be encoded",
		})
		if mErr != nil {
			return nil
		}
	}
	return append(data, '\n')
}
