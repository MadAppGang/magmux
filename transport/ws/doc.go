// Package ws is a hand-written RFC 6455 server endpoint: the handshake, the
// frame codec, and a Conn that reassembles messages.
//
// It is hand-written because magmux's dependency rule is stdlib plus
// golang.org/x/sys and x/term, and a WebSocket library would be the first
// third-party runtime dependency in the binary (A4). The scope that makes that
// defensible is narrow: magmux is a SERVER, so it never masks a frame and never
// negotiates an extension, and it speaks one subprotocol it defined itself.
// Everything a browser client does is accepted; nothing else is.
//
// The split is deliberate. ReadFrame and WriteFrame are pure functions over an
// io.Reader / io.Writer with no net.Conn, no timers and no state, which is what
// makes FuzzWSReadFrame able to drive the whole parser from a byte slice. Conn
// adds exactly two things on top: continuation reassembly with a cap, and one
// write mutex.
//
// Protocol errors carry the close code they must be reported with, because a
// caller that has to map "the RSV bits were set" onto 1002 by itself will
// eventually map one of them onto 1000.
//
// It is an implementation detail of magmux, with no API stability before v1.
package ws
