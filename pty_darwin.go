//go:build darwin

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (master *os.File, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	fd := int(master.Fd())

	// grantpt
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("grantpt: %w", err)
	}

	// unlockpt
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}

	// ptsname
	var nameBuf [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&nameBuf[0]))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", errno)
	}
	slaveName := string(nameBuf[:clen(nameBuf[:])])

	slave, err = os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave %s: %w", slaveName, err)
	}

	return master, slave, nil
}

func clen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}
