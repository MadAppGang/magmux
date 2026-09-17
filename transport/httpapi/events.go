package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
	"github.com/MadAppGang/magmux/transport/sse"
)

// sseSink is one EventSource stream as the hub sees it.
//
// It is the sink that CANNOT honestly report n == 0, and the hub's torn-write
// rule turns on that fact. ResponseWriter.Write counts bytes accepted into the
// server's buffer, not bytes on the wire, and after a deadline a write "may
// succeed if the data has been buffered"; the real failure surfaces in Flush,
// by which time part of a `data:` line may already have gone out. So Write and
// Flush are treated as ONE message, and every failure of either is reported
// torn — n >= 1 — which closes the subscriber without finals rather than
// splicing `event: results` into a half-written line.
type sseSink struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	enc *sse.Writer

	mu     sync.Mutex
	id     uint64
	closed bool
	done   chan struct{}
	once   sync.Once
}

func newSSESink(w http.ResponseWriter) *sseSink {
	return &sseSink{
		w:    w,
		rc:   http.NewResponseController(w),
		enc:  sse.NewWriter(w),
		done: make(chan struct{}),
	}
}

// Write renders one bus message as one SSE event.
//
// The bus carries whole line-JSON messages, so the `event:` name is the
// message's own `type` field and `data:` is the JSON without its newline. The
// `id:` is a per-connection counter and NOT a replay cursor: magmux keeps no
// event log, Last-Event-ID is ignored, and a client that reconnects gets a
// fresh aggregate and fresh keyframes instead of a gap it could not detect.
func (s *sseSink) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(b), http.ErrBodyNotAllowed
	}
	s.id++
	payload := bytes.TrimRight(b, "\n")
	if _, err := s.enc.Event(eventTypeOf(payload), strconv.FormatUint(s.id, 10), payload); err != nil {
		return len(b), err
	}
	if err := s.rc.Flush(); err != nil {
		return len(b), err
	}
	return len(b), nil
}

// SetWriteDeadline reaches the underlying net.Conn, which it can because the
// server is HTTP/1.1 only (TLSNextProto is empty and NextProtos is http/1.1).
// On an HTTP/2 connection there would be no such conn and this would fail,
// which is one more reason h2 is disabled rather than merely unused.
func (s *sseSink) SetWriteDeadline(t time.Time) error { return s.rc.SetWriteDeadline(t) }

// Close ends the stream. There is no frame to send — the end of an SSE stream
// is the end of the body — so this only releases the handler, which returns and
// lets net/http finish the response.
func (s *sseSink) Close(reason string) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

// eventTypeOf reads the `type` field out of a bus message.
//
// A shallow decode into a one-field struct rather than a byte scan: a scan
// would have to understand escaping to be right, and this runs at most once per
// frame per subscriber, where the frame itself is orders of magnitude more
// work. An unnamed message defaults to "message", which is what EventSource
// dispatches to onmessage.
func eventTypeOf(b []byte) string {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Type == "" {
		return "message"
	}
	return env.Type
}

// handleEvents serves GET /v1/events: the aggregate, then every event, then the
// two finals, then the end of the body.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	cred, ok := s.authenticate(w, r, useTicket)
	if !ok {
		return
	}
	// Every refusal below is an HTTP status, and all of them happen before a
	// single byte of the stream is written: once the 200 is out there is no way
	// left to say "no".
	if s.cfg.Hub.Closing() {
		writeErr(w, protocol.Errf(protocol.CodeNotReady, "magmux is shutting down"))
		return
	}
	if !s.cfg.Ready(LayoutWait) {
		writeErr(w, protocol.Errf(protocol.CodeNotReady, "magmux has no layout yet"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nginx and several other proxies buffer a response body until it is
	// complete, which for a stream is never. This is the documented opt-out.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()

	sink := newSSESink(w)
	sub := s.cfg.Hub.Session(s.caller(r, "sse", cred, cred.kind.ReadOnly()), sink, nil)
	sub.Start(s.cfg.Aggregate())

	// ?watch= attaches the stream to panes. It is `watch` by another name, and
	// watch is class read, so a viewer may use it — which is the whole point of
	// the view token.
	if spec := r.URL.Query().Get("watch"); spec != "" {
		applyWatchSpec(sub, spec, fpsParam(r))
	}

	select {
	case <-sub.Done():
		// The hub finished with this subscriber: either the finals were written
		// or it was closed. Returning ends the body.
	case <-r.Context().Done():
		// The client went away. Close, not kill: a reply already queued is
		// still written (and lost at the socket, which is the peer's business),
		// and the lanes this connection filled still drain.
		sub.Close("client disconnected")
		<-sink.done
	}
}

// fpsParam reads ?fps=, leaving the clamping to the hub so one rule applies on
// every transport.
func fpsParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("fps"))
	return n
}

// applyWatchSpec turns `watch=3,4` or `watch=all` into subscriptions.
//
// These go through Sub.Watch rather than Sub.Call: there is no reply to order
// the first frame against on this transport, so the slot is activated
// immediately. A pane that cannot be watched is reported as an `error` event
// rather than failing the whole stream — a client asking for four panes, one of
// which just closed, wants the other three.
func applyWatchSpec(sub *hub.Sub, spec string, fps int) {
	if spec == "all" || spec == "*" {
		if err := sub.WatchAll(fps); err != nil {
			sub.Send(errorEvent(0, err))
		}
		return
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			sub.Send(errorEvent(0, protocol.Errf(protocol.CodeBadRequest,
				"watch: %q is not a pane index", part)))
			continue
		}
		if _, err := sub.Watch(n, protocol.WatchFrames, fps); err != nil {
			sub.Send(errorEvent(n, err))
		}
	}
}

// errorEvent is a bus-shaped message for a failure that has no request to be
// the reply to. It goes through the Sub's own queue, so it arrives after the
// aggregate like everything else.
func errorEvent(pane int, err error) []byte {
	ev := map[string]any{"type": "error", "code": protocol.CodeOf(err), "error": err.Error()}
	if pane > 0 {
		ev["pane"] = pane
	}
	b, merr := json.Marshal(ev)
	if merr != nil {
		return nil
	}
	return append(b, '\n')
}
