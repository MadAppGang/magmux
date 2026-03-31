//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (master *os.File, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	fd := int(master.Fd())

	// unlockpt — Linux uses TIOCSPTLCK ioctl with value 0
	val := 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.TIOCGPTN, uintptr(unsafe.Pointer(&val))); errno != 0 {
		// ignore — just need the ptn number below
	}

	// Get pty number
	var ptn uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptn))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", errno)
	}

	// Unlock
	unlock := 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", errno)
	}

	slaveName := "/dev/pts/" + strconv.FormatUint(uint64(ptn), 10)
	slave, err = os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave %s: %w", slaveName, err)
	}

	return master, slave, nil
}
