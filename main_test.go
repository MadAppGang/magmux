package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

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

	// Locate the magmux binary. Prefer the freshly-built one in repo root;
	// fall back to building one in a tempdir if not present.
	binPath, err := filepath.Abs("./magmux")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, err := os.Stat(binPath); err != nil {
		// Build a binary into a tempdir for the test.
		tmp := t.TempDir()
		binPath = filepath.Join(tmp, "magmux")
		build := exec.Command("go", "build", "-o", binPath, ".")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build magmux: %v\n%s", err, out)
		}
	}

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
