// Package firebase mirrors a magmux session into a Firebase Realtime Database
// and, optionally, executes verified commands read back out of it.
//
// It is a driving adapter in the same sense as the socket and the HTTP server:
// it holds one hub.Sub, its Sink is a write-coalescing map rather than a
// connection, and it never hears of a Pane, a Screen or a treeMu. The one thing
// that makes it different from every other adapter is that its peer is a
// DATABASE. There is no connection to close, no back-pressure to feel and no
// reader to block, so the bounds that a socket gets for free have to be built:
// a 500 ms flush tick, a token-bucket byte budget, a priority order that sheds
// frames before it sheds state, and a heartbeat that lets a reader tell a live
// session from a killed one.
//
// Three rules shape everything here.
//
//   - **A mirror is not a terminal.** Frames go last, at 2 fps by default, and
//     are the first thing cut when the byte budget runs out. A reader that
//     needs the real screen watches over WebSocket; this is for a phone on a
//     train.
//
//   - **The host predicate comes before the credential.** A service-account
//     token is admin on the whole database, so it must never be presented to a
//     host that only LOOKS like Firebase. IsDatabaseURL is an exact-label match
//     checked before any authenticated I/O, and the same predicate guards the
//     Authorization header across a redirect — Go strips it on a cross-host
//     307, and re-adding it unconditionally would hand the token to whoever
//     answered.
//
//   - **A command runs at most once.** Before any op that is not class read,
//     magmux writes a durable `claimed` result and waits for RTDB to
//     acknowledge it. A crash after the claim leaves a record that reads
//     "outcome unknown, and it will never run again", which is the honest
//     answer; a crash before it leaves a command that was never claimed and
//     never ran. Across a restart nothing replays at all, because `sid` carries
//     the process start time and the new session listens on a different path.
//
// It is an implementation detail of magmux, with no API stability before v1.
package firebase
