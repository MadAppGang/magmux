//go:build unix

package doctor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// boundedOnFIFO runs read and FAILS, rather than hanging the suite, when it
// does not return within two seconds; on a timeout it releases the stuck open
// by opening the pipe's write end without blocking.
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
	case <-time.After(2 * time.Second):
	}
	for i := 0; i < 50; i++ {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		select {
		case <-done:
			t.Fatalf("%s blocked on a FIFO with no writer: it was opened before its type was checked (review r2 #1)", what)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("%s is still blocked on a FIFO and could not be released", what)
}

// Every file the doctor opens is judged by name first: a FIFO left where a
// transcript, a timeline or settings.json belongs is an answer, never a hung
// `agents doctor`.
func TestTheDoctorNeverOpensAFIFO(t *testing.T) {
	dir := t.TempDir()
	tail := filepath.Join(dir, "t.jsonl")
	settings := filepath.Join(dir, claude.SettingsFile)
	for _, p := range []string{tail, settings} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	}
	fsys := os.DirFS(dir)

	var err error
	boundedOnFIFO(t, tail, "tailLines", func() { _, err = tailLines(fsys, "t.jsonl", 64) })
	if err == nil {
		t.Error("a FIFO was tailed as a transcript")
	}

	d := &run{env: Env{Settings: fsys}}
	var (
		window time.Duration
		from   string
	)
	boundedOnFIFO(t, settings, "retention", func() { window, from, _ = d.retention() })
	if window != claude.DefaultTranscriptRetention || from != "the default" {
		t.Errorf("a FIFO settings.json gave %v from %q, want the default", window, from)
	}
}
