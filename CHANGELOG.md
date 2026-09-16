# Changelog

All notable changes to magmux are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Releases before v0.11.0 predate this file; their notes were generated from
commit subjects and remain on the
[GitHub releases page](https://github.com/MadAppGang/magmux/releases).

## [Unreleased]

### Added

- **Remote control: magmux over HTTP, WebSocket and server-sent events.**
  `--listen 127.0.0.1:7777` serves a small REST surface (`/v1/panes`,
  `/v1/ops`, `/v1/ops/{name}`, `/v1/panes/{n}/screen`, `/v1/capabilities`), an
  event stream at `/v1/events`, and a WebSocket at `/v1/ws` that carries ops,
  replies, events and — this part is new everywhere — **live pane frames**. A
  frame is a row-level diff with style runs and a cursor, so a browser can
  render a session without a terminal emulator; `mode:"notify"` sends bare
  change notices instead, for a client that would rather fetch the screen
  itself. Everything a local program could already do over the unix socket, a
  remote one can now do in the same words, because both are adapters onto one
  hub. Off unless you ask for it: without `--listen` no token is generated, no
  file is written and no port is opened.
- **A token that is always there, and never on a command line.**
  `MAGMUX_TOKEN`, `--token-file`, or magmux generates one into
  `{sock-dir}/magmux-{id}.token` at mode 0600 and removes it at exit. It is
  resolved and written *before* the listener binds, so an accepting port
  implies a finished token file — poll the port, then read the file, with no
  window in between. Comparison is constant-time over digests, and a token file
  that is a symlink, is not owned by you, or is readable by anyone else is
  refused rather than shrugged at.
- **A read-only credential.** `--view-token-file` grants `capabilities`,
  `list`, `ops`, `capture`, `transcript` and `watch`; everything that types,
  steers or decorates is 403. `--view-op plugin.op` opens one named exception
  at a time. For a dashboard on a wall or a screen-share, where the session
  should be visible without handing over the keyboard.
- **Single-use tickets**, minted by `POST /v1/tickets` and spent on the two
  endpoints a browser cannot put a header on — `/v1/events` and `/v1/ws`. Thirty
  seconds, one use, and it inherits the kind of the token that minted it, so it
  is never a way up. A WebSocket can also carry the token as the subprotocol
  `magmux.auth.<token>`, which magmux never echoes back.
- **TLS and browser controls.** `--tls-cert` / `--tls-key` for HTTPS and WSS,
  `--allow-origin` for CORS. A non-loopback bind without TLS prints a loud
  warning — the token and every keystroke would be in clear text, and a pane is
  a shell — but is not refused, because a VPN interface is a legitimate place to
  listen.
- **Plugins: `--plugin CMD`.** A plugin is a separate process that connects to
  magmux's socket and registers a name and some ops. Its ops then appear in the
  op table as `<plugin>.<op>` and *every* transport can call them — the socket's
  `call`, `POST /v1/ops/{name}`, a WebSocket, an MCP tool (`<plugin>__<op>`), a
  signed Firebase command — with no code in magmux that knows what the plugin
  does. A plugin can open a pane with `controller:"self"` and become that pane's
  controller, which is how something other than Claude Code can report a
  session's state. `examples/plugins/ticket-runner` is a complete worked
  example in TypeScript with no model, no network and no API key; `sdk.ts` and
  `client/pluginsdk.go` are the client libraries.
- **MCP: panes as resources, plugin ops as tools.** `magmux mcp` now publishes
  `magmux://{session}/pane/{n}/screen` and
  `magmux://{session}/plugin/{name}/events` as readable, subscribable
  resources — `resources/subscribe` yields `notifications/resources/updated`
  when a pane moves, whoever moved it — and turns every plugin op into a tool,
  announced with `notifications/tools/list_changed` when a plugin registers or
  dies.
- **A Firebase Realtime Database mirror: `--firebase config.json`.** Pane state,
  the op catalogue, an event ring and the screens at 2 fps, so a phone on a
  train can watch a build finish. Optionally the other direction too: a client
  can write a command back, defended twice over — the shipped security rules
  refuse a non-owner uid, and magmux verifies an HMAC with a key that is never
  in the database. A command runs at most once, because magmux writes a durable
  claim and waits for it to be acknowledged before any side effect.
  `examples/firebase/` has the rules, a config template, an emulator setup and
  the committed HMAC vector any signing client must reproduce.
- **`test/rc/*.ts`: six end-to-end validation cases** against a real binary over
  HTTP, WebSocket, SSE, the unix socket, `magmux mcp` on stdio and a Firebase
  emulator. Unlike `test/ui/*` they cost nothing and need no credentials —
  there is no model anywhere in them — and each prints the numbers it measured
  rather than a bare pass. `task test:rc`, or `task test:rc:full` to include the
  emulator (needs firebase-tools and a JDK 21+; also gated on
  `MAGMUX_FIREBASE_EMULATOR=1`).

### Changed

- **The repository is packages, and the install path changed with it.**
  `go install github.com/MadAppGang/magmux/cmd/magmux@latest` — the repo root is
  no longer a main package, and `@latest` without the suffix will not build.
  `brew install magmux` is unaffected. The ~8,350-line `main.go` is now
  `mux/` split by section, with the leaves (`pty`, `proc`, `sockdir`, `theme`),
  the MCP server (`mcp`), the hub, the transports, the plugin host and the two
  public packages (`protocol`, `client`) beside it. Import direction is enforced
  downward by a test, so nothing below `mux` can reach back into it — which is
  what lets the HTTP surface, the plugin host and the Firebase adapter each be
  tested against a bare hub with no terminal at all. No behaviour changed in the
  move; each step is verified by a mechanical content-equivalence proof under
  `test/reorg/`.
- **Every connection is now a hub subscriber with its own bounded queue and its
  own writer.** A client that stops reading fills its own queue and is
  disconnected; nobody else notices, and publishing never blocks. Teardown is
  bounded by one absolute deadline — backlogs are discarded and `results`,
  `shutdown`, EOF jump the queue — so the ordering every subscriber relies on
  holds whatever the backlog was.
- **Writes from one connection to one pane are now ordered.** Each (connection,
  pane) pair has a lane drained by a single goroutine, so two `send`s to one
  pane can no longer be typed into each other, and a lane outlives its
  connection: a one-shot client that pipes four lines into `nc -U` and hangs up
  gets all four delivered.

### Fixed

- **A rate-limited screen notification could be dropped for good.** A client
  watching a pane in `notify` mode is told at most once a second that the pane
  moved. If a change arrived inside that second and the pane then fell silent,
  the notice was discarded and never re-sent — the client sat on a screen it
  believed was current, with no way to discover otherwise. The framer now
  remembers a deferred offer and comes back for it. Found by the new
  `test/rc/case4-mcp.ts`, which types into a pane over HTTP and waits for the
  notification over MCP.

## [0.11.0] - 2026-09-09

### Added

- **magmux tells its children which background it resolved**, as
  `MAGMUX_THEME=light|dark`. A TUI child never needed it — it queries OSC 11
  and magmux answers from the same resolution — but a child that is not a TUI
  had no way to ask, and so had to hardcode one background's palette. The
  export is appended after the inherited environment, so magmux's own
  resolution beats a `MAGMUX_THEME` set in the shell; `--theme light` under a
  dark shell no longer tells children the opposite of what magmux is drawing.
- **A staged multi-agent demo** (`task demo`, `demo/`). Three agent panes
  driven by one controller, with the control panel open — free, deterministic,
  and needing no API key. The agents are staged, but each files a real Claude
  Code transcript, so magmux attaches a real controller to every pane and the
  panel's `◀ IN` rows remain what magmux itself observed.

### Changed

- **The control panel reads as badges rather than coloured text.** Route states
  are filled chips, the interleaved stream carries a direction-tinted rule that
  a request's acknowledgement inherits, and each exchange is headed by the
  controller's step tag on the left and the state magmux observed on the right.
  The two chips are coloured from different sources on purpose: the panel's
  provenance rule is now legible at a glance instead of on a careful read.
- **The pilot screen splits its messages.** Every entry is a chip plus a body
  block under a direction-tinted gutter, so an instruction sent and a turn
  observed are distinguishable without reading either.

### Fixed

- **The pilot was illegible on a light terminal.** Its palette was truecolor
  chosen for a dark background, and body text was a near-white lavender — so
  the most important content on the screen was the least readable thing on it.
  Body text now follows `MAGMUX_THEME`, and falls back to the terminal's own
  default foreground when it is unset. Badges stay truecolor deliberately: a
  chip paints its own ground, so the only contrast that matters is ink against
  chip.
- **The pilot blamed itself for provider failures.** `pi` records a refused
  request as a *completed* assistant message carrying `stopReason: "error"` —
  `prompt()` returns normally and throws nothing. Watching only text deltas
  missed it entirely, so a dead API key surfaced as three empty turns, two
  nudges, and the summary "the pilot stopped without calling finish". The
  provider's own error is now reported, and nudging stops once one is seen.

[0.11.0]: https://github.com/MadAppGang/magmux/releases/tag/v0.11.0
