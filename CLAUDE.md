# magmux - Development Notes

## Build

```bash
go build -o magmux .
```

## Architecture

~4,800 lines of Go across a few files. `main.go` (~3,950 lines) holds the
terminal core; the tool-controller layer lives beside it.

`main.go`, in order:

1. **Cell/Screen** — Cell struct (rune + Color + Attr), Screen buffer with scrollback
2. **VT Parser** — DEC ANSI state machine (port of vtparser.c), handles CSI/ESC/OSC/C0
3. **Pane** — Binary tree layout node, owns PTY + Screen + VT parser
4. **PTY helpers** — Raw /dev/ptmx + ioctls (no CGo); platform bits in `pty_darwin.go` / `pty_linux.go`
5. **Renderer** — ANSI escape code output with dirty-flag optimization
6. **Multiplexer** — Main event loop, input routing, mouse handling, SIGWINCH
7. **Selection** — Mouse drag text selection + clipboard copy (OSC 52 + pbcopy)

Tool controllers:

- `controller.go` — the `ToolController` interface and the `Snapshot` /
  `ControllerState` surface every controller produces, plus the optional
  `InputNotifier` interface.
- `controller_claude.go` — observes a Claude Code pane by tailing its JSONL
  transcript under `~/.claude/projects/`.

Controlled sessions (an external AI agent steering a pane):

- `pilot.go` — the `send` socket verb and PTY injection. The inbound half.
- `control.go` — the control panel: the OUT/IN log of pilot↔session traffic,
  painted into a PTY-less pane.
- `pilot/pilot.ts` — the pi.dev agent that does the steering, with its
  toolbox replaced by `send_to_session` + `finish`.
- `pilot/magmux.ts` — its socket bridge.
- `test/ui/case3.ts` — the visual end-to-end case for the whole loop.

## Key Design Decisions

- **TERM=screen-256color** — Apps self-limit to supported escape sequences. This eliminates the need to handle Kitty keyboard protocol, xterm extensions, etc.
- **Dirty-flag rendering** — Only redraw when pane content changes. Idle panes cost zero CPU/IO.
- **Mouse: tmux model** — Click switches focus. Drag selects text (normal mode). Alt-screen apps get mouse forwarded.
- **No ncurses/tcell** — Raw ANSI output. Simpler, fewer dependencies, full control.
- **Color type** — Supports default (-1), indexed (0-255), and truecolor (RGB) in a single struct.

### Controller invariants

These are easy to re-break; each caused a filed bug or cost real debugging time.

- **Pane idleness has two independent sources, and they must be reconciled.**
  A pane learns it is idle from the terminal (OSC 9 notification, bracketed-paste
  cycle, window title, text-idle timeout → `Pane.inputReady`), and a controller
  learns it from the tool's own transcript. Neither is complete: the transcript's
  `stop_hook_summary` only exists when the user has a Stop hook configured, and
  the terminal heuristics don't know about turns. `applyTerminalIdle` merges
  them, ordering the two by `Pane.inputReadyAt` vs the controller's
  `lastApplyAt` so the fresher signal wins and the state cannot oscillate.
  The rule is that the live `snapshot` event and the shutdown `results` event
  must never disagree — `results` reads `inputReady`, so a controller that
  ignores it will contradict itself (issue #2).

- **`~/.claude/projects/<dir>` naming is an undocumented contract we don't own.**
  Claude Code replaces **every** non-alphanumeric character with `-` (so `.`,
  `_` and spaces too, not just `/`) — see `encodeProjectDir`. Because Claude
  Code can change this at any time, and because a pane's real cwd may differ
  from magmux's (`cd /foo && claude '...'`), the directory name is never the
  only way in: discovery falls back to scanning every project directory and
  matching on transcript content. Keep it that way — a wrong directory name
  otherwise strands the controller in `starting` silently and forever.

- **Controller idle state is one-way, so injected input must un-stick it.**
  `applyTerminalIdle` promotes a snapshot to `awaiting_input` and then refuses
  to touch it ("already settled"); only a new transcript entry moves it back
  to working. That is fine for a human typing — Claude Code writes the
  submitted prompt to its transcript immediately — but transcript discovery
  can lag or fail outright, and then a pilot's `send` would leave the state
  settled and the pilot would wait forever for a turn that had already begun.
  `sendToPane` therefore calls `InputNotifier.NotifyInput`, and the next
  `Poll` demotes a settled snapshot to working. The demotion is deliberately
  *not* sticky: if the tool ignores the instruction, the idle heuristics
  settle the pane again, so a dropped instruction surfaces as a suspiciously
  fast empty turn rather than a permanent "working".

### Controlled-session invariants

- **`send` must bypass `writePTY`, and that is the whole point.** In grid mode
  `writePTY` suppresses input to a pane that is `dead || inputReady`, so a
  finished grid can be dismissed with `q`. But `inputReady` is exactly the
  state a pilot wants to act on — the session finished its turn and is waiting
  for the next instruction. Pilot sends therefore take `injectPTY`, which
  skips that guard while still clearing the completion state (tint, overlay,
  `inputReady`, `hadTextOutput`) the way a real keystroke would. Route a pilot
  send through `writePTY` and every instruction after the first is silently
  dropped.

- **The panel's two directions come from different places, and must stay that
  way.** `▶ OUT` rows are recorded from the pilot's own `send`; `◀ IN` rows are
  recorded only from `pollControllers`, i.e. from what magmux itself observed
  the pane do. A pilot can therefore never fabricate a completion, and the
  panel can show the session disagreeing with what the pilot believes it
  asked for. Relatedly, an IN row only counts when an instruction is
  outstanding (`sent > observed`) — a session is idle from the moment it
  boots, and counting that as a completed turn showed `done 1` against zero
  instructions and drove the progress meter on a run that had not started.

- **The control pane has no process, so every "every pane" loop must skip it.**
  It is never dead and never goes idle, so `allPanesDone` counting it means
  `-w` can never fire; `startReadLoops` and `waitForChild` would dereference a
  nil PTY and nil `cmd`. It also has no child to redraw it on SIGWINCH, so the
  resize path must repaint it explicitly or it comes back blank.

## Dependencies

Go: only `golang.org/x/sys` (PTY ioctls) and `golang.org/x/term` (raw mode).
Zero third-party — the control panel is raw ANSI written through the pane's own
VT parser rather than a TUI library, which is what keeps that true.

The pilot is separate and out-of-process by design: it is TypeScript
(`@earendil-works/pi-coding-agent`, run with bun), talks to magmux only over
the documented socket, and nothing in the Go binary depends on it.

## Release

Uses GoReleaser. To release:

1. Tag: `git tag -a v0.1.0 -m "Initial release"`
2. Push: `git push origin main --tags`
3. CI builds binaries for darwin/linux (arm64/amd64)
4. GoReleaser creates GitHub Release + updates Homebrew formula

## VT Parser Coverage

Covers ~95% of tmux's escape sequences. See the gap analysis in the research docs. Key sequences handled:

- CSI: A-H (cursor), J/K (erase), L/M (lines), P/@ (chars), S/T (scroll), m (SGR), r (scroll region), n (DSR)
- ESC: 7/8 (cursor save/restore), D/M/E (index), c (reset), (0/(B (charset)
- DEC modes: 1049/47/1047 (alt screen), 2004 (bracketed paste), 1004 (focus), 1000/1002/1006 (mouse)
- SGR: 0-9, 21-29, 30-49, 53/55, 90-107, 38/48;5;N, 38/48;2;R;G;B
