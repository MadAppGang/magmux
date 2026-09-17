# magmux remote control: one hub, two clients, one public API

```bash
task demo:rc
```

Two stacked tmux panes and a browser tab. **Run it from inside tmux and it uses
the tmux you are in** — see the two modes below.

| | |
|---|---|
| **top pane** | a real magmux with a real HTTP/WebSocket listener on loopback — **at the full size of your window**, everything the driver does not need |
| **bottom pane** | **the driver** — a menu of real interactions, and where you start. 14 rows, and inside tmux it is *the pane you typed in* |
| **browser tab** | the same decoder and the same core, painting DOM — **the terminal fills the page**, with the driver's actions as a panel beside it. Press `f` to give it the whole page |
| *(optional)* | **`RC_DEMO_MIRROR=1`** adds a third pane: a read-only client painting the same frames as ANSI, in a terminal |

**magmux gets the lion's share of the window, and that is the point.** On a
50-row terminal that is 35 rows of magmux against the driver's 14 — a real
terminal, at your terminal's real width, which is what both mirrors then show.
The window chrome in the browser prints that size (`pane 0 · 200×34`), so a
small terminal there always means a small terminal here.

**Press a number and Enter in the bottom pane, or click an action in the browser's driver
panel.** Every action is one HTTP request to magmux over the public API, and the
effect is on both mirrors a moment later. You can also just type into the top
pane by hand — nothing in this demo self-drives. The panel in the page is an
ESCALATION and the page says so in its footer: `demo/rc/serve.ts` holds the
session token and types on the page's behalf. `RC_DEMO_WEB_DRIVER=0` removes it
and leaves the page purely read-only.

## Two tmux modes, and the demo is a GUEST in one of them

The mode is measured, not configured: it is whether `$TMUX` is set — i.e.
whether you are already sitting in tmux.

### Inside tmux → **adopt** (the usual case)

No new server, no new session, **no new window**, and nothing is ever attached.
The launcher is already running in a pane of your own window, so **that pane is
the driver**: it splits panes above itself and then runs the menu right there.

```
┌────────────────────────────┐
│ magmux          (new pane) │  ALL the rows nothing else needs
│                            │  — 35 of a 50-row window
│                            │
├────────────────────────────┤
│ the mirror      (new pane) │  only under RC_DEMO_MIRROR=1
├────────────────────────────┤
│ the driver ← THE PANE YOU  │  where you typed `task demo:rc`
│              TYPED IN      │  14 rows, the compact menu
└────────────────────────────┘
```

The size is a **fact** here rather than a guess: what is subdivided is
`#{pane_height}` and `#{pane_width}` of the pane you typed in, on a real client.

**The guest rule.** Teardown kills exactly the two panes it created, each proven
this run's by two facts that must agree: the `#{pane_id}` captured at creation
and a `@rc_demo` pane option set on it. Never `kill-window`, never
`kill-session`, **never `kill-server`**, and `destroy-unattached` is never set on
anything — that option on your session would destroy your work the moment you
detached. Your other windows, your session and your tmux server are untouched,
and when the demo ends your pane is back at full height with your prompt in it.

`q` or `Ctrl-C` ends it: the driver is this launcher's own foreground process,
so they are the same exit hitting the same trap. **Detaching does not end it.**

### Outside tmux → **own server** (a plain terminal)

A private server (`tmux -L magmux-<id>`), its own session **created at this
terminal's real size**, its stacked panes, attached at the end. The `-x`/`-y` is
load-bearing: `new-session -d` has no client to take a size from and defaults to
**80x24**, so a session created without them lays its panes out 80 columns wide
whatever terminal you are sitting at — and every client, the browser included,
is then shown an 80-column terminal because the source really is 80 columns.
Nothing of anyone's is on that server, so teardown may
`kill-server`, the `destroy-unattached` hook can reach nothing else, and
**detaching does end the demo** — all correct when the demo is the only thing
there.

`RC_DEMO_TMUX=own` forces the private server from inside tmux (the old
behaviour); `RC_DEMO_TMUX=adopt` forces adoption, which needs `$TMUX` to point
at a server.

### Panes, never windows

One window, split into panes — always, in both modes. The demo's whole point is
watching one instruction reach both clients at once, and with a window per
surface you are never looking at the mirror while it happens. A short window is
answered by dropping the terminal mirror — which is the default anyway — never
by scattering the demo across windows, and a shortfall is said out loud with the
row counts rather than silently worked around. `RC_DEMO_LAYOUT=windows` is still there for anyone who
wants the literal separate-terminals reading, and it is own-server only.

That is the whole demonstration, and it is the whole claim: one hub, two front
ends, not two code paths that resemble each other.

```
 you type into the magmux pane
   → writePTY → the shell's PTY → output → Pane.readLoop
   → noteOutputLocked (one atomic increment) → wakes the pane's framer
   → the framer diffs ONCE against the shared shadow, encodes changed rows once
   → offers the frame into each subscriber's latest-wins slot
        ├─ Sub#1 → demo/rc/cli.ts   → applyFrame → ANSI painter → tmux pane B
        └─ Sub#2 → demo/rc/web/app.ts → applyFrame → DOM painter → the page
```

## The driver

`demo/rc/driver/` — a **Bubble Tea TUI**, in the bottom pane, or on its own
against a demo that is already running:

```bash
task demo:rc:drive        # a second terminal, same magmux
```

**It holds the FULL SESSION TOKEN and both mirrors hold the READ-ONLY view
token.** A permanent header row says so, in two badges, with a fingerprint of
each — and every action carries a coloured bar down its left in the same two
hues, so which credential an action spends is readable before anything is
pressed. That contrast *is* the demo: this driver can type into a shell, and
the two surfaces watching it provably cannot — actions 8 and 9 make magmux say
it in its own words, off the wire, rather than in a message anyone here wrote.

The screen is a dashboard, not a transcript. The actions stay on screen while
the response has a home beside them, and the telemetry is visible rather than
inferred:

```
 magmux rc  [LIVE]  pane 0 · 120×38  seq 812    rtt ███▓▒░ 0.67ms  frames ▁▂▄▆█▇▅▃ 91
 ▸ this driver [FULL SESSION TOKEN] 4_zmaC…AbO8   ▸ every mirror [READ-ONLY VIEW TOKEN] c6d63a…a487
 ╭ actions 13 ─────────╮╭ request / response [403] ███▓░ 0.28ms ────────────────────╮
 │▌  7 close it        ││ ▶ POST /v1/ops/input  {"pane":0,"text":"rm -rf /"}        │
 │▌▸ 8 try `input`,VIEW││   · as the VIEW token                                     │
 │▌  9 try `open_pane` ││ ◀ [403] {"code":"forbidden","error":"op \"input\" is …"} │
 ╰─────────────────────╯│ [✓ BOUNDARY PROVEN]                                       │
 ╭ panes 2 ────────────╮│ 403 forbidden, off the wire, in magmux's own words:       │
 │◀  0 [running] demo  ││ ↳ the mirrors show nothing new, because nothing happened  │
 ╰─────────────────────╯╰───────────────────────────────────────────────────────────╯
 ╭ ops by class 13 · rev 1 ───────────────────────────────────────────────────────╮
 │ ███         ███     ← amber: reachable with the view token. violet: not.       │
 ╰────────────────────────────────────────────────────────────────────────────────╯
 ↑↓ move · enter run · tab focus · ]/[ retarget · q quit    ↳ watch the mirror pane…
```

Every instrument on it is **measured, not decorated**:

- **`frames`** is a sparkline of frame arrivals per second over the last half
  minute, off the driver's own WebSocket subscription. Press action 2 and watch
  it move.
- **`rtt`** is a gradient meter of the last liveness probe's round trip, on a
  logarithmic scale — a loopback op answers in a few hundred microseconds and a
  link across a VPN in tens of milliseconds, and a linear meter with a ceiling
  high enough for the second is flat for the first. The same meter sits beside
  every response.
- **`LIVE` / `STALE` / `DEAD`** comes from the same liveness rule both mirrors
  use: a read-class `list` every 2s with a 3s budget, and two consecutive misses
  is a verdict. **Silence is never the signal** — an idle pane legitimately
  produces no frames.
- **`ops by class`** is `GET /v1/ops` as a bar chart, coloured by which
  credential opens each class. It is actions 8 and 9 drawn in advance, and it
  grows a `+1 plugin` badge when action 10's plugin registers.

A refusal that was the point reads as a **success**: the `403` badge stays red,
because that is what magmux said, and the verdict badge beside it is a green
`✓ BOUNDARY PROVEN`. Only a 403 that was *not* expected turns the verdict red.

The layout is a size budget, so it degrades rather than breaking: below ~17 rows
the log panel folds away and the response panel takes its rows; below the space
for a pane table and an op chart, the left column is just the actions. It is
built to be legible in the demo's own **14-row** pane and at **80×24**.

### Light and dark

The palette is a pair, resolved through Lip Gloss's `LightDark`: every token has
a light member dark and saturated enough to read on a cream terminal and a dark
member bright enough to read on a black one. The background is asked for at
startup (`tea.BackgroundColorMsg`) and can be pinned with `--theme light|dark`
or `MAGMUX_THEME` — which is also what magmux exports into every pane it opens,
so a driver started inside a magmux pane inherits the resolution magmux already
made. There is no third case: a terminal that answers nothing is treated as
dark, because a dark palette on a light terminal is merely low-contrast while
the reverse is invisible.

### `--run N`, and the two shapes that are not a TUI

A TUI cannot be driven by piping stdin, so the driver has three faces and they
are three renderings of **one** stream of evidence — the actions emit typed
`Line` values and each face draws them:

```bash
.task-grids/rc-driver --state $STATE --id $ID --run 8   # one action, plain text, real exit code
```

- **the TUI**, when stdin *and* stdout are both a terminal;
- **a line menu**, when either is not — a pipe, a heredoc, `demo.sh` spawned by
  a test. It prints the same banner, the same numbered menu and the same
  request/response lines the old CLI printed, and `q` still quits it;
- **`--run N`**, which performs one action, prints its evidence as plain text
  and exits `0` when every verdict it made was the one it set out to prove, `1`
  when one failed or it could not run at all, and `2` when there is no such
  action. `demo/rc/selftest.ts` drives this.

`--url`, `--token-file`, `--view-token-file`, `--state`, `--id` and `--pane` all
still work, and `--yes` pre-confirms the one action that asks.

### Why the driver is its own Go module

`demo/rc/driver/go.mod` is a **separate module** with a `replace` back to the
repository. magmux's root module has exactly two dependencies — `x/sys` for the
PTY ioctls and `x/term` for raw mode — and that is a property of the project
rather than an accident: the control panel is raw ANSI written through magmux's
own VT parser instead of through a TUI library precisely to keep it. Bubble Tea,
Lip Gloss, Bubbles and ntcharts are some twenty modules between them, and a demo
does not get to spend that.

`go list -m all` at the root still answers three lines, and `go test ./...`
never descends into a nested module — which is the whole mechanism this rests
on, so `demo/rc/selftest.ts`'s last three checks assert both, and that the
driver module builds and vets on its own.

It **is** allowed to reach back into magmux through the `replace`: it decodes
frames into `protocol.Frame` and speaks WebSocket through `transport/ws`'s
client half rather than carrying a second copy of RFC 6455 and the frame schema.
A second decoder that is subtly wrong does not fail — it draws a slightly
different picture nobody notices.

`demo.sh` and `task demo:rc:drive` both **`go build`** it, on every start, into
`.task-grids/rc-driver`. Not `go run`, which links a throwaway binary every
time; and not a cached artefact, because a stale driver against a changed
repository shows yesterday's behaviour with no sign that it is doing so. Go's
build cache makes an unchanged rebuild a couple of hundred milliseconds, which
is the price of never being stale.

| | Action | What it really does | Credential |
|---|---|---|---|
| **1** | type a command | `input` — `ls --color=auto`, then Enter | session |
| **2** | a burst of scrolling output | `input` — 200 lines, to watch row deltas keep up | session |
| **3** | run `top` | `input` — the **alt screen**; magmux forces a keyframe | session |
| **4** | quit top | `input` with `keys:["q"]` — back to primary, another keyframe | session |
| **5** | open a second pane | `open_pane` — magmux splits its own layout | session |
| **6** | point the mirrors at it | prints the pane list and **how each client retargets itself** (below) | session |
| **7** | close it | `close_pane`, then the *same id* refused `404 no_such_pane`, then a new pane taking the **next** id | session |
| **8** | `input` as a viewer | **403 forbidden**, printed verbatim: `class input` | **view** |
| **9** | `open_pane` as a viewer | **403 forbidden**: `class control` — then `GET /v1/panes` at **200**, so it is a capability and not a ban | **view** |
| **10** | run a ticket | `ticket.run_ticket`, an op from the plugin, then polls `ticket.status` to `done` | session |
| **11** | stall a WebSocket client | a real subscriber that handshakes, watches at 30fps, reads one frame and then reads nothing, held while the session is driven | session |
| **12** | **kill magmux** | SIGKILL, after typing `kill` to confirm (`--yes` for `--run 12`). **Ends the run** | — |
| **13** | print the next raw frame | opens a *viewer's* connection and prints the frame JSON | **view** |

**Action 12 ends the demo.** It asks for an explicit `kill` first — a confirm
box that owns the keyboard until it is answered, so no stray Enter reaches it —
it is the last destructive action, and afterwards it prints what happened — SIGKILL means there
was no `results` → `shutdown` → EOF, so both mirrors see their socket simply
end, report the close code in red and exit 1. SIGTERM would have given the
orderly path and a green exit 0; the item says so rather than leaving it to be
inferred. It finds magmux by `--id <this run> --view-token-file`, the two flags
`server.sh` writes adjacently, and never by `pkill -f magmux` — which is how
somebody loses the magmux they were working in.

**Action 10 needs the plugin**, which `server.sh` starts with
`--plugin 'bun examples/plugins/ticket-runner/main.ts …'`. A plugin that fails
to start is reported and skipped rather than fatal, so action 10 checks the op
table first and says so plainly instead of erroring. The plugin's log goes to
`{sock-dir}/magmux-{id}.plugin-ticket.log`, never to a pane — magmux never lets
a plugin write to the terminal, which is what keeps it off a screen the demo is
mirroring.

### Action 6, and the thing the public API deliberately cannot do

A watch is **per-subscriber** state: the hub keeps one latest-wins slot per
(connection, pane), and magmux offers no op that reaches into somebody else's
connection to point it at a different pane. That is not a gap to work around. A
demo that invented a private side channel here would be contradicting the one
public API it exists to demonstrate.

So retargeting is the **client's own act**, and both clients now expose it —
`RemoteClient.retarget()`, which is `unwatch` then `watch` on its own socket:

- **terminal** — focus the mirror pane (`<prefix> ↑`; the driver prints your own
  prefix key, since a hardcoded `Ctrl-B` is a key that does nothing under
  `set -g prefix C-a`) and press `]` or `[` to
  cycle, or the pane's digit. The status bar carries the list, `[ ] <0> 2 3`,
  with the current pane in angle brackets.
- **browser** — the pane picker in the status bar, top right.

Both keep their pane list from the connect-time aggregate and then from
`pane_opened` / `pane_closed`, so a pane the driver opens appears in both
pickers without anything being asked for. The control panel is excluded from
both: it is magmux's own chrome, it never changes, and a viewer who landed on
it would conclude the mirror had died.

Item 6 prints the pane list and those two instructions. It does not pretend to
do the switching itself.

### The page, and the driver panel in it

The browser tab is not a thumbnail of the demo, it is the demo: the font size is
computed from the pane's own geometry and the room the window leaves, in BOTH
dimensions — the limiting one wins, so a tall pane uses the height instead of
overflowing and a short one is not stretched into a letterbox strip. It is
recomputed on a window resize AND whenever a frame header reports a new
geometry, because a pane reflows the moment anybody drags the window.

**`f` gives the terminal the whole page**, and the `⤢ full width` control in the
status bar does the same; `Esc` comes back. The driver panel and the footer go,
the stage becomes the window and the terminal refits into it. The STATUS BAR
never goes: it is the only thing that can tell a viewer the picture has stopped
being live, and a full-screen mirror with no way to say so is the still picture
this demo exists to make impossible. Below a readable floor it stops shrinking and the terminal scrolls,
and the title strip says so rather than clipping in silence. The status bar
(state, pane, geometry, seq, frames, fps, age, `alt`, `cursor hidden`) and the
read-only footer are unchanged in what they say. Dark by default;
`prefers-color-scheme: light` re-colours the page's chrome and deliberately
leaves the SCREEN dark, because it is a picture of a terminal and repainting a
shell's own colours onto white would misreport the thing being mirrored.

**The panel is the driver's actions, in the page**, and it prints each exchange in
the same shape (`▶ POST …` / `◀ HTTP 200 …`) so the two drivers show identical
evidence for the same action. Eleven of the thirteen items are there. Two are
not:

- **item 11, stall a WebSocket client** — it needs a socket that handshakes and
  then stops reading. A browser's WebSocket always drains its own receive buffer
  and gives page script no way to stop it, so a page cannot stage that failure at
  all; the driver stages it with a real socket it stops reading.
- **item 12, kill magmux** — it is not an HTTP op. It is a SIGKILL to a pid found
  by argv, which is precisely the "run something on this machine" power this
  endpoint is built NOT to have, and it would end the run the page is watching.

Item 6 is there as *list the panes*: the retargeting itself is the page's own act
through the pane picker, for the reason below. Item 13, the raw frame, is done
**in the browser over the page's own read-only connection** — it needs no
escalation, so it must not have one.

### The panel's credential, and the escalation it is

The page holds the READ-ONLY view token, and that is its whole security claim.
The driver's actions need the full session token, so **the page is not given
one**: the buttons POST an action NAME to `demo/rc/serve.ts`, which holds both
credentials, performs the action, and returns magmux's own status, body and
timing unchanged — including the 403s. The page's WebSocket stream is untouched:
view token, in the browser, read-only.

**Say it plainly, because it is easy to miss.** Without the driver endpoint,
stealing this page's URL buys a read-only video feed of a shell. With it, it buys
the ability to TYPE INTO that shell. The guards are the same two the config
endpoint already has — an unguessable per-run path and `Sec-Fetch-Site:
same-origin` — and the prize behind them is now much bigger. **Same guards,
bigger prize.** `RC_DEMO_WEB_DRIVER=0` removes the endpoint entirely and leaves
the page purely read-only, and is the switch to use for anyone who wants that.

Two properties keep it from being a general proxy, and both are tested:

1. It is an **allow-list of named actions** (`ACTIONS` in `serve.ts`): the op,
   the method, the path, the credential and every argument are written there. An
   action that is not on the list is refused by `serve.ts` itself, with
   `"by": "serve.ts"` in the body so the panel can never print one of its
   refusals as though magmux had said it.
2. The only value a request may carry besides the action name is an **integer
   pane id**, validated as one. The page already knows every pane id from its own
   read-only aggregate.

The driver path is minted per run like the config path, and it is a DIFFERENT
path: it travels in the config response rather than in the URL, so the one path
that can type into a shell never reaches an address bar, a screenshot or a shell
history.

## How it ends

`demo/rc/demo.sh` is the sole entry point **and the sole owner of teardown**.
There is exactly one `trap … EXIT INT TERM` in it, and everything the demo
started dies there: magmux, the static server, the panes it created, both token
files and the state directory.

**Adopt mode**: `q` in the driver, or `Ctrl-C` in that same pane — the driver is
the launcher's own foreground process, so both are one exit through one trap.
Detaching leaves the demo running.

**Own-server mode**: `Ctrl-B d` detaches, the launcher's `attach-session`
returns, the trap runs, and the demo is over. **Detaching does not leave it
running in the background**, which will surprise a tmux user exactly once — so
it is said here, and in the banner. `Ctrl-C` in the launcher is the same thing.
To keep a session running past your terminal, this is the wrong tool: run magmux
yourself.

If the launcher was killed hard — `kill -9`, or the terminal window closed — the
trap never ran. That is the one case `task demo:rc:stop` exists for. It is
idempotent and safe to run when nothing is up.

## What is real

**All of it.** There is no staging here, unlike `demo/README.md`'s showcase,
where the agents are scripts printing agent-shaped output. Every byte on both
clients came out of a shell you are typing into.

| Piece | What it really is |
|---|---|
| the top pane | `./magmux`, built from this tree, with `--listen` on a kernel-chosen loopback port |
| the transport | RFC 6455 WebSocket on `/v1/ws`, the credential in `Sec-WebSocket-Protocol` |
| the frames | magmux's own row/run encoding from `mux/frame.go`, keyframe then deltas |
| the mirror pane | `bun demo/rc/cli.ts` under `RC_DEMO_MIRROR=1` — a separate process, no shared memory with magmux |
| the bottom pane | `demo/rc/driver/` — a Bubble Tea TUI and an HTTP client and nothing else; every action is a request you can curl |
| the plugin | `examples/plugins/ticket-runner` — a separate process whose ops magmux knows nothing about |
| the browser | the same TypeScript, bundled by `Bun.build` at startup, running in a browser |
| the credential | a real 0600 token file, refused by `auth.LoadFile` if it is anything else |

Nothing self-drives. A demo that produced output on a timer would make its own
central claim unfalsifiable: a client that had stopped receiving frames would
look exactly like a pane that had gone quiet. The pane is an interactive shell,
and the only two things that move it are your keyboard and the driver menu —
which moves it by asking magmux to, over the same API anything else would use.

## The two clients share a decoder, and that is load-bearing

```
demo/rc/frame.ts      decode only      — pure; no I/O, no DOM, no ANSI
demo/rc/client.ts     connection core  — WebSocket, hello/watch/unwatch,
                                         liveness, teardown; drives a Painter
demo/rc/cli.ts        Painter → ANSI
demo/rc/web/app.ts    Painter → DOM
```

The domain is not thin: keyframe-versus-delta merging, cell-column arithmetic
over double-width glyphs, three colour encodings and a nine-bit attribute mask.
Two copies would be two sets of bugs, and the browser copy is the one nobody
notices is wrong. `serve.ts` runs `Bun.build` on `web/app.ts` at startup, so the
browser really runs the shared source rather than a copy kept in step by
discipline — `demo/rc/selftest.ts` asserts the served bundle contains the
decoder's own symbols.

**The one review rule: `Painter.row()` takes a decoded row and cell columns,
never an ANSI string.** The moment it takes a string, the web painter becomes a
terminal emulator and the sharing is undone.

Not shared, deliberately: the colour → output mapping. `frame.ts` exports the
classification (`-1` default, `0..255` indexed, `>= 1<<24` truecolor);
`cli.ts` turns it into SGR and `app.ts` into CSS, because that is surface, not
protocol.

## Underline is missing from the colour card, and that is not a bug

The card `demo/rc/welcome.sh` prints is a test vector on screen: the 16 base
colours, two planes of the 6×6×6 cube, the greyscale ramp, a 60-step truecolor
sweep, and the attribute mask. Every branch of the decoder is exercised in the
very first frame both clients receive, so colour fidelity is checked by eye, on
two surfaces, before anything is typed.

The underline sample uses **SGR 21, not SGR 4**, and the reason is magmux's:

```go
// mux/vt.go:885
case p == 4: // underline — suppress like MTM
```

magmux deliberately drops SGR 4. SGR 21 (double underline) is what actually sets
`AttrUnderline`, which is the bit the frame protocol carries as `1<<6` and which
both painters draw. A card written with SGR 4 would show no underline anywhere
and look exactly like a mirror that had dropped an attribute — when in fact it
had reproduced magmux's own screen perfectly. This cost real debugging time
once.

## Both mirrors hold the READ-ONLY view token; the driver holds the session one

Neither mirror can type. That is not a convention, it is a capability:

- `mux/input.go:206` makes `input` `ClassInput`, and a view token is refused it
  with `403 forbidden`. `send`, `open_pane` and `close_pane` likewise.
- `hub/call.go:129-138` handles `watch` / `unwatch` / `resync` before the
  registry is consulted, so a read-only caller can still open a live stream.
- `mux/ops_builtin.go:94` makes `list` `ClassRead`, which is what the liveness
  probe uses.

So the demo hands out a capability that **provably cannot type into a shell**,
which is the honest thing to do with a page left open in a browser. magmux's own
session token exists — it is generated at startup because there is no "no token"
mode — and nothing in this demo reads it. Both files are removed at exit.

`demo/rc/selftest.ts` asserts the refusal on two transports rather than trusting
it, because "read-only" that is not tested is a comment.

## The flags, and why each one is there

`demo/rc/server.sh` is one `exec ./magmux` with the whole set in one place:

| Flag | Why |
|---|---|
| `--listen 127.0.0.1:0` | loopback only, kernel-chosen port. Nothing is reachable off the machine, and two demos can run side by side |
| `--id rc-demo` | makes the socket path knowable before magmux runs — the same reason `demo/showcase.sh` uses it |
| `--view-token-file $STATE/view.token` | the read-only credential. **It is never generated** — magmux refuses to invent a read-only capability nobody is tracking — so `serve.ts` mints the 0600 file before anything binds |
| `--sock-dir $STATE` | one directory to remove at exit, and this demo's socket stays out of the operator's `/tmp` |
| `--allow-origin http://127.0.0.1:<port>` and `http://localhost:<port>` | the static server's origin under both spellings a browser may use. magmux refuses any other `Origin` (`transport/httpapi/server.go:343`) |
| `--plugin 'bun examples/plugins/ticket-runner/main.ts --demo-id <id>'` | the demo plugin, so the driver's item 10 has a real op to call. Its log goes to `{sock-dir}/magmux-{id}.plugin-ticket.log` and never to a pane. The trailing `--demo-id` is ignored by the plugin and exists so its process carries this run's id in its argv like everything else, which is what lets the teardown check find it without `pkill` |
| `-e demo/rc/welcome.sh --label demo` | one session pane: the colour card, then an interactive shell |

There is deliberately **no `--token` flag** in magmux and none here: a value on a
command line is in every `ps` listing on the machine. Both clients read their
token from a file, which is what an operator does.

## The residual exposure, stated plainly

The browser page has to get the view token somehow, and a page cannot be handed
a file. So `demo/rc/serve.ts` serves it over HTTP on loopback. That is a weaker
gate than the 0600 permission `auth.LoadFile` enforces in code, and weaker than
`paneSecrets` (`mux/pane.go:229`), which strips `MAGMUX_VIEW_TOKEN` from every
pane's environment precisely so a shell inside a pane cannot read it.

Three things narrow it, and none of them is "the token is 0600 on disk":

1. **An unguessable per-run path.** The config is not at `/config.json`; it is
   at `/c/<32 random base64url chars>.json`, minted at startup. The path travels
   in the URL the launcher opens and appears nowhere else.
2. **`Sec-Fetch-Site: same-origin`, or a self `Origin`.** The browser sets the
   first on a same-origin fetch and page script cannot forge it. `curl` sends
   neither by default, so a curl from another shell on the same machine gets
   `403`.
3. **No `Access-Control-Allow-Origin` on the response.** Another origin may make
   the request; it cannot read the answer.

What remains: a process on this machine that can both guess the path and set the
header could read the token for as long as the demo runs, and would then have a
live video feed of the shell you are typing into. It could not type. The demo is
ephemeral, loopback-only, and removes both tokens at exit — but this is a demo's
trade-off, not a pattern to copy into something long-lived.

**And with the driver panel, it could type.** The endpoint that performs the
panel's actions holds the SESSION token, behind those same two guards on a second
unguessable path, so the same process that could steal the feed could also run
the allow-listed actions — which includes typing a command into the shell. Same
guards, bigger prize, stated here rather than left to be discovered.
`RC_DEMO_WEB_DRIVER=0` takes the endpoint away and puts the page back to
read-only.

## How it starts up, and why in that order

```
task demo:rc → demo/rc/demo.sh
   │
   ├─1─▶ bun demo/rc/serve.ts        127.0.0.1:<Pw>, kernel-chosen
   │        mints the VIEW token (0600) · Bun.build()s web/app.ts → /app.js
   │        serves / · /app.js · /c/<secret>.json · prints one `ready {…}` line
   │
   ├─2─▶ stacked panes — split into YOUR window in adopt mode, or into a session
   │      on a PRIVATE tmux server outside tmux, created at the REAL terminal size
   │        pane A (the rest) demo/rc/server.sh   2> $STATE/magmux.err
   │        pane B            bun demo/rc/cli.ts  — only under RC_DEMO_MIRROR=1
   │        pane C (14 rows)  demo/rc/driver — YOUR OWN PANE when adopting
   │
   └─3─▶ open http://127.0.0.1:<Pw>/?c=<configPath>
```

Two orderings are load-bearing and neither can be swapped:

- **The static server binds first**, because magmux's `--allow-origin` has to
  name a port the kernel has not chosen yet.
- **magmux binds second**, and the launcher then writes its URL into
  `$STATE/magmux.url`. The page has to learn magmux's own kernel-chosen port,
  which did not exist when the static server started; `serve.ts` reads that file
  **per request** and answers `503 {"error":"magmux has not bound yet"}` until it
  appears. Caching it at startup would serve `null`, and restarting `serve.ts`
  afterwards would break the `--allow-origin` promise.

Nothing sleeps waiting for startup. Both waits are the same idiom — poll a file
for a line, with a deadline, and print the whole file if the deadline passes:

1. `serve.ts` writes the token, builds the bundle, binds, and prints
   `ready {"port":…,"configPath":…}`. Credential before port, which is magmux's
   own security order (`mux/remote.go:16-24`).
2. magmux's stderr goes to `$STATE/magmux.err` and the launcher polls it for
   `magmux: listening on <url>`. That line is the documented readiness contract:
   an accepting port implies a complete token file at its final name.

## Failure behaviour

**Silence is never the signal.** An idle pane legitimately produces no frames —
that is what a wake-driven framer is for — so a client that treated a quiet
stream as failure would cry wolf every time you stopped typing, and one that
treated it as health would show a still picture of a magmux that died ten
minutes ago. So the client ASKS. Four things are verdicts:

| Detector | Mechanism | Verdict |
|---|---|---|
| liveness probe | every 2s, a read-class `{"op":"list"}` with a 3s budget; two consecutive misses | dead |
| socket closed | the `close` event; code and reason shown verbatim | dead |
| pane gone | `{"type":"pane_closed","pane":N}` | ended |
| orderly shutdown | `results` → `shutdown` → EOF, in that order | ended, exit 0 |

Between probes both clients show the age of the last frame in grey. Age is
information; a failed probe is a verdict.

The terminal client leaves the alternate screen, restores the cursor, prints the
reason in red on stderr and **exits non-zero**. The browser shows a red status
bar naming the reason, dims the screen so a stale picture cannot pass for a live
one, and offers a **Reconnect** button with no automatic retry — a silent retry
loop is exactly the still picture this is trying to prevent.

`remain-on-exit on` is set on both tmux panes, so a crash stays on screen with
its exit status instead of the pane vanishing. That is what makes the failure
behaviour visible rather than merely true.

## Knobs

| Variable | Default | Effect |
|---|---|---|
| `RC_DEMO_TMUX` | measured from `$TMUX` | `adopt` uses the tmux you are in; `own` starts a private server |
| `RC_DEMO_MIRROR` | `0` | `1` adds the terminal mirror pane — a third surface, at the cost of about half of magmux's rows. Worth it when the DECODER is what is under review; the browser tab is a second client either way. On a window too short for three panes the launcher says so and drops it |
| `RC_DEMO_GEOM` | — *(off)* | `COLSxROWS` (e.g. `120x36`) runs magmux **headless at that size with no magmux pane at all** — the escape hatch for a terminal too small for a pane worth looking at. `on` uses `120x36`. **Never chosen automatically.** See below |
| `RC_DEMO_LAYOUT` | `panes` | `windows` gives a tmux window per surface (own-server mode only). Saying `panes` explicitly asks for the three-pane layout on a window too short for it — the launcher warns that magmux may refuse it and builds it anyway |
| `RC_DEMO_DRIVER` | `pane` | `window` puts the driver menu in its own window (own-server mode only). Never chosen automatically |
| `RC_DEMO_DRIVER_ROWS` | `14` | how many rows the driver's compact menu is given |
| `RC_DEMO_MIN_ROWS` | computed (23, or 33 with the mirror) | the height the launcher tests the window against before choosing a layout |
| `RC_DEMO_WEB_DRIVER` | `1` | `0` removes the browser page's driver endpoint entirely — no path is minted, no action list is served, the panel is hidden — and leaves the page **purely read-only**. The switch for anyone who does not want a page that can type into a shell |
| `NO_BROWSER` | — | `1` prints the URL and opens nothing |
| `NO_ATTACH` | — | `1` builds the whole demo and never attaches; the launcher holds the session open and still owns teardown. This is the shape the automated end-to-end evidence uses, where the panes are read with `tmux -L magmux-rc-demo capture-pane -p -t %0` |
| `RC_DEMO_ID` | `rc-demo` | names magmux's socket, the tmux server, the session and the state directory, so two demos can run at once |
| `RC_DEMO_STATE` | `.task-grids/<id>` | where tokens, logs and `magmux.url` live |
| `RC_DEMO_KEEP` | — | `1` leaves `$STATE` behind for a post-mortem (tokens are still removed) |
| `RC_DEMO_COLS` / `RC_DEMO_ROWS` | this terminal (`tput`, or the launcher's own pane) | the size the layout is built at. Overriding it is how the automated checks pin their numbers |

Notes on the tmux side, all measured rather than assumed:

- In **own-server mode** the session lives on a **private tmux server**
  (`tmux -L magmux-<id>`), so the teardown can `kill-server` without touching
  anything of yours. In **adopt mode** none of that applies: the demo owns two
  panes and nothing else, and `kill-server` / `kill-session` /
  `destroy-unattached` appear nowhere on that path.
- Every tmux target is a **pane id** (`%0`), never `session:window.pane`.
  `base-index 1` / `pane-base-index 1` in a `~/.tmux.conf` is read even by a
  `-L` server, and with them set `$SESSION:0.0` is `can't find window: 0`.
- `destroy-unattached on` is set from a `client-attached` **hook**, not beside
  `new-session -d`. Set directly on a detached session it destroys the session
  and the whole server within about three seconds — measured on tmux 3.7c — so
  the demo would have died during its own startup and the corpse would have read
  as "magmux crashed". The hook keeps the property the option exists for (a
  `kill -9` of the launcher still cannot leave a session behind) and none of the
  damage.

## The layout, and the arithmetic in it

Stacked panes in one window rather than separate terminal windows. The demo's
whole point is watching an instruction reach both clients, and with separate
windows you are never looking at the mirror while it happens. Stacking also
gives every pane the same column count, which the ANSI painter needs.

**The thing being demonstrated gets the room.** The driver is a fixed, compact
14 rows — a two-column menu, its prompt and the last answer, with `h` for the
long form — and magmux gets everything else. It is a fixed FLOOR rather than a
share for exactly that reason: a driver that grew with the window would grow at
magmux's expense.

The sizes are ROWS, not percentages, because `split-window -l N%` is a
percentage **of the pane being split**, not of the window, and reading that
chain as a fraction of the window is how a 25% pane came out at 15%. With `R`
rows to share and one divider per split:

```
two panes — THE DEFAULT           three panes (RC_DEMO_MIRROR=1)
  driver  D = 14                    driver  D = 14
  magmux  A = R - 1 - D             mirror  M = A + 1
                                    magmux  A = (R - 2 - D - 1) / 2
  R = 50  →  magmux 35              spare row → the driver, so M = A + 1 stays exact

  smallest comfortable window       R = 50  →  magmux 16
  = 14 + 1 + 8 = 23 rows            smallest = 14 + 2 + 8 + 9 = 33 rows
```

**35 rows against 16 is why the mirror is opt-in.** The browser tab is already a
second, independent client — same `client.ts`, same `frame.ts`, a different
painter — and it shows the claim at a size a room can read. The tmux mirror
shows the same claim a second time in a third of the height, and every row of it
comes out of the subject. `RC_DEMO_MIRROR=1` is worth it when the DECODER is
what is under review: two surfaces, one wire, side by side, diffable by eye.

**A layout that cannot work is never built. The launcher degrades first, then
refuses.** Below 33 rows it drops the terminal mirror and builds the two-pane
layout, in one line that says so; below 23 it refuses outright, naming the rows
it has, the rows each layout needs and what to do about it. The refusal happens
**before the state directory, the static server and magmux**, so nothing has
been started and there is nothing to clean up.

That is magmux's own rule applied to its demo — *CREATION refuses; RESHAPE
clamps*. magmux needs `minPaneRows` (3) + its status row = **4 rows** and
`minPaneCols` (20) for a single pane and REFUSES anything smaller: it binds its
listener, writes its token, prints `cannot lay out 1 panes in 80x2` and exits,
taking the generated token file with it. A launcher that built such a layout
anyway turned a layout mistake into a missing-token report three steps
downstream — so no built layout is allowed to violate that floor, and the
arithmetic is asserted on the final numbers rather than on the inputs.

**A named SHAPE is built; a requested SURFACE is dropped.** `RC_DEMO_LAYOUT=panes`
names the three-pane geometry and is built on a short window regardless — an
explicit shape is not silently substituted — whereas `RC_DEMO_MIRROR=1` asks for
a surface, and on a window that cannot hold it the launcher says so and drops
it, because the browser tab is the second client either way and the alternative
is taking the rows out of magmux. For the mirror on a short window anyway:
`RC_DEMO_LAYOUT=panes RC_DEMO_MIRROR=1`, or move the floor with
`RC_DEMO_MIN_ROWS=N`. In the named case the launcher warns that magmux may
refuse the layout, and if magmux does, it stops and prints **magmux's own reason** rather than
letting the next process report a missing file.

**The mirror is one row taller than magmux, and that is not a rounding
accident.** It spends a row on its own status bar and magmux spends one on its
own, so equal panes leave the mirror one row short of the screen it is
mirroring — and it then clips and says so, in red, for the whole life of the
demo. That is why the spare row from an even split goes to the driver: handing
it to magmux instead would make the two the same height and bring the red
`CLIPPED` banner back on half of all window sizes.

**A `split-window -l N` is rows; a `-l N%` is a percentage OF THE PANE BEING
SPLIT, not of the window.** The sizes above are rows for exactly that reason —
reading the old 40%-of-62% chain as a fraction of the window is how a 25% pane
came out at 15%, and it is the driver that paid for it.

**A short window is answered by dropping the mirror, not by adding a window,
and the launcher does it for you.** `RC_DEMO_DRIVER=window` and
`RC_DEMO_LAYOUT=windows` still exist and are own-server only; neither is ever
chosen for you.

**In adopt mode the budget is the launcher's own pane, not the window.** You may
already have splits of your own, so what is being subdivided is `#{pane_height}`
and `#{pane_width}` of the pane you typed in — a real client's size, and a fact
rather than a guess.

### The exception: `RC_DEMO_GEOM`, for a terminal that is simply too small

Some terminals cannot be split into anything worth looking at: 80 columns is 80
columns however it is divided, and a browser shown an 80-column screen is
showing *your* terminal rather than magmux's.

```bash
RC_DEMO_GEOM=120x36 task demo:rc
```

magmux then runs **headless** at exactly that pane size — `--headless` is forced
when stdin is not a tty, and a headless run takes its geometry from
`COLUMNS`/`LINES` (CLAUDE.md, *Headless invariants*) — and gets **no tmux pane
at all**. The tmux panes are the driver and, under `RC_DEMO_MIRROR=1`, the
mirror, which will clip a screen bigger than its pane and says so in red in its
own status bar. The number names the PANE, so magmux is given one row more for
the status bar it reserves; the browser's chrome then reads `pane 0 · 120×36`,
which is what was asked for.

**It is never chosen for you, at any width.** A launcher that switched shape on
its own would take away the pane you type into and make a run irreproducible
from the command that started it.

**The trade is real: there is nothing to type into by hand.** No magmux pane
means no keyboard into the session, so the driver's menu (item 1) is the only
way to drive it. The banner says so, in yellow, every run.

Values outside `20..500` columns or `3..200` rows are **refused before anything
starts**, with the bounds named. That refusal is not pedantry: magmux does not
fail on an out-of-range `COLUMNS`/`LINES`, it uses 80x24 and says so on a stderr
this launcher has redirected into a file — so an unvalidated value would look
exactly like a demo that ignored you.

## The path not taken: `/v1/events` plus a ticket

Nothing here consumes `/v1/events`, magmux's server-sent-events stream, and
nothing mints a ticket. Both exist and both are tested (`test/rc/case1-http.ts`).

The reason is that a ticket solves a problem this demo does not have.
`POST /v1/tickets` mints a single-use 30-second credential so that a raw token
never has to appear in a **URL** — and the ticket mechanism's real consumer is
`EventSource`, which cannot set a request header at all. A WebSocket can carry
its credential in `Sec-WebSocket-Protocol` as `magmux.auth.<token>`, which puts
nothing in a URL and nothing in a proxy log, and magmux echoes back `magmux.v1`
and never the auth entry. And the page would still have to hold the token in
order to mint a ticket with it.

So: no ticket, and one transport rather than two. An SSE variant of the page
would exercise the one public surface this demo leaves untouched, which is worth
building the day somebody wants a viewer that survives a proxy that mangles
upgrades.

## Automated checks

```bash
task demo:rc:test        # bun demo/rc/selftest.ts
```

**This is the regression check and it is not the demo.** It prints a list of
pass lines and exits; `task demo:rc` is the thing to run to see the feature
work, and the driver menu is the thing to press keys in. The two are kept apart
on purpose — a check list that tried to be a demonstration would be a worse
version of both.

Costs nothing, needs no credentials, and there is no model anywhere in it. It
builds a real magmux, starts a real static server, and drives both over the real
wire. Fifteen groups:

1. `applyFrame` over a hand-built keyframe plus three deltas — text, spans,
   cursor, and the nine attribute bits pinned against `mux/cell.go:10-18`
2. double-width glyphs, cells ↔ code points, both directions
3. the WebSocket sequence against the binary: aggregate first, `hello` returns
   `readOnly: true`, the watch reply carries geometry, the keyframe covers every
   row, no frame precedes the reply that created it
4. **one pane, two credentials**: a full-token connection types `echo <marker>`
   and the read-only one sees it within 1s, latency printed
5. the view token's `input` and `send` are refused `forbidden`
6. **magmux is killed and the client says so** within 5s and exits non-zero
7. the static server: the 0600 token, the secret config path, the 503 before
   magmux binds, the 403 for a request with no `Sec-Fetch-Site`, and that
   `/app.js` is a real bundle of the shared source
8. `Origin`, as `--allow-origin` decides it — the exact flags `server.sh`
   passes, so a typo fails a check rather than a demo
8b. **the page's driver endpoint**: its own per-run path, the 403 without
   `Sec-Fetch-Site: same-origin`, the 404 on a guessed path, the panel built
   from the allow-list, a permitted action returning magmux's real status and
   body *and* the command appearing on the read-only stream, the VIEW-token
   action coming back as magmux's real 403, an action off the list refused by
   `serve.ts` itself, and `RC_DEMO_WEB_DRIVER=0` leaving no endpoint at all
9. **the launcher in OWN-SERVER mode, run for real** under
   `NO_BROWSER=1 NO_ATTACH=1 RC_DEMO_TMUX=own` and a per-run id: the ready
   banner, the URL file the browser page reads, *three* tmux panes matching the
   three the banner names, the colour card on the magmux pane AND on its mirror,
   the **driver** pane with its menu and both credentials named in its header,
   the ticket-runner plugin registered so item 10 has an op to call, the session
   surviving unattached, and a teardown that leaves no process, no tmux server
   and no `$STATE`. The mode is pinned because this suite is usually run from
   inside tmux, where the launcher would otherwise — correctly — adopt it.
10. **the launcher in ADOPT mode**, against a throwaway tmux server standing in
   for an operator's, with two windows of its own created first. It asserts that
   **no new window appeared**, that the launcher's own pane was split into three
   with the two new ones stamped `@rc_demo` and focus left alone, that magmux
   and the mirror both carry the colour-card marker, and that the driver came up
   on the launcher's own stdout with the session token accepted. Then it ends the
   run the documented way — `q` on the driver's stdin — and asserts the
   operator's pane is alone again at full height, **the bystander window is still
   there and the server is still running**, and nothing of the demo survives.
   That last one is the regression guard for the guest rule: without it, a future
   change could quietly start destroying somebody's work.
11-13. **the height budget**: a 30-row window asking for the mirror DEGRADES to
   two panes and still works; a 16-row window is REFUSED before the state
   directory, the static server, magmux or a tmux server exists; and a
   three-pane layout named by hand on a window too short for it is built anyway,
   magmux refuses the 3-row pane it gets, and the launcher prints **magmux's own
   reason** rather than the missing-token message three steps downstream
14. **the default proportions and the REAL size**, in BOTH tmux modes: on a
   200x50 window the panes come out magmux 35 / driver 14, and magmux's own
   `watch` reply — the message the browser sizes itself from — reports
   **200x34**, never 80x24. That is the `pane 0 · 80×8` screenshot, closed as a
   number
15. **`RC_DEMO_GEOM`**: an out-of-bounds value refused before anything starts,
   with the bounds named; and `120x36` from a 100x30 window producing **no
   magmux tmux pane at all**, a watch reply of 120x36, and a headless magmux
   that the launcher's own trap still kills

Both SKIP, with a reason, when tmux is not installed.

It deliberately does **not** live in `test/rc/`. That directory is the six
remote-control validation criteria and `run-all.ts` parses `__RC__ <n>` out of
them; a seventh entry would change what that suite claims to be.

## Files

| Path | Responsibility |
|---|---|
| `demo.sh` | sole entry point; preflight, ordering, readiness, tmux, browser, teardown |
| `stop.sh` | the escape hatch for a launcher that was killed hard; idempotent |
| `server.sh` | pane A's command: `exec ./magmux` with the flag set, stderr to a file |
| `welcome.sh` | the colour card, then `exec $SHELL -i` |
| `serve.ts` | mints the view token, `Bun.build`s the page, serves it, the driver endpoint's allow-list, ppid watchdog |
| `frame.ts` | the one decoder: screen model, `applyFrame`, `cellCols`, `rowSpans` |
| `client.ts` | the one core: auth, aggregate, `hello`/`watch`, liveness, end of run |
| `cli.ts` | ANSI painter; alt screen, absolute positioning, SGR, the size guard, the `[ ]` pane switcher |
| `driver/` | the driver TUI: a Go module of its own (Bubble Tea), every action one real request over the public HTTP API. `--run N` is the same action as plain text |
| `web/index.html`, `web/app.ts` | DOM painter, the fit-to-window terminal, status bar, pane picker, reconnect, computed 256-colour palette, the driver panel |
| `selftest.ts` | the automated checks; reuses `test/rc/harness.ts` |
