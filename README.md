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

# A session driven from outside, with the control panel watching
magmux -c -e 'claude'
```

| Flag | |
|---|---|
| `-e CMD` | run CMD in a pane (repeatable) |
| `-g FILE` | grid file, one command per line |
| `-w` | exit once every pane is done |
| `-c` | add the [control panel](#the-control-panel) |
| `-x SECS` | close SECS after a driver finishes (default: wait for a keypress) |

## Controls

| Key | Action |
|-----|--------|
| `Ctrl-G q` | Quit |
| `Ctrl-G Tab` | Switch focus to next pane |
| Mouse click | Switch focus to clicked pane |
| Mouse drag | Select text (auto-copies to clipboard) |
| Mouse wheel | Scroll the [control panel](#the-control-panel) under the cursor |
| `↑` `↓` `PgUp` `PgDn` `Home` `End` | Scroll the control panel, when focused |

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
- **Controlled sessions** — drive a pane from the IPC socket, with a built-in
  [control panel](#the-control-panel) (`-c`) showing what was asked and what
  came back
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
| `control` | `dir` (`out`\|`note`), `pane`, `label`, `text`, `keys`, `at`, or `event`/`goal`/`summary` for notes | when a controlled session is driven (see below) |

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

## Controlled Sessions

A subscriber can also *drive* a pane, not just watch it. That closes the loop:
an external agent reads a session's state off the socket and pushes the next
instruction back in, guiding an interactive coding tool through a multi-step
task.

```bash
magmux -c \
  -e 'claude' \
  -e 'bun pilot/pilot.ts --pane 0 --goal "get the test suite green"'
```

## The Control Panel

`-c` adds a **control panel** — a magmux pane like any other, except it has no
process. magmux paints it itself, by writing ANSI through the pane's own VT
parser exactly as a child would, so it inherits borders, selection and
dirty-flag rendering and costs nothing while idle. It is built into the binary;
there is nothing to install and nothing to run.

It is the instrument panel for a controlled session: what was asked, what came
back, how long each turn took.

```
CONTROL PLANE                                       2m 40s
──────────────────────────────────────────────────────────
PILOT ══▶ SESSION                              [ WORKING ]
claude-sonnet-5                                     pane 0
4 sent  │  3 done  │  1 in flight              ███████░░░
goal get the test suite green, then clear every warning
──────────────────────────────────────────────────────────
 1 ✓ step 1/5   ████████░░░░░░░░  32.0s  Bash
 2 ✓ step 2/5   ████████████████  1m 14s Edit
 3 ✓ verify     █████░░░░░░░░░░░  21.0s  Bash
 4 ◐ step 3/5   ██░░░░░░░░░░░░░░  28.0s  Read
─────────────────────────────────── ▲ 4 earlier ──────────
▶ now run go vet ./... and list every warning
◀ go vet reported 4 findings across 3 files. All four are
  shadowed variables inside error branches — the inner err
  is assigned but never checked…
```

**Header.** The badge is the live state of the *driven session*, not the
driver: amber `WORKING`, green `AWAITING`, red on an error or permission block.
Below it, the driver's model and the pane it is steering.

**Counters.** `sent` is instructions pushed in; `done` is turns **magmux itself
observed** complete. `done` is never taken from the driver's word for it, so if
a driver claims work it never got, the two diverge visibly. The meter appears
only when the driver declared a planned step count.

**Ledger.** One row per step: number, outcome (`✓` done, `◐ ` in flight, `✗`
error, `⚠` permission block), the driver's own label, a duration bar, the
elapsed time, and the last tool the session used. The bars are scaled against
the slowest turn, so the column is a *comparison* — a row twice as long took
twice as long. That is the thing a driver's own log cannot show you.

**Exchange.** The full conversation, oldest first — every instruction and every
reply, in full, nothing truncated. It scrolls rather than cutting text off.

### Using it

| | |
|---|---|
| wheel over the panel | scroll, whichever pane has focus |
| `↑` `↓` `PgUp` `PgDn` | scroll (panel focused — `Ctrl-G Tab` to focus it) |
| `Home` / `g` | jump to the start of the run |
| `End` / `G` | resume following the newest exchange |
| `q` | close, once the run has finished |

The header shows `▲ N earlier` when there is history above the fold, and
`▲ N back · End to follow` once you have scrolled up, so you always know
whether you are looking at the live tail or the past.

When a driver declares the run over, the panel says so and how to leave:

```
 FINISHED  press q to close
   test suite green and every go vet warning cleared
```

It waits for that keypress by default — a finished run that vanishes before you
have read it is worse than one that lingers. Pass `-x SECS` to close
automatically instead; the countdown is shown, and **any keypress cancels it**.

**Inbound verbs:**

```jsonc
// announce what you are driving, so the panel can show it
{"type":"pilot","event":"start","pane":0,"goal":"...","steps":3,"model":"..."}

// type an instruction into a pane and submit it
{"type":"send","pane":0,"text":"run the tests","label":"step 1/3"}

// press named keys instead (enter, escape, tab, up/down/left/right,
// ctrl-a..ctrl-w, home/end/pageup/pagedown, or any single character)
{"type":"send","pane":0,"keys":["escape"],"enter":false}

// close out the run
{"type":"pilot","event":"finish","summary":"all green"}
```

`send` writes to a pane **even when it is idle** — steering a finished turn is
the entire point — and clears the pane's completion state exactly as a real
keystroke would. Multi-line text is wrapped in bracketed paste when the pane has
requested that mode, and Enter is sent after a short delay so the TUI has taken
the text before it is submitted.

**The panel's two directions come from different sources on purpose.** `▶` rows
are recorded from your `send`; `◀` rows are recorded only from what magmux
itself observed the pane do. A driver can never fabricate a completion, and the
panel can show a session disagreeing with what the driver believes it asked for.

The panel is driver-agnostic: anything that speaks these verbs on the socket
fills it in. `pilot/pilot.ts` below is one such driver, not a requirement — a
shell script piping JSON into `nc -U $MAGMUX_SOCK` gets the same panel.

### The pilot

`pilot/pilot.ts` is a reference driver built on [pi.dev](https://pi.dev). Its
toolbox is replaced with exactly two tools — `send_to_session` and `finish` —
where `send_to_session` blocks until magmux observes the turn settle and returns
the session's own answer as the tool result. The multi-step loop is therefore
just pi's ordinary agent loop, with no orchestration code on top.

It finds its multiplexer through `MAGMUX_SOCK`, so running it as a magmux pane
needs no configuration.

```bash
task pilot:check         # preflight: which models pi can use
task pilot:demo          # watch a pilot build a file line by line (cheap, scratch dir)
GOAL="get the tests green" task pilot
task test:pilot          # full e2e, asserts the artifact on disk
```

**Credentials.** Only the pilot needs them — magmux needs none, and the Claude
Code pane it drives uses its own auth. pi reads provider keys from the
environment it is started in; how they get there is up to you.
`task pilot:check` reports what it found.

## Why Go?

The original MTM is ~1,800 lines of C using ncurses. magmux is ~2,100 lines of Go with no ncurses dependency — raw ANSI escape codes to stdout. Go provides:

- Memory safety (no buffer overflows in escape sequence parsing)
- Goroutine-per-pane concurrency (simpler than C's `select()` loop)
- Static binary distribution (no ncurses/libc dependency)
- Accessible to teams that don't maintain C code

## License

MIT
