package unixsock

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/unixsock/unixsocktest"
)

// addrLimit is sizeof(sun_path) on this OS: 104 on darwin, 108 on linux.
var addrLimit = len(syscall.RawSockaddrUnix{}.Path)

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unix socket addressing test requires darwin or linux")
	}
}

// deepSockPath returns a socket path of at least DeepPathLen bytes, and
// asserts it really is past the address limit.
func deepSockPath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(unixsocktest.DeepDir(t, name, unixsocktest.DeepPathLen), name)
	if len(path) < unixsocktest.DeepPathLen || len(path) < addrLimit {
		t.Fatalf("socket path is %d bytes, want >= %d and >= the %d-byte address limit: %s",
			len(path), unixsocktest.DeepPathLen, addrLimit, path)
	}
	return path
}

// servePong accepts one connection at a time and answers each line with "pong".
func servePong(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
			_, _ = c.Write([]byte("pong\n"))
		}(c)
	}
}

// roundTrip dials path, sends "ping" and returns the reply line.
func roundTrip(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, &net.Dialer{Timeout: 2 * time.Second}, path)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping\n")); err != nil {
		return "", err
	}
	return bufio.NewReader(c).ReadString('\n')
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
func TestListenAndDialPastSunPath(t *testing.T) {
	requireUnix(t)
	path := deepSockPath(t, "s.sock")

	// Positive control: the standard library refuses this path by its absolute
	// name. If it did not, the path is not past the limit and the rest of this
	// test would prove nothing.
	if ln, err := net.Listen("unix", path); err == nil {
		ln.Close()
		_ = os.Remove(path)
		t.Fatalf("net.Listen accepted a %d-byte path; the control is broken: %s", len(path), path)
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("unixsock.Listen(%d-byte path): %v", len(path), err)
	}
	defer ln.Close()
	defer os.Remove(path)
	go servePong(ln)

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("no socket file at the deep path %s: %v", path, err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("%s is %v, want a socket", path, fi.Mode())
	}

	got, err := roundTrip(path)
	if err != nil {
		t.Fatalf("unixsock.Dial(%d-byte path): %v", len(path), err)
	}
	if got != "pong\n" {
		t.Fatalf("reply = %q, want %q", got, "pong\n")
	}
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestPerThreadCwdDoesNotLeak guards against addressing a deep socket by
// changing the PROCESS working directory: other goroutines, and children forked
// from other threads, would see it. Every observation of the deep directory as
// a cwd is a leak.
func TestPerThreadCwdDoesNotLeak(t *testing.T) {
	requireUnix(t)
	const loops = 100
	const observers = 8

	path := deepSockPath(t, "s.sock")
	deep := filepath.Dir(path)
	realDeep, err := filepath.EvalSymlinks(deep)
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("unixsock-cwd-marker-%d", os.Getpid())
	if err := os.WriteFile(filepath.Join(deep, marker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the marker %s is reachable relatively before the test starts; the cwd check would be blind", marker)
	}
	startWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	var checks, forks, leaks atomic.Int64
	var leakMu sync.Mutex
	var leakSeen []string
	leak := func(what string) {
		leaks.Add(1)
		leakMu.Lock()
		if len(leakSeen) < 5 {
			leakSeen = append(leakSeen, what)
		}
		leakMu.Unlock()
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < observers; i++ {
		wg.Add(1)
		go func(fork bool) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if wd, err := os.Getwd(); err == nil && (wd == deep || wd == realDeep) {
					leak("os.Getwd = " + wd)
				}
				if _, err := os.Stat(marker); err == nil {
					leak("relative os.Stat found " + marker)
				}
				checks.Add(1)
				if fork {
					out, err := exec.Command("/bin/pwd", "-P").Output()
					if err == nil {
						forks.Add(1)
						if strings.TrimSpace(string(out)) == realDeep {
							leak("/bin/pwd -P = " + realDeep)
						}
					}
				}
			}
		}(i%2 == 0)
	}
	// Deferred as well as called below, so a t.Fatal in the loop cannot leave
	// an observer running past the test.
	halt := sync.OnceFunc(func() {
		close(stop)
		wg.Wait()
	})
	defer halt()

	for i := 0; i < loops; i++ {
		ln, err := Listen(path)
		if err != nil {
			t.Fatalf("loop %d: unixsock.Listen(%d-byte path): %v", i, len(path), err)
		}
		go servePong(ln)
		got, err := roundTrip(path)
		ln.Close()
		_ = os.Remove(path)
		if err != nil {
			t.Fatalf("loop %d: round trip: %v", i, err)
		}
		if got != "pong\n" {
			t.Fatalf("loop %d: reply = %q, want %q", i, got, "pong\n")
		}
	}
	halt()

	t.Logf("%d checks, %d forks, %d leaks across %d Listen/Dial loops", checks.Load(), forks.Load(), leaks.Load(), loops)
	if checks.Load() == 0 || forks.Load() == 0 {
		t.Fatalf("the observers did not run (checks=%d forks=%d); the test proved nothing", checks.Load(), forks.Load())
	}
	if n := leaks.Load(); n != 0 {
		t.Fatalf("the deep directory was visible as a cwd %d times; first: %v", n, leakSeen)
	}
	if wd, _ := os.Getwd(); wd != startWd {
		t.Fatalf("the process cwd changed from %s to %s", startWd, wd)
	}
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestShortPathTakesDirectRoute pins that a path under the limit is bound by
// its own name, exactly as net.Listen binds it, and that closing the listener
// still removes the file the way net.Listen's listener does.
func TestShortPathTakesDirectRoute(t *testing.T) {
	requireUnix(t)
	dir, err := os.MkdirTemp("", "us")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	if len(path) >= addrLimit {
		t.Skipf("TMPDIR is too long for a short-path test: %s is %d bytes", path, len(path))
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("unixsock.Listen(%s): %v", path, err)
	}
	if got := ln.Addr().String(); got != path {
		t.Errorf("Addr() = %q, want the path itself %q", got, path)
	}
	go servePong(ln)
	if got, err := roundTrip(path); err != nil || got != "pong\n" {
		t.Errorf("round trip = %q, %v; want pong", got, err)
	}
	ln.Close()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("closing a short-path listener left %s behind; net.Listen's listener removes it", path)
	}
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestListenerDoesNotUnlinkRelativeName: a deep socket is bound by a short
// alias, and a listener that unlinked that alias on Close would remove a file
// of the same name in the process's own cwd.
func TestListenerDoesNotUnlinkRelativeName(t *testing.T) {
	requireUnix(t)
	path := deepSockPath(t, "s.sock")

	t.Chdir(t.TempDir())
	bystander := filepath.Base(path)
	if err := os.WriteFile(bystander, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("unixsock.Listen(%d-byte path): %v", len(path), err)
	}
	defer os.Remove(path)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fi, err := os.Lstat(bystander)
	if err != nil {
		t.Fatalf("closing the deep listener removed %s from the process cwd: %v", bystander, err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s in the process cwd is now %v, want the regular file the test made", bystander, fi.Mode())
	}
}
