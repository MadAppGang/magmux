# magmux

Minimal terminal multiplexer written in Go. Zero third-party dependencies.

A port of [MTM](https://github.com/deadpixi/mtm) (Rob King) from C to Go, designed as a lightweight pane splitter for running multiple terminal applications side by side.

## Install

```bash
# Homebrew (macOS/Linux)
brew tap MadAppGang/tap && brew install magmux

# Go install
go install github.com/MadAppGang/magmux@latest

# From source
git clone https://github.com/MadAppGang/magmux
cd magmux && go build -o magmux .
```

## Usage

```bash
# Default: 3 panes with your shell
magmux

# Custom commands in each pane
magmux -e 'htop' -e 'vim' -e 'bash'

# Two coding agents side by side
magmux -e 'claude' -e 'opencode'
```

## Controls

| Key | Action |
|-----|--------|
| `Ctrl-G q` | Quit |
| `Ctrl-G Tab` | Switch focus to next pane |
| Mouse click | Switch focus to clicked pane |
| Mouse drag | Select text (auto-copies to clipboard) |

## Layout

With 3 commands, magmux creates this layout:

```
┌──────────────────┬──────────────────┐
│   Command 1      │   Command 2      │
│   (top-left)     │   (top-right)    │
├──────────────────┴──────────────────┤
│   Command 3 (bottom)                │
├─────────────────────────────────────┤
│   Status bar                        │
└─────────────────────────────────────┘
```

## Features

- **Pane splitting** — horizontal and vertical with binary tree layout
- **VT-100 terminal emulation** — DEC ANSI state machine parser
- **256-color + truecolor** — full SGR support including RGB
- **Mouse support** — click to focus, drag to select, auto-copy to clipboard
- **Line drawing characters** — G0/G1 charset switching for TUI borders
- **Alt screen** — proper handling for vim, htop, Claude Code, OpenCode
- **SIGWINCH** — automatic resize when terminal size changes
- **Scrollback buffer** — 1000 lines per pane
- **Zero dependencies** — only `golang.org/x/sys` and `golang.org/x/term`

## Architecture

```
Host Terminal
  └── magmux (raw mode + mouse tracking)
       ├── Pane 1 (PTY + VT parser + screen buffer)
       ├── Pane 2 (PTY + VT parser + screen buffer)
       └── Pane 3 (PTY + VT parser + screen buffer)
```

Each pane runs a goroutine reading from its PTY, parsing VT escape sequences into a cell grid. The render loop checks dirty flags and only redraws when content changes.

Key design: child processes see `TERM=screen-256color`, which limits escape sequences to what the multiplexer supports — the same approach tmux and MTM use.

## Configuration

| Env Variable | Default | Description |
|---|---|---|
| `MAGMUX_SEL_FG` | `0` (black) | Selection foreground (256-color index) |
| `MAGMUX_SEL_BG` | `220` (yellow) | Selection background (256-color index) |
| `MAGMUX_DEBUG` | (unset) | Enable debug logging to `/tmp/magmux-debug.log` |

## IPC Socket Protocol

magmux exposes a Unix-domain socket that external tools can subscribe to for live
pane state — this is the **stable integration API** (used by [madbench](https://github.com/MadAppGang/madbench)).

**Path scheme.** On startup magmux binds `/tmp/magmux-<pid>.sock`, where `<pid>`
is the PID of the magmux process. The same path is exported to child processes
via the `MAGMUX_SOCK` environment variable. A parent that spawned magmux knows
its child PID and derives the path directly.

**Framing.** JSON Lines: one JSON object per line, terminated by `\n` (UTF-8).
The stream is bidirectional but subscribers typically only read. Unknown event
types and unknown fields should be ignored for forward compatibility.

**Lifecycle & ordering guarantees:**

1. **On connect** the subscriber immediately receives one aggregate `snapshot`
   event carrying the current state of every pane. A subscriber that connects
   *after* some panes have already exited still gets full state from this event.
2. **During the run** magmux pushes `snapshot` (per-pane, on meaningful change)
   and `exit` (per-pane, on completion) events as they happen.
3. **On shutdown** magmux broadcasts a final `results` event (authoritative
   final state of all panes), then a `shutdown` event, then flushes and closes
   each connection. The `results` event is **guaranteed to arrive before EOF**,
   and before `shutdown`. Teardown is bounded (~2s) so a wedged subscriber
   cannot hang magmux's exit.

**Events pushed to subscribers (`type` → fields):**

| `type` | Shape | When |
|---|---|---|
| `snapshot` (connect) | `panes`: array of pane-state objects (see below) | once, immediately on connect |
| `snapshot` (per-pane) | `pane` (int), `controller`, `state`, `project`, `model`, `prompt`, `response`, `tool`, `startedAt`, `completedAt` | on meaningful per-pane change |
| `exit` | `pane` (int), `exitCode` (int), `duration`, `lastLine`, `response`, `prompt`, `tool`, `model` | when a pane's process exits |
| `results` | `panes`: array of pane-state objects, `endedAt` (RFC3339) | once, at shutdown, before EOF |
| `shutdown` | (no extra fields) | once, after `results`, right before close |

Disambiguate the two `snapshot` variants by field: the connect-time aggregate
carries a **`panes`** array; the per-pane live event carries a singular **`pane`** index.

**Pane-state object** (elements of the `panes` array in `snapshot`/`results`):

| Field | Type | Notes |
|---|---|---|
| `pane` | int | pane index, stable for the process lifetime; correlates to `-e` argument order |
| `state` | string | `completed` \| `failed` \| `awaiting_input` \| `running` |
| `exitCode` | int | process exit code (0 until exit) |
| `dead` | bool | whether the pane's process has exited |
| `controller` | string | controller name, if any (e.g. `claude`) |
| `model` | string | model name, if detected (omitted when empty) |
| `project` | string | project name, if detected (omitted when empty) |
| `prompt` | string | last user prompt, if any (omitted when empty) |
| `response` | string | last response, if any (omitted when empty) |
| `tool` | string | last tool used, if any (omitted when empty) |
| `startedAt` | string | RFC3339, if known (omitted when zero) |
| `completedAt` | string | RFC3339, if known (omitted when zero) |

Inbound messages (subscribers writing to magmux) drive status-bar text, pane
tints, and agent hook events; see `sockMsg` in `main.go`. These are optional and
unrelated to the read-only subscribe flow above.

## Why Go?

The original MTM is ~1,800 lines of C using ncurses. magmux is ~2,100 lines of Go with no ncurses dependency — raw ANSI escape codes to stdout. Go provides:

- Memory safety (no buffer overflows in escape sequence parsing)
- Goroutine-per-pane concurrency (simpler than C's `select()` loop)
- Static binary distribution (no ncurses/libc dependency)
- Accessible to teams that don't maintain C code

## License

MIT
