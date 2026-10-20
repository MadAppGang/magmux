package unixsock

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// inDir calls fn(name) on a fresh OS thread whose own working directory is dir.
//
// darwin has no bindat/connectat and /dev/fd does not resolve names under a
// directory fd, so the short address is the bare name, resolved against a
// per-thread cwd set with __pthread_fchdir. A process-wide chdir is not an
// option: every other goroutine and every child forked meanwhile would see it,
// and serialising it against forks deadlocks with net's own use of
// syscall.ForkLock.
//
// The work runs on a NEW goroutine that locks its thread and never unlocks it.
// When that goroutine returns, the runtime terminates the thread instead of
// handing it to another goroutine, so the changed cwd dies with it: nothing is
// reverted, and the caller's goroutine and thread, including any per-thread
// cwd they already carry, are never touched. A panic in fn is recovered on
// that goroutine and returned as an error, so it cannot escape onto a thread
// that is still in use.
//
// The caller waits for the result. A dial inside fn honours its own context,
// so the wait is bounded by it.
func inDir(dir, name string, fn func(addr string) error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread is discarded on return
		done <- onThisThread(dir, name, fn)
	}()
	return <-done
}

// onThisThread switches the calling thread's cwd to dir and calls fn(name).
// The caller has locked the goroutine to a thread it will discard.
func onThisThread(dir, name string, fn func(addr string) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while addressing %s in %s: %v", name, dir, r)
		}
	}()
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer syscall.Close(fd)
	if _, _, e := syscall.Syscall(syscall.SYS___PTHREAD_FCHDIR, uintptr(fd), 0, 0); e != 0 {
		return fmt.Errorf("pthread_fchdir %s: %w", dir, e)
	}
	return fn(name)
}
