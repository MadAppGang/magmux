// Package httpapi is magmux over HTTP: a small REST surface, a WebSocket
// session, and a server-sent-events stream, all of them adapters onto the same
// hub the unix socket uses.
//
// Nothing here knows what a pane IS. Every request becomes a hub op call or a
// hub Session, and the only translation this package owns is the one the hub
// deliberately does not: protocol codes to HTTP statuses, JSON bodies to op
// args, and a transport's framing to the bus's line-JSON.
//
// The security posture is the point of the package, because a pane is a shell:
//
//   - Every authentication, authorisation, Origin, Host and limit decision is
//     made BEFORE any upgrade, as an ordinary HTTP status. No WebSocket close
//     code ever carries an auth failure, which matters because a browser cannot
//     read one reliably and because a client that has to upgrade to learn it was
//     unauthorised has already been given a connection.
//   - The raw token never appears in a URL. EventSource and a browser WebSocket
//     cannot set a header, so they present a single-use 30-second ticket minted
//     by an authenticated POST instead.
//   - Origin is checked against --allow-origin or the request's own host, and
//     Host is checked on a loopback bind, which is what stops a page on the
//     internet driving a magmux on the developer's laptop through DNS
//     rebinding.
//   - http.Server.ErrorLog is routed away from stderr by the caller, because
//     magmux may be holding a raw-mode terminal and a TLS handshake error
//     printed into it corrupts the frame.
//
// It is an implementation detail of magmux, with no API stability before v1.
package httpapi
