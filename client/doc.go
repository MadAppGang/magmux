// Package client is a Go client for a running magmux's control socket, the
// connection `magmux mcp` drives sessions through.
//
// Dial attaches to one magmux and returns a Session. A Session keeps a live
// view of every pane (State), seeded from the aggregate snapshot magmux sends
// first on every connection and updated from each event after it. It sends
// requests that magmux answers with one reply each: ListPanes, Capture,
// Transcript, SendKeys, OpenPane and ClosePane. A failed request's error
// carries magmux's code, which protocol.CodeOf reads. RunInstruction sends
// one instruction to an agent pane and waits for the turn it starts to
// finish.
//
// A magmux that predates replies answers every request with silence. The
// Session detects that (IsLegacy); after that, requests that need a reply fail
// with ErrLegacyMagmux and SendKeys falls back to a write nobody answers.
//
// The wire format and its error codes are package protocol's. This is one of
// magmux's two public packages, with protocol: the two meant for programs
// other than magmux.
package client
