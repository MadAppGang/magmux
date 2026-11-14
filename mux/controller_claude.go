package mux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MadAppGang/magmux/claudestate/claude"
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

	// bindID and bindPath are the DECLARED binding (claude_binding.go): the
	// session id and, when the caller gave it, the exact transcript path.
	// Write-once at construction. With bindID set, discovery never runs.
	bindID   string
	bindPath string
	// boundBy is how discovery found sessionPath (bindPrompt / bindMtime),
	// published for `list`.
	boundBy atomic.Pointer[string]

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

	// The InputGate's state (controller_claude_input.go). Every field is safe
	// from any goroutine: tty takes its own leaf lock, the rest are atomics.
	//
	// tty is nil until Start, and Start builds it only for a GATED pane, so an
	// observed-only Claude pane costs exactly what it did before. Settable in
	// struct-literal tests.
	tty       *ttyWatch
	reg       claudeRegistry
	everReady atomic.Bool  // has ever been READY: "starting" vs a regression
	boxSince  atomic.Int64 // unix nanos the box became continuously visible; 0 = not visible
	// prompts is the recent transcript PROMPT records (real user entries,
	// queue enqueues, queued_command attachments), published for acceptance.
	// queue is the render goroutine's own list of enqueued prompts not yet
	// dequeued, which dates a dequeued prompt by its ENQUEUE (F11).
	prompts atomic.Pointer[promptLog]
	queue   []queuedPrompt

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
	c := &ClaudeCodeController{
		pane:       p,
		wantPrompt: extractClaudePrompt(p),
	}
	if p != nil {
		c.bindID, c.bindPath = p.bindID, p.bindPath
	}
	return c
}

// encodeProjectDir converts the directory a session was LAUNCHED in into the
// directory name Claude Code files its transcripts under in ~/.claude/projects.
//
// # There is one encoder now, and this is not it
//
// This function had its own implementation until phase 4 of the observation
// layer, and that implementation was wrong in two independent ways — both
// measured, both silent. It replaced RUNES where Claude Code replaces UTF-16
// CODE UNITS, so a character outside the basic multilingual plane produced one
// dash where Claude writes two; and it had no 200-character truncate-and-hash
// branch at all, so any launch directory whose encoded form exceeds 200
// characters got a name Claude Code never wrote. The second was latent on this
// machine — 347 real project directories, the longest 149 characters — but a
// deep path fires it.
//
// The body is now a call to claude.EncodeProjectDir (claudestate), which is the algorithm
// extracted verbatim from the Claude Code 2.1.273 bundle and pinned against
// 20,019 vectors generated by running that JavaScript. Two implementations of
// an undocumented external contract is exactly the second copy NFR6 exists to
// prevent, and this one was already wrong.
//
// # Watch the input, not just the algorithm
//
// The adapter's parameter is the LAUNCH directory. Feeding a correct encoder a
// session's LIVE working directory resolves about 96% of sessions, and the 4%
// that miss land in a directory that EXISTS and holds another session's
// transcripts — so swapping the encoder without checking the input trades a
// loud miss for a quiet wrong answer. The value passed here is safe: see
// Start, which resolves c.cwd before the child is spawned, from the `cd` prefix
// or from cmd.Dir or from magmux's own directory. It is the directory claude
// is about to be launched in, whatever the field is called, and it is never
// re-read from the running process.
func encodeProjectDir(launchDir string) string {
	return claude.EncodeProjectDir(launchDir)
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
	// Walk the launch looking for a claude invocation with a quoted prompt.
	for _, arg := range launchArgs(p) {
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

// launchArgs is what the prompt and cwd extractors read: the pane's original
// launch line when newPane recorded one, else cmd.Args as they always did.
// For a shell launch the two give the same answer — cmd.Args is
// [shell, -l, -c, line] and only the line can mention claude — so the
// editor-mode rewrite can change what is exec'd without moving discovery, and
// a struct-literal pane with no launchLine behaves exactly as before.
func launchArgs(p *Pane) []string {
	switch {
	case p == nil:
		return nil
	case p.launchLine != "":
		return []string{p.launchLine}
	case p.cmd != nil:
		return p.cmd.Args
	}
	return nil
}

func (c *ClaudeCodeController) Name() string { return claudeCodeName }

func (c *ClaudeCodeController) Start(ctx context.Context) error {
	if c.spawnedAt.IsZero() {
		c.spawnedAt = time.Now()
	}
	c.snap.State = CtrlStarting
	if c.bindID != "" && c.pane != nil && c.pane.mux != nil && !c.pane.mux.declareSession(c.bindID, c.pane) {
		// Two panes of one magmux declaring one session: the second waits, and
		// findNamedSession keeps refusing it the file while the first holds it.
		if dbgFile != nil {
			fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p session %s is already declared by another pane\n", c.pane, c.bindID)
		}
	}
	// The Start split. The readiness half first, and it cannot fail, so a
	// gated pane has a working gate whatever the transcript half does.
	c.startTTYWatch()
	err := c.startTranscript()
	if err == nil {
		return nil
	}
	if dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p transcript discovery unavailable: %v\n", c.pane, err)
	}
	if inputGateOf(c.pane) != nil {
		// A GATED pane keeps its controller with transcript discovery dormant
		// (projectDir stays "", so pollTranscript is a no-op). Returning the
		// error would make attachController drop the controller, and with it
		// the gate: a declared pane would silently become ungated.
		return nil
	}
	// An observed-only pane is exactly as before: no transcript, no controller.
	return err
}

// startTranscript is Start's transcript half: where claude runs, and therefore
// which ~/.claude/projects directory to tail. Its one failure is
// os.UserHomeDir, which leaves discovery with nowhere to look.
func (c *ClaudeCodeController) startTranscript() error {
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
	// Compute project dir: under the Claude config directory the child uses
	// (CLAUDE_CONFIG_DIR, as claudeConfigDir resolves it), else ~/.claude. A
	// declared transcript says where it lives outright.
	config := configDirOfTranscript(c.bindPath)
	if config == "" {
		config = c.claudeConfigDir()
	}
	if config == "" {
		return fmt.Errorf("no Claude config directory: CLAUDE_CONFIG_DIR is unset and the home directory is unknown")
	}
	c.projectsDir = filepath.Join(config, claudeProjectsDir)
	c.projectDir = filepath.Join(c.projectsDir, encodeProjectDir(c.cwd))
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
	for _, arg := range launchArgs(p) {
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

// Stop ends the termios watch, if any; close is idempotent and a heat after it
// starts nothing.
func (c *ClaudeCodeController) Stop() error {
	if c.tty != nil {
		c.tty.close()
	}
	return nil
}

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
// The demotion itself is demoteSettled (controller_plugin.go), shared with the
// plugin-backed controller so the two cannot drift: what a new turn must clear
// — the previous turn's answer above all — is one rule and has one copy.
func (c *ClaudeCodeController) consumeInjected() {
	if !c.injected.Swap(false) {
		return
	}
	if demoteSettled(&c.snap) && dbgFile != nil {
		fmt.Fprintf(dbgFile, "[ctrl/claude] pane=%p input injected → working\n", c.pane)
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
	c.publishInput()
	return c.snap, err
}

// publishInput refreshes Snapshot.Input, the published readiness VIEW, for a
// gated pane; every other pane keeps it zero. Poll is not a demand: one cold
// termios sample (a no-op while the hot sampler runs), then a decision on
// history. It never emits: snapshotChanged does not compare Input (D10).
func (c *ClaudeCodeController) publishInput() {
	if inputGateOf(c.pane) == nil {
		c.snap.Input = InputReadiness{}
		return
	}
	now := time.Now()
	if c.tty != nil {
		c.tty.sampleCold(now)
	}
	in, _ := c.readiness(now, ReadinessQuery{Class: ClassType, Types: true}, false)
	in.Gated = true
	c.snap.Input = in
}

// applyTerminalIdle reconciles this controller's view with the pane's own.
//
// The rule, and every reason for it, is mergeTerminalIdle's
// (controller_plugin.go): this controller's freshest evidence is the last
// transcript entry it consumed, so lastApplyAt is what a terminal idle signal
// is judged against.
//
// It is SHARED rather than duplicated because both controllers have the same
// hole in the same place. The transcript only announces "turn finished" via a
// stop_hook_summary entry, which Claude Code emits only when a Stop hook is
// configured, so a pane can sit at CtrlStarting indefinitely while it is
// visibly idle — and if transcript discovery failed outright, forever. A plugin
// watching for a pattern that never appears is in exactly that position. One
// merge is what keeps the live `snapshot` and the final `results` from
// disagreeing, for both of them.
func (c *ClaudeCodeController) applyTerminalIdle() {
	if c.pane == nil {
		return
	}
	mergeTerminalIdle(&c.snap, paneTerminalIdle(c.pane), c.lastApplyAt, "claude")
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
	if c.bindID != "" {
		return c.findNamedSession()
	}
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
	c.setBoundBy(bindMtime)
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
		c.setBoundBy(bindPrompt)
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), claude.TranscriptSuffix) {
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
		l, ok := decodeCtrlLine(scanner.Bytes())
		if !ok || l.Kind != claude.LineUser {
			continue
		}
		// The FIRST user entry decides, prompt or not: a first user entry that
		// is a tool result answers "" rather than looking further.
		if l.IsPrompt {
			return l.Prompt
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
// The parse itself is claude.ParseTurns (claudestate). It moved there with Turn and
// ToolCall — see the alias in controller.go — because a ToolCall's correlation
// id is unexported and whatever builds one has to live in the package that
// declares it. This wrapper is what keeps every caller here, and every test,
// compiling untouched.
func parseClaudeTurns(r io.Reader, maxTurns, byteBudget int) ([]Turn, error) {
	return claude.ParseTurns(r, maxTurns, byteBudget)
}

// applyLine parses one JSONL entry and updates the snapshot state machine.
//
// The record is DECODED by claude.DecodeLine (through decodeCtrlLine), the one
// reader of Claude's transcript format; this function only decides what each kind means for the
// snapshot, and names none of Claude's keys or values.
func (c *ClaudeCodeController) applyLine(line []byte) {
	l, ok := decodeCtrlLine(line)
	if !ok {
		return
	}

	// Project name: derive from cwd of any entry that has one
	if c.snap.Project == "" && l.CWD != "" {
		c.snap.Project = filepath.Base(l.CWD)
	}

	switch l.Kind {
	case claude.LineUser:
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
		if !l.IsPrompt {
			return
		}
		content := l.Prompt
		// Acceptance evidence: a transcript user entry, keyed, sequenced and
		// dated by when it was SUBMITTED — its own timestamp, or its enqueue's
		// when it is a dequeued prompt (review H2, r3 M2).
		written := l.Timestamp
		c.notePrompt(content, c.dequeuedAt(content, written))
		// New turn starting
		c.snap.LastUserPrompt = content
		c.snap.State = CtrlWorking
		c.snap.StartedAt = written
		c.snap.CompletedAt = time.Time{}
		c.snap.LastResponse = ""
		c.snap.LastTool = ""

	case claude.LineAssistant:
		if l.Model != "" {
			c.snap.Model = l.Model
		}
		// The last non-blank text block and the last tool call of this entry.
		if l.HasText {
			c.snap.LastResponse = l.Text
		}
		if l.HasTool {
			c.snap.LastTool = l.Tool
		}
		// Don't transition state here — wait for stop_hook_summary

	case claude.LineStopHookSummary:
		// The turn's Stop hook ran, so the turn is over.
		c.snap.State = CtrlAwaitingInput
		c.snap.CompletedAt = l.Timestamp

	case claude.LineQueueOperation, claude.LineAttachment:
		// A queued (mid-turn) prompt: acceptance evidence only, never a turn
		// boundary — research F11 (controller_claude_input.go, noteQueued).
		c.noteQueued(l)
	}
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

// setBoundBy publishes how discovery found the transcript, for `list`.
func (c *ClaudeCodeController) setBoundBy(mode string) {
	c.boundBy.Store(&mode)
}
