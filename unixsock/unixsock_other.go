//go:build !darwin && !linux

package unixsock

import "path/filepath"

// inDir has no short alias to offer on this OS, so it passes the real path
// and the standard library reports the over-long address in its own words.
func inDir(dir, name string, fn func(addr string) error) error {
	return fn(filepath.Join(dir, name))
}
