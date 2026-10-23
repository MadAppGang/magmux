package watch

import (
	"os"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// Budget is what the watcher may spend (D15).
//
// Descriptors is the darwin claim (kqueue: a watched directory costs 1 + its
// current entry count, a file 1). Watches is the linux claim (inotify: every
// path costs one watch, counted against the per-user max_user_watches). A zero
// field makes no claim. Files, JobDirs and JobTranscripts are tt's ceilings
// (A3): Files bounds every armed transcript, interactive and job alike.
type Budget struct {
	Descriptors, Watches, Files, JobDirs, JobTranscripts int
}

// tt's ceilings (tt:adapters/watchkq/watchkq.go:22-31).
const (
	ceilingFiles          = 1024
	ceilingJobDirs        = 64
	ceilingJobTranscripts = 64
)

// budgetShare is A3's quarter: the watcher claims a quarter of the limit.
const budgetShare = 4

// RuntimeBudget computes the budget for this process, now. It is never a
// constant: machines differ by a factor of 4,000.
//
// darwin: Descriptors is a quarter of the SOFT RLIMIT_NOFILE, read
// in-process, after Go's own startup raise of the soft limit to the hard one.
// linux: Watches is a quarter of /proc/sys/fs/inotify/max_user_watches, and
// Descriptors a quarter of the soft RLIMIT_NOFILE.
func RuntimeBudget() (Budget, error) {
	b := Budget{Files: ceilingFiles, JobDirs: ceilingJobDirs, JobTranscripts: ceilingJobTranscripts}
	return runtimeBudget(b)
}

// targetCost is what holding t costs against the claim, counted now.
func targetCost(t model.WatchTarget) int {
	if !t.Dir || !costsEntries {
		return 1
	}
	entries, err := os.ReadDir(t.Path)
	if err != nil {
		return 1
	}
	return 1 + len(entries)
}

func clampInt(v uint64) int {
	const maxInt = int(^uint(0) >> 1)
	if v > uint64(maxInt) {
		return maxInt
	}
	return int(v)
}
