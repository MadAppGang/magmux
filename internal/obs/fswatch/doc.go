// Package fswatch is magmux's file-change source: the thing that says "this
// session record changed or vanished" and never says "a kqueue event arrived".
//
// The port boundary is stated in those terms deliberately. The two platforms
// do not merely differ in spelling — they differ in what the kernel is willing
// to report. inotify's watch on a DIRECTORY reports writes to the files inside
// it; kqueue's does not, and fsnotify compensates by opening a descriptor on
// every entry of the directory it was given. Nothing outside this package sees
// an fsnotify value, so one test table runs against both implementations and
// neither leaks into a consumer.
//
// # What a consumer gets
//
// Two kinds, Changed and Vanished; one event per path per 50ms window; the
// `.json` filter that keeps the `<pid>.<sha256>.key` siblings from waking
// anything; a [Watcher.Stop] that is idempotent and leaks neither a goroutine
// nor a descriptor; and errors on their own channel, because the way this
// subsystem fails is SILENCE and a silent watcher is indistinguishable from a
// quiet machine.
//
// # The claim this package rests on
//
// See fsnotify_regression_test.go. The whole design assumes a measured
// behaviour of a third-party library on Darwin, and a claim like that needs a
// guard underneath it — not a comment. That test came before any of this code
// and it outranks it: if it ever fails, this package is wrong, not the test.
package fswatch
