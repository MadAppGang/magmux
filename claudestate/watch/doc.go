// Package watch is the library's file-change source: fsnotify behind tt's
// watch protocol (arm / verify / release), the D8 coalescing window, and the
// D15 budget recounted on every wave. It is the ONLY importer of fsnotify in
// the module (guard G10).
//
// # The protocol, and who drives it
//
// The coordinator calls three methods, in this order, every cycle:
//
//   - [Watcher.Arm] arms targets and ARMS ONLY: it never releases. It is
//     idempotent over held targets, so the growing supersets of an arm wave
//     cost nothing for what is already held, and a held target is never
//     closed and reopened (that would discard whatever the kernel had queued
//     on it).
//   - [Watcher.Verify] compares each held target's (dev, ino) with what the
//     path names now, re-arms a replaced one BEFORE the cycle reads it, and
//     recounts the budget. An overrun goes into ArmReport.Shed.
//   - [Watcher.Release] is the single releasing call, after publish. It
//     releases everything not kept and arms nothing.
//
// Arm, Verify and Release are called from ONE goroutine, the coordinator's,
// and the held set is that goroutine's alone. The event loop is a second
// goroutine that owns the fsnotify channels, the coalescing state and its one
// timer. The two share nothing but the fsnotify watcher, whose Add and Remove
// are its own to synchronise. So there is no lock here (G5).
//
// # What is never armed
//
// A projects directory, a project directory and a subagents directory are
// never Added (C11, D15): watching them costs a descriptor per transcript on
// darwin. Transcripts are armed one by one, as FILES. A directory target is
// accepted only as the spine or a job directory.
//
// # The claim this package rests on
//
// See fsnotify_regression_test.go (G11). On darwin a kqueue watch on a
// directory does not fire when a file inside it is rewritten in place, and
// fsnotify compensates by opening a descriptor on every entry. That is a
// measured library behaviour, not a documented promise, and the registry
// records are rewritten IN PLACE. If that test ever fails, this package is
// wrong, not the test.
package watch
