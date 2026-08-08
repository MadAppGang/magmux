package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// testMagmuxBin is the path to a magmux binary built once for the whole test
// package (see TestMain). Empty if the build failed or was skipped.
var testMagmuxBin string

// TestMain builds a single magmux binary that PTY/socket subprocess tests share,
// avoiding a per-test `go build`.
func TestMain(m *testing.M) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		dir, err := os.MkdirTemp("", "magmux-test-bin")
		if err == nil {
			bin := filepath.Join(dir, "magmux")
			build := exec.Command("go", "build", "-o", bin, ".")
			if out, berr := build.CombinedOutput(); berr == nil {
				testMagmuxBin = bin
			} else {
				fmt.Fprintf(os.Stderr, "TestMain: build magmux failed: %v\n%s", berr, out)
			}
			defer os.RemoveAll(dir)
		}
	}
	os.Exit(m.Run())
}

// magmuxBinForTest returns the shared prebuilt binary, or builds a fallback.
func magmuxBinForTest(t *testing.T) string {
	t.Helper()
	if testMagmuxBin != "" {
		return testMagmuxBin
	}
	// Fallback: an already-built binary in the repo root.
	if p, err := filepath.Abs("./magmux"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "magmux")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build magmux: %v\n%s", err, out)
	}
	return bin
}

// TestAutoExitNonTUIPane is a regression test for issue #1:
// "Plain (non-TUI) panes never marked done; -w never auto-exits."
//
// The bug: when a plain shell command (no alt-screen, no controller) exits,
// magmux's auto-exit (-w) does not fire and the process hangs indefinitely.
// Root cause: inputLoop blocks on os.Stdin.Read with no path to observe
// m.quit closing, so even when renderLoop closes m.quit, main never returns.
//
// Reproduction: spawn ./magmux as a subprocess inside a PTY, run a single
// non-TUI pane that exits in ~1s, with -w. Assert that magmux exits within
// 5 seconds. Pre-fix this test hangs at ~5s deadline; post-fix it exits in ~1.5s.
//
// REGRESSION: Plain (non-TUI) panes never marked done; -w never auto-exits —
// Fixed in /dev:fix session dev-fix-20260506-110717-82296388
func TestAutoExitNonTUIPane(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY-based test requires darwin or linux")
	}

	binPath := magmuxBinForTest(t)

	// Open a PTY pair so magmux gets a real controlling terminal (it requires
	// raw mode and refuses to run otherwise).
	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	// Spawn magmux with -e (single command) and -w (auto-exit on done).
	// The shell pane echoes once and sleeps 1s — a classic non-TUI workload.
	cmd := exec.Command(binPath, "-e", `sh -c "echo hi; sleep 1"`, "-w")
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}

	// Drain the master FD in the background so writes from magmux to the PTY
	// don't block. We don't care about the output content — just that magmux
	// exits.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	// Wait for exit, with a deadline. Pre-fix this hangs forever (we observed
	// hangs of >>30s in manual reproduction). Post-fix exits in ~1.5s.
	const deadline = 5 * time.Second
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	select {
	case <-time.After(deadline):
		// Bug present — kill and fail.
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-exitCh
		t.Fatalf("magmux did not auto-exit within %v with -w on a non-TUI pane (regression: issue #1)", deadline)
	case err := <-exitCh:
		// Expect clean exit (status 0). magmux returns 0 when -w fires cleanly.
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				t.Fatalf("magmux exited with non-zero status %d: %v", exitErr.ExitCode(), err)
			}
			t.Fatalf("magmux wait error: %v", err)
		}
	}
}

// socketEvent is the decoded shape of one JSON line on the magmux IPC socket.
// Only the fields the test asserts on are modeled; unknown fields are ignored.
type socketEvent struct {
	Type       string        `json:"type"`
	Pane       *int          `json:"pane"`       // singular: per-pane snapshot/exit
	Panes      []interface{} `json:"panes"`      // plural: connect snapshot + final results
	ExitCode   *int          `json:"exitCode"`   // present on exit events
	State      string        `json:"state"`      // controller lifecycle on snapshot events
	Controller string        `json:"controller"` // controller name on snapshot events
}

// TestSocketSubscriberContract exercises the stable socket integration API that
// madbench (and other subscribers) depend on:
//
//  1. Late subscriber: even connecting after panes may have exited, the first
//     line is an aggregate {"type":"snapshot","panes":[...]} carrying full state.
//  2. Shutdown ordering: the final {"type":"results",...} event is delivered
//     BEFORE the connection EOFs (the flush-then-close guarantee, replacing the
//     old drain-sleep race).
//
// It spawns a real magmux under a PTY, derives the socket path from the child
// PID, and reads the line-delimited JSON stream to EOF.
func TestSocketSubscriberContract(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY/socket test requires darwin or linux")
	}

	binPath := magmuxBinForTest(t)

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	// One pane that lives ~1.5s, then exits cleanly; -w auto-exits magmux after.
	// The window is long enough to connect a subscriber and observe the full
	// lifecycle (snapshot → exit → results → EOF).
	cmd := exec.Command(binPath, "-e", `sh -c "echo hi; sleep 1.5"`, "-w")
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	// Drain the PTY master so magmux's writes never block.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	sockPath := fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid)

	// Dial with short retries: the socket binds during magmux init (a few ms
	// after Start). ~50ms x 40 = 2s worst case.
	var conn net.Conn
	for i := 0; i < 40; i++ {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("could not connect to magmux socket %s: %v", sockPath, err)
	}
	defer conn.Close()

	// Read every event to EOF, with an overall deadline so a hang fails loudly.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	var events []socketEvent
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		var ev socketEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // ignore anything unparseable
		}
		events = append(events, ev)
	}
	// scanner ended: either EOF (clean shutdown) or read deadline.
	if err := scanner.Err(); err != nil {
		t.Fatalf("socket read error before EOF (shutdown race?): %v", err)
	}

	if len(events) == 0 {
		t.Fatal("received no events on socket")
	}

	// (1) First event must be the connect-time aggregate snapshot.
	first := events[0]
	if first.Type != "snapshot" || first.Panes == nil {
		t.Fatalf("first event = %+v; want aggregate snapshot with non-nil panes array", first)
	}

	// (2) A final results event must have been received BEFORE EOF. Because the
	// scanner reached clean EOF above, any results event in the stream proves
	// the flush-then-close ordering held.
	var sawResults bool
	var resultsIdx int
	for i, ev := range events {
		if ev.Type == "results" && ev.Panes != nil {
			sawResults = true
			resultsIdx = i
		}
	}
	if !sawResults {
		t.Fatalf("no final results event before EOF; events=%+v", events)
	}

	// The shutdown event (if present) must not precede the results event —
	// results is the authoritative final state and must land first.
	for i, ev := range events {
		if ev.Type == "shutdown" && i < resultsIdx {
			t.Fatalf("shutdown event (idx %d) arrived before results (idx %d)", i, resultsIdx)
		}
	}
}

// TestSocketResultsDeliveredAtAnyConnectTime hammers the connect/shutdown
// race that made TestSocketSubscriberContract flaky (~1 failure in 20).
//
// The old handleSocketConn wrote its connect-time aggregate snapshot and
// only then registered the connection for broadcasts. A subscriber landing
// in that window — or any time after the final broadcasts began — was never
// sent `results` and saw a bare EOF instead, breaking the ordering
// guarantee integrators rely on. The window is microseconds wide, so a
// subscriber that always connects early (as the contract test does) hits it
// only occasionally.
//
// This test aims straight at it: short-lived panes, and a connect delay
// swept across the whole run so subscribers land before, during, and after
// teardown. Every connection that reaches the socket must receive `results`
// before EOF.
func TestSocketResultsDeliveredAtAnyConnectTime(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY/socket test requires darwin or linux")
	}
	binPath := magmuxBinForTest(t)

	// Delays sweep past the pane's ~300ms lifetime so some subscribers
	// connect while the shutdown broadcasts are in flight.
	delays := []time.Duration{
		0, 50 * time.Millisecond, 150 * time.Millisecond, 250 * time.Millisecond,
		300 * time.Millisecond, 320 * time.Millisecond, 340 * time.Millisecond,
		360 * time.Millisecond, 380 * time.Millisecond, 400 * time.Millisecond,
	}

	for i, delay := range delays {
		t.Run(fmt.Sprintf("connect_after_%s", delay), func(t *testing.T) {
			master, slave, err := openPTY()
			if err != nil {
				t.Fatalf("openPTY: %v", err)
			}
			defer master.Close()
			defer slave.Close()

			cmd := exec.Command(binPath, "-e", `sh -c "echo hi; sleep 0.3"`, "-w")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start magmux: %v", err)
			}
			defer func() {
				_ = cmd.Process.Signal(syscall.SIGKILL)
				_, _ = cmd.Process.Wait()
			}()
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := master.Read(buf); err != nil {
						return
					}
				}
			}()

			sockPath := fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid)
			time.Sleep(delay)

			// Connect once, with brief retries only while the socket is still
			// being bound. A refused connection after magmux has exited is a
			// legitimate outcome (nothing to subscribe to) and is not a failure.
			var conn net.Conn
			for a := 0; a < 10; a++ {
				conn, err = net.Dial("unix", sockPath)
				if err == nil || delay > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if conn == nil {
				t.Skipf("iteration %d: magmux already gone at +%s (nothing to subscribe to)", i, delay)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

			var sawResults, sawAny bool
			scanner := bufio.NewScanner(conn)
			scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
			for scanner.Scan() {
				var ev socketEvent
				if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
					continue
				}
				sawAny = true
				if ev.Type == "results" && ev.Panes != nil {
					sawResults = true
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("read error before EOF (connected at +%s): %v", delay, err)
			}
			if sawAny && !sawResults {
				t.Fatalf("subscriber connected at +%s received events but no `results` before EOF", delay)
			}
		})
	}
}

// TestControllerSnapshotReachesAwaitingInput is the regression test for
// issue #2: a pane with an attached controller broadcast exactly one
// snapshot — state "starting" — and never another, even once the child was
// idle at its prompt. Subscribers could only learn the real state from the
// shutdown `results` event.
//
// The repro needs no real Claude Code. It needs the two conditions that
// produced the bug:
//
//  1. A controller attaches (the command line mentions `claude `), so the
//     pane is controller-managed and broadcasts snapshots.
//  2. No transcript ever matches, so the transcript state machine never sees
//     a stop_hook_summary and stays at CtrlStarting — the same dead end a
//     mis-encoded project dir produced in the field.
//
// The pane then emits an OSC 9 notification, which is how the terminal
// reports "idle, waiting for input" and is the signal `results` always
// trusted. Post-fix the live snapshot must report awaiting_input too.
func TestControllerSnapshotReachesAwaitingInput(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY/socket test requires darwin or linux")
	}

	binPath := magmuxBinForTest(t)

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	// "claude " in the command attaches ClaudeCodeController; the printf emits
	// the OSC 9 notification; the sleep keeps the pane alive long enough for
	// the ~4Hz controller poll to observe and broadcast the transition.
	cmd := exec.Command(binPath, "-e",
		`sh -c "printf 'starting claude now\n'; sleep 1; printf '\033]9;done\007'; sleep 1"`, "-w")
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	sockPath := fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid)
	var conn net.Conn
	for i := 0; i < 40; i++ {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("could not connect to magmux socket %s: %v", sockPath, err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	var states []string
	var sawAwaitingInput, sawController bool
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var ev socketEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		// Per-pane snapshots only (the aggregate connect snapshot has Panes set).
		if ev.Type != "snapshot" || ev.Pane == nil || ev.Panes != nil {
			continue
		}
		sawController = sawController || ev.Controller != ""
		states = append(states, ev.State)
		if ev.State == "awaiting_input" {
			sawAwaitingInput = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("socket read error before EOF: %v", err)
	}

	if !sawController {
		t.Fatalf("no controller ever attached; the repro did not set up (states seen: %v)", states)
	}
	if !sawAwaitingInput {
		t.Fatalf("live snapshot never reached awaiting_input (regression: issue #2); states seen: %v", states)
	}
}
