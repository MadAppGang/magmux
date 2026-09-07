# Theme resolution: how the palette is chosen, and how to check it

Shipped in v0.10.0. The mechanism is documented in `theme.go` (file header,
`resolveTheme`, `themeWord`, `classifyColorFGBG`) and in the CLAUDE.md
headless invariant. This note records what is NOT derivable from the code:
the decisions behind it and a verified way to observe it from outside.

## The order, and the one rule people get wrong

```
--theme > MAGMUX_THEME > TERM_THEME > OSC 11 probe > COLORFGBG > dark
```

`auto` is "no opinion" at EVERY level, `--theme` included. Before v0.10.0,
`--theme auto` beat a set `MAGMUX_THEME` and forced the probe. That was
retired on purpose: two normalisers with different `auto` semantics was the
"two definitions" shape this repo treats as a defect, so `themeSetting` was
deleted and `themeWord` is the one normaliser. Consequence: there is no CLI
value that forces the probe over a set `MAGMUX_THEME`. Unset the variable.

`TERM_THEME` is not magmux's variable. Unknown values (`system`, `solarized`)
are ignored silently, by construction: `resolveTheme` is pure and cannot
write to stderr. `--theme` keeps its bad-value warning via
`validThemeSetting`, which is now flag-validation only.

`COLORFGBG` sits AFTER the probe. A measured colour beats an index the shell
inherited. It is reached when the probe is skipped (headless, non-tty,
`TERM=dumb`, which is a nil probe callback) or ran and got nothing. Before
v0.10.0 a silent probe went straight to dark.

## Children can still disagree (open follow-up)

Children inherit `os.Environ()` unfiltered. A child that follows the same
rule reads the user's `TERM_THEME=light`, never asks OSC 11, and ignores
magmux's `--theme dark`. The OSC 11 answer is consistent with magmux's own
palette; the environment is not rewritten. Decide separately whether magmux
should export `TERM_THEME` to children set to the resolved value.

## How to observe it from outside (verified 2026-09-08)

A private tmux server is enough, and it has a useful property: tmux does
NOT answer `OSC 11;?` from a detached server, so the probe is silent there
and every "probe ran and got nothing" path is reachable.

```
tmux -L t -f /dev/null new-session -d -s t -x 100 -y 32
tmux -L t new-window -d -n light -t t -e TERM_THEME=light \
  "./magmux -c -e \"printf '\\033]11;?\\a'; head -c 40 | od -c; sleep 600\""
tmux -L t capture-pane -p -e -t t:light > light.ansi
```

Three signals in that capture:

1. The child's pane echoes the reply magmux sent, e.g.
   `^[]11;rgb:efef/f1f1/f5f5^G` for the light palette.
2. The control panel's startup column is about 12ms when a word-valued
   source answered and about 166ms when the 150ms probe ran and timed out.
   That difference IS "TERM_THEME skips the probe", observable without
   reading code.
3. `grep -o '38;2;11;87;208' light.ansi` (light accent) versus
   `38;2;137;180;250` (dark accent): one of them is zero in every capture.

Render with the go-tui skill's `ansi-to-png.ts`. It paints a black page, so
a light-palette capture looks wrong there; render it with `aha` and a
`<body style="background:#EFF1F5;color:#4C4F69">` to see it on Latte.

Gotchas hit on the way: `tmux send-keys` with nested quotes trips the
terminal plugin's occupancy hook, so pass the command to `new-window`
instead. `MAGMUX_DEBUG=1` truncates `/tmp/magmux-debug.log` per process,
so with several instances only the last writer's `theme:` line survives.
