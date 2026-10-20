package sockdir

// Where magmux's IPC socket lives, and the rules for removing the ones a magmux
// that died badly left behind.
//
// Two separate concerns share this file because they share one fact — the shape
// of the name `magmux-<id>.sock` — and splitting them would leave that shape
// stated twice:
//
//   - the DIRECTORY (`Dir`, `--sock-dir` / `MAGMUX_SOCK_DIR`), which four
//     call sites in two other files read;
//   - the REAPER (`ReapStale`), whose entire safety argument rests on
//     the id half of that name being a pid it can interrogate.
//
// Nothing here holds a lock, and `ReapStale` deliberately has no
// *Magmux receiver: it does `os.ReadDir`, `syscall.Kill` and `os.Remove`, all
// blocking I/O, and CLAUDE.md's rule 1 ("never hold treeMu across blocking
// I/O") has no exceptions. A free function taking two strings cannot break it.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// sockDirDefault is the documented location and must stay byte-for-byte what
// it is: README (×8), Taskfile (×7), test/ui/harness.ts and twelve Go test
// sites build `/tmp/magmux-<pid>.sock` by formatting the string themselves. A
// changed default breaks all of them SILENTLY — the wrong directory is not an
// error, so the symptom is a dial timeout with no message. Overriding is
// opt-in for exactly that reason.
const sockDirDefault = "/tmp"

// Dir is where magmux binds and where `magmux mcp` looks. It is a var so an
// override reaches the consumers that do not have a *Magmux to ask —
// mcp_spawn.go's ReadDir and Join, and mcp_tools.go's Sprintf and TrimPrefix.
//
// It starts as the default and reads nothing at package init, so importing
// this package can neither print nor fail. Each entry point (mux.Main,
// mcp.Run) writes it once, from Resolve, before any goroutine exists. A test
// that wants a different directory sets the Magmux.sockDir field instead.
var Dir = sockDirDefault

// Resolve returns the socket directory for this process: flag (the --sock-dir
// value) when it is given, else MAGMUX_SOCK_DIR when it is set, else the
// default.
//
// A given flag is the whole answer: the env is not consulted, so a stale
// MAGMUX_SOCK_DIR cannot veto a correct flag. An unusable directory, from
// either source, is an ERROR naming the source, the directory and the reason.
// There is no fallback: a caller polling the directory it asked for must be
// told, not handed a socket somewhere else.
func Resolve(flag string) (string, error) {
	src, v := "--sock-dir", flag
	if v == "" {
		src, v = "MAGMUX_SOCK_DIR", os.Getenv("MAGMUX_SOCK_DIR")
	}
	if v == "" {
		return sockDirDefault, nil
	}
	dir, err := ValidDir(v)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", src, v, err)
	}
	return dir, nil
}

// ValidDir returns dir cleaned, or an error naming why it cannot hold the
// socket: it is empty, it cannot be stat'ed (the stat error is included), it
// is not a directory, or nobody can write it.
//
// Depth is no reason to refuse: unixsock binds and dials a socket whatever the
// length of its path.
//
// Writability is checked only NEGATIVELY, by mode: a directory nobody can write
// is certainly wrong, but "the owner bit is set" says nothing about us. The
// definitive answer is the bind, and a failed bind is fatal too.
func ValidDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("empty")
	}
	clean := filepath.Clean(dir)
	fi, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("cannot stat it: %w", err)
	}
	if !fi.IsDir() {
		return "", errors.New("not a directory")
	}
	if fi.Mode().Perm()&0o222 == 0 {
		return "", errors.New("not writable by anyone")
	}
	return clean, nil
}

// pidSockPattern gates DELETION and is deliberately NARROWER than
// mcp_spawn.go's sockNamePattern, which gates DISCOVERY. Discovery is
// permissive on purpose — report anything that looks like ours, including an
// --id name. Deletion cannot be, because the capture group IS the liveness
// oracle: if it is not a pid, there is nothing to ask.
var pidSockPattern = regexp.MustCompile(`^magmux-([0-9]+)\.sock$`)

// pidTokenPattern and pidTokenTmpPattern gate deletion of the REMOTE-CONTROL
// token file, under exactly the same rule and for exactly the same reason.
//
// A magmux that listens on --listen with no token source generates one and
// writes it to `magmux-<pid>.token`, beside the socket and under the same id.
// The normal exit removes it; a SIGKILL, a crash or a power loss does not, and
// nothing else in the system ever will. A stale token file is not merely
// litter: it is a live credential for a port that may later be taken by
// something else, so leaving it is worse than leaving a stale socket.
//
// The tmp pattern catches the other half: WriteFile creates
// `magmux-<id>.token.<pid>.tmp` and renames it, so a process killed between
// those two steps leaves a file whose OWN pid suffix is the liveness oracle.
// Note which pid each pattern interrogates — the socket's and the token's is
// the owning magmux, the tmp file's is the writer, and for a generated token
// they are the same process anyway.
var (
	pidTokenPattern    = regexp.MustCompile(`^magmux-([0-9]+)\.token$`)
	pidTokenTmpPattern = regexp.MustCompile(`^magmux-[A-Za-z0-9_-]+\.token\.([0-9]+)\.tmp$`)
)

const (
	// The sweep runs on the way to the bind, so it has to be bounded. Measured
	// against this machine's real /tmp (1394 entries, 729 magmux sockets, 712
	// of them reapable):
	//
	//	os.ReadDir            4.4ms   (reads and sorts the WHOLE directory)
	//	scan, with e.Type()   ~4ms    (of which every kill(pid,0): 1.2ms)
	//	os.Remove             78µs EACH
	//
	// Removal dominates, and it is per-file, so the STEADY STATE — a directory
	// with a handful of stale sockets in it — costs about 9ms and the deadline
	// is never reached. A directory with 1500 accumulated sockets in it does
	// reach it, and is then swept PARTIALLY, ~500 per start, until it is clean.
	// That is the intended behaviour and not a compromise: the deadline exists
	// so a pathological directory or a stalled filesystem cannot hold up a
	// bind, and 1500 stale sockets IS the pathological directory. Each start
	// finishes what the last one left.
	//
	// reapMaxEntries caps the SCAN, not the READ: os.ReadDir has already read
	// and sorted everything by the time the loop starts. The sorted order is
	// worth keeping in a function that deletes files — it makes the sweep, and
	// therefore its partial prefix, deterministic.
	ReapDeadline   = 50 * time.Millisecond
	reapMaxEntries = 8192
)

// pidIsGone reports whether pid is DEFINITELY not a running process.
//
// Only ESRCH is proof. nil means alive; EPERM means alive and owned by another
// user (the same reading mcp_spawn.go's discovery takes, and /tmp is shared
// between users on macOS); any other errno means the oracle does not know. All
// three answer "keep", because an oracle that does not know must say keep.
//
// errors.Is rather than == : Go wraps errno differently across call sites.
func pidIsGone(pid int) bool {
	if pid <= 0 {
		return false
	}
	return killErrMeansGone(syscall.Kill(pid, 0))
}

// killErrMeansGone is the errno half of pidIsGone, split out for ONE reason:
// the EPERM branch is load-bearing and cannot be reached by any fixture in this
// repo.
//
// Every pid a test can produce — the test binary, its children, a reaped
// `true` — belongs to the test's own uid, so Kill returns nil or ESRCH and EPERM
// never occurs. Mutate the rule to `ESRCH || EPERM` and the ENTIRE suite still
// passes, while on a shared /tmp (macOS, as the comment above notes) the reaper
// would unlink every OTHER user's LIVE magmux socket — the precise thing AC2.4
// forbids. Splitting the classification out makes that branch assertable
// without a setuid fixture; see TestKillErrMeansGone.
func killErrMeansGone(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

// ReapStale removes the files in dir that a dead magmux left behind — its
// socket, and the remote-control token it generated — and returns how many it
// removed. self is this magmux's own socket path.
//
// Both kinds are swept in ONE pass over one directory listing (see reapToken),
// because the listing is the expensive half and the removals are not.
//
// THE ORACLE IS THE PID, NOT A DIAL. An earlier design unlinked on
// ECONNREFUSED; that is measurably wrong on darwin, twice over:
//
//   - a LIVE AF_UNIX listener whose accept queue is full returns ECONNREFUSED
//     (measured, backlog=1, never accepting: dial 1 queued, dials 2-5 refused),
//     so a BUSY magmux would have its socket unlinked by another magmux's
//     reaper — triggered by load, and invisible on linux, which commonly
//     returns EAGAIN instead;
//   - between bind() and listen(), both INSIDE net.Listen, the file exists and
//     a dial is refused. Microseconds wide, and `go test` starts many magmuxes
//     at once, which is exactly the load that finds microsecond windows.
//
// A pid's liveness is immune to both, and to a loaded machine generally. The
// dial is therefore gone entirely — no timeouts, no parallelism, no net in the
// hot path.
//
// A name is ours to delete only if ALL of:
//
//  1. it matches pidSockPattern, yielding pid;
//  2. the name ROUND-TRIPS: fmt.Sprintf("magmux-%d.sock", pid) == name. One
//     line, and it removes the leading-zero and integer-overflow class at a
//     stroke — magmux-0001234.sock is not a name magmux ever mints, so it is
//     not ours. It also makes pid > 0 structural, which keeps us away from
//     Kill(0, …) and Kill(-pid, …), whose process-GROUP semantics reapPane's
//     own comment already flags as a live hazard in this codebase;
//  3. it is not our own socket;
//  4. it really is a socket inode (fs.ModeSocket), not a regular file that
//     happens to be named like one;
//  5. pidIsGone(pid), evaluated LAST and immediately before os.Remove.
//
// Rules 1-2 are the --id protection: a non-numeric name has no liveness oracle,
// and it does not accumulate anyway, because --id rebinds one fixed path that
// listenSocket already removes before binding.
//
// Rule 5 is deliberately ONE call. It used to be two identical adjacent calls
// with nothing between them, on the theory that a re-check "immediately before
// os.Remove" bought something; it does not. The second call subsumed the first
// entirely, and no test could tell the two apart because there is nothing
// either one can observe that the other cannot. What matters is only that the
// check is the last thing before the removal, which is what the ordering here
// states.
//
// Rule 5 narrows but cannot eliminate pid recycling: the OS could wrap the pid
// space, a new magmux take pid P, remove the stale file and bind a fresh one,
// all between our check and our os.Remove. A few instructions wide, and it
// requires the pid space to wrap onto exactly this pid. Adding checks does not
// close it — the removal is by PATHNAME, and Go exposes no unlink that verifies
// the inode it is unlinking, so some window always remains between the last
// observation and the syscall. The other direction — a recycled pid makes us
// KEEP a stale socket — is the safe failure, and the next run may clear it.
// Stated rather than claimed closed.
//
// The deadline is checked between entries and the function returns what it
// managed. A partial sweep is correct: the next magmux finishes it.
func ReapStale(dir, self string, deadline time.Duration) int {
	if deadline <= 0 {
		deadline = ReapDeadline
	}
	// The clock starts BEFORE os.ReadDir, so the deadline covers the read as
	// well as the scan. It used to start after, which made the bound a claim
	// about the cheap half only: os.ReadDir reads and sorts the WHOLE directory
	// and is the one call here with no cap on it, so a pathological or stalled
	// directory could spend arbitrarily long inside it and the deadline would
	// not have begun. Measured at 1.04ms for 2111 entries, so in the ordinary
	// case this changes nothing — but "bounded" has to mean bounded, and the
	// case that motivates the deadline at all is the one that is not ordinary.
	//
	// The bound is still not a hard ceiling and cannot be: the read is a single
	// syscall sequence that nothing can interrupt, and one os.Remove can run
	// past the deadline once the loop has already decided to make it. What is
	// guaranteed is that no NEW work starts after the deadline has passed.
	//
	// If a directory ever grows large enough for the READ itself to matter, the
	// replacement is os.Open + f.ReadDir(reapMaxEntries), which bounds the read
	// but returns directory order — so the sweep, and its partial prefix, would
	// stop being deterministic. Noted so that is a decision and not a
	// discovery.
	start := time.Now()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for i, e := range entries {
		if i >= reapMaxEntries || time.Since(start) > deadline {
			break
		}
		name := e.Name()
		// Cheapest conjuncts first. All five are ANDed, so the order is free
		// to choose, and this one keeps the per-entry lstat behind e.Info()
		// off every entry that is not already a candidate.
		m := pidSockPattern.FindStringSubmatch(name)
		if m == nil {
			// Not a socket of ours. It may still be a token file of ours, which
			// is swept under the same five rules with fs.ModeSocket replaced by
			// "a regular file". It shares this loop rather than getting its own
			// pass because os.ReadDir dominates the cost (4.4ms against 78µs per
			// removal) and a second listing would double the expensive half to
			// halve the cheap one.
			if reapToken(dir, name, e) {
				removed++
			}
			continue // rule 1
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || fmt.Sprintf("magmux-%d.sock", pid) != name {
			continue // rule 2
		}
		path := filepath.Join(dir, name)
		if path == self {
			// Belt-and-braces: the sweep runs BEFORE the bind, so it never
			// sees our own socket at all. A reaper whose safety depends on
			// correctly recognising itself is one refactor away from deleting
			// its own socket, so the check exists but is not load-bearing.
			continue // rule 3
		}
		// e.Type(), NOT e.Info().
		//
		// They carry the same bit — fs.FileMode.Type() is exactly the type bits
		// Info().Mode() would report, and ModeSocket is one of them — but on
		// darwin and linux Type() is served from the d_type that getdirentries
		// already returned, while Info() does a fresh lstat PER ENTRY.
		// Measured against this machine's real /tmp: 51.6ms of a 56ms scan was
		// e.Info(), against 1.2ms for every kill(pid,0) put together. Info()
		// only becomes free the other way round, when the filesystem reports
		// DT_UNKNOWN and os.ReadDir has already had to lstat — so Type() is
		// never worse and is 40x better here.
		if e.Type()&fs.ModeSocket == 0 {
			continue // rule 4
		}
		// Rule 5, and it is the LAST thing before the removal on purpose. See
		// the doc comment: one call, not two, and the residual window is
		// narrowed rather than closed.
		if !pidIsGone(pid) {
			continue
		}
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed
}

// reapToken removes one stale remote-control token file and reports whether it
// did.
//
// The rules mirror ReapStale's exactly, with one substitution: rule 4 asks for
// a REGULAR file rather than a socket, because that is what a token is. A
// directory or a symlink named `magmux-1234.token` is not ours and is left
// alone — os.Remove on a symlink would unlink the link, but a name magmux never
// mints is a name magmux does not delete.
//
// A --id token file (`magmux-web.token`) is deliberately unreachable here, for
// the reason ValidSocketID's comment gives: a non-numeric id has no liveness
// oracle. It does not accumulate either, because one --id rebinds one fixed
// path that the next start replaces.
func reapToken(dir, name string, e fs.DirEntry) bool {
	m := pidTokenPattern.FindStringSubmatch(name)
	if m == nil {
		m = pidTokenTmpPattern.FindStringSubmatch(name)
	}
	if m == nil {
		return false // rule 1
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 || strconv.Itoa(pid) != m[1] {
		// Rule 2, the round-trip: `magmux-0001234.token` is not a name magmux
		// ever writes, so it is not ours to remove.
		return false
	}
	if pid == os.Getpid() {
		// Rule 3. The sweep runs before this process writes its own token, so
		// this cannot fire today; it exists so a future caller that sweeps later
		// cannot delete the file it is about to serve.
		return false
	}
	if !e.Type().IsRegular() {
		return false // rule 4
	}
	if !pidIsGone(pid) {
		return false // rule 5, last before the removal
	}
	return os.Remove(filepath.Join(dir, name)) == nil
}

// ValidSocketID reports whether a --id NAME may be interpolated into the
// socket path. Restricted to [A-Za-z0-9_-]+ so a name can carry no path
// separator and no "..": the socket is created with this process's own
// privileges, and `--id ../../home/me/.ssh/agent` must not be able to reach
// out of /tmp and unlink something. The length cap bounds the FILE NAME:
// `magmux-` + 64 + `.sock` is 76 bytes, which fits the short address unixsock
// uses for a socket in a deep directory (the bare name on darwin,
// /proc/self/fd/N/<name> on linux), so any --sock-dir depth still binds.
//
// A purely NUMERIC name is rejected, and that is a deliberate, documented
// restriction on the flag's alphabet rather than an oversight. The startup
// reaper (sockdir.go) identifies "ours to delete" BY the name being a pid, and
// its only liveness oracle is treating that number as one. A live
// `magmux --id 1234` would therefore be reapable by any other magmux the
// moment pid 1234 is dead — almost always. Disambiguating instead by reading
// the pid's argv needs /proc (absent on darwin) or sysctl, is racy, and buys
// back an alphabet nothing in this repo uses.
func ValidSocketID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	allDigits := true
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '-':
			allDigits = false
		default:
			return false
		}
	}
	return !allDigits
}
