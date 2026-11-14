//go:build darwin

package watch

import (
	"fmt"
	"syscall"
)

// costsEntries: fsnotify's kqueue backend opens a descriptor for every entry
// of a watched directory, including entries created after the arm.
const costsEntries = true

func runtimeBudget(b Budget) (Budget, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return Budget{}, fmt.Errorf("watch: reading RLIMIT_NOFILE: %w", err)
	}
	b.Descriptors = clampInt(lim.Cur) / budgetShare
	return b, nil
}

// claimOf is the claim Cost is held to on this OS.
func claimOf(b Budget) int { return b.Descriptors }

const (
	limitEMFILE = "EMFILE: the process's open-file limit (soft RLIMIT_NOFILE; `ulimit -n`, `launchctl limit maxfiles`) is exhausted"
	limitENOSPC = "ENOSPC: the kernel refused another watch"
)
