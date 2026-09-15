// Package pty opens a pseudo-terminal pair and sets its window size, with raw
// /dev/ptmx and ioctls and no cgo. The platform halves are pty_darwin.go and
// pty_linux.go.
//
// It is an implementation detail of magmux, with no API stability before v1.
package pty
