# magmux - Development Notes

## Build

```bash
go build -o magmux ./cmd/magmux          # the binary
go test -race -count=1 ./...             # the suite; run it ALONE
task test:rc                             # the six remote-control criteria, bun
task test:rc:full                        # …including the Firebase emulator
```

`./cmd/magmux`, not `.`: the repo root is not a main package. The installable
path changed with it — `go install github.com/MadAppGang/magmux/cmd/magmux@latest`.

`test/rc/*.ts` is the end-to-end validation suite: bun cases that start a real
binary with a real listener and drive it over HTTP, WebSocket, server-sent
events, the unix socket, `magmux mcp` on stdio and a Firebase emulator. Unlike
`test/ui/*` they cost nothing and need no credentials — there is no model
anywhere in them — so they are safe to run on any change. Criterion 3 and the
Firebase quarter of criterion 5 are gated on `MAGMUX_FIREBASE_EMULATOR=1`: they
need firebase-tools and a **JDK 21+** (firebase-tools 15.19.1 refuses anything
older and exits BEFORE it binds the port, so an older JDK looks like an emulator
that never came up), and they download an emulator jar on first run. Without the
variable they skip and say so. The same gate guards
`transport/firebase`'s `TestEmulatorEndToEnd`.

## Architecture

Layout: `cmd/magmux/main.go` is a thin shim. Its first statement dispatches
`magmux mcp` to package `mcp` (`mcp.Run`); otherwise it is
`os.Exit(mux.Main(os.Args[1:]))`. `buildinfo/` holds `Version` / `Commit`,
which GoReleaser sets with `-X github.com/MadAppGang/magmux/buildinfo.Version=…`.
The leaf packages `pty/`, `proc/`, `sockdir/` and `theme/` sit below `mux`,
and the MCP server is package `mcp` in `mcp/`; each is an implementation detail
of magmux with no API stability before v1. The two PUBLIC packages are
`protocol/` — the socket's error codes (`protocol.Error`, `Errf`, `CodeOf`,
`Code*`), event names and version; `mux/sockrpc.go` keeps `sockErr`,
`sockErrf`, `verbErrCode` and `sockCode*` as aliases of them — and `client/`,
the Go socket client (`Session`, `Dial`, `RunInstruction`) that `mcp` drives
sessions through, formerly `mcp/mcp_client.go`. Everything else is package `mux` in
`mux/`, and `go test` runs each package with cwd = its own directory, so
`TestMain` builds `../cmd/magmux`. There is no `internal` directory, so the
compiler no longer stops a lower package importing `mux`:
`cmd/magmux/import_direction_test.go` (`TestImportDirection`) is that guard.
`test/reorg/r3.sed` is the rename map for the identifiers that changed name
when the leaves moved out; `test/reorg/r4.sed` is the same for `protocol` and
`client`.

The terminal core (formerly `mux/main.go`, ~8,350 lines) is split by section
into files in `mux/`; the tool-controller layer and the socket layer live
beside it. `test/reorg/r2-ranges.txt` maps every line of the old
`mux/main.go` to the file it moved to.

The core's sections, in the old file's order:

1. **Cell/Screen** — Cell struct (rune + Color + Attr), the viewport grid, and
   the scrollback ring behind it: `mux/cell.go`, `mux/screen.go`,
   `mux/scrollback.go` (the ring, and scroll mode)
2. **VT Parser** — DEC ANSI state machine (port of vtparser.c), handles CSI/ESC/OSC/C0: `mux/vt.go`
3. **Pane** — Binary tree layout node, owns PTY + Screen + VT parser: `mux/pane.go`
4. **PTY helpers** — Raw /dev/ptmx + ioctls (no CGo): package `pty`, platform bits in `pty/pty_darwin.go` / `pty/pty_linux.go`, `pty.SetWinSize` in `pty/pty.go`
5. **Renderer** — ANSI escape code output with dirty-flag optimization: `mux/render.go`
   (also `renderLoop`, `render`, `writeTerm` and `renderLocked`)
6. **Multiplexer** — Main event loop, input routing, mouse handling, SIGWINCH:
   `mux/mux.go`; the layout builders, grid-file parser and grid exit handling in
   `mux/grid.go`; socket IPC in `mux/socket.go`; `open_pane` / `close_pane` in
   `mux/dynpanes.go`; `Main` and flag parsing in `mux/cli.go`
7. **Selection** — Mouse drag text selection + clipboard copy (OSC 52 + pbcopy): `mux/selection.go`
   (also `parseSGRMouse`)
8. **Chrome** — the panel/status-bar toggles (`Ctrl-G p` / `Ctrl-G s`),
   `statusRowsLocked` / `reflowLocked`, and the status bar's panel digest:
   `mux/chrome.go`.
   magmux's default is to show none of itself: a lone session pane is a bare
   terminal, because `renderBorder` only ever paints a SPLIT node.

Socket lifecycle:

- `sockdir/sockdir.go` (package `sockdir`) — where the socket is bound (`--sock-dir` / `MAGMUX_SOCK_DIR`)
  and the startup sweep that removes pid-named sockets whose owner is provably
  dead. Free functions, no `*Magmux`, no locks.

Tool controllers:

- `mux/controller.go` — the `ToolController` interface and the `Snapshot` /
  `ControllerState` surface every controller produces, plus the optional
  `InputNotifier` interface.
- `mux/controller_claude.go` — observes a Claude Code pane by tailing its JSONL
  transcript under `~/.claude/projects/`.

Controlled sessions (an external AI agent steering a pane):

- `mux/pilot.go` — the `send` socket verb and PTY injection. The inbound half.
- `mux/control.go` — the control panel: the OUT/IN log of pilot↔session traffic,
  painted into a PTY-less pane.
- `pilot/pilot.ts` — the pi.dev agent that does the steering, with its
  toolbox replaced by `send_to_session` + `finish`.
- `pilot/magmux.ts` — its socket bridge.
- `test/ui/case3.ts` — the visual end-to-end case for the whole loop.

Remote control (a browser, an agent or a phone driving a session from off the
machine). The shape is hexagonal and the direction is strictly one way: `mux`
registers itself INTO the hub and the adapters, and nothing below `mux` has ever
heard of a `Pane`, a `Screen` or a `treeMu`. That is what
`cmd/magmux/import_direction_test.go` enforces, and it is why every one of these
packages can be tested against a bare hub with no terminal at all.

- `hub/` — the application port. `hub.go` is the op REGISTRY (name → spec + func,
  per source), `call.go` the call path and the watch verbs, `sub.go` one
  subscriber (bounded FIFO, one writer goroutine, the teardown cut), `lane.go`
  per-(Sub, pane) ordered delivery, `watch.go` the `Watcher` interface `mux`'s
  streamer implements.
- `mux/stream.go`, `mux/frame.go`, `mux/input.go` — the live screen: the
  readLoop hook, one framer goroutine per WATCHED pane, the row/run encoding,
  and the `input` op.
- `mux/ops_builtin.go`, `mux/sockrc.go`, `mux/sockconn.go` — the built-in ops as
  hub ops, the `ops`/`call`/`watch` verbs, and the socket connection as a Sub.
- `mux/remote.go` — the ONLY place `mux` and the transport packages meet:
  `--listen` becomes a token, a TLS pair and a bound listener, in that order.
- `auth/` — tokens (generate, load, constant-time check), the view token, the
  single-use tickets that keep a raw token out of a URL.
- `transport/ws/`, `transport/sse/` — the RFC 6455 codec and handshake, and the
  SSE reader/writer. Both pure and fuzzable.
- `transport/httpapi/` — the server: REST, `/v1/events` (SSE), `/v1/ws`, the
  auth/Origin/Host/CORS middleware, and the protocol-code → HTTP-status map.
- `transport/firebase/` — the Realtime Database mirror and the verified inbound
  commands.
- `plugin/` — the plugin host: spawn, registration over the ordinary socket,
  invoke routing, limits, death. `mux/controller_plugin.go` is the pane-side
  half, where a plugin becomes a pane's controller.
- `mcp/rc.go` — panes and plugin event rings as MCP resources, and plugin ops as
  dynamic MCP tools.
- `protocol/` — the PUBLIC wire types shared by all of them: errors and codes,
  event names, `OpSpec`, `Frame`/`Line`/`Run`, the `plugin.op` ↔ `plugin__op`
  name grammar.
- `client/` — the PUBLIC Go socket client, plus the plugin SDK.
- `examples/plugins/ticket-runner/` — the demo plugin (TypeScript, bun) and its
  SDK. `examples/firebase/` — the emulator config, the shipped security rules
  and the committed HMAC vector.
- `test/rc/*.ts` — the six end-to-end validation cases.

## Key Design Decisions

- **TERM=screen-256color** — Apps self-limit to supported escape sequences. This eliminates the need to handle Kitty keyboard protocol, xterm extensions, etc.
- **Dirty-flag rendering** — Only redraw when pane content changes. Idle panes cost zero CPU/IO.
- **Mouse: tmux model** — Click switches focus. Drag selects text (normal mode). Alt-screen apps get mouse forwarded.
- **No ncurses/tcell** — Raw ANSI output. Simpler, fewer dependencies, full control.
- **Color type** — Supports default (-1), indexed (0-255), and truecolor (RGB) in a single struct.

### Scrollback invariants

- **Only the PRIMARY screen records, and that decides the feature's reach.**
  `scrollUp` files an evicted row into `Screen.sb` only when `sbCap > 0 &&
  top == 0 && bot == s.rows`. The alternate screen is a separate `*Screen` built
  by `newAltScreen` with `sbCap = 0`, so an alt→primary round trip cannot touch
  the primary's ring — which is what stops `vim` flushing a shell's history, and
  is what every terminal does. Say it out loud when documenting this: **Claude
  Code runs in the alt screen**, so an agent pane has little or no scrollback
  and `read_pane transcript:N` stays the authoritative history for it.
  Scrollback is for shell panes — builds, test runs, dev servers. The
  `top == 0 && bot == s.rows` half is the same rule for DECSTBM: a child
  animating a region under its own header is redrawing a frame, not scrolling
  output off.

- **`scrollUp` is the hottest path in the parser and must not allocate.** The
  evicted row and the new bottom row are SWAPPED, as they always were; the only
  change is that the swap partner comes from `pushScrollback`, which hands back
  the row it is about to drop. Allocation happens only while the ring is filling
  (once per eviction, at most `sbCap` times) and when a recycled row is the
  wrong width after a resize. `TestScrollbackIsAllocationFreeOnceFull` pins it
  at zero.

- **Scrollback is CONTENT: `p.mu`, never `treeMu`.** `Pane.capture` /
  `captureAt` keep their "caller must NOT hold p.mu" contract — they take the
  lock, copy rows to strings, and do every join and cut outside it.
  `renderLocked` holds `treeMu.RLock` and takes `p.mu` for `sbOff`, which is the
  legal order.

- **`viewRow` is the ONE mapping from (offset, viewport row) to cells.** History
  and the viewport are one document — `sbRow(sbLen-1)` sits directly on top of
  `cells[0]` — and the renderer, `capture` and the scroll keys all read through
  it. A second copy of that arithmetic drifts the first time the ring wraps.
  Likewise there is one cell walk, `rowText` in capture.go, under both
  `rowsText` (selection) and `viewText` (capture).

- **A scrollback row keeps the width it was printed at; nothing reflows.**
  `Screen.resize` keeps the ring deliberately: a resize happens every time the
  human reveals the panel, and history that evaporates on a window nudge is
  history nobody trusts. Reflowing is not an option either — magmux does not
  record which rows were soft-wrapped continuations, so re-wrapping would invent
  line breaks the child never wrote. The consequence is that every reader must
  bound its walk by `len(row)`: `renderPane` pads short rows with blanks rather
  than stopping early, because the renderer never clears and a short row would
  leave the previous frame standing to its right.

- **Scroll mode has no flag; it IS `screen.sbOff > 0`.** One piece of state
  cannot disagree with itself, so scrolling back to live is leaving the mode,
  the badge and the key-swallowing turn on and off together, and there is no
  `m.scrollPane` to go stale when focus moves or a pane closes. Entry is an
  ACTION (`Ctrl-G [` scrolls back a page) rather than a toggle, because a mode
  entered with an unchanged screen looks like a key that did nothing. Entry must
  stay deliberate: while the mode is on, `consumeScrollKey` swallows everything,
  and arrows / PageUp / PageDown / the wheel must keep reaching a full-screen
  TUI in every other case.

- **`setAltScreen` is anchored on `primaryScreen`, not on the current screen.**
  `doCSI` resolves `s := vt.node.screen`, so on the way OUT of the alt screen
  `s` IS the alt screen and its own `.altScreen` is nil — the old `else if
  s.altScreen != nil` guard was never true and a pane that entered the alt
  screen never came back. Same root cause in `Pane.resize`, which resized
  `p.screen.altScreen` and so left the primary at the old size for the whole
  life of a full-screen app. Both were invisible while magmux's main tenant was
  Claude Code, which enters the alt screen at startup and leaves it by exiting.

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

### Dynamic-pane invariants (`treeMu`, pane ids)

- **`m.allPanes` is an append-only slot table and `Pane.id` IS the index.**
  Nothing renumbers, ever: `close_pane` sets `p.closed` and keeps the slot
  forever. The socket protocol's only addressing mode is an integer, so a
  compacting slice would make `send` to pane 1 quietly reach a different
  session the first time anything closed — no error, no log. Every int → pane
  conversion goes through `paneByIDLocked` / `livePanesLocked`; a surviving raw
  subscript of `allPanes` writes tint, overlay or keystrokes into a detached
  pane. Enforce with `grep -n 'm\.allPanes\[' mux/*.go`, which must hit nothing
  outside `paneByIDLocked`.

- **`treeMu` guards the layout; `p.mu` still guards content.** `treeMu` covers
  `m.root`, `m.allPanes` (header, elements, each element's `id`/`closed`/
  `label`), `m.focused`, `m.statusText`, `m.rows`/`m.cols`, `m.closeAt`, the
  package-level `sel`, and every `Pane`'s STRUCTURAL fields (`splitType, y, x,
  h, w, ratio, child1, child2, parent`). Content fields (`screen, dirty, dead,
  tint, inputReady, …`) stay under `p.mu`. Lock order:

  ```
  treeMu -> p.mu -> hub.mu -> sub.mu
  treeMu -> cp.mu ;  treeMu -> claimedMu
  ```

  **`hub.mu` and `sub.mu` (package `hub`) are LEAVES, and that is the whole of
  their rule.** `hub.mu` guards the op registry, the Sub set, the lane set and
  the shutdown flags; `sub.mu` guards one subscriber's queues. Neither is ever
  held across an `OpFunc`, a `Sink` call (a write to somebody's socket), a
  `Watcher` call or a plugin-host call, and no lock of magmux's is ever taken
  while either is held — `Hub.Call` copies the func out under `RLock` and calls
  it after the release, and `Hub.Session` hands the adapter back the Sub so the
  connect-time aggregate is built with no hub lock held. The single edge inside
  the package is `hub.mu -> sub.mu`. They replaced `sockClientsMu`, which was a
  leaf on the same terms and which one wedged subscriber could hold for 100ms
  per event per client.

  Three rules:
  1. **Never hold `treeMu` across blocking I/O** — `ptmx.Write`, `conn.Write`,
     `cmd.Start`, `os.Stdout.Write`, `controller.Poll`, execing `pbcopy`.
     Resolve the pointer under RLock, release, then do the I/O. **There is no
     exception**, and `controller.Poll` used to be documented as one: it is a
     transcript tail only *after* the transcript has been found, and until then
     it re-scans every directory under `~/.claude/projects` on every 250ms tick.
     `pollControllers` therefore snapshots the pane list under RLock and polls
     with the lock released.
  2. **`sync.RWMutex` is not reentrant.** A second `RLock` on one goroutine
     deadlocks if a writer queued between them, and the failure mode is a
     silent HANG, not a race report. Every function reachable from a site that
     already holds it needs a `…Locked()` twin — `allPanesDoneLocked` above
     all, because `renderLocked` holds RLock throughout.
     `TestConcurrentOpenCloseIsRaceFree` carries a goroutine-dump watchdog
     precisely because a bare test timeout names nothing.
  3. Never acquire `treeMu` while holding `p.mu`, `cp.mu`, `hub.mu`, `sub.mu`
     or `claimedMu`.

- **`renderLocked` builds a frame; `render` writes it.** The three slow things
  in a frame all live outside the lock, and each was a real stall:
  `controller.Poll` (filesystem, rule 1 above), the snapshot broadcast
  (`conn.Write` has a 100ms-per-client deadline, so one wedged subscriber
  stalls every writer), and the frame itself (`os.Stdout` on a full tty blocks
  for as long as it likes). `inputLoop` also reads `m.closeAt` under **RLock**
  and escalates to `Lock` only when the countdown is armed — it used to take
  the write lock once per keystroke to re-zero a zero value, and Go's RWMutex
  puts later readers behind a waiting writer, so that one keystroke cost the
  render loop a whole frame. Same reason `render` returns a `quit` flag instead
  of closing `m.quit` itself: closing it wakes the socket teardown, which
  broadcasts `results` and closes every subscriber, and a snapshot still queued
  at that moment is lost — which is exactly the final `awaiting_input` snapshot
  `-w` exists to deliver (issue #2). `TestRenderWritesTerminalWithTreeMuReleased`
  and `TestControllerPollRunsWithTreeMuReleased` pin this by measuring how long
  a `treeMu.Lock` waits while a deliberately slow writer/poll is in flight.

- **`waitForChild` is the only caller of `cmd.Wait`, so it runs for every
  child.** `readLoop` sets `p.dead` when the PTY closes, which looks like
  reaping and is not — the process entry survives until it is waited on, and
  `p.reaped` stays false, which is the flag that stops `reapPane`'s delayed
  `SIGKILL` landing on a pid the OS has since recycled. Only the PRESENTATION
  (the ✓ DONE / ✗ FAIL tombstone and the `exit` event) is grid mode's, and it
  is gated inside `waitForChild` on `p.gridMode`. `OpenPane`'s two unwind paths
  fork a child that is never published — no `wg.Add`, no `readLoop`, no waiter
  — so they go through `unwindPane`, which reaps AND waits.

- **A split allocates a FRESH internal node; the leaf is never converted.** The
  leaf owns `screen`, `ptmx`, `cmd` and `vt.node == p`, and is pointed at from
  outside the tree by `ClaudeCodeController.pane`, `ControlPanel.pane`,
  `sel.pane` and `m.claimedSessions`. Converting it in place strands all of
  them: the pane still paints and never updates again.

- **`close_pane` must release the transcript claim.** `m.claimedSessions` maps
  transcript → `*Pane` and is cleaned nowhere else; a stranded entry leaves the
  next pane in the same project stuck in `starting` silently and forever.

- **Three states, not two: `dead` ≠ `closed` ≠ `hidden`.** `dead` = the process
  is gone but the pane is still on screen with its ✓ DONE / ✗ FAIL overlay — a
  self-exit never auto-collapses, because in grid mode the finished grid IS the
  report. `closed` = the pane is gone and its id slot is a tombstone; it is
  permanent. `hidden` = the pane is **alive and complete** — every byte of its
  history, its id, its entry in `results` — and merely not spliced into the
  tree, so it occupies no columns and the other panes reflow over it. Only the
  control panel is ever hidden (`Ctrl-G p`), and it starts that way unless `-c`
  asked otherwise.

  The distinction has teeth in three places. (1) Hiding must never set
  `closed`: a tombstone cannot be undone, and `Ctrl-G p` would be a one-way
  door that silently destroyed the ledger. (2) `results` must keep saying
  `state:"panel"` for a hidden panel — hidden is a fact about magmux's chrome,
  not about a session, so it rides as its own `hidden` field;
  `test/ui/case3.ts` asserts the state. (3) Every loop that reads GEOMETRY or
  PAINTS must skip `p.hidden`, because a hidden pane keeps the y/x/h/w it had
  when it left the tree: `largestLiveLeafLocked` and `resolveSplitTargetLocked`
  would nominate it as a split target and land an agent's `open_pane` in a
  subtree nothing paints, the render sweep would order a full repaint every
  second for a pane nobody can see, and `focusNext` would park the keyboard on
  it. `allPanesDone` and `buildPaneResults` deliberately do NOT skip it — the
  first already skips `isControl`, and the second is the whole point.

- **Hiding the panel is `removeLeafLocked`; showing it is `splitNodeLocked`
  against a REMEMBERED anchor.** `removeLeafLocked` collapses the panel's
  parent into its sibling and hands the sibling the parent's exact geometry, so
  the inverse is to split that same sibling again with the same type, ratio and
  side. `m.panelAnchor` is therefore the sibling — a POINTER, because the
  sibling is usually an internal node and internal nodes have no id — and
  `showPanelLocked` re-verifies it is still reachable from `m.root`
  (`nodeInTreeLocked`) before using it, falling back to the root when an agent
  closed the pane underneath it. Anchoring to the root unconditionally would
  look right for one or two panes and put the panel in the wrong place for
  four, where the builders nest it inside the right-hand column.
  `panelFirst` is stated as a negative so the zero value (child2, the
  right-hand column) is what every layout builder produces.

- **A refused show must refuse, not clamp.** `reshapeChildren` clamps at zero
  rather than going negative, so showing the panel on a terminal too narrow for
  two usable halves would not crash — it would produce a panel with no columns
  in it, and the session would have lost the space for nothing. `splitFits`
  uses the same floor and the same arithmetic as `OpenPane`, deliberately:
  "usable" cannot mean one thing for an agent's `open_pane` and another for the
  panel. The refusal is said in the status bar (`chromeNote`), because a
  keystroke that appears to do nothing is indistinguishable from a broken one.

- **`--no-idle-done` withdraws a CLAIM, it does not change an OBSERVATION.**
  `inputReady` is still set, and `snapshot` / `results` / the controller state a
  pilot waits on are byte-identical with the flag on. What it suppresses is
  magmux asserting the session finished: the ✓ DONE overlay and green tint (both
  copies — `renderLocked`'s sweep and `applyControllerSnapshot`'s), the grid
  counter's `done` column, and `-w`, which then waits for the process to exit.
  Touching `inputReady` itself would break the rule that the live snapshot and
  the shutdown `results` can never disagree, for the sake of a display flag.
  Input is deliberately NOT on the list: a keystroke reaches an idle pane with
  or without the flag, which is why the flag is a comfort and not the fix for
  issue #333. The flag reaches `-w` as a PARAMETER of `paneDoneLocked`, never
  as a second predicate beside it: "done" having two definitions is the defect
  that function exists to close, and re-opening it here would put the
  exit-status proof on one path and not the other.

- **Chrome flags are stated as NEGATIVES (`hideStatus`, `noIdleDone`, `hidden`,
  `panelFirst`).** Every unit test builds a `Magmux` as a struct literal, so
  the zero value has to be the behaviour that predates the flag — a status row
  reserved by `buildGrid`, a panel on the right. `statusRowsLocked` is the one
  place the row is counted, and `reflowLocked` is the one place the tree is
  resized to the terminal; both toggles and SIGWINCH go through them, which is
  what stops the three copies of `statusH := 1` drifting apart.

- **Showing the panel must not steal focus; hiding it must not strand focus.**
  A human is typing into their agent, and revealing an instrument that takes
  the keyboard mid-sentence is worse than not having the instrument — the same
  reason `main()` deliberately moves focus off the panel at startup. The
  reverse case is not symmetric: focus left on a hidden pane sends every
  keystroke somewhere nobody can see, so `hidePanelLocked` moves it to
  `firstLiveLeaf`, which prefers a real session.

- **`reshapeChildren` clamps.** `w2 = p.w - w1 - 1` has no natural floor;
  three splits deep on 80 columns, or one SIGWINCH shrinking a tree that was
  legal when built, goes negative. Clamp there, not in `OpenPane` — creation is
  not the only moment geometry changes.

- **CREATION refuses; RESHAPE clamps. `buildGrid`/`buildColumn` are creation.**
  `buildColumn`'s `botH := h - topH - 1` and `buildGrid`'s
  `w2 := m.cols - w1 - 1` had no floor either, and at the headless 80×24 that
  is reachable from the command line: ~32 `-e` panes give a zero-height pane
  and ~64 a negative one. A zero-row `Screen` **captures as empty**, so a
  scenario reports no output rather than an error — the same
  misattributed-diagnostics shape as the exit-status race below. The floor is
  `minPaneRows`/`minPaneCols`, the SAME constants `OpenPane` and `splitFits`
  use and **no second copy**: "usable" cannot mean one thing for an agent's
  `open_pane`, another for `Ctrl-G p`, and a third for `-e`. The refusal
  happens before `newPaneFor`, so no child is spawned for a layout already
  known to be impossible, and `errPanesDontFit` names the count AND the
  geometry, because "no room" without either does not tell a caller whether to
  drop a pane or find a bigger window. `buildLayout` (the bare `magmux`
  layout) stays exempt: magmux chose that pane count itself, so there is no
  caller to have made a mistake. Practical consequence, and it is a real
  behaviour change for interactive users: **13+ `-e` panes on 80×24, and 2+ on
  a 40-column terminal, now exit 1 with a message.** The count that fits is
  whatever `topH := h*topN/len(cmds)` produces, so the tests assert the
  PROPERTY — *`buildGrid` either errors, or every leaf is at least 3×20* — and
  never a table of thresholds.

- **`layoutSpec` exists because the base case cannot name what the caller
  asked for.** `buildColumn`'s `len(cmds)==1` branch knows only its own
  one-element slice and its own box, so left to itself it reports "cannot lay
  out 1 panes in 40x1" for a 40-pane request — true of the recursion, useless
  to the human. The count and the terminal ride down from `buildGrid`.

### Headless invariants (`--headless`, and stdin that is not a tty)

- **`headless` is written in `init()` and nowhere else, and only ever set.**
  `--headless` forces it on the struct literal; `init()` also turns it on when
  `!term.IsTerminal(m.stdinFile().Fd())`, which is what makes
  `magmux -w -e CMD < /dev/null` work with no flag (it used to exit 1 with
  "raw mode: operation not supported by device"). Resolving it in `init()`
  rather than `main()` is what makes the mode reachable from a test that never
  spawns a process: `&Magmux{stdin: pipeReader}` + `init()` degrades. It is
  stated as a POSITIVE that is FALSE by default for the same reason
  `hideStatus`/`panelFirst` are negatives — every unit test builds a `Magmux`
  as a struct literal, so the zero value must be today's behaviour, and an
  `interactive bool` would invert that and silently make every literal
  headless.

- **`renderLoop` MUST keep running headless. Only `writeTerm` is suppressed.**
  Not starting the render loop is the obvious fix and it is the wrong one: it
  kills `pollControllers`, the snapshot broadcast and `-w`'s auto-exit at once
  — `renderLocked` is where `quit` is computed. The frame is BUILT and thrown
  away. `writeTerm` is the single funnel and the guard is unconditional,
  including when `m.out` is set: "headless" means "emits no frame", one rule.
  Guarding the callee rather than `render()`'s `if out != ""` is deliberate, so
  a future caller cannot bypass it.

- **Zero bytes on stdout is closed by ENUMERATION, not by assertion.** There
  are exactly four writers in the core (the files split from `mux/main.go`) and
  the fourth is unreachable: the alt-screen sequence in `init()` (inside the
  tty branch) and `restore()`'s disable sequence (early return), both in
  `mux/mux.go`; `writeTerm` (early return) in `mux/render.go`; and
  `putClipboard`'s OSC 52 in `mux/selection.go` — reached only from `parseSGRMouse` ← `tryParseEscape`
  ← `inputLoop`, which does not run headless. **It gets no guard on purpose**:
  one would imply the path is live and invite someone to make it so. The fifth
  site is not on stdout at all — `detectThemeColor` writes its OSC 11 query to
  **FD 0**, which `!term.IsTerminal(fd)` does NOT cover under a `--headless`
  forced from a real tty, so `initTheme`'s guard carries `m.headless ||`
  explicitly. `initTheme` itself still runs: `--theme`, `MAGMUX_THEME`,
  `TERM_THEME` and `COLORFGBG` still choose a palette (in that order, first
  answer wins, `auto` is no opinion at every level), and the palette is what
  children are told about the background. The guard is hoisted OUT of the
  probe closure: a skipped probe is a nil callback to `resolveTheme`, which is
  what lets the walk reach `COLORFGBG` headless instead of stopping at dark.
  `TERM_THEME` and `COLORFGBG` are read through `themeEnv` with `os.Getenv`
  only — no file is ever opened for them.

- **A child is TOLD the resolved theme, as `MAGMUX_THEME=light|dark`.** A TUI
  does not need it: it queries OSC 11 and `answerColorQuery` replies from the
  same resolution. A child that is not a TUI cannot ask, and `pilot/pilot.ts`
  is the case that proved it — a plain script writing ANSI, which hardcoded one
  background's palette (`text` was `rgb(205,214,244)`, a near-white lavender)
  and drew every body line illegibly on the other. The export is appended
  AFTER `os.Environ()` and `os/exec` keeps the last occurrence of a duplicated
  key, so magmux's own resolution beats a `MAGMUX_THEME` inherited from the
  shell — otherwise `--theme light` under a dark shell tells every child the
  opposite of what magmux is drawing. `cfg.Env` still goes last and still
  overrides. A NESTED magmux inherits it, and `MAGMUX_THEME` sits second in
  the chain, above the probe: the inner magmux is looking at a PTY, so the
  outer one's reading is better evidence than anything it can probe.
  `TestChildIsToldTheResolvedTheme` pins all four cases.

- **Headless blocks on `<-mux.quit`, never on `inputLoop`.** `inputLoop`'s
  stdin goroutine closes `stdinCh` on the first read error, and `/dev/null` is
  EOF on the first read — so `inputLoop` would return in microseconds, `main`
  would fall through to `waitSocketShutdown`, and a `-w` run would exit before
  a single pane had produced output.

- **`-w` needs a SESSION, and the two shapes where it never fires are
  documented rather than fixed.** `allPanesDoneLocked` ends `return sessions > 0`
  after skipping `p.isControl`, so `magmux --headless -w` with no `-e`/`-g`
  (`gridMode` false) and `magmux --headless -w -c` with no `-e` (`sessions == 0`)
  both run until signalled. There is no `quit` socket verb. That is a
  legitimate mode — a controller-driven session should not exit on its own —
  but it is indistinguishable from a hang, so it is in `--help` and in the
  README, and `kill -TERM` is the way out.

- **`handleSIGWINCH` returns before `signal.Notify` headless.** A pipe-stdin
  process is never sent SIGWINCH, but a FORCED `--headless` on a real tty can
  be, and it is still ignored on purpose: headless geometry is synthetic and
  stable, and reflowing panes under a controller because a human dragged a
  window is a surprise nobody asked for.

- **Geometry is `COLUMNS`/`LINES`, else 80×24, and the bounds are not
  paranoia.** `newScreen(h, w)` allocates `h*w` Cells, so `COLUMNS=99999999` is
  an OOM with a stack trace instead of an error, and `COLUMNS=0` gives every
  pane a zero-width screen whose every capture is empty with no error anywhere.
  Both arrive from the environment, the input class nobody validates. Reading
  them first has a deliberate consequence: magmux exports exactly those two to
  its own children, so a headless magmux started INSIDE a magmux pane inherits
  that pane's size. That is the right answer and it is surprising, hence tested.

- **The pty positive control is mandatory and is not optional scaffolding.**
  If auto-degrade ever misfires, every end-to-end test in this repo keeps
  passing against a magmux that never paints a byte — they all assert on the
  socket. `TestPTYRunStillPaintsStdout` is the one test that fails. It must
  keep DRAINING the pty master for the whole run, too: stopping once the
  assertion is satisfied fills the pty buffer, blocks magmux in
  `os.Stdout.WriteString`, and SIGTERM cannot then get it past `restore()`.

### The `-w` exit-status race (`dead` ≠ `reaped`)

- **`p.dead` has two independent writers and only ONE of them knows the exit
  status.** `readLoop` sets `dead` alone when the PTY closes; `reapChild` sets
  `dead`, `reaped` and `exitCode` under a SINGLE `p.mu` acquisition when
  `cmd.Wait` returns, on a different goroutine. `-w`'s gate used to test
  `!p.dead && !p.inputReady`, which `readLoop`'s write satisfies on its own —
  so `magmux -w -e 'exit 7'` could broadcast `{"state":"completed","exitCode":0}`
  and close every subscriber before the real status existed anywhere. A run
  that FAILED, recorded as having PASSED. Headless is what makes it critical:
  there is no ✗ FAIL tombstone and no human, so `results` is the only report.

- **`paneDoneLocked` is the ONE definition of "finished", and `reaped` gates
  the DEAD branch only.** `inputReady` still counts on its own, or `-w` would
  never fire for a Claude Code pane — an idle agent never exits, it finished
  its TURN. The fix is read-side ONLY: `reaped == true` already IMPLIES
  `exitCode` is final, because of that single-lock write, so nothing about
  `waitForChild` or the write ordering changes.

- **`Pane.deadAt`'s zero value is what makes the fix safe.** `time.Since(zero
  time)` is centuries, so `reapGrace` expires instantly and every existing
  `Pane{dead:true}` literal is still "done", exactly as today. A test that
  wants to exercise the race must set `deadAt: time.Now()` explicitly. Both
  writers stamp it under `p.mu` guarded by `IsZero()`, so it is the moment the
  pane FIRST went dead and neither writer can move it. `reapGrace` (2s) bounds
  the wait so a `cmd.Wait` that never returns — a SIGSTOPped child, or a
  grandchild holding the PTY — cannot convert "wrong answer" into "no answer",
  which would be a worse trade. A pane with `p.cmd == nil` is done immediately:
  `reapChild` returns early on one, so `reaped` never becomes true.

- **`buildPaneResultsLocked` needs the same honesty, separately.** Even with
  `-w` fixed, the connect-time aggregate can still be built in the window
  between the PTY closing and `cmd.Wait` returning. `p.dead && !p.reaped &&
  p.cmd != nil` reports `"running"` — the state it had an instant ago — rather
  than inventing a success. `exitCode` and `dead` keep their keys: removing one
  is a schema change, and a consumer keying on `state` never reads `exitCode`
  for a `running` pane.

### Controlled-session invariants

- **"Done" answers two different questions, and `dead` is the answer to only
  one of them.** For `-w` and auto-exit, done means "nothing left to wait for",
  and `allPanesDone` asks `paneDoneLocked` — the one definition, where an idle
  turn counts unless `--no-idle-done` withdrew the claim, and a DEAD pane counts
  only once `reaped` proves its exit status. For anything that decides
  whether a KEY reaches a child, done means "there is nothing to type into",
  which is `allPanesDead` — a pane that is merely idle is a live agent between
  turns, blocked on a read. Three sites take the second predicate:
  `writePTY`'s grid guard, `inputLoop`'s bare-key quit branch, and
  `keyHintLocked`. All three used the first, and together they made an attached
  session read-only the moment its agent went quiet (issue #333): the input
  loop swallowed every plain key because ONE idle pane satisfied
  `allPanesDone`, `writePTY` would have dropped it anyway, and the chord hints
  blanked so the bar denied the keyboard existed. A program could steer the
  pane over the socket the whole time. Keep the two predicates apart, and keep
  the bar's advertised quit key on the same predicate as the input loop —
  telling a person to press `q` at a live agent puts a stray `q` in its prompt.

- **`send` and a keystroke are the same act, so they share
  `clearCompletionLocked`.** `injectPTY` and `writePTY` refuse the same thing
  (a pane with no live child) and clear the same completion state — `tint`,
  overlay, `inputReady`, `inputSignal`, `hadTextOutput`, and both idle clocks
  (`lastTextAt`, `titleIdleAt`). They drifted once: `writePTY` left the clocks
  standing, so the text-idle sweep re-fired on output that was already on
  screen and re-settled the pane a frame later. `hadTextOutput = false` is the
  load-bearing half, because `renderLocked`'s 5s rule needs NEW output before
  it can call the pane idle again. The one thing that must never take either
  path is magmux answering a child's own query — see `replyLocked`.

- **The panel's two directions come from different places, and must stay that
  way.** `▶ OUT` rows are recorded from the pilot's own `send`; `◀ IN` rows are
  recorded only from `pollControllers`, i.e. from what magmux itself observed
  the pane do. A pilot can therefore never fabricate a completion, and the
  panel can show the session disagreeing with what the pilot believes it
  asked for. Relatedly, an IN row only counts when an instruction is
  outstanding (`sent > observed`) — a session is idle from the moment it
  boots, and counting that as a completed turn showed `done 1` against zero
  instructions and drove the progress meter on a run that had not started.

- **`-c` means "start the panel VISIBLE", not "add a panel".** Every session
  gets a panel now; the flag only chooses whether it is on screen at boot. That
  is what keeps `task pilot:demo`, `task mcp:demo`, `test/ui/case3.ts` and
  `case4.ts` looking exactly as they did — they all pass `-c`. The two paths
  build it differently on purpose: `-c` appends a `PaneConfig{Control:true}` to
  the layout builders, exactly as before, so the visible layout is unchanged
  byte for byte; without `-c`, `installHiddenPanel` builds the session layout
  untouched and adds the panel to the id table afterwards, so a hidden-panel
  magmux is byte-identical to one with no panel at all. Handing the builders an
  extra command instead would change the shape they build —
  `buildLayout`'s 3+ branch only ever builds three panes and would drop it
  silently. The visible consequence is that the panel now holds an id in every
  session, so the first pane an agent opens is no longer necessarily 1; ids
  were always documented as sparse and never to be assumed, and
  `TestOpenPaneOverSocket` now asks for the next free slot rather than
  hardcoding it.

- **The control pane has no process, so every "every pane" loop must skip it.**
  It is never dead and never goes idle, so `allPanesDone` counting it means
  `-w` can never fire; `startReadLoops` and `waitForChild` would dereference a
  nil PTY and nil `cmd`. It also has no child to redraw it on SIGWINCH, so the
  resize path must repaint it explicitly or it comes back blank.

### Panel-as-wire-tap invariants (routing, replies)

- **The panel is a wire tap on ONE exchange, and is not a participant in it.**
  The exchange is between the **controller** (whatever is driving) and the
  **controlled agents** (the panes). An MCP client is not a third party: it is
  just another controller, reaching magmux through `magmux mcp` translating
  `tools/call` into the same socket verbs anything else would send, and that
  process hop is plumbing. So there is **no third row class and no
  self-reporting** — the panel never records something magmux did on its own
  behalf. Two directions only: `▶ OUT` is the controller's request as it
  arrived on the socket, `◀ IN` is magmux's own observation via
  `pollControllers`. The moment the panel reports on itself, "who closed pane
  2" becomes a question it has to answer about itself, and the provenance model
  the whole instrument rests on is gone.

- **`observed` and `ctrlStep.state` are written in exactly ONE function,
  `recordObserved`.** An ack/reply from magmux renders inline on the OUT row it
  answers (the indented `⇦`) and must NEVER close a turn — a request magmux
  accepted is not a turn the session completed, and letting the ack close it
  makes `done` count magmux's own bookkeeping. This is a grep-able invariant:

  ```
  $ grep -n 'observed *=\|observed++\|steplog\[i\].state = ' mux/control.go
  ```

  must hit `recordObserved` and the run-zeroing in `recordStart`, and nothing
  else.

- **Outstanding is evaluated PER ROUTE, never globally.** `r.sent > r.observed`
  on the route, not `cp.sent > cp.observed` across the panel. A global counter
  lets one pane's boot-time idle close a different pane's outstanding step —
  that is the "done 1 against zero instructions" bug above, resurrected at N
  panes and much harder to see. A pane with no route is ignored entirely, so
  panes the controller never touched cannot flood the stream.

- **The status-bar digest is the panel read through a keyhole, and invents no
  third number.** With the panel hidden by default, the bar is the only place a
  controlled run announces itself, so it carries the panel's own `sent` /
  `observed` under the same provenance rule — `▶` is what the controller asked
  for, `◀` is what magmux observed. Reconciling them into a single "progress"
  figure would be a second, quieter provenance model beside the panel's. It
  reads through `ControlPanel.digest()`, the same lock-free value-copy pattern
  as `snapshotLocked`: `renderLocked` already holds `treeMu.RLock`, the order
  `treeMu -> cp.mu` is legal, and the reverse never is — so `cp.mu` is taken
  and released inside `digest()` and never held across rendering. The digest
  degrades signal → state → counters and is bounded by `m.cols`, counters
  included: a bar that overruns wraps onto the pane above it, and corrupting a
  session's output to announce magmux is the exact opposite of the point.

- **Replies are opt-in per message and unicast.** Only a message carrying an
  `id` gets one, and it goes only to the connection that sent it — never
  broadcast, and **never recorded into `m.finalEvents`**. `finalEvents` is
  replayed to a client that connects during teardown, so a reply in there would
  land after `results` and break the `results` → `shutdown` → EOF ordering every
  subscriber relies on.

- **`exit` has no delivery guarantee; `results` does.** `exit` is a live
  broadcast from `waitForChild`'s own goroutine and is never replayed to a
  connection. Under `-w` that goroutine races the teardown `-w` triggers: the
  read loop sets `dead`, the render loop closes `m.quit`, and the socket server
  can broadcast `results` and close every subscriber before `cmd.Wait` has
  returned — so the `exit` is simply gone. A test that reads its answer out of
  an `exit` event under `-w` therefore flakes at a few percent against a magmux
  that works. Either assert on `results`, or drop `-w` and quit deliberately
  with the Ctrl-G `q` chord once the event has landed
  (`TestSocketIDFlagBindsNamedSocket` and `TestSocketReaderAcceptsLargeLine` do
  the latter). This has now been diagnosed twice; it should not be a third time.

### Hub invariants (`hub/`: the registry, the bus, the lanes, the teardown)

- **`hub` must never import `mux`, and that is the whole architecture in one
  sentence.** The hub is the application port: it knows an op name, a spec and a
  func, and it knows how to turn bytes into somebody's connection. It does not
  know what a pane IS. `mux` registers its built-ins INTO it
  (`mux/ops_builtin.go`) and installs its streamer as the `Watcher`; the socket,
  HTTP, WebSocket, SSE and Firebase are driving adapters that hand it a `Caller`
  and a `Sink`. Break the direction and every one of those packages becomes
  untestable without a terminal, which is exactly what they were split out to
  avoid. `cmd/magmux/import_direction_test.go` is the guard, and it has a
  negative control: adding a deliberate `mux` import to `hub` must fail it.

- **`hub.mu` is a LEAF, and the only order inside the package is
  `hub.mu -> sub.mu`.** It is never held across an `OpFunc`, a `Sink` call, or
  anything else that can block, and no lock of `mux`'s is ever taken while it is
  held. A registry lock held across a plugin's op — which is a round trip to
  another process — would stop every other transport for the duration.

- **Registration and unregistration are per SOURCE, never per op.**
  `Register(source, ops…)` / `UnregisterSource(source)` with `"magmux"` for the
  built-ins and the plugin's name otherwise. A plugin that dies takes exactly
  its own ops with it, and `Rev` changes so a client can tell that the op list
  it cached is stale — which is what `ops_changed` carries and what makes MCP's
  `notifications/tools/list_changed` possible at all.

- **`Publish` never blocks, and that is the bus's entire contract.** It takes
  already-marshalled bytes and hands the SAME slice to every live `Sub`. Each
  `Sub` owns a bounded FIFO (`fifoMaxMsgs` 1024, `fifoMaxBytes` 8 MiB) and one
  writer goroutine, so a subscriber that stops reading fills its own queue and
  is closed with `slow_consumer` — alone. If publishing could block, one laptop
  lid would stop magmux for everybody, and the symptom would be no error
  anywhere. `TestPublishNeverBlocksOnAStalledSink` and
  `TestOverflowClosesOnlyThatSubscriber` drive the FIFO directly — filling a
  kernel socket buffer on loopback can take megabytes, so that is where the
  shedding rule is actually proven — and `test/rc/case6-slow-client.ts` stages
  the same failure on a real stack, where what it can show is that the stall was
  CONTAINED.

- **The connect-time aggregate is the `head`, and it is NOT in the FIFO.** Every
  subscriber's first message is the aggregate, on every transport, and it cannot
  be discarded — a `Finalize` landing between registering a `Sub` and starting
  it must still let it through first (`TestFinalizeReplayOrder`,
  `TestSSEDeliversTheAggregateFirst`). The property it buys is that there is no
  window in which an event is published, missed by the aggregate and missed by
  the stream: `handleSocketConn` used to build the aggregate BEFORE it
  registered, and anything published in between was simply lost.

- **A lane OUTLIVES its connection.** A `send` is not one write — it is text,
  then a key every 20 ms, then a pause, then Enter — so a one-shot client
  (README's `nc -U`) reaches EOF while its second instruction is still queued
  behind the first one's pacing. On close a lane accepts nothing new and DRAINS
  what it holds. Dropping the queue instead loses an instruction silently, on
  exactly the client shape most likely to hit it (`TestOneShotSendSurvivesClose`).

- **`Quiesce` is the only thing that discards a queued item unrun, and it says
  so.** It calls `Discard` rather than dropping the item, because the panel has
  already shown that request as an OUT row and a request that vanishes between
  the panel and the pane is the one thing the panel exists to make impossible.
  The item's `ctx` is cancelled by `Quiesce` and by nothing else: a caller's own
  timeout ends that CALLER'S WAIT and never reorders or truncates a lane.

- **The torn-write rule is why `Sink.Write` returns an `n` at all.** `n > 0`
  means bytes may have left the process, so the stream is mid-line and NOTHING
  may follow it — the subscriber gets EOF without its finals, because splicing
  `results` into a half-written line is worse than not sending it. `n == 0`
  leaves the stream line-aligned and the finals can still be written. Only a
  sink over a plain `net.Conn` can honestly report `n == 0`; a TLS or buffered
  sink (`wsSink` under TLS, `sseSink` always) must report every failure as torn,
  because after a `tls.Conn` write times out the TLS state is corrupt and a
  `ResponseWriter.Write` counts bytes buffered rather than bytes sent.
  `TestFinalizeTornWriteRealSocket` and `TestFinalizeTornWriteSSE` pin both
  halves.

- **`Finalize` bounds the whole teardown with ONE absolute deadline.** The
  message already in flight gets `finalCut` (500 ms); everything after it — the
  pending aggregate, then `results`, then `shutdown` — shares `finalDeadline`
  (2 s) measured from the moment `Finalize` started, so the worst case is ~2.5 s
  from `m.quit` whatever the backlog was. Backlogs are DISCARDED and the finals
  jump the queue: a subscriber 1,024 events behind does not need those events,
  it needs the answer. A `Session` opened after `Finalize` gets its aggregate and
  the finals on its own `lateDeadline` clock, because the teardown it missed is
  already over.

### Streaming invariants (`mux/stream.go`, `mux/frame.go`)

- **Until something watches a pane, streaming costs nothing measurable.** The
  hook in `readLoop` is one atomic increment and one atomic nil load
  (`Pane.noteOutputLocked`), under the `p.mu` the read loop already holds — no
  allocation, no second lock, no branch that touches the screen.
  `TestReadLoopHookIsAllocationFree` pins it at zero. A pane with no watcher has
  NO framer goroutine at all (property N2, `framerCount()`); the first watcher
  starts one and the last to leave stops it.

- **The framer is WAKE-DRIVEN, not polled, and the tick's early return must stay
  allocation-free.** A wake is a non-blocking send into a cap-1 channel, so a
  thousand writes between two frames coalesce into one. `TestIdleWatchedPaneCostsNothing`
  pins a watched-but-idle tick at zero allocations and zero frames. A 15 fps
  poll over eight watched panes would diff 120 screens a second forever.

- **A DEFERRED offer must be remembered, because nothing else remembers it.**
  `tick` advances `fr.lastGen` as soon as it reads the screen and folds the
  change into the SHARED shadow, so the next wake finds an unchanged generation
  and, past that, a diff with no rows in it. If the pane then goes quiet there
  is no further generation to bring the work back. `framer.deferred` plus
  `paneWatcher.pending` is what survives that, and `retryDeferred` is the only
  thing that ever comes back for it. The symptom was worst in NOTIFY mode, whose
  limit is a whole second: a client subscribed, the pane changed a few hundred
  milliseconds later and then fell silent, and the `changed` was dropped for
  good — a client sitting on a screen it believed was current, which is the one
  failure a notification channel exists to prevent. Found by
  `test/rc/case4-mcp.ts`, pinned by
  `TestNotifyModeDoesNotDropTheChangeItRateLimited`.

- **`paneWatcher.tooSoon` is the ONE place the two modes' rate limits are
  written down.** Frames mode is bounded by its own fps; notify mode by
  `notifyMinInterval` (one second), because a `changed` costs the client a whole
  read of the pane and thirty of them a second is not a notification, it is a
  poll with extra steps. They used to be stated twice — an fps check in the tick
  loop and a second, longer check inside `offerChanged` — and the second one
  silently swallowed work the first had already let through.

- **`w.dirty` is per watcher and the shadow is shared.** A row is diffed once,
  by whichever tick saw it, and copied into the shadow; a watcher whose frame
  rate made it skip that tick would lose the row forever if the framer did not
  remember, PER WATCHER, which rows it still owes. Rows are encoded from the
  shadow when they are actually offered, so a slow watcher receives the row as
  it is NOW rather than as it was when it changed.

- **A keyframe is every row; a delta replaces whole rows.** `key: true` means
  the client clears first and then has a complete screen with no memory of what
  came before, which is why a second watcher joining, a resize, an alt-screen
  switch and an explicit `resync` all force one — a client cannot apply row
  deltas across a geometry change. `pendingKey` forces it for EVERYONE watching
  the pane: a keyframe is a correct frame for a watcher that only needed a
  delta, and one shared full frame is cheaper than maintaining two encodings of
  one screen.

- **A frame is not only its rows.** Moving the cursor, hiding it (DECTCEM),
  scrolling back locally and filling the history ring all change what a client
  draws without changing a cell, so the comparison is against the WHOLE
  `FrameHeader` and a header-only frame is a legitimate frame. Second-guessing
  that in `offerFrame` is what made the cursor invisible to every viewer.

- **The stream shows the LIVE screen whatever the local human scrolled to.**
  `viewRow(0, …)`, always; `scrolled` in the header is how a client learns the
  person at the keyboard is looking at something else. Two viewers must not
  fight over one viewport.

- **The watch reply and the first frame are ordered by a SLOT, not by luck.** A
  `watch` creates an INACTIVE slot; `replyAndActivate` queues the reply and
  activates the slot in one `sub.mu` acquisition. So no frame can precede the
  reply that created it, on any transport — a client sizes itself from the reply
  and clears for the keyframe, and a frame arriving first would be applied to a
  screen of unknown dimensions. `pane_closed` clears the slots and refuses a
  concurrent `Offer`, which is why a picture of a pane can never arrive after
  the news that it is gone.

- **Everything the framer reads from the pane it reads under `p.mu` and nothing
  else — never `treeMu`.** It diffs against its shadow, copies the differing
  rows, and releases the lock; the JSON encoding happens afterwards, unlocked,
  from the shadow. A 24-row encode sitting between the read loop and its next
  byte is a stalled child. Lock order: `streamMu -> fr.mu -> sub.mu`, and
  nothing here takes `treeMu` while holding any of them.

### Transport invariants (`transport/`, `auth/`)

- **A token ALWAYS exists once magmux listens.** It comes from `MAGMUX_TOKEN`,
  from `--token-file`, or it is generated and written to
  `{sockdir}/magmux-{id}.token` at 0600. There is no "no token" mode, and there
  is deliberately no `--token` flag: a value on a command line is in every `ps`
  listing on the machine. A pane is a shell, so this is remote code execution
  and there is no such thing as an unauthenticated convenience mode.

- **The ORDER in `mux/remote.go` is a security order.** Tokens resolved and the
  generated one written; the TLS pair loaded; the insecure-bind warning printed;
  and only THEN the listener binds. So an accepting port implies a final token
  file at its final name with its final contents — which is the readiness signal
  every harness uses (`test/rc/harness.ts` reads the token off disk, exactly as
  an operator does). Every failure before the bind takes the generated token file
  with it: a file naming a credential for a port nothing is listening on is
  litter that looks like a secret.

- **Comparison is over SHA-256 digests with `subtle.ConstantTimeCompare`, and
  BOTH candidates are compared before either result is read.** Otherwise the
  time, or the branch, says which of the two tokens was closer.

- **A token file is refused unless it is a regular file, owned by this euid,
  with no group or other bit set.** A token another user can read is not a
  secret, and a symlink is somebody else's file. The same rule covers Firebase's
  `commands.keyFile`, which gets the credential treatment and not the config
  treatment.

- **The view token is never generated.** A read-only capability that appears by
  itself is a capability nobody is tracking. It must also differ from the
  session token: a read-only credential that is also the full one grants nothing
  and hides that it does not, so magmux refuses to start.

- **Every refusal is an HTTP STATUS, decided BEFORE any upgrade.** No WebSocket
  close code ever carries an auth failure — a browser cannot read one reliably,
  and a client that has to upgrade in order to learn it was unauthorised has
  already been given a connection. Same for `/v1/events`: once the 200 is out
  there is no way left to say no, so every check happens above it.

- **The raw token never appears in a URL.** EventSource and a browser WebSocket
  cannot set a header, so they present a single-use 30-second TICKET minted by
  an authenticated `POST /v1/tickets`. `ticketAllowed` is an allow-list of
  exactly those two endpoints, not a flag, because a ticket travels where it
  gets logged. A ticket inherits its minter's kind, so it is never a way up; a
  spent, expired and never-existed ticket are one answer, so the set is not
  enumerable. The WebSocket's other channel is the subprotocol
  `magmux.auth.<token>`, and magmux echoes `magmux.v1` and NEVER the auth entry
  — echoing it would put the token in a response header and therefore in every
  proxy log on the way back.

- **Origin is checked against `--allow-origin` or the request's own host, and
  Host is checked on a loopback bind.** That second one is what stops a page on
  the internet driving a magmux on a developer's laptop through DNS rebinding.
  Neither is authentication; both are there because the browser will happily
  attach a credential the user did not mean to spend.

- **`--listen` on a non-loopback address without TLS prints a loud, unconditional
  warning and keeps going.** A Tailscale or WireGuard interface is a legitimate
  place to bind and magmux cannot tell one from a coffee-shop LAN. It is a
  warning rather than a refusal for that reason, and it says the true thing: the
  token and every keystroke cross the network in clear text, and a pane is a
  shell.

- **`StatusFor` is the ONE protocol-code → HTTP-status map.** The hub speaks
  magmux's vocabulary and has no opinion about HTTP; the socket has no statuses
  at all. A second mapping in a second adapter means the same failure answered
  409 on one transport and 400 on another. The body always carries BOTH the code
  and the message, because 409 covers five codes and a client branching on the
  status alone could not tell "the pane is dead" from "the pane is the control
  panel".

- **`http.Server.ErrorLog` must never reach stderr.** A TLS handshake against a
  plain port, or a client that wrote garbage, becomes a log line — and magmux may
  be holding a raw-mode terminal with an alternate screen on it, where a stray
  line corrupts the frame with no way to repaint it. `debugWriter` resolves
  `dbgFile` at WRITE time, because the `http.Server` is built before `init()`
  opens it and a logger bound at construction would be bound to nil forever.

- **Pane env drops every secret.** `MAGMUX_TOKEN`, `MAGMUX_VIEW_TOKEN`,
  `MAGMUX_PLUGIN_TOKEN`, `MAGMUX_PLUGIN_ID` and `MAGMUX_FIREBASE` are removed
  from `os.Environ()` before the appends, so a shell in a pane cannot read the
  credential that would let it drive every other pane. `cfg.Env` can still set
  them explicitly, which is how a plugin's own child gets its token.
  `TestPaneEnvCarriesNoSecrets` sits beside `TestChildIsToldTheResolvedTheme`.

### Plugin invariants (`plugin/`, `mux/controller_plugin.go`)

- **A plugin is a separate process on the ORDINARY socket that does one extra
  thing: it registers.** From that moment its ops are in the op table as
  `<plugin>.<op>` and every transport can call them — `call`,
  `POST /v1/ops/{name}`, a WebSocket message, an MCP dynamic tool
  (`<plugin>__<op>`) or a signed Firebase command — with no code in magmux that
  knows what the plugin does. Adding a per-transport case for plugin ops would
  be four places to forget one.

- **Nothing in `plugin/` imports the multiplexer.** A pane, a screen, a
  controller and the layout lock are all behind two callbacks
  (`Config.Snapshot`, `Config.OnExit`), which is what lets the whole host be
  tested against a bare hub with no terminal — and what makes the
  import-direction guard mean something.

- **Registration is AUTHENTICATED; everything else a plugin claims is CHECKED.**
  A plugin magmux spawned proves itself with the one-time token magmux put in
  its environment; one an operator ran by hand proves itself with the session's
  token. After that, the pane it reports on and the events it emits are checked
  against what it registered, because the socket's only access control is the
  filesystem and anything with a file descriptor can send these bytes. A
  snapshot for a pane the plugin does not own is `forbidden`
  (`TestPluginSelfOpenPaneNeedsRegistration`).

- **`controller:"self"` is resolved from the REGISTRATION, never from the
  message.** A plugin claims a pane by BEING one: `open_pane` with
  `controller:"self"` means "the plugin on THIS connection", so there is no
  string a client could send to claim somebody else's pane. The claim happens at
  OPEN, atomically with the pane existing, which is why there is no window in
  which a pane is live and unowned.

- **A plugin's death is announced in the order a client can act on.** `ops` are
  unregistered first, then `ops_changed`, then `plugin_exited` — so a client
  that reacts to the exit by re-fetching `ops` cannot see the dead plugin's ops
  again. Calls in flight become `plugin_gone` (HTTP 502: magmux is the gateway
  and the plugin is the upstream), a plugin that hangs is cancelled and reported
  as `timeout`, and no plugin process is left orphaned.

- **A plugin-observed pane weakens "a pilot cannot fabricate completion", and
  the panel says so.** The plugin is the observer for panes it claimed, and its
  `awaiting_input` is a claim about a session magmux is not itself following —
  reconciled with the terminal's own idle signals by the same
  `applyTerminalIdle` every controller goes through. The panel labels such a
  pane with the plugin that owns it (`controller: "plugin:<name>"`), because
  "who said this session was done" must stay answerable.

### Firebase invariants (`transport/firebase/`)

- **A mirror is not a terminal.** Frames are last in the priority order, 2 fps
  by default, and the FIRST thing cut when the byte budget runs out; state and
  meta are shed only after them. Its peer is a DATABASE — no connection to
  close, no back-pressure to feel, no reader to block — so every bound a socket
  gets for free is built by hand here: a 500 ms flush tick, a token-bucket byte
  budget, that priority order, and a heartbeat that lets a reader tell a live
  session from a killed one. Anyone who needs the real screen watches over
  WebSocket.

- **The host predicate comes BEFORE the credential.** A service-account token is
  admin on the whole database, so it must never be presented to a host that only
  LOOKS like Firebase. `IsDatabaseURL` is an exact-label match (`evil.com`,
  `x.firebaseio.com.evil.com`, a userinfo section, plain `http`, a non-443 port
  are all refused) run before any authenticated I/O, and the SAME predicate
  guards the `Authorization` header across a redirect — Go strips it on a
  cross-host 307, and re-adding it unconditionally would hand the token to
  whoever answered.

- **The command defence is TWO layers, and either one alone fails open.**
  (1) The shipped RULES (`examples/firebase/database.rules.json`) refuse a write
  from a uid that is not in the mirrored owner list, and pin `uid === auth.uid`,
  so a forged command never lands and magmux never sees it — that is the layer
  that survives magmux being wrong. (2) MAGMUX refuses a bad HMAC from a real
  owner, plus the timestamp skew, the nonce, the op allowlist and the rest of
  (a)-(f) — that is the layer that survives the DATABASE being wrong: a stolen
  session, a mis-set rule, a compromised console. `test/rc/case3-firebase.ts`
  proves both on the real rules engine, and asserts that nothing from the
  refused command reached a PTY.

- **The owner list is mirrored ABOVE the session, not inside it.** The rules have
  to resolve an owner before they know which session a write is for, and a
  session-scoped owner list would let a forged session define its own owners.

- **A command runs AT MOST once.** Before any op that is not class read, magmux
  writes a durable `claimed` result and WAITS for RTDB to acknowledge it. A
  crash after the claim leaves a record that reads "outcome unknown, and it will
  never run again", which is the honest answer; a crash before it leaves a
  command that was never claimed and never ran. Across a restart nothing replays
  at all, because `sid` carries the process start time and the new session
  listens on a different path. `TestCommandClaimPrecedesSideEffect` is the
  ordering; the claim-then-run rule is why the side effect can never be the
  thing that happens twice.

- **The canonical string has `args` LAST, and `args` is signed as the RAW JSON
  STRING on the wire.** `args` is the only field whose content is
  attacker-chosen and unbounded, so a newline inside it cannot shift a later
  field into a different position. Signing a re-encoding of the parsed value
  would mean signing one spelling and verifying another. `sigPrefix`
  (`magmux.cmd.v1`) versions the whole meaning, because a key is a long-lived
  secret and the canonical string is the only thing that gives it meaning.
  `examples/firebase/hmac-vector.json` is the committed vector, and any client
  that signs must reproduce it — `test/rc/emulator.ts` checks itself against it
  before it signs anything, precisely so a harness bug cannot look like a magmux
  bug.

- **RTDB KEYS CANNOT CONTAIN `.` `$` `#` `[` `]` `/`, so every free-form payload
  is stored as a JSON STRING.** An op's schema can hold `$ref`, a plugin's event
  data is whatever the plugin says, and an op's result is whatever the op
  returns; all three travel as strings, so no plugin can make the mirror
  unwritable by naming a key with a dollar in it. An op NAME becomes a key
  through `opKey`, which reuses MCP's `<plugin>__<op>` spelling rather than
  inventing a second mapping. Row keys are `r0..rN` and event keys are
  `e000000000042`, never arrays: RTDB turns a contiguous integer-keyed object
  into a JSON array on read, which silently changes a client's parse the moment
  a row goes missing.

- **The emulator case is gated and is never weakened to make it run.**
  `MAGMUX_FIREBASE_EMULATOR=1`, firebase-tools and a JDK 21+. It is the only
  place three assumptions are TESTED rather than asserted: that
  `Authorization: Bearer owner` really is the emulator's admin bypass (undocumented;
  the source is firebase-tools 15.19.1, `lib/emulator/hubExport.js:152-157`),
  that the shipped rules really do refuse a non-owner on the real rules engine,
  and that a `$ref` schema really does mirror.

## Dependencies

Go: only `golang.org/x/sys` (PTY ioctls) and `golang.org/x/term` (raw mode).
Zero third-party — the control panel is raw ANSI written through the pane's own
VT parser rather than a TUI library, which is what keeps that true.

The pilot is separate and out-of-process by design: it is TypeScript
(`@earendil-works/pi-coding-agent`, run with bun), talks to magmux only over
the documented socket, and nothing in the Go binary depends on it.

## Release

Uses GoReleaser. To release:

1. **Add the `## [X.Y.Z]` section to `CHANGELOG.md` first.** The release job
   extracts that section and passes it to GoReleaser as `--release-notes`, and
   it FAILS if the section is missing. That is deliberate: an empty notes file
   would publish an unreadable release and nothing downstream would report it,
   whereas a failed job is visible and re-runnable once the entry is added.
   The tag must already be pushed for the job to run, so the cost of forgetting
   is a re-run, not a burned version.
2. Tag the MERGE commit: `git tag -a v0.1.0 -m "..." <merge-sha>`
3. Push the tag as an explicit ref: `git push origin refs/tags/v0.1.0` —
   never `--tags`, which pushes every local tag the machine has accumulated.
4. CI builds binaries for darwin/linux (arm64/amd64)
5. GoReleaser creates the GitHub Release + updates the Homebrew formula

There is no version string to edit anywhere: `.goreleaser.yml` injects it at
build time via `-X github.com/MadAppGang/magmux/buildinfo.Version={{.Version}}`
(from `./cmd/magmux`), so **the tag is the version**.
`magmux --version` on a snapshot build reports `X.Y.Z-SNAPSHOT-<sha>`, which is
how to tell a real release binary from a local one.

Homebrew ships a CASK, not a formula, since v0.12.0. `goreleaser check` is
clean — no deprecated keys remain, so it CAN now be used as a CI gate.

The move was not a rename. It changed the artifact and its location in the tap
(`Formula/magmux.rb` -> `Casks/magmux.rb`), and three things in
`MadAppGang/homebrew-tap` had to change together: the cask was added, the old
formula deleted, and `tap_migrations.json` added mapping `magmux` to
`madappgang/tap` so an existing `brew upgrade` finds the cask instead of
silently finding nothing. GoReleaser writes the cask on each release; it does
NOT maintain the other two, so they are one-time state that lives in the tap
repo and not here.

Two consequences worth knowing before touching this again:

- **The `postflight` quarantine hook is load-bearing, and it is the cask's
  fault, not Go's or macOS's.** Homebrew quarantines whatever it downloads —
  the cached tarball carries `com.apple.quarantine` before extraction — and
  CASKS then propagate it onto the payload (`Quarantine.propagate`,
  `cask/download.rb:129`), which formulas never do; every call site is under
  `cask/`. Gatekeeper enforces the attribute on any Mach-O executable, `.app`
  bundle or not, and refuses it because the binary is unsigned. Measured, not
  assumed: installing the cask without the hook leaves the attribute set, and
  `magmux --version` prints nothing and exits while `brew install` still
  reports success. The permanent fix is signing + notarizing in CI, which
  needs a paid Apple Developer account; until then the hook is the whole of
  the mitigation. Homebrew warns that `postflight` is
  deprecated in favour of `postflight_steps`; that string comes from
  GoReleaser's template, so it is theirs to fix, and a visible warning is the
  better half of that trade.
- **Casks have no `test` stanza, so `brew test magmux` is gone.** The formula
  ran `magmux --version` and its test was itself the subject of a fixed bug.
  Nothing now checks the published binary from Homebrew's side — **and nothing
  checks it from ours.** `.github/workflows/release.yml` extracts the changelog
  section and runs GoReleaser; no published artifact is ever downloaded or
  executed. A binary that cannot start would ship with every check green. This
  said the opposite until v0.13.0, when a release review read the workflow and
  found the smoke test it named had never existed.

## VT Parser Coverage

Covers ~95% of tmux's escape sequences. See the gap analysis in the research docs. Key sequences handled:

- CSI: A-H (cursor), J/K (erase), L/M (lines), P/@ (chars), S/T (scroll), m (SGR), r (scroll region), n (DSR)
- ESC: 7/8 (cursor save/restore), D/M/E (index), c (reset), (0/(B (charset)
- DEC modes: 1049/47/1047 (alt screen), 2004 (bracketed paste), 1004 (focus), 1000/1002/1006 (mouse)
- SGR: 0-9, 21-29, 30-49, 53/55, 90-107, 38/48;5;N, 38/48;2;R;G;B
