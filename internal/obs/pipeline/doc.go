// Package pipeline is the observation layer's concurrency model: one actor
// goroutine per session, one hub that broadcasts the document, and the three
// freeze points that make a document safe to hand to a reader holding no lock.
//
// Everything upstream of here is a value. [github.com/MadAppGang/magmux/internal/obs]
// is plain data, fswatch and registry are single-owner, and phase 1's cascade
// is a state machine. This package is where those get a goroutine and an
// owner.
//
// # The rule the whole design rests on
//
//	The hub never blocks on a subscriber, which is what makes it safe for a
//	subscriber to block on the hub.
//
// A subscriber that cannot keep up is dropped from THAT broadcast — not
// unsubscribed — and the drop is counted and published, never silent. That one
// rule removes the deadlock argument entirely: no cycle can form because one
// direction never waits. It is the same rule, in the same words, that fswatch
// and termev already publish drops under, and the count is carried on the next
// delivered value for the same reason: a consumer that fell behind learns
// exactly how far and cannot mistake a gap for a quiet machine.
//
// # Three freeze points
//
//  1. MIDDLEWARE CHAIN EXIT. When a session's chain finishes, the fact set it
//     produced is cloned and that clone is the actor's state. The chain's own
//     copy is unreachable from the actor afterwards, so a middleware that kept
//     a pointer into it — and they do keep pointers; [obs.Session] is full of
//     them — cannot write into a fact set that has already been published.
//  2. HUB PUBLISH. The document is deep-copied ONCE, on the publisher's
//     goroutine, and the hub retains no reference to it after the broadcast
//     returns.
//  3. SINK HANDOFF. A sink may not retain a pointer past its own send. The
//     document it receives is shared with every other sink of that broadcast
//     (see below), so a sink that mutates it corrupts every other sink's copy.
//     Read it, derive from it, let it go.
//
// # One clone per publish, SHARED — the decision, and why
//
// "Deep-copied once" and "each sink gets its own copy" are different designs
// and this package implements the first: one [obs.Fleet.Clone] per publish,
// handed to every subscriber. Per-sink clones would cost N copies of a
// document that is a few hundred bytes per session times thirty sessions, on
// every record change, to defend against a sink that mutates what it was
// given — which is a rule a sink can simply follow, and which no amount of
// copying protects against once a sink hands its copy on to something else.
//
// What the shared clone buys is the property a test can actually assert: the
// hub HOLDS NOTHING after a publish. There is no "latest document" accessor
// here and no cached copy, so the object the subscribers share is unreachable
// from any writer rather than merely conventionally read-only. The clone
// happens on the publisher's goroutine, before the value crosses the channel,
// so a publisher that keeps and mutates its own document cannot race the copy
// either.
//
// # Ownership
//
// No mutexes and no atomics, here or anywhere under internal/obs. Every piece
// of mutable state in this package belongs to exactly one goroutine:
//
//   - an [Actor]'s goroutine owns its fact set, its middleware chain and its
//     counters. Nothing else can reach them: they are not fields anything
//     outside can read, and the only ways in are the event method and the
//     command methods.
//   - the [Hub]'s goroutine owns the subscriber list and every drop count.
//   - the [Pipeline]'s goroutine owns the registry source (which is documented
//     single-owner), the watcher, and the actor table.
//
// Reads of that state are COMMANDS WITH REPLY CHANNELS, not accessors, and
// that shape is established now rather than when something needs it: phase 6's
// actuator — a middleware that writes into a session — needs exactly this path,
// and retrofitting a command channel onto an event-only actor means rewriting
// every call site. The read side is the same mechanism as the write side will
// be, which is why [Actor.Snapshot] and [Hub.Stats] look the way they do
// instead of returning a field.
//
// # Waiting
//
// Nothing in this package waits on a clock. There is no ticker and no poll
// loop — a cycle happens because a record changed or because somebody asked
// for one. [obs.Clock] reaches the registry source through [Config] for its
// one-shot torn-read retry, and that is the only timer in the whole path.
package pipeline
