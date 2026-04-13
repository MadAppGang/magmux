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
// pane was launched with; we then scan ~/.claude/projects/<dashed-cwd>/
// for the JSONL file whose first "user" entry matches that prompt. This
// correctly disambiguates multiple Claude panes running in the same cwd.
type ClaudeCodeController struct {
	pane *Pane

	cwd        string    // pane's cwd (used to find the project dir)
	projectDir string    // ~/.claude/projects/<dashed-cwd>
	spawnedAt  time.Time // when the controller was attached
	wantPrompt string    // first user prompt passed to the pane (from cmd.Args)

	sessionPath string // resolved active session file (empty until found)
	fileSize    int64  // last read offset

	snap Snapshot
}

func newClaudeCodeController(p *Pane) *ClaudeCodeController {
	return &ClaudeCodeController{
		pane:       p,
		wantPrompt: extractClaudePrompt(p),
	}
}

// extractClaudePrompt parses the pane's command args for the prompt
// argument to `claude`. Returns "" if the command is `claude` with no
// prompt (interactive mode) or if the prompt can't be determined.
//
// Examples of cmd.Args this handles:
//   ["zsh", "-l", "-c", `claude "say hi"`]
//   ["zsh", "-l", "-c", `cd /foo && claude 'list files'`]
//   ["claude", "say", "hi"]  (unlikely but possible)
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
	// Resolve cwd. magmux's spawned children inherit magmux's cwd,
	// which is where `task` (or the user) ran the command from.
	if c.cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			c.cwd = wd
		}
	}
	// Compute project dir
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dashed := strings.ReplaceAll(c.cwd, "/", "-")
	c.projectDir = filepath.Join(home, ".claude", "projects", dashed)
	c.snap.State = CtrlStarting
	return nil
}

func (c *ClaudeCodeController) Stop() error { return nil }

func (c *ClaudeCodeController) Poll() (Snapshot, error) {
	if c.projectDir == "" {
		return c.snap, nil
	}

	// 1. Resolve the active session file if we don't have one yet
	if c.sessionPath == "" {
		path := c.findActiveSession()
		if path == "" {
			// Not started yet
			return c.snap, nil
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
		return c.snap, nil
	}
	if info.Size() <= c.fileSize {
		return c.snap, nil
	}

	f, err := os.Open(c.sessionPath)
	if err != nil {
		return c.snap, err
	}
	defer f.Close()

	if _, err := f.Seek(c.fileSize, io.SeekStart); err != nil {
		return c.snap, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 4MB max line
	for scanner.Scan() {
		c.applyLine(scanner.Bytes())
	}
	c.fileSize = info.Size()
	return c.snap, nil
}

// findActiveSession returns the path of the JSONL file that belongs to
// THIS pane's claude process. Matching strategy:
//   1. Prefer files whose first user prompt matches c.wantPrompt exactly.
//   2. Fall back to "most recently modified, not already claimed" for
//      panes where wantPrompt is empty (interactive mode).
// Files already claimed by sibling controllers are skipped. Returns ""
// if no matching file is found yet (we'll retry on the next poll).
func (c *ClaudeCodeController) findActiveSession() string {
	entries, err := os.ReadDir(c.projectDir)
	if err != nil {
		return ""
	}

	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Must have been touched at or after the spawn time
		if info.ModTime().Before(c.spawnedAt) {
			continue
		}
		fullPath := filepath.Join(c.projectDir, e.Name())
		// Skip sessions already owned by sibling panes
		if c.pane != nil && c.pane.mux != nil &&
			c.pane.mux.isSessionClaimed(fullPath, c.pane) {
			continue
		}
		candidates = append(candidates, candidate{fullPath, info.ModTime()})
	}

	// Strategy 1: match by prompt content (exact match on first user message)
	if c.wantPrompt != "" {
		for _, cand := range candidates {
			if readFirstUserPrompt(cand.path) == c.wantPrompt {
				if c.pane != nil && c.pane.mux != nil {
					if !c.pane.mux.claimSession(cand.path, c.pane) {
						continue
					}
				}
				return cand.path
			}
		}
		// No match yet — the target session file may not have been written
		// to disk yet. Retry next poll.
		return ""
	}

	// Strategy 2: fall back to most-recent mtime (interactive mode)
	var best string
	var bestMtime time.Time
	for _, cand := range candidates {
		if best == "" || cand.mtime.After(bestMtime) {
			best = cand.path
			bestMtime = cand.mtime
		}
	}
	if best == "" {
		return ""
	}
	if c.pane != nil && c.pane.mux != nil {
		if !c.pane.mux.claimSession(best, c.pane) {
			return ""
		}
	}
	return best
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
