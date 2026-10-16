package main

import "golang.org/x/sys/unix"

const (
	ioctlGet      = unix.TCGETS
	ioctlSetFlush = unix.TCSETSF // drain output, DISCARD pending input, then set
)
