# magmux - Development Notes

## Build

```bash
go build -o magmux .
```

## Architecture

~4,800 lines of Go across a few files. `main.go` (~3,950 lines) holds the
terminal core; the tool-controller layer lives beside it.

`main.go`, in order:

1. **Cell/Screen** — Cell struct (rune + Color + Attr), the viewport grid, and
   the scrollback ring behind it
2. **VT Parser** — DEC ANSI state machine (port of vtparser.c), handles CSI/ESC/OSC/C0
3. **Pane** — Binary tree layout node, owns PTY + Screen + VT parser
4. **PTY helpers** — Raw /dev/ptmx + ioctls (no CGo); platform bits in `pty_darwin.go` / `pty_linux.go`
5. **Renderer** — ANSI escape code output with dirty-flag optimization
6. **Multiplexer** — Main event loop, input routing, mouse handling, SIGWINCH
7. **Selection** — Mouse drag text selection + clipboard copy (OSC 52 + pbcopy)
8. **Chrome** — the panel/status-bar toggles (`Ctrl-G p` / `Ctrl-G s`),
   `statusRowsLocked` / `reflowLocked`, and the status bar's panel digest.
   magmux's default is to show none of itself: a lone session pane is a bare
   terminal, because `renderBorder` only ever paints a SPLIT node.

Tool controllers:

- `controller.go` — the `ToolController` interface and the `Snapshot` /
  `ControllerState` surface every controller produces, plus the optional
  `InputNotifier` interface.
- `controller_claude.go` — observes a Claude Code pane by tailing its JSONL
  transcript under `~/.claude/projects/`.

Controlled sessions (an external AI agent steering a pane):

- `pilot.go` — the `send` socket verb and PTY injection. The inbound half.
- `control.go` — the control panel: the OUT/IN log of pilot↔session traffic,
  painted into a PTY-less pane.
- `pilot/pilot.ts` — the pi.dev agent that does the steering, with its
  toolbox replaced by `send_to_session` + `finish`.
- `pilot/magmux.ts` — its socket bridge.
- `test/ui/case3.ts` — the visual end-to-end case for the whole loop.

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
  pane. Enforce with `grep -n 'm\.allPanes\[' *.go`, which must hit nothing
  outside `paneByIDLocked`.

- **`treeMu` guards the layout; `p.mu` still guards content.** `treeMu` covers
  `m.root`, `m.allPanes` (header, elements, each element's `id`/`closed`/
  `label`), `m.focused`, `m.statusText`, `m.rows`/`m.cols`, `m.closeAt`, the
  package-level `sel`, and every `Pane`'s STRUCTURAL fields (`splitType, y, x,
  h, w, ratio, child1, child2, parent`). Content fields (`screen, dirty, dead,
  tint, inputReady, …`) stay under `p.mu`. Lock order:

  ```
  treeMu -> p.mu -> sockClientsMu
  treeMu -> cp.mu ;  treeMu -> claimedMu
  ```

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
  3. Never acquire `treeMu` while holding `p.mu`, `cp.mu`, `sockClientsMu` or
     `claimedMu`.

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
  issue #333.

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

### Controlled-session invariants

- **"Done" answers two different questions, and `dead` is the answer to only
  one of them.** For `-w` and auto-exit, done means "nothing left to wait for",
  so `allPanesDone` counts `dead || inputReady`. For anything that decides
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
  $ grep -n 'observed *=\|observed++\|steplog\[i\].state = ' control.go
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

## Dependencies

Go: only `golang.org/x/sys` (PTY ioctls) and `golang.org/x/term` (raw mode).
Zero third-party — the control panel is raw ANSI written through the pane's own
VT parser rather than a TUI library, which is what keeps that true.

The pilot is separate and out-of-process by design: it is TypeScript
(`@earendil-works/pi-coding-agent`, run with bun), talks to magmux only over
the documented socket, and nothing in the Go binary depends on it.

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
