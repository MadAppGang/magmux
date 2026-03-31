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

## Why Go?

The original MTM is ~1,800 lines of C using ncurses. magmux is ~2,100 lines of Go with no ncurses dependency — raw ANSI escape codes to stdout. Go provides:

- Memory safety (no buffer overflows in escape sequence parsing)
- Goroutine-per-pane concurrency (simpler than C's `select()` loop)
- Static binary distribution (no ncurses/libc dependency)
- Accessible to teams that don't maintain C code

## License

MIT
