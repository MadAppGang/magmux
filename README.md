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

magmux shows nothing of itself by default. `magmux -e 'claude'` is a bare
terminal running Claude Code: one pane, no border, the whole window. The
[control panel](#the-control-panel) is always there — it just starts hidden,
and `Ctrl-G p` reveals it without losing a row of its history. `Ctrl-G s` does
the same for the status bar.

| Flag | |
|---|---|
| `-e CMD` | run CMD in a pane (repeatable) |
| `-g FILE` | grid file, one command per line |
| `-w` | exit once every pane is done |
| `-c` | start with the [control panel](#the-control-panel) visible |
| `--no-status` | start with the status bar hidden |
| `-x SECS` | close SECS after a driver finishes (default: wait for a keypress) |
| `--id NAME` | bind `/tmp/magmux-NAME.sock` instead of the pid socket |

Subcommands: [`magmux mcp`](#magmux-mcp) runs magmux as an MCP server.

## Controls

| Key | Action |
|-----|--------|
| `Ctrl-G q` | Quit |
| `Ctrl-G Tab` | Switch focus to next pane |
| `Ctrl-G p` | Show / hide the [control panel](#the-control-panel) |
| `Ctrl-G s` | Show / hide the status bar |
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

## Configuration

| Env Variable | Default | Description |
|---|---|---|
| `MAGMUX_SCROLLBACK` | `1000` | Lines of [history](#scrollback) kept per pane; `0` turns it off |
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

`--id NAME` binds `/tmp/magmux-<name>.sock` instead — the path is then known
*before* magmux starts, which is what lets a caller that did not fork magmux
find it. `NAME` is restricted to `[A-Za-z0-9_-]{1,64}`; an invalid one is
ignored with a line on stderr and the pid socket binds as usual. The pid
default is unchanged, and `--id` **replaces** it rather than adding to it.

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
| `pane_opened` | `pane` (int), `cmd`, and `cwd` / `label` when they were given | when `open_pane` adds a pane |
| `pane_closed` | `pane` (int) | when `close_pane` removes one |
| `reply` | `id`, `ok`, plus `result` or `code`+`error` | answer to *your* message, if it carried an `id` (see below) |

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
| `controller` | string | controller name, if any (e.g. `claude`) |
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
| `label` | string | the name given at `open_pane` (omitted when empty) |
| `cmd` | string | the pane's command line, if known (omitted otherwise) |
| `cwd` | string | the pane's working directory, if known (omitted otherwise) |
| `pid` | int | the pane process's pid, if running (omitted otherwise) |
| `inputSignal` | string | which heuristic called the pane idle — `osc`, `2004`, `title`, `idle`, `ctrl`, `perm` (omitted when none) |

The last nine are additive; the forward-compat clause above already covers
them, and every one is omitted rather than reported as a zero when it cannot be
sourced.

**Pane ids are sparse.** `close_pane` tombstones the slot and nothing ever
renumbers, so ids can have gaps and `results` can contain entries with
`"state":"closed"`. A client treating the `panes` array as a dense positional
list will address the wrong session after the first close: the `pane` field was
always the authoritative index, and now it matters. Tombstones are reported
rather than omitted precisely so the hole is visible.

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
reworded at any time.

| `code` | Meaning |
|---|---|
| `bad_request` | the message is malformed, or a field is not what the verb needs |
| `no_such_pane` | no pane with that index (it may have been closed) |
| `pane_is_control` | that pane is the control panel, which has no process |
| `pane_dead` | the pane's process is gone, so the bytes had nowhere to go |
| `unknown_verb` | this magmux does not know that `type` |
| `unsupported` | known verb, not available in this mode |
| `busy` | the pane already has a request of this kind in flight |
| `timeout` | the verb's budget expired |
| `too_small` | `open_pane`: no room to split without going below the minimum pane size |
| `internal` | magmux's own fault |

`reply` is never replayed to a late connection, so it cannot land after
`results`; the `results` → `shutdown` → EOF ordering is unaffected.

For `send`, `ok:true` means the bytes reached the PTY — not that the TUI
accepted them. An app that was not ready for a paste can still drop it.

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
pane tints, and agent hook events; see `sockMsg` in `main.go`. These are
optional and unrelated to the read-only subscribe flow above.

`{"type":"tint","color":"green"}` colours the pane's **border** (green, red or
yellow; `""` clears it). It deliberately does not touch the pane's interior:
magmux does not know what foreground colours the program in the pane is using
and cannot recolour them, so any background it painted underneath them would be
illegible for somebody — a dark wash hides a dark-on-light program, a light one
hides a light-on-dark program like Claude Code. A pane with no split beside it
has no border to colour; use `{"type":"overlay"}` there, which sets both halves
of its own colour and is legible anywhere.

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
the text before it is submitted. `label` is the short tag the panel shows in its
verb column; an MCP client puts its tool name there, which is that layer's
entire footprint on this protocol.

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
| `send_keys` | type text and press named keys, without waiting |
| `send_and_wait` | give an agent one instruction and wait for the resulting turn |

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
2. **A discovered session.** One reachable `/tmp/magmux-*.sock`, or whatever
   `attach_session` is pointed at.
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
