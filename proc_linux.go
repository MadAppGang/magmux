//go:build linux

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
	"os"
	"strconv"
	"strings"
)

// ppidOf returns the parent pid of pid, read from /proc/<pid>/stat.
//
// Field 4 is the ppid, but the fields cannot simply be split on spaces: field
// 2 is the executable name in parentheses and may itself contain spaces and
// parentheses (`(my prog (2))`). The only safe anchor is the LAST ')' in the
// line — everything after it is fixed-width, space-separated fields starting
// at field 3 (state).
func ppidOf(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("ppidOf: invalid pid %d", pid)
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, fmt.Errorf("ppidOf %d: %w", pid, err)
	}
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 || end+1 >= len(s) {
		return 0, fmt.Errorf("ppidOf %d: malformed stat", pid)
	}
	fields := strings.Fields(s[end+1:])
	// fields[0] is the state (field 3), fields[1] is the ppid (field 4).
	if len(fields) < 2 {
		return 0, fmt.Errorf("ppidOf %d: malformed stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("ppidOf %d: %w", pid, err)
	}
	return ppid, nil
}
