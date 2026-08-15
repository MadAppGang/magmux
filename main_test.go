package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// TestParsePaneIndexRejectsGarbage guards the fix for a silent mis-target:
// parsePaneIndex used fmt.Sscanf and dropped its error, so any unparseable
// `pane` left the index at its zero value and the message was applied to
// PANE 0 — a real session, chosen by accident, with nothing logged.
//
// The second half is the part that actually matters to a caller: the sentinel
// is worth nothing unless the dispatch path drops it, so a tint aimed at
// "api" must leave pane 0 alone.
func TestParsePaneIndexRejectsGarbage(t *testing.T) {
	m := &Magmux{}
	cases := []struct {
		in   any
		want int
		why  string
	}{
		{"abc", paneInvalid, "a label is not an index"},
		{"", paneInvalid, "an empty string was given, so it is not 'unspecified'"},
		{"1x", paneInvalid, "Sscanf would have taken the leading 1"},
		{"  ", paneInvalid, "whitespace is not a number"},
		{"-", paneInvalid, "a bare sign is not a number"},
		{"*", paneAll, "the documented fan-out"},
		{"3", 3, "a numeric string is a valid index"},
		{" 4 ", 4, "surrounding whitespace is tolerated"},
		{float64(3), 3, "JSON numbers decode as float64"},
		{nil, paneUnspecified, "an absent field means 'the pane I announced'"},
	}
	for _, c := range cases {
		if got := m.parsePaneIndex(c.in); got != c.want {
			t.Errorf("parsePaneIndex(%#v) = %d, want %d (%s)", c.in, got, c.want, c.why)
		}
	}

	m = newTestMux(t, PaneConfig{Control: true}, PaneConfig{Control: true})
	m.dispatchSocketMsg(sockMsg{Type: "tint", Pane: "api", Color: "red"})
	for i, p := range m.livePanes() {
		if p.tint != "" {
			t.Fatalf("tint for pane %q landed on pane %d; an unparseable pane must be dropped", "api", i)
		}
	}
	m.dispatchSocketMsg(sockMsg{Type: "tint", Pane: float64(1), Color: "red"})
	if m.paneByID(1).tint != "red" || m.paneByID(0).tint != "" {
		t.Fatalf("tint for pane 1 gave tints %q / %q; want \"\" / \"red\"",
			m.paneByID(0).tint, m.paneByID(1).tint)
	}
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

// TestSocketReaderAcceptsLargeLine is the regression guard for the socket
// reader's missing scanner.Buffer.
//
// bufio.Scanner's default 64KB token limit does not skip an oversized line: it
// makes Scan return false, which ends handleSocketConn and closes the
// connection. So one long message cost a client everything — no further verbs
// dispatched, no further broadcasts, no error on either side. The line here is
// 256KB (over the old limit, well under the 4MB one) and the connection has to
// survive it: the `send` written afterwards must still reach the pane's PTY,
// and the stream must still run to `results` rather than EOF on the spot.
func TestSocketReaderAcceptsLargeLine(t *testing.T) {
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
	// Without a window size every pane screen is zero rows and the exit
	// event's lastLine is empty whatever the pane printed.
	setWinSize(master, 24, 100)

	// Deliberately NOT -w, for the same reason TestSocketIDFlagBindsNamedSocket
	// is not: `exit` is a live broadcast from waitForChild's own goroutine and,
	// unlike `results`, is never replayed to a connection. Under -w that
	// goroutine races the teardown -w triggers — the read loop sets `dead`, the
	// render loop closes m.quit, and the socket server can broadcast results
	// and close every subscriber before cmd.Wait has returned — so the event
	// this test reads its answer out of is simply gone, roughly one run in
	// twenty-five. Quitting by hand once the event has landed removes the race
	// without weakening the assertion: the send still has to have reached the
	// pane's PTY, and the stream still has to run to `results`.
	cmd := exec.Command(binPath, "-e", `head -n 1 | sed s/^/GOT:/; sleep 1`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
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

	write := func(v any) error {
		b, _ := json.Marshal(v)
		_, err := conn.Write(append(b, '\n'))
		return err
	}

	// The oversized line. A valid, harmless message — the size is the point.
	big := strings.Repeat("x", 256*1024)
	if err := write(map[string]any{"type": "status", "text": big}); err != nil {
		// magmux closes the connection the moment the scanner overflows, so
		// without the fix the write breaks part-way through this very line.
		t.Fatalf("writing the 256KB line: %v — the reader dropped the client mid-line "+
			"instead of skipping the message", err)
	}

	// Give the pane a moment to reach its `read`, then prove the connection is
	// still being served by driving it.
	time.Sleep(400 * time.Millisecond)
	const payload = "after-big-line"
	if err := write(map[string]any{"type": "send", "pane": 0, "text": payload}); err != nil {
		t.Fatalf("the connection was already gone after the 256KB line (%v): "+
			"an oversized line must skip a message, not drop the client", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	var (
		echoed     bool
		gotResults bool
		lastLines  []string
		allLines   []string
		quitSent   bool
	)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "exit":
			s, _ := ev["lastLine"].(string)
			lastLines = append(lastLines, s)
			if strings.Contains(s, "GOT:"+payload) {
				echoed = true
			}
			if !quitSent {
				// magmux's own quit chord, so the run still ends through the
				// normal teardown that broadcasts results.
				quitSent = true
				if _, err := master.Write([]byte{0x07, 'q'}); err != nil {
					t.Fatalf("write quit chord to the PTY: %v", err)
				}
			}
		case "results":
			gotResults = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("socket read error before EOF: %v", err)
	}

	if len(lastLines) == 0 {
		t.Fatalf("no exit event reached this subscriber, so the pane's output could not be read at all; whole stream was:\n%s",
			strings.Join(allLines, "\n"))
	}
	if !echoed {
		t.Errorf("the send after a 256KB line never reached the pane: want an exit lastLine containing %q, got %q "+
			"(the oversized line dropped the whole connection)", "GOT:"+payload, lastLines)
	}
	if !gotResults {
		t.Error("no results event: the connection died on the oversized line instead of skipping past it")
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("magmux exited with error: %v", err)
	}
	killed = true
}

// TestSocketIDFlagBindsNamedSocket covers --id: the socket path becomes
// /tmp/magmux-<name>.sock, which is what lets a caller know where to dial
// before magmux has started rather than having to find its pid. The pid
// default is not exercised here because every other socket test in the package
// depends on it byte-for-byte.
//
// The child's own MAGMUX_SOCK is the assertion that matters — a named socket
// that panes cannot see would be a half-implemented flag.
func TestSocketIDFlagBindsNamedSocket(t *testing.T) {
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
	setWinSize(master, 24, 100)

	name := fmt.Sprintf("test%d", time.Now().UnixNano())
	sockPath := "/tmp/magmux-" + name + ".sock"
	defer os.Remove(sockPath)

	// Deliberately NOT -w. `exit` is a live broadcast: unlike `results` it is
	// never replayed to a connection that arrives late, and it is produced by
	// waitForChild on its own goroutine. Under -w that goroutine races the
	// teardown that -w triggers — the pane's read loop sets `dead`, the render
	// loop sees it and closes m.quit, and the socket server can broadcast
	// results and close every subscriber before cmd.Wait has even returned. The
	// event is then simply gone and the test fails against a magmux that
	// worked. Quitting by hand once the event has arrived removes the race
	// entirely.
	cmd := exec.Command(binPath, "--id", name, "-e", `printenv MAGMUX_SOCK; sleep 1`)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	var conn net.Conn
	for i := 0; i < 40; i++ {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("could not connect to the named socket %s: %v", sockPath, err)
	}
	defer conn.Close()

	// The pid path must not also have been bound: --id replaces it, it does
	// not add to it.
	pidPath := fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid)
	if _, err := os.Stat(pidPath); err == nil {
		t.Errorf("%s exists as well as %s; --id must replace the pid socket, not add one", pidPath, sockPath)
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var (
		childSock  string
		gotResults bool
		exits      []string // every exit lastLine, for the failure message
		allLines   []string
		quitSent   bool
	)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "exit":
			s, _ := ev["lastLine"].(string)
			exits = append(exits, fmt.Sprintf("pane %v exit %v lastLine=%q", ev["pane"], ev["exitCode"], s))
			if strings.Contains(s, "magmux-") {
				childSock = strings.TrimSpace(s)
			}
			if !quitSent {
				// magmux's own quit chord, so the run ends through the normal
				// teardown that broadcasts results and unlinks the socket.
				quitSent = true
				if _, err := master.Write([]byte{0x07, 'q'}); err != nil {
					t.Fatalf("write quit chord to the PTY: %v", err)
				}
			}
		case "results":
			gotResults = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("socket read error before EOF: %v", err)
	}

	if len(exits) == 0 {
		t.Fatalf("no exit event reached this subscriber; whole stream was:\n%s", strings.Join(allLines, "\n"))
	}
	if childSock != sockPath {
		// The exit events are printed because the two ways this fails look
		// identical otherwise: the child genuinely saw an empty MAGMUX_SOCK,
		// or its output had not been parsed off the PTY when it exited.
		t.Errorf("child saw MAGMUX_SOCK=%q, want %q; exit events were %v", childSock, sockPath, exits)
	}
	if !gotResults {
		t.Fatal("no results event — magmux did not shut down cleanly")
	}

	// Wait for the process so the teardown that unlinks the socket has
	// actually finished before we look for the file.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("magmux exited with error: %v", err)
	}
	killed = true
	if _, err := os.Stat(sockPath); err == nil {
		t.Errorf("%s survived shutdown; the named socket must be unlinked like the pid one", sockPath)
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

// ── OSC colour queries ────────────────────────────────────────────────────────
//
// The bug these cover: Claude Code rendered as a completely blank pane inside
// magmux and rendered perfectly in tmux. It is not a drawing bug. Theme-aware
// TUIs ask the terminal what colour its background is (OSC 11) before choosing
// a light or a dark theme, and they BLOCK on the answer. magmux answered DA1,
// DA2 and DSR but no OSC query at all, so the question went into a void and the
// application never got as far as its first frame.

// queryPane returns a PTY-less pane whose replyLocked output the caller can
// read, plus that read end. A pipe rather than a real PTY because the assertion is on
// the exact bytes magmux sends back, and a PTY's line discipline is free to
// rewrite them.
func queryPane(t *testing.T) (*Pane, *os.File) {
	t.Helper()
	p := newControlPane(0, 0, 6, 40, "query")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	p.ptmx = w
	t.Cleanup(func() { r.Close(); w.Close() })
	return p, r
}

// readReply returns whatever the pane wrote back, or "" if it wrote nothing
// within the deadline. "Nothing" is an expected outcome — a colour SET must not
// be answered — so this must not be fatal.
func readReply(t *testing.T, r *os.File) string {
	t.Helper()
	return readReplyWithin(t, r, 2*time.Second)
}

// readReplyWithin is readReply with the wait named, so the cases that expect
// silence do not each pay the generous deadline the answered cases get. The
// parser answers inside vt.write, synchronously, so anything it was going to
// send is already in the pipe by the time this is called.
func readReplyWithin(t *testing.T, r *os.File, d time.Duration) string {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 256)
	n, err := r.Read(buf)
	if n == 0 {
		if err != nil && !os.IsTimeout(err) {
			t.Fatalf("read reply: %v", err)
		}
		return ""
	}
	return string(buf[:n])
}

// TestOSCBackgroundQueryIsAnswered is the bug stated as an assertion: a child
// that asks what colour the terminal is gets an answer, terminated the way it
// asked.
func TestOSCBackgroundQueryIsAnswered(t *testing.T) {
	defer useTheme(themeDark)()

	cases := []struct{ name, query, want string }{
		{"background, BEL", "\x1b]11;?\x07", "\x1b]11;rgb:1e1e/1e1e/2e2e\x07"},
		{"background, ST", "\x1b]11;?\x1b\\", "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\"},
		{"foreground, BEL", "\x1b]10;?\x07", "\x1b]10;rgb:cdcd/d6d6/f4f4\x07"},
		{"foreground, ST", "\x1b]10;?\x1b\\", "\x1b]10;rgb:cdcd/d6d6/f4f4\x1b\\"},
		// Cursor colour: xterm's default cursor is the foreground, and it is the
		// only answer certain to be visible against the background just reported.
		{"cursor, BEL", "\x1b]12;?\x07", "\x1b]12;rgb:cdcd/d6d6/f4f4\x07"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, r := queryPane(t)
			p.vt.write([]byte(c.query))
			got := readReply(t, r)
			if got == "" {
				t.Fatalf("query %q got no reply at all — this is the bug: a theme-aware "+
					"TUI blocks here and never draws its first frame", c.query)
			}
			if got != c.want {
				t.Fatalf("query %q\n got %q\nwant %q", c.query, got, c.want)
			}
		})
	}
}

// TestOSCColorReplyEchoesTheQueryTerminator states the terminator rule on its
// own, because getting it wrong is silent: the application unblocks, and then
// finds a stray BEL or a stray ESC \ in its input.
func TestOSCColorReplyEchoesTheQueryTerminator(t *testing.T) {
	defer useTheme(themeDark)()

	for _, term := range []string{"\x07", "\x1b\\"} {
		p, r := queryPane(t)
		p.vt.write([]byte("\x1b]11;?" + term))
		got := readReply(t, r)
		if !strings.HasSuffix(got, term) {
			t.Errorf("a query terminated with %q was answered %q; the reply must end the "+
				"same way the question did", term, got)
		}
		// And exactly once: a BEL reply must not also carry an ST.
		if strings.Count(got, "\x07")+strings.Count(got, "\x1b\\") != 1 {
			t.Errorf("reply %q carries more than one terminator", got)
		}
	}
}

// TestOSCColorSetIsNotAnswered guards the other half of the contract. "?" is
// the whole difference between a question and a statement: `OSC 11;rgb:...` is
// a child SETTING the background, and replying to it puts bytes into the input
// of a program that is not reading any.
func TestOSCColorSetIsNotAnswered(t *testing.T) {
	defer useTheme(themeDark)()

	for _, set := range []string{
		"\x1b]11;rgb:1111/2222/3333\x07",
		"\x1b]11;#ff0000\x07",
		"\x1b]10;#000000\x1b\\",
		"\x1b]12;red\x07",
		"\x1b]4;1;?\x07", // a palette-entry query, which magmux does not serve
		"\x1b]110;?\x07", // reset-foreground, not a query for 10
		"\x1b]11;\x07",   // truncated: no argument at all
		"\x1b]11;??\x07", // not a well-formed query
		"\x1b]11\x07",    // no argument separator
	} {
		p, r := queryPane(t)
		p.vt.write([]byte(set))
		if got := readReplyWithin(t, r, 200*time.Millisecond); got != "" {
			t.Errorf("%q was answered %q; only the \"?\" query form gets a reply", set, got)
		}
	}
}

// TestOSCColorQuerySurvivesFragmentation covers the two ways a query reaches
// the parser in pieces, since a PTY read boundary falls anywhere: split across
// writes, and preceded by a query that was never terminated.
func TestOSCColorQuerySurvivesFragmentation(t *testing.T) {
	defer useTheme(themeDark)()
	const want = "\x1b]11;rgb:1e1e/1e1e/2e2e\x07"

	t.Run("split across writes", func(t *testing.T) {
		p, r := queryPane(t)
		for _, frag := range []string{"\x1b", "]1", "1;", "?", "\x07"} {
			p.vt.write([]byte(frag))
		}
		if got := readReply(t, r); got != want {
			t.Fatalf("fragmented query got %q, want %q", got, want)
		}
	})

	t.Run("after an abandoned query", func(t *testing.T) {
		p, r := queryPane(t)
		// An OSC that never terminates, cancelled by CAN: the parser must not be
		// left in the OSC state, and must still answer what comes next.
		p.vt.write([]byte("\x1b]11;\x18hello" + "\x1b]11;?\x07"))
		if got := readReply(t, r); got != want {
			t.Fatalf("query after an abandoned one got %q — a malformed query wedged the parser", got)
		}
	})
}

// TestOSCColorQueryTracksTheTheme is what makes the answer worth giving. The
// point of replying is that the child picks a readable theme, so the reported
// background must be on the same side of the light/dark line as the palette
// magmux paints its own chrome with.
func TestOSCColorQueryTracksTheTheme(t *testing.T) {
	for _, kind := range []themeKind{themeDark, themeLight} {
		t.Run(kind.String(), func(t *testing.T) {
			defer useTheme(kind)()

			p, r := queryPane(t)
			p.vt.write([]byte("\x1b]11;?\x07"))
			reply := readReply(t, r)
			body := strings.TrimSuffix(strings.TrimPrefix(reply, "\x1b]11;"), "\x07")
			c, ok := parseXColor(body)
			if !ok {
				t.Fatalf("reply %q is not a colour a terminal client could parse", reply)
			}
			// Round-trips: what we sent is exactly the palette's own background.
			if c != pal.assumedBack {
				t.Errorf("reported background %+v, want the active palette's %+v", c, pal.assumedBack)
			}
			if got, _, _ := classifyOSC11(body); got != kind {
				t.Errorf("with --theme %s the reported background classifies as %s; a child "+
					"would pick the opposite theme to the one magmux is drawing", kind, got)
			}
		})
	}
}

// TestOSCColorQueryPrefersTheProbedBackground: when the startup probe managed
// to read the real terminal's background, that is what children are told. The
// palette's assumed background is the fallback, not the answer.
func TestOSCColorQueryPrefersTheProbedBackground(t *testing.T) {
	defer useTheme(themeDark)()

	measured := rgb{0x2B, 0x30, 0x3B}
	setDetectedBackground(measured)

	p, r := queryPane(t)
	p.vt.write([]byte("\x1b]11;?\x07"))
	want := "\x1b]11;" + xColorString(measured) + "\x07"
	if got := readReply(t, r); got != want {
		t.Fatalf("got %q, want %q — a measured background beats the palette's assumption", got, want)
	}
}

// TestTerminalRepliesReachASettledPane. magmux answering its own child is NOT
// input, and routing the answers through writePTY conflated the two.
//
// writePTY refuses a pane that is dead or awaiting input in grid mode — that is
// what lets `q` dismiss a finished grid — but awaiting-input is precisely the
// state a long-lived TUI sits in. Claude Code re-queries OSC 11 on every
// SIGWINCH, and magmux sends one to every pane each time `Ctrl-G p` reshapes
// the layout. So a settled pane asked the question, got nothing back, and
// blocked: the blank-pane hang answerColorQuery exists to prevent, arriving by
// a second route. DA and DSR are on the same path, and a TUI blocks on those too.
func TestTerminalRepliesReachASettledPane(t *testing.T) {
	defer useTheme(themeDark)()

	for _, c := range []struct{ name, query, want string }{
		{"OSC 11 background", "\x1b]11;?\x07", "\x1b]11;rgb:1e1e/1e1e/2e2e\x07"},
		{"DA1", "\x1b[c", "\x1b[?1;2c"},
		{"DA2", "\x1b[>c", "\x1b[>1;10;0c"},
		{"DSR cursor position", "\x1b[6n", "\x1b[1;1R"},
		{"DSR status", "\x1b[5n", "\x1b[0n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, r := queryPane(t)
			p.gridMode = true
			p.inputReady = true
			p.tint = "success"
			p.overlayText = "✓ DONE"
			p.overlayStyle = "success"

			p.vt.write([]byte(c.query))

			if got := readReply(t, r); got != c.want {
				t.Fatalf("a pane sitting at awaiting_input asked %q and was answered %q, want %q — "+
					"the reply was suppressed as if it were a keystroke into a finished pane, "+
					"and the child is now blocked on an answer that will never come",
					c.query, got, c.want)
			}
		})
	}
}

// TestTerminalReplyDoesNotClearCompletionState is the same conflation seen from
// the other side. writePTY clears inputReady, tint, the overlay and
// hadTextOutput under "user input resets idle state" — correct for a keystroke,
// wrong for magmux answering its own child. A pane that had finished its turn
// silently read as working again and lost its ✓ DONE chrome, for nothing more
// than a background query it did not ask a human for.
func TestTerminalReplyDoesNotClearCompletionState(t *testing.T) {
	defer useTheme(themeDark)()

	p, r := queryPane(t)
	p.inputReady = true
	p.tint = "success"
	p.overlayText = "✓ DONE"
	p.overlayStyle = "success"
	p.hadTextOutput = true

	p.vt.write([]byte("\x1b]11;?\x07"))
	if got := readReply(t, r); got == "" {
		t.Fatalf("the query was not answered at all")
	}

	if !p.inputReady {
		t.Errorf("answering a colour query cleared inputReady; the pane now reads as working")
	}
	if p.tint != "success" || p.overlayText != "✓ DONE" || p.overlayStyle != "success" {
		t.Errorf("answering a colour query erased the completion chrome: tint=%q overlay=%q/%q",
			p.tint, p.overlayText, p.overlayStyle)
	}
	if !p.hadTextOutput {
		t.Errorf("answering a colour query cleared hadTextOutput; the idle heuristic has been reset " +
			"by magmux's own reply")
	}
}

// TestWritePTYLocksTheFieldsItWrites is a -race test, and it is the only kind
// that can fail here: writePTY tested p.dead/p.inputReady and then assigned
// inputReady, tint, overlayText, overlayStyle and hadTextOutput with no lock at
// all, from the INPUT goroutine, once per keystroke and once per forwarded
// mouse event. The render goroutine reads every one of those under p.mu —
// leafTint, allPanesDoneLocked, the grid counter, renderLocked itself. Without
// -race it is a torn read nobody sees until a pane paints the wrong colour.
func TestWritePTYLocksTheFieldsItWrites(t *testing.T) {
	p := newControlPane(0, 0, 6, 40, "race")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	p.ptmx = pw
	// Drain: a full pipe buffer would turn a race test into a hang.
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	defer func() { pw.Close(); pr.Close() }()

	// The render goroutine's half of the conflict: these fields are p.mu's, and
	// it always took the lock. Re-arming inputReady each pass is what keeps
	// writePTY's reset block live for every one of the writes below.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			p.inputReady = true
			p.tint = "success"
			p.overlayText = "✓ DONE"
			p.overlayStyle = "success"
			p.hadTextOutput = true
			p.mu.Unlock()
		}
	}()

	for i := 0; i < 500; i++ {
		p.writePTY([]byte("x"))
	}
	close(stop)
	wg.Wait()
}

// ── clicking a split border ──────────────────────────────────────────────────

// TestClickOnASplitBorderCannotPanicTheMultiplexer is the crash the whole
// process dies of, and it takes every session inside magmux with it.
//
// findPaneAtLocked returns nil on a border, so the click does NOT move focus —
// focus stays on the pane it was already on, one cell away. The left-click
// branch then recorded the anchor as `row0-f.y` / `col0-f.x` with no clamp at
// all, while the drag and release branches immediately below it clamp both. A
// border above or to the left of the focused pane therefore starts a selection
// at -1, and the very next frame indexes p.screen.cells with it.
//
// The two orientations panic at DIFFERENT lines — a vertical border gives
// sx = -1 and dies in the column loop, a horizontal one gives sy = -1 and dies
// on the row subscript before the column loop is ever reached — so both are
// here.
func TestClickOnASplitBorderCannotPanicTheMultiplexer(t *testing.T) {
	for _, c := range []struct {
		name  string
		split SplitType
	}{
		{"vertical border, left of the focused pane", SplitHorizontal},
		{"horizontal border, above the focused pane", SplitVertical},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newTestMux(t, ctrlPanes(1)...)
			id, err := m.OpenPane(OpenPaneRequest{
				PaneConfig: PaneConfig{Control: true},
				Target:     0,
				Split:      c.split,
				Focus:      true,
			})
			if err != nil {
				t.Fatalf("OpenPane: %v", err)
			}
			np := m.paneByID(id)

			// The border cell is the row or column immediately before the new
			// pane — the one splitLeafLocked reserved for the divider.
			row, col := np.y, np.x
			if c.split == SplitHorizontal {
				col = np.x - 1
			} else {
				row = np.y - 1
			}
			m.treeMu.RLock()
			onBorder := m.findPaneAtLocked(row, col)
			focused := m.focused
			m.treeMu.RUnlock()
			if onBorder != nil {
				t.Fatalf("(%d,%d) is not a border: findPaneAtLocked returned pane %d", row, col, onBorder.id)
			}
			if focused != np {
				t.Fatalf("setup: focus is on pane %v, not the new pane %d", focused, np.id)
			}

			// SGR mouse coordinates are 1-indexed.
			if _, ok := m.parseSGRMouse([]byte(fmt.Sprintf("\x1b[<0;%d;%dM", col+1, row+1))); !ok {
				t.Fatalf("the mouse press was not parsed")
			}

			m.treeMu.RLock()
			sy, sx, active, pane := sel.sy, sel.sx, sel.active, sel.pane
			m.treeMu.RUnlock()
			if !active || pane != np {
				t.Fatalf("the border click did not start a selection in the focused pane")
			}
			if sy < 0 || sx < 0 {
				t.Fatalf("clicking the split border anchored the selection at row %d, col %d in the "+
					"focused pane's own frame; the drag and release branches clamp and this one does not",
					sy, sx)
			}

			// Belt and braces: the frame that follows must survive an out-of-range
			// anchor however it got there, because the alternative is that magmux
			// panics and takes every session in it down.
			m.treeMu.Lock()
			sel.sy, sel.sx, sel.ey, sel.ex = -1, -1, -1, -1
			sel.active = true
			sel.pane = np
			m.markAllDirtyLocked()
			_, _, _ = m.renderLocked()
			m.treeMu.Unlock()
		})
	}
}

// TestOSCBackgroundQueryAnsweredEndToEnd is the bug in the shape the user hit
// it: a real magmux, a real child in a real PTY, asking the real question.
//
// The child puts its tty in raw mode with VMIN=0/VTIME set, so the read is
// bounded whichever way this goes — pre-fix it reads nothing and prints an
// empty answer rather than hanging until the test deadline.
func TestOSCBackgroundQueryAnsweredEndToEnd(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY test requires darwin or linux")
	}

	binPath := magmuxBinForTest(t)

	// tr strips ESC and BEL so the answer can be printed back as plain text:
	// echoing the raw reply would be re-parsed as an OSC sequence by magmux and
	// would never appear on screen.
	script := filepath.Join(t.TempDir(), "probe.sh")
	body := "stty raw -echo min 0 time 10\n" +
		"printf '\\033]11;?\\007'\n" +
		"reply=$(head -c 64 | tr -d '\\033\\007')\n" +
		"stty sane\n" +
		"printf 'PROBE[%s]\\n' \"$reply\"\n" +
		"sleep 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	// Without a size the pane is 0 rows, nothing is ever rendered, and every
	// assertion below passes vacuously.
	setWinSize(master, 24, 100)

	// --theme dark fixes the expected answer: no probe of the outer terminal
	// runs, so the palette's own background is what gets reported.
	cmd := exec.Command(binPath, "--theme", "dark", "-e", "sh "+script, "-w")
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
	var conn net.Conn
	for i := 0; i < 60; i++ {
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
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))

	var probeLines []string
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev["type"] != "exit" {
			continue
		}
		if s, _ := ev["lastLine"].(string); strings.Contains(s, "PROBE[") {
			probeLines = append(probeLines, s)
		}
	}
	if len(probeLines) == 0 {
		t.Fatal("the probe pane produced no PROBE[...] line; the child never got as far as printing")
	}
	// ESC was stripped by tr, so a served reply reads as "]11;rgb:...".
	const want = "]11;rgb:1e1e/1e1e/2e2e"
	if !strings.Contains(probeLines[0], want) {
		t.Fatalf("child saw %q, want it to contain %q — an unanswered OSC 11 query is "+
			"exactly why Claude Code rendered as a blank pane", probeLines[0], want)
	}
}

// ── DECSTBM (CSI r) ───────────────────────────────────────────────────────────
//
// The bug these cover, and it is THE blank-pane bug: Claude Code's very first
// escape sequence is a bare `CSI r` — "reset the scrolling region to the whole
// page". magmux read the omitted bottom parameter with p1, which defaults an
// omitted parameter to 1, and so set the scrolling region to rows 1..1. The
// guard meant to catch that (`if bot == 0 { bot = s.rows }`) could never fire,
// because p1 had already turned the absent 0 into a 1.
//
// From there every LF hit `curY == scrollBot-1`, scrolled a one-line region,
// and blanked it. The application drew its whole UI onto row 0, one line at a
// time, each line erasing the last. Nothing was ever unanswered and nothing
// ever hung — magmux was simply told the page was one line tall by its own
// parameter defaulting, and drew exactly that.

// decstbmPane is a pane with a screen tall enough for a scroll region to be a
// meaningful thing, and no PTY: these assertions are on cells, not on bytes
// sent back to a child.
func decstbmPane(t *testing.T) *Pane {
	t.Helper()
	return newControlPane(0, 0, 10, 20, "decstbm")
}

// TestDECSTBMDefaultsToTheWholePage states the parameter rule on its own. An
// omitted top is line 1; an omitted (or zero) bottom is the LAST line, not
// line 1 — that asymmetry is the whole bug.
//
// Every case starts from a NON-default region, and that is the point of the
// test rather than a detail of it. A fresh pane already sits at [0,rows), so a
// case that starts there cannot tell "the sequence reset the region" from "the
// sequence was ignored and the default was still standing" — and being ignored
// is exactly what a bare `CSI r` did: `bot` was read with p1, which turns an
// absent parameter into 1, so `top >= bot` fired and DECSTBM was dropped on the
// floor with the old region left in force. The test passed anyway, which is how
// the bug shipped. A TUI that sets `CSI 2;20r` for a body under a fixed header
// and then emits a bare `CSI r` to restore the page stayed pinned at rows 2-20
// for the life of the pane.
func TestDECSTBMDefaultsToTheWholePage(t *testing.T) {
	// The region every case is dragged out of: a body under a fixed header,
	// which is the shape real TUIs set and then reset.
	const start = "\x1b[3;7r" // → [2,7)
	cases := []struct {
		name, seq        string
		wantTop, wantBot int
	}{
		{"bare CSI r — what Claude Code sends first", "\x1b[r", 0, 10},
		{"explicit zeros", "\x1b[0;0r", 0, 10},
		{"omitted bottom", "\x1b[1;r", 0, 10},
		{"omitted top", "\x1b[;10r", 0, 10},
		{"omitted top, zero bottom", "\x1b[;0r", 0, 10},
		{"a real region", "\x1b[2;8r", 1, 8},
		{"bottom past the page is clamped", "\x1b[1;99r", 0, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := decstbmPane(t)
			p.vt.write([]byte(start))
			if s := p.screen; s.scrollTop != 2 || s.scrollBot != 7 {
				t.Fatalf("setup: %q left the region at [%d,%d), want [2,7)", start, s.scrollTop, s.scrollBot)
			}
			p.vt.write([]byte(c.seq))
			s := p.screen
			if s.scrollTop != c.wantTop || s.scrollBot != c.wantBot {
				t.Fatalf("after %q, %q set the scroll region to [%d,%d), want [%d,%d) on a %d-row page",
					start, c.seq, s.scrollTop, s.scrollBot, c.wantTop, c.wantBot, s.rows)
			}
		})
	}
}

// TestBareDECSTBMRestoresThePageAfterAHeaderRegion is the same rule as a user
// sees it, and the failure the vacuous version above could not show: a pane
// pinned to a five-line body, told to go back to the whole page, must then use
// the whole page. Before the fix the bare CSI r was ignored and the output
// stayed trapped in rows 3-7.
func TestBareDECSTBMRestoresThePageAfterAHeaderRegion(t *testing.T) {
	p := decstbmPane(t)
	p.vt.write([]byte("\x1b[3;7r")) // body under a two-line header
	p.vt.write([]byte("\x1b[r"))    // and back to the whole page
	p.vt.write([]byte("\x1b[H"))    // home, which origin mode is off so is row 0

	// Nine lines onto a ten-row page: with the whole page as the region nothing
	// scrolls at all and every line keeps its own row.
	p.vt.write([]byte("l0\r\nl1\r\nl2\r\nl3\r\nl4\r\nl5\r\nl6\r\nl7\r\nl8"))

	shot := p.capture(0, true)
	want := "l0\nl1\nl2\nl3\nl4\nl5\nl6\nl7\nl8"
	if shot.Text != want {
		t.Fatalf("after CSI 3;7r then a bare CSI r the pane reads:\n%q\nwant:\n%q\n"+
			"(the bare CSI r was ignored and the region is still the header's body)",
			shot.Text, want)
	}
}

// TestDECSTBMRejectsAnInvertedOrDegenerateRegion is the other half: a
// scrolling region must be at least two lines, and a bottom above the top is
// nonsense. Neither may be honoured, because both reintroduce the one-line
// region that blanked the screen — and neither may wedge the parser.
func TestDECSTBMRejectsAnInvertedOrDegenerateRegion(t *testing.T) {
	for _, seq := range []string{
		"\x1b[5;5r",  // one line tall
		"\x1b[8;2r",  // inverted
		"\x1b[99;2r", // top past the page
		"\x1b[3;3r",
	} {
		p := decstbmPane(t)
		p.vt.write([]byte("\x1b[2;9r")) // a valid region first, to prove it survives
		p.vt.write([]byte(seq))
		s := p.screen
		if s.scrollBot-s.scrollTop < 2 {
			t.Errorf("%q left a %d-line scroll region [%d,%d) — a region that short means "+
				"every newline scrolls in place and the screen goes blank",
				seq, s.scrollBot-s.scrollTop, s.scrollTop, s.scrollBot)
		}
		if s.scrollTop != 1 || s.scrollBot != 9 {
			t.Errorf("%q was honoured (region now [%d,%d)); an invalid DECSTBM must leave "+
				"the previous region [1,9) alone", seq, s.scrollTop, s.scrollBot)
		}
	}
}

// TestOutputAfterBareDECSTBMFillsThePage is the bug as a user sees it, and the
// one that fails loudest before the fix: three printed lines end up on three
// rows, not stacked on top of each other on row 0.
func TestOutputAfterBareDECSTBMFillsThePage(t *testing.T) {
	p := decstbmPane(t)
	p.vt.write([]byte("\x1b[r"))
	p.vt.write([]byte("alpha\r\nbravo\r\ncharlie"))

	shot := p.capture(0, true)
	want := "alpha\nbravo\ncharlie"
	if shot.Text != want {
		t.Fatalf("after a bare CSI r the pane reads:\n%q\nwant:\n%q\n"+
			"(all three lines landing on one row is the blank-Claude-Code bug: the "+
			"scroll region was set to a single line and every LF erased the last)",
			shot.Text, want)
	}
	if shot.CurY != 2 {
		t.Fatalf("cursor is on row %d after two newlines, want row 2 — the cursor never "+
			"leaving row 0 is what pins every frame on top of the previous one", shot.CurY)
	}
}

// TestClaudeCodeOpeningSequenceRendersEndToEnd runs the real prologue Claude
// Code emits — the exact escape sequences captured from a live session, in
// order — through a real magmux process over a PTY, and asks magmux itself what
// the pane looks like. Before the fix `text` came back holding one line.
func TestClaudeCodeOpeningSequenceRendersEndToEnd(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY test requires darwin or linux")
	}

	binPath := magmuxBinForTest(t)

	// The prologue, verbatim: DECSTBM reset, cursor show/hide, bracketed paste,
	// focus events, mode 2031, keyboard-protocol push/pop, then three lines of
	// UI. Only the first sequence matters; the rest are here so the test breaks
	// if any of them start eating the page too.
	script := filepath.Join(t.TempDir(), "prologue.sh")
	body := "printf '\\033[r\\033[?25h\\033[?25l\\033[?2004h\\033[?1004h\\033[?2031h\\033[<u\\033[>1u'\n" +
		"printf 'ROW-ONE\\r\\nROW-TWO\\r\\nROW-THREE\\r\\n'\n" +
		"sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write prologue script: %v", err)
	}

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	// Without a size the pane is 0 rows and every assertion below is vacuous.
	setWinSize(master, 24, 100)

	sockID := fmt.Sprintf("decstbm-%d", os.Getpid())
	sockPath := "/tmp/magmux-" + sockID + ".sock"
	t.Cleanup(func() { os.Remove(sockPath) })

	cmd := exec.Command(binPath, "--id", sockID, "--theme", "dark", "-e", "sh "+script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	// magmux's own rendering has to be drained or it blocks on a full PTY, and
	// it is also the only place a startup error would appear — so keep it.
	var screenMu sync.Mutex
	var screen []byte
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				screenMu.Lock()
				screen = append(screen, buf[:n]...)
				screenMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	var text string
	var lastErr error
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		// `id` is what makes a request answerable — without it the verb runs and
		// the reply is never sent, and the read below would sit until the
		// deadline having proved nothing.
		_, _ = conn.Write([]byte(`{"type":"capture","pane":"0","id":1}` + "\n"))
		scanner := bufio.NewScanner(conn)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			var ev struct {
				Type   string `json:"type"`
				Result struct {
					Text string `json:"text"`
				} `json:"result"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				continue
			}
			if ev.Type == "reply" {
				text = ev.Result.Text
				break // the connection stays open; nothing further is coming
			}
		}
		lastErr = scanner.Err()
		conn.Close()
		if strings.Contains(text, "ROW-THREE") {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if text == "" {
		screenMu.Lock()
		out := string(screen)
		screenMu.Unlock()
		t.Fatalf("the pane captured completely blank — this IS the reported bug, verbatim "+
			"(last socket error: %v). magmux wrote:\n%q", lastErr, out)
	}

	for _, want := range []string{"ROW-ONE", "ROW-TWO", "ROW-THREE"} {
		if !strings.Contains(text, want) {
			t.Fatalf("pane capture is missing %q; it reads:\n%q\n"+
				"Every line overwriting the one before it on row 0 is the blank pane: "+
				"the bare CSI r that opens Claude Code's output set the scroll region "+
				"to a single line", want, text)
		}
	}
	if lines := strings.Split(text, "\n"); len(lines) < 3 {
		t.Fatalf("pane capture has %d line(s), want at least 3:\n%q", len(lines), text)
	}
}
