package pty

import (
	"os"

	"golang.org/x/sys/unix"
)

// ── PTY helpers — see pty_darwin.go / pty_linux.go ────────────────────────────

func SetWinSize(f *os.File, rows, cols int) {
	unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}
