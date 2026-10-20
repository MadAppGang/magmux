# magmux

Minimal terminal multiplexer written in Go. Zero third-party dependencies.

A port of [MTM](https://github.com/deadpixi/mtm) (Rob King) from C to Go, designed as a lightweight pane splitter for running multiple terminal applications side by side.

## Install

```bash
# Homebrew (macOS/Linux)
brew tap MadAppGang/tap && brew install magmux

# Go install — note the /cmd/magmux suffix, which is new: the repo root is
# no longer a main package. `@latest` without it will not build.
go install github.com/MadAppGang/magmux/cmd/magmux@latest

# From source
git clone https://github.com/MadAppGang/magmux
cd magmux && go build -o magmux ./cmd/magmux
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

# Named panes, so a client can address them by name instead of by index
magmux -e 'bun test --watch' --label tests -e 'bun dev' --label server

# No terminal at all: CI, a benchmark harness, an agent. Reads `results`
# off the socket and exits with every pane's real exit status.
magmux --headless -w -e 'bun test' -e 'go test ./...'
magmux -w -e 'bun test' < /dev/null      # the same thing; headless is automatic
```

magmux shows nothing of itself by default. `magmux -e 'claude'` is a bare
terminal running Claude Code: one pane, no border, the whole window. The
[control panel](#the-control-panel) is always there — it just starts hidden,
and `Ctrl-G p` reveals it without losing a row of its history. `Ctrl-G s` does
the same for the status bar.

| Flag | |
|---|---|
| `-e CMD` | run CMD in a pane (repeatable) |
| `--label NAME` | name the pane the preceding `-e` created |
| `-g FILE` | grid file, one command per line |
| `-w` | exit once every pane is done ([needs at least one `-e`/`-g`](#headless)) |
| `--headless` | [run with no terminal](#headless); automatic when stdin is not a tty |
| `-c` | start with the [control panel](#the-control-panel) visible |
| `--no-status` | start with the status bar hidden |
| `--no-idle-done` | an idle pane is resting, not finished: no ✓ DONE, and `-w` waits for exit |
| `-x SECS` | close SECS after a driver finishes (default: wait for a keypress) |
| `--id NAME` | bind `magmux-NAME.sock` instead of the pid socket (not all digits) |
| `--sock-dir DIR` | bind the IPC socket in DIR instead of `/tmp`, at any depth; exits 1 if DIR is unusable |

[Remote control](#remote-control) — off unless you ask for it:

| Flag | |
|---|---|
| `--listen ADDR` | serve HTTP, WebSocket and SSE on ADDR (`127.0.0.1:7777`, `:7777`) |
| `--token-file PATH` | read the bearer token from PATH instead of generating one |
| `--view-token-file PATH` | a second, READ-ONLY token from PATH |
| `--view-op NAME` | let the view token call this plugin op too (repeatable, `plugin.op`) |
| `--tls-cert FILE` / `--tls-key FILE` | serve HTTPS/WSS; both or neither |
| `--allow-origin ORIGIN` | allow a browser Origin (repeatable) |
| `--firebase FILE` | [mirror the session](#firebase) into a Firebase Realtime Database |
| `--plugin CMD` | run CMD as a [plugin](#plugins) (repeatable) |

Subcommands: [`magmux mcp`](#magmux-mcp) runs magmux as an MCP server.

## Controls

| Key | Action |
|-----|--------|
| `Ctrl-G q` | Quit |
| `Ctrl-G Tab` | Switch focus to next pane |
| `Ctrl-G p` | Show / hide the [control panel](#the-control-panel) |
| `Ctrl-G s` | Show / hide the status bar |
| `Ctrl-G r` | Return to the terminal's size after a client [`resize`](#screen-size) |
| `Ctrl-G [` | Scroll the focused pane back through its [scrollback](#scrollback) |
| Mouse click | Switch focus to clicked pane |
| Mouse drag | Select text (auto-copies to clipboard) |
| Mouse wheel | Scroll the pane under the cursor — the [control panel](#the-control-panel), or an ordinary pane's scrollback. Forwarded to full-screen apps |
| `↑` `↓` `PgUp` `PgDn` `Home` `End` | Scroll the control panel, when focused |

`Ctrl-G [` scrolls back one page and puts the pane in **scroll mode**: `k`/`j`
and `↑`/`↓` move a line, `PgUp`/`PgDn` and `b`/`space` move a page, `g` and `G`
jump to the oldest line and back to live, and `q`, `Enter` or `Esc` leave. A
badge in the pane's top-right corner shows how far back you are and how to get
out. Nothing typed in scroll mode reaches the child; outside it, every one of
those keys goes to the child untouched, which is what a full-screen TUI needs.

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

**Every pane needs at least 3 rows and 20 columns**, and magmux **refuses** a
layout that cannot give every pane that much rather than producing panes too
small to show a prompt and a line of output. A column of `k` panes needs
`4k - 1` rows (3 each plus a border between), and `-e` fills two columns, so the
practical ceiling is about **12 panes on an 80×24 terminal** and 24 on 80×50. On
a terminal narrower than 41 columns only a single pane is possible, since
`2 × 20 + 1` will not fit.

The refusal names the count, the geometry and the floor:

```
$ magmux --headless -e 'echo 1' … 40 times …
magmux: cannot lay out 40 panes in 80x23: every pane needs at least 3 rows and
20 columns (use fewer -e commands, a larger terminal, or set COLUMNS/LINES)
```

Two asymmetries worth knowing. `-c` **costs a pane** — the panel is appended to
the layout, so 12 panes succeed and 12 panes with `-c` refuse; without `-c` the
panel lives outside the tree and costs nothing. And the bare `magmux` layout
(no `-e`, no `-g`) is exempt: magmux chose that pane count itself, so there is
no caller to have made a mistake and three cramped shells beat refusing to
start.

The same floor governs `Ctrl-G p` and an agent's `open_pane`, deliberately —
"usable" cannot mean one thing for a flag and another for a socket verb.

## Headless

`--headless` runs magmux with **no terminal at all**: no raw mode, no alternate
screen, no OSC 11 probe, and **not one byte on stdout**. The socket is the whole
interface. It is what makes magmux usable from CI, from a benchmark harness, or
from an agent that has no tty to give it — and it is the reason several magmuxes
can run at once, since a run no longer needs a pty each. Only the probe is
skipped: `--theme`, `MAGMUX_THEME`, `TERM_THEME` and `COLORFGBG` still pick the
palette children are told about over OSC 11, so a headless run can be told it
is on a light terminal.

It turns on **automatically when stdin is not a terminal**, so the shape that
used to fail —

```
$ magmux -w -e 'echo hi' < /dev/null
magmux: raw mode: operation not supported by device
```

— now just works, with no flag. Pass `--headless` explicitly to force the mode
from inside a real terminal.

What does *not* change: the render loop still runs (it is what drives controller
polling, the `snapshot` broadcast and `-w`'s auto-exit — only the *write* is
suppressed), every pane still spawns, `results` has exactly the shape it always
had, and the control panel still exists and still reports `state: "panel"`.

Geometry comes from `COLUMNS`/`LINES`, else 80×24. That means a headless magmux
started **inside** a magmux pane inherits that pane's size, since those are the
two variables magmux exports to its own children — deliberate, so the nested
run's captures line up with the space a human is looking at.

**`-w` needs a session to wait for.** `magmux --headless -w` with no `-e`/`-g`
falls into the default shell layout and never auto-exits, and neither does
`magmux --headless -w -c` with no `-e`, because the control panel is not a
session. Both are legitimate — a controller-driven run should not exit on its
own — but they are indistinguishable from a hang, so end them with `kill -TERM`,
which magmux turns into its ordinary teardown (`results`, then `shutdown`, then
EOF for every subscriber).

## Features

- **Pane splitting** — horizontal and vertical with binary tree layout
- **VT-100 terminal emulation** — DEC ANSI state machine parser
- **256-color + truecolor** — full SGR support including RGB
- **Mouse support** — click to focus, drag to select, auto-copy to clipboard
- **Line drawing characters** — G0/G1 charset switching for TUI borders
- **Alt screen** — proper handling for vim, htop, Claude Code, OpenCode
- **SIGWINCH** — automatic resize when terminal size changes
- **[Scrollback](#scrollback)** — 1000 lines per pane, primary screen only,
  readable by a human (`Ctrl-G [`, wheel) and by an agent (`capture` /
  `read_pane` with an offset)
- **Controlled sessions** — drive a pane from the IPC socket, with a built-in
  [control panel](#the-control-panel) (`-c`) showing what was asked and what
  came back
- **[Remote control](#remote-control)** — `--listen` serves the same ops over
  HTTP, and a WebSocket carries live pane frames, so a browser can watch and
  type without a terminal emulator
- **[Plugins](#plugins)** — a separate process adds ops that every transport can
  call, and can own a pane and report its state
- **Zero dependencies** — only `golang.org/x/sys` and `golang.org/x/term`

## Scrollback

Each pane keeps the last **1000 rows** that scrolled off the top of its screen —
`MAGMUX_SCROLLBACK` changes the number, `0` turns it off. The ring fills lazily
and drops the oldest row when it is full, so a pane that never scrolls costs
nothing and a busy one costs a bounded amount (roughly `rows × cols × 20` bytes,
about 4 MB for a full 1000-row ring at 200 columns).

Three ways in:

- **`Ctrl-G [`** scrolls the focused pane back a page and enters scroll mode
  (see [Controls](#controls)).
- **The mouse wheel** over an ordinary pane.
- **`capture` / `read_pane` with an `offset`**, for an agent — see
  [Read verbs](#read-verbs).

**Only the primary screen records, and that is the rule to know.** A
full-screen app — Claude Code, `vim`, `htop`, `less` — runs on the terminal's
*alternate* screen, which no terminal records into history; it is why quitting
`vim` does not leave its buffer in your scrollback. Switching to the alternate
screen and back leaves the primary's history exactly as it was.

The practical consequence is a division of labour rather than a gap:

| Pane | History lives in |
|---|---|
| shell, build, test run, dev server | **scrollback** — it has no transcript |
| Claude Code and other agent sessions | **the transcript** (`{"type":"transcript"}`, `read_pane transcript:N`) — it has little or no scrollback |

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

Everything that can drive the session goes through one place:

```
  unix socket ─┐                      ┌─ ops (built-in + plugin)
  HTTP / WS ───┤                      │
  SSE ─────────┼──▶  hub  ──▶ mux ────┼─ panes, screens, the layout
  magmux mcp ──┤   (registry,         │
  Firebase ────┘     bus, lanes)      └─ events, live frames
```

The hub is the application port: it knows an op's name, its schema and who may
call it, and it knows how to turn one event into bytes on every subscriber's
connection. It has never heard of a pane. Each transport is an adapter that
hands it a caller and a connection — which is why the socket, a browser and an
agent are told the same things in the same words, and why adding a transport
adds no cases anywhere else.

## Configuration

The palette is resolved once at startup, first answer wins: `--theme`,
`MAGMUX_THEME`, `TERM_THEME`, the OSC 11 probe (interactive tty only),
`COLORFGBG`, then dark. `auto` is "no opinion" at every level and falls through.

| Env Variable | Default | Description |
|---|---|---|
| `MAGMUX_THEME` | `auto` | `light`, `dark` or `auto`. Second in the order: `--theme` wins over it, and it beats everything below. Set it when a terminal answers OSC 11 wrongly |
| `TERM_THEME` | (unset) | `light` or `dark`, as set by the terminal or shell. When it answers, magmux does not probe the terminal at all. Any other value (including `auto`) is ignored without a warning |
| `COLORFGBG` | (unset) | `fg;bg` or `fg;x;bg` colour indexes (rxvt, konsole, iTerm2). Consulted only when the probe was skipped or did not answer; background 0-6 or 8 is dark, 7 or 9-15 is light |
| `MAGMUX_SCROLLBACK` | `1000` | Lines of [history](#scrollback) kept per pane; `0` turns it off |
| `MAGMUX_SEL_FG` | `0` (black) | Selection foreground (256-color index) |
| `MAGMUX_SEL_BG` | `220` (yellow) | Selection background (256-color index) |
| `MAGMUX_SOCK_DIR` | `/tmp` | Directory for the [IPC socket](#ipc-socket-protocol). `--sock-dir` wins over it; an unusable directory makes magmux and `magmux mcp` exit 1 |
| `MAGMUX_DEBUG` | (unset) | Enable debug logging to `/tmp/magmux-debug.log` |

## IPC Socket Protocol

magmux exposes a Unix-domain socket that external tools can subscribe to for live
pane state — this is the **stable integration API** (used by [madbench](https://github.com/MadAppGang/madbench)).

**Path scheme.** On startup magmux binds `/tmp/magmux-<pid>.sock`, where `<pid>`
is the PID of the magmux process. The same path is exported to child processes
via the `MAGMUX_SOCK` environment variable. A parent that spawned magmux knows
its child PID and derives the path directly.

`--id NAME` binds `/tmp/magmux-<name>.sock` instead — the path is then known
*before* magmux starts, which is what lets a caller that did not fork magmux
find it. `NAME` is restricted to `[A-Za-z0-9_-]{1,64}` **and may not be all
digits**; an invalid one is ignored with a line on stderr and the pid socket
binds as usual. The pid default is unchanged, and `--id` **replaces** it rather
than adding to it.

The all-digits restriction is deliberate and worth the sentence: the startup
sweep below identifies a socket as reapable *by its name being a pid*, so a live
`--id 1234` would be indistinguishable from the socket of a dead process 1234 —
and almost always removable.

**Directory.** `--sock-dir DIR`, or `MAGMUX_SOCK_DIR`, moves the socket out of
`/tmp`; the flag wins, and when it is given the variable is not consulted. Any
depth works, including a socket path longer than the OS limit on socket
addresses (104 bytes on macOS, 108 on Linux). A directory that is missing, is
not a directory or is not writable is fatal: magmux exits 1 and names the
source, the directory and the reason on stderr, and never falls back to
`/tmp`. So does a bind that fails (`magmux: socket PATH: ERROR`), before any
pane starts. `--sock-dir` also exports
`MAGMUX_SOCK_DIR` to children, so a `magmux mcp` started inside a pane finds the
right directory — but it cannot reach an MCP server the *client* launched in its
own process tree. For that, set `MAGMUX_SOCK_DIR` in the client's own `env`
block; it is the only channel that spans both process trees.

**Startup sweep.** magmux removes stale sockets in that directory as it starts:
the ordinary teardown unlinks the socket, but `SIGKILL`, a panic, an OOM kill
and a power loss all skip it, so without a sweep those files accumulate forever.
It only removes a socket named `magmux-<pid>.sock` whose `<pid>` it can prove is
gone (`kill(pid, 0)` reporting `ESRCH`, checked twice, and only on a real socket
inode). A running session, a socket owned by another user, and any `--id` socket
are all left alone — an `--id` name has no liveness oracle, so it is never
touched. `SIGTERM`, `SIGINT` and `SIGHUP` now run the normal teardown, so a
session ended that way removes its own socket and still delivers `results`; a
second signal exits immediately.

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
| `snapshot` (per-pane) | `pane` (int), `controller`, `state`, `project`, `model`, `prompt`, `response`, `tool`, `startedAt`, `completedAt`, and `label` when the pane has one | on meaningful per-pane change |
| `exit` | `pane` (int), `exitCode` (int), `duration`, `lastLine`, `response`, `prompt`, `tool`, `model` | when a pane's process exits |
| `results` | `panes`: array of pane-state objects, `endedAt` (RFC3339) | once, at shutdown, before EOF |
| `shutdown` | (no extra fields) | once, after `results`, right before close |
| `control` | `dir` (`out`\|`note`), `pane`, `label`, `text`, `keys`, `at`, or `event`/`goal`/`summary` for notes | when a controlled session is driven (see below) |
| `pane_opened` | `pane` (int), `cmd`, and `cwd` / `label` when they were given | when `open_pane` adds a pane |
| `pane_closed` | `pane` (int) | when `close_pane` removes one |
| `reply` | `id`, `ok`, plus `result`, or `code`+`error` and sometimes `reason`/`typed` | answer to *your* message, if it carried an `id` (see below) |

`reply` is the one event that is **not** broadcast: it goes only to the
connection that asked for it. `pane_opened`/`pane_closed` are broadcasts like
the rest.

**Guaranteed delivery.** `results` is replayed to a connection that arrives
during teardown, so it always lands before EOF. `exit`, `snapshot`, `control`,
`pane_opened` and `pane_closed` are live broadcasts with no such guarantee — a
subscriber that connects late, or one still being served when magmux tears
down, can miss them. Anything you must not lose, read off `results`.

Disambiguate the two `snapshot` variants by field: the connect-time aggregate
carries a **`panes`** array; the per-pane live event carries a singular **`pane`** index.

**Pane-state object** (elements of the `panes` array in `snapshot`/`results`):

| Field | Type | Notes |
|---|---|---|
| `pane` | int | pane index, stable for the process lifetime; correlates to `-e` argument order |
| `state` | string | `completed` \| `failed` \| `awaiting_input` \| `running` \| `closed` \| `panel` |
| `exitCode` | int | process exit code (0 until exit) |
| `dead` | bool | whether the pane's process has exited |
| `closed` | bool | present and true on a tombstone (see below) |
| `control` | bool | present and true on the control panel pane, which has no process |
| `hidden` | bool | control panel only: true when it is not currently on screen (`Ctrl-G p`). Its `state` is `panel` either way — hidden is a fact about magmux's chrome, not about a session |
| `controller` | string | controller name, if any (e.g. `claude-code`) |
| `model` | string | model name, if detected (omitted when empty) |
| `project` | string | project name, if detected (omitted when empty) |
| `prompt` | string | last user prompt, if any (omitted when empty) |
| `response` | string | last response, if any (omitted when empty) |
| `tool` | string | last tool used, if any (omitted when empty) |
| `startedAt` | string | RFC3339, if known (omitted when zero) |
| `completedAt` | string | RFC3339, if known (omitted when zero) |
| `focused` | bool | whether this pane currently has keyboard focus |
| `rows` `cols` | int | the pane's screen geometry — without the width there is no telling a wrapped line from a hard one |
| `altMode` | bool | whether the pane's app is on the alternate screen (vim, htop, a TUI agent) |
| `label` | string | the name given at `open_pane` or by `-e CMD --label NAME` (omitted when empty) |
| `cmd` | string | the pane's command line, if known (omitted otherwise) |
| `cwd` | string | the pane's working directory, if known (omitted otherwise) |
| `pid` | int | the pane process's pid, if running (omitted otherwise) |
| `inputSignal` | string | which heuristic called the pane idle — `osc`, `2004`, `title`, `idle`, `ctrl`, `perm` (omitted when none) |
| `gateBasis` | string | why the pane's input is gated: `direct` (a `claude …` launch) or `declared` (`--controller`, `open_pane controller:"claude-code"`). Omitted when the pane is not gated. See [The input gate](#claude-code-panes-the-input-gate) |
| `editorOverride` | string | what magmux did about Claude's editor mode: `added` \| `merged` \| `merged_file` \| `unparsed` \| `delegated` (omitted when nothing was attempted) |
| `input` | object | gated panes only: `{"ready":false,"reason":"starting","detail":"…"}`, the gate's published view, refreshed four times a second. A send decides on its own fresh reads, so this is advisory |
| `sessionId` | string | Claude panes: the Claude Code session the pane is bound to — the declared one, or the found transcript's (omitted until there is one) |
| `transcriptPath` | string | Claude panes: the transcript the controller tails — the declared path, or the one found (omitted until known) |
| `bound` | string | how that transcript was chosen: `named` (declared with `--session-id`/`--transcript` or `open_pane` `session_id`/`transcript`; the only file ever tailed), `prompt` (its first prompt matched the launch's) or `mtime` (the newest transcript in the cwd's project directory — a GUESS, which can be another session's). See [Declaring the session](#declaring-the-session) |
| `transcriptSeen` | bool | whether that transcript exists yet. A declared pane reports `false` until its child writes it |

The last sixteen are additive; the forward-compat clause above already covers
them, and every one is omitted rather than reported as a zero when it cannot be
sourced. The one exception is `transcriptSeen`, which is present, `false`
included, whenever the pane has a binding to report.

**Pane ids follow argument order.** Panes created from `-e` receive ids
`0..N-1` in the order the flags were given, and magmux's own panes — the control
panel — are appended **after** them, whether or not `-c` was passed. So `-e A -e
B` gives you pane 0 and pane 1, and the first pane an agent opens is *not*
necessarily 1.

**Pane ids are sparse.** `close_pane` tombstones the slot and nothing ever
renumbers, so ids can have gaps and `results` can contain entries with
`"state":"closed"`. A client treating the `panes` array as a dense positional
list will address the wrong session after the first close: the `pane` field was
always the authoritative index, and now it matters. Tombstones are reported
rather than omitted precisely so the hole is visible.

**Or don't count at all.** `-e CMD --label NAME` names a pane at startup, and
the name comes back on `results`, on the `list` verb and on every live
`snapshot` event — so a client can resolve `"tests"` once and never reason about
indices again. `--label` binds to the `-e` immediately before it; with no
preceding `-e` it is dropped with a line on stderr, and `-g` discards labels
along with the `-e` commands it overrides.

**`exitCode` is only meaningful once the status has been collected.** A pane
whose PTY has closed but whose `cmd.Wait` has not yet returned reports
`"state":"running"`, not `"completed"` — magmux will not claim a success it
cannot back. The final `results` under `-w` always waits for the real status, so
`exitCode` there is the child's own.

### Request/response

A message carrying an `"id"` gets exactly one `reply` event, **unicast to the
connection that sent it**. A message with no `id` is dispatched exactly as it
always was, and answered exactly as it always was — which is to say not at all.
Existing clients never send an `id`, so they never receive a `reply`; nothing
about their stream changes, including for verbs they have never heard of, which
stay silently ignored.

```jsonc
--> {"type":"capture","id":7,"pane":0,"lines":40}
<-- {"type":"reply","id":7,"ok":true,"result":{"pane":0,"rows":24,"cols":100,
     "alt":false,"truncated":false,"cursor":{"y":3,"x":0},"text":"..."}}

--> {"type":"send","id":8,"pane":9,"text":"hi"}
<-- {"type":"reply","id":8,"ok":false,"code":"no_such_pane",
     "error":"no pane 9 (it may have been closed)"}
```

The `id` round-trips **verbatim**: send `7` and you get `7`, send `"7"` and you
get `"7"`. `ok:true` carries `result`; `ok:false` carries `code` and `error`.
Branch on `code` — the `error` string beside it is for a human and may be
reworded at any time. A failure can also carry `reason`, a machine-readable
sub-code that says which failure of that kind it was, `"typed":true`,
which says bytes of this request reached the pane before it failed, and
`"maybeSubmitted":true`, which says the text may already be in the
conversation through an Enter that was not magmux's — never answer that one by
sending again. All three are omitted when they do not apply, so a failure without them is the line it
always was.

| `code` | Meaning |
|---|---|
| `bad_request` | the message is malformed, or a field is not what the verb needs. On a [gated](#claude-code-panes-the-input-gate) pane also: Enter not in the last position, two Enters, an unknown key name, or a negative `readyTimeoutMs`. Nothing is written |
| `no_such_pane` | no pane with that index (it may have been closed) |
| `pane_is_control` | that pane is the control panel, which has no process |
| `pane_dead` | the pane's process is gone, so the bytes had nowhere to go (`typed` if it died partway through a send) |
| `unknown_verb` | this magmux does not know that `type` |
| `unsupported` | known verb, not available in this mode |
| `busy` | the pane cannot take this now. `reason` says why: `queue_full` (this connection's queue for the pane is full), `in_flight` (a cap on concurrent requests), or, on a gated pane, `starting`, `terminal_not_ready`, `no_input_box`, `box_not_empty`, `vim_mode`, `dialog:<kind>` or `typing_in_progress`. Without `typed` nothing was written; with `typed` the text was typed and Enter was withheld |
| `not_accepted` | gated pane only: the input box did not take what was typed. `reason` is `text_mismatch` or `empty_box` (Enter was **not** pressed), or `still_in_box` or `unconfirmed` (it was) |
| `timeout` | the verb's budget expired. For `send`, only the caller's wait expired: the delivery carries on |
| `too_small` | `open_pane`: no room to split without going below the minimum pane size |
| `internal` | magmux's own fault |

`reply` is never replayed to a late connection, so it cannot land after
`results`; the `results` → `shutdown` → EOF ordering is unaffected.

For `send` to an ordinary pane, `ok:true` means the bytes reached the PTY, not
that the program accepted them: an app that was not ready for a paste can
still drop it. On a gated pane, `ok:true` after a submit means more. The box
was verified to hold exactly what was typed before Enter was pressed, and a new
user-prompt entry or transcript entry carrying it has appeared since. The
reply's `evidence` says which of the two it was.


### Read verbs

```jsonc
// what this magmux can do; also the version probe — an older magmux
// answers nothing at all, because an unknown verb without an id is silent
{"type":"capabilities","id":1}
// -> {"protocol":1,"version":"0.4.0","commit":"abc1234","pid":4212,
//     "sock":"/tmp/magmux-4212.sock","rows":48,"cols":180,"gridMode":true,
//     "scrollback":1000,"verbs":[...],"events":[...]}

// every pane, built by the same call snapshot and results are built from,
// so list and results can never disagree about the same pane
{"type":"list","id":2}
// -> {"panes":[ <pane-state objects, exactly as above> ]}

// a pane's screen as text; `lines` keeps the LAST N rows
{"type":"capture","id":3,"pane":0,"lines":40}
// -> {"pane":0,"rows":24,"cols":100,"alt":false,"truncated":false,
//     "cursor":{"y":3,"x":0},"text":"line\nline\n…",
//     "offset":0,"scrollback":312,"atTop":false}

// the same pane, one screenful further back in its history
{"type":"capture","id":4,"pane":0,"offset":24}
// -> {…,"offset":24,"scrollback":312,"atTop":false}
```

**`capture` reads the visible screen at `offset: 0` and the pane's scrollback
above it.** `offset` counts ROWS back from the bottom of the live screen, so
`offset: rows` is the screenful directly above what is on display and
successive reads at `rows`, `2*rows`, `3*rows` walk history backwards without
overlap. The reply always carries three fields to navigate by: `scrollback` is
how many rows of history that pane holds, `offset` is the one magmux **settled
on** (an over-large request clamps rather than failing), and `atTop` says the
oldest row it still has is in view. `truncated` is unrelated — it says only
that your `lines` cut something off.

Two bounds matter:

- **History is bounded and drops the oldest first.** `capabilities` reports the
  ceiling — 1000 rows per pane by default, `MAGMUX_SCROLLBACK` to change it,
  `0` to turn it off. The ring fills lazily, so a pane that never scrolls costs
  nothing.
- **Only the primary screen records.** A full-screen app runs on the terminal's
  *alternate* screen, and no terminal records that into history — which is why
  quitting `vim` does not leave its buffer in your scrollback. So a pane running
  Claude Code, `vim`, `htop` or `less` reports `"scrollback": 0` and every
  offset returns the live screen. For those, the session's own transcript
  (`{"type":"transcript"}`) is the history. Scrollback is for shell panes:
  builds, test runs, dev servers.

The control panel pane is capturable like any other.

### Pane lifecycle

```jsonc
// split the layout and run a command in the new half
{"type":"open_pane","id":4,"cmd":"claude","cwd":"/proj","label":"api",
 "target":0,"split":"vertical","ratio":0.5,"focus":false,
 "env":["FOO=bar"]}
// -> {"pane":3,"cmd":"claude","cwd":"/proj","label":"api"}

// close a pane and reap its process
{"type":"close_pane","id":5,"pane":3,"force":false}
// -> {"pane":3,"closed":true}

// move keyboard focus
{"type":"focus","id":6,"pane":0}
// -> {"pane":0,"focused":true}
```

`cmd` runs through the user's login shell exactly as `-e` does, so the same
quoting, pipelines and `cd x && y` forms work. `dir` is accepted as a synonym
for `cwd`. `target` is the pane to split and defaults to the focused one; an
absent field takes that default, but a *present* one that is not a pane index
is refused rather than rounded — the wrong pane gets split and the layout is
silently wrong. `split` is `auto` (the default, which splits the longer axis),
`horizontal` or `vertical`. A split that would leave a pane below 3 rows or 20
columns is refused with `too_small` before anything is forked.

`close_pane` retains the index as a tombstone forever, and `force` escalates to
`SIGKILL` on the process group after 2s. `focus` exists because `open_pane`'s
`focus` flag is one-shot.

Inbound messages (subscribers writing to magmux) also drive status-bar text,
pane tints, and agent hook events; see `sockMsg` in `mux/socket.go`. These are
optional and unrelated to the read-only subscribe flow above.

`{"type":"tint","color":"green"}` colours the pane's **border** (green, red or
yellow; `""` clears it). It deliberately does not touch the pane's interior:
magmux does not know what foreground colours the program in the pane is using
and cannot recolour them, so any background it painted underneath them would be
illegible for somebody — a dark wash hides a dark-on-light program, a light one
hides a light-on-dark program like Claude Code. A pane with no split beside it
has no border to colour; use `{"type":"overlay"}` there, which sets both halves
of its own colour and is legible anywhere.

### Screen size

`resize` sets the size of magmux's whole screen, so every child gets a real
`SIGWINCH` and redraws — the way to make a headless session the size of the
browser or terminal that is showing it.

```jsonc
// the one session pane, sized directly
{"type":"resize","pane":0,"rows":40,"cols":100,"id":"r1"}
// the whole screen, status row included
{"type":"resize","rows":50,"cols":160,"id":"r2"}
// give the size back to the terminal
{"type":"resize","auto":true,"id":"r3"}

// -> {"type":"reply","id":"r1","ok":true,
//     "result":{"rows":40,"cols":100,"changed":true,"manual":true}}
// -> {"type":"reply","id":"r1","ok":false,"code":"too_small","error":"…"}
```

Without `pane`, `rows`/`cols` are the whole screen. With it they are that
pane's size, and the screen becomes `rows` plus the status row (none under
`--no-status`) by `cols`. The pane form is only accepted when that pane is the
**one** session on screen — with two, a pane's size does not decide the
screen's, and the answer would claim a size the pane did not get — so it is
refused `bad_request` with more than one session pane, or with the control
panel visible. The result's `rows`/`cols` are the pane's size after the call
when `pane` was given, else the screen's; `manual` says whether a client's size
is now in force.

A requested size stays until `{"auto":true}` or `Ctrl-G r`. The human's window
changing size never releases it; on an interactive terminal a window smaller
than the requested size **clips** it while it cannot be shown, and the window
growing back restores it. Same size again answers `changed:false` and touches
nothing — no reflow, no `SIGWINCH`, no keyframe — so a client may repeat itself
freely. A watcher sees an applied resize as a keyframe at the new geometry.

| Code | When |
|---|---|
| `bad_request` | `rows`/`cols` missing, not whole numbers, or below 1; `auto` together with a size; `pane` as described above |
| `too_small` | any session pane would get less than **3 rows × 20 columns** |
| `too_large` | a side over 10000, more than 4,000,000 cells, or — interactive only — larger than the terminal |
| `no_such_pane`, `pane_is_control` | `pane` names nothing, or names the control panel |

A refused resize leaves every pane exactly as it was: the size is checked by a
dry run before any pane is touched. It is class `control`, so a view token
cannot call it.

**Shrinking is lossy; debounce.** Growing keeps scrollback at the width it was
printed at, but a shrink discards the live cells that no longer fit, and
nothing brings them back. A client following a browser window should send the
size the drag ended at, not every size it passed through.

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

### Claude Code panes: the input gate

Typing into a TUI is a race against the TUI. Claude Code draws its input box
about a second after it starts, then resets its terminal once or twice while it
loads, sometimes after the box is already on screen. Text that arrives inside
that window vanishes, or sits in the box with nobody to press Enter. Under vim
editor mode it is worse: the box starts in INSERT, and the Enter that would have
rescued it does nothing. A plain `send` cannot tell any of this from success,
because the bytes did reach the PTY.

So on a Claude Code pane magmux owns input readiness. Such a pane is **gated**,
and `list`/`results` say so with `gateBasis`:

- **`direct`**: the pane's command is a `claude` invocation magmux can see,
  such as `-e 'claude --model opus'` or `-e 'cd /x && claude'`. A loose match
  (`grep claude x`, `tail -f /tmp/claude`) is observed but never gated. Nor is
  an invocation that never shows a box: print mode (`claude -p`), a subcommand
  as the FIRST argument (`claude mcp list`, `claude update`, `claude doctor`, …)
  and `--version` / `--help`. Those are not rewritten either. A subcommand word
  after an option is not recognised: `claude --name doctor` is an interactive
  session named doctor, and Claude's options take zero, one, several or an
  optional value, so `claude --settings x mcp list` is also treated as direct,
  gated and rewritten. A send to such a pane answers `busy` (no box).
- **`declared`**: the pane was declared a Claude Code pane, whatever its command
  is. `--controller claude-code` declares every `-e` and `-g` pane, and
  `open_pane` with `"controller":"claude-code"` declares one. This is how a
  wrapper that runs `claude` itself (claudish) gets the gate. A grid that mixes
  shells with Claude panes should declare per pane with `open_pane`, because the
  flag gates the shells too, and a shell never shows a Claude box, so every
  send to it would wait and then be refused.

#### Declaring the session

A declared WRAPPER shows magmux no prompt, so magmux has to guess which
transcript under `~/.claude/projects/<cwd>/` is the pane's: the newest one,
preferring files that appeared after the pane started. Claude Code writes no
transcript until its first prompt is accepted, so at the first send every
candidate is somebody else's — a sibling started a moment later, or the parent
session in the same directory — and the guess sticks. The gate then reads that
session: a false `busy dialog:permission` from its registry, a refused retype
from its conversation.

A caller that knows the session says so, and magmux stops guessing:

```bash
magmux --controller claude-code --session-id "$ID" \
       --transcript "$CLAUDE_CONFIG_DIR/projects/<dir>/$ID.jsonl" -e '. launch.sh'
```

```jsonc
{"type":"open_pane","cmd":". launch.sh","controller":"claude-code","session_id":"…","transcript":"/abs/….jsonl"}
```

Either field alone is enough, and given both the file must be `<session_id>.jsonl`.
The flags need `--controller claude-code` and exactly one `-e` (no `-g`); the
`open_pane` fields need `"controller":"claude-code"`. The pane then tails exactly
that transcript, waits for it to appear, and never falls back to discovery. With
only an id, the file is looked for under `$CLAUDE_CONFIG_DIR/projects` (the
pane's env, else magmux's, else `~/.claude`), in the cwd's directory first and
then in any. Claude's session registry is read by that id, under the
transcript's config directory. `list` echoes the binding (`sessionId`,
`transcriptPath`, `bound:"named"`, `transcriptSeen`), so check it before
trusting the gate.

**A send outlives its connection.** If the socket drops mid-send, the delivery
runs to completion (only shutdown cancels it), but its reply went to the
connection that is gone, and no verb reports a past send's outcome. A new
connection learns it from the session: `transcript`, or `list`'s `prompt`, which
on a declared pane are the right session's.

A pane a plugin owns is never gated. Everything below applies to gated panes
only. A send to any other pane is byte for byte what it always was, and so is
its reply.

**What a send does depends on its class.** A final `enter` or `return` key
counts as `"enter":true`.

| Class | Shape | Gated |
|---|---|---|
| KEYS | no `text`, no `paste`, no Enter: any keys, printable ones included (`{"keys":["escape"],"enter":false}`, `{"keys":["2"],"enter":false}`) | no |
| TYPE | `text` (or `paste`) and no Enter | yes: typed when ready and verified, never submitted (`accept:"in_box"`) |
| SUBMIT | anything with an Enter, including the default `{"text":"…"}` | yes: typed when ready, verified, submitted, confirmed |

**Enter goes last, and goes once.** On a gated pane `enter`/`return` may only be
the final key, a final Enter key plus `"enter":true` is two Enters, and an
unknown key name is refused rather than skipped. All three are `bad_request`
before anything is written, because a send that half-ran could not be
verified. Split the request instead.

**A CR in `text` is a newline.** On a gated pane `\r` and `\r\n` inside `text`
become `\n`, in every mode, so a multi-line text is typed as lines and the only
Return is the final, checked Enter. Any other control character but tab
(ctrl-c, Escape, DEL, …) is `bad_request`: send it as a key, or use `noGate`.
Ungated, `text` is written exactly as sent.

**Bash mode works.** A text that starts with `!` switches Claude's box into
bash mode: Claude consumes the `!` and shows it as the box's glyph. The box
check treats the leading `!` as that switch and compares the rest, and the
`! command` history entry is the evidence. A text WITHOUT a leading `!` never
verifies against a box that is already in bash mode, because submitting it
would run a shell command rather than send a prompt.

**The default `enter` no longer reaches a dialog.** `enter` defaults to true,
so `{"keys":["2"]}` is a SUBMIT, and while a dialog shows it is refused
`busy`/`dialog:<kind>`. Enter on a permission dialog picks option 1, "Yes",
and magmux will not press it on your behalf. To answer a numbered menu on
purpose, send `{"keys":["2"],"enter":false}` (a KEYS send, never gated), or
add `noGate`.

**A gated send, step by step:**

1. **Wait until the box can take input.** The box is on screen; the terminal is
   in raw mode, and both have been still for a second; Claude's boot reset has
   happened, or (the first time) 3.5 s have passed since the terminal first
   went raw; no dialog and no `-- INSERT --` is showing; and, for a send that
   types, the box is empty. A paste also waits for Claude to turn on bracketed paste. Each of
   these is read fresh at the moment of the decision. Claude being busy with a
   turn is **not** a reason to wait: typing while it works is allowed, and
   Claude queues the prompt.
2. **Type**, with the same check before every write after the first. A dialog
   that appears partway stops the typing.
3. **Verify the box**, starting 150 ms after the last byte. Claude can take
   several hundred milliseconds to draw what was typed, so this is a wait, not
   one look: magmux re-reads the box every 40 ms until it holds what was
   typed, or until its content has stopped changing for 1 s, for at most 3 s.
   The `readyTimeoutMs` deadline caps that wait but never cuts it below 1.2 s,
   so a send that became ready at the last moment, or a small
   `readyTimeoutMs`, still waits for the draw. It must hold exactly what was typed
   (`verified:"exact"`), or show ONE new `[Pasted text #N …]` token for a paste
   (`paste_token`; two new tokens are more than was pasted, a mismatch). When the content cannot be known from the request, because
   the box was not empty or an editing key ran, it must at least not be empty
   (`non_empty`). Otherwise Enter is not pressed.
4. **Press Enter**, then wait up to three 2-second windows for **evidence**: a
   new `❯` entry in Claude's history carrying the verified text
   (`evidence:"history"`), or an equal user entry in its transcript
   (`transcript`). An emptied box is never evidence, because a lost prompt
   looks exactly like a submitted one. If the text is still in the box, magmux
   verifies it again and presses Enter once more, and the reply then says
   `accept:"resent_enter"`. If the box never showed any of the text, nothing
   was submitted and nothing partial is left, and magmux types it ONCE more
   (back to step 1, within the same deadline) on either of two signals of
   loss: the box stayed COMPLETELY empty for that whole second AND the
   terminal went through a reset after the text was typed (a late startup
   terminal reset swallows the text whole: `retypeCause:"terminal_reset"`);
   or the box was COMPLETELY empty on every read for the whole 3 s window,
   with no reset (Claude dropped the text silently and never drew a rune of
   it; render lag tops out well under a second, so a 3 s blank is not a slow
   draw: `retypeCause:"never_drawn"`) and the conversation above the box did
   not change and no turn started meanwhile (if either did, another writer's
   Enter may have submitted the text before it was drawn, so it is
   `text_mismatch` and not retyped). The ok reply then carries
   `retyped:true` and `retypeCause`. A blank box with no reset whose 3 s
   window the deadline cut short cannot be told from a slow draw, so it is
   `text_mismatch`, never a retype. Before retyping, magmux checks whether
   the text was in fact submitted by another Enter (a person at the
   keyboard, say); if it was, the reply is ok with
   `accept:"submitted_by_other"`, that evidence, and `enter:false`, because
   magmux pressed no Enter (a TYPE send too: its text is no longer in the
   box), and nothing is retyped. A box that held part of the text, all of
   it, or more than was typed is never retyped, and nor is a paste (paste
   mode, a bracketed multi-line auto send, or an auto send over 800 runes):
   a late-drawn paste and its retype would be two tokens, and Enter would
   submit both. After the one retype the same verify applies, and a second
   loss is `text_mismatch`.

`readyTimeoutMs` (default 10000, at most 60000) is ONE deadline. Waiting for
the box, waiting for another delivery to finish typing into the same pane, the
pre-Enter re-wait and the verify's wait for the text to be drawn all share it,
so it is the most a send ever waits for readiness.

```jsonc
--> {"type":"send","id":9,"pane":2,"text":"run the tests"}
<-- {"type":"reply","id":9,"ok":true,"result":{"pane":2,"bytes":13,"keys":0,"enter":true,
     "waitedMs":2400,"verified":"exact","accept":"confirmed","evidence":"history"}}
```

`waitedMs` is the time spent waiting for readiness, including any return to
the gate before the first key or a retype. `dismissed` lists the
dialogs that were Escaped, in order, and is omitted when there were none.
`retyped:true` and `retypeCause` (`terminal_reset` or `never_drawn`) appear
only when the one automatic retype ran. `accept` is `confirmed`,
`resent_enter`, `in_box` (a TYPE send, left in the box), or
`submitted_by_other`: the text left the box through somebody else's Enter and
evidence for it appeared, so magmux pressed none, and `enter` is then false.
On a gated reply `enter` says whether MAGMUX pressed Enter.

**`dismissDialogs:true`** presses Escape on a permission, question or plan
dialog, before typing and again before the Enter. Escape declines, and it is
the only key this ever presses, so it can never approve anything. magmux then
waits for the box to come back and delivers as usual. It presses at most three
times per send, at least 400 ms apart, and only while it holds the pane's
typing token, so an Escape never lands in the middle of another send's typing.
A permission or question dialog is pressed only when it shows its own
`Esc to cancel` hint; the plan dialog shows none and is pressed anyway, because
Escape was measured to decline it and leave Claude in plan mode. A boot dialog
(trust, bypass, API key, onboarding) is never dismissed: those are questions
for the human, and Escape on the trust prompt exits Claude. Neither is a
`select` dialog, the catch-all for a region with the generic
`Enter to confirm · Esc to cancel` footer: the boot dialogs carry that very
footer, so a reworded trust prompt would land there.

**`noGate:true`** turns all of it off for one send: no Enter rule, no wait, no
verify, no acceptance. It is the ungated send exactly, even into a dialog.

**What a refusal tells you.** magmux reports what it observed. It does not
prescribe a retry.

| Reply | Enter pressed? | What it means |
|---|---|---|
| `busy` `starting` | no | Claude is booting: no box yet, the boot reset not over, or the box only just appeared |
| `busy` `terminal_not_ready` | no | the pane was ready before, and now the terminal is not raw, changed less than a second ago, or has bracketed paste off for a paste |
| `busy` `no_input_box` | no | the pane was ready before, and now shows neither a box nor a dialog magmux recognises |
| `busy` `box_not_empty` | no | the box already holds text, and this send would type into it |
| `busy` `vim_mode` | no | `-- INSERT --` is showing: vim editing mode is on, so typed text would be read as commands |
| `busy` `dialog:<kind>` | no | a blocking dialog: `permission`, `question`, `plan`, `select`, `trust`, `bypass`, `api_key` or `onboarding`. Only Escapes were written before typing |
| `busy` `typing_in_progress` | no | another delivery was still typing into the pane at the deadline |
| `busy` with `typed:true` | no | the text was typed and the Enter withheld, by a dialog or by a terminal that did not come back in time. The box may hold the text, or part of it |
| `not_accepted` `text_mismatch` | no (not by magmux) | the box held something other than what was typed: partial text, more than was typed (two paste tokens included), a box still changing after 3 s, or nothing — after the one automatic retype, for a paste (never retyped), or with neither loss signal (no terminal reset, and the 3 s window cut short by the deadline or interrupted), when the text may still appear. The detail says which. Nothing was submitted, EXCEPT when the detail says the box showed the text and then emptied without magmux's Enter: then it is unknown whether another Enter submitted it, so check the conversation before sending it again (the reply carries `"maybeSubmitted":true`). A retype is refused `box_not_empty` until the box is cleared |
| `not_accepted` `empty_box` | no | a submit found the box empty, so there was nothing to submit |
| `not_accepted` `still_in_box` | yes, once or twice | the text is still in the box, unsubmitted. **Do not retype it**: it is there |
| `not_accepted` `unconfirmed` | yes | no user-prompt entry and no transcript entry that can be attributed to THIS send appeared within the windows. An identical older prompt does not count. A history entry must be a NEW user-prompt entry after the ones the verify saw, and magmux must be able to tell where those end: when the visible prompts are all the same text (a resent `continue`) and the top of the conversation is not on screen, or a command wipes the screen (`/clear`), the history cannot say, and only the transcript can. A transcript record must have been SUBMITTED after the verify: a queued prompt counts from its enqueue, and an older identical queued prompt that runs later is still the older one. The prompt may have been lost, or taken without a visible sign. You decide |

**How long to wait for the reply.** Every part of a gated send is paced by a
constant, so its upper bound is a formula: `readyTimeoutMs`, plus the typing,
plus 22 ms per key, plus the 150 ms settle and the verify's 1.2 s minimum
window (each counted twice, for the one retype), plus three 2 s acceptance
windows, plus 2 s of slack (`protocol.SendBudget`). A short prompt comes to
about 21 s, and `readyTimeoutMs:60000` to about 71 s. A caller that names no timeout is
never cut off before that:

| Transport | Default wait for a gated send |
|---|---|
| the `send` verb on the socket | none: the reply comes when the delivery ends |
| `call` on the socket, no `timeoutMs` | the larger of 30 s and the bound, at most 15 min |
| REST, `POST /v1/ops/send` | the larger of 30 s and the bound |
| WebSocket, no `timeoutMs` | the larger of 30 s and the bound, at most 15 min |
| a Firebase command | none: its result is written when the delivery ends |
| Go, `client.Session.SendWith` | the larger of `client.SendTimeout` (20 s) and the bound |

An explicit `timeoutMs` is honoured as given. If it expires, the CALLER gets
`timeout` and the delivery carries on: the text and the Enter still land. On
the socket, the next send to that pane on the same connection waits for it. Only shutdown stops a
delivery halfway.

**Claude panes run in normal editor mode.** When magmux launches `claude`
directly, it adds `--settings='{"editorMode":"normal"}'`, which overrides the
user's and the project's settings. If the command already carries
`--settings`, magmux merges `editorMode` into it rather than appending a second
flag, because Claude reads only the last one, and every other key the caller
passed survives. A settings file is merged into a private copy under
`{sock-dir}/magmux-settings-<euid>/` (mode 0600), and the copy is removed when
the child is reaped. `list` and `results` report the rewritten `cmd`, and the
outcome as `editorOverride`:

| `editorOverride` | Meaning |
|---|---|
| `added` | there was no `--settings`, so magmux added one |
| `merged` | merged into the caller's inline `--settings` JSON |
| `merged_file` | the caller's settings file, merged into a private copy |
| `unparsed` | a rewrite was not provably safe (a `$VAR` in the value, a relative path after a `cd`, a redirection between the flag and its value, an unreadable file, …), so the command runs exactly as written |
| `delegated` | a declared wrapper: magmux cannot see the `claude` it runs, so the wrapper must force normal mode itself |

The gate does not take the override on trust. If `-- INSERT --` shows on a
gated pane for any reason (`unparsed`, a wrapper that did not override, `/vim`
typed at runtime), every send that would type is refused `busy`/`vim_mode`,
and nothing is typed into a vim buffer.

## The Control Panel

Every session has a **control panel** — a magmux pane like any other, except it
has no process. magmux paints it itself, by writing ANSI through the pane's own
VT parser exactly as a child would, so it inherits borders, selection and
dirty-flag rendering and costs nothing while idle. It is built into the binary;
there is nothing to install and nothing to run.

It starts **hidden**, so a session pane has the whole terminal. `Ctrl-G p`
shows it and `Ctrl-G p` hides it again; `-c` starts it already visible. Hiding
is not closing — the panel keeps every row of its ledger while it is away, and
`results` still reports it as `state: "panel"` either way (plus a `hidden`
flag). While it is hidden, the status bar carries a one-line digest of it:

```
* magmux │ 2/3 done │ 1 running │ ▶4 ◀3 │ p0 working · Bash 14s │ ▶ send "run the tests…" │ ^g p
```

`▶` is what the controller asked for and `◀` is what magmux observed — the same
two numbers the panel shows, with the same provenance rule. The newest signal
is dropped as the terminal narrows; the counters are the last to go.

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

### More than one pane

That is the one-pane layout. When a controller drives several panes — which is
what `open_pane` and the MCP server make ordinary — the panel switches to a
**routed** layout: one row per pane, and a single interleaved stream underneath.

```
 CONTROLLER ══▶ 4 PANES                              claude-code/2.1  4m 2s
 ─────────────────────────────────────────────────────────────────────────
 12 sent │ 11 done │ 1 in flight                             ████████████░
 ─────────────────────────────────────────────────────────────────────────
 ▸0  api       ◐ WORKING       3/2                ‹3m 2s ▃█        Bash
  1  web       ✓ AWAITING      3/3                1m 36s ▄▁█       Read
  2  infra     ✓ AWAITING      4/4                 1m 1s ▁█▁▅      Grep
  3  scratch   ✗ GONE          2/2                  3.9s █▁        Edit
 ─────────────────────────────────────────────────────  ▲ 24 earlier
 01:03  3 ▶ send_and_wait  scratch: make the tests green, then report back
          ⇦ ok             34 bytes + enter
 01:03  3 ◀ AWAITING       42 passed, 0 failed                       18.8s
 01:04  1 ▶ capture        read the screen
          ⇦ pane_dead      pane 1 rejected the read
 01:04 ▸0 ▶ send_and_wait  now run go vet ./... and list every warning
```

Each route is a pane the controller has touched, keyed on the pane index, and
routes are never deleted — a pane the agent closed stays as `✗ GONE`, because
the history of something closed *because it had failed* is exactly what you want
left to read. `▸` marks the focused pane. Counters are per route, so one pane
going idle can never close another pane's outstanding step.

The stream is a single chronological lane rather than per-pane columns:
causality across panes *is* the interleaving. The indented `⇦` line is magmux's
answer to the request directly above it — it never closes a turn, and only a
`◀` row does. Narrow or short panes degrade rather than wrap: the route table
collapses to a one-line strip, and the stream drops its time column.

A single route keeps the one-pane layout above, so a `-c` pilot run looks
exactly as it always did.

### Using it

| | |
|---|---|
| wheel over the panel | scroll, whichever pane has focus |
| `↑` `↓` `PgUp` `PgDn` | scroll (panel focused — `Ctrl-G Tab` to focus it) |
| `Home` / `g` | jump to the start of the run |
| `End` / `G` | resume following the newest exchange |
| `1`–`9` | show only that pane's traffic |
| `0` | show every route again |
| `[` `]` | cycle through the open routes, via an "all" position |
| `q` | close, once the run has finished |

The panel is read-only. It cannot focus, send or close anything — the moment it
could, "who closed pane 2" would become a question it has to answer about
itself.

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
// announce what you are driving, so the panel can show it. `client` is your
// own identity for the panel header ("claude-code/2.1"), and is optional
{"type":"pilot","event":"start","pane":0,"goal":"...","steps":3,"model":"...",
 "client":"claude-code/2.1"}

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
the text before it is submitted. On a Claude Code pane `send` does more: it
waits for the box, verifies it and confirms the submit, as [the input
gate](#claude-code-panes-the-input-gate) describes, and the panel's `⇦` ack
says what it found (`waited 2.4s · 13 bytes + enter · verified exact ·
accepted (history)`). `label` is the short tag the panel shows in its verb
column; an MCP client puts its tool name there, which is that layer's entire
footprint on this protocol.

**The panel's two directions come from different sources on purpose.** `▶` rows
are the controller's request as it arrived on the socket; `◀` rows are recorded
only from what magmux itself observed the pane do. A driver can never fabricate
a completion, and the panel can show a session disagreeing with what the driver
believes it asked for.

The panel is driver-agnostic: anything that speaks these verbs on the socket
fills it in. `pilot/pilot.ts` below is one such driver, not a requirement — a
shell script piping JSON into `nc -U $MAGMUX_SOCK` gets the same panel, and so
does an MCP client, which reaches magmux through `magmux mcp` translating tool
calls into exactly these verbs.

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

## Remote Control

Everything a program on the same machine can do over the [unix
socket](#ipc-socket-protocol), a program somewhere else can do over HTTP — the
same ops, the same events, the same words. Plus one thing the socket does not
have: a live view of a pane's screen, as JSON frames, over a WebSocket.

```bash
magmux --listen 127.0.0.1:7777 -e zsh
# magmux: listening on http://127.0.0.1:7777
# magmux: token in /tmp/magmux-48213.token (mode 0600, removed at exit)
```

It is **off unless you ask for it**. Without `--listen` no token is generated,
no file is written and no port is opened.

### The token

A pane is a shell, so the token is remote code execution, and magmux treats it
that way:

- **There is always one.** It comes from `MAGMUX_TOKEN`, from `--token-file`, or
  magmux generates one and writes it to `{sock-dir}/magmux-{id}.token` at mode
  0600, removed at exit. There is no unauthenticated mode.
- **There is no `--token` flag,** deliberately: a value on a command line is in
  every `ps` listing on the machine.
- **A token file must be a regular file you own with no group or other
  permission bits.** A symlink is somebody else's file; a group-readable token
  is not a secret. magmux refuses both rather than shrugging.
- **The port implies the file.** The token is resolved and written *before* the
  listener binds, so the moment the port accepts, the token file is there with
  its final contents. Poll the port, then read the file — there is no window.

```bash
TOKEN=$(cat /tmp/magmux-48213.token)
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:7777/v1/panes
```

Without the token every endpoint is `401`. There is no "read-only if you have no
credential" mode.

### Endpoints

| | |
|---|---|
| `GET /v1/capabilities` | version, geometry, and what this magmux is listening on |
| `GET /v1/ops` | every op, with its JSON Schema — including ops a plugin added |
| `GET /v1/panes` | the pane list; the same object `list` returns on the socket |
| `GET /v1/panes/{n}/screen` | a pane as text (`?lines=`, `?offset=` for scrollback) |
| `POST /v1/ops/{name}` | call any op; the body is its args |
| `POST /v1/tickets` | mint a single-use 30-second credential (see below) |
| `GET /v1/events` | the event stream, as server-sent events |
| `GET /v1/ws` | a WebSocket: ops, replies, events and live frames |

```bash
# open a pane, type into it, read it back, close it
curl -sH "Authorization: Bearer $TOKEN" -d '{"cmd":"bun test --watch"}' \
     http://127.0.0.1:7777/v1/ops/open_pane
curl -sH "Authorization: Bearer $TOKEN" -d '{"pane":2,"text":"a\r"}' \
     http://127.0.0.1:7777/v1/ops/input
curl -sH "Authorization: Bearer $TOKEN" \
     'http://127.0.0.1:7777/v1/panes/2/screen?lines=20'
curl -sH "Authorization: Bearer $TOKEN" -d '{"pane":2}' \
     http://127.0.0.1:7777/v1/ops/close_pane
```

Failures carry both an HTTP status and magmux's own code, because the status is
a coarse class several codes share: `{"ok":false,"code":"pane_dead","error":"…"}`
with a 409. Branch on `code`.

### A viewer that cannot type

`--view-token-file` is a second token that may READ and nothing else:
`capabilities`, `list`, `ops`, `capture`, `transcript`, and `watch` / `unwatch`
/ `resync` so it can follow a pane live. `input`, `send`, `open_pane`,
`close_pane`, `resize`, `status`, `tint`, `overlay` and every plugin op are `403`. It is
for a dashboard on a wall, a
screen-share, a status page — anywhere you want the session visible without
handing over the keyboard.

It is never generated. Write one yourself:

```bash
head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n' > /tmp/view.token
chmod 600 /tmp/view.token
magmux --listen 127.0.0.1:7777 --view-token-file /tmp/view.token -e zsh
```

`--view-op plugin.op` opens one exception at a time, for a plugin op you have
decided a viewer may call (`--view-op ticket.status`). It only accepts a
qualified plugin name, so it can never widen a built-in.

### Drive magmux from a browser or an agent

A browser cannot set an `Authorization` header on an `EventSource` or a
`WebSocket`, so magmux gives it two ways in that keep the raw token out of
URLs and proxy logs:

```js
// 1. a WebSocket: the token rides as a second subprotocol
const ws = new WebSocket("ws://127.0.0.1:7777/v1/ws",
                         ["magmux.v1", `magmux.auth.${token}`]);

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);
  if (msg.type === "snapshot") drawPaneList(msg);   // the first message, always
  if (msg.type === "frame")    paint(msg);          // key: full screen, else a diff
};

ws.onopen = () => {
  ws.send(JSON.stringify({ id: 1, op: "hello", args: { client: "my-dashboard" } }));
  ws.send(JSON.stringify({ id: 2, op: "watch", args: { pane: 0, fps: 15 } }));
  ws.send(JSON.stringify({ id: 3, op: "input", args: { pane: 0, text: "ls\r" } }));
};
```

```js
// 2. an EventSource: mint a ticket first, spend it in the URL
const { ticket } = await (await fetch("/v1/tickets", {
  method: "POST", headers: { Authorization: `Bearer ${token}` },
})).json();
const es = new EventSource(`/v1/events?ticket=${ticket}&watch=0,2&fps=10`);
```

A ticket is single-use, lives 30 seconds, and inherits the kind of the token
that minted it — a viewer's ticket is still a viewer. It is accepted on exactly
two endpoints, `/v1/events` and `/v1/ws`, because those are the two that cannot
carry a header.

A **frame** is a row-level diff: `lines` carries only the rows that changed,
each with its text and a list of `[col, len, fg, bg, attr]` style runs, plus a
header with the cursor, the geometry and whether the pane is on its alternate
screen. `key: true` means "clear and redraw, this is every row". Ask for
`mode: "notify"` instead and you get a bare `{"type":"changed","pane":n}` — for
a client that would rather fetch the screen itself than be sent one.

An **agent** reaches the same surface through [`magmux mcp`](#magmux-mcp),
which turns every pane into a readable, subscribable resource and every plugin
op into a tool. Nothing about the session is different; it is the same hub seen
from another window.

### Over a network

`--listen 127.0.0.1:7777` and an ssh tunnel is the safe default and needs
nothing else. If you bind somewhere reachable, use TLS:

```bash
magmux --listen 0.0.0.0:7777 --tls-cert cert.pem --tls-key key.pem \
       --allow-origin https://dash.example.com -e zsh
```

magmux prints a loud warning for a non-loopback bind without `--tls-cert`,
because the token and every keystroke would cross the network in clear text. It
is a warning and not a refusal: a Tailscale or WireGuard interface is a
perfectly good place to bind, and magmux cannot tell one from a café LAN.

For browsers, `--allow-origin` is the CORS allow-list; without it only
same-origin requests pass. On a loopback bind magmux also checks the `Host`
header, which is what stops a page on the internet driving your laptop's magmux
through DNS rebinding.

## Plugins

A plugin is a separate process that connects to magmux's socket like any other
client and then registers a name and some ops. From that moment its ops are in
magmux's op table as `<plugin>.<op>` and **every** transport can call them —
`call` on the socket, `POST /v1/ops/ticket.run_ticket`, a WebSocket message, an
MCP tool (`ticket__run_ticket`), a signed Firebase command. There is no code in
magmux that knows what your plugin does.

```bash
magmux --listen 127.0.0.1:7777 -e zsh \
       --plugin 'bun examples/plugins/ticket-runner/main.ts'
```

```bash
curl -sH "Authorization: Bearer $TOKEN" \
     -d '{"title":"run the migration"}' \
     http://127.0.0.1:7777/v1/ops/ticket.run_ticket
```

[`examples/plugins/ticket-runner`](examples/plugins/ticket-runner) is the whole
thing in ~300 lines of TypeScript, with no model, no network and no API key: it
opens a pane with `controller:"self"`, waits for its agent's prompt, sends the
ticket, emits `progress` events as it goes, and finally reports the pane as
`awaiting_input` carrying the agent's answer. `sdk.ts` beside it is the client
library; `client/pluginsdk.go` is the Go equivalent.

That last step is the point. `awaiting_input` is what a driver waits for, and a
plugin saying it is a claim about a session magmux is not itself following —
magmux reconciles it with what the terminal actually saw, and the control panel
attributes the pane to the plugin that claimed it, so "who said this session was
done" stays answerable.

A plugin magmux spawned authenticates with a one-time token magmux puts in its
environment; one you started yourself uses the session token. Its ops vanish the
moment it exits, calls in flight become `plugin_gone`, and one that hangs is
cancelled and reported as `timeout`.

## Firebase

`--firebase config.json` mirrors the session into a Firebase Realtime Database:
pane state, the op catalogue, an event ring and — at 2 fps, shed first when the
byte budget is tight — the screens. It exists so a phone on a train can watch a
build finish. It is not a remote terminal; for that, watch over a WebSocket.

```json
{
  "databaseURL": "https://your-project-default-rtdb.firebaseio.com",
  "root": "magmux",
  "host": "laptop",
  "credentials": "~/.config/magmux/sa.json",
  "commands": {
    "enabled": true,
    "owners": ["YOUR-FIREBASE-UID"],
    "keyFile": "~/.config/magmux/cmd.key",
    "allowOps": ["list", "capture", "send"]
  }
}
```

With `commands.enabled` a client can write a command back and magmux will run
it. That direction is defended twice, and either layer alone would fail open:

1. The **security rules** in [`examples/firebase/database.rules.json`](examples/firebase)
   refuse a write from a uid that is not an owner, and pin `uid` to the
   authenticated user. A forged command never lands.
2. **magmux** verifies an HMAC over the command with a key that is never in the
   database, plus a timestamp window, a nonce and the `allowOps` list. That is
   the layer that survives a stolen session or a mis-set rule.

A command runs **at most once**: magmux writes a durable claim and waits for the
database to acknowledge it before any side effect, so a crash leaves "outcome
unknown" rather than a command that runs twice.

[`examples/firebase/`](examples/firebase) has the rules, a config template, an
emulator setup and `hmac-vector.json` — the committed test vector any client
that signs commands must reproduce exactly.

## Subcommands

### `magmux mcp`

`magmux mcp` runs magmux as an [MCP](https://modelcontextprotocol.io) server on
stdio, so an AI agent can spawn and drive real panes — real PTYs running real
interactive tools, with a human watching every one of them. It is the same
socket protocol as everything above; the MCP server is a translator, not a
second API.

**Session-scoped, and usually what you want.** `--mcp-config` takes a JSON file
*or* an inline JSON string, and persists nothing:

```bash
claude --mcp-config '{"mcpServers":{"magmux":{"command":"/path/to/magmux","args":["mcp"]}}}'
```

Add `--strict-mcp-config` to ignore every other MCP configuration for that
session — the right setting when what you are testing is magmux itself.

This fits the flagship case above, where the agent runs *inside* a magmux pane,
inherits `MAGMUX_SOCK` and attaches to its own host session: the server exists
for exactly as long as the window does. Nesting the inline form in `-e` is not
worth the quoting, though — that command goes through `$SHELL -l -c`, so the
single quotes collide. Put the same JSON in a file, as `test/ui/case4.ts` does,
and pass the path:

```bash
cat > /tmp/magmux-mcp.json <<'JSON'
{"mcpServers":{"magmux":{"command":"/path/to/magmux","args":["mcp"]}}}
JSON
magmux -c -e 'claude --mcp-config /tmp/magmux-mcp.json'
```

**Persistent,** for when you want magmux available in every session:

```bash
claude mcp add magmux -- /path/to/magmux mcp
```

It speaks JSON-RPC 2.0 as newline-delimited JSON. stdout carries protocol only.
`MAGMUX_MCP_LOG` sends logs to a file instead of stderr.

**Tools:**

| Tool | |
|---|---|
| `list_sessions` | every magmux on this machine, with its panes and their states |
| `attach_session` | pick one by id, pid or socket path; it becomes the default |
| `request_session` | how to get a session when there is none — it never starts one |
| `list_panes` | index, label, state, command and pid for each pane |
| `open_pane` | split the layout and run a command in the new pane |
| `close_pane` | close a pane and reap its process |
| `read_pane` | the pane's state, its rendered screen (and its scrollback), its transcript |
| `send_keys` | type text and press named keys, without waiting for the turn. On a gated Claude pane it still waits for the box, and takes `dismiss_dialogs`, `ready_timeout_ms` and `no_gate` |
| `send_and_wait` | give an agent one instruction and wait for the resulting turn. Takes the same three gate arguments |

`send_and_wait` is the workhorse: it waits for the turn to visibly *start*
before waiting for it to finish, so it can never hand back the previous turn's
answer. `read_pane` is the agent's eyes, and it reads from two independent
sources: `transcript:N` returns the last N turns from the session's own record
on disk and is authoritative, while `screen` is what the pane is painting —
plus, with `offset:N`, the rows that have scrolled off above it. The
alternate-screen rule above applies here too and decides which source to reach
for: **an agent pane keeps no scrollback, so its transcript is its history; a
shell, build or dev-server pane has no transcript, so its scrollback is.**

**How it finds a session,** in order:

1. **The host session.** The agent is already running inside a magmux pane, so
   `MAGMUX_SOCK` is set and that session is used. This is the flagship case:
   the agent drives its siblings in the window the human is already looking at,
   and the human watches the whole time. (An agent may never drive its own
   pane — it would be waiting for a turn it is itself inside. That is refused.)
2. **A discovered session.** One reachable `magmux-*.sock` in the socket
   directory (`/tmp`, or `MAGMUX_SOCK_DIR`), or whatever `attach_session` is
   pointed at. If the magmux you want was started with `--sock-dir`, set
   `MAGMUX_SOCK_DIR` to the same directory in this client's own `env` block —
   `--sock-dir` reaches magmux's children, but nothing magmux does at runtime
   can reach a server the client launched itself. A `MAGMUX_SOCK_DIR` that is
   missing, not a directory or not writable stops `magmux mcp` before it
   serves: exit 1, the reason on stderr, nothing on stdout.
3. **Inside tmux.** `request_session` tells the client to use **its own** tmux
   MCP tooling to split a pane and run `magmux --id NAME -c -e '<cmd>'` there,
   then call `attach_session`.
4. **Otherwise**, it hands the human that same command to run, and waits to be
   pointed at the result with `attach_session`.

**magmux never execs `tmux` itself**, and never starts a magmux of its own. A
session nobody can see defeats the design: the point of driving a real pane
rather than a subprocess is that a human can read it, interrupt it, and take
the keyboard. `-c` in those instructions adds the control panel, so what the
agent asked for and what the session actually did are both on screen.

### The self-hosting demo

```bash
task mcp:demo
```

The flagship flow, live and in one window. It opens bare, the way magmux opens
by default: one full-screen Claude Code session, `--mcp-config` pointed at the
freshly built binary, nothing else on screen. That session then splits **its
own multiplexer** — `open_pane` — to start a second Claude Code session in a
scratch directory, and drives it with `send_and_wait` until it has produced
`squares.txt` holding `1 4 9 16 25`.

Nothing is configured to make the attachment happen. magmux exports
`MAGMUX_SOCK` before any child starts and Claude Code hands its environment to
the MCP servers it spawns, so `magmux mcp` finds the window it is already
inside.

What to watch for:

- **the window splitting itself**, mid-run, while you are looking at it — a
  full-screen session becomes two panes with nobody at the keyboard. This is
  the moment, which is why the demo does not start with the panel up;
- **the routing, once you ask for it.** `Ctrl-G p` reveals the
  [control panel](#the-control-panel) — `▶ open_pane`, `▶ send_and_wait`, and
  `◀` rows for the turns magmux itself observed the new session take. The `▶`
  rows are the driver's request; the `◀` rows are not, so the panel can show
  the two sessions disagreeing. `Ctrl-G p` hides it again, and the status line
  carries the same counters as a digest while it is away;
- **the file on disk**, printed after the window exits. The panel is only as
  trustworthy as what each agent says it did; the file is the part neither of
  them can print into existence.

`MCP_CLOSE_AFTER=20` closes the window twenty seconds after everything settles
instead of waiting for `q`, and `MCP_MODEL=opus` picks the driving model.
`task test:mcp` is the automated cousin: same artifact, but the driver is
denied `Bash`, `Write` and `Edit` so it *cannot* do the work itself.

> **Not yet run end to end.** It spends real model budget and needs a TTY, so
> the flow above is written but unverified.

## Why Go?

The original MTM is ~1,800 lines of C using ncurses. magmux is ~2,100 lines of Go with no ncurses dependency — raw ANSI escape codes to stdout. Go provides:

- Memory safety (no buffer overflows in escape sequence parsing)
- Goroutine-per-pane concurrency (simpler than C's `select()` loop)
- Static binary distribution (no ncurses/libc dependency)
- Accessible to teams that don't maintain C code

## License

MIT
