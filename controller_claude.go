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
		for {
			after = strings.TrimLeft(after, " ")
			if after == "" || after[0] != '-' {
				break
			}
			// Skip one flag (and optional value)
			sp := strings.Index(after, " ")
			if sp < 0 {
				break
			}
			after = after[sp+1:]
		}
		// Now `after` starts with the prompt, which may be quoted.
		after = strings.TrimSpace(after)
		if after == "" {
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
	// Resolve cwd. magmux's spawned children inherit magmux's cwd, which is
	// where `task` (or the user) ran the command from — but a pane launched
	// as `cd /foo && claude '...'` writes its transcript under /foo instead,
	// so prefer a cd target parsed out of the command when there is one.
	if c.cwd == "" {
		if dir := extractClaudeCwd(c.pane); dir != "" {
			c.cwd = dir
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

func (c *ClaudeCodeController) Poll() (Snapshot, error) {
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
		c.sessionPath = path
		c.fileSize = 0
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p locked onto session %s\n", c.pane, filepath.Base(path))
		}
	}

	// 2. Stat the file; if it grew, read the new bytes and parse them
	info, err := os.Stat(c.sessionPath)
	if err != nil {
		// Session file disappeared — reset
		c.sessionPath = ""
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
		// New turn starting
		msg, _ := entry["message"].(map[string]any)
		if content, ok := msg["content"].(string); ok {
			c.snap.LastUserPrompt = content
		}
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
