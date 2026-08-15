package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ClaudeCodeController observes a Claude Code session by reading its JSONL
// transcript file. Discovery: at attach time we capture the prompt the
// pane was launched with; we then scan ~/.claude/projects/<encoded-cwd>/
// for the JSONL file whose first "user" entry matches that prompt. This
// correctly disambiguates multiple Claude panes running in the same cwd.
//
// The encoded-cwd directory name is an undocumented cross-process contract
// owned by Claude Code, so we never let it be the only way in: if the
// computed directory is missing or holds no match, discovery widens to
// every directory under ~/.claude/projects and matches on transcript
// content instead (see findActiveSession). That keeps a change to Claude
// Code's encoding — or a pane whose real cwd differs from magmux's, as in
// `cd /foo && claude '...'` — from silently stranding us in CtrlStarting.
type ClaudeCodeController struct {
	pane *Pane

	cwd         string    // pane's cwd (used to find the project dir)
	projectDir  string    // ~/.claude/projects/<encoded-cwd>
	projectsDir string    // ~/.claude/projects (root, for the widened scan)
	spawnedAt   time.Time // when the controller was attached
	wantPrompt  string    // first user prompt passed to the pane (from cmd.Args)

	sessionPath string // resolved active session file (empty until found)
	fileSize    int64  // last read offset

	// preexisting holds the transcripts that were already on disk the first
	// time we scanned. Our pane's claude creates a *new* file shortly after
	// spawn, so anything in this set belongs to some other session — the
	// concurrent-session mix-up described in issue #3.
	preexisting map[string]bool
	baselined   map[string]bool // dirs whose baseline has been captured
	lastBroadAt time.Time       // last widened scan (throttled)
	lastApplyAt time.Time       // wall time we last applied a transcript entry

	// injected is set by NotifyInput when a pilot pushes an instruction into
	// the pane, and consumed by the next Poll. An atomic rather than a lock
	// because NotifyInput runs on the socket goroutine while everything else
	// in this controller runs on the render goroutine; a flag keeps that the
	// only shared state between them.
	injected atomic.Bool

	// published is sessionPath republished for OTHER goroutines. Transcript is
	// served on the socket goroutine while everything else here runs on the
	// render goroutine, and sessionPath is written by that render goroutine on
	// every lock-on and every reset — so reading the field itself from the
	// socket would be a plain data race.
	//
	// It is a copy rather than a lock for the same reason `injected` is: it
	// keeps the ONE piece of state shared between the two goroutines explicit,
	// and it means Transcript takes nothing at all before doing file I/O,
	// which is what stops a slow disk from stalling a frame. Publishing is
	// one-way — the render goroutine writes, everyone else reads — and the
	// worst a racing reader can see is the previous path, which it then reads
	// from disk and finds unchanged or gone. Both are honest answers.
	published atomic.Pointer[string]

	snap Snapshot
}

const (
	// broadScanInterval throttles the widened scan over every project
	// directory. A full walk of a large ~/.claude/projects (200+ dirs, 3k
	// entries) measures ~15ms, and pollControllers runs on the render
	// goroutine, so once a second keeps it under 2% duty.
	broadScanInterval = time.Second

	// broadScanWindow bounds how long we keep widening. Claude Code writes
	// its first transcript entry within a second or two of starting; if
	// nothing has matched in a minute, nothing will, and a pane that never
	// locks on should not walk every project directory for the rest of the
	// session. The cheap per-poll scan of the computed directory continues
	// regardless.
	broadScanWindow = time.Minute
)

func newClaudeCodeController(p *Pane) *ClaudeCodeController {
	return &ClaudeCodeController{
		pane:       p,
		wantPrompt: extractClaudePrompt(p),
	}
}

// encodeProjectDir converts a working directory into the directory name
// Claude Code files its transcripts under in ~/.claude/projects.
//
// Claude Code replaces every character outside [A-Za-z0-9] with '-', not
// just the path separator. Verified against the `cwd` recorded inside real
// transcripts: `/Users/x/mag/babble_ml` is filed as `-Users-x-mag-babble-ml`
// and `/Users/x/Models bioses` as `-Users-x-Models-bioses`. Replacing only
// '/' leaves a literal dot (or underscore, or space) in the name, no such
// directory exists, and discovery fails silently.
func encodeProjectDir(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// extractClaudePrompt parses the pane's command args for the prompt
// argument to `claude`. Returns "" if the command is `claude` with no
// prompt (interactive mode) or if the prompt can't be determined.
//
// Examples of cmd.Args this handles:
//
//	["zsh", "-l", "-c", `claude "say hi"`]
//	["zsh", "-l", "-c", `cd /foo && claude 'list files'`]
//	["claude", "say", "hi"]  (unlikely but possible)
func extractClaudePrompt(p *Pane) string {
	if p.cmd == nil {
		return ""
	}
	// Walk cmd.Args looking for a claude invocation with a quoted prompt.
	for _, arg := range p.cmd.Args {
		if !strings.Contains(arg, "claude") {
			continue
		}
		// Find the substring after `claude ` (with any number of flags before the prompt)
		idx := strings.Index(arg, "claude ")
		if idx < 0 {
			continue
		}
		after := arg[idx+len("claude "):]
		// Skip any flags (starting with -) and their values
		sawFlag := false
		for {
			after = strings.TrimLeft(after, " ")
			if after == "" || after[0] != '-' {
				break
			}
			sawFlag = true
			// Skip one flag (and optional value)
			sp := strings.Index(after, " ")
			if sp < 0 {
				// A trailing flag with nothing after it, e.g.
				// `claude --dangerously-skip-permissions`. There is no
				// prompt — this is interactive mode.
				//
				// Breaking here instead would leave the flag itself in
				// `after` and return it as the prompt. That is not a
				// cosmetic slip: a non-empty wantPrompt sends discovery
				// down the match-by-prompt path, hunting for a transcript
				// whose first user message is literally
				// "--dangerously-skip-permissions". Nothing ever matches,
				// the mtime fallback interactive panes rely on is never
				// reached, and the controller silently never locks on — so
				// model, response and tool stay empty forever while the
				// pane looks fine on screen.
				after = ""
				break
			}
			after = after[sp+1:]
		}
		// Now `after` starts with the prompt, which may be quoted.
		after = strings.TrimSpace(after)
		if after == "" || after[0] == '-' {
			continue
		}
		// Strip surrounding quotes if present
		if (after[0] == '"' || after[0] == '\'') && len(after) > 1 {
			quote := after[0]
			// Find matching close quote
			end := strings.IndexByte(after[1:], quote)
			if end >= 0 {
				return after[1 : 1+end]
			}
		}
		// Unquoted, and we skipped at least one flag on the way here: this
		// token is more likely that flag's value than a prompt. We have no
		// table of which claude flags take values, and cannot get one — it
		// is someone else's CLI. So prefer to claim no prompt.
		//
		// The two outcomes are not symmetric. An empty wantPrompt falls back
		// to mtime discovery, which is correct for an interactive pane. A
		// *wrong* wantPrompt (e.g. "opus" from `claude --model opus`) sends
		// discovery hunting for a transcript that cannot exist, and the
		// controller never locks on at all. Guessing costs far more than
		// abstaining.
		if sawFlag {
			continue
		}
		// Unquoted: take up to end of line or next shell operator
		for _, delim := range []string{";", "&&", "||", " | ", " 2>", " >"} {
			if i := strings.Index(after, delim); i >= 0 {
				after = after[:i]
			}
		}
		return strings.TrimSpace(after)
	}
	return ""
}

func (c *ClaudeCodeController) Name() string { return "claude-code" }

func (c *ClaudeCodeController) Start(ctx context.Context) error {
	if c.spawnedAt.IsZero() {
		c.spawnedAt = time.Now()
	}
	// Resolve cwd, in descending order of how directly it states where claude
	// will actually run:
	//
	//  1. a `cd /foo &&` prefix parsed out of the command;
	//  2. the pane's own cmd.Dir — set by open_pane's cwd. Without this step a
	//     dynamically opened claude pane silently regresses to the slow
	//     scan-every-project fallback, because os.Getwd() is magmux's
	//     directory and no longer has anything to do with this pane;
	//  3. os.Getwd(), the inherited default.
	//
	// Content-matching remains the last resort, exactly as before — cmd.Dir is
	// only a better first guess, never the only way in.
	if c.cwd == "" {
		if dir := extractClaudeCwd(c.pane); dir != "" {
			c.cwd = dir
		} else if c.pane != nil && c.pane.cmd != nil && c.pane.cmd.Dir != "" {
			c.cwd = c.pane.cmd.Dir
		} else if wd, err := os.Getwd(); err == nil {
			c.cwd = wd
		}
	}
	// Compute project dir
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c.projectsDir = filepath.Join(home, ".claude", "projects")
	c.projectDir = filepath.Join(c.projectsDir, encodeProjectDir(c.cwd))
	c.snap.State = CtrlStarting
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p cwd=%q projectDir=%q exists=%v wantPrompt=%q\n",
			c.pane, c.cwd, c.projectDir, dirExists(c.projectDir), c.wantPrompt)
	}
	return nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// extractClaudeCwd parses a leading `cd <dir> &&` out of the pane's command
// so the controller looks under the directory claude will actually run in.
// Returns "" when the command has no cd prefix, when the target is relative
// (we cannot resolve it reliably against the shell's own state), or when the
// directory does not exist.
func extractClaudeCwd(p *Pane) string {
	if p == nil || p.cmd == nil {
		return ""
	}
	for _, arg := range p.cmd.Args {
		idx := strings.Index(arg, "cd ")
		if idx != 0 && !(idx > 0 && (arg[idx-1] == ' ' || arg[idx-1] == ';')) {
			continue
		}
		rest := strings.TrimSpace(arg[idx+len("cd "):])
		end := strings.Index(rest, "&&")
		if end < 0 {
			continue
		}
		dir := strings.TrimSpace(rest[:end])
		dir = strings.Trim(dir, `"'`)
		if !strings.HasPrefix(dir, "/") || !dirExists(dir) {
			continue
		}
		return dir
	}
	return ""
}

func (c *ClaudeCodeController) Stop() error { return nil }

// NotifyInput records that a pilot pushed an instruction into this pane.
// See the InputNotifier docs for why a controller needs to be told.
func (c *ClaudeCodeController) NotifyInput() {
	c.injected.Store(true)
}

// consumeInjected demotes a settled snapshot back to working when an
// instruction has been injected since the last poll.
//
// Without this the state is one-way: applyTerminalIdle promotes to
// awaiting_input and only a transcript entry can move it off again. Claude
// Code normally writes the submitted prompt to its transcript straight away,
// which would do the job — but transcript discovery can lag or fail outright
// (that is why the widened scan exists), and in that case a pilot would sit
// waiting for a turn that had already started.
//
// Deliberately not sticky: if the tool ignores the instruction, the ordinary
// idle heuristics settle the pane again and applyTerminalIdle promotes it
// back, so a dropped instruction surfaces as a fast empty turn rather than a
// permanent "working".
func (c *ClaudeCodeController) consumeInjected() {
	if !c.injected.Swap(false) {
		return
	}
	switch c.snap.State {
	case CtrlAwaitingInput, CtrlAwaitingPermission, CtrlError:
		c.snap.State = CtrlWorking
		c.snap.StartedAt = time.Now()
		c.snap.CompletedAt = time.Time{}
		c.snap.LastTool = ""
		// A new turn must not inherit the previous turn's answer. The
		// transcript's own user entry clears this too (applyLine), but this
		// path exists precisely for when the transcript is lagging or was
		// never found — and pollControllers emits `response` on every
		// snapshot, so anything left here is broadcast as if the new turn had
		// said it. A tool-only turn would then re-settle still carrying it.
		c.snap.LastResponse = ""
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p input injected → working\n", c.pane)
		}
	}
}

func (c *ClaudeCodeController) Poll() (Snapshot, error) {
	// Runs first: an injected instruction starts a new turn, so it must not
	// be overridden by transcript state left over from the previous one.
	c.consumeInjected()
	err := c.pollTranscript()
	// Reconcile with the terminal's own idle detection. Runs even when the
	// transcript was never found, so the live snapshot can still reach
	// awaiting_input (issue #2).
	c.applyTerminalIdle()
	return c.snap, err
}

// applyTerminalIdle promotes the snapshot to CtrlAwaitingInput when the pane
// itself has detected that its child is idle at a prompt.
//
// The transcript only announces "turn finished" via a stop_hook_summary
// entry, which Claude Code emits only when a Stop hook is configured. A pane
// can therefore sit at CtrlStarting indefinitely while it is visibly idle —
// and if transcript discovery failed outright, forever. The pane's own idle
// detection (OSC 9 notification, bracketed-paste cycle, window title, text
// idle) already knows better, and buildPaneResults has always reported from
// it. Promoting here is what stops the live `snapshot` event and the final
// `results` event from disagreeing.
//
// Promotion is one-way and ordered: it applies only when the terminal went
// idle *after* the last transcript entry we consumed. A new turn arriving on
// the transcript is therefore authoritative and the state cannot flap
// between working and awaiting_input.
func (c *ClaudeCodeController) applyTerminalIdle() {
	if c.pane == nil {
		return
	}
	switch c.snap.State {
	case CtrlAwaitingInput, CtrlAwaitingPermission, CtrlError, CtrlGone:
		return // already settled; nothing to promote
	}

	c.pane.mu.Lock()
	ready, signal, readyAt := c.pane.inputReady, c.pane.inputSignal, c.pane.inputReadyAt
	c.pane.mu.Unlock()

	// "ctrl"/"perm" are set *by* a controller snapshot — reading them back
	// would be a feedback loop, not evidence.
	if !ready || signal == "ctrl" || signal == "perm" {
		return
	}
	// The transcript moved at or after the terminal went idle, so the
	// transcript is the fresher signal. Leave the state alone.
	if !readyAt.After(c.lastApplyAt) {
		return
	}

	c.snap.State = CtrlAwaitingInput
	if c.snap.CompletedAt.IsZero() {
		c.snap.CompletedAt = readyAt
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p terminal idle (%s) → awaiting_input\n",
			c.pane, signal)
	}
}

func (c *ClaudeCodeController) pollTranscript() error {
	if c.projectDir == "" {
		return nil
	}

	// 1. Resolve the active session file if we don't have one yet
	if c.sessionPath == "" {
		path := c.findActiveSession()
		if path == "" {
			// Not started yet
			return nil
		}
		c.setSessionPath(path)
		c.fileSize = 0
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p locked onto session %s\n", c.pane, filepath.Base(path))
		}
	}

	// 2. Stat the file; if it grew, read the new bytes and parse them
	info, err := os.Stat(c.sessionPath)
	if err != nil {
		// Session file disappeared — reset
		c.setSessionPath("")
		c.fileSize = 0
		return nil
	}
	if info.Size() <= c.fileSize {
		return nil
	}

	f, err := os.Open(c.sessionPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(c.fileSize, io.SeekStart); err != nil {
		return err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 4MB max line
	for scanner.Scan() {
		c.applyLine(scanner.Bytes())
		// Record that the transcript advanced, so a terminal idle signal
		// from *before* this entry cannot override it.
		c.lastApplyAt = time.Now()
	}
	c.fileSize = info.Size()
	return nil
}

// setSessionPath records the transcript this controller is tailing and
// republishes it for readers on other goroutines. Every write to sessionPath
// goes through here — a bare assignment would leave Transcript serving the
// previous session's file, or a file that has since been deleted, with nothing
// to say it had gone stale.
//
// Caller is the render goroutine (pollTranscript).
func (c *ClaudeCodeController) setSessionPath(path string) {
	c.sessionPath = path
	c.published.Store(&path)
}

// discoveredSession returns the transcript path for a reader on any goroutine,
// or "" if discovery has not succeeded (yet, or at all).
func (c *ClaudeCodeController) discoveredSession() string {
	if p := c.published.Load(); p != nil {
		return *p
	}
	return ""
}

// sessionCandidate is one transcript that could belong to this pane.
type sessionCandidate struct {
	path  string
	mtime time.Time
	fresh bool // file appeared after we started scanning (not pre-existing)
}

// findActiveSession returns the path of the JSONL file that belongs to
// THIS pane's claude process. Matching strategy:
//  1. Prefer files whose first user prompt matches c.wantPrompt exactly.
//     If the encoded-cwd directory yields no match, widen the search to
//     every project directory — content matching is exact, so a wider net
//     costs nothing in precision and makes the directory name, an
//     undocumented contract we do not control, non-load-bearing.
//  2. For panes with no prompt (interactive mode) fall back to mtime, but
//     prefer transcripts that appeared *after* we attached over ones that
//     were already on disk. A concurrent claude session in the same cwd is
//     pre-existing, so this keeps us off its transcript (issue #3).
//
// Files already claimed by sibling controllers are skipped. Returns ""
// if no matching file is found yet (we'll retry on the next poll).
func (c *ClaudeCodeController) findActiveSession() string {
	candidates := c.collectCandidates(c.projectDir)

	if c.wantPrompt != "" {
		if path := c.matchByPrompt(candidates); path != "" {
			return path
		}
		// The computed directory held nothing matching. Either claude has
		// not flushed the transcript yet (retry next poll is enough), or the
		// directory is wrong — a dotted/underscored cwd we mis-encoded, or a
		// pane that cd'd elsewhere. Widen the net, throttled.
		if time.Since(c.lastBroadAt) < broadScanInterval ||
			(!c.spawnedAt.IsZero() && time.Since(c.spawnedAt) > broadScanWindow) {
			return ""
		}
		c.lastBroadAt = time.Now()
		return c.matchByPrompt(c.collectAllCandidates())
	}

	// Strategy 2: interactive mode. Prefer a transcript that appeared after
	// we attached; only fall back to a pre-existing one if there is none.
	var best sessionCandidate
	for _, cand := range candidates {
		if best.path == "" ||
			(cand.fresh && !best.fresh) ||
			(cand.fresh == best.fresh && cand.mtime.After(best.mtime)) {
			best = cand
		}
	}
	if best.path == "" {
		return ""
	}
	if c.pane != nil && c.pane.mux != nil {
		if !c.pane.mux.claimSession(best.path, c.pane) {
			return ""
		}
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p strategy=mtime fresh=%v %s\n",
			c.pane, best.fresh, filepath.Base(best.path))
	}
	return best.path
}

// matchByPrompt returns the first candidate whose opening user message is
// exactly c.wantPrompt and that we can claim. Returns "" if none match.
func (c *ClaudeCodeController) matchByPrompt(candidates []sessionCandidate) string {
	for _, cand := range candidates {
		if readFirstUserPrompt(cand.path) != c.wantPrompt {
			continue
		}
		if c.pane != nil && c.pane.mux != nil {
			if !c.pane.mux.claimSession(cand.path, c.pane) {
				continue
			}
		}
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p strategy=prompt %s\n",
				c.pane, cand.path)
		}
		return cand.path
	}
	return ""
}

// collectCandidates lists the unclaimed transcripts in dir that were
// touched at or after this controller attached.
func (c *ClaudeCodeController) collectCandidates(dir string) []sessionCandidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if dbgFile != nil && c.sessionPath == "" {
			fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p readdir %q failed: %v\n", c.pane, dir, err)
		}
		return nil
	}
	// The first scan of each directory establishes its baseline: everything
	// already on disk there belongs to a session that started before us.
	if c.preexisting == nil {
		c.preexisting = make(map[string]bool)
		c.baselined = make(map[string]bool)
	}
	first := !c.baselined[dir]
	c.baselined[dir] = true

	var candidates []sessionCandidate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		fullPath := filepath.Join(dir, e.Name())
		if first {
			c.preexisting[fullPath] = true
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Must have been touched at or after the spawn time
		if info.ModTime().Before(c.spawnedAt) {
			continue
		}
		// Skip sessions already owned by sibling panes
		if c.pane != nil && c.pane.mux != nil &&
			c.pane.mux.isSessionClaimed(fullPath, c.pane) {
			continue
		}
		candidates = append(candidates, sessionCandidate{
			path:  fullPath,
			mtime: info.ModTime(),
			fresh: !c.preexisting[fullPath],
		})
	}
	return candidates
}

// collectAllCandidates walks every project directory. Only used as a
// fallback for prompt-matched discovery, where an exact content match
// makes a wide search safe.
func (c *ClaudeCodeController) collectAllCandidates() []sessionCandidate {
	if c.projectsDir == "" {
		return nil
	}
	dirs, err := os.ReadDir(c.projectsDir)
	if err != nil {
		return nil
	}
	var all []sessionCandidate
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		full := filepath.Join(c.projectsDir, d.Name())
		if full == c.projectDir {
			continue // already scanned
		}
		all = append(all, c.collectCandidates(full)...)
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p widened scan: %d dirs, %d candidates\n",
			c.pane, len(dirs), len(all))
	}
	return all
}

// readFirstUserPrompt returns the content of the first `type:"user"`
// entry in the JSONL file. Returns "" if the file has no user entry yet
// (still initializing) or any error occurs.
func readFirstUserPrompt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if t, _ := entry["type"].(string); t != "user" {
			continue
		}
		msg, _ := entry["message"].(map[string]any)
		if content, ok := msg["content"].(string); ok {
			return content
		}
		return ""
	}
	return ""
}

// ── turn history ────────────────────────────────────────────────────────────
//
// Snapshot answers "what is this session doing"; the code below answers "what
// did it actually say and do". Both read the same file, found the same way:
// Transcript serves whatever path pollTranscript locked onto, so the two can
// never disagree about which session belongs to this pane. What they cannot
// share is the PARSE — the tailer is an incremental state machine that keeps
// only the latest of each field, and history needs whole turns with their tool
// inputs and results. The one rule they must not diverge on, "a `user` entry
// with array content is a tool RESULT and not a prompt", is therefore factored
// into claudePromptText and called from both.

// transcriptByteBudget bounds the memory ONE Transcript call holds while it
// parses. It is not truncation: no field is ever shortened, but once the
// retained turns exceed the budget the oldest of them are dropped, so a caller
// asking for 50 turns of a session that pasted a 40MB file into a tool result
// gets the newest turns that fit rather than the machine's memory.
const transcriptByteBudget = 2 << 20 // 2MB

// Transcript returns the last `turns` turns of this session's own JSONL
// record, oldest first.
//
// It RE-READS the file on every call rather than retaining turns as we tail.
// Retaining would be faster and is the wrong trade here: steady-state memory
// stays flat for a pane nobody ever asks about (the common case — most panes
// are never read), the tailer's hot path stays a state machine over the last
// few bytes rather than a growing ring, and a re-read is correct across the
// rewrites the tailer cannot see. Claude Code compacts, edits and replays its
// transcripts; a retained ring would still be serving turns the file no longer
// contains. The cost is one pass over a file we would have had to open anyway.
//
// Safe on any goroutine: it reads one atomically published path and then owns
// its own file handle. It holds no lock at all, and therefore none across the
// I/O.
func (c *ClaudeCodeController) Transcript(turns int) ([]Turn, error) {
	path := c.discoveredSession()
	if path == "" {
		return nil, errNoTranscript
	}
	f, err := os.Open(path)
	if err != nil {
		// Located once, unreadable now: rotated, compacted, or deleted out
		// from under us. That is still "we cannot reach the record", not "the
		// session said nothing", so it stays the same class of error.
		return nil, fmt.Errorf("%w: %v", errNoTranscript, err)
	}
	defer f.Close()
	return parseClaudeTurns(f, normalizeTranscriptTurns(turns), transcriptByteBudget)
}

// parseClaudeTurns reads a Claude Code JSONL transcript and returns its last
// maxTurns turns, oldest first, holding no more than byteBudget bytes of them.
//
// Split out from Transcript so it can be tested against a reader with no file,
// no controller and no discovery — this is the part most likely to rot, since
// the JSONL shape belongs to Claude Code and can change without notice.
func parseClaudeTurns(r io.Reader, maxTurns, byteBudget int) ([]Turn, error) {
	sc := bufio.NewScanner(r)
	// Same 4MB budget as the tailer: a single tool result carrying a whole file
	// comfortably exceeds bufio's 64KB default, and an oversized token does not
	// get skipped — Scan returns false and the rest of the transcript silently
	// vanishes.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		turns []Turn
		sizes []int // bytes retained per turn, parallel to turns
		total int
		// open reports whether the last retained turn is an assistant turn
		// still being appended to. Claude Code writes one assistant reply as
		// several entries; they are one turn.
		open bool
	)
	// trim drops the OLDEST turns until the request's bounds are met, always
	// keeping at least one: a caller who asked for history and whose newest
	// turn alone blows the budget still wants that turn.
	trim := func() {
		for len(turns) > 1 && (len(turns) > maxTurns || total > byteBudget) {
			total -= sizes[0]
			turns, sizes = turns[1:], sizes[1:]
			if len(turns) == 0 {
				open = false
			}
		}
	}
	push := func(t Turn, size int) {
		turns = append(turns, t)
		sizes = append(sizes, size)
		total += size
		trim()
	}
	grow := func(i, n int) {
		if i < 0 || i >= len(sizes) || n == 0 {
			return
		}
		sizes[i] += n
		total += n
		trim()
	}

	for sc.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			continue // a partially flushed line; the next poll sees it whole
		}
		msg, _ := entry["message"].(map[string]any)

		switch t, _ := entry["type"].(string); t {
		case "user":
			if text, isPrompt := claudePromptText(msg); isPrompt {
				open = false
				push(Turn{
					Role:      TurnUser,
					Text:      text,
					Timestamp: parseClaudeTimestamp(entry["timestamp"]),
				}, len(text))
				continue
			}
			// Not a prompt: a tool RESULT, which Claude Code also files as a
			// `user` entry. It belongs to the call it answers, not to a turn of
			// its own — see the note in applyLine on what treating it as a turn
			// boundary costs.
			for _, block := range claudeContentBlocks(msg) {
				if bt, _ := block["type"].(string); bt != "tool_result" {
					continue
				}
				id, _ := block["tool_use_id"].(string)
				text := claudeToolResultText(block["content"])
				if i := attachToolResult(turns, id, text); i >= 0 {
					grow(i, len(text))
				}
			}

		case "assistant":
			if !open {
				push(Turn{
					Role:      TurnAssistant,
					Timestamp: parseClaudeTimestamp(entry["timestamp"]),
				}, 0)
				open = true
			}
			cur := &turns[len(turns)-1]
			added := 0
			for _, block := range claudeContentBlocks(msg) {
				switch bt, _ := block["type"].(string); bt {
				case "text":
					txt, _ := block["text"].(string)
					txt = strings.TrimSpace(txt)
					if txt == "" {
						continue
					}
					if cur.Text != "" {
						cur.Text += "\n\n"
						added += 2
					}
					cur.Text += txt
					added += len(txt)
				case "tool_use":
					name, _ := block["name"].(string)
					id, _ := block["id"].(string)
					input := claudeToolInput(block["input"])
					cur.Tools = append(cur.Tools, ToolCall{Name: name, Input: input, id: id})
					added += len(name) + len(input)
				}
			}
			grow(len(turns)-1, added)
		}
	}
	if err := sc.Err(); err != nil {
		// Whatever was parsed before the failure is real and is returned with
		// the error: a caller that can show six of eight turns and say why the
		// rest are missing is strictly better off than one handed nothing.
		return turns, err
	}
	return turns, nil
}

// attachToolResult files a tool result against the call it answers and returns
// the index of the turn it landed in, or -1.
//
// Matching is by tool_use_id, searching backwards because a result is always
// answering a recent call. The id-less fallback exists because the id is
// Claude Code's field and not ours: without it a result would be dropped
// silently, and "the tool ran and returned nothing" is exactly the wrong thing
// to tell a driver.
func attachToolResult(turns []Turn, id, text string) int {
	for i := len(turns) - 1; i >= 0; i-- {
		for j := len(turns[i].Tools) - 1; j >= 0; j-- {
			call := &turns[i].Tools[j]
			if id != "" {
				if call.id != id {
					continue
				}
			} else if call.Result != "" {
				continue // answered already; the open call is the one we want
			}
			call.Result = text
			return i
		}
	}
	return -1
}

// claudePromptText reports whether a `user` entry is a REAL prompt, and
// returns it if so.
//
// The rule — string content is a prompt, array content is a tool result — is
// an undocumented shape of Claude Code's record, and both readers in this file
// depend on it for different reasons: the tailer must not treat a tool result
// as a turn boundary, and the history parser must not file one as a turn. One
// function so they cannot drift apart.
func claudePromptText(msg map[string]any) (string, bool) {
	content, ok := msg["content"].(string)
	return content, ok
}

// claudeContentBlocks returns the content blocks of a message, or nil when the
// content is a bare string (a prompt) or missing.
func claudeContentBlocks(msg map[string]any) []map[string]any {
	raw, _ := msg["content"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		if block, ok := b.(map[string]any); ok {
			out = append(out, block)
		}
	}
	return out
}

// claudeToolInput renders a tool_use input as the JSON it was recorded as.
// Kept as text rather than as a nested object: it crosses a socket and an MCP
// boundary on its way to a model, and a model reads `{"command":"ls -la"}`
// exactly as well as a decoded map while costing every hop less.
func claudeToolInput(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// claudeToolResultText flattens a tool_result's content, which Claude Code
// writes as a plain string for simple tools and as a block array for the ones
// that return structured output.
func claudeToolResultText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if txt, ok := block["text"].(string); ok && txt != "" {
				parts = append(parts, txt)
				continue
			}
			// A non-text block (an image, say). Naming it beats dropping it:
			// "the tool returned something we did not render" is information.
			if bt, ok := block["type"].(string); ok && bt != "" {
				parts = append(parts, "["+bt+"]")
			}
		}
		return strings.Join(parts, "\n")
	default:
		return claudeToolInput(v)
	}
}

// applyLine parses one JSONL entry and updates the snapshot state machine.
func (c *ClaudeCodeController) applyLine(line []byte) {
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		return
	}

	// Project name: derive from cwd of any entry that has one
	if c.snap.Project == "" {
		if cwd, ok := entry["cwd"].(string); ok && cwd != "" {
			c.snap.Project = filepath.Base(cwd)
		}
	}

	t, _ := entry["type"].(string)
	switch t {
	case "user":
		// Claude Code files tool RESULTS as "user" entries too, with an array
		// content instead of a string. Only a string is a real prompt, and
		// only a real prompt starts a new turn.
		//
		// Treating a tool result as a turn boundary resets the turn mid-flight:
		// StartedAt jumps forward (so the reported duration is the time since
		// the last tool, not since the prompt), and LastResponse/LastTool are
		// wiped. The response usually reappears from the closing assistant
		// message, but the tool does not — which is why a turn that plainly ran
		// Bash reported no tool at all.
		msg, _ := entry["message"].(map[string]any)
		content, isPrompt := claudePromptText(msg)
		if !isPrompt {
			return
		}
		// New turn starting
		c.snap.LastUserPrompt = content
		c.snap.State = CtrlWorking
		c.snap.StartedAt = parseClaudeTimestamp(entry["timestamp"])
		c.snap.CompletedAt = time.Time{}
		c.snap.LastResponse = ""
		c.snap.LastTool = ""

	case "assistant":
		msg, _ := entry["message"].(map[string]any)
		if model, ok := msg["model"].(string); ok && model != "" {
			c.snap.Model = model
		}
		// content is an array of blocks: text, tool_use, etc.
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			bt, _ := block["type"].(string)
			switch bt {
			case "text":
				if txt, ok := block["text"].(string); ok && strings.TrimSpace(txt) != "" {
					c.snap.LastResponse = strings.TrimSpace(txt)
				}
			case "tool_use":
				if name, ok := block["name"].(string); ok {
					c.snap.LastTool = name
				}
			}
		}
		// Don't transition state here — wait for stop_hook_summary

	case "system":
		subtype, _ := entry["subtype"].(string)
		if subtype == "stop_hook_summary" {
			c.snap.State = CtrlAwaitingInput
			c.snap.CompletedAt = parseClaudeTimestamp(entry["timestamp"])
		}

	case "permission-mode":
		// Permission mode change — informational only
	}
}

// parseClaudeTimestamp parses an ISO-8601 timestamp string from a JSONL
// entry. Returns zero time if the value is missing or malformed.
func parseClaudeTimestamp(v any) time.Time {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// claudeCodeFactory returns a ToolController for panes running `claude`.
func claudeCodeFactory(p *Pane) ToolController {
	if p.cmd == nil || p.cmd.Path == "" {
		return nil
	}
	// Look at args[0] (the actual program name) and args[len-1] (often
	// `-l -c "claude ..."` from zsh -c). Match either.
	base := filepath.Base(p.cmd.Path)
	if base == "claude" {
		return newClaudeCodeController(p)
	}
	// Magmux wraps gridfile/`-e` commands as `zsh -l -c "..."`. Inspect
	// the wrapped command for a leading `claude`.
	for _, arg := range p.cmd.Args {
		if strings.Contains(arg, "claude ") || strings.HasSuffix(arg, "claude") || strings.HasPrefix(arg, "claude ") {
			return newClaudeCodeController(p)
		}
	}
	return nil
}

// Suppress unused warning for the helper.
var _ = fmt.Sprintf
