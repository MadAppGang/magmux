package pty

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Mode is the part of a terminal's termios that says whether, and how, the
// program behind it is reading input.
type Mode struct {
	Canonical bool   // ICANON set: the cooked line discipline
	Echo      bool   // ECHO set
	sum       uint64 // fingerprint over Iflag, Oflag, Cflag, Lflag, Cc[VMIN], Cc[VTIME]
}

// Same reports whether two modes are indistinguishable: the same flags and the
// same fingerprint. A change anywhere in the fingerprinted fields, including a
// VMIN/VTIME switch with ICANON unchanged, is a change.
func (m Mode) Same(o Mode) bool {
	return m.Canonical == o.Canonical && m.Echo == o.Echo && m.sum == o.sum
}

// ReadMode reads the termios of the terminal behind f, which is a PTY MASTER.
// The kernel reports the slave's settings, which are the child's, on a master.
// darwin: measured (research F2), with the parent's slave copy already closed.
// linux: tty_mode_ioctl redirects a master's TCGETS to tty->link. Both are
// proven by TestMasterReportsSlaveTermios.
//
// The ioctl runs inside f.SyscallConn().Control, never on f.Fd(). Control
// holds a reference on the descriptor for the duration, so a master closed by
// another goroutine fails cleanly ("use of closed file") instead of the ioctl
// landing on whatever file the kernel has since given that number. Fd() would
// also switch the descriptor to blocking mode.
//
// One ioctl and one small allocation; it never blocks.
func ReadMode(f *os.File) (Mode, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return Mode{}, fmt.Errorf("termios: %w", err)
	}
	var (
		t    *unix.Termios
		ierr error
	)
	if err := rc.Control(func(fd uintptr) {
		t, ierr = unix.IoctlGetTermios(int(fd), ioctlReadTermios)
	}); err != nil {
		return Mode{}, fmt.Errorf("termios: %w", err)
	}
	if ierr != nil {
		return Mode{}, fmt.Errorf("termios: %w", ierr)
	}
	return modeOf(t), nil
}

// modeOf reduces a termios to a Mode. The uint64 conversions absorb the flag
// width difference between darwin (uint64) and linux (uint32).
func modeOf(t *unix.Termios) Mode {
	lflag := uint64(t.Lflag)
	return Mode{
		Canonical: lflag&unix.ICANON != 0,
		Echo:      lflag&unix.ECHO != 0,
		sum: fingerprint(
			uint64(t.Iflag),
			uint64(t.Oflag),
			uint64(t.Cflag),
			lflag,
			uint64(t.Cc[unix.VMIN]),
			uint64(t.Cc[unix.VTIME]),
		),
	}
}

// fingerprint is FNV-1a over the values' bytes. It only has to tell two
// successive samples of one terminal apart, not resist an adversary.
func fingerprint(vs ...uint64) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, v := range vs {
		for i := 0; i < 8; i++ {
			h ^= v & 0xff
			h *= prime
			v >>= 8
		}
	}
	return h
}
