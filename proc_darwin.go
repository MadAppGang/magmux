//go:build darwin

package main

// Parent-process lookup, used by the MCP server's self-pane guard.
//
// An agent that drives the pane it is itself running in blocks forever: it
// waits for a turn to finish that cannot finish until the agent returns. The
// only way to tell "that pane is me" apart from "that pane is another agent"
// is ancestry — the pane's pid is one of our own ancestors — so every platform
// needs a ppid lookup.

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ppidOf returns the parent pid of pid. It returns an error when the process
// does not exist or is not visible to us, which the caller treats as "the
// chain ends here" rather than as a failure.
func ppidOf(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("ppidOf: invalid pid %d", pid)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("ppidOf %d: %w", pid, err)
	}
	if kp == nil {
		return 0, fmt.Errorf("ppidOf %d: no such process", pid)
	}
	return int(kp.Eproc.Ppid), nil
}
