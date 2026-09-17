# A `resize` op: client-chosen screen geometry

Status: **designed, not built.** Approved by the maintainer on 2026-09-17 and
then deferred, because the change that prompted it turned out to be unnecessary
(see "Why it was deferred"). This file exists so the design is not re-derived.

## The problem, in magmux's own terms

`m.rows` / `m.cols` mean two things at once:

1. the size of the local tty, and
2. the size of the virtual screen the layout is built into.

Every consumer reads the second meaning — `buildGrid`, `reflowLocked`,
`sockCapabilities`, the Firebase mirror's geometry, `buildPaneResultsLocked`.
Exactly two producers write the first: `init()` via `term.GetSize`, and
`handleSIGWINCH`.

Because both meanings live in one pair of ints, a remote client cannot ask for a
geometry. A browser 2560px wide shows whatever the local tmux pane happens to
be. Splitting those two meanings is the whole feature; everything else falls out
of it.

tmux has the same constraint and one extra lever. A tmux window has ONE grid and
every attached client sees it; `window-size` (`latest` / `largest` / `smallest` /
`manual`) only decides whose size wins, and clients that do not match are padded
or clipped. tmux never reflows either — and neither can magmux, because a
scrollback row keeps the width it was printed at and nothing records which rows
were soft-wrapped continuations. What tmux can do that magmux cannot is resize
the window to fit a client.

## The shape: two ints become four

| field | means | written by | read by |
|---|---|---|---|
| `m.rows`, `m.cols` | the VIRTUAL screen — the layout, what clients see, what `capabilities` reports | `applySizeLocked` only | everything that reads it today, unchanged |
| `m.autoRows`, `m.autoCols` | the geometry magmux picks when nobody asked — the tty, or `headlessSize()` | `init()`, `handleSIGWINCH` | `screenSizeLocked`, `clipLocked` |
| `m.manualRows`, `m.manualCols` | what a client asked for; `0` = nobody asked | the op | `screenSizeLocked`, the status bar |

Two new one-place claims sit beside the existing ones, and `reflowLocked` is
deliberately left byte-identical so its own claim ("the one place the tree is
resized") stays true:

- `screenSizeLocked()` — the ONE place the virtual size is DECIDED: manual, else
  auto, else what it already has. The last branch is the headless case, not a
  bug fallback.
- `applySizeLocked()` — the ONE place `m.rows`/`m.cols` are WRITTEN after
  `init()`. It sets them from `screenSizeLocked()` then calls `reflowLocked()`.

`handleSIGWINCH` writes `auto*` and calls `applySizeLocked` + `markAllDirty` +
`clearNext`. It must NOT touch `manual*` — releasing manual mode on a window
resize is precisely what manual mode exists to prevent.

## Decisions

**Per-session, not per-pane.** Two different requests hide behind "resize":
changing the whole screen (what a wide browser wants) and moving a split ratio
(what "make pane 2 wider" means). They share no code. `resize` does the first.
`pane` is accepted only when the session has ONE session leaf, where
`{"pane":0,"rows":44,"cols":160}` exactly means "size the screen so pane 0 ends
up 44x160" — i.e. `screen = (44 + statusRowsLocked(), 160)`. With more than one
leaf it is REFUSED with `bad_request`, because solving backwards through integer
ratios has more than one answer and every wrong answer is invisible: success
plus a pane that is not the size asked for. Per-pane resizing, if wanted, is a
separate `set_ratio` op that writes `parent.ratio` and calls `reshapeChildren`.

**`ClassControl`, not `ClassDisplay`.** Verified: `auth.Store.Authorize` lets a
view token through only for a built-in op of class read or one named by
`--view-op`, and `hub.Call` refuses any non-`ClassRead` op to a read-only
caller — so `ClassDisplay` would also be refused today. `ClassControl` is still
right, because a resize calls `pty.SetWinSize` on every child and `Screen.resize`
truncates live columns. `--view-op resize` must read as "viewers may change the
screen for everyone", not as "viewers may tint things".

**Manual mode** is entered by a successful resize (no separate verb — a mode you
can enter without changing anything is one nobody can see they are in) and
released two ways: `resize {"auto":true}`, or `Ctrl-G r` locally. The local key
is not optional: a human whose terminal was resized by a remote client and who
has no key to undo it is in a mode they cannot leave. There is no timed release
and no release-on-SIGWINCH.

**Bounds** reuse the existing constant. `maxHeadlessCells` (4,000,000) caps the
PRODUCT and its comment already explains why a per-side cap alone is an OOM;
rename it `maxScreenCells`, add `maxScreenSide` (10,000), and extract
`checkScreenSize(rows, cols)`. `headlessSize()` calls it and falls back to 80x24
with a line on stderr — the value came from the environment and there is nobody
to refuse. The op calls it and REFUSES — the value came from a request and there
is somebody to tell.

**Refuse, do not clamp.** CLAUDE.md states it as "CREATION refuses; RESHAPE
clamps", and `buildColumn`'s comment gives the real rule: a SIGWINCH has nobody
to tell. An op has somebody to tell, so it refuses `too_small` when the geometry
cannot give every session leaf `minPaneRows` x `minPaneCols`. Checking that
without a second copy of the layout arithmetic needs `splitBoxes` extracted out
of `reshapeChildren` — the boxes computed, nothing written — so a dry-run
`treeFitsLocked` can recurse with the same function. The dry run must not mutate:
`Screen.resize` destroys cells that do not fit, so there is no rollback.

This creates a deliberate asymmetry: a SIGWINCH to the same too-small geometry
still clamps, while the op refuses. A clamped pane is alive, addressable, and
captures as empty — the failure shape this codebase has already fixed twice.

**Watchers need nothing new.** Traced rather than assumed: `Pane.resize` →
`noteGeometryLocked` → `framer.tick`'s `full` predicate → `key = true` for every
watcher, with the new geometry in the header. A geometry change already forces a
keyframe on every transport. Two small gaps remain and were deferred: a
`notify`-mode client learns the new size only when it next reads, and a
non-watching subscriber learns nothing until the next `snapshot`. A
`protocol.EventResized` broadcast would close both at the cost of a wire-schema
addition across the socket events, `capabilities`, the Firebase event ring and
MCP.

**Scrollback needs nothing.** `Screen.resize` keeps the ring, each row keeps its
original width, nothing reflows, and every reader already bounds by `len(row)`.
Two consequences belong in the op's description because a client cannot discover
them: growing is lossless and leaves history at its old width inside a wider
pane, and **shrinking is lossy for live content** — `Screen.resize` copies only
what fits. A client that resizes on every browser-window drag will shred the live
screen, so debouncing is the client's job.

## The hard part: the renderer must clip

magmux renders straight to stdout with absolute positioning and a dirty-flag
optimisation. It assumes the virtual screen IS the tty. tmux can show a 160x44
window to an 80x24 client because a tmux CLIENT draws a viewport and clips;
magmux has no viewport. So a virtual screen larger than the terminal corrupts the
local display unless the renderer learns to clip.

- `clipLocked()` is the ONE `min(virtual, tty)`, in the spirit of
  `statusRowsLocked`. Headless returns the virtual size unclipped, keyed on
  `m.headless` rather than on `autoRows` (a headless magmux does have `autoRows`,
  from `headlessSize`, and is not a terminal).
- `Renderer.span(row, col, width) int` is the enforcement primitive: it positions
  the cursor and returns how many cells are actually on screen. **Zero means
  paint nothing, and the cursor is deliberately NOT moved** — a painter that
  writes anyway lands at the previous cursor position, putting stray glyphs in
  the middle of a visible pane, which is worse than painting off the edge where
  the terminal would merely clamp.
- Grep-able invariant, like `m.allPanes[`: `grep -n 'r\.moveTo(' mux/render.go`
  must hit `span` and `showCursor` and nothing else. There are 14 emit sites,
  and a fourth CUP emitter hides OUTSIDE the `Renderer` in `renderLocked`'s
  cursor-only fast path.
- **The status bar is the sharpest consequence.** It is bounded by `m.cols`
  today, and `m.cols` stops meaning "the tty's width". Its width must come from
  `clipLocked()`, or the whole fitting machinery that guarantees `q quit`
  survives on a narrow terminal would place it at a column the clip eats — the
  human loses the way out, silently. Its ROW must move to the last visible row,
  or under manual-larger the bar is off-screen and the mode cannot announce
  itself. The cost is that it covers one row of pane content locally.
- **Mouse and selection need no transform**, and that is the payoff of showing
  the top-left corner and never panning: tty (r,c) IS virtual (r,c). One new
  case: a click on the moved status row used to be impossible and now lands on a
  pane in virtual coordinates, so it needs a guard.
- **A SMALLER manual size is not free.** The renderer never clears, so shrinking
  leaves the last frame's characters standing in the abandoned rows and columns
  forever. A one-shot `clearNext` flag emits `\x1b[2J` on the next frame.

## Phases

- **A — "the size is one decision".** Four fields, `screenSizeLocked`,
  `applySizeLocked`, `init()` and `handleSIGWINCH` rewired, the bounds constant
  renamed and extracted, `splitBoxes` extracted. NO behaviour change; the
  existing suite is the test. Worth its own review, because it is the only phase
  whose diff can be read against a "nothing should change" standard.
- **B — the op and manual mode**, refusing any size larger than the terminal.
  This is already the whole feature for `magmux --headless --listen`, which needs
  no clip. Everything a client can observe is final here.
- **C — the renderer clips.** 14 sites, its own tests, and it ends by deleting
  Phase B's four-line refusal, so the refusal is the feature flag.

## The tests that matter

1. **A frame walker, not golden frames**: build a `Magmux` with virtual 60x200
   and tty 24x80, call `renderLocked`, and assert every `\x1b[R;CH` in the
   returned bytes has `R <= 24`, `C <= 80`, and that the runes written before the
   next CUP do not carry the column past 80. One test covers all 14 sites and
   every site added later.
2. A source-level test that `r.moveTo` is only reached through `span`.
3. A SIGWINCH does not undo a manual size.
4. A refused resize leaves the tree BYTE-IDENTICAL — that is the assertion, not
   the error code, because there is no rollback.
5. `TestPTYRunStillPaintsStdout` extended under a manual size. If the clip ever
   misfires so magmux paints nothing, every socket-based test in this repo keeps
   passing; this is the only one that fails.

## Why it was deferred

The request that prompted it was "the browser shows an 80x8 terminal". That
turned out to be a bug in the demo launcher, not a missing capability: a DETACHED
tmux session has no client to take its size from, so tmux gives it 80x24, and any
pane built before a client attaches inherits that. Fixing the launcher to measure
the real window gave magmux 115x47 and the browser a proper terminal with no
product change at all.

So `resize` is worth building when a client genuinely needs a geometry LARGER
than the terminal magmux is running in — a wide browser onto a session in a small
pane, or a headless magmux whose size should change after startup. Note that
Phase B alone delivers the second case, and `COLUMNS`/`LINES` are read once in
`headlessSize()` at exec time, so today there is no way to change a headless
magmux's geometry at all.
