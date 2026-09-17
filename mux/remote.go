package mux

// Remote control over HTTP: where --listen is turned into a bound listener, a
// token and a server.
//
// This file is the ONLY place mux and the transport packages meet. The
// direction is strictly one way — mux registers itself INTO httpapi through
// three callbacks (the aggregate, the layout wait, the error log) and httpapi
// never hears of a Pane, a Screen or a treeMu. That is what
// TestImportDirection enforces, and it is why the HTTP surface can be tested
// against a bare hub with no terminal at all.
//
// The ORDER in this file is its other point, and it is a security order rather
// than a stylistic one. Everything here runs before mux.init():
//
//  1. the tokens are resolved, and a generated one is written to its final name;
//  2. the TLS pair is loaded, so a typo in a path is a message and not a
//     handshake failure an hour later;
//  3. the insecure-bind warning is printed to stderr;
//  4. the listener binds.
//
// So an accepting port implies a final token file on disk, which is the
// readiness signal an end-to-end harness uses: poll the port, then read the
// token, with no window in which the file is missing or half-written. And every
// failure is a plain line on stderr and an exit 1, while magmux still owns a
// normal terminal — after init() there is an alternate screen in the way and
// raw mode on top of it.

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"

	"github.com/MadAppGang/magmux/auth"
	"github.com/MadAppGang/magmux/buildinfo"
	"github.com/MadAppGang/magmux/transport/httpapi"
)

// remoteOptions is --listen and everything that qualifies it, straight off the
// command line.
type remoteOptions struct {
	listen        string
	tokenFile     string
	viewTokenFile string
	viewOps       []string
	tlsCert       string
	tlsKey        string
	allowOrigins  []string
}

// enabled reports whether the operator asked for remote control at all. Without
// --listen nothing in this file runs: no token is generated, no file is
// written, no port is opened.
func (o remoteOptions) enabled() bool { return o.listen != "" }

// remote is a prepared but not yet serving HTTP surface.
type remote struct {
	srv    *httpapi.Server
	tokens *auth.Resolved
	once   sync.Once
}

// prepareRemote resolves the tokens and binds the listener. It returns an error
// rather than exiting so its caller can print one message in one place.
func (m *Magmux) prepareRemote(opts remoteOptions) (*remote, error) {
	tokens, err := auth.Resolve(auth.Config{
		EnvToken:      os.Getenv("MAGMUX_TOKEN"),
		TokenFile:     opts.tokenFile,
		EnvViewToken:  os.Getenv("MAGMUX_VIEW_TOKEN"),
		ViewTokenFile: opts.viewTokenFile,
		DefaultPath:   auth.DefaultPath(m.socketDir(), m.remoteID()),
		ViewOps:       opts.viewOps,
	})
	if err != nil {
		return nil, err
	}

	// Everything from here can fail, and every failure has to take the
	// generated token file with it: a file naming a credential for a port
	// nothing is listening on is litter that looks like a secret.
	fail := func(err error) (*remote, error) {
		tokens.Cleanup()
		return nil, err
	}

	if (opts.tlsCert == "") != (opts.tlsKey == "") {
		return fail(fmt.Errorf("--tls-cert and --tls-key must be given together"))
	}

	if opts.tlsCert == "" && !httpapi.AddrIsLoopback(opts.listen) {
		// S3, and it is deliberately loud, unconditional and on stderr. A pane
		// is a shell, so this bind is remote code execution for anyone who can
		// reach the port AND read the token off the wire — which, without TLS,
		// is anyone on the path. It is a warning and not a refusal because a
		// Tailscale or a WireGuard interface is a legitimate place to bind and
		// magmux cannot tell one from a coffee-shop LAN.
		fmt.Fprintf(os.Stderr,
			"magmux: WARNING: --listen %s is not loopback and --tls-cert is not set.\n"+
				"magmux: The token and every keystroke cross the network in clear text,\n"+
				"magmux: and a pane is a shell. Use --tls-cert/--tls-key, or bind\n"+
				"magmux: 127.0.0.1 and reach it over ssh or a VPN.\n", opts.listen)
	}

	srv, err := httpapi.New(httpapi.Config{
		Addr:         opts.listen,
		Hub:          m.bus(),
		Tokens:       tokens.Store,
		Tickets:      auth.NewTickets(0),
		AllowOrigins: opts.allowOrigins,
		TLSCert:      opts.tlsCert,
		TLSKey:       opts.tlsKey,
		// net/http's own diagnostics — a TLS handshake against a plain port, a
		// client that wrote garbage — must NEVER reach stderr after init().
		// magmux may be holding a raw-mode terminal with an alternate screen on
		// it, and a log line printed into that corrupts the frame with no way
		// to repaint it. dbgFile or nothing.
		ErrorLog:   log.New(debugWriter{}, "http: ", 0),
		Aggregate:  m.remoteAggregate,
		Ready:      m.waitLayoutReady,
		Transports: remoteTransports(opts),
	})
	if err != nil {
		return fail(err)
	}
	return &remote{srv: srv, tokens: tokens}, nil
}

// remoteID is the name a generated token file takes, and it is the SAME id the
// socket uses: --id if there is one, else the pid. One flag then names both
// files, and the startup sweep can reason about both from one listing.
func (m *Magmux) remoteID() string {
	if m.sockID != "" {
		return m.sockID
	}
	return strconv.Itoa(os.Getpid())
}

// remoteAggregate is the connect-time aggregate, in exactly the shape the bus
// carries an event: one line of JSON with its newline.
//
// It is the same bytes handleSocketConn puts in a socket Sub's head, built by
// the same buildPaneResults, so a WebSocket client and a socket client are told
// the same thing in the same words. Each sink then reframes it — a WS text
// frame, an SSE `data:` line — which is the one thing a transport owns.
func (m *Magmux) remoteAggregate() []byte {
	return eventLine(map[string]any{
		"type":  "snapshot",
		"panes": m.buildPaneResults(),
	})
}

// remoteTransports is the block `capabilities` reports, so a client can tell a
// TLS bind from a plaintext one without probing for it.
func remoteTransports(opts remoteOptions) map[string]any {
	h := map[string]any{
		"listen":   opts.listen,
		"tls":      opts.tlsCert != "",
		"insecure": opts.tlsCert == "" && !httpapi.AddrIsLoopback(opts.listen),
		"endpoints": []string{
			"/v1/capabilities", "/v1/ops", "/v1/panes", "/v1/ops/{name}",
			"/v1/panes/{n}/screen", "/v1/tickets", "/v1/events", "/v1/ws",
		},
	}
	if len(opts.allowOrigins) > 0 {
		h["allowOrigin"] = opts.allowOrigins
	}
	return map[string]any{"http": h, "version": buildinfo.Version}
}

// serve starts accepting. It is called after the socket server, at the same
// point in startup, so both transports come up together.
func (r *remote) serve() {
	if r == nil {
		return
	}
	go r.srv.Serve()
}

// cleanup closes the listener and removes the token file magmux generated.
//
// It is NOT the graceful end of a stream: by the time anything calls this the
// hub's Finalize has already given every subscriber results, shutdown and EOF
// on whatever transport it was on. What is left is a socket to drop and a
// credential to stop existing.
//
// Idempotent, because it is reached from a defer, from the failure paths that
// os.Exit past that defer, and from the normal end of Main.
func (r *remote) cleanup() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.srv.Close()
		r.tokens.Cleanup()
	})
}

// tokenPath is the generated token file, or "" when the token came from the
// environment or from the operator's own file. Printed at startup so a human
// knows where to look.
func (r *remote) tokenPath() string {
	if r == nil || r.tokens == nil {
		return ""
	}
	return r.tokens.GeneratedPath
}

// debugWriter routes a log line to dbgFile if there is one, and discards it
// otherwise.
//
// It resolves dbgFile at WRITE time on purpose: the http.Server is built before
// init(), which is where MAGMUX_DEBUG opens the file, so a logger bound to the
// value at construction would be bound to nil forever.
type debugWriter struct{}

func (debugWriter) Write(p []byte) (int, error) {
	if dbgFile != nil {
		return dbgFile.Write(p)
	}
	return io.Discard.Write(p)
}

// remoteNote is the one line magmux prints about a listener it opened.
//
// On stderr, never stdout: a headless magmux writes zero bytes to stdout and
// that rule has no exceptions. It names the URL and, when magmux generated the
// token, the file to read it from — without ever printing the token itself,
// which would put it in every scrollback and every CI log.
func (r *remote) note(w io.Writer) {
	if r == nil {
		return
	}
	fmt.Fprintf(w, "magmux: listening on %s\n", r.srv.URL())
	if p := r.tokenPath(); p != "" {
		fmt.Fprintf(w, "magmux: token in %s (mode 0600, removed at exit)\n", p)
	}
}
