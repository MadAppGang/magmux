package sockdir

import (
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/unixsock/unixsocktest"
)

// ── Phase 3: ValidDir and Resolve ───────────────────────────────────────────

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
func TestValidSockDir(t *testing.T) {
	good := t.TempDir()

	regular := filepath.Join(good, "afile")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	readOnly := filepath.Join(good, "readonly")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(readOnly, 0o700) })

	// A directory whose pid socket is past sun_path. unixsock binds at any
	// depth, so depth is no reason to refuse a directory.
	deep := unixsocktest.DeepDir(t, "magmux-1.sock", unixsocktest.DeepPathLen)

	cases := []struct {
		name string
		dir  string
		why  string // "" = accepted; else a substring the error must contain
	}{
		{"good", good, ""},
		{"tmp cleaned", "/tmp/", ""},
		{"deep", deep, ""},
		{"empty", "", "empty"},
		{"missing", filepath.Join(good, "nope"), "cannot stat"},
		{"regular file", regular, "not a directory"},
		{"not writable", readOnly, "not writable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidDir(c.dir)
			if c.why != "" {
				if err == nil {
					t.Fatalf("ValidDir(%q) = %q, nil; want an error containing %q", c.dir, got, c.why)
				}
				if !strings.Contains(err.Error(), c.why) {
					t.Errorf("ValidDir(%q) error %q does not name the reason %q", c.dir, err, c.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidDir(%q): %v; want it accepted", c.dir, err)
			}
			if got != filepath.Clean(c.dir) {
				t.Errorf("ValidDir returned %q, want the cleaned %q", got, filepath.Clean(c.dir))
			}
		})
	}
}

// REGRESSION: G-10 deep socket paths (>= sun_path) refused or silently moved to /tmp — Fixed in /dev:fix session dev-fix-20261017-g10-socket
//
// TestResolve pins the one resolver for --sock-dir and MAGMUX_SOCK_DIR: an
// unusable directory is an error naming its source and the directory, never a
// quiet move to /tmp.
func TestResolve(t *testing.T) {
	good := t.TempDir()
	other := t.TempDir()
	missing := filepath.Join(good, "does-not-exist")

	t.Run("unset is the default", func(t *testing.T) {
		t.Setenv("MAGMUX_SOCK_DIR", "")
		got, err := Resolve("")
		if err != nil || got != sockDirDefault {
			t.Errorf("Resolve(\"\") with no env = %q, %v; want %q, nil", got, err, sockDirDefault)
		}
	})
	t.Run("valid env", func(t *testing.T) {
		t.Setenv("MAGMUX_SOCK_DIR", good)
		got, err := Resolve("")
		if err != nil || got != good {
			t.Errorf("Resolve(\"\") with MAGMUX_SOCK_DIR=%q = %q, %v; want %q, nil", good, got, err, good)
		}
	})
	t.Run("invalid env is an error", func(t *testing.T) {
		t.Setenv("MAGMUX_SOCK_DIR", missing)
		got, err := Resolve("")
		if err == nil {
			t.Fatalf("Resolve(\"\") with MAGMUX_SOCK_DIR=%q = %q, nil; want an error, not a fallback", missing, got)
		}
		for _, want := range []string{"MAGMUX_SOCK_DIR", missing} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})
	t.Run("valid flag wins over an invalid env", func(t *testing.T) {
		t.Setenv("MAGMUX_SOCK_DIR", missing)
		got, err := Resolve(other)
		if err != nil || got != other {
			t.Errorf("Resolve(%q) with a bad MAGMUX_SOCK_DIR = %q, %v; want %q, nil — the env must not veto a correct flag",
				other, got, err, other)
		}
	})
	t.Run("invalid flag is an error", func(t *testing.T) {
		t.Setenv("MAGMUX_SOCK_DIR", "")
		got, err := Resolve(missing)
		if err == nil {
			t.Fatalf("Resolve(%q) = %q, nil; want an error, not a fallback", missing, got)
		}
		for _, want := range []string{"--sock-dir", missing} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})
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
// `t.TempDir() + "/magmux-<pid>.sock"` runs past sun_path — measured here, at
// 111 bytes — and these fixtures create their socket inodes with plain
// net.Listen, which refuses such a path with "bind: invalid argument". Deep
// paths are unixsock's business and are tested there.
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
// documented in ReapStale, and is not closeable by any number of checks
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

	n := ReapStale(dir, self, ReapDeadline)
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
// directory is not fatal — listenSocket calls this unconditionally, before the
// bind, on every start.
func TestReapStaleSocketsIgnoresMissingDir(t *testing.T) {
	if n := ReapStale(filepath.Join(t.TempDir(), "nope"), "", ReapDeadline); n != 0 {
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
	// The measurement is the BEST of several identical sweeps, not one sample.
	//
	// One sample measures the machine as much as the code. On one box,
	// unloaded, this sweep took 15ms; the same sweep, on the same box, beside
	// the rest of the suite under -race, took 255ms and failed a bound set from
	// the unloaded figure. Repeated runs of identical work spanned 34-241ms —
	// and all of that spread is scheduler preemption, because the work is
	// byte-identical every time. The minimum is the run that happened to get a
	// core to itself, so it is the closest this test can get to the cost of the
	// CODE; a per-entry cost model that has genuinely got worse moves the
	// minimum too, because it is work that has to be done on every run.
	//
	// Each iteration re-creates the socket, because the previous sweep removed
	// it, and each asserts the functional claim — that is the deterministic
	// half, and the one that matters: at 1501 entries the sweep still REACHES a
	// real socket and removes it. A backlog that grew past the deadline must
	// still shrink on every start, which is the contract the reaper is designed
	// around.
	const sweeps = 5
	best := time.Duration(math.MaxInt64)
	var samples []time.Duration
	for i := 0; i < sweeps; i++ {
		mkSocket(t, filepath.Join(dir, fmt.Sprintf("magmux-%d.sock", dead)))
		start := time.Now()
		n := ReapStale(dir, "", ReapDeadline)
		elapsed := time.Since(start)
		samples = append(samples, elapsed)
		if n != 1 {
			t.Errorf("sweep %d reaped %d, want 1 (only one entry is a real socket)", i+1, n)
		}
		best = min(best, elapsed)
	}

	// The bound is deliberately an order of magnitude above the deadline rather
	// than equal to it. A tight bound cannot detect what it would need to:
	// reintroducing DirEntry.Info() (an lstat per entry, the regression
	// e.Type() exists to avoid) costs ~12ms at this size, well inside the
	// noise. So the bound is set where it is still meaningful: a sweep that has
	// become quadratic, or that has started blocking on I/O per entry, blows
	// past a second and nothing else does. The deadline itself is enforced IN
	// the code and pinned by TestReapStaleSocketsRespectsDeadline; that is
	// where "the sweep is bounded" is actually proven, and it needs no wall
	// clock at all.
	const ceiling = 20 * ReapDeadline
	if best > ceiling {
		t.Errorf("the fastest of %d sweeps of 1501 entries took %v, over the %v ceiling — "+
			"every sweep was slow, so this is not load, it is a change in the per-entry "+
			"cost model (samples: %v)", sweeps, best, ceiling, samples)
	}
	t.Logf("swept 1501 entries; best %v of %v (deadline %v, failure ceiling %v)",
		best, samples, ReapDeadline, ceiling)
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

	// ReapStale treats <= 0 as "use the default", so the smallest
	// positive value is how an already-expired budget is expressed. The loop
	// checks the deadline BEFORE its first entry, so nothing may be removed.
	if got := ReapStale(dir, "", time.Nanosecond); got != 0 {
		t.Fatalf("removed %d socket(s) under a 1ns deadline; want 0 — the deadline is not being honoured", got)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed despite an expired deadline: %v", filepath.Base(p), err)
		}
	}

	// The same corpus, a real budget: every one must go. Without this, a reaper
	// that simply never removed anything would satisfy the assertions above.
	if got := ReapStale(dir, "", ReapDeadline); got != n {
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

// ── Phase 1: ValidSocketID refuses a purely numeric --id ────────────────────

func TestValidSocketIDRejectsPurelyNumeric(t *testing.T) {
	// Rejected because a purely numeric id is indistinguishable from the
	// pid-named default, and the reaper's ONLY oracle is treating that number
	// as a pid. A live `--id 1234` would otherwise be reapable by any other
	// magmux the moment pid 1234 is dead — almost always.
	for _, bad := range []string{"1234", "0", "00", "9999999999999999999999"} {
		if ValidSocketID(bad) {
			t.Errorf("ValidSocketID(%q) = true; an all-digit id is ambiguous with a pid socket", bad)
		}
	}
	for _, good := range []string{"v1234", "1234a", "abc", "test-1", "a", "1_2"} {
		if !ValidSocketID(good) {
			t.Errorf("ValidSocketID(%q) = false; only ALL-digit ids are ambiguous", good)
		}
	}
}
