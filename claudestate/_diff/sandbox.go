package main

// sandbox.go is the ONE file in this module that writes (architecture §4.7
// C1, guard G2's one cited entry). It may call exactly three path functions,
// os.MkdirTemp, os.OpenRoot and os.RemoveAll of that MkdirTemp result, and
// every other write goes through an *os.Root: the sandbox's own, or the one
// opened on claudestate/testdata for -record. Nothing here can write outside
// those two directories, because an *os.Root refuses a name that escapes it,
// symlinks included.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/internal/difftest"
)

// sandbox is one run's private directory.
type sandbox struct {
	dir  string // as os.MkdirTemp returned it: NOT symlink-resolved
	root *os.Root
}

// withSandbox makes a fresh directory, hands it to fn, and removes it after,
// unless keep is set. The path must survive being spliced into JSON and a
// /bin/sh script, or nothing runs (§8.2 step 3).
func withSandbox(keep bool, fn func(*sandbox) error) error {
	dir, err := os.MkdirTemp("", "claudestate-diff-")
	if err != nil {
		return fmt.Errorf("making the sandbox: %w", err)
	}
	if keep {
		fmt.Fprintf(os.Stderr, "_diff: keeping the sandbox at %s\n", dir)
	} else {
		defer os.RemoveAll(dir)
	}
	if !difftest.SafeForJSON(dir) {
		return fmt.Errorf("the sandbox path %q contains a quote, a backslash or a control character", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("opening the sandbox: %w", err)
	}
	defer root.Close()
	return fn(&sandbox{dir: dir, root: root})
}

// path is the absolute OS path of a sandbox-relative name, for a child's
// flags and environment.
func (s *sandbox) path(rel string) string { return filepath.Join(s.dir, filepath.FromSlash(rel)) }

func (s *sandbox) mkdir(rel string) error { return s.root.MkdirAll(rel, 0o700) }

func (s *sandbox) writeFile(rel string, data []byte, perm fs.FileMode) error {
	if err := s.root.MkdirAll(path.Dir(rel), 0o700); err != nil {
		return err
	}
	return s.root.WriteFile(rel, data, perm)
}

// copyFS copies every regular file of src under dst. rewrite may rename a
// file and change its bytes; it sees slash-separated names relative to src.
func (s *sandbox) copyFS(dst string, src fs.FS, rewrite func(name string, data []byte) (string, []byte, error)) error {
	if err := s.mkdir(dst); err != nil {
		return err
	}
	return fs.WalkDir(src, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name == "." {
				return nil
			}
			return s.mkdir(path.Join(dst, name))
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("copying %s: not a regular file", name)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := fs.ReadFile(src, name)
		if err != nil {
			return err
		}
		to := name
		if rewrite != nil {
			if to, data, err = rewrite(name, data); err != nil {
				return fmt.Errorf("rewriting %s: %w", name, err)
			}
		}
		return s.writeFile(path.Join(dst, to), data, info.Mode().Perm()|0o600)
	})
}

// extractTar streams a tar (git archive's output) into dst. A leading
// "tools/tt/" is stripped, so both `git archive REV:tools/tt` and
// `git archive REV tools/tt` work. Only directories and regular files are
// accepted.
func (s *sandbox) extractTar(dst string, r io.Reader) error {
	if err := s.mkdir(dst); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the tt archive: %w", err)
		}
		name := strings.TrimPrefix(path.Clean(h.Name), "tools/tt")
		name = strings.TrimPrefix(name, "/")
		switch h.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			if name == "" || name == "." || name == "tools" {
				continue
			}
			if err := s.mkdir(path.Join(dst, name)); err != nil {
				return err
			}
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := s.writeFile(path.Join(dst, name), data, fs.FileMode(h.Mode).Perm()|0o600); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the tt archive holds %s, which is neither a directory nor a regular file", h.Name)
		}
	}
}

// recorder writes the canonical references (§8.2 step 10) into
// claudestate/testdata/tt-reference/, through a second *os.Root opened on
// claudestate/testdata.
type recorder struct {
	root *os.Root
}

func openRecorder(testdata string) (*recorder, error) {
	root, err := os.OpenRoot(testdata)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", testdata, err)
	}
	if err := root.MkdirAll(referenceDir, 0o755); err != nil {
		root.Close()
		return nil, err
	}
	return &recorder{root: root}, nil
}

func (r *recorder) write(name string, data []byte) error {
	return r.root.WriteFile(path.Join(referenceDir, name), data, 0o644)
}

func (r *recorder) close() { r.root.Close() }
