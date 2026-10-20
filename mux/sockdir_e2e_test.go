package mux

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

	"github.com/MadAppGang/magmux/pty"
	"github.com/MadAppGang/magmux/unixsock/unixsocktest"
)

func TestSockDirFlagBindsElsewhere(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("PTY/socket test requires darwin or linux")
	}
	binPath := magmuxBinForTest(t)

	// Not t.TempDir(): on darwin TMPDIR is ~80 bytes and the test name is part
	// of the path, so the socket would overrun sun_path. Depth is
	// TestSockDirFlagBindsDeepDir's subject, not this test's.
	dir := sockTestDir(t)

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer master.Close()
	defer slave.Close()
	pty.SetWinSize(master, 24, 100)

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

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestSockDirFlagBindsDeepDir: a --sock-dir whose socket path is past sun_path
// is honoured — magmux binds there, a pane sees it in MAGMUX_SOCK, and /tmp
// gets nothing.
func TestSockDirFlagBindsDeepDir(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("subprocess test requires darwin or linux")
	}
	bin := magmuxBinForTest(t)

	deep := unixsocktest.DeepDir(t, "magmux-1.sock", unixsocktest.DeepPathLen)
	envFile := filepath.Join(deep, "env.txt")
	h := spawnHeadlessMagmux(t, bin, deep, []string{"MAGMUX_SOCK_DIR="},
		"--sock-dir", deep, "-w", "-e",
		fmt.Sprintf(`sh -c 'printf "SOCK=%%s DIR=%%s\n" "$MAGMUX_SOCK" "$MAGMUX_SOCK_DIR" > %s; sleep 2'`, envFile))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("magmux stderr: %s", h.stderr.String())
		}
	})
	if len(h.sock) < unixsocktest.DeepPathLen {
		t.Fatalf("the socket path is %d bytes, want >= %d: %s", len(h.sock), unixsocktest.DeepPathLen, h.sock)
	}

	dialASAP(t, h.sock, 10*time.Second)

	if _, err := os.Lstat(fmt.Sprintf("/tmp/magmux-%d.sock", h.cmd.Process.Pid)); err == nil {
		t.Errorf("/tmp/magmux-%d.sock exists; a deep --sock-dir must be honoured, not moved to /tmp", h.cmd.Process.Pid)
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
	if !strings.Contains(got, "SOCK="+h.sock+" ") {
		t.Errorf("the pane reported %q; want MAGMUX_SOCK=%s", got, h.sock)
	}
}

// requireRefusal asserts a loud refusal: magmux exits non-zero within 5s,
// stderr names every string in wants, and no pid socket appeared in /tmp.
func requireRefusal(t *testing.T, h *headlessMagmux, wants ...string) {
	t.Helper()
	code, exited := h.exitWithin(5 * time.Second)
	// Checked before the cleanup stops a magmux that is still running: its
	// teardown removes the socket.
	tmpSock := fmt.Sprintf("/tmp/magmux-%d.sock", h.cmd.Process.Pid)
	if _, err := os.Lstat(tmpSock); err == nil {
		t.Errorf("%s exists: magmux fell back to /tmp instead of refusing", tmpSock)
	}
	stderr := h.stderr.String()
	if !exited {
		t.Fatalf("magmux is still running after 5s; want a non-zero exit\nstderr: %s", stderr)
	}
	if code == 0 {
		t.Errorf("magmux exited 0; want non-zero\nstderr: %s", stderr)
	}
	for _, want := range wants {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not name %q; got %q", want, stderr)
		}
	}
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestSockDirFlagRefusedLoudly: an unusable --sock-dir is fatal. An agent
// polling the directory it asked for must get an exit status, not a socket
// somewhere else.
func TestSockDirFlagRefusedLoudly(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("subprocess test requires darwin or linux")
	}
	bin := magmuxBinForTest(t)
	missing := filepath.Join(sockTestDir(t), "definitely-not-here")

	h := spawnHeadlessMagmux(t, bin, missing, []string{"MAGMUX_SOCK_DIR="},
		"--sock-dir", missing, "-e", "sleep 30")
	requireRefusal(t, h, "--sock-dir", missing, "cannot stat")
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestSockDirEnvRefusedLoudly: the same rule for MAGMUX_SOCK_DIR. The env and
// the flag go through one resolver, so they fail the same way.
func TestSockDirEnvRefusedLoudly(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("subprocess test requires darwin or linux")
	}
	bin := magmuxBinForTest(t)
	missing := filepath.Join(sockTestDir(t), "definitely-not-here")

	h := spawnHeadlessMagmux(t, bin, missing, []string{"MAGMUX_SOCK_DIR=" + missing},
		"-e", "sleep 30")
	requireRefusal(t, h, "MAGMUX_SOCK_DIR", missing)
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestSockDirUnwritableExitsNonZero: a directory magmux cannot bind in is
// fatal, whichever check catches it. Mode 0500 has no write bit at all, so the
// directory check refuses it before any bind. Mode 0520 passes that check (a
// write bit is set, for the group) and is still unwritable by its owner, so
// only the bind can find out — and a bind error must end the process, not
// leave it running with no socket.
func TestSockDirUnwritableExitsNonZero(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("subprocess test requires darwin or linux")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes through any mode bits")
	}
	bin := magmuxBinForTest(t)

	cases := []struct {
		name  string
		mode  os.FileMode
		wants []string
	}{
		{"0500 refused by the directory check", 0o500, []string{"--sock-dir", "not writable"}},
		{"0520 refused by the bind", 0o520, []string{"magmux: socket"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(sockTestDir(t), "ro")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, c.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })

			h := spawnHeadlessMagmux(t, bin, dir, []string{"MAGMUX_SOCK_DIR="},
				"--sock-dir", dir, "-e", "sleep 30")
			requireRefusal(t, h, append([]string{dir}, c.wants...)...)
		})
	}
}
