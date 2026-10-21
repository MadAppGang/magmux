// Package claudecode is the Claude Code middleware: the stage that reads a
// session's own transcript and subagent files and adds what it finds to the
// fact set.
//
// # Why this is the phase that matters
//
// Everything upstream of here is TERMINAL-derived — what the screen looked
// like, how long it has been quiet, whether a process is alive. The
// architecture calls that `inferred`, and it is a guess about a session made
// from the outside. This package produces HARNESS-derived facts: the tool's
// own record of what it did. Which session needs a human, why, and what went
// wrong are all answerable only from here.
//
// # A middleware reads deeply, and will also write
//
// The user's framing, and it decides the shape:
//
//	on top of primitives this plugin should have deep full access to claude
//	code session … some plugin which parses conversation in real time and
//	provides information about the state, which can be reused by everyone
//
// and, separately: it should be able to write as well — restart the tool,
// change a setting, type into the CLI. This package builds the READING half.
// The writing half attaches later as an OPTIONAL interface that an actor
// type-asserts, the same way magmux's own InputNotifier sits beside
// ToolController, and it reaches a middleware through the command-with-reply
// path that [github.com/MadAppGang/magmux/internal/obs/pipeline].Actor already
// has. Nothing here declares a vocabulary for it: a vocabulary invented for a
// design that does not exist yet is guaranteed to be the wrong one.
//
// # One instance per session, and it remembers
//
// [Middleware] returns a pipeline.Factory, so the pipeline builds one instance
// per session and there is no API through which two sessions could share one.
// That matters here more than anywhere: an instance remembers the transcript
// it resolved, and a shared one would answer every session with the first
// session's file — silently, and wrong in exactly the cases this feature
// exists for.
//
// # What it will not do
//
//   - It does not touch [obs.Session.Status] or StatusSince. A fault is
//     ORTHOGONAL to a status: a session that errored an hour ago and is now
//     waiting for the user is waiting, WITH a fault, and neither fact may
//     overwrite the other.
//   - It does not count subagents from the transcript. The transcript
//     undercounts by 7.2%, and by 100% in the worst measured case.
//   - It does not read a tool result's own error flag. Every one of 497
//     observed occurrences was recovered from; counting them reports 29 of 32
//     healthy sessions as broken.
//   - It does not count a session's errors over its whole transcript. The
//     count is of RECENT errors — those in the last 200 KB — and whenever
//     that window is only part of the file, the published detail says how
//     much was examined out of how much, on a zero as well as on a count.
//     Fix round 1 decided this, reversibly: attention acts on the count, and
//     an error twenty megabytes back is not a reason to interrupt anyone. It
//     also found the zero published bare, for 13 of 36 live sessions whose
//     only errors were outside the window.
//   - It does not treat a transcript's modification time as activity. Some
//     record types are written with no conversational turn behind them, and
//     one measured session looked 1.37 hours stale while being correctly idle.
//     The mtime is used for one thing: deciding whether a re-read is needed.
//
// # Concurrency
//
// No goroutines, no mutexes, no atomics, no timers. A middleware instance is
// touched only by its own actor's goroutine. The one thing shared across
// actors is a [LaunchDirs], which is why that port is documented as needing to
// be safe for concurrent use — the same rule, and the same reason, as
// obs.PaneLocator.
package claudecode
