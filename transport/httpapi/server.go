package httpapi

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/hub"
	"github.com/MadAppGang/magmux/protocol"
)

// Limits. Each of these is a per-connection or per-request bound, so one client
// spends its own budget and nobody else's — the same rule the hub applies to
// queues.
const (
	// MaxBody bounds a request body. 1 MB is generous for an op's args (the
	// largest realistic one is a pasted instruction) and small enough that 64
	// concurrent uploads cannot be a memory event.
	MaxBody = 1 << 20
	// MaxMessage bounds one WebSocket message after reassembly. Deliberately
	// the same number as MaxBody: a client must not be able to get a larger
	// payload in by choosing the other transport.
	MaxMessage = 1 << 20
	// MaxConns bounds simultaneous connections. Past it a connection is still
	// ACCEPTED and answered 503 busy with Retry-After, rather than being
	// dropped at the listener: a client that gets a TCP reset cannot tell an
	// overloaded magmux from one that is not running.
	MaxConns = 64
	// ReadHeaderTimeout is the slowloris bound. It is the one timeout that can
	// be absolute here — a request's BODY may legitimately be slow, and a
	// WebSocket or an SSE stream has no end at all.
	ReadHeaderTimeout = 10 * time.Second
	// LayoutWait is how long a streaming connection waits for the session
	// layout before it is answered not_ready. It matches the socket's own
	// layoutReadyTimeout, so the two transports do not disagree about when
	// magmux is up.
	LayoutWait = 5 * time.Second
)

// Config is everything the server needs from magmux. None of it is a *Magmux:
// this package must not import mux, and the three things it genuinely needs
// from the core arrive as functions.
type Config struct {
	// Addr is the bind address, as given to --listen.
	Addr string
	// Hub is the op registry and the bus. Required.
	Hub *hub.Hub
	// Tokens checks credentials and answers the view-token questions.
	Tokens *auth.Store
	// Tickets is the single-use exchange for EventSource and browser
	// WebSockets.
	Tickets *auth.Tickets
	// AllowOrigins is --allow-origin, exact scheme://host[:port] values. CORS
	// headers are emitted for these and for nothing else.
	AllowOrigins []string
	// TLSCert / TLSKey enable https. Both or neither; the caller validates the
	// pair before binding.
	TLSCert, TLSKey string
	// ErrorLog is where net/http's own diagnostics go. It must NOT be stderr:
	// magmux may be holding a raw-mode terminal, and a TLS handshake error
	// printed into it corrupts the frame. nil discards.
	ErrorLog *log.Logger
	// Aggregate returns the connect-time aggregate as ONE line-JSON message,
	// newline included — exactly the shape the bus carries, so each sink
	// reframes it the same way it reframes an event.
	Aggregate func() []byte
	// Ready blocks until the session layout exists and reports whether it does.
	// Streaming endpoints answer 503 not_ready when it does not.
	Ready func(timeout time.Duration) bool
	// MaxConns overrides the connection cap. 0 takes MaxConns.
	MaxConns int
	// Transports is the per-transport block `capabilities` reports, so a client
	// can tell an insecure bind from a TLS one without probing.
	Transports map[string]any
}

// Server is a bound listener and the handler over it. New binds; Serve starts
// accepting. They are separate because the BIND must happen before magmux takes
// over the terminal — a port already in use has to be a plain stderr message
// and an exit 1, not a failure discovered behind an alternate screen.
type Server struct {
	cfg      Config
	ln       net.Listener
	srv      *http.Server
	loopback bool

	conns   atomic.Int64
	connSeq atomic.Uint64

	closeOnce sync.Once
}

// New validates the configuration and binds the listener.
func New(cfg Config) (*Server, error) {
	if cfg.Hub == nil {
		return nil, fmt.Errorf("httpapi: a hub is required")
	}
	if cfg.Tokens == nil {
		return nil, fmt.Errorf("httpapi: a token store is required; magmux never listens without one")
	}
	if cfg.Tickets == nil {
		cfg.Tickets = auth.NewTickets(0)
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = MaxConns
	}
	if cfg.Ready == nil {
		cfg.Ready = func(time.Duration) bool { return true }
	}
	if cfg.Aggregate == nil {
		cfg.Aggregate = func() []byte { return nil }
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, fmt.Errorf("--tls-cert and --tls-key must be given together")
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	s := &Server{cfg: cfg, loopback: listenerIsLoopback(ln)}
	s.ln = &countingListener{Listener: ln, srv: s}

	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("tls certificate: %w", err)
		}
		s.ln = tls.NewListener(s.ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// HTTP/1.1 only, and this is load-bearing rather than
			// conservatism: a WebSocket needs http.Hijacker, and an HTTP/2
			// stream cannot be hijacked. Negotiating h2 here would make every
			// upgrade fail under TLS and work in plaintext.
			NextProtos: []string{"http/1.1"},
		})
	}

	errLog := cfg.ErrorLog
	if errLog == nil {
		errLog = log.New(discard{}, "", 0)
	}
	s.srv = &http.Server{
		Handler:           s.handler(),
		ErrorLog:          errLog,
		ReadHeaderTimeout: ReadHeaderTimeout,
		// The same reason as NextProtos above, for the non-TLS path and for
		// belt and braces on the TLS one: an empty map disables every
		// negotiated protocol upgrade net/http would otherwise install.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connCtxKey{}, c)
		},
	}
	return s, nil
}

// Addr is where the server actually bound. With a port of 0 it is the one the
// OS chose, which is how a test finds it.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Loopback reports whether the bind is reachable only from this machine. It
// decides whether the Host check applies and whether a plaintext bind deserves
// the insecure warning.
func (s *Server) Loopback() bool { return s.loopback }

// URL is the base URL a client should use.
func (s *Server) URL() string {
	scheme := "http"
	if s.cfg.TLSCert != "" {
		scheme = "https"
	}
	return scheme + "://" + s.Addr().String()
}

// Serve accepts until the listener is closed. It blocks; callers run it on its
// own goroutine.
func (s *Server) Serve() {
	_ = s.srv.Serve(s.ln)
}

// Close drops the listener and every connection at once.
//
// It is deliberately NOT http.Server.Shutdown: Shutdown waits for active
// handlers, and an SSE handler blocks for the life of its stream. The orderly
// end of a stream is the hub's Finalize (results, shutdown, EOF), which has
// already run by the time anything calls this; what is left is the socket, and
// holding it open would only delay the exit.
func (s *Server) Close() {
	s.closeOnce.Do(func() { _ = s.srv.Close() })
}

// ── connection accounting ───────────────────────────────────────────────────

type connCtxKey struct{}

// countingListener tracks how many connections are open, and marks the ones
// past the cap so the handler can answer them rather than the listener dropping
// them.
type countingListener struct {
	net.Listener
	srv *Server
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	n := l.srv.conns.Add(1)
	return &countedConn{Conn: c, srv: l.srv, over: n > int64(l.srv.cfg.MaxConns)}, nil
}

type countedConn struct {
	net.Conn
	srv  *Server
	over bool
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.srv.conns.Add(-1) })
	return c.Conn.Close()
}

// connOver reports whether this request arrived on a connection past the cap.
//
// Under TLS the connection in the context is the *tls.Conn, so the counted one
// is reached through NetConn. Reading it through an interface rather than a
// type switch on *tls.Conn keeps this working for any future wrapper.
func connOver(r *http.Request) bool {
	c, _ := r.Context().Value(connCtxKey{}).(net.Conn)
	for i := 0; c != nil && i < 4; i++ {
		if cc, ok := c.(*countedConn); ok {
			return cc.over
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok {
			return false
		}
		c = u.NetConn()
	}
	return false
}

// Conns is how many connections are open. For tests and the debug log.
func (s *Server) Conns() int { return int(s.conns.Load()) }

// ── routing and the outer middleware ────────────────────────────────────────

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /v1/ops", s.handleOps)
	mux.HandleFunc("GET /v1/panes", s.handlePanes)
	mux.HandleFunc("POST /v1/ops/{name}", s.handleCallOp)
	mux.HandleFunc("GET /v1/panes/{pane}/screen", s.handleScreen)
	mux.HandleFunc("POST /v1/tickets", s.handleTicket)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("GET /v1/ws", s.handleWS)
	mux.HandleFunc("/", s.handleNotFound)
	return s.wrap(mux)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, protocol.Errf(protocol.CodeUnknownVerb, "no such endpoint: %s %s", r.Method, r.URL.Path))
}

// wrap is everything that applies to every request, in the order a hostile
// request should meet it: capacity first (it costs nothing to refuse), then the
// two checks that decide whether this request is even addressed to us, then
// CORS.
//
// Authentication is deliberately NOT here. It is per-route, because the three
// credential channels differ per route — a Bearer header everywhere, a
// WebSocket subprotocol on /v1/ws, a ticket on /v1/ws and /v1/events — and a
// single middleware that understood all three would accept a ticket on
// endpoints that must never take one.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if connOver(r) {
			w.Header().Set("Retry-After", "1")
			// 503 rather than `busy`'s usual 429: see writeErrStatus. The
			// connection is ACCEPTED and answered rather than dropped at the
			// listener, because a client that gets a TCP reset cannot tell an
			// overloaded magmux from one that is not running.
			writeErrStatus(w, http.StatusServiceUnavailable, protocol.Errf(protocol.CodeBusy,
				"magmux is already serving %d connections", s.cfg.MaxConns))
			return
		}

		// DNS rebinding. A page on any origin can make the browser resolve its
		// own hostname to 127.0.0.1 and then talk to whatever is listening
		// there — Origin is the ATTACKER's, which our Origin check would catch,
		// but only if the request carries one, and a form post or an <img> does
		// not. Requiring the Host to be a loopback name closes it for the whole
		// class, and costs nothing: a loopback bind is by definition only
		// reachable as localhost.
		if s.loopback && !hostIsLoopback(r.Host) {
			writeErr(w, protocol.Errf(protocol.CodeForbidden,
				"Host %q is not a loopback name; magmux is bound to %s", r.Host, s.Addr()))
			return
		}

		origin := r.Header.Get("Origin")
		cors := origin != "" && s.corsOrigin(origin)

		if r.Method == http.MethodOptions {
			// Preflight needs no credential, by design: the browser sends it
			// WITHOUT the Authorization header it is asking permission to use,
			// so demanding one here would make every cross-origin request fail
			// before it was made.
			if !cors {
				writeErr(w, protocol.Errf(protocol.CodeForbidden,
					"origin %q is not in --allow-origin", origin))
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Magmux-Client")
			h.Set("Access-Control-Max-Age", "600")
			// No Access-Control-Allow-Credentials on purpose. magmux
			// authenticates with a bearer token and has no cookies, and
			// allowing credentials would tell the browser to attach ambient
			// ones it should never send here.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if origin != "" && !cors && !originIsSelf(origin, r) {
			writeErr(w, protocol.Errf(protocol.CodeForbidden,
				"origin %q is not allowed; pass --allow-origin %s to permit it", origin, origin))
			return
		}
		if cors {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}

// corsOrigin reports whether --allow-origin named this origin. The comparison
// is exact on scheme, host and port: a suffix or wildcard match here is how
// "allow example.com" becomes "allow notexample.com".
func (s *Server) corsOrigin(origin string) bool {
	for _, o := range s.cfg.AllowOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// originIsSelf reports whether the Origin is the request's own scheme and host,
// which is the same-origin case every non-browser client and every page served
// from magmux itself produces.
func originIsSelf(origin string, r *http.Request) bool {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.EqualFold(origin, scheme+"://"+r.Host)
}

// hostIsLoopback reports whether a Host header names this machine.
func hostIsLoopback(host string) bool {
	if host == "" {
		return false
	}
	h := host
	if strings.Count(h, ":") > 1 && !strings.HasPrefix(h, "[") {
		// A bare IPv6 literal with no brackets is not a legal Host value; a
		// client sending one is not a browser and gets the refusal.
		return false
	}
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// listenerIsLoopback reports whether a bound listener can be reached from
// another machine.
func listenerIsLoopback(ln net.Listener) bool {
	ta, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return false
	}
	// An unspecified address (0.0.0.0 / ::) is every interface, which is the
	// case the insecure warning exists for.
	if ta.IP == nil || ta.IP.IsUnspecified() {
		return false
	}
	return ta.IP.IsLoopback()
}

// AddrIsLoopback answers the same question for an address that has not been
// bound yet, which is what the pre-init() warning needs.
func AddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		// ":8080" is every interface.
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A name magmux cannot resolve here. Treated as NOT loopback, which is
		// the safe direction: the worst outcome is a warning the operator did
		// not need.
		return false
	}
	return ip.IsLoopback()
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
