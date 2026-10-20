package unixsock

import (
	"os"
	"strconv"
	"syscall"
)

// inDir calls fn with /proc/self/fd/<fd>/<name>, where fd is dir opened for
// the duration of the call. The kernel resolves the magic link, so the address
// stays short (about 96 bytes for a 76-byte name) whatever the depth of dir,
// and no working directory is changed. Without /proc mounted the call fails
// with ENOENT, which the caller wraps with the real path.
func inDir(dir, name string, fn func(addr string) error) error {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer syscall.Close(fd)
	return fn("/proc/self/fd/" + strconv.Itoa(fd) + "/" + name)
}
