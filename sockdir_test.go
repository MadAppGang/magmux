package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// ── Phase 3: validSockDir ───────────────────────────────────────────────────

// dirOfLength makes a directory whose absolute path is exactly n bytes.
//
// It lives under /tmp rather than t.TempDir() deliberately: on darwin TMPDIR is
// already ~80 bytes, which is over the sun_path budget before the fixture adds
// anything, so a temp-rooted fixture cannot express "fine for a pid, too long
// for a 64-char --id" — the exact case this check exists for.
func dirOfLength(t *testing.T, n int) string {
	t.Helper()
	name := fmt.Sprintf("magmux-sockdirtest-%d-", os.Getpid())
	dir := filepath.Join("/tmp", name)
	if len(dir) > n {
		t.Fatalf("cannot build a %d-byte directory: the fixture prefix is already %d", n, len(dir))
	}
	dir += strings.Repeat("d", n-len(dir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestValidSockDir(t *testing.T) {
	good := t.TempDir()

	regular := filepath.Join(good, "afile")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A directory that is fine for a 5-digit pid and too long for a 64-char
	// --id. This is the case rev 1's empty probe could not see, which is why
	// validSockDir takes the id that will REALLY be bound.
	longDir := dirOfLength(t, 40)
	id64 := strings.Repeat("i", 64)

	cases := []struct {
		name string
		dir  string
		id   string
		ok   bool
	}{
		{"good", good, "12345", true},
		{"good/no-id", good, "", true},
		{"empty", "", "12345", false},
		{"missing", filepath.Join(good, "nope"), "12345", false},
		{"regular file", regular, "12345", false},
		{"tmp cleaned", "/tmp/", "12345", true},
		{"long dir, short id", longDir, "12345", true},
		{"long dir, 64-char id", longDir, id64, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, why := validSockDir(c.dir, c.id)
			if ok != c.ok {
				t.Fatalf("validSockDir(%q, %q) ok=%v why=%q, want ok=%v", c.dir, c.id, ok, why, c.ok)
			}
			if !ok {
				if why == "" {
					t.Errorf("a refusal must say why")
				}
				if got != sockDirDefault {
					t.Errorf("a refusal must fall back to %q, got %q", sockDirDefault, got)
				}
				return
			}
			if got != filepath.Clean(c.dir) {
				t.Errorf("validSockDir returned %q, want the cleaned %q", got, filepath.Clean(c.dir))
			}
		})
	}
}

// TestValidSockDirLengthIsCheckedAgainstTheRealPath states the property the
// table above encodes, so a future edit that reintroduces the empty probe fails
// with a message that explains itself.
func TestValidSockDirLengthIsCheckedAgainstTheRealPath(t *testing.T) {
	dir := dirOfLength(t, 40)
	if _, ok, _ := validSockDir(dir, ""); !ok {
		t.Fatalf("with no id the length check must not fire (dir is %d bytes)", len(dir))
	}
	if _, ok, why := validSockDir(dir, strings.Repeat("i", 64)); ok {
		t.Fatalf("a 64-char --id under a %d-byte directory overruns sun_path and must be refused (why=%q)", len(dir), why)
	}
}

func TestResolveSockDirEnv(t *testing.T) {
	t.Setenv("MAGMUX_SOCK_DIR", "")
	if got := resolveSockDirEnv(); got != sockDirDefault {
		t.Errorf("unset MAGMUX_SOCK_DIR = %q, want %q", got, sockDirDefault)
	}
	dir := t.TempDir()
	t.Setenv("MAGMUX_SOCK_DIR", dir)
	if got := resolveSockDirEnv(); got != dir {
		t.Errorf("MAGMUX_SOCK_DIR=%q resolved to %q", dir, got)
	}
	// An invalid value degrades SILENTLY to the default: `magmux mcp` speaks
	// JSON-RPC on stdout and one stray byte desynchronises the client.
	t.Setenv("MAGMUX_SOCK_DIR", filepath.Join(dir, "does-not-exist"))
	if got := resolveSockDirEnv(); got != sockDirDefault {
		t.Errorf("an invalid MAGMUX_SOCK_DIR = %q, want a silent fallback to %q", got, sockDirDefault)
	}
}

// ── Phase 1: the reaper ─────────────────────────────────────────────────────

// deadPid returns a pid that is certainly not running: a child that has been
// run to completion and reaped, so the process table entry is gone and
// kill(pid, 0) is ESRCH.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	pid := cmd.Process.Pid
	if !pidIsGone(pid) {
		t.Skipf("pid %d is somehow still alive right after being reaped", pid)
	}
	return pid
}

// sockTestDir is a scratch directory short enough to bind a unix socket in.
//
// NOT t.TempDir(), and this is not a style choice. On darwin TMPDIR is ~80
// bytes and the test's own name is part of the path, so
// `t.TempDir() + "/magmux-<pid>.sock"` runs past sun_path and net.Listen fails
// with "bind: invalid argument" — measured here, at 111 bytes. That is exactly
// the failure validSockDir's length check exists to catch, and it makes
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

// mkSocket creates a REAL socket inode with no listener behind it.
//
// os.Create would make a regular file, which rule 4 excludes — so a broken
// sweep that never removed anything would still look correct against it. The
// SetUnlinkOnClose(false) dance is the only way to leave the inode behind.
func mkSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket inode did not survive close: %v", err)
	}
}

// livePid starts a real child that will outlive the test body and returns its
// pid, so the liveness gate is exercised against a process that is NOT this
// one. os.Getpid() alone is a weak fixture: a "gate" that special-cased self,
// or that answered "alive" for any pid it had already seen, would satisfy it.
func livePid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if pidIsGone(cmd.Process.Pid) {
		t.Skipf("pid %d died immediately after Start", cmd.Process.Pid)
	}
	return cmd.Process.Pid
}

// TestReapStaleSockets is the rule-by-rule table, and it is written so that
// each survivor is saved by exactly ONE rule — a sweep with any single rule
// missing removes a file that is named in the failure message.
//
// WHAT IT PROVES about the liveness gate specifically: with rule 5 deleted,
// BOTH `livePath` (this process) and `childPath` (a real, unrelated, running
// child) are removed, and the count goes from 1 to 3. The child matters: it is
// the difference between "the gate works" and "the gate happens to recognise
// self". The final whole-directory comparison catches the reverse failure too —
// a sweep that removes something no rule permits is reported by NAME rather
// than as an off-by-one in a count.
//
// WHAT IT DOES NOT PROVE: nothing here says anything about the pid-recycling
// window between the last pidIsGone and os.Remove. That window is real, is
// documented in reapStaleSockets, and is not closeable by any number of checks
// — removal is by pathname and Go has no inode-verifying unlink — so there is
// no behaviour for a test to pin. Nor does it prove the ORDER of the rules;
// only that every one of them is applied.
func TestReapStaleSockets(t *testing.T) {
	dir := sockTestDir(t)
	dead := deadPid(t)
	live := os.Getpid()

	stale := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", dead))
	mkSocket(t, stale)

	livePath := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", live))
	mkSocket(t, livePath)

	// And a socket owned by a live process that is not us at all. Rule 5.
	childPath := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", livePid(t)))
	mkSocket(t, childPath)

	// "self" is a socket for a dead pid, so ONLY rule 3 can save it.
	dead2 := deadPid(t)
	self := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", dead2))
	mkSocket(t, self)

	// A regular file named exactly like a stale socket. Rule 4.
	dead3 := deadPid(t)
	regular := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", dead3))
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// A real socket with a non-numeric (--id) name. Rule 1 — no oracle exists
	// for it, so it must never be touched.
	named := filepath.Join(dir, "magmux-alpha.sock")
	mkSocket(t, named)

	// Not ours at all.
	other := filepath.Join(dir, "notmagmux.sock")
	mkSocket(t, other)

	// Leading zeros: not a name magmux ever mints, so not ours to delete even
	// though the pid it names is dead. Rule 2's round-trip.
	dead4 := deadPid(t)
	zeros := filepath.Join(dir, fmt.Sprintf("magmux-0%d.sock", dead4))
	mkSocket(t, zeros)

	n := reapStaleSockets(dir, self, reapDeadline)
	if n != 1 {
		t.Errorf("reaped %d sockets, want exactly 1", n)
	}

	if _, err := os.Stat(stale); err == nil {
		t.Errorf("%s: a socket owned by a dead pid must be reaped", stale)
	}
	survivors := []struct{ path, why string }{
		{livePath, "the pid is alive — it is ours (rule 5)"},
		{childPath, "the pid is alive — a running child, not us (rule 5)"},
		{self, "it is our own socket (rule 3)"},
		{regular, "it is a regular file, not a socket (rule 4)"},
		{named, "a non-numeric --id name has no liveness oracle (rule 1)"},
		{other, "it is not a magmux name"},
		{zeros, "a leading-zero name is not one magmux mints (rule 2's round-trip)"},
	}
	for _, keep := range survivors {
		if _, err := os.Stat(keep.path); err != nil {
			t.Errorf("%s was reaped but must survive: %s", keep.path, keep.why)
		}
	}

	// Whole-directory comparison, so an over-eager sweep is named rather than
	// merely counted, and so a file nobody thought to list cannot vanish
	// unnoticed.
	want := map[string]string{}
	for _, keep := range survivors {
		want[filepath.Base(keep.path)] = keep.why
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for name, why := range want {
		if !got[name] {
			t.Errorf("%s is gone; it must survive: %s", name, why)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s survived and no rule permits it", name)
		}
	}
}

// TestReapStaleSocketsIgnoresMissingDir pins that an unreadable or absent
// directory is not fatal — socketServer calls this unconditionally, before the
// bind, on every start.
func TestReapStaleSocketsIgnoresMissingDir(t *testing.T) {
	if n := reapStaleSockets(filepath.Join(t.TempDir(), "nope"), "", reapDeadline); n != 0 {
		t.Fatalf("reaped %d from a directory that does not exist", n)
	}
}

// TestReapStaleSocketsAtScale is the cost claim, checked rather than asserted
// from a design doc: the whole point of running the sweep SYNCHRONOUSLY before
// net.Listen is that it is cheap enough to.
func TestReapStaleSocketsAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 1500 socket inodes")
	}
	dir := sockTestDir(t)
	dead := deadPid(t)
	// One real socket the sweep must remove, plus 1500 plain files that make
	// the directory the size of this machine's real /tmp. The files exercise
	// the scan (pattern, Atoi, round-trip, Info) without needing 1500 inodes
	// worth of listen()/close(). The socket itself is created inside the loop
	// below, because each sweep removes it.
	for i := 0; i < 1500; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", 900000+i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkSocket(t, filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", dead)))

	start := time.Now()
	n := reapStaleSockets(dir, "", reapDeadline)
	elapsed := time.Since(start)

	// The functional assertion, and the one that is deterministic: at 1501
	// entries the sweep still REACHES a real socket and removes it. A backlog
	// that grew past the deadline must still shrink on every start, which is
	// the contract the reaper is designed around.
	if n != 1 {
		t.Errorf("reaped %d, want 1 (only one entry is a real socket)", n)
	}

	// The timing bound is deliberately an order of magnitude above the deadline
	// rather than equal to it, and that is a correction to what this test used
	// to assert.
	//
	// `elapsed <= reapDeadline` reads like a tight cost claim and is in fact a
	// measurement of the machine: on one box, unloaded, this sweep took 15ms;
	// the same sweep, on the same box, beside the rest of the suite under -race,
	// took 255ms and failed. Repeated runs of identical work spanned 34-241ms.
	// Nor can a tight bound detect what it would need to: reintroducing
	// DirEntry.Info() (an lstat per entry, the regression e.Type() exists to
	// avoid) costs ~12ms at this size — well inside that noise.
	//
	// So the bound is set where it is still meaningful: a sweep that has become
	// quadratic, or that has started blocking on I/O per entry, blows past a
	// second and nothing else does. The deadline itself is enforced IN the code
	// and pinned by TestReapStaleSocketsRespectsDeadline; that is where "the
	// sweep is bounded" is actually proven, and it needs no wall clock at all.
	const ceiling = 20 * reapDeadline
	if elapsed > ceiling {
		t.Errorf("sweep of 1501 entries took %v, over the %v ceiling — this is not load, "+
			"it is a change in the per-entry cost model", elapsed, ceiling)
	}
	t.Logf("swept 1501 entries in %v (deadline %v, failure ceiling %v)", elapsed, reapDeadline, ceiling)
}

// TestReapStaleSocketsRespectsDeadline pins that the deadline actually stops the
// sweep.
//
// PROVES: an already-expired deadline removes NOTHING from a corpus that is
// entirely removable, and that same corpus IS swept completely when the budget
// is real. Delete the `time.Since(start) > deadline` check and the first
// assertion fails.
//
// DOES NOT PROVE: any wall-clock bound. That is what the previous version
// asserted, and it was vacuous twice over — it built its corpus from 200
// REGULAR FILES, which rule 4 skips, so nothing was ever removable and the test
// passed identically against a reaper with no deadline check at all. The pair of
// runs below is what makes the deadline observable: a reaper that ignores it
// fails the first, and a reaper that removes nothing fails the second.
func TestReapStaleSocketsRespectsDeadline(t *testing.T) {
	const n = 40
	dir := sockTestDir(t)

	// A pid above any plausible PID_MAX (darwin 99998; linux 32768 by default)
	// cannot name a live process, so every socket below is genuinely reapable.
	// Verified rather than assumed, because the whole fixture rests on it —
	// deadPid() would spawn n real processes to say the same thing.
	const base = 9_000_000
	if !pidIsGone(base) {
		t.Skipf("pid %d is not reported gone; this platform can host it", base)
	}

	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", base+i))
		mkSocket(t, p)
		paths = append(paths, p)
	}

	// reapStaleSockets treats <= 0 as "use the default", so the smallest
	// positive value is how an already-expired budget is expressed. The loop
	// checks the deadline BEFORE its first entry, so nothing may be removed.
	if got := reapStaleSockets(dir, "", time.Nanosecond); got != 0 {
		t.Fatalf("removed %d socket(s) under a 1ns deadline; want 0 — the deadline is not being honoured", got)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed despite an expired deadline: %v", filepath.Base(p), err)
		}
	}

	// The same corpus, a real budget: every one must go. Without this, a reaper
	// that simply never removed anything would satisfy the assertions above.
	if got := reapStaleSockets(dir, "", reapDeadline); got != n {
		t.Fatalf("removed %d of %d socket(s) under a real deadline", got, n)
	}
}

// ── the EPERM branch, which no fixture in this repo can reach ────────────────

// TestKillErrMeansGone pins the errno classification that decides whether a
// socket is deleted.
//
// PROVES: only ESRCH is treated as proof of death. In particular EPERM — a live
// process owned by ANOTHER USER, which is routine on the shared /tmp this reaper
// sweeps — must answer "keep".
//
// WHY IT EXISTS: every pid the rest of the suite can produce belongs to the
// test's own uid, so Kill returns nil or ESRCH and EPERM never occurs. The
// mutation `errors.Is(err, ESRCH) || errors.Is(err, EPERM)` therefore passes
// TestReapStaleSockets and every other test in this package, while in production
// it unlinks every other user's LIVE magmux socket. This is the only test that
// fails against it.
func TestKillErrMeansGone(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		gone bool
	}{
		{"nil means the process is alive and ours", nil, false},
		{"ESRCH is the only proof of death", syscall.ESRCH, true},
		{"EPERM means ALIVE and owned by another user", syscall.EPERM, false},
		{"EINVAL means the oracle does not know", syscall.EINVAL, false},
		{"an unrelated error means the oracle does not know", os.ErrClosed, false},
		{"a WRAPPED ESRCH still counts — errors.Is, not ==", fmt.Errorf("kill: %w", syscall.ESRCH), true},
		{"a WRAPPED EPERM still keeps", fmt.Errorf("kill: %w", syscall.EPERM), false},
	} {
		if got := killErrMeansGone(c.err); got != c.gone {
			t.Errorf("killErrMeansGone(%v) = %v, want %v — %s", c.err, got, c.gone, c.name)
		}
	}
}

// TestPidIsGoneRejectsNonPositive pins the guard that keeps Kill(0, …) and
// Kill(-pid, …) — whose process-GROUP semantics reapPane's own comment flags as
// a live hazard in this codebase — unreachable from the reaper.
func TestPidIsGoneRejectsNonPositive(t *testing.T) {
	for _, pid := range []int{0, -1, -os.Getpid()} {
		if pidIsGone(pid) {
			t.Errorf("pidIsGone(%d) = true; a non-positive pid must never be reapable", pid)
		}
	}
}

// ── Phase 1: validSocketID refuses a purely numeric --id ────────────────────

func TestValidSocketIDRejectsPurelyNumeric(t *testing.T) {
	// Rejected because a purely numeric id is indistinguishable from the
	// pid-named default, and the reaper's ONLY oracle is treating that number
	// as a pid. A live `--id 1234` would otherwise be reapable by any other
	// magmux the moment pid 1234 is dead — almost always.
	for _, bad := range []string{"1234", "0", "00", "9999999999999999999999"} {
		if validSocketID(bad) {
			t.Errorf("validSocketID(%q) = true; an all-digit id is ambiguous with a pid socket", bad)
		}
	}
	for _, good := range []string{"v1234", "1234a", "abc", "test-1", "a", "1_2"} {
		if !validSocketID(good) {
			t.Errorf("validSocketID(%q) = false; only ALL-digit ids are ambiguous", good)
		}
	}
}
