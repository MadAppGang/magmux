//go:build unix

package source

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/model"
	"github.com/MadAppGang/magmux/claudestate/ports"
)

// fifoBound is how long one read of a FIFO may take before the test calls it
// a hang. A refused FIFO costs a stat; a second is three orders of magnitude
// of headroom.
const fifoBound = 2 * time.Second

// mkfifo makes a named pipe at path, which is always under the test's own
// t.TempDir(). No writer ever opens it, so a blocking open of it waits forever.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
}

// boundedOnFIFO runs read and FAILS, rather than hanging the suite, when it
// does not return within fifoBound. On a timeout it releases the stuck read
// by opening the pipe's write end without blocking (which succeeds exactly
// because a reader is waiting in open), so the goroutine ends with the test.
func boundedOnFIFO(t *testing.T, fifo, what string, read func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		read()
	}()
	select {
	case <-done:
		return
	case <-time.After(fifoBound):
	}
	for i := 0; i < 50; i++ {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		select {
		case <-done:
			t.Fatalf("%s did not return within %v on a FIFO with no writer: the open blocked before the "+
				"file type was checked (review r2 #1)", what, fifoBound)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("%s is still blocked on a FIFO after %v and could not be released", what, fifoBound)
}

// A record named *.json that is a FIFO is refused by its NAME before it is
// opened, and reported: os.DirFS's Open of a FIFO blocks until a writer
// arrives, so a check made only on the opened handle is never reached, and the
// coordinator (and with it Stop) would hang (review r2 #1). The handle checks
// stay behind it for a file swapped in after the name check.
func TestAFIFONamedAsARecordIsAProblemAndNeverAHang(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "1.json")
	mkfifo(t, fifo)
	r := NewRegistry(ports.Tree{Path: dir, FS: os.DirFS(dir)}, ports.RealClock(), time.Millisecond)
	var got RegistryResult
	boundedOnFIFO(t, fifo, "Registry.Read", func() { got = r.Read() })
	if got.State != model.SourceDegraded || !has(got.Problems, model.ProblemUnreadable) {
		t.Fatalf("a FIFO record is not a reported problem: %+v", got)
	}
	for _, p := range got.Problems {
		if p.Kind == model.ProblemUnreadable && !strings.Contains(p.Detail, "not a regular file") {
			t.Errorf("the problem does not say why: %q", p.Detail)
		}
	}
}

func TestAFIFOJobRecordIsAProblemAndNeverAHang(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "j1"), 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "j1", "state.json")
	mkfifo(t, fifo)
	j := NewJobs(ports.Tree{Path: dir, FS: os.DirFS(dir)})
	var got JobsResult
	boundedOnFIFO(t, fifo, "Jobs.Read", func() { got = j.Read([]string{"j1"}, func(string) bool { return true }) })
	if got.State != model.SourceDegraded || !has(got.Problems, model.ProblemUnreadable) {
		t.Fatalf("a FIFO job record is not a reported problem: %+v", got)
	}
}

// The same rule for the one tail reader: a transcript named *.jsonl that is a
// FIFO is an error, never a blocked actor (which Stop waits for).
func TestATailOfAFIFOIsAnErrorAndNeverAHang(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "t.jsonl")
	mkfifo(t, fifo)
	var err error
	boundedOnFIFO(t, fifo, "Tail", func() { _, _, err = Tail(os.DirFS(dir), "t.jsonl", 64) })
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a FIFO transcript was tailed: err %v", err)
	}
}

// A symlink to a FIFO is the same refusal: the name check follows the link,
// as the open would.
func TestASymlinkToAFIFOIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	mkfifo(t, fifo)
	if err := os.Symlink("pipe", filepath.Join(dir, "1.json")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	r := NewRegistry(ports.Tree{Path: dir, FS: os.DirFS(dir)}, ports.RealClock(), time.Millisecond)
	var got RegistryResult
	boundedOnFIFO(t, fifo, "Registry.Read through a symlink", func() { got = r.Read() })
	if !has(got.Problems, model.ProblemUnreadable) {
		t.Fatalf("a symlinked FIFO is not a reported problem: %+v", got)
	}
}
