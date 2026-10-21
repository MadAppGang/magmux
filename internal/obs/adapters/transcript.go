package adapters

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTailBytes is how much of a transcript is read, from the END.
//
// Measured for COLOUR: a 200 KB tail missed no agentColor record across 35
// live transcripts, and the worst case needed 44.1 KB. The largest transcript
// observed on the same machine was 630.9 MB and still growing, so a whole-file
// scan is not slow — it is impossible, and it will stay impossible. The bound
// is a field on [Transcripts] rather than a constant at the read site so a test
// can shrink it: a bound only exercised against a 48 KB file proves the
// mechanism and says nothing about the number.
//
// It was NOT measured for errors, and it does miss them. On 2026-09-19, 13 of
// 36 live sessions had API-level errors in their transcripts and none inside
// this window — the nearest was 316 KB from the end. That is why the error
// count is documented as RECENT; see [Reading.APIErrors].
const DefaultTailBytes = 200 << 10

// LocateMethod says HOW a transcript was found, which is not bookkeeping: it
// is the difference between an answer magmux can stand behind and one it had
// to search for.
type LocateMethod uint8

const (
	// LocatedNowhere is a miss. No transcript exists for that session id under
	// this root — which is a real state and not an error: a session that has
	// just started has not written one yet, and one that never writes a
	// transcript never will.
	LocatedNowhere LocateMethod = iota
	// LocatedByLaunchDir means the encoded launch directory held it, with no
	// search at all. This is the 100% case, and it is available only because
	// magmux SPAWNED the session and therefore holds the directory it was
	// launched in.
	LocatedByLaunchDir
	// LocatedByScan means the launch directory was unknown, or was wrong
	// because the session moved after startup, and the transcript was found by
	// looking for its session id under every project directory. For those
	// sessions this is not a fallback: it is the only correct answer.
	LocatedByScan
)

func (m LocateMethod) String() string {
	switch m {
	case LocatedByLaunchDir:
		return "launch directory"
	case LocatedByScan:
		return "scan"
	default:
		return "nowhere"
	}
}

// Location is where one session's transcript is.
//
// The zero value is a miss, and [Location.Found] is how to ask. Nothing about
// it is derived from a timestamp: which file belongs to a session is decided
// by the session id in its NAME and by nothing else.
type Location struct {
	// SessionID is the session this location is for. It is carried so that
	// [Transcripts.Subagents] cannot be handed one session's directory and
	// another session's id.
	SessionID string
	// Path is the transcript file, or "" for a miss.
	Path string
	// Dir is the project directory it was found in, or "".
	Dir string
	// Method says how it was found.
	Method LocateMethod
	// Matches is how many project directories hold a transcript with this
	// session id. It is normally 1, and 2 is a real possibility after a
	// session changes directory. It is reported rather than hidden because the
	// caller is the only one that can decide what to do about it, and a
	// resolver that silently picks one of two is the exact failure mode this
	// whole area exists to avoid. It is 0 on a miss, and 1 when the launch
	// directory answered without a scan — that path looks at one directory and
	// cannot count the others.
	Matches int
}

// Found reports whether a transcript was located.
func (l Location) Found() bool { return l.Path != "" }

// Reading is what one bounded read of a transcript found.
//
// Every field is scoped to WHAT WAS READ, and Truncated is what says whether
// that is the whole file. A caller that reports the error count without
// reading Truncated is reporting the errors in the last 200 KB as though they
// were the errors in the session, and that is not the same claim.
//
// This comment used to add that it was "usually the same number". Measured
// against the live registry on 2026-09-19 it was a DIFFERENT number for 13 of
// 36 sessions (36%): each had errors, every one outside the window, and the
// count said 0. The middleware now discloses the window on a zero as well as on
// a count; see claudecode's faultDetail.
type Reading struct {
	// Color is the last agentColor record in what was read, or nil for "no
	// record of one". nil is the ONLY spelling of absence here: the empty
	// string is a VALUE — a session explicitly coloured with the default — so
	// a reader that treats "" as missing loses a distinction the tool makes.
	Color *string

	// APIErrors is how many API-level error records were in what was read —
	// the last [Reading.Window] bytes of the transcript, which on a truncated
	// read is the RECENT part of it and not the session. Errors before the
	// window are not counted, and a zero here says nothing about them.
	//
	// This is isApiErrorMessage and never is_error: every one of 497 observed
	// occurrences of is_error was RECOVERED, and counting those reports 29 of
	// 32 healthy sessions as broken.
	APIErrors int

	// LastError is the text of the most recent API-level error, for a human.
	// Free text; never switched on.
	LastError string

	// BytesRead is how many bytes were actually read, across every window.
	// It is here so that "transcripts are tailed, never scanned whole" is an
	// assertion a test can make. Asserting elapsed time instead passes on a
	// small file and proves nothing about a 630.9 MB one.
	BytesRead int64

	// Size is the file's size at the moment it was read, and ModTime its
	// modification time. They are CACHE VALIDITY and nothing else — see
	// [Transcripts.Read]. Neither is an activity signal: records of type
	// artifact-autoreact-ledger touch a transcript with no conversational turn
	// behind them, and one measured session looked 1.37 hours stale against
	// its transcript while being correctly and continuously idle.
	Size    int64
	ModTime int64

	// Truncated is true when the file was larger than the bound, so the read
	// covered a window rather than the whole session.
	Truncated bool

	// Window is how many bytes APIErrors and LastError were counted over: the
	// whole file when it fit, else the bound, counted back from the end. It is
	// what a caller discloses beside the count — "how much was examined, out
	// of how much" is Window and Size — and it deliberately excludes the head
	// fallback, which answers the colour question only and adds nothing to the
	// count.
	Window int64

	// HeadFallback is true when the tail window held no colour and a bounded
	// window at the START of the file was read to look for one. It answers the
	// colour question only; see [Transcripts.Read].
	HeadFallback bool

	// Malformed is how many lines in the windows did not parse as JSON. The
	// first and last are expected on a truncated read and are not counted:
	// this is what is left after the partial lines are discarded.
	Malformed int
}

// Subagents is the result of counting one session's subagents.
//
// Known is the field that matters, and it means "we were able to LOOK" rather
// than "the directory was there". A count of 0 with Known true is "this session
// has spawned nothing"; Known false is "nobody counted", which is a session
// whose transcript was never found, or one whose directory could not be read
// for some reason other than its absence. Collapsing the two is the difference
// between a finished session and an unexamined one — see [Transcripts.Subagents]
// for the measurement that decides which side a MISSING directory falls on.
type Subagents struct {
	// Count is how many metadata files were there. Zero when the directory
	// does not exist, which is the ordinary shape of a session that has never
	// spawned a subagent.
	Count int
	// Known is whether we were in a position to count.
	Known bool
	// Malformed is how many of the counted files did not parse as JSON.
	//
	// They are COUNTED anyway, and that is a decision rather than an
	// oversight: a subagent's existence is attested by its file existing, and
	// a metadata file caught half-written describes a subagent that is running
	// right now. The alternative reading — an unparseable file yields no
	// opinion — would make a live subagent disappear from the count at the
	// exact moment it started. The number is reported so the choice is
	// visible to a caller that disagrees.
	Malformed int
}

// Transcripts reads Claude Code's transcript tree, at <config>/projects.
//
// # It is read-only, and that is not a figure of speech
//
// It opens files, stats them and lists directories. It creates nothing, writes
// nothing and removes nothing, not even a temporary file or a lock, because
// the tree it reads belongs to thirty running processes that are writing it at
// the same time.
//
// # Nothing is hardcoded
//
// Root is injected for the same reason the registry's is: a hardcoded
// ~/.claude means the only way to test this is against live sessions. Every
// path this type opens is built by joining Root with names it has validated as
// single path components, so a session id carrying a separator or a `..`
// cannot reach out of the tree — the containment is structural, not a rule
// somebody has to remember.
//
// # Concurrency
//
// A Transcripts is a plain value with no mutable state. Copies are
// independent and every method is safe to call from any goroutine.
type Transcripts struct {
	// Root is the projects directory, normally $CLAUDE_CONFIG_DIR/projects or
	// ~/.claude/projects. Required.
	Root string

	// TailBytes bounds each window. Non-positive takes [DefaultTailBytes].
	TailBytes int
}

func (t Transcripts) bound() int64 {
	if t.TailBytes > 0 {
		return int64(t.TailBytes)
	}
	return DefaultTailBytes
}

// errBadName reports a name that cannot be a single path component. It is
// returned rather than sanitised: a caller that asked for a session id with a
// slash in it has a bug, and quietly cleaning the value would hide it.
var errBadName = errors.New("adapters: not a single path component")

func checkName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsRune(name, filepath.Separator) ||
		strings.ContainsRune(name, '/') {
		return fmt.Errorf("%w: %q", errBadName, name)
	}
	return nil
}

// Locate answers where a session's transcript is, or that it does not know.
//
// # The launch directory, and why the live one will not do
//
// launchDir is the directory the session was LAUNCHED in. When it is known,
// one stat answers the question. It may be empty, and for any session magmux
// did not spawn it will be: then this scans the project directories for a file
// named after the session id.
//
// Feeding this function a session's LIVE working directory instead resolves
// about 96% of real sessions. The other 4% do not fail loudly, because the
// wrongly derived directory usually EXISTS and holds another session's
// transcripts. What stops that from becoming a wrong ANSWER here is that the
// file is addressed by session id: a wrong directory can only miss. A resolver
// that instead picked "the newest .jsonl in the derived directory" would
// return another session's transcript and report its colour, its error count
// and its subagents as this session's, with nothing anywhere to say so.
//
// The scan is the only correct answer for a session that MOVED after startup —
// which is not exotic: 115 transcripts on the machine this was measured on sit
// under a parent repository's directory name because the session was moved
// into a worktree afterwards.
func (t Transcripts) Locate(sessionID, launchDir string) (Location, error) {
	if t.Root == "" {
		return Location{}, fmt.Errorf("adapters: a projects root is required")
	}
	if err := checkName(sessionID); err != nil {
		return Location{}, err
	}
	loc := Location{SessionID: sessionID}
	name := sessionID + TranscriptSuffix

	if launchDir != "" {
		dir := ProjectDirName(launchDir)
		// The encoder cannot produce a separator — every character outside
		// [A-Za-z0-9] becomes a dash — but the environment override can name
		// anything, and it is checked rather than trusted.
		if err := checkName(dir); err == nil {
			path := filepath.Join(t.Root, dir, name)
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
				loc.Path, loc.Dir = path, filepath.Join(t.Root, dir)
				loc.Method, loc.Matches = LocatedByLaunchDir, 1
				return loc, nil
			}
		}
	}

	entries, err := os.ReadDir(t.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No projects tree at all. That is a miss, not a failure: a
			// machine with no Claude Code on it is a legitimate machine.
			return loc, nil
		}
		return loc, fmt.Errorf("adapters: reading %s: %w", t.Root, err)
	}
	// Every directory is examined even after a hit, so that Matches is a
	// COUNT rather than "at least one". ReadDir returns entries sorted by
	// name, so the first match is the same one on every run and on every
	// machine — a resolver that answered differently depending on directory
	// order would make two magmuxes disagree about one session.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(t.Root, e.Name(), name)
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		loc.Matches++
		if loc.Path == "" {
			loc.Path, loc.Dir = path, filepath.Join(t.Root, e.Name())
			loc.Method = LocatedByScan
		}
	}
	return loc, nil
}

// FileState is a transcript's size and modification time, and it exists for
// ONE purpose: deciding whether a previous [Reading] is still current.
//
// It is not a freshness signal about the session and must never be used as
// one. Transcript records that carry no conversational turn — the artifact
// ledgers — touch the file, and one measured session looked 1.37 hours stale
// against its transcript while being correctly idle throughout. A cross-check
// of a session's status instant against this number manufactures a wrong
// answer out of a right one.
type FileState struct {
	Size    int64
	ModTime int64
}

// Stat reports the transcript's current [FileState]. See that type before
// using it for anything but a cache check.
func (t Transcripts) Stat(loc Location) (FileState, error) {
	if !loc.Found() {
		return FileState{}, fmt.Errorf("adapters: no transcript to stat")
	}
	st, err := os.Stat(loc.Path)
	if err != nil {
		return FileState{}, fmt.Errorf("adapters: %w", err)
	}
	return FileState{Size: st.Size(), ModTime: st.ModTime().UnixNano()}, nil
}

// Read reads the transcript at loc, bounded, and reports what it found.
//
// # What it reads
//
// The LAST [Transcripts.TailBytes] bytes, and the whole file when it is
// smaller than that. On a truncated read the first partial line is discarded,
// because half a JSON record is not a record.
//
// If that window holds no colour at all, a second window of the same size is
// read from the START of the file. That answers the colour question ONLY: a
// colour is last-record-wins, so a session that set one early and never again
// has its only record at the top, and a bounded tail would report "no colour"
// for a session that plainly has one. The error count is deliberately not
// taken from the head window — the two windows overlap on a file smaller than
// twice the bound, and a count that double-reports is worse than a count that
// says what it covered.
//
// # Size and ModTime are cache validity, never activity
//
// They are reported so a caller can skip a re-read when nothing has changed.
// They are not a freshness signal and must never be cross-checked against a
// session's own status instant: transcript records of type
// artifact-autoreact-ledger are written with no conversational turn behind
// them, and one measured session looked 1.37 hours stale against its
// transcript while being correctly idle the whole time. Correcting a
// correctly-written status field against this number manufactures a wrong
// answer out of a right one.
func (t Transcripts) Read(loc Location) (Reading, error) {
	var r Reading
	if !loc.Found() {
		return r, fmt.Errorf("adapters: no transcript to read")
	}
	f, err := os.Open(loc.Path)
	if err != nil {
		return r, fmt.Errorf("adapters: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return r, fmt.Errorf("adapters: %w", err)
	}
	r.Size, r.ModTime = st.Size(), st.ModTime().UnixNano()

	bound := t.bound()
	if r.Size <= bound {
		r.Window = r.Size
		n, err := t.scan(f, &r, window{}, false)
		r.BytesRead += n
		return r, err
	}

	r.Truncated, r.Window = true, bound
	if _, err := f.Seek(r.Size-bound, io.SeekStart); err != nil {
		return r, fmt.Errorf("adapters: %w", err)
	}
	n, err := t.scan(io.LimitReader(f, bound), &r, window{cutAtStart: true}, false)
	r.BytesRead += n
	if err != nil {
		return r, err
	}
	if r.Color != nil {
		return r, nil
	}

	r.HeadFallback = true
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return r, fmt.Errorf("adapters: %w", err)
	}
	n, err = t.scan(io.LimitReader(f, bound), &r, window{cutAtEnd: true}, true)
	r.BytesRead += n
	return r, err
}

// window says which ends of a read are in the middle of a record. A window cut
// at the start begins half way through one; a window cut at the end stops half
// way through one. Those partial lines are DROPPED rather than counted as
// malformed: they are an artefact of the bound, not a defect in the file, and
// a Malformed count that always reads 1 or 2 is a count nobody looks at.
type window struct {
	cutAtStart bool
	cutAtEnd   bool
}

// scan walks the JSONL lines of one window.
//
// colourOnly is the head fallback, which must not add to the error count: the
// two windows overlap on a file smaller than twice the bound, and a count that
// double-reports is worse than one that says what it covered.
func (t Transcripts) scan(r io.Reader, out *Reading, w window, colourOnly bool) (int64, error) {
	counted := &countingReader{r: r}
	sc := bufio.NewScanner(counted)
	// The same 4MB line budget magmux's own tailer uses. A single record
	// carrying a whole file exceeds bufio's 64KB default, and an oversized
	// token is not skipped — Scan returns false and the rest of the window
	// silently vanishes.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// One line of lookahead, so that the last line of a window cut at the end
	// can be dropped: it is only knowable as "the last one" once Scan says
	// there is no other.
	var (
		held    []byte
		haveOne bool
	)
	first := true
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if first {
			first = false
			if w.cutAtStart {
				continue
			}
		}
		if haveOne {
			t.record(held, out, colourOnly)
		}
		held, haveOne = line, true
	}
	if haveOne && !w.cutAtEnd {
		t.record(held, out, colourOnly)
	}
	if err := sc.Err(); err != nil {
		return counted.n, fmt.Errorf("adapters: reading the transcript: %w", err)
	}
	return counted.n, nil
}

// record folds one whole JSONL line into the reading.
func (t Transcripts) record(line []byte, out *Reading, colourOnly bool) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(line, &rec); err != nil {
		// A record caught half-written by the reader. Saying how many there
		// were is the difference between a quiet zero and a visible one.
		out.Malformed++
		return
	}
	if raw, ok := rec[KeyAgentColor]; ok {
		var c string
		if err := json.Unmarshal(raw, &c); err == nil {
			// Last record wins — confirmed semantics, and 31 of 35 transcripts
			// carry more than one, up to 396. Overwriting rather than keeping
			// the first is the whole rule.
			out.Color = &c
		}
	}
	if colourOnly {
		return
	}
	if raw, ok := rec[KeyIsAPIErrorMessage]; ok {
		var isErr bool
		if err := json.Unmarshal(raw, &isErr); err == nil && isErr {
			out.APIErrors++
			if text := errorText(rec); text != "" {
				out.LastError = text
			}
		}
	}
}

// errorText pulls the human-readable text out of an API-error record. It is
// called only for records already flagged as errors, which is why it may
// decode the whole message: those are rare, and everything else in a window
// gets away with the shallow decode above.
func errorText(rec map[string]json.RawMessage) string {
	raw, ok := rec[KeyMessage]
	if !ok {
		return ""
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var parts []string
	for _, block := range ContentBlocks(msg) {
		if txt, _ := block[KeyBlockText].(string); txt != "" {
			parts = append(parts, strings.TrimSpace(txt))
		}
	}
	return strings.Join(parts, " ")
}

// countingReader is how BytesRead is honest: it counts what was actually
// pulled off the file, rather than what the caller intended to read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Subagents counts a session's subagents FROM FILES, and never from the
// transcript.
//
// The transcript undercounts by 7.2% overall, because a subagent nested at
// spawnDepth 2 leaves no record in its parent's transcript at all. The worst
// measured case is a session whose transcript parses as ZERO against
// twenty-three actual, and that session is in this repository's test data for
// exactly this reason.
//
// Two traps, both of which fail silently:
//
//   - THE DEPTH. The directory is <projects>/<slug>/<sessionId>/subagents/,
//     under the session id and not directly under the project slug. One level
//     too high finds nothing and reports a confident zero.
//   - THE SUFFIX. Only *.meta.json counts. The directory holds other files —
//     notes, placeholders — and counting directory entries inflates the
//     answer.
//
// # A MISSING directory is a determined ZERO, and that was measured
//
// The question Known answers is "were we in a position to look", not "was the
// directory there". A resolved transcript means we were: the directory has one
// possible location, we looked at it, and it is not there.
//
// This was the other way round until it was measured against the live tree,
// read-only, on 2026-09-18:
//
//	transcripts examined          : 3189
//	  subagents/ directory EXISTS : 192   (empty 6, non-empty 186)
//	  subagents/ directory MISSING: 2997  (94.0%)
//
// 186 of the 192 that exist are non-empty, so Claude Code creates the directory
// LAZILY, when it spawns the first subagent. A missing directory is therefore
// the ordinary shape of a session that has never spawned one — not a failure to
// count. Reporting it as undetermined made the count undetermined for 94% of
// sessions, which is not caution: a column that is blank almost always carries
// no information at all, and it left the layer unable to ever say "zero".
//
// The two cases that stay undetermined are the ones where we genuinely could
// not look: no transcript was located, so there is no session directory to look
// IN; or the read failed for a reason other than the directory being absent — a
// permission error is not an observation.
func (t Transcripts) Subagents(loc Location) (Subagents, error) {
	var s Subagents
	if !loc.Found() {
		// No transcript was located, so there is no session directory to look
		// in. Unknown, and emphatically not zero.
		return s, nil
	}
	if err := checkName(loc.SessionID); err != nil {
		return s, err
	}
	dir := filepath.Join(loc.Dir, loc.SessionID, SubagentsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// We looked where Claude Code would have created it. That IS
			// counting; see the measurement above.
			s.Known = true
			return s, nil
		}
		return s, fmt.Errorf("adapters: reading %s: %w", dir, err)
	}
	s.Known = true
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), SubagentMetaSuffix) {
			continue
		}
		s.Count++
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !json.Valid(b) {
			s.Malformed++
		}
	}
	return s, nil
}
