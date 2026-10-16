package sockdir

// The private settings directory and the dead-owner sweep of the files in it.
//
// A Claude pane whose launch names a settings FILE gets a merged private copy
// of it (mux/claude_settings.go): the caller's keys plus editorMode:"normal".
// The copy cannot be inlined into the command line instead, because a settings
// file may carry an `env` block of secrets, and a command line is in every `ps`
// listing, in `results`, and in the Firebase mirror.
//
// The copy lives in `<sockdir>/magmux-settings-<euid>/`, which is PRIVATE: the
// sockdir itself may be the shared /tmp. The directory gets the token-file rule
// (see the Transport invariants in CLAUDE.md) applied to a directory — a real
// directory, not a symlink, owned by this euid, mode exactly 0700 — and is
// verified before every use, so a directory another user created first, or a
// symlink swapped in for it, is refused rather than written into or swept.
//
// The file is pid-named, `magmux-<pid>-settings-<rand>.json`, for the same
// reason the socket is: the pid is the liveness oracle the sweep interrogates.
// A magmux that dies by SIGKILL leaves the file behind, and nothing else in the
// system will ever remove it.

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

// pidSettingsPattern gates DELETION of a settings copy, under ReapStale's five
// rules. The `[0-9]+` tail is os.CreateTemp's random part, which is decimal.
var pidSettingsPattern = regexp.MustCompile(`^magmux-([0-9]+)-settings-[0-9]+\.json$`)

// SettingsFilePattern is the os.CreateTemp pattern for a settings copy owned
// by pid. It is the one place the name's shape is spelled for WRITING;
// pidSettingsPattern is the same shape for deletion, and the round-trip rule
// below ties the two together.
func SettingsFilePattern(pid int) string {
	return fmt.Sprintf("magmux-%d-settings-*.json", pid)
}

func settingsPrefix(pid int) string { return fmt.Sprintf("magmux-%d-settings-", pid) }

// errNotPrivate is every refusal of the private directory. The caller's only
// question is "may I write here", and every failure answers no.
var errNotPrivate = errors.New("settings directory is not private")

// VerifyPrivateDir checks that dir is a real directory (Lstat: a symlink is
// somebody else's directory), owned by this euid, with mode exactly 0700.
func VerifyPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", errNotPrivate, dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is not owned by euid %d", errNotPrivate, dir, os.Geteuid())
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("%w: %s has mode %#o, want 0700", errNotPrivate, dir, perm)
	}
	return nil
}

// EnsurePrivateDir creates dir at 0700 if it does not exist, then verifies it.
// An existing directory is NEVER chmod'ed into shape: a directory that is not
// already private was made by something else, and is refused. A directory this
// call created is chmod'ed once, because the umask applies to Mkdir and the
// rule is "exactly 0700", not "0700 minus whatever the umask took".
func EnsurePrivateDir(dir string) error {
	err := os.Mkdir(dir, 0o700)
	switch {
	case err == nil:
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrExist):
		return err
	}
	return VerifyPrivateDir(dir)
}

// ReapSettings removes the settings copies in dir whose owning magmux is
// provably dead, and returns how many it removed. dir is the PRIVATE directory
// (`<sockdir>/magmux-settings-<euid>`); a missing directory sweeps nothing, and
// one that fails VerifyPrivateDir is not swept at all — a swapped-in symlink is
// never followed.
//
// A name is ours to delete only if ALL of ReapStale's rules hold, with a
// regular file in place of a socket:
//
//  1. it matches pidSettingsPattern, yielding pid;
//  2. the pid round-trips: `magmux-<pid>-settings-` is a prefix of the name
//     (so `magmux-0012-settings-1.json` is not ours);
//  3. the pid is not ours;
//  4. it is a regular file (e.Type(): no symlink, no directory);
//  5. pidIsGone(pid), evaluated last, immediately before the removal.
//
// It shares ReapStale's deadline semantics: the clock starts before the read,
// and no new work starts after it has passed.
func ReapSettings(dir string, deadline time.Duration) int {
	if deadline <= 0 {
		deadline = ReapDeadline
	}
	start := time.Now()
	if VerifyPrivateDir(dir) != nil {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	self := os.Getpid()
	for i, e := range entries {
		if i >= reapMaxEntries || time.Since(start) > deadline {
			break
		}
		pid, ok := settingsOwner(e.Name())
		if !ok { // rules 1 and 2
			continue
		}
		if pid == self {
			continue // rule 3
		}
		if !e.Type().IsRegular() {
			continue // rule 4
		}
		if !pidIsGone(pid) {
			continue // rule 5, last before the removal
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// RemoveOwnSettings removes every settings copy in dir that THIS process owns
// — the exit path's sweep, for a pane whose child was never reaped. Same
// directory check and the same name rules as ReapSettings, with rule 3
// inverted and rule 5 dropped (we are, self-evidently, alive).
func RemoveOwnSettings(dir string) int {
	if VerifyPrivateDir(dir) != nil {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	self := os.Getpid()
	for _, e := range entries {
		pid, ok := settingsOwner(e.Name())
		if !ok || pid != self || !e.Type().IsRegular() {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// settingsOwner applies rules 1 and 2: the name's shape, and the pid's
// round-trip, which also makes pid > 0 structural.
func settingsOwner(name string) (int, bool) {
	m := pidSettingsPattern.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 || len(name) < len(settingsPrefix(pid)) || name[:len(settingsPrefix(pid))] != settingsPrefix(pid) {
		return 0, false
	}
	return pid, true
}
