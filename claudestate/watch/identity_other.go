//go:build !unix

package watch

import "os"

// identity is unavailable without a unix stat: ok stays false, so every
// Verify re-arms, which is the safe direction.
type identity struct {
	dev, ino uint64
	ok       bool
}

func identityOf(path string) (identity, os.FileInfo, error) {
	fi, err := os.Stat(path)
	return identity{}, fi, err
}
