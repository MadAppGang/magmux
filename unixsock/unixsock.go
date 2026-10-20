// Package unixsock binds and dials unix stream sockets at any path length, for
// magmux and for any program that imports magmux's client.
//
// The kernel takes a unix socket address in sockaddr_un.sun_path, which is 104
// bytes on darwin and 108 on linux, and Go refuses a longer path with EINVAL
// before it reaches the kernel. The limit applies to the ADDRESS, not to where
// the file lives, so a socket in a deep directory is reachable through a short
// name that resolves to it:
//
//   - darwin: the base name, resolved against a per-THREAD working directory
//     (__pthread_fchdir) on a thread that is used for that one call and then
//     discarded. No other goroutine, thread or forked child ever sees it.
//   - linux: /proc/self/fd/<dirfd>/<base name>, with no working directory
//     touched at all.
//
// A path shorter than sun_path takes the direct call, exactly as net.Listen and
// net.Dialer would make it.
//
// It is a leaf: it imports only the standard library, and it holds no policy
// about WHERE a socket lives. That belongs to sockdir.
package unixsock

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"syscall"
)

// addrMax is sizeof(sun_path) on this OS: 104 on darwin, 108 on linux. A path
// of this length or longer cannot be passed to the kernel by its own name.
var addrMax = len(syscall.RawSockaddrUnix{}.Path)

// Listen binds a unix stream socket at path, whatever its length.
//
// The caller removes the socket file by its real path. On a path shorter than
// sun_path the listener is net.Listen's, and Close unlinks the file as it
// always has. On a longer path Close does NOT unlink: the listener knows the
// socket only by the short alias it was bound through, and that alias names a
// different file (or none) once the bind is over. For the same reason Addr()
// on a long path reports the alias, not path.
func Listen(path string) (*net.UnixListener, error) {
	var ln net.Listener
	long, err := at(path, func(addr string) (err error) {
		ln, err = net.Listen("unix", addr)
		return err
	})
	if err != nil {
		return nil, err
	}
	ul := ln.(*net.UnixListener)
	if long {
		ul.SetUnlinkOnClose(false)
	}
	return ul, nil
}

// Dial connects to the unix stream socket at path, whatever its length,
// honouring d's timeouts and ctx. On a path past sun_path the connection's
// RemoteAddr() is the short alias it was dialled through, not path.
func Dial(ctx context.Context, d *net.Dialer, path string) (net.Conn, error) {
	var c net.Conn
	_, err := at(path, func(addr string) (err error) {
		c, err = d.DialContext(ctx, "unix", addr)
		return err
	})
	return c, err
}

// at calls fn with an address that reaches path: path itself when it fits in
// sun_path, else a short alias built by the OS's inDir. It reports whether the
// alias was used. Errors from the alias route are wrapped with %w, so
// errors.Is(err, syscall.ECONNREFUSED) and its kin still see the errno.
func at(path string, fn func(addr string) error) (bool, error) {
	if len(path) < addrMax {
		return false, fn(path)
	}
	dir, name := filepath.Split(path)
	if len(name) >= addrMax {
		return true, fmt.Errorf("unix socket %s: file name is %d bytes, over the %d-byte address limit", path, len(name), addrMax)
	}
	if err := inDir(filepath.Clean(dir), name, fn); err != nil {
		return true, fmt.Errorf("unix socket %s: %w", path, err)
	}
	return true, nil
}
