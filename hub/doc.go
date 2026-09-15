// Package hub is magmux's application port: the one place an op is registered
// and called, and the one place an event becomes bytes on somebody's
// connection.
//
// It knows nothing about terminals and nothing about transports. mux registers
// its built-in ops INTO the hub and publishes its events through it; the unix
// socket, and later HTTP/WS/SSE and Firebase, are driving adapters that hand
// the hub a Caller and a Sink. That direction is enforced by
// cmd/magmux/import_direction_test.go: this package must never import mux.
//
// There are two halves.
//
// The REGISTRY maps an op name to an OpSpec (what it is, and who may call it)
// and an OpFunc (what it does). Register/UnregisterSource are per SOURCE —
// "magmux" for the built-ins, a plugin's name otherwise — so a plugin that
// dies takes exactly its own ops with it, and Rev changes so a client can tell
// that the op list it cached is stale.
//
// The BUS is a fan-out with no broker semantics: one process, no retry, no
// replay. Publish takes already-marshalled bytes and hands the same slice to
// every live Sub without blocking. Each Sub owns a bounded FIFO and one writer
// goroutine, so a subscriber that stops reading fills its own queue and is
// closed, and nobody else notices. At shutdown Finalize discards every
// backlog, jumps the final messages to the front and bounds the whole teardown
// with one absolute deadline, which is what makes the results -> shutdown ->
// EOF ordering hold whatever the backlog was.
//
// hub.mu is a LEAF. It is never held across an OpFunc, a Sink call or anything
// else that can block, and no lock of mux's is ever taken while it is held.
// The only lock order inside this package is hub.mu -> sub.mu.
//
// This package is an implementation detail of magmux: no API stability is
// promised before v1. The two packages meant for other programs are protocol
// and client.
package hub
