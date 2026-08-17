package main

// Headless mode: a magmux with no terminal at all.
//
// This file holds the repo's FIRST subprocess tests that do not allocate a pty.
// Every other end-to-end test goes through openPTY() (startRPCMagmux,
// sockrpc_test.go) because magmux refused to start without a terminal —
// --headless is precisely the removal of that requirement, so this harness is
// both the feature's test and its proof.
//
// It also holds the ONE test that still needs a real pty on purpose:
// TestPTYRunStillPaintsStdout, the positive control. If auto-degrade ever
// misfires, every socket assertion in the repo keeps passing against a magmux
// that never paints a single byte, and nothing else in the suite notices.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

// ── harness ─────────────────────────────────────────────────────────────────

// syncBuffer is a bytes.Buffer a test may read while exec's copier goroutine is
// still writing to it. exec.Cmd starts one such goroutine per non-*os.File
// stdio writer and only joins them in cmd.Wait, so a bare bytes.Buffer read
// mid-run is a data race -race reports — and the whole point here is to assert
// on stdout BEFORE the process has exited.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

type headlessMagmux struct {
	t              *testing.T
	cmd            *exec.Cmd
	dir            string
	sock           string
	stdout, stderr *syncBuffer

	// exited closes once the single cmd.Wait has returned. waitErr is written
	// before the close and read only after it, which is the whole
	// synchronisation this needs.
	exited  chan struct{}
	waitErr error
}

// startHeadlessMagmux spawns magmux with NO pty and NO terminal on any of its
// three standard descriptors.
//
// It deliberately does not use stopMagmux: that helper calls
// cmd.Process.Wait(), and this harness already has a goroutine in cmd.Wait()
// (which it needs, to join exec's stdout/stderr copiers before a test reads the
// buffers). Two concurrent waits on one process race for the status. The
// escalation below is the same one stopMagmux performs, done inline.
func startHeadlessMagmux(t *testing.T, args ...string) *headlessMagmux {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("subprocess test requires darwin or linux")
	}
	bin := magmuxBinForTest(t)

	// sockTestDir, NOT t.TempDir(). On darwin TMPDIR is ~80 bytes and the test's
	// own NAME is part of the path, so a unix socket under t.TempDir() overruns
	// sun_path and net.Listen fails with "bind: invalid argument" — and a
	// shorter test name sits just under the limit, so the same harness would
	// pass or fail according to what the test is called. Every fixture in this
	// repo that creates a real socket uses sockTestDir for exactly this reason.
	dir := sockTestDir(t)
	args = append([]string{"--sock-dir", dir}, args...)

	cmd := exec.Command(bin, args...)
	// Stdin nil => exec hands the child os.DevNull, which is not a terminal, so
	// auto-degrade fires with no flag at all. Tests that want the FORCED path
	// pass --headless explicitly and assert the same outcome.
	cmd.Stdin = nil
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// No SysProcAttr: Setsid/Setctty exist only to give a child a controlling
	// terminal, and there is no terminal here.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}

	h := &headlessMagmux{
		t: t, cmd: cmd, dir: dir,
		sock:   filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", cmd.Process.Pid)),
		stdout: stdout, stderr: stderr,
		exited: make(chan struct{}),
	}
	go func() {
		h.waitErr = cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-h.exited:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-h.exited:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGKILL)
			<-h.exited
		}
	})
	return h
}

// wait blocks for the process and returns its exit code. A timeout is a fatal
// test failure, not a returned error: a headless magmux that does not exit is
// exactly the R-3/R-5 hang this mode is most at risk of, and reporting it as
// "exit code -1" would let a caller squint past it.
func (h *headlessMagmux) wait(d time.Duration) int {
	h.t.Helper()
	select {
	case <-h.exited:
	case <-time.After(d):
		_ = h.cmd.Process.Signal(syscall.SIGKILL)
		<-h.exited
		h.t.Fatalf("magmux did not exit within %v\nstderr: %s", d, h.stderr.String())
	}
	if h.waitErr == nil {
		return 0
	}
	if ee, ok := h.waitErr.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	h.t.Fatalf("wait magmux: %v", h.waitErr)
	return -1
}

// requireSilentStdout is AC1.4, and it is asserted by byte count rather than by
// "looks like no escape codes": headless means NOTHING, so a single byte is a
// failure whatever it is.
func (h *headlessMagmux) requireSilentStdout() {
	h.t.Helper()
	if n := h.stdout.Len(); n != 0 {
		h.t.Fatalf("headless magmux wrote %d bytes to stdout, want 0: %q",
			n, h.stdout.String())
	}
}

// ── in-process unit tests ───────────────────────────────────────────────────

// TestWriteTermIsSilentHeadless pins the single suppression point.
//
// The m.out half matters: "headless" means "emits no frame", one rule. A future
// reader tempted to let an injected writer through would make the mode mean two
// different things depending on who is looking.
func TestWriteTermIsSilentHeadless(t *testing.T) {
	var buf bytes.Buffer
	m := &Magmux{headless: true, out: &buf}
	m.writeTerm("\x1b[H\x1b[2Jframe")
	if buf.Len() != 0 {
		t.Fatalf("headless writeTerm emitted %q, want nothing", buf.String())
	}

	// The zero value is today's behaviour, and this is what says so.
	var buf2 bytes.Buffer
	m2 := &Magmux{out: &buf2}
	m2.writeTerm("frame")
	if buf2.String() != "frame" {
		t.Fatalf("interactive writeTerm emitted %q, want %q", buf2.String(), "frame")
	}
}

// TestInitAutoDegradesOnPipedStdin is the auto-degrade with no subprocess: a
// pipe is not a terminal, so init() must turn the mode on by itself.
//
// No existing test calls init() at all, which is why this one also asserts the
// whole post-condition rather than just the flag.
func TestInitAutoDegradesOnPipedStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	m := &Magmux{stdin: r}
	if err := m.init(); err != nil {
		t.Fatalf("init with a piped stdin must succeed, got %v", err)
	}
	if !m.headless {
		t.Fatal("init did not auto-degrade to headless on a non-terminal stdin")
	}
	if m.rows != 24 || m.cols != 80 {
		t.Errorf("headless geometry is %dx%d, want 24x80", m.rows, m.cols)
	}
	if m.quit == nil {
		t.Error("init did not make m.quit; renderLoop would spin and -w could never fire")
	}
	if m.rawState != nil {
		t.Error("init put a pipe in raw mode")
	}
}

// TestInitHeadlessFlagIsNeverCleared: the flag FORCES the mode. Auto-degrade
// may only ever turn it on.
func TestInitHeadlessFlagIsNeverCleared(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	m := &Magmux{stdin: r, headless: true}
	if err := m.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !m.headless {
		t.Fatal("init cleared an explicitly-set headless flag")
	}
}

// TestHeadlessSizeBounds. COLUMNS/LINES come from the ENVIRONMENT, the input
// class nobody validates: 0 gives every pane a zero-width screen whose capture
// is empty with no error anywhere, and 99999999 is an OOM with a stack trace.
func TestHeadlessSizeBounds(t *testing.T) {
	cases := []struct {
		lines, cols        string
		wantRows, wantCols int
	}{
		{"", "", 24, 80},
		{"50", "132", 50, 132},
		{"0", "0", 24, 80},
		{"-5", "-5", 24, 80},
		{"abc", "abc", 24, 80},
		{"99999999", "99999999", 24, 80},
		{"10000", "10000", 24, 80}, // the per-side bound is exclusive
		{"", "100", 24, 100},       // one set, one not

		// The PRODUCT cap. This case previously expected 9999x9999 to be
		// HONOURED, on the reasoning that each side is under the per-side bound
		// — which is true and is not the question. 9999*9999 is 99,980,001
		// Cells at 20 bytes each: a ~2GB allocation before any output, i.e.
		// exactly the OOM headlessSize's comment claims to prevent. A per-side
		// bound cannot express that; only the product can.
		{"9999", "9999", 24, 80},
		// Either side alone is legal and the pair is not — the case a per-side
		// bound is structurally unable to catch.
		{"2001", "2000", 24, 80},
		// At the cap, not over it: the comparison is `>`, so this is honoured.
		// Pins the boundary from the legal side, so a `>=` typo is caught.
		{"2000", "2000", 2000, 2000},
		// A genuinely large but sane terminal must NOT be refused. Guards
		// against the cap being set so low it breaks real callers.
		{"500", "1000", 500, 1000},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("LINES=%q_COLUMNS=%q", c.lines, c.cols), func(t *testing.T) {
			t.Setenv("LINES", c.lines)
			t.Setenv("COLUMNS", c.cols)
			if c.lines == "" {
				os.Unsetenv("LINES")
			}
			if c.cols == "" {
				os.Unsetenv("COLUMNS")
			}
			rows, cols := headlessSize()
			if rows != c.wantRows || cols != c.wantCols {
				t.Errorf("headlessSize() = %dx%d, want %dx%d", rows, cols, c.wantRows, c.wantCols)
			}
		})
	}
}

// TestHeadlessInheritsPaneGeometry states the deliberate consequence of reading
// COLUMNS/LINES first: magmux exports exactly those two to its own children, so
// a headless magmux started INSIDE a magmux pane inherits that pane's size. It
// is the right answer and it is surprising, so it is written down here.
func TestHeadlessInheritsPaneGeometry(t *testing.T) {
	t.Setenv("LINES", "17")
	t.Setenv("COLUMNS", "63")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	m := &Magmux{stdin: r}
	if err := m.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if m.rows != 17 || m.cols != 63 {
		t.Fatalf("headless geometry is %dx%d, want the inherited 17x63", m.rows, m.cols)
	}
}

// TestAutoExitNoOpShapes pins the two commands where -w can never fire, both of
// which are documented in --help because they are indistinguishable from a hang.
func TestAutoExitNoOpShapes(t *testing.T) {
	// Shape 1: no -e/-g at all => gridMode false => the -w branch is not even
	// evaluated. Asserted on the guard's own terms.
	m := &Magmux{autoExit: true, gridMode: false}
	if m.autoExit && m.gridMode {
		t.Fatal("gridMode must be false without -e/-g")
	}

	// Shape 2: -c with no -e. The only pane is the control panel, which
	// allPanesDoneLocked skips, so sessions stays 0 forever.
	panel := &Pane{isControl: true}
	m2 := &Magmux{autoExit: true, gridMode: true, allPanes: []*Pane{panel}}
	if m2.allPanesDone() {
		t.Fatal("a lone control panel must never satisfy -w: it is not a session")
	}
}

// ── subprocess tests, no pty ────────────────────────────────────────────────

// TestHeadlessAutoDegradeExitsCleanly is THE bug from the report:
// `magmux -w -e 'echo hi' < /dev/null` exited 1 with
// "raw mode: operation not supported by device".
func TestHeadlessAutoDegradeExitsCleanly(t *testing.T) {
	h := startHeadlessMagmux(t, "-w", "-e", "echo hi")
	if code := h.wait(20 * time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0\nstderr: %s", code, h.stderr.String())
	}
	h.requireSilentStdout()
	if s := h.stderr.String(); s != "" {
		t.Errorf("stderr should be empty on a clean run, got %q", s)
	}
}

// TestHeadlessFlagWritesNothingToStdout is AC1.4 on the FORCED path.
func TestHeadlessFlagWritesNothingToStdout(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless", "-w", "-e", "echo hi", "-e", "echo there")
	if code := h.wait(20 * time.Second); code != 0 {
		t.Fatalf("exit code %d, want 0\nstderr: %s", code, h.stderr.String())
	}
	h.requireSilentStdout()
}

// TestHeadlessResultsCarriesEveryPane reads the socket, which headless makes the
// whole interface. It also pins that the hidden control panel still exists and
// still reports state "panel" — headless changes nothing about magmux's chrome
// model, only about whether it is painted.
func TestHeadlessResultsCarriesEveryPane(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless", "-w", "-e", "echo alpha", "-e", "echo beta")
	conn := dialASAP(t, h.sock, 10*time.Second)
	rc := &rpcConn{t: t, c: conn, sc: newLineScanner(conn)}

	results := rc.awaitResults()
	if len(results) != 3 {
		t.Fatalf("results has %d panes, want 3 (two -e plus the hidden panel): %v", len(results), results)
	}
	sessions, panels := 0, 0
	for _, r := range results {
		switch r["state"] {
		case "panel":
			panels++
			if r["hidden"] != true {
				t.Errorf("the panel must still be hidden without -c: %v", r)
			}
		case "completed":
			sessions++
		default:
			t.Errorf("unexpected pane state %v in %v", r["state"], r)
		}
	}
	if sessions != 2 || panels != 1 {
		t.Fatalf("want 2 completed sessions and 1 panel, got %d and %d", sessions, panels)
	}
	h.wait(20 * time.Second)
	h.requireSilentStdout()
}

// TestHeadlessConcurrentRuns is the madbench shape: several headless magmuxes
// at once, each with its own socket directory. The concurrency cap was the
// actual blocker — a run that needs one pty per magmux is limited by the
// machine's pty table and by whatever else is holding one.
func TestHeadlessConcurrentRuns(t *testing.T) {
	const n = 4
	var wg sync.WaitGroup
	fail := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := startHeadlessMagmux(t, "--headless", "-w",
				"-e", fmt.Sprintf("echo run-%d", i))
			if code := h.wait(30 * time.Second); code != 0 {
				fail <- fmt.Sprintf("run %d: exit %d, stderr %q", i, code, h.stderr.String())
				return
			}
			if h.stdout.Len() != 0 {
				fail <- fmt.Sprintf("run %d: %d bytes on stdout", i, h.stdout.Len())
			}
		}(i)
	}
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Error(msg)
	}
}

// TestHeadlessRunsUntilSignalled covers R-5: with no -e there is no session, so
// -w can never fire and the run legitimately stays up. That is a mode, not a
// hang — but only because kill -TERM ends it, which is what this asserts.
func TestHeadlessRunsUntilSignalled(t *testing.T) {
	h := startHeadlessMagmux(t, "--headless", "-w", "-c")
	// It must still be alive after a beat: an immediate exit would mean -w fired
	// on a layout with no sessions in it.
	select {
	case <-h.exited:
		t.Fatalf("headless -w -c exited on its own (code %d); the panel is not a session",
			h.wait(time.Second))
	case <-time.After(750 * time.Millisecond):
	}
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := h.wait(10 * time.Second); code != 0 {
		t.Fatalf("SIGTERM exit code %d, want 0\nstderr: %s", code, h.stderr.String())
	}
	h.requireSilentStdout()
}

// ── the positive control, which DOES need a pty ─────────────────────────────

// TestPTYRunStillPaintsStdout is the guard on risk R-2.
//
// If auto-degrade ever misfires — a stdin check that gets inverted, a harness
// that stops handing over a terminal — every end-to-end test in this repo keeps
// passing against a magmux that never paints anything, because they all assert
// on the socket. Nothing else in the suite would notice. This is the one test
// that fails.
func TestPTYRunStillPaintsStdout(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY test requires darwin or linux")
	}
	bin := magmuxBinForTest(t)

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	setWinSize(master, 24, 100)

	cmd := exec.Command(bin, "-e", "sleep 30")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	t.Cleanup(func() {
		stopMagmux(t, cmd)
		master.Close()
		slave.Close()
	})

	// Drain the master FOREVER, exactly as startRPCMagmux does, and sample the
	// drained bytes. Reading only until the assertion is satisfied and then
	// stopping wedges the whole test: the pty buffer fills, magmux blocks in
	// os.Stdout.WriteString, SIGTERM cannot get it past the restore() write on
	// the way out, and the teardown escalates into two concurrent
	// os.Process.Wait calls. The drain is not incidental scaffolding.
	//
	// Only magmux writes here: each pane's child has its own pty, so every byte
	// on this one came from init()'s alt-screen sequence or from the renderer.
	got := &syncBuffer{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				got.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for got.Len() < 512 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if got.Len() == 0 {
		t.Fatal("magmux under a pty wrote NOTHING to stdout — auto-degrade has misfired " +
			"and every socket test in this repo is now running against a magmux that never paints")
	}
	if !strings.Contains(got.String(), "\x1b[?1049h") {
		t.Errorf("no alternate-screen entry in %d bytes of pty output; init() did not take the tty path",
			got.Len())
	}
	t.Logf("pty positive control: %d bytes painted", got.Len())
}

// ── shared helpers ──────────────────────────────────────────────────────────

// newLineScanner is rpcMagmux.dial()'s scanner, extracted so a connection made
// without that pty-bound harness gets the same 1MB line budget. A default
// bufio.Scanner caps at 64KB and a `results` for a busy grid exceeds that,
// which surfaces as a stream that simply ends.
func newLineScanner(conn net.Conn) *bufio.Scanner {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return sc
}

// awaitResults reads until the final `results` event and returns its panes.
// results, never an `exit` event: `exit` is a live broadcast from
// waitForChild's own goroutine with no delivery guarantee, and under -w it
// races the teardown -w itself triggers. CLAUDE.md records that trap as
// diagnosed twice.
func (rc *rpcConn) awaitResults() []map[string]any {
	rc.t.Helper()
	for {
		ev, _, ok := rc.next()
		if !ok {
			rc.t.Fatal("stream ended before `results` arrived")
		}
		if ev["type"] != "results" {
			continue
		}
		raw, ok := ev["panes"].([]any)
		if !ok {
			rc.t.Fatalf("results has no panes array: %v", ev)
		}
		out := make([]map[string]any, 0, len(raw))
		for _, r := range raw {
			m, ok := r.(map[string]any)
			if !ok {
				rc.t.Fatalf("results pane is not an object: %v", r)
			}
			out = append(out, m)
		}
		return out
	}
}

// paneNum pulls an integer out of a JSON-decoded results entry. Every number in
// a decoded event is a float64, and comparing one to an int literal silently
// fails to compile or silently compares false depending on how it is written.
func paneNum(t *testing.T, entry map[string]any, key string) int {
	t.Helper()
	v, ok := entry[key]
	if !ok {
		t.Fatalf("results entry has no %q: %v", key, entry)
	}
	f, ok := v.(float64)
	if !ok {
		if n, ok := v.(json.Number); ok {
			i, err := n.Int64()
			if err != nil {
				t.Fatalf("%q is not an integer: %v", key, v)
			}
			return int(i)
		}
		t.Fatalf("%q is not a number: %v (%T)", key, v, v)
	}
	return int(f)
}
