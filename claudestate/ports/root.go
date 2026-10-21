package ports

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Tree is one Claude directory.
//
//   - Path: the directory as configured, made absolute and filepath.Clean'ed,
//     NOT symlink-resolved. Record paths (a job's linkScanPath) are compared
//     against it lexically, as tt does.
//   - Dir: Path with symlinks resolved; what the watcher arms and Verify
//     stats. "" means unwatchable.
//   - FS: reads the same directory (os.DirFS(Dir) in production).
//
// A zero Tree is an ABSENT source (reported Off), not an error. A Tree with FS
// and Path but Dir == "" (an fstest.MapFS, or testdata under a canonical Path)
// can be read and cannot be watched.
type Tree struct {
	Path, Dir string
	FS        fs.FS
}

// IsZero reports an absent source.
func (t Tree) IsZero() bool { return t.Path == "" && t.Dir == "" && t.FS == nil }

// Root is the three Claude trees a cycle reads.
type Root struct {
	Sessions, Jobs, Projects Tree
	// Base is the Claude root's Dir, "" when the trees were given separately
	// (and then the root is never armed: no M12).
	Base string
}

// Directory names under one Claude root.
const (
	sessionsDirName = "sessions"
	jobsDirName     = "jobs"
	projectsDirName = "projects"
)

// TreeAt builds the Tree for dir.
//
// Path is Clean(Abs(dir)). Dir is EvalSymlinks of the deepest EXISTING
// ancestor, joined with the missing remainder, so a directory that does not
// exist yet still yields a usable Tree and a nil error: `jobs/` appearing
// later is a normal event, not a misconfiguration.
//
// The error is only for an unresolvable path: Getwd fails for a relative dir,
// or a symlink loop. With an error the returned Tree is zero.
//
// TreeAt reads metadata only (lstat through EvalSymlinks); it opens nothing.
func TreeAt(dir string) (Tree, error) {
	if dir == "" {
		return Tree{}, errors.New("ports: TreeAt: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Tree{}, fmt.Errorf("ports: TreeAt %s: %w", dir, err)
	}
	abs = filepath.Clean(abs)
	resolved, err := resolveExisting(abs)
	if err != nil {
		return Tree{}, fmt.Errorf("ports: TreeAt %s: %w", dir, err)
	}
	return Tree{Path: abs, Dir: resolved, FS: os.DirFS(resolved)}, nil
}

// resolveExisting resolves symlinks in the deepest existing ancestor of abs
// and re-joins the part that does not exist yet.
func resolveExisting(abs string) (string, error) {
	var missing []string
	p := abs
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				r = filepath.Join(r, missing[i])
			}
			return r, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		missing = append(missing, filepath.Base(p))
		p = parent
	}
}

// RootAt is sessions/, jobs/ and projects/ under one Claude directory, with
// Base set to that directory's Dir.
func RootAt(claudeDir string) (Root, error) {
	base, err := TreeAt(claudeDir)
	if err != nil {
		return Root{}, err
	}
	var r Root
	for _, t := range []struct {
		name string
		dst  *Tree
	}{
		{sessionsDirName, &r.Sessions},
		{jobsDirName, &r.Jobs},
		{projectsDirName, &r.Projects},
	} {
		tr, err := TreeAt(filepath.Join(base.Path, t.name))
		if err != nil {
			return Root{}, err
		}
		*t.dst = tr
	}
	r.Base = base.Dir
	return r, nil
}

// RootFromEnv is $CLAUDE_CONFIG_DIR, else ~/.claude.
//
// HOSTS ONLY. No test may call it (guard G8): a test that reaches the real
// Claude root reads thirty live agents' state.
func RootFromEnv() (Root, error) {
	return rootFrom(os.Getenv("CLAUDE_CONFIG_DIR"), os.UserHomeDir)
}

// rootFrom is RootFromEnv with its two environment reads injected, so the rule
// can be tested without the real environment.
func rootFrom(configDir string, home func() (string, error)) (Root, error) {
	if configDir != "" {
		return RootAt(configDir)
	}
	h, err := home()
	if err != nil {
		return Root{}, fmt.Errorf("ports: no CLAUDE_CONFIG_DIR and no home directory: %w", err)
	}
	if h == "" {
		return Root{}, errors.New("ports: no CLAUDE_CONFIG_DIR and an empty home directory")
	}
	return RootAt(filepath.Join(h, ".claude"))
}
