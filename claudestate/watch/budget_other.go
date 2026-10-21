//go:build !darwin && !linux

package watch

import "errors"

const costsEntries = false

// The library is built and tested on darwin and linux only (CI's matrix).
// Elsewhere there is no measured budget, and a guessed one would be the
// constant D15 rejects; a caller must pass Config.Budget explicitly.
func runtimeBudget(Budget) (Budget, error) {
	return Budget{}, errors.New("watch: no runtime budget on this OS; set Config.Budget")
}

func claimOf(b Budget) int { return b.Descriptors }

const (
	limitEMFILE = "EMFILE: the process's open-file limit is exhausted"
	limitENOSPC = "ENOSPC: the kernel refused another watch"
)
