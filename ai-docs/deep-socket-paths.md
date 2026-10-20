# Deep socket paths: why `unixsock` exists

This is the design record behind magmux 0.18.0's `unixsock` package and the fatal `--sock-dir` / `MAGMUX_SOCK_DIR`. Its measurements are why the obvious alternatives were rejected. Line numbers refer to the tree before 0.18.0.

**What shipped differs from the "Approach" section below in four ways**, decided at review:

1. On darwin, the bind or dial runs on a NEW goroutine that calls `runtime.LockOSThread()` and never unlocks. Go destroys a thread whose goroutine exits while locked, so nothing is ever reverted, no caller's thread is touched, and a panic in the call cannot leave a thread with the wrong cwd (`unixsock/unixsock_darwin.go`).
2. The socket is bound synchronously in `mux.Main` (`listenSocket`) before any pane or plugin starts. A bind failure exits 1, and the accept loop (`serveSocket`) runs on its own goroutine.
3. `SetUnlinkOnClose(false)` is set only on the long branch. Short paths behave exactly as before.
4. Linux CI runs the `unixsock` tests through `go test ./...`.

The claudish half lives in claudish: it removed its 100-byte refusal and set `MIN_MAGMUX_VERSION = "0.18.0"`.

## Root Cause (one sentence)

magmux hands the absolute socket path straight to Go's `net.Listen`/`DialContext("unix", abs)` (magmux `mux/socket.go:187`, `client/client.go:651-652`, `mcp/mcp_spawn.go:163-164`), which Go refuses with EINVAL once the path is longer than `sizeof(sun_path)` (104 on darwin, 108 on linux). Instead of addressing a deep socket by a short name, magmux avoids that failure in `sockdir.ValidDir` (`sockdir/sockdir.go:103-107`): it substitutes `/tmp` without telling the caller (env path `sockdir.go:60-67`; flag path `mux/cli.go:443-457`, which only warns). So a deep `--sock-dir` is never honoured, and claudish has to refuse 100 bytes or more itself (`packages/cli/src/pane/pane-launch.ts:111-119`) so that it does not poll a socket that is not there.

Evidence (measured 2026-10-17, Go 1.27.2 darwin/arm64, Bun 1.4.0, Docker debian:bookworm-slim / oven/bun:1.4.0):
- Go `net.Listen("unix", <311-byte abs>)` -> `bind: invalid argument` (darwin). On linux, a 209-byte absolute path fails the same way.
- Bun binds and connects long absolute paths on BOTH OSes: darwin 286 B, linux 185 B, `bind OK file=true`, `connect OK pong`. So claudish needs no transport change.
- The limit applies to the ADDRESS passed, not to where the file lives. See Approach for the two addressing mechanisms, both measured.

## Alternative Hypotheses (3-5, dismissed)

1. **The kernel's sun_path limit blocks every runtime, so only madbench (Route 2) can fix this.** Dismissed: Bun binds and connects at 185-286 bytes on darwin and linux (measured). In Go the EINVAL comes from Go's own `SockaddrUnix` length check on the address it was given. A short address that names a deep file works on both OSes (below).
2. **claudish's 100-byte check in `sockPathFor` is the defect.** Dismissed as the root: it exists only because magmux falls back without telling the caller (its own doc comment at `pane-launch.ts:111` says so). Removing it alone makes claudish poll `<root>/magmux-<id>.sock` while magmux listens in `/tmp`, which is worse.
3. **Use `bindat`/`connectat` with a directory fd on darwin.** Dismissed: they are not in the darwin syscall table. The SDK `sys/syscall.h` has `SYS_connect 98`, `SYS_bind 104` and `SYS_connectx 447`, and no `*at` variants. `golang.org/x/sys v0.42.0` defines them only in `zsysnum_freebsd_*.go`.
4. **`/dev/fd/<dirfd>/<name>` on darwin, mirroring linux's `/proc/self/fd`.** Dismissed by measurement: `bind: no such file or directory` (fdesc does not resolve names under a directory fd).
5. **Process-wide `os.Chdir` under `syscall.ForkLock` (option a/b), a short symlink directory (option d), or a helper child (option c).**
   - (a)/(b) DEADLOCKS on darwin. Go's `net` takes `syscall.ForkLock.RLock()` in `net/sys_cloexec.go:22` for every socket, because darwin has no `SOCK_CLOEXEC`. With `ForkLock.Lock()` held, `net.Listen` blocked for good (SIGQUIT dump: goroutine 1 in `sync.RWMutex.RLock` at `sys_cloexec.go:22`). Without ForkLock, a plain `os.Chdir` leaked into concurrent goroutines (`os.Getwd` and relative `os.Stat` saw the deep cwd 133,534 times) and into 429 of 473 concurrent forks (`/bin/pwd` printed the deep dir). The 1 ms hold widens the window, but the leak is real. Moving the bind earlier in magmux's `main` cannot protect `client.Dial`, which is a PUBLIC package called from other people's processes, and `mcp` probes sockets from parallel goroutines (`mcp_spawn.go:120-139`).
   - (d) needs a short directory that the sandbox lets the process read. The bug exists because there is none.
   - (c) needs to re-exec a helper binary. A program that imports `client` has no such binary.
6. (Side claim in the handoff) **The reaper's liveness probes dial sockets and need the same fix.** Dismissed: `sockdir.ReapStale` deliberately never dials. Its oracle is `kill(pid,0)` (`sockdir.go:203-217`), and it only does `os.ReadDir` and `os.Remove` on absolute paths, which have no sun_path limit.

## Fix Approach

### Scope

Route 1 only. magmux binds and dials a socket at any depth, through ONE helper used by every bind and dial site. A bad `--sock-dir` / `MAGMUX_SOCK_DIR` becomes a loud failure: non-zero exit, stderr only, the source, directory and reason named, and no `/tmp` fallback. claudish drops its 100-byte refusal and raises `MIN_MAGMUX_VERSION` to the magmux release that ships this (0.18.0). Out of scope: Route 2 (madbench), making the bind synchronous or fatal (not needed, see Approach), and the two latent off-child-path `/tmp` hardcodes in claudish (reported in the audit).

### Files to modify

magmux (`/Users/jack/mag/magmux/.claude/worktrees/long-socket-path`):
- NEW `unixsock/unixsock.go`, `unixsock/unixsock_darwin.go`, `unixsock/unixsock_linux.go`, `unixsock/unixsock_other.go` (`//go:build !darwin && !linux`, because `client/` is public), and `unixsock/unixsock_test.go`. A leaf package that imports only std. NOT inside `sockdir`: `sockdir` reads `MAGMUX_SOCK_DIR` and owns magmux's directory policy, and a program that imports `client` must not inherit either.
- `sockdir/sockdir.go:32-110`: `Dir` becomes a plain default (`var Dir = sockDirDefault`). `resolveSockDirEnv` (60-67) is replaced by `Resolve(flag string) (string, error)`. `sunPathMax` (69-75) is deleted. `ValidDir` (77-110) becomes `ValidDir(dir string) (string, error)`, with no id parameter, no length check and no fallback value. Fix the stale comment at 388-395 (`ValidSocketID`: "re-checks the length … (ValidDir)").
- `mux/cli.go:417-458`: replace the whole `ValidDir` / "ignoring --sock-dir" block with a `sockdir.Resolve` call. Update the help text at 84-86, 159 and 225-228 (drop "Ignored with a message"; say "fatal if unusable").
- `mux/socket.go:184-199`: `ln, err := unixsock.Listen(sockPath)` replaces `net.Listen("unix", sockPath)` at 187. Teardown at 204-210 is unchanged: it already does `os.Remove(sockPath)` by the absolute path, and `cli.go:767-768` does too.
- `client/client.go:646-655`: `unixsock.Dial(ctx, &net.Dialer{Timeout: 2 * time.Second}, sockPath)`.
- `mcp/mcp_spawn.go:156-166` (`probeSocket`): `unixsock.Dial(ctx, &net.Dialer{Timeout: timeout}, path)`. Keep `errors.Is` classification at 126-133 working; the helper wraps with `%w`.
- `mcp/mcp.go:186-212` (`Run`): resolve the directory before `newMCPServer`. Update the help text at 236-241.
- `mux/startup_test.go:53-57` (`dialASAP`): dial through `unixsock.Dial`, so the e2e tests can reach a deep socket. Other test dialers (`main_test.go`, `plugin_test.go`, `pilot_test.go`, `sockrpc_test.go`, `test/resize/harness_test.go:136,185`, `mcp/rc_e2e_test.go:261`) stay on short `/tmp` dirs and can stay as they are.
- `README.md:64, 280, 305-310`, `CLAUDE.md:81-83` (add `unixsock/` to "Socket lifecycle"), `CHANGELOG.md` `[Unreleased]`: mark as BREAKING that an unusable `--sock-dir` or `MAGMUX_SOCK_DIR` is now fatal, and record that deep socket directories are supported. Release as 0.18.0.

claudish (`/Users/jack/mag/claudish/.claude/worktrees/long-socket-path`):
- `packages/cli/src/pane/pane-launch.ts:111-119`: delete the 100-byte check. With it gone, `sockPathFor` is identical to `sockPathOf` (`pane/process-identity.ts:329-331`), so delete it and call `sockPathOf` at `pane/pane-session.ts:1892` (import at :41). Pillar 2: one function per fact.
- `pane-launch.ts:483`: `MIN_MAGMUX_VERSION = "0.18.0"`. Fix the doc comment at :504 (it says "≥ 0.14.0").
- `pane-launch.ts:620`: `FATAL_MAGMUX_STDERR = /ignoring --id|magmux: (?:--sock-dir|MAGMUX_SOCK_DIR) |magmux: socket/`, to match the new magmux message, because "ignoring --sock-dir" no longer exists.
- Version literals that restate the constant (pillar 2): interpolate `MIN_MAGMUX_VERSION` at `mcp-server.ts:460`, `mcp-server.ts:1866`, `team-cli.ts:90` and `team-orchestrator.ts:1109`. Docs: `docs/usage/mcp-server.md:645`, `ai-docs/architecture/pane-session.md:537`.
- `.github/magmux.env`: `MAGMUX_TAG=v0.18.0` and the four `MAGMUX_SHA256_*`, copied once magmux v0.18.0 is published. `release.yml:58-62` already refuses a pin older than `MIN_MAGMUX_VERSION`, and `test.yml` installs the same pin, so the pane suites run against 0.18.0.
- Stale comments ("so socket paths stay < 100 bytes"): `pane/test-helpers/hermetic-env.ts:9`, `mcp-e2e/runner.ts:116`, `test-helpers/contract-mcp.ts:40`. The short `/tmp/cpt-*` roots may stay.

### Approach

**Mechanism: a per-THREAD working directory, never a process-wide chdir.**

- **darwin:** pin the goroutine with `runtime.LockOSThread()`, then switch only that thread's cwd with `syscall.Syscall(syscall.SYS___PTHREAD_FCHDIR, dirfd)` (number 349, exported by Go's own std `syscall` for darwin amd64 and arm64; no cgo; `x/sys` not needed). Call `net.Listen` / `Dialer.DialContext("unix", <base name>)` on that thread, revert with `__pthread_fchdir(-1)`, then `UnlockOSThread`. If the revert fails, return WITHOUT unlocking: Go then terminates the thread when the goroutine exits, so a wrong cwd can never be handed to another goroutine.
- **linux:** open the directory with `O_RDONLY|O_DIRECTORY|O_CLOEXEC` (std `syscall` has no `O_PATH` on linux/amd64) and pass `/proc/self/fd/<fd>/<base name>`, at most about 96 bytes for a 64-character id. No cwd is touched at all.
- **Other OSes** (`client` is public): use the path directly, or return an error naming the path, its length and the limit.
- **Paths shorter than `len(syscall.RawSockaddrUnix{}.Path)`** (104 or 108) take the unchanged direct call. The `/tmp` default, every existing test and every short `--sock-dir` behave byte for byte as today.

Measured evidence (from scratch experiments, not kept):
- darwin, 311-byte path, 200 iterations of bind + dial by name under the per-thread cwd: all OK, socket file at the deep path. Concurrently, 8 goroutines ran `os.Getwd`, relative `os.Stat` and `exec /bin/pwd -P`: 54,585 checks and 495 forks, **0** saw the deep cwd, and the process cwd never changed.
- Process-wide `os.Chdir` + `ForkLock`: deadlock (`net/sys_cloexec.go:22`). `os.Chdir` alone: leaked (numbers under Alternative 5).
- linux (debian:bookworm-slim, Go 1.27 static arm64), 209-byte path: `/proc/self/fd/3/<name>` (92 bytes) -> `proc listen: <nil>`, `file at deep path: true`, `proc dial: <nil>`, reply `"pong\n"`.

**Why the race question goes away.** The per-thread cwd is invisible to every other thread, and a fork from another thread copies that thread's cwd, which is the process cwd (measured: 0/495). So `go mux.socketServer()` (`mux/cli.go:615`) can keep running beside pane spawning (`mux/pane.go:265-266`: `cmd.Dir = cfg.Dir`, empty for `-e` panes, so they inherit magmux's cwd), plugin spawning (`plugin/spawn.go:97-104`: no `Dir`), `ClaudeCodeController.Start`'s `os.Getwd()` (`mux/controller_claude.go:237`) and `mcp`'s parallel probes (`mcp_spawn.go:120-139`). No ordering change is needed in `main`. The same helper is correct inside any host program that imports `client`.

**The helper (one implementation; every bind and dial site calls it):**

```go
// unixsock.go
package unixsock

// addrMax is sizeof(sun_path): 104 on darwin, 108 on linux.
var addrMax = len(syscall.RawSockaddrUnix{}.Path)

// Listen binds a unix stream socket at path, whatever its length. The caller
// removes the file by its real path; the listener never unlinks (its address may
// be a short alias that names a different file once the thread cwd is restored).
func Listen(path string) (*net.UnixListener, error) {
	var ln net.Listener
	if err := at(path, func(a string) (err error) { ln, err = net.Listen("unix", a); return err }); err != nil {
		return nil, err
	}
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	return ul, nil
}

// Dial connects to the unix socket at path, whatever its length, honouring d and ctx.
func Dial(ctx context.Context, d *net.Dialer, path string) (net.Conn, error) {
	var c net.Conn
	err := at(path, func(a string) (err error) { c, err = d.DialContext(ctx, "unix", a); return err })
	return c, err
}

func at(path string, fn func(addr string) error) error {
	if len(path) < addrMax {
		return fn(path) // today's behaviour, byte for byte, errors included
	}
	dir, name := filepath.Split(path)
	if len(name) >= addrMax {
		return fmt.Errorf("unix socket %s: file name is %d bytes, over the %d-byte address limit", path, len(name), addrMax)
	}
	if err := inDir(filepath.Clean(dir), name, fn); err != nil {
		return fmt.Errorf("unix socket %s: %w", path, err) // %w keeps errors.Is(ECONNREFUSED/ENOENT/EACCES)
	}
	return nil
}
```

```go
// unixsock_darwin.go: a per-thread cwd; no other goroutine or fork can observe it.
func inDir(dir, name string, fn func(string) error) error {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	runtime.LockOSThread()
	if _, _, e := syscall.Syscall(syscall.SYS___PTHREAD_FCHDIR, uintptr(fd), 0, 0); e != 0 {
		runtime.UnlockOSThread()
		return fmt.Errorf("pthread_fchdir %s: %w", dir, e)
	}
	ferr := fn(name)
	if _, _, e := syscall.Syscall(syscall.SYS___PTHREAD_FCHDIR, ^uintptr(0), 0, 0); e != 0 { // -1: back to the process cwd
		return fmt.Errorf("pthread_fchdir revert: %w", e) // thread stays locked: Go discards it
	}
	runtime.UnlockOSThread()
	return ferr
}

// unixsock_linux.go: no cwd at all; the kernel resolves the magic link.
func inDir(dir, name string, fn func(string) error) error {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return fn(fmt.Sprintf("/proc/self/fd/%d/%s", fd, name)) // ENOENT here when /proc is not mounted: loud, names the path
}
```

`SetUnlinkOnClose(false)` is safe for magmux: `socketServer` already removes by the absolute path (`socket.go:208`, and `cli.go:767-768` on exit). It is REQUIRED on the long branch: Go's `UnixListener.close` unlinks `ln.path` RELATIVELY, which after the revert would name a file in magmux's own cwd.

**Loud failure, one resolver for the flag and the env (pillar 2).**

```go
// sockdir.go
var Dir = sockDirDefault // written once by the entry point, after Resolve

// Resolve returns the socket directory: --sock-dir when given (the env is then not
// consulted, so a stale MAGMUX_SOCK_DIR cannot veto a correct flag), else
// MAGMUX_SOCK_DIR, else /tmp. An unusable directory is an ERROR naming its
// source, the directory and the reason. There is no fallback.
func Resolve(flag string) (string, error) {
	src, v := "--sock-dir", flag
	if v == "" {
		src, v = "MAGMUX_SOCK_DIR", os.Getenv("MAGMUX_SOCK_DIR")
	}
	if v == "" {
		return sockDirDefault, nil
	}
	dir, err := ValidDir(v)
	if err != nil {
		return "", fmt.Errorf("%s %q: %v", src, v, err)
	}
	return dir, nil
}

// ValidDir: empty / cannot stat it (with the stat error) / not a directory /
// not writable by anyone. No length check: unixsock binds at any depth.
func ValidDir(dir string) (string, error)
```

- **Flag and env path, `mux.Main`** (replaces `cli.go:427-458`; runs before `prepareRemote` and `mux.init()`, so stderr is still a plain terminal and there is nothing to restore):
  ```go
  dir, err := sockdir.Resolve(sockDirArg)
  if err != nil {
  	fmt.Fprintf(os.Stderr, "magmux: %v\n", err) // e.g. magmux: --sock-dir "/x/y": cannot stat it: stat /x/y: no such file or directory
  	return 1
  }
  sockdir.Dir, muxSockDir = dir, dir
  if sockDirArg != "" { os.Setenv("MAGMUX_SOCK_DIR", dir) } // as today: children, incl. a `magmux mcp` in a pane, inherit the flag
  ```
- **`magmux mcp`** (`mcp.Run`, after the `--help` scan and before `newMCPServer(os.Stdout, …)`): call `sockdir.Resolve("")`. On error, write `magmux mcp: MAGMUX_SOCK_DIR "<dir>": <reason>` to **os.Stderr** and `return 1`. Not one byte reaches stdout, because the server, the only stdout writer, is never constructed. MCP clients report a server that fails to start, and keep its stderr in their logs. That replaces today's misleading "no sessions found". The package-init resolution (`sockdir.go:48`) is removed, so nothing prints, or can fail, at import time in any importer.
- The `id` argument and `sunPathMax` go away. `ValidSocketID`'s 64-character cap stays: `magmux-` + 64 + `.sock` = 76 bytes, which fits the short-name address on darwin (<104) and the `/proc/self/fd/N/` address on linux (<108).

**claudish.** Delete the check, raise the minimum, and match the new message (files above). Keep connecting by the absolute path: Bun handles more than 104/108 bytes on both OSes (measured). The pin mechanism already exists, at two layers:
- runtime: `assertMagmuxAvailable` (`pane-launch.ts:504-530`) refuses `magmux --version` below `MIN_MAGMUX_VERSION`, and `pane-session.ts:523-526` re-checks it against `capabilities`;
- release: `release.yml:58-62` refuses a `.github/magmux.env` pin below the constant.

Raising the constant to `0.18.0` and the pin to `v0.18.0` (after magmux publishes it) is therefore enough. An old magmux on PATH is refused with `magmux_unavailable: … panes need >= 0.18.0`, and is never paired with the relaxed check. Order: magmux 0.18.0 release, then the claudish change and pin bump in one commit.

### Tests to change and RED tests to add

**magmux tests that pin today's behaviour and must change:**
- `sockdir/sockdir_test.go:18-37` `dirOfLength`: delete. It exists only for the length case.
- `sockdir/sockdir_test.go:39-87` `TestValidSockDir`: new signature `ValidDir(dir) (string, error)`. The case `{"long dir, 64-char id", …, false}` (:65) goes. A refusal asserts a non-nil error that names the reason, not the `sockDirDefault` fallback (:76-78).
- `sockdir/sockdir_test.go:89-100` `TestValidSockDirLengthIsCheckedAgainstTheRealPath`: delete. It pins the length refusal.
- `sockdir/sockdir_test.go:102-118` `TestResolveSockDirEnv` becomes `TestResolve`. It covers: unset gives `/tmp`; a valid env gives that dir; an invalid env gives an error containing `MAGMUX_SOCK_DIR` and the dir (today :114-117 asserts the SILENT fallback); a valid flag with an invalid env gives the flag dir and no error; an invalid flag gives an error containing `--sock-dir`.
- `mux/sockdir_e2e_test.go:97-152` `TestSockDirFlagFallsBackLoudly`: replaced by RED #3. It pins "comes up on `/tmp`" (:139-141) and "stderr mentions /tmp" (:149-151).
- Stays green and must NOT change: `mux/sockdir_test.go:19-29` `TestSocketPathDefaultIsUnchanged` (`sockdir.Dir` still defaults to `/tmp` and no longer reads the env at init, which makes the test more hermetic), `:31-43` `TestSocketPathHonoursSockDirField`, `mux/sockdir_e2e_test.go:24-95` `TestSockDirFlagBindsElsewhere`, and all reaper tests.
- Stale rationale comments only (no failure): `mux/sockdir_test.go:45-54`, `sockdir/sockdir_test.go:139-148`, `mux/sockdir_e2e_test.go:30-33`, `mux/headless_test.go:94-100`, `mcp/rc_e2e_test.go:224-225`, `mcp/mcp_test.go:375`, `client/pluginsdk_test.go:44`.

**magmux RED tests (each fails on the baseline for the reason given):**
1. `mux/sockdir_e2e_test.go` `TestSockDirFlagBindsDeepDir`. Use `t.TempDir()` plus nested segments so `filepath.Join(dir, "magmux-<pid>.sock")` is at least 120 bytes (assert the length). Run `magmux --sock-dir <deep> -w -e 'sh -c "printf … > envfile; sleep 2"'` and `dialASAP` the deep path through `unixsock.Dial`. Assert `MAGMUX_SOCK` equals the deep path and `/tmp/magmux-<pid>.sock` does not exist. RED today: stderr `ignoring --sock-dir … using /tmp` and the dial times out.
2. `unixsock/unixsock_test.go`:
   - `TestListenAndDialPastSunPath`, 120+ byte path. Positive control: plain `net.Listen("unix", path)` must FAIL, which proves the path really is over the limit. Then `unixsock.Listen` works, `os.Lstat(path)` has `ModeSocket`, and `unixsock.Dial` round-trips `pong`.
   - `TestPerThreadCwdDoesNotLeak`: a port of the scratch experiment, with 8 observers checking `os.Getwd`, a relative marker `os.Stat` and `exec /bin/pwd -P` across 200 Listen/Dial loops; 0 leaks.
   - `TestShortPathTakesDirectRoute`: `ln.Addr().String() == path` for a `/tmp` path.
   - `TestListenerDoesNotUnlinkRelativeName`: put a file named like the socket in the test's cwd; Close must leave it alone.

   RED by compile failure; the behavioural RED is #1.
3. `mux/sockdir_e2e_test.go` `TestSockDirFlagRefusedLoudly`, with `--sock-dir <missing>`: `cmd.Wait()` returns within 5 s, `ExitCode() != 0`, and stderr contains `--sock-dir`, the missing dir and `cannot stat`. Assert `/tmp/magmux-<pid>.sock` never appears. RED today: magmux runs and binds `/tmp`.
4. `TestSockDirEnvRefusedLoudly`: the same with `MAGMUX_SOCK_DIR=<missing>` and no flag; stderr names `MAGMUX_SOCK_DIR`. RED today: silent `/tmp`.
5. `mcp` `TestMCPRefusesBadSockDirOnStderrOnly`: `magmux mcp` with `MAGMUX_SOCK_DIR=<missing>` and stdin from `/dev/null`. Exit != 0, **stdout is exactly 0 bytes**, stderr names `MAGMUX_SOCK_DIR` and the dir. RED today: it serves and exits 0 on EOF.
6. `mcp/rc_e2e_test.go` `TestMCPReachesDeepSocket`: headless magmux with a deep `--sock-dir`, then `magmux mcp` with `MAGMUX_SOCK_DIR=<deep>`. `list_sessions` reports the session Reachable (`probeSocket`), and one tool call succeeds (`client.Dial`). RED today: `ValidDir` throws the env away, so it sees "no sessions found".
7. `client` `TestDialDeepSocket`: a fake server via `unixsock.Listen` at a 120+ byte path writes one snapshot line, and `client.Dial` succeeds. RED today: `dial unix …: invalid argument`.

**claudish:**
- `packages/cli/src/pane/pane-launch.test.ts:214-217`. RED: "a 121-byte pane socket path is accepted". Build `root = "/tmp/" + "d".repeat(k)` so `sockPathFor(root, "c1-abcdefghij")` is exactly 121 bytes (assert `Buffer.byteLength`), and expect the joined path back with no throw. RED today: `PaneRootError … it must be < 100`. In the same commit, retarget the test to `sockPathOf` when `sockPathFor` is deleted.
- `pane-launch.test.ts:274-298` and `pane/pane-registry.test.ts:436`: the fake `magmux 0.14.0` / `0.13.9` / `/panes need >= 0.14.0/` literals break when the constant moves. Derive them from `MIN_MAGMUX_VERSION` (current, and one patch or minor below).
- New: `FATAL_MAGMUX_STDERR` matches `magmux: --sock-dir "/x": not a directory` and `magmux: MAGMUX_SOCK_DIR "/x": cannot stat it`, and still matches `magmux: socket …`.
- Verification (handoff steps 1-3): `CLAUDISH_PANE_ROOT=<dir giving a 120+ byte socket path>`, `create_session` reaches `completed`, and CMON-1 runs under `sandbox: home`.

## /tmp audit (claudish child path)

The child path is the MCP server's `create_session` / `team` slot (`pane-session.ts`, `mcp-session/session-manager.ts`, `team-orchestrator.ts`), plus the pane watcher (`/bin/sh`), the pane magmux, the claudish pane child and its Claude Code. "Follows root" means it derives from `sockRootFor(env)`, which already honours `CLAUDISH_PANE_ROOT` (`pane-launch.ts:46-51`), and `CLAUDISH_PANE_ROOT` reaches the child through `CLAUDISH_PANE_ENV`.

| # | Site | Default | On child path? | Verdict |
|---|---|---|---|---|
| 1 | `pane/pane-launch.ts:46-51` `sockRootFor`; `:54` `SOCK_ROOT` | `/tmp/claudish-mux-<uid>` | YES | This is the measured `EPERM … lstat '/tmp/claudish-mux-501'`. It already follows `CLAUDISH_PANE_ROOT`; no code change. The bench sets the variable through the plugin's `.mcp.json`, which is Route 1's stated bench step. |
| 2 | `pane-launch.ts:111-119` `sockPathFor` | 100-byte refusal | YES | Remove (this fix). |
| 3 | `pane-launch.ts:586-588` `ctlDir`/`turnDir` (`mkdtemp(join(root,"launch-"))`) | under root | YES | Follows root. |
| 4 | `pane/pane-registry.ts:56,281,287,537-540` records, pane limit, `sweepOrphanPanes`; `mcp-server.ts:2376` startup `ensureSwept(sockRootFor())`; `pane-session.ts:1924-1925` | under root | YES | Follows root (the reaper and scan the handoff worried about). |
| 5 | Pane magmux `--sock-dir root` (`pane-launch.ts:631-645`); magmux reaper `sockdir.ReapStale(filepath.Dir(sockPath))` (`mux/socket.go:177`); exports `MAGMUX_SOCK_DIR=root` (`mux/cli.go:454`) | root | YES | Follows root. magmux's own `/tmp` default is never consulted when the flag is given. |
| 6 | magmux `mux/mux.go:524-526` `/tmp/magmux-debug.log` | `/tmp` | only with `MAGMUX_DEBUG` | Irrelevant: opt-in, and the `os.Create` error is ignored. |
| 7 | magmux `plugin/spawn.go:236-239` plugin logs, `os.TempDir()` | `$TMPDIR` | no (claudish panes start no plugins) | Irrelevant. |
| 8 | `mcp-session/home-dir.ts:26-30` `CLAUDISH_SESSIONS_DIR` | `~/.claudish/sessions` | YES | HOME-based, so inside the sandbox; not `/tmp`. Irrelevant. |
| 9 | `update-checker.ts:50` | `~/.cache/claudish`, fallback `tmpdir()` | YES (the pane child is interactive, `index.ts:645-648, 847`) | Primary is under HOME; the fallback is `$TMPDIR` (madbench's `home/.tmp`), never `/tmp`. Irrelevant. |
| 10 | `claude-runner.ts:366, 804` `HOME \|\| USERPROFILE \|\| tmpdir()` | `$TMPDIR` only when HOME is unset | YES | Irrelevant (madbench sets HOME). |
| 11 | `mcp/run-prompt-claude-code.ts:275` `mkdtemp(tmpdir())` | `$TMPDIR` | no (`run_prompt`, not `create_session`) | Irrelevant. |
| 12 | `config.ts:92-95` | doc comment of `CLAUDISH_PANE_ROOT`'s default only | n/a | No own default. Update only if the default ever moves. |
| 13 | `launcher/magmux-wrapper.ts:235` `/tmp/magmux-${id}.sock`, `:257` `tmpRoot ?? "/tmp"` | `/tmp` | NO (`magmuxPaneCapability` returns `pane-child` first, `magmux-wrapper.ts:192`) | Off the child path; out of scope. Latent pillar-2 defect: it restates magmux's default and ignores an inherited `MAGMUX_SOCK_DIR`. After this fix, a bad inherited `MAGMUX_SOCK_DIR` makes its magmux exit 1 instead of binding `/tmp`, and the wrapper should pass `--sock-dir` explicitly. Follow-up. |
| 14 | `team-grid.ts:406-407` `/tmp/magmux-${proc.pid}.sock` | `/tmp` | NO (CLI `team --grid`; MCP `team` and `team --mode json` go through `pane-session`) | Same latent defect as #13: under an inherited `MAGMUX_SOCK_DIR` it polls the wrong path. Follow-up: spawn with `--id` + `--sock-dir` that claudish chooses. |
| 15 | Tests only: `pane/test-helpers/hermetic-env.ts:56`, `mcp-e2e/runner.ts:121`, `test-helpers/contract-mcp.ts:48`, `team-grid.e2e-helpers.ts:309-393` | `/tmp/cpt-*`, `/tmp` | test | Irrelevant; fix the stale "< 100 bytes" comments. |

Must follow `CLAUDISH_PANE_ROOT`: rows 1-5, which already do. Must change: row 2 only. Nothing else on the child path reads `/tmp`.

## Self-Critique

MINIMAL_CHANGE: PASS. One leaf helper (about 80 lines over four build-tagged files) replaces three call sites. `ValidDir` loses code (no length check, no id, no fallback). One `Resolve` serves both the flag and the env path. claudish deletes a check and moves one constant. `main`'s goroutine order, the reaper and socket teardown are untouched, and the bind does not have to become synchronous because the per-thread cwd removes the race rather than narrowing it.

ROOT_CAUSE_NOT_SYMPTOM: PASS. The address magmux passes is fixed at every bind and dial site, and the fallback that hid the failure is deleted. claudish's check, which guarded against the symptom, goes only once the pinned minimum guarantees the fixed magmux.

REGRESSION_SAFETY: PASS with two stated risks.
(a) Turning an unusable `MAGMUX_SOCK_DIR` into an exit 1 is BREAKING for anyone with a stale value in their environment (including claudish's recovery wrapper and `team --grid`, rows 13-14). The request intends this; it is a CHANGELOG `BREAKING` line and a minor-version bump.
(b) `SYS___PTHREAD_FCHDIR` has no prototype in the public SDK headers; its number is exported by Go's own std `syscall` tables. Only paths of 104 bytes or more take that branch, so a future macOS change cannot affect the `/tmp` default or any short directory, and its failure is a returned error, never a wrong cwd.
Short paths keep Go's direct call, errors included.

TEST_COVERAGE: PASS. RED tests cover each failure the bug report names: a deep bind and dial in `unixsock`, the mux e2e, the mcp discovery plus attach, and `client.Dial`; loud failure on the flag, on the env, and in `magmux mcp` with zero stdout bytes; claudish accepting 121 bytes and the new fatal-stderr regex. A leak test stops a regression to a process-wide chdir. Both branches of `at()` are exercised.

EDGE_CASES: PASS, with one item unmeasured.
- The file NAME alone over the limit cannot happen: `ValidSocketID` caps it at 64, giving a 76-byte name; the helper still returns a named error.
- linux without `/proc`: ENOENT, wrapped with the real path, so the failure is loud.
- A flag that is good beside a bad env: the flag wins and the env is not checked.
- A directory removed between `Resolve` and the bind: the bind error on stderr is the existing `magmux: socket …`, which `FATAL_MAGMUX_STDERR` catches.
- A failed revert: the thread is discarded, never reused.
- A relative `--sock-dir` is accepted today and exported relative in `MAGMUX_SOCK`; consider `filepath.Abs` in `ValidDir` (optional, not needed for G-10).
- UNMEASURED: `__pthread_fchdir` under madbench's macOS `home` Seatbelt profile. The socket directory is inside the run tree, which the profile allows, but verify with handoff step 3 (CMON-1 under `sandbox: home`) before claiming G-10 closed.
