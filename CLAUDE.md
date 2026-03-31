# magmux - Development Notes

## Build

```bash
go build -o magmux .
```

## Architecture

Single-file Go program (~2,100 lines). Key components in order:

1. **Cell/Screen** — Cell struct (rune + Color + Attr), Screen buffer with scrollback
2. **VT Parser** — DEC ANSI state machine (port of vtparser.c), handles CSI/ESC/OSC/C0
3. **Pane** — Binary tree layout node, owns PTY + Screen + VT parser
4. **PTY helpers** — Raw /dev/ptmx + ioctls (no CGo)
5. **Renderer** — ANSI escape code output with dirty-flag optimization
6. **Multiplexer** — Main event loop, input routing, mouse handling, SIGWINCH
7. **Selection** — Mouse drag text selection + clipboard copy (OSC 52 + pbcopy)

## Key Design Decisions

- **TERM=screen-256color** — Apps self-limit to supported escape sequences. This eliminates the need to handle Kitty keyboard protocol, xterm extensions, etc.
- **Dirty-flag rendering** — Only redraw when pane content changes. Idle panes cost zero CPU/IO.
- **Mouse: tmux model** — Click switches focus. Drag selects text (normal mode). Alt-screen apps get mouse forwarded.
- **No ncurses/tcell** — Raw ANSI output. Simpler, fewer dependencies, full control.
- **Color type** — Supports default (-1), indexed (0-255), and truecolor (RGB) in a single struct.

## Dependencies

Only `golang.org/x/sys` (PTY ioctls) and `golang.org/x/term` (raw mode). Zero third-party.

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
