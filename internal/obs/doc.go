// Package obs is magmux's session-observation core: the vocabulary in which
// magmux describes what an agent session is doing, independent of which tool
// produced it.
//
// The core owns its OWN vocabulary. Nothing here DECODES a key or a value that
// belongs to an observed tool: every one of those strings is read in exactly
// one file — internal/obs/adapters/schema.go — and reaches the core only
// through that file's decoders, which is NFR6 made enforceable by
// TestOnlySchemaFileDecodesClaudeStrings.
//
// Some of the core's own wire words are spelled the same as Claude's, on
// purpose (see statusEnum). They are homographs, not copies: ownership is
// about which side a rename moves, not about spelling, which is why the rule
// is enforced against decode sites. The core also cannot import package
// main (Go forbids importing main), which is the point: magmux's pane model
// and this session model are different things, and only the adapters know
// both.
//
// Three rules run through every type in this package.
//
// # The zero value is always "unknown", never a negative
//
// A status nobody determined is [StatusUnknown], not [StatusIdle]. A count
// nobody took is a nil *int, not 0. An attention flag nobody could decide is a
// nil *bool, not false. "No opinion" and "no" are different answers and the
// model may not collapse them: a default that is right most of the time on
// live data is wrong precisely in the case a user is relying on it. Every
// optional value here is therefore either a pointer, whose nil means
// undetermined, or an enum whose zero value is Unknown — and both marshal to
// JSON null, so the distinction survives onto the wire.
//
// # Nothing time-derived is stored
//
// A [Session] carries the instants it was told about and nothing computed from
// them. A duration frozen into the model is wrong the moment it is read, and
// the registry this model is built from mutates between reads — which is why a
// [Fleet] carries one snapshot epoch for the whole document rather than
// presenting its numbers as durable. Durations are computed at encode time
// from the reader's clock; see [Session.InStatusFor].
//
// # Foreign data degrades; our own data does not
//
// An unrecognised value arriving from a tool's on-disk format yields "no
// opinion" and the raw string is kept, because those formats are undocumented
// and will change. An unrecognised value in this package's own JSON is a
// defect and is reported as an error. The lenient path stops at the adapter
// boundary deliberately.
//
// # The one thing here that is not a value: the [Clock] port
//
// [Clock], [Timer] and [FakeClock] are the layer's shared seam onto WAITING,
// and they live in the core rather than in whichever source needed one first.
// They began in termev, which was the only thing that had to wait; fswatch's
// coalescing window then needed the same seam, and two sources with two copies
// of one test seam is how the copies drift. termev keeps type aliases, so
// nothing written against the old spelling had to move.
//
// An interface adds no goroutine, no mutex and no atomic, so the statement
// below still holds exactly as it did. And the port is deliberately not "what
// time is it": it CREATES TIMERS. Every instant this layer acts on is carried
// by the event that reports it, and a caller needing an instant that MOVES
// between reads injects its own func() time.Time rather than asking the clock
// — see [Clock] and registry.Config.Now, which document the trap.
//
// # Concurrency
//
// This package adds no goroutines, no mutexes and no atomics. The concurrency
// model — one actor goroutine per pane, channels only, and the three freeze
// points — arrives later; the types here are plain values precisely so that a
// document can be deep-copied once (see [Fleet.Clone]) and then read without
// any synchronisation at all. [FakeClock] is held to the same rule and has no
// mutable state of any kind; its own documentation says how.
package obs
