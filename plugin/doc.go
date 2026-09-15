// Package plugin is magmux's plugin host: it spawns plugin processes, accepts
// their registrations over the ordinary control socket, routes op invocations
// to them, and cleans up when they die.
//
// # What a plugin is
//
// A plugin is a separate process that connects to magmux's socket like any
// other client and then does one extra thing: it REGISTERS, declaring a name,
// a set of ops and a set of events. From that moment its ops are in magmux's op
// table under `<plugin>.<op>`, so every transport can call them — the socket's
// `call`, HTTP's POST /v1/ops/{name}, a WebSocket, an MCP dynamic tool — with
// no code in magmux that knows what the plugin does.
//
// The reverse direction is the point of this package. magmux sends the plugin
// an `invoke`, the plugin answers with an `invoke_result`, and magmux turns
// that into an ordinary op result with an ordinary protocol error code. A
// plugin that hangs is cancelled and reported as `timeout`; a plugin that dies
// with calls in flight leaves them as `plugin_gone`.
//
// # What this package must not know
//
// Nothing here imports the multiplexer. A pane, a screen, a controller and the
// layout lock are all on the other side of two callbacks (Config.Snapshot and
// Config.OnExit), which is what lets the whole host be tested against a bare
// hub with no terminal at all — and what makes the import-direction guard in
// cmd/magmux mean something.
//
// # Trust
//
// Registration is authenticated. A plugin magmux spawned proves itself with the
// one-time token magmux put in its environment; a plugin an operator ran by
// hand proves itself with the session's own token. Everything else a plugin
// claims — the pane it reports on, the events it emits — is checked against
// what it registered, because the socket's own access control is the filesystem
// and anything with a file descriptor can send these bytes.
package plugin
