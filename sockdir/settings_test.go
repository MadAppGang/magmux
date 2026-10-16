package sockdir

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// privateDir makes a verified private settings directory under a fresh
// scratch dir.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(sockTestDir(t), fmt.Sprintf("magmux-settings-%d", os.Geteuid()))
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	return dir
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func settingsName(pid int, rand string) string {
	return fmt.Sprintf("magmux-%d-settings-%s.json", pid, rand)
}

// TestReapSettingsRemovesOrphan: a settings copy whose owner is a reaped
// `true` child is removed, and the count says so.
func TestReapSettingsRemovesOrphan(t *testing.T) {
	dir := privateDir(t)
	orphan := settingsName(deadPid(t), "123")
	touch(t, filepath.Join(dir, orphan))

	if n := ReapSettings(dir, 0); n != 1 {
		t.Errorf("ReapSettings removed %d, want 1", n)
	}
	if got := names(t, dir); len(got) != 0 {
		t.Errorf("left %q; the orphan's owner is dead", got)
	}
}

// TestReapSettingsKeepsLiveFile: our own pid's file and a live child's file
// both survive — the child is what proves the liveness gate, not merely a
// self check.
func TestReapSettingsKeepsLiveFile(t *testing.T) {
	dir := privateDir(t)
	self := settingsName(os.Getpid(), "1")
	child := settingsName(livePid(t), "2")
	touch(t, filepath.Join(dir, self))
	touch(t, filepath.Join(dir, child))

	if n := ReapSettings(dir, 0); n != 0 {
		t.Errorf("ReapSettings removed %d, want 0", n)
	}
	want := []string{self, child}
	sort.Strings(want)
	if got := names(t, dir); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("left %q, want %q", got, want)
	}
}

// TestReapSettingsIgnoresNonMatching: every survivor is saved by exactly one
// rule — the round-trip (a leading zero), the shape, the file type (a symlink
// and a directory under a reapable name).
func TestReapSettingsIgnoresNonMatching(t *testing.T) {
	dir := privateDir(t)
	dead := deadPid(t)
	target := filepath.Join(sockTestDir(t), "target.json")
	touch(t, target)

	keep := []string{
		fmt.Sprintf("magmux-00%d-settings-1.json", dead), // rule 2: does not round-trip
		fmt.Sprintf("magmux-%d-settings-x.json", dead),   // rule 1: not CreateTemp's digits
		fmt.Sprintf("magmux-%d-settings-1.json.bak", dead),
		fmt.Sprintf("magmux-%d.sock", dead),
		"magmux-0012-settings-1.json",
	}
	for _, n := range keep {
		touch(t, filepath.Join(dir, n))
	}
	link := settingsName(dead, "7") // rule 4: a symlink
	if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
		t.Fatal(err)
	}
	sub := settingsName(dead, "8") // rule 4: a directory
	if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, link, sub)
	sort.Strings(keep)

	if n := ReapSettings(dir, 0); n != 0 {
		t.Errorf("ReapSettings removed %d, want 0", n)
	}
	if got := names(t, dir); fmt.Sprint(got) != fmt.Sprint(keep) {
		t.Errorf("left %q, want %q", got, keep)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the symlink's target was touched: %v", err)
	}
}

// TestReapSettingsRefusesUnverifiedDir: a directory that is not private is not
// swept at all — not a 0755 one, and not a symlink to a private one.
func TestReapSettingsRefusesUnverifiedDir(t *testing.T) {
	dead := deadPid(t)

	open := filepath.Join(sockTestDir(t), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(open, settingsName(dead, "1")))
	if n := ReapSettings(open, 0); n != 0 {
		t.Errorf("swept a 0755 directory (%d removed)", n)
	}

	real := privateDir(t)
	touch(t, filepath.Join(real, settingsName(dead, "2")))
	link := filepath.Join(sockTestDir(t), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if n := ReapSettings(link, 0); n != 0 {
		t.Errorf("followed a symlinked directory (%d removed)", n)
	}
	if n := ReapSettings(filepath.Join(sockTestDir(t), "missing"), 0); n != 0 {
		t.Errorf("a missing directory swept %d", n)
	}
}

// TestEnsurePrivateDir: created exactly 0700 whatever the umask; an existing
// directory with a foreign mode is refused, never chmod'ed into shape; a
// symlink is refused.
func TestEnsurePrivateDir(t *testing.T) {
	base := sockTestDir(t)
	d := filepath.Join(base, "fresh")
	if err := EnsurePrivateDir(d); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if err := EnsurePrivateDir(d); err != nil {
		t.Fatalf("second call on our own dir: %v", err)
	}

	foreign := filepath.Join(base, "foreign")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(foreign, 0o755)
	if err := EnsurePrivateDir(foreign); err == nil {
		t.Error("a pre-existing 0755 directory was accepted")
	}
	if fi, _ := os.Stat(foreign); fi.Mode().Perm() != 0o755 {
		t.Errorf("a refused directory was chmod'ed to %#o", fi.Mode().Perm())
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(d, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(link); err == nil {
		t.Error("a symlink to a private directory was accepted")
	}
}

// TestRemoveOwnSettings: the exit sweep takes our own files and nobody
// else's, live or dead.
func TestRemoveOwnSettings(t *testing.T) {
	dir := privateDir(t)
	mine := settingsName(os.Getpid(), "1")
	other := settingsName(livePid(t), "2")
	orphan := settingsName(deadPid(t), "3")
	for _, n := range []string{mine, other, orphan} {
		touch(t, filepath.Join(dir, n))
	}
	if n := RemoveOwnSettings(dir); n != 1 {
		t.Errorf("RemoveOwnSettings removed %d, want 1", n)
	}
	want := []string{other, orphan}
	sort.Strings(want)
	if got := names(t, dir); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("left %q, want %q", got, want)
	}
}

// TestSettingsFilePatternRoundTrips: a name os.CreateTemp mints from the
// write-side pattern is a name the delete-side rules accept, for our pid.
func TestSettingsFilePatternRoundTrips(t *testing.T) {
	dir := privateDir(t)
	f, err := os.CreateTemp(dir, SettingsFilePattern(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	pid, ok := settingsOwner(filepath.Base(f.Name()))
	if !ok || pid != os.Getpid() {
		t.Errorf("CreateTemp minted %q, which settingsOwner reads as (%d, %v)", filepath.Base(f.Name()), pid, ok)
	}
}
