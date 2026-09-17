package plugin

// Spawning a plugin process, and ending one.
//
// A spawned plugin is an ordinary child process with four deliberate
// properties:
//
//	/bin/sh -c CMD   so `--plugin 'bun main.ts --flag'` quotes like a shell
//	Setpgid          so shutdown can signal the plugin AND its own children
//	stdin /dev/null  so a plugin that reads stdin gets EOF rather than the tty
//	stdout+stderr    to a log file, never to magmux's stdout, which is a frame
//
// The last one is not tidiness. magmux is usually holding a raw-mode terminal
// with an alternate screen on it; one stray line from a plugin corrupts the
// display with no way to repaint it, and a plugin author would have no idea
// their `console.log` did it.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Shutdown timings. One second between SIGTERM and SIGKILL: a plugin's whole
// exit path is closing a socket and returning, and anything slower is either
// wedged or ignoring the signal.
const (
	termGrace = time.Second
)

// process is one spawned plugin: the child, the token it was issued, and the
// log file its output goes to.
type process struct {
	cmd   *exec.Cmd
	cmdln string
	// token is what this child must present in plugin.register. It is issued
	// per process, not per magmux, so a plugin cannot register under another
	// plugin's name using a token it read out of a shared environment.
	token string
	// id is MAGMUX_PLUGIN_ID: magmux's own label for this child, used before it
	// has a name to be known by.
	id string
	// log is the open file its stdout and stderr go to. Renamed to carry the
	// plugin's real name once it registers (see nameLog).
	log     *os.File
	logPath string

	waitOnce sync.Once
	waitErr  error
	exited   chan struct{}
}

// Spawn starts every --plugin command. It returns the first failure and leaves
// whatever already started running: a plugin that failed to start is a plugin
// that will never register, which is exactly what the caller is told.
//
// sock is MAGMUX_SOCK — passed in rather than read from the environment because
// the host is built before the socket path is settled, and a plugin pointed at
// the wrong magmux is the one failure that looks like everything working.
func (h *Host) Spawn(cmds []string, sock string) error {
	if h == nil || len(cmds) == 0 {
		return nil
	}
	for i, cmdln := range cmds {
		if err := h.spawnOne(cmdln, sock, i+1); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) spawnOne(cmdln, sock string, n int) error {
	tok, err := newToken()
	if err != nil {
		return fmt.Errorf("--plugin %q: %v", cmdln, err)
	}
	id := fmt.Sprintf("p%d", n)

	logPath := h.logPath(id)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("--plugin %q: cannot open %s: %v", cmdln, logPath, err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		log.Close()
		return fmt.Errorf("--plugin %q: %v", cmdln, err)
	}
	defer devNull.Close()

	cmd := exec.Command("/bin/sh", "-c", cmdln)
	cmd.Stdin = devNull
	cmd.Stdout = log
	cmd.Stderr = log
	// Setpgid, NOT Setsid: the plugin gets its own process group so shutdown can
	// signal the whole tree (a `bun main.ts` that spawned a helper), while
	// staying in magmux's session so it is still killed if magmux is.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(append([]string{}, h.env()...),
		"MAGMUX_SOCK="+sock,
		"MAGMUX_PLUGIN_TOKEN="+tok,
		"MAGMUX_PLUGIN_ID="+id,
	)

	if err := cmd.Start(); err != nil {
		log.Close()
		return fmt.Errorf("--plugin %q: %v", cmdln, err)
	}
	pr := &process{cmd: cmd, cmdln: cmdln, token: tok, id: id, log: log, logPath: logPath, exited: make(chan struct{})}

	h.mu.Lock()
	h.procs = append(h.procs, pr)
	h.mu.Unlock()

	fmt.Fprintf(log, "\n=== magmux started this plugin at %s: %s\n", time.Now().Format(time.RFC3339), cmdln)
	h.debugf("[plugin] spawned %s (pid %d): %s → %s\n", id, cmd.Process.Pid, cmdln, logPath)

	// One cmd.Wait per child, and it is this goroutine. A plugin that exits on
	// its own is the same event as its connection closing, and whichever
	// happens first unregisters it; the other is then a no-op.
	go h.reap(pr)
	return nil
}

// reap waits for one plugin process and unregisters whatever it had registered.
func (h *Host) reap(pr *process) {
	pr.waitOnce.Do(func() { pr.waitErr = pr.cmd.Wait() })
	close(pr.exited)

	code := 0
	if pr.waitErr != nil {
		code = pr.cmd.ProcessState.ExitCode()
	}
	h.debugf("[plugin] %s exited with code %d (%s)\n", pr.id, code, pr.cmdln)
	fmt.Fprintf(pr.log, "=== plugin exited with code %d at %s\n", code, time.Now().Format(time.RFC3339))

	// Whatever it registered goes now: magmux cannot invoke a process that has
	// exited, and leaving the ops in the table would advertise ops that answer
	// plugin_gone for the rest of the session.
	for _, p := range h.pluginsOn(pr) {
		h.unregister(p, fmt.Sprintf("process exited with code %d", code), &code)
	}
	_ = pr.log.Close()
}

// pluginsOn lists the registrations made by a given child. It is a scan rather
// than a back-pointer because the association is only ever known from the
// token, and a child that registered nothing is the common case at shutdown.
func (h *Host) pluginsOn(pr *process) []*plug {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*plug
	for _, p := range h.plugins {
		if p.proc == pr {
			out = append(out, p)
		}
	}
	return out
}

// Shutdown ends every spawned plugin: SIGTERM to the process GROUP, a second's
// grace, then SIGKILL, then cmd.Wait.
//
// It runs AFTER the socket teardown, so a plugin sees results → shutdown → EOF
// on its own connection and can exit by itself — which most will, making the
// SIGTERM below a formality. The negative pid is the process group, so a plugin
// that spawned helpers takes them with it instead of leaving orphans behind.
//
// cmd.Wait is not optional: a killed child that is never waited on stays a
// zombie for the life of magmux, and magmux outlives its plugins by design.
func (h *Host) Shutdown() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.closing = true
	procs := append([]*process(nil), h.procs...)
	h.mu.Unlock()
	if len(procs) == 0 {
		return
	}

	for _, pr := range procs {
		pr.signal(syscall.SIGTERM)
	}
	deadline := time.After(termGrace)
	for _, pr := range procs {
		select {
		case <-pr.exited:
		case <-deadline:
			// One shared grace, not one each: the whole point of the bound is
			// that teardown takes a second, however many plugins there are.
			pr.signal(syscall.SIGKILL)
		}
	}
	for _, pr := range procs {
		select {
		case <-pr.exited:
		case <-time.After(termGrace):
			h.debugf("[plugin] %s did not exit after SIGKILL\n", pr.id)
		}
	}
}

// signal sends to the process GROUP. A plugin that spawned its own children —
// `bun` running a helper, a shell pipeline — leaves them behind if only the
// leader is signalled, and they inherit the plugin's socket and its token.
func (pr *process) signal(sig syscall.Signal) {
	if pr.cmd.Process == nil {
		return
	}
	select {
	case <-pr.exited:
		return
	default:
	}
	if err := syscall.Kill(-pr.cmd.Process.Pid, sig); err != nil {
		// The group may be gone already; fall back to the leader so a signal is
		// never silently skipped.
		_ = pr.cmd.Process.Signal(sig)
	}
}

// ── plumbing ────────────────────────────────────────────────────────────────

// logPath is {LogDir}/magmux-{ID}.plugin-{who}.log. `who` is the assigned
// MAGMUX_PLUGIN_ID at spawn time and the plugin's own name once it registers —
// see nameLog for why the file is renamed rather than named correctly up front.
func (h *Host) logPath(who string) string {
	dir := h.cfg.LogDir
	if dir == "" {
		dir = os.TempDir()
	}
	id := h.cfg.ID
	if id == "" {
		id = fmt.Sprintf("%d", os.Getpid())
	}
	return filepath.Join(dir, fmt.Sprintf("magmux-%s.plugin-%s.log", id, who))
}

// nameLog renames a running plugin's log file to carry its registered name.
//
// The name is not known when the file is opened: stdout and stderr have to be
// redirected before the process starts, and the process announces its name
// several milliseconds later. Renaming an open file leaves the descriptor
// valid on POSIX, so the writes continue into the renamed path — and a plugin
// that crashed BEFORE registering leaves its diagnosis in
// `plugin-p1.log`, which is precisely the case where you need the file to
// exist at all.
func (h *Host) nameLog(pr *process, name string) {
	if pr == nil || pr.logPath == "" {
		return
	}
	want := h.logPath(name)
	if want == pr.logPath {
		return
	}
	if err := os.Rename(pr.logPath, want); err != nil {
		h.debugf("[plugin] could not rename %s to %s: %v\n", pr.logPath, want, err)
		return
	}
	pr.logPath = want
}

// newToken mints a spawned plugin's credential: 32 random bytes as unpadded
// base64url, the same shape and the same entropy as the session token.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a plugin token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// env is the base environment for a spawned plugin.
func (h *Host) env() []string {
	if h.cfg.Env != nil {
		return h.cfg.Env
	}
	return os.Environ()
}
