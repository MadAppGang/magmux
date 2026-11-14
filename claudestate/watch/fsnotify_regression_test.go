package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// G11 (architecture §6). Moved from internal/obs/fswatch with the watcher.
//
// TestFsnotifySeesInPlaceWrites is the load-bearing regression test for this
// whole subsystem's one third-party dependency.
//
// # What it pins
//
// Watch a DIRECTORY. Rewrite a file already inside it, in place, with
// O_WRONLY|O_TRUNC. A WRITE event must arrive.
//
// # Why that is not obvious
//
// On Darwin the kernel primitive is kqueue, and a kqueue watch on a directory
// vnode does NOT fire when a child file's contents change. It fires when the
// directory itself changes — a name added, removed or renamed. By the kernel's
// rules, the rewrite this test performs is invisible.
//
// fsnotify compensates. Its kqueue backend opens a descriptor on every entry
// in a watched directory, and re-scans on directory change so new files are
// covered too. Its own comment says it does this "to mimic inotify". That is a
// library behaviour, not a platform guarantee, and nothing in fsnotify's
// documented API promises it.
//
// This matters because the records being watched are rewritten IN PLACE rather
// than replaced by a rename — measured three independent ways, including the
// writing tool's own "in place" publish discipline and a captured before/after
// pair with three rewrites, three in place, none renamed. So the only event
// this subsystem cares about is precisely the event the kernel does not
// deliver.
//
// # What happens if it is ever wrong
//
// The watcher goes SILENT. Not slow, not noisy — silent, with no error
// anywhere, and every session's state frozen at whatever it was when magmux
// started. That is indistinguishable from a quiet machine, which is the worst
// shape a failure can take: the feature reports nothing and looks correct
// doing it.
//
// So this test exists to fail loudly on the day a future fsnotify stops adding
// those per-entry watches. It is the entire defence against that version
// coupling, and it must not be weakened into "some event arrived": an event
// naming the DIRECTORY is exactly what a raw kqueue would give, and accepting
// it would make the test pass in the one case it is written to catch.
func TestFsnotifySeesInPlaceWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")

	// The file must exist BEFORE the watch is added. That is the whole point:
	// the compensating behaviour is a descriptor opened on each entry found at
	// Add time, and a file created afterwards would prove something else.
	if err := os.WriteFile(path, []byte(`{"status":"before"}`), 0o600); err != nil {
		t.Fatalf("writing the record: %v", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// The DIRECTORY, not the file. Watching each file individually is the
	// design this replaces; if the directory watch does not work, that design
	// comes back.
	if err := w.Add(dir); err != nil {
		t.Fatalf("Add(%s): %v", dir, err)
	}

	// Rewrite in place, which is the exact shape the real writer uses: open
	// the existing file for writing, truncate it, write the new content. No
	// temporary file, no rename, so the directory entry never changes.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("reopening the record: %v", err)
	}
	if _, err := f.WriteString(`{"status":"after"}`); err != nil {
		f.Close()
		t.Fatalf("rewriting the record: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the record: %v", err)
	}

	deadline := time.After(2 * time.Second)
	var seen []string
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				t.Fatalf("the event channel closed before a write arrived; saw %v", seen)
			}
			seen = append(seen, ev.String())
			if ev.Has(fsnotify.Write) && filepath.Base(ev.Name) == filepath.Base(path) {
				return
			}
		case err, ok := <-w.Errors:
			if !ok {
				t.Fatalf("the error channel closed before a write arrived; saw %v", seen)
			}
			t.Fatalf("watcher error: %v; saw %v", err, seen)
		case <-deadline:
			t.Fatalf("no WRITE event naming %s within 2s.\n"+
				"Events seen: %v\n"+
				"fsnotify %s no longer watches the individual entries of a watched "+
				"directory, so an in-place rewrite is now invisible on this platform. "+
				"The watcher this subsystem is built on would be silent, with no error. "+
				"Do not relax this test: either pin the previous fsnotify, or watch "+
				"each record file directly.",
				filepath.Base(path), seen, fsnotifyVersion)
		}
	}
}

// fsnotifyVersion is the version this test was measured against. It names the
// version in the failure message above, where that is the difference between a
// diagnosable failure and a puzzle, and internal/audit's dependency pin (G10)
// reads it from this file and requires go.mod to agree, so the module cannot be
// bumped without this test being re-run against the new version.
const fsnotifyVersion = "v1.9.0"
