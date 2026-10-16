package main

import "golang.org/x/sys/unix"

const (
	ioctlGet      = unix.TIOCGETA
	ioctlSetFlush = unix.TIOCSETAF // drain output, DISCARD pending input, then set
)
