package mux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
)

// A DECLARED transcript binding: the caller names the Claude Code session a
// pane runs (--session-id / --transcript, or open_pane session_id /
// transcript), and the controller tails exactly that file and nothing else.
//
// Discovery cannot be made right for a wrapper launch. A pane whose command is
// `. pane-launch.sh` shows no prompt, so findActiveSession falls back to the
// newest transcript in the cwd's project directory — and the child writes NO
// transcript until its first prompt is accepted, so at the first send every
// candidate belongs to somebody else: a sibling slot started a moment later
// (fresh, so it wins), or the parent session still being written. claimSession
// is per magmux process and claudish runs one magmux per pane, so nothing
// excludes them, and the claim then sticks for the life of the pane. The gate
// reads that foreign transcript (convChange, the registry) and answers for a
// session the pane is not running.
//
// The caller already KNOWS the session: claudish hands Claude Code the
// --session-id and computes the transcript path itself. So a declared binding
// waits for that file to appear and never falls back to discovery — an empty
// answer for a while is honest; a foreign one is not.

// bindMode says how a Claude pane's transcript was found, echoed in `list` as
// `bound` so a caller can refuse to trust a gate it cannot attribute.
const (
	bindNamed  = "named"  // declared by the caller; the only file ever tailed
	bindPrompt = "prompt" // discovery: first user prompt matched the launch's
	bindMtime  = "mtime"  // discovery: newest transcript, fresh preferred
)

// sessionIDMax bounds a declared id. Claude Code's are UUIDs (36).
const sessionIDMax = 128

// namedScanEvery throttles the id-only search over every project directory,
// for the same reason broadScanInterval throttles discovery's.
const namedScanEvery = time.Second

// normalizeBinding validates a declared binding and fills the id from the
// path when only the path was given. Both empty is no binding.
//
// The id is restricted to the characters a UUID can hold plus '_', so it can
// never be a path: it is joined into a file name. The path must be absolute
// (relative to WHOSE cwd?) and name a .jsonl file. Given both, they must agree
// — a path that is not <id>.jsonl would tail one session and read the
// registry of another, the exact mix-up the binding exists to prevent.
func normalizeBinding(id, path string) (string, string, error) {
	if id == "" && path == "" {
		return "", "", nil
	}
	if path != "" {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", "", fmt.Errorf("transcript must be a clean absolute path (got %q)", path)
		}
		if !strings.HasSuffix(path, claude.TranscriptSuffix) {
			return "", "", fmt.Errorf("transcript must name a %s file (got %q)", claude.TranscriptSuffix, path)
		}
		base := strings.TrimSuffix(filepath.Base(path), claude.TranscriptSuffix)
		if id == "" {
			id = base
		} else if id != base {
			return "", "", fmt.Errorf("transcript %q is not session %q's file (want %s%s)", path, id, id, claude.TranscriptSuffix)
		}
	}
	if len(id) == 0 || len(id) > sessionIDMax {
		return "", "", fmt.Errorf("session id must be 1-%d characters (got %d)", sessionIDMax, len(id))
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", "", fmt.Errorf("session id may hold only letters, digits, '-' and '_' (got %q)", id)
		}
	}
	return id, path, nil
}

// findNamedSession is findActiveSession for a declared binding: the declared
// path, else <config>/projects/<encoded cwd>/<id>.jsonl, else that file name
// in any project directory (throttled) — the encoded-cwd name is Claude
// Code's undocumented contract, so it is a first guess and never the only way
// in. It never considers a file with any other name. Returns "" until the
// file exists and is claimed.
func (c *ClaudeCodeController) findNamedSession() string {
	path := c.bindPath
	if path == "" {
		path = c.namedPathByID()
	} else if _, err := os.Stat(path); err != nil {
		path = ""
	}
	if path == "" {
		return ""
	}
	if c.pane != nil && c.pane.mux != nil && !c.pane.mux.claimSession(path, c.pane) {
		// Another pane of THIS magmux declared or holds the same file. Waiting
		// is the only honest answer; tailing it would share one session's
		// state between two panes.
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p declared %s is claimed by another pane\n", c.pane, path)
		}
		return ""
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p strategy=named %s\n", c.pane, path)
	}
	return path
}

func (c *ClaudeCodeController) namedPathByID() string {
	name := c.bindID + claude.TranscriptSuffix
	if c.projectDir != "" {
		p := filepath.Join(c.projectDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if c.projectsDir == "" || time.Since(c.lastBroadAt) < namedScanEvery {
		return ""
	}
	c.lastBroadAt = time.Now()
	hits, _ := filepath.Glob(filepath.Join(c.projectsDir, "*", name))
	if len(hits) == 1 {
		return hits[0]
	}
	if len(hits) > 1 && dbgFile != nil {
		// One session id in two project directories is not a state Claude Code
		// produces; picking one would be a guess, so keep waiting.
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p session %s is in %d project dirs; not choosing\n",
			c.pane, c.bindID, len(hits))
	}
	return ""
}

// configDirOfTranscript is the Claude config directory a transcript lives
// under, <config>/projects/<dir>/<id>.jsonl, or "" when the path is not
// shaped that way.
func configDirOfTranscript(path string) string {
	projects := filepath.Dir(filepath.Dir(path))
	if filepath.Base(projects) != claudeProjectsDir {
		return ""
	}
	return filepath.Dir(projects)
}

// Binding is the pane's transcript binding for `list`, safe from any
// goroutine: the declared fields are write-once before publication, and the
// discovered ones come from the published atomics.
func (c *ClaudeCodeController) Binding() (path, id, mode string, seen bool) {
	found := c.discoveredSession()
	if c.bindID != "" {
		path = c.bindPath
		if path == "" {
			path = found
		}
		return path, c.bindID, bindNamed, found != ""
	}
	if found == "" {
		return "", "", "", false
	}
	if m := c.boundBy.Load(); m != nil {
		mode = *m
	}
	return found, strings.TrimSuffix(filepath.Base(found), claude.TranscriptSuffix), mode, true
}
