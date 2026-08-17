package main

// End-to-end coverage for --sock-dir. The unit half lives in sockdir_test.go;
// this half is about the three things only a real process can show: that magmux
// actually BINDS where it was told, that the pid socket in /tmp is not created
// as well, and that a child pane inherits both MAGMUX_SOCK and
// MAGMUX_SOCK_DIR — the second being the only channel that can reach a
// `magmux mcp` started from inside a pane.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSockDirFlagBindsElsewhere(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY/socket test requires darwin or linux")
	}
	binPath := magmuxBinForTest(t)

	// Not t.TempDir(): on darwin TMPDIR is ~80 bytes and the test name is part
	// of the path, so the socket would overrun sun_path and net.Listen would
	// fail with "invalid argument" — the exact failure --sock-dir's length
	// check exists to report, and not the thing under test here.
	dir := sockTestDir(t)

	master, slave, err := openPTY()
	if err != nil {
		t.Fatalf("openPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	setWinSize(master, 24, 100)

	// The pane writes both variables to a FILE rather than to its stdout. Its
	// stdout is the PTY, where the text arrives interleaved with frames and
	// wrapped at the pane's width; and the other obvious source, an `exit`
	// event's lastLine, is a live broadcast with no delivery guarantee that
	// races teardown under -w — a trap CLAUDE.md records as diagnosed twice.
	// A file is neither.
	envFile := filepath.Join(dir, "env.txt")
	cmd := exec.Command(binPath, "--sock-dir", dir, "-e",
		fmt.Sprintf(`sh -c 'printf "SOCK=%%s DIR=%%s\n" "$MAGMUX_SOCK" "$MAGMUX_SOCK_DIR" > %s; sleep 2'`, envFile), "-w")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	defer stopMagmux(t, cmd)

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	want := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", cmd.Process.Pid))
	dialASAP(t, want, 10*time.Second)

	// The pid socket in /tmp must NOT also exist: --sock-dir moves the socket,
	// it does not add one.
	if _, err := os.Stat(fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid)); err == nil {
		t.Errorf("/tmp/magmux-%d.sock exists as well as %s; --sock-dir must move the socket, not add one",
			cmd.Process.Pid, want)
	}

	var got string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(envFile); err == nil && len(b) > 0 {
			got = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, "SOCK="+want) {
		t.Errorf("the pane reported %q; want MAGMUX_SOCK=%s — a socket panes cannot see is a half-implemented flag", got, want)
	}
	if !strings.Contains(got, "DIR="+dir) {
		t.Errorf("the pane reported %q; want MAGMUX_SOCK_DIR=%s — children must inherit it, because "+
			"that is the only channel that reaches a `magmux mcp` started inside a pane", got, dir)
	}
}

// TestSockDirFlagFallsBackLoudly pins the never-fatal contract: a bad
// --sock-dir costs the caller its chosen path and nothing else, and says so.
// Silence here is the worst outcome — an agent polling the directory it asked
// for would simply hang.
func TestSockDirFlagFallsBackLoudly(t *testing.T) {
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

	missing := filepath.Join(sockTestDir(t), "definitely-not-here")

	// stderr on its own pipe: on the PTY it would be tangled up in the frames.
	cmd := exec.Command(binPath, "--sock-dir", missing, "-e", "true", "-w")
	cmd.Stdin, cmd.Stdout = slave, slave
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start magmux: %v", err)
	}
	defer stopMagmux(t, cmd)

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := master.Read(buf); err != nil {
				return
			}
		}
	}()

	// It must still come up, on the default path.
	fallback := fmt.Sprintf("/tmp/magmux-%d.sock", cmd.Process.Pid)
	dialASAP(t, fallback, 10*time.Second)

	buf := make([]byte, 4096)
	n, _ := stderr.Read(buf)
	msg := string(buf[:n])
	if !strings.Contains(msg, "--sock-dir") {
		t.Errorf("stderr did not mention the ignored --sock-dir; got %q", msg)
	}
	if !strings.Contains(msg, "/tmp") {
		t.Errorf("stderr did not name the directory actually used; got %q", msg)
	}
}
