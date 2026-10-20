// Package unixsocktest builds the testdata directories that the socket tests in
// every package share: one place decides how deep a "deep" socket path is.
package unixsocktest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DeepPathLen is the shortest socket path the deep-path tests use. It is over
// sizeof(sun_path) on both darwin (104) and linux (108), so a socket at that
// path cannot be addressed by its absolute name.
const DeepPathLen = 120

// DeepDir returns a directory under t.TempDir(), nested so that
// filepath.Join(dir, name) is at least minLen bytes. Pass the SHORTEST name the
// caller can end up binding (for a pid socket, "magmux-1.sock"), so the real
// path is at least as long.
func DeepDir(t testing.TB, name string, minLen int) string {
	t.Helper()
	dir := t.TempDir()
	for len(filepath.Join(dir, name)) < minLen {
		dir = filepath.Join(dir, "deep-socket-dir-"+strings.Repeat("d", 16))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make deep dir %s: %v", dir, err)
	}
	if got := len(filepath.Join(dir, name)); got < minLen {
		t.Fatalf("deep dir %s gives a %d-byte path for %q, want >= %d", dir, got, name, minLen)
	}
	return dir
}
