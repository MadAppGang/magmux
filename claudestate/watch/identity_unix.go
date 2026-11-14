//go:build unix

package watch

import (
	"os"
	"syscall"
)

// identity is what a path names: its (dev, ino) and its file type. A path's
// identity is what decides whether a held target must be re-armed: a
// replace-publish leaves the path in place and changes the inode under it,
// and a watch on the old inode is deaf from that moment on.
//
// (dev, ino) alone is tt's vnode identity, and it is sound only while the
// watch pins the inode. On darwin it does: a kqueue watch is an open
// descriptor, so the number cannot be reused while it is held. Linux inotify
// holds no descriptor: removing the path drops the watch and frees the inode,
// and ext4 and tmpfs hand the number to the next create in that directory. So
// a file replaced by a directory, or a directory by a file, can report the
// (dev, ino) it replaced. The type is part of the identity because the
// replacement can never borrow that: it is whatever the new object is.
//
// What this does NOT catch: a replacement of the SAME type that reuses the
// number on Linux. (dev, ino, type) are then all equal; telling them apart
// needs a creation time or a generation number, which stat(2) does not report
// on Linux.
type identity struct {
	dev, ino uint64
	typ      os.FileMode
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
	return identity{dev: uint64(st.Dev), ino: uint64(st.Ino), typ: fi.Mode().Type(), ok: true}, fi, nil
}
