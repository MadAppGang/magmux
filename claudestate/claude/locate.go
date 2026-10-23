package claude

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/MadAppGang/magmux/claudestate/model"
)

// ProjectsRel is tt's containment rule for a path a record declares (a job's
// linkScanPath), verbatim (tt:adapters/transcriptfs/transcriptfs.go:132-208).
//
// A record is UNTRUSTED INPUT: following an absolute path we did not
// construct, wherever it points, would turn "read a transcript tail" into
// "read a tail of any file". So: "" names nothing; a relative path is
// refused; the path is Cleaned FIRST, so ".../projects/../../../etc/passwd"
// fails containment rather than passing it; then filepath.Rel against
// projectsPath, and ".", ".." or "../..." is refused. LEXICAL ONLY: no symlink
// is resolved and nothing is statted. ok == false with declared != "" is the
// caller's ProblemOutsideRoot.
func ProjectsRel(projectsPath, declared string) (rel string, ok bool) {
	if strings.TrimSpace(declared) == "" || !filepath.IsAbs(declared) {
		return "", false
	}
	root := filepath.Clean(projectsPath)
	if root == "" || root == "." {
		return "", false
	}
	r, err := filepath.Rel(root, filepath.Clean(declared))
	if err != nil {
		return "", false
	}
	if r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(r), true
}

// LocateRequest is one transcript lookup.
type LocateRequest struct {
	SessionID string
	// DeclaredPath is relative to the projects FS (ProjectsRel for an
	// absolute path). Set, it is addressed directly and never falls back.
	DeclaredPath string
	// DeclaredID: the host declared the session id (--session-id); located by
	// id only, and the result is Declared.
	DeclaredID bool
}

// LocateTranscript locates one transcript. It uses fs.Stat and fs.ReadDir
// only: nothing is opened, and nothing is kept. Hosts call it for a declared
// binding; the pipeline calls it, by id, for every session with no job
// speaker (§3.1 step 9, D5).
//
//   - DeclaredPath set: Stat it. A regular file is {Path, 1, Declared}; absent
//     (or a directory) is {Matches: 0, Declared}. No fallback, ever: a refused
//     or reaped path must not become a neighbour's transcript.
//   - Otherwise the by-id glob "*/<id>.jsonl" (tt's SessionGlobFor). It is
//     non-recursive, so a subagent transcript at depth 3 is unreachable. One
//     regular-file match is {Path, 1}; two or more are {Path: "", Matches: n}
//     and nothing is opened (D5); none is {Matches: 0}.
//   - An id outside tt's validSessionID alphabet (empty, longer than 128
//     bytes, or any byte outside [0-9A-Za-z_-]) is never globbed: a glob
//     metacharacter in an id would match another session's file.
//
// err != nil only for a filesystem error other than not-exist.
func LocateTranscript(fsys fs.FS, q LocateRequest) (model.Location, error) {
	if q.DeclaredPath != "" {
		loc := model.Location{Declared: true}
		rel := path.Clean(q.DeclaredPath)
		if !fs.ValidPath(rel) {
			return loc, nil
		}
		info, err := fs.Stat(fsys, rel)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return loc, nil
		case err != nil:
			return loc, fmt.Errorf("claude: stat %s: %w", rel, err)
		case !info.Mode().IsRegular():
			return loc, nil
		}
		loc.Path, loc.Matches = rel, 1
		return loc, nil
	}

	loc := model.Location{Declared: q.DeclaredID}
	if !ValidSessionID(q.SessionID) {
		return loc, nil
	}
	matches, err := globByID(fsys, q.SessionID)
	if err != nil {
		return loc, err
	}
	switch len(matches) {
	case 0:
	case 1:
		loc.Path, loc.Matches = matches[0], 1
	default:
		loc.Matches = len(matches)
	}
	return loc, nil
}

// SessionGlobFor is the by-id pattern, tt's: "*/<id>.jsonl".
func SessionGlobFor(sessionID string) string { return "*/" + sessionID + TranscriptSuffix }

// globByID is fs.Glob of SessionGlobFor, keeping regular files only (a
// DIRECTORY named "<id>.jsonl" is a shape we do not understand). It reads
// directory metadata only. A root that does not exist is zero matches.
func globByID(fsys fs.FS, id string) ([]string, error) {
	pattern := SessionGlobFor(id)
	matches, err := fs.Glob(fsys, pattern)
	if err != nil {
		return nil, fmt.Errorf("claude: glob %s: %w", pattern, err)
	}
	out := matches[:0]
	for _, m := range matches {
		info, err := fs.Stat(fsys, m)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // reaped between the listing and the stat
			}
			return nil, fmt.Errorf("claude: stat %s: %w", m, err)
		}
		if info.Mode().IsRegular() {
			out = append(out, m)
		}
	}
	return out, nil
}

// ValidSessionID is tt's validSessionID: an id that can be pasted into a glob
// without changing what the glob means. A WHITELIST, deliberately: reasoning
// about which metacharacters are dangerous in combination is wrong once and
// then wrong forever.
func ValidSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// CheckDeclared is main's binding rule, with no I/O: when both a declared path
// and a declared session id are given, the file must be "<sessionID>.jsonl".
// A host with an absolute declared path outside the projects tree passes
// os.DirFS(filepath.Dir(p)) and filepath.Base(p) to LocateTranscript.
func CheckDeclared(declaredPath, sessionID string) error {
	if declaredPath == "" || sessionID == "" {
		return nil
	}
	if base := path.Base(filepath.ToSlash(declaredPath)); base != sessionID+TranscriptSuffix {
		return fmt.Errorf("claude: the declared transcript %q is not %s%s", base, sessionID, TranscriptSuffix)
	}
	return nil
}

// SubagentsDir is "<projectDir>/<id>/subagents" for a located transcript, ""
// when loc.Matches != 1. The depth matters: one level too high is a silent 0.
func SubagentsDir(loc model.Location) string {
	if loc.Matches != 1 || loc.Path == "" {
		return ""
	}
	return path.Join(strings.TrimSuffix(loc.Path, TranscriptSuffix), SubagentsDirName)
}

// CountSubagents counts subagents/*.meta.json by NAME, through fs.ReadDir
// only (D1, C7). It never opens a meta file: subagents/ is never armed (C11),
// so reading one would be a content read no arm covers (Invariant A), and a
// meta file carries the instruction the agent was given.
//
//   - located, directory absent: &0, EvidenceAbsent. Claude creates the
//     directory LAZILY on the first spawn (186 of 192 that exist are
//     non-empty; 94% of transcripts have none), so absence is a determined 0.
//   - located, directory present: &n, EvidenceCounted.
//   - not located: nil, EvidenceNoTranscript. Ambiguous: nil, EvidenceAmbiguous.
//   - any other read error: nil, EvidenceUnreadable. A permission error is not
//     an observation.
func CountSubagents(fsys fs.FS, loc model.Location) (*int, model.Evidence) {
	switch {
	case loc.Matches >= 2:
		return nil, model.EvidenceAmbiguous
	case loc.Matches != 1 || loc.Path == "":
		return nil, model.EvidenceNoTranscript
	}
	entries, err := fs.ReadDir(fsys, SubagentsDir(loc))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			n := 0
			return &n, model.EvidenceAbsent
		}
		return nil, model.EvidenceUnreadable
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), SubagentMetaSuffix) {
			n++
		}
	}
	return &n, model.EvidenceCounted
}
