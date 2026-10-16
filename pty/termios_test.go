package pty

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// waitMode polls ReadMode on the master until want holds or the deadline
// passes, and returns the last mode it read.
func waitMode(t *testing.T, ptmxRead func() (Mode, error), want func(Mode) bool, within time.Duration) Mode {
	t.Helper()
	deadline := time.Now().Add(within)
	var m Mode
	for {
		var err error
		m, err = ptmxRead()
		if err != nil {
			t.Fatalf("ReadMode: %v", err)
		}
		if want(m) || time.Now().After(deadline) {
			return m
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMasterReportsSlaveTermios is the proof the design rests on (research F2):
// a termios read on the MASTER, after the parent has closed its own copy of the
// slave, reports the settings the CHILD put on the slave. darwin was measured;
// on linux it is tty_mode_ioctl routing a master's TCGETS through tty->link,
// and this test is where that claim is checked — CI runs ubuntu and macOS.
func TestMasterReportsSlaveTermios(t *testing.T) {
	ptmx, pts, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ptmx.Close()

	read := func() (Mode, error) { return ReadMode(ptmx) }

	before, err := read()
	if err != nil {
		t.Fatalf("ReadMode before the child: %v", err)
	}
	if !before.Canonical {
		t.Fatalf("a fresh pty must start cooked, got %+v", before)
	}

	// The child flips its terminal raw, holds it, then puts it back. The
	// windows are generous so a loaded CI runner cannot step over one.
	cmd := exec.Command("/bin/sh", "-c", "stty raw -echo; sleep 1.5; stty sane; sleep 30")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	// The parent's slave copy goes away, exactly as spawnPTY does after Start.
	if err := pts.Close(); err != nil {
		t.Fatalf("close slave: %v", err)
	}

	// Drain the master so nothing the child writes can ever block it.
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()

	raw := waitMode(t, read, func(m Mode) bool { return !m.Canonical }, 10*time.Second)
	if raw.Canonical {
		t.Fatalf("master never reported the child's `stty raw`: %+v", raw)
	}
	if raw.Echo {
		t.Errorf("master reported ECHO after `stty -echo`: %+v", raw)
	}
	if raw.Same(before) {
		t.Errorf("raw and cooked modes must fingerprint differently")
	}

	sane := waitMode(t, read, func(m Mode) bool { return m.Canonical }, 10*time.Second)
	if !sane.Canonical {
		t.Fatalf("master never reported the child's `stty sane`: %+v", sane)
	}
	if !sane.Echo {
		t.Errorf("master did not report ECHO after `stty sane`: %+v", sane)
	}
}

// TestReadModeAfterCloseErrs pins why ReadMode goes through SyscallConn rather
// than Fd(): a closed master is an error, never an ioctl on whatever file the
// kernel has since handed that descriptor number to.
func TestReadModeAfterCloseErrs(t *testing.T) {
	ptmx, pts, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	pts.Close()
	if _, err := ReadMode(ptmx); err != nil {
		t.Fatalf("ReadMode on an open master: %v", err)
	}
	ptmx.Close()
	if m, err := ReadMode(ptmx); err == nil {
		t.Fatalf("ReadMode on a closed master returned %+v and no error", m)
	}
	if _, err := ReadMode(nil); err == nil {
		t.Fatalf("ReadMode(nil) returned no error")
	}
}

// TestModeFingerprint: Same compares every field the fingerprint covers, so a
// change to VMIN alone — a TUI switching read strategies — is a change.
func TestModeFingerprint(t *testing.T) {
	base := unix.Termios{Lflag: unix.ICANON | unix.ECHO}
	base.Cc[unix.VMIN] = 1

	a := modeOf(&base)
	if !a.Canonical || !a.Echo {
		t.Fatalf("modeOf lost the flags: %+v", a)
	}
	if !a.Same(modeOf(&base)) {
		t.Fatalf("a mode is not the same as itself")
	}

	mutate := map[string]func(*unix.Termios){
		"Iflag": func(x *unix.Termios) { x.Iflag |= unix.ICRNL },
		"Oflag": func(x *unix.Termios) { x.Oflag |= unix.OPOST },
		"Cflag": func(x *unix.Termios) { x.Cflag |= unix.CS8 },
		"Lflag": func(x *unix.Termios) { x.Lflag |= unix.ISIG },
		"VMIN":  func(x *unix.Termios) { x.Cc[unix.VMIN] = 0 },
		"VTIME": func(x *unix.Termios) { x.Cc[unix.VTIME] = 5 },
	}
	for name, f := range mutate {
		c := base
		f(&c)
		if a.Same(modeOf(&c)) {
			t.Errorf("a change to %s is not seen by Same", name)
		}
	}

	// Fields outside the fingerprint (another control character) are not.
	c := base
	c.Cc[unix.VINTR] = 0x7f
	if !a.Same(modeOf(&c)) {
		t.Errorf("a control character outside the fingerprint changed it")
	}
}
