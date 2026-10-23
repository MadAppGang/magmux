//go:build unix

package watch

import (
	"os"
	"syscall"
)

// identity is the (dev, ino) a path names. A path's identity is what decides
// whether a held target must be re-armed: a replace-publish leaves the path in
// place and changes the inode under it, and a watch on the old inode is deaf
// from that moment on.
type identity struct {
	dev, ino uint64
	ok       bool
}

// identityOf stats path, following symlinks (a Tree's Dir is already
// resolved). It reads metadata only.
func identityOf(path string) (identity, os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return identity{}, nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return identity{}, fi, nil
	}
	return identity{dev: uint64(st.Dev), ino: uint64(st.Ino), ok: true}, fi, nil
}
