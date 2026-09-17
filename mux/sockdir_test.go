package mux

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// ── Phase 0: the byte-for-byte default ──────────────────────────────────────

// TestSocketPathDefaultIsUnchanged is the reason --sock-dir is opt-in.
//
// Twelve Go test sites, README ×8, Taskfile ×7 and test/ui/harness.ts all build
// "/tmp/magmux-<pid>.sock" by formatting the string themselves. A changed
// default breaks every one of them SILENTLY — the wrong directory is not an
// error, so the symptom is a dial timeout with no message. This test is the
// thing that fails loudly instead.
func TestSocketPathDefaultIsUnchanged(t *testing.T) {
	want := fmt.Sprintf("/tmp/magmux-%d.sock", os.Getpid())
	if got := (&Magmux{}).socketPath(); got != want {
		t.Fatalf("zero-value Magmux binds %q, want %q — the documented default moved", got, want)
	}
	// --id substitutes only the name half.
	want = "/tmp/magmux-mysession.sock"
	if got := (&Magmux{sockID: "mysession"}).socketPath(); got != want {
		t.Fatalf("--id path is %q, want %q", got, want)
	}
}

func TestSocketPathHonoursSockDirField(t *testing.T) {
	dir := t.TempDir()
	m := &Magmux{sockDir: dir}
	if got, want := m.socketPath(), filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", os.Getpid())); got != want {
		t.Fatalf("socketPath() = %q, want %q", got, want)
	}
	// A trailing slash is cleaned by filepath.Join rather than producing a
	// double separator.
	m = &Magmux{sockDir: dir + "/", sockID: "abc"}
	if got, want := m.socketPath(), filepath.Join(dir, "magmux-abc.sock"); got != want {
		t.Fatalf("socketPath() with a trailing slash = %q, want %q", got, want)
	}
}

// sockTestDir is a scratch directory short enough to bind a unix socket in.
//
// NOT t.TempDir(), and this is not a style choice. On darwin TMPDIR is ~80
// bytes and the test's own name is part of the path, so
// `t.TempDir() + "/magmux-<pid>.sock"` runs past sun_path and net.Listen fails
// with "bind: invalid argument" — measured here, at 111 bytes. That is exactly
// the failure sockdir.ValidDir's length check exists to catch, and it makes
// t.TempDir() unusable for any fixture that must create a real socket inode.
// It is also the reason the headless harness must not use t.TempDir() for
// --sock-dir.
func sockTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "magmux-reap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
