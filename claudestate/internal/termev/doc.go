// Package termev is the terminal-events source: the half of the observation
// layer that derives facts from a pane's own PTY traffic, with no
// help from the tool running inside it.
//
// It holds two things.
//
// # The staleness cascade
//
// A ladder of one-shot thresholds — 5s, 10s, 30s, 1m, 5m, 30m, 1h — armed from
// the instant a pane last produced output, re-armed each time more output
// arrives, and cancelled when someone types into the pane. Each threshold
// emits at most once per quiet period and names the threshold it crossed, so a
// consumer can say "silent for five minutes" without having to time anything
// itself.
//
// It is NOT a ticker and NOT a poll loop. A ticker would wake once per period
// per pane forever, which is exactly the cost magmux's dirty-flag rendering
// exists to avoid: an idle pane must cost nothing. At most ONE timer is alive
// per cascade at any moment, and a cascade that has exhausted its ladder or
// been cancelled by input holds none at all. TestNoTickerAnywhereInTheObsLayer
// pins that by reading the source.
//
// # Screen diffs
//
// [Diffs] keeps the previous frame, the current frame and the delta between
// them, all bounded. The delta exists so a consumer doing heuristics can tell
// "nothing changed at all" from "one rune on one row changed" — a spinner —
// from "the screen was rewritten". Counting the difference is the only way to
// make that distinction, because magmux's dirty flag says a repaint is due and
// says nothing about how much moved.
//
// # What is not here
//
// No host types. Nothing in this package imports a pane model, and it
// cannot: the library knows no pane, and the host's port implementations are
// the only code that knows both. A frame arrives as rows of text and an
// activity instant arrives as a [time.Time]; where those came from is the
// caller's business.
//
// # Concurrency
//
// No mutexes, no atomics, no goroutines and no timers. [Cascade] and [Diffs]
// are plain single-owner values with no synchronisation of any kind — the
// goroutine that made one is the only goroutine that may touch it.
//
// There is nothing here that OWNS a cascade, and there deliberately is not.
// Phase 1 shipped a Driver that put a goroutine and a one-shot timer around
// one, said in its own documentation that it was temporary, and named the
// per-session actor as the thing that would replace it. That actor exists, and
// phase 5 moved the cascade under it: verdict holds one cascade
// per session, and the actor that owns that stage arms the single one-shot
// timer the stage asks for. The Driver had no caller left afterwards and was
// deleted rather than kept as a second, unused owner of the same pattern —
// which is how two owners of one state machine drift apart.
//
// # Time
//
// Every instant this package acts on is CARRIED BY the event that reports it.
// [Cascade.Activity] takes the moment the output arrived, not the moment the
// cascade heard about it, because those differ by however long the report sat
// in a channel and the whole point of the ladder is to measure the former. The
// ports.Clock port exists to CREATE TIMERS, not to answer "what time is
// it"; see its own documentation.
package termev
