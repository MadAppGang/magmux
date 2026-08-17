package main

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestSIGTERMTearsDownLikeAQuit is the real-process half, and it is the reason
// the twelve test teardowns had to move to SIGTERM in the same change: SIGKILL
// is uncatchable, so a suite that only ever SIGKILLs cannot exercise a signal
// handler at all.
//
// It asserts the property the whole design rests on — that the handler closes
// m.quit and lets the ORDINARY teardown run — by checking the three things only
// that teardown produces: the final `results` event, the `shutdown` after it,
// and a clean EOF after both. A handler that took the one-line shortcut and
// called os.Remove(m.sockPath) itself would pass none of them.
//
// Asserted on `results`, never on an `exit` event: `exit` is a live broadcast
// with no delivery guarantee and racing teardown, which CLAUDE.md records as
// having been diagnosed twice.
func TestSIGTERMTearsDownLikeAQuit(t *testing.T) {
	// A pane that outlives the test, so nothing but the signal can end the run.
	mux := startRPCMagmux(t, "-e", "sleep 30")
	conn := mux.dial()

	// The connect-time aggregate proves the layout is up, which also means
	// markLayoutReady has run — the point after which handleSignals is
	// registered. Signalling before this races the startup window the handler
	// deliberately does not cover.
	ev, _, ok := conn.next()
	if !ok || ev["type"] != "snapshot" {
		t.Fatalf("first line was %v, want the connect-time snapshot", ev)
	}

	if err := mux.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	var sawResults, sawShutdown bool
	for _, e := range conn.drain() { // drain runs to EOF
		switch e["type"] {
		case "results":
			if sawShutdown {
				t.Error("results arrived AFTER shutdown; subscribers rely on the opposite order")
			}
			sawResults = true
		case "shutdown":
			sawShutdown = true
		}
	}
	if !sawResults {
		t.Error("no results event after SIGTERM: the handler is not going through the ordinary m.quit teardown")
	}
	if !sawShutdown {
		t.Error("no shutdown event after SIGTERM")
	}

	// And the socket is unlinked — by socketServer's teardown goroutine, not by
	// the handler. Reaching EOF above means that goroutine got past its
	// broadcasts; give the remaining ln.Close/os.Remove a moment to land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mux.sock); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%s survived a SIGTERM teardown; this is the leak the whole change exists to stop", mux.sock)
}

// The signalLoop tests below drive the loop directly rather than through
// handleSignals, so nothing here registers a real signal.Notify and the test
// binary's own signal disposition is untouched.
//
// exit is injected, and the fake exits with runtime.Goexit() rather than
// blocking in select{} — os.Exit never returns, so a fake that DID return would
// let the loop run on past the exit and assert nothing meaningful, and a fake
// that blocked forever would leak a goroutine per case.
//
// Note that phase 2 calls m.restore(), which writes a short "leave the
// alternate screen, show the cursor, mouse off" escape string to os.Stdout.
// With m.rawState nil (which is what a struct literal gives) that is all it
// does, and every sequence in it is a disable of something already disabled, so
// it is inert. It becomes a true no-op once --headless lands and restore()
// learns to skip a session that never took the terminal.

func TestSignalLoopFirstSignalQuitsSecondExits(t *testing.T) {
	m := &Magmux{quit: make(chan struct{})}
	ch := make(chan os.Signal, 2)
	got := make(chan int, 1)
	go m.signalLoop(ch, func(c int) { got <- c; runtime.Goexit() })

	ch <- syscall.SIGTERM

	// A real barrier, not a sleep: m.quit closing IS phase 1 completing.
	select {
	case <-m.quit:
	case <-time.After(2 * time.Second):
		t.Fatal("the first signal did not close m.quit")
	}

	// And it did NOT hard-exit. Closing m.quit is the whole point of the first
	// signal: the socket server's teardown goroutine is what broadcasts
	// `results`, and an exit here would delete it.
	select {
	case c := <-got:
		t.Fatalf("the first signal exited with %d; it must only close m.quit", c)
	default:
	}

	ch <- syscall.SIGINT
	select {
	case c := <-got:
		if want := 128 + int(syscall.SIGINT); c != want {
			t.Fatalf("second signal exited with %d, want %d", c, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second signal did not hard-exit; phase 2 is unreachable " +
			"(this is exactly what the rejected for/select loop got wrong)")
	}
}

// TestSignalLoopDuringTeardownStillHardExits covers the case that makes phase 1
// fall THROUGH on <-m.quit instead of returning: quit came from somewhere else
// (-w, -x, the Ctrl-G q chord) and the human is now signalling because that
// teardown is taking too long.
//
// It is deliberately repeated. Both of phase 1's cases are READY here — m.quit
// is closed and a signal is waiting — and Go picks among ready cases at RANDOM,
// so a single iteration passes half the time against a loop that swallows the
// signal and then blocks forever waiting for a second one. That is precisely
// the nondeterminism the two-phase design was written to avoid, and the design
// as drafted reintroduced it; one iteration cannot see that.
//
// It also closes m.quit THROUGH quitOnce, because every production closer does
// (render, the chord, -x, the socket verbs). A raw close would leave quitOnce
// unfired, and the loop's own close would then panic on an already-closed
// channel — a fault in the fixture, not in magmux.
func TestSignalLoopDuringTeardownStillHardExits(t *testing.T) {
	for i := 0; i < 200; i++ {
		m := &Magmux{quit: make(chan struct{})}
		m.quitOnce.Do(func() { close(m.quit) }) // teardown already under way
		ch := make(chan os.Signal, 1)
		got := make(chan int, 1)
		go m.signalLoop(ch, func(c int) { got <- c; runtime.Goexit() })

		ch <- syscall.SIGTERM
		select {
		case c := <-got:
			if want := 128 + int(syscall.SIGTERM); c != want {
				t.Fatalf("iteration %d: exited with %d, want %d", i, c, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: a signal arriving during teardown must still be able to "+
				"cut it short, whichever of the two ready select cases won", i)
		}
	}
}

// TestSignalLoopStaysAReceiverAfterQuit is the third failure of the rejected
// loop shape, and the one with the worst consequence: a goroutine that returns
// after the first signal leaves signal.Notify registered with NO receiver, so
// every later ^C is SWALLOWED rather than taking its default disposition — and
// a user hammering Ctrl-C at a wedged teardown is left with only SIGKILL.
//
// Observable here as: the loop must still be draining ch after phase 1.
func TestSignalLoopStaysAReceiverAfterQuit(t *testing.T) {
	m := &Magmux{quit: make(chan struct{})}
	ch := make(chan os.Signal) // UNBUFFERED: a send proves a receiver exists
	got := make(chan int, 1)
	go m.signalLoop(ch, func(c int) { got <- c; runtime.Goexit() })

	ch <- syscall.SIGHUP // phase 1
	select {
	case <-m.quit:
	case <-time.After(2 * time.Second):
		t.Fatal("the first signal did not close m.quit")
	}

	// An unbuffered send blocks until somebody receives. If the loop had
	// returned, this would time out — which is the bug.
	sent := make(chan struct{})
	go func() { ch <- syscall.SIGHUP; close(sent) }()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing is receiving on the signal channel after phase 1; later signals would be swallowed")
	}
	<-got
}
