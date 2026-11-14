package watch

// A replacement that is handed the inode number its predecessor freed.
//
// On darwin a held watch is an open descriptor (kqueue), so the watched
// inode cannot be freed and its number cannot be reused while the watch
// exists: (dev, ino) is enough there. Linux inotify holds no descriptor.
// Removing the path drops the watch and frees the inode, and ext4 and tmpfs
// give the number to the very next create in that directory, so a file
// replaced by a directory (or the reverse) can report the (dev, ino) the
// watcher recorded. The darwin test host never reuses a number, so these
// tests make the watcher's stat report the held one, which is what Linux did
// on CI for the pipeline's verifygrant tests.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// reuseHeldInode makes w report, for path, the (dev, ino) it holds for path:
// the replacement took the freed number. Everything else is the real stat.
func reuseHeldInode(t *testing.T, w *Watcher, path string) {
	t.Helper()
	held := w.held[path].id
	if !held.ok {
		t.Skip("no unix identity on this platform: every Verify re-arms")
	}
	w.stat = func(p string) (identity, fs.FileInfo, error) {
		id, fi, err := identityOf(p)
		if p == path && err == nil && id.ok {
			id.dev, id.ino = held.dev, held.ino
		}
		return id, fi, err
	}
}

// The target is replaced by an object of the other type that reuses its inode
// number. Verify must see the change, and the re-arm must refuse the
// replacement because it is not what the target says it is.
func TestAReplacementOfTheOtherTypeThatReusesTheInodeIsSeen(t *testing.T) {
	cases := []struct {
		name    string
		target  func(string) model.WatchTarget
		create  func(t *testing.T, p string)
		replace func(t *testing.T, p string)
	}{
		{"a file replaced by a directory", transcript,
			func(t *testing.T, p string) { rewrite(t, p, "{}\n") },
			func(t *testing.T, p string) {
				if err := os.Mkdir(p, 0o700); err != nil {
					t.Fatal(err)
				}
			}},
		{"a directory replaced by a file", jobDir,
			func(t *testing.T, p string) { mkdir(t, p) },
			func(t *testing.T, p string) { rewrite(t, p, "x") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(tempDir(t), "target")
			c.create(t, p)
			w, _ := start(t, roomy)
			mustArm(t, w, c.target(p))

			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
			c.replace(t, p)
			reuseHeldInode(t, w, p)

			rep := w.Verify()
			if rep.Held != 0 || rep.Armed != 0 || len(rep.Failed) != 1 {
				t.Fatalf("Verify after a replacement that reused the inode number: %+v; "+
					"want it seen, re-armed and refused", rep)
			}
			if _, still := w.held[p]; still {
				t.Fatal("the replaced target is still held")
			}
		})
	}
}

// The comparison itself: one (dev, ino), two types, two identities.
func TestIdentitiesWithOneInodeNumberAndTwoTypesDiffer(t *testing.T) {
	dir := tempDir(t)
	f := filepath.Join(dir, "f")
	rewrite(t, f, "x")
	fileID, _, err := identityOf(f)
	if err != nil {
		t.Fatal(err)
	}
	dirID, _, err := identityOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fileID.ok || !dirID.ok {
		t.Skip("no unix identity on this platform")
	}
	dirID.dev, dirID.ino = fileID.dev, fileID.ino
	if fileID == dirID {
		t.Fatalf("a file and a directory with one (dev, ino) compare equal: %+v", fileID)
	}
}
