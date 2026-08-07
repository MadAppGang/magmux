# Bug: panes running plain (non-TUI) commands are never marked "done"; `-w` never auto-exits

**Severity:** blocker — prevents `-w` from being usable for any non-TUI workload (CI smoke tests, scripted runs, batch comparison harnesses).

**Magmux version:** `0.4.1 (e7d80b0)` — same commit reported by both the released binary at `/opt/homebrew/bin/magmux` and a fresh `go build` of the working tree at HEAD `e7d80b0`.

**Platform:** macOS (Darwin 25.4.0, arm64, Apple Silicon).

**Reporter:** Jack Rudenko, while validating madbench's `pkg/runner/magmux` adapter (`madbench` repo, Phase 7 smoke test).

---

## Summary

When magmux runs panes whose command is a plain (non-TUI) shell invocation
— e.g. `sh -c "echo X; sleep 1"` — the pane **never transitions to "done"**
in the status bar, the DONE/FAIL overlay never renders, and `-w` (auto-exit
when all panes done) **never fires**. The status bar stays at
`<N>/<total> done, <M> running` indefinitely, even after the child
processes have exited and been reaped by the kernel.

This affects both `-e` and `-g` invocations and reproduces with a single
pane, so it is not an `-e`-specific or multi-pane race.

## Steps to reproduce

All four cases were run from a real PTY (tmux pane on `/dev/ttys008`) and
observed for at least 5 seconds. `-w` is present in every invocation.

### Case 1 — `-e`, two panes

```bash
magmux \
  -e 'sh -c "echo hi from magmux pane; sleep 1"' \
  -e 'sh -c "echo second pane; sleep 1"' \
  -w
```

**Expected:** both panes complete, both DONE popups render, magmux auto-exits within ~1.5s.

**Actual:** sometimes 1/2 done after the first pane's overlay renders, sometimes 0/2 done. Never auto-exits. Status bar reads `* magmux v0.4.1 │ 1/2 done │ 1 running │ 1.1s` (or `0/2 done`) and stays there until the user presses `q`.

### Case 2 — `-g` grid file, two panes

```bash
printf 'sh -c "echo dev-A; sleep 1"\nsh -c "echo dev-B; sleep 1"\n' > /tmp/grid.txt
magmux -g /tmp/grid.txt -w
```

**Expected:** identical to case 1.

**Actual:** `0/2 done, 2 running` after 5+ seconds. Never auto-exits. Identical bug regardless of `-e` vs `-g`.

### Case 3 — `-e`, single pane

```bash
magmux -e 'sh -c "echo solo; sleep 1"' -w
```

**Expected:** pane runs for ~1s, DONE popup renders, magmux auto-exits.

**Actual:** `0/1 done, 1 running` after 5+ seconds. Never auto-exits.

### Case 4 — `task test:single-good` from this repo's Taskfile

```bash
cd /Users/jack/mag/magmux
task test:single-good
```

This is a one-pane grid invocation that the Taskfile already documents as "expect green ✓ DONE popup with duration + last output". On `e7d80b0`, the popup does eventually render in some runs but the auto-exit (`-w`) is not present in this target — so it doesn't surface the auto-exit bug, only the timing variance in DONE detection. Mentioning here because it's a likely starting point for the fix.

## Out-of-band evidence: children are reaped, status is stale

While magmux was hung in case 3, a sibling shell ran:

```bash
ps -o pid,ppid,stat,command -p <magmux-pid>
# 95579   85573 S+   magmux -e sh -c "echo solo; sleep 1" -w

pstree <magmux-pid>
# (no descendants)
```

The `sh` child has exited and been reaped by the kernel (zero descendant
processes), but magmux's status bar still shows the pane as "running" and
no overlay has rendered. Whatever signal magmux uses to mark a pane
"done" did not fire for these plain commands.

## What this is NOT

- Not `-e`-specific (case 2 reproduces with `-g`).
- Not a multi-pane race (case 3 reproduces with one pane).
- Not a missing TTY — reproduction was done inside a real tmux pane (`tty` reported `/dev/ttys008`).
- Not a working-tree regression — `git status` is clean and HEAD is `e7d80b0`, the latest tagged commit on `main`.
- Not specific to `bash` vs `sh` — both reproduce. The common factor is "plain command, no alternate-screen, no controller attached".

## Code-reading notes (offered as starting points, not as a diagnosis)

I read the spawn + completion paths to narrow the hypothesis space, but
**I have not identified the bug** — these are notes a maintainer can use
as a head start, not a root-cause claim.

- `main.go:3835-3839` — `waitForChild` goroutines DO start for both `-g`
  and `-e` modes (`useGrid = true` is set at lines 3785 and 3788
  respectively). The waiters are spawned.
- `main.go:3262-3309` — `waitForChild` calls `p.cmd.Wait()`, then sets
  `p.dead = true` and triggers the DONE/FAIL overlay. Logic looks correct.
- `main.go:1251-1290` — `spawnPTY` performs the canonical
  `exec.Command` + `Setsid/Setctty` + `cmd.Start` + parent-side
  `pts.Close()` sequence. PTY parenting looks textbook-correct.
- `main.go:3456-3465` — text-idle "inputReady" path is **gated to**
  `(p.altMode || p.controller != nil)`, so plain `sh -c` panes (no alt
  screen, no controller) cannot reach "done" via the idle heuristic —
  they are 100% dependent on the `p.dead` path. This is by design per
  the comment, so it isn't the bug, but it explains why the failure mode
  is total (no fallback path) for non-TUI workloads.
- `main.go:3502` — the `done` predicate `if p.dead || p.inputReady`
  is consistent with the above and looks correct.

What I did **not** read:

- `pty_darwin.go` / `pty_linux.go` — the platform-specific PTY open path.
- The read-loop / EOF handling for the master side (`ptmx`).
- Whether `cmd.Wait()` actually returns for these processes. The kernel
  reaps the child (no descendants in `pstree`), but magmux's state stays
  stale. That's compatible with `Wait` blocking, with the `dead` flag
  not propagating, with a render-loop staleness, or with a channel /
  goroutine deadlock. I did not narrow this further.

## Impact

This blocks any use of magmux as a programmable / scripted multi-agent
multiplexer. Specifically: madbench's magmux runner adapter
(`pkg/runner/magmux/`) cannot complete its Phase 7 live smoke test
because the parent process (`madbench`) waits for `magmux` to exit, and
magmux never does.

## Suggested first step for whoever picks this up

Add a debug log line at the top of `waitForChild` ("entering Wait for
pid X") and another immediately after `p.cmd.Wait()` returns ("Wait
returned for pid X, err=…"). Run case 3 with `-w` and a log file. If
the second log line never appears, `Wait` is blocking and the next
question is why. If it does appear but the overlay never renders, the
bug is downstream of `Wait` (state propagation / render loop).

---

*Filed-from*: `/Users/jack/mag/madbench/ai-docs/sessions/dev-feature-magmux-runner-20260430-191130-418560b6/validation/phase7-smoke-blocker.md` (madbench Phase 7 validation).
