//go:build linux

package watch

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// costsEntries: inotify costs one watch per added path, a directory included,
// whatever it holds.
const costsEntries = false

// maxUserWatches is read-only, and outside the Claude root.
const maxUserWatches = "/proc/sys/fs/inotify/max_user_watches"

func runtimeBudget(b Budget) (Budget, error) {
	raw, err := os.ReadFile(maxUserWatches)
	if err != nil {
		return Budget{}, fmt.Errorf("watch: reading %s: %w", maxUserWatches, err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return Budget{}, fmt.Errorf("watch: %s: %w", maxUserWatches, err)
	}
	b.Watches = clampInt(n) / budgetShare

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return Budget{}, fmt.Errorf("watch: reading RLIMIT_NOFILE: %w", err)
	}
	b.Descriptors = clampInt(lim.Cur) / budgetShare
	return b, nil
}

// claimOf is the claim Cost is held to on this OS: watches, because inotify
// holds one descriptor per Watcher however many paths it watches.
func claimOf(b Budget) int { return b.Watches }

const (
	limitEMFILE = "EMFILE: the open-file limit (soft RLIMIT_NOFILE) or the inotify instance limit (/proc/sys/fs/inotify/max_user_instances) is exhausted"
	limitENOSPC = "ENOSPC: the per-user inotify watch limit (/proc/sys/fs/inotify/max_user_watches) is exhausted; it is shared with every process the user runs"
)
