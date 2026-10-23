package claude

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// The union corpus. Every test here reads it READ-ONLY, through os.DirFS or
// os.ReadFile; none touches the real Claude root, probes a pid or speaks to
// tmux. See testdata/PROVENANCE.md for what each file is.
const corpus = "../testdata"

func rootDir(scenario, tree string) string { return filepath.Join(corpus, "roots", scenario, tree) }

func readCorpus(t *testing.T, rel ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{corpus}, rel...)...))
	if err != nil {
		t.Fatalf("reading testdata: %v", err)
	}
	return b
}

func mmRecord(t *testing.T, name string) (Record, []model.Problem, error) {
	t.Helper()
	return DecodeRecord(name, readCorpus(t, "roots", "magmux-corpus", "sessions", name))
}

func ttRecord(t *testing.T, name string) (Record, []model.Problem, error) {
	t.Helper()
	return DecodeRecord(name, readCorpus(t, "roots", "tt-corpus", "sessions", name))
}

func ttJob(t *testing.T, dir string) (Job, []model.Problem, error) {
	t.Helper()
	return DecodeJob(dir, readCorpus(t, "roots", "tt-corpus", "jobs", dir, JobStateFile))
}

func hasProblem(ps []model.Problem, kind model.ProblemKind, field string) bool {
	for _, p := range ps {
		if p.Kind == kind && (field == "" || p.Field == field) {
			return true
		}
	}
	return false
}

// tailOf is what internal/source.Tail hands the decoders: the last n bytes of
// a file and the window that describes them.
func tailOf(b []byte, n int) ([]byte, model.Window) {
	w := model.Window{TailBytes: n, FileSize: int64(len(b))}
	if len(b) > n {
		w.Partial = true
		return b[len(b)-n:], w
	}
	return b, w
}

// openCounter is an fs.FS that counts Open calls, so "nothing was opened" is a
// count rather than an inference from a right answer.
type openCounter struct {
	fs.FS
	opens []string
}

func (c *openCounter) Open(name string) (fs.File, error) {
	c.opens = append(c.opens, name)
	return c.FS.Open(name)
}

// ReadDir and Stat go through the wrapped FS's own methods when it has them,
// so a metadata read is not counted as an open.
func (c *openCounter) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(c.FS, name) }
func (c *openCounter) Stat(name string) (fs.FileInfo, error)      { return fs.Stat(c.FS, name) }

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, err
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, []byte(l))
		}
	}
	return out
}
