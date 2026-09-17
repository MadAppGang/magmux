#!/usr/bin/env bash
# `task demo:rc` — one magmux, two read-only clients, one public API.
#
#   demo/rc/demo.sh                 stacked tmux panes + the browser page
#   RC_DEMO_MIRROR=1 …              ADD the terminal mirror pane (off by default)
#   RC_DEMO_GEOM=120x36 …           run magmux HEADLESS at that size, with no
#                                   magmux pane at all — the browser is the
#                                   terminal. `off` pins the tmux-pane shape.
#   RC_DEMO_TMUX=adopt|own …        force a tmux mode; the default is measured
#   RC_DEMO_LAYOUT=windows …        the old literal reading: a window per surface
#   NO_BROWSER=1 …                  print the URL, open nothing
#   NO_ATTACH=1 …                   build it all, never attach (own mode's CI shape)
#   RC_DEMO_KEEP=1 …                leave $STATE behind for a post-mortem
#
# ── THE THING BEING DEMONSTRATED GETS THE ROOM ──────────────────────────────
#
# magmux is what this demo is about, so magmux gets everything the other
# surfaces do not need: the driver's menu is COMPACT (two columns, 14 rows) and
# the terminal mirror is OFF unless asked for. On a 50-row window that is ~35
# rows of magmux rather than the ~16 the old three-way split left it.
#
# The mirror is off by default because the BROWSER is already a second,
# independent client painting the same frames through the same decoder — and it
# paints them better, at a font size a person can read across a room. Two
# terminals side by side is a picture worth having when the claim itself is
# under review (`RC_DEMO_MIRROR=1`); it is not worth a third of the height on
# every ordinary run.
#
# ── TWO TMUX MODES, chosen by whether $TMUX is set ──────────────────────────
#
# "Assuming we have tmux" means USE THE TMUX THE OPERATOR IS IN. Starting a
# private server from inside one nests a tmux in a tmux, with two status bars,
# which is a demonstration of nothing.
#
#   ADOPT  ($TMUX set, or RC_DEMO_TMUX=adopt)
#     No new server, no new session, NO NEW WINDOW, and nothing is ever
#     attached. This launcher is already running in a pane of the operator's own
#     window, so that pane IS the driver: the script splits panes ABOVE itself
#     for magmux and the mirror, and then runs the driver TUI in the foreground,
#     here.
#
#         ┌────────────────────────────┐
#         │ magmux          (new pane) │  ALL the rows nothing else needs
#         │                            │
#         ├────────────────────────────┤
#         │ the mirror      (new pane) │  only under RC_DEMO_MIRROR=1
#         ├────────────────────────────┤
#         │ the driver ← THE PANE YOU  │  this script, then the driver TUI
#         │              TYPED IN      │  14 rows, actions + response
#         └────────────────────────────┘
#
#     …and under RC_DEMO_GEOM there is no magmux pane at all: magmux runs
#     headless at a size this terminal does not have to be able to hold, the
#     browser is the terminal, and the operator's pane stays whole.
#
#     Three awkward things collapse: there is nowhere to "go back to" in order
#     to end the demo, the driver's `q` and Ctrl-C are the SAME exit hitting the
#     same trap, and nothing is attached or nested so the tmux-in-tmux bug
#     cannot come back by any path.
#
#     *** THE DEMO IS A GUEST. *** Teardown kills exactly the panes it created,
#     each addressed by the `#{pane_id}` captured at creation AND re-checked
#     against a pane option this run set on it. Never the operator's pane, never
#     their window, never their session, NEVER kill-server, and
#     `destroy-unattached` is never set on anything — that option on somebody
#     else's session destroys their work the moment they detach. Detaching does
#     NOT end the demo here; `q`, Ctrl-C, or `demo/rc/stop.sh` do.
#
#   OWN SERVER  ($TMUX unset, or RC_DEMO_TMUX=own)
#     A private server (`tmux -L magmux-<id>`), its own session, three stacked
#     panes, attached at the end. Nothing of the operator's is on it, so the
#     teardown can `kill-server`, `destroy-unattached` can reach nothing else,
#     and detaching DOES end the demo — all correct when the demo is the only
#     thing on that server.
#
# THIS SCRIPT IS THE SOLE ENTRY POINT AND THE SOLE OWNER OF TEARDOWN. Nothing
# else kills anything; there is exactly one `trap … EXIT INT TERM`, at the
# bottom of the setup, and every process this demo starts dies there. A demo
# that leaks a listening port holding a live video feed of somebody's shell is
# worse than a demo that fails.
#
# The order below is load-bearing twice over, and neither ordering can be
# swapped:
#
#   1. The STATIC SERVER binds first, because magmux's `--allow-origin` has to
#      name a port the kernel has not chosen yet.
#   2. MAGMUX binds second, and the launcher then writes its URL into
#      `$STATE/magmux.url` — because the page has to learn magmux's own
#      kernel-chosen port, which did not exist when the static server started.
#      `serve.ts` reads that file per request and answers 503 until it appears.
#      That file IS the channel; without it the browser client cannot work.
#
# Nothing sleeps waiting for startup. Both waits are the same idiom: poll a
# file for a line, with a deadline, and print the whole file if the deadline
# passes. A readiness signal that is a fixed sleep is a demo that fails on a
# loaded laptop and wastes the difference on every other run.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

# RC_DEMO_ID separates two demos running at once: it names magmux's socket, the
# tmux server socket, the tmux session and the default state directory, so a
# second run collides with nothing (risk R-5). Both TCP ports are
# kernel-chosen, so they never collide either.
ID="${RC_DEMO_ID:-rc-demo}"
STATE="${RC_DEMO_STATE:-$ROOT/.task-grids/$ID}"
SESSION="magmux-$ID"
TMUX_SOCK="magmux-$ID"          # a PRIVATE tmux server (`tmux -L`), own mode only

# PANES, not windows, and that is the default rather than a preference.
#
# The demo's whole point is watching one instruction reach both clients at once,
# and with a window per surface you are never looking at the mirror while it
# happens. Stacking also gives every pane the SAME column count, which the ANSI
# painter needs — a column shortfall clips every styled row (risk R-2).
# `windows` is kept for anyone who wants the literal separate-terminals reading;
# it is a last resort and never something the demo picks on its own.
LAYOUT="${RC_DEMO_LAYOUT:-panes}"
[ "$LAYOUT" = "stacked" ] && LAYOUT="panes"     # the name this knob used to have

# THE TERMINAL MIRROR IS OFF BY DEFAULT, and that is a decision about what the
# demo is FOR rather than a budget compromise.
#
# The claim is "one hub, two independent clients through one public API", and
# the browser tab is the second client: same `client.ts`, same `frame.ts`, a
# different painter. It demonstrates the claim at a font size a room can read.
# The tmux mirror demonstrates the same claim again, in a third of the window's
# height, at whatever size is left — and every one of those rows comes out of
# the thing being demonstrated. So it is opt-in: `RC_DEMO_MIRROR=1` is worth it
# when the DECODER is what is under review (two surfaces, one wire, side by
# side, diffable by eye) and not much else.
case "${RC_DEMO_MIRROR:-0}" in
  1|yes|on|true) WANT_MIRROR=1 ;;
  *)             WANT_MIRROR=0 ;;
esac

# How many rows the driver's menu wants. It is COMPACT — two columns, reprinted
# by `m` — so 14 rows holds the whole menu plus the prompt and the last answer.
# It is a FLOOR expressed as a fixed size, deliberately: every row above it goes
# to magmux, and a driver that grew with the window would take the demo's own
# subject with it.
DRIVER_ROWS="${RC_DEMO_DRIVER_ROWS:-14}"
# Below this a magmux pane is not worth looking at: the colour card is 16 rows,
# and under 8 the shell prompt is all that is left of it.
MIN_MAGMUX_ROWS=8

# Where the DRIVER goes. In adopt mode this is not a choice at all — the driver
# is the pane the operator typed in. In own mode `window` is an explicit escape
# hatch and NEVER a fallback the demo picks for you: a driver that silently
# moved to another window is a driver nobody finds, which is what this knob's
# old `auto` value did.
DRIVER_PLACE="${RC_DEMO_DRIVER:-pane}"

SERVE_LOG="$STATE/serve.log"
SERVE_PIDFILE="$STATE/serve.pid"
MAGMUX_ERR="$STATE/magmux.err"
URL_FILE="$STATE/magmux.url"
VIEW_TOKEN="$STATE/view.token"
SESSION_TOKEN="$STATE/magmux-$ID.token"
# What stop.sh needs in order to be as careful as this launcher: which mode was
# used, which server, which panes, and which server INCARNATION. Written by
# `write_tmux_state` below, where every field is justified.
TMUX_STATE="$STATE/tmux.state"

READY_DEADLINE="${RC_DEMO_READY_TIMEOUT:-30}"   # seconds, each of the two polls

SERVE_PID=""
# magmux's own pid, and ONLY under RC_DEMO_GEOM, where it is a background child
# of this launcher rather than a tmux pane. Empty otherwise, which is what every
# `[ -n … ]` below tests.
MAGMUX_PID=""

B=$'\033[1m'; G=$'\033[90m'; R=$'\033[31m'; Y=$'\033[33m'; N=$'\033[0m'

say()  { printf '%s\n' "$*" >&2; }
die()  { printf '%sdemo:rc: %s%s\n' "$R" "$*" "$N" >&2; exit 1; }
note() { printf '%sdemo:rc: %s%s\n' "$G" "$*" "$N" >&2; }
warn() { printf '%sdemo:rc: %s%s\n' "$Y" "$*" "$N" >&2; }

# ── which tmux ──────────────────────────────────────────────────────────────
# `auto` is a MEASUREMENT, not a preference: tmux sets $TMUX in every pane it
# owns, so it is the one reliable answer to "is there already a tmux here". The
# override exists in both directions because a forced answer is sometimes right
# — `own` for anyone who wants the self-contained shape from inside tmux,
# `adopt` for a harness pointing $TMUX at a server of its own.
TMUX_MODE="${RC_DEMO_TMUX:-auto}"
case "$TMUX_MODE" in
  auto)  if [ -n "${TMUX:-}" ]; then TMUX_MODE="adopt"; else TMUX_MODE="own"; fi ;;
  adopt|own) ;;
  *) die "RC_DEMO_TMUX must be 'adopt', 'own' or unset, not '$TMUX_MODE'" ;;
esac

# The pane option every pane this run creates is stamped with, and the second of
# the two facts teardown requires before killing anything. A pane id alone is
# not enough: ids restart at %0 when a server restarts, so a stale id from a
# previous incarnation is a live id belonging to somebody else's shell.
MARK_OPT="@rc_demo"
DEMO_PANES=()

# ONE place decides which tmux server every command below talks to. In own mode
# `env -u TMUX` is not cosmetic: tmux refuses `attach-session` on the presence
# of that variable alone, even when the target is a different server.
if [ "$TMUX_MODE" = "own" ]; then
  tm() { env -u TMUX tmux -L "$TMUX_SOCK" "$@"; }
else
  tm() { tmux "$@"; }
fi

# Ask the AMBIENT tmux about the pane this launcher is running in. $TMUX_PANE is
# set beside $TMUX and is the only way to be sure: `display-message` with no
# target answers about whatever window the client is looking at NOW, which is
# not necessarily the one the operator typed `task demo:rc` in.
ambient() { # $1 = a tmux FORMAT
  if [ -n "${TMUX_PANE:-}" ]; then
    tmux display-message -p -t "$TMUX_PANE" "$1" 2>/dev/null
  else
    tmux display-message -p "$1" 2>/dev/null
  fi
}

# ── 1. preflight ────────────────────────────────────────────────────────────
# Refuse with a clear message rather than producing a bad picture. Every one of
# these failures is otherwise a demo that comes up looking broken in a way that
# points at magmux instead of at the machine.

command -v tmux >/dev/null 2>&1 || die "tmux is not installed — this demo is a tmux layout (brew install tmux)"
command -v bun  >/dev/null 2>&1 || die "bun is not installed — both clients are TypeScript (brew install oven-sh/bun/bun)"
# Resolved once, and used absolutely below. In adopt mode the panes are children
# of a tmux server that was started long before this demo, with whatever PATH
# the operator's login shell had then — which need not include bun.
BUN_BIN="$(command -v bun)"

# `tmux -V` is PARSED, not merely run. The layout uses `split-window -l N`, the
# `-e` flag for per-pane environment and a `client-attached` hook, none of which
# exists before 3.0, and a 2.x tmux would silently come up as an unsplit 80x24
# window — risk R-9, a bad picture rather than an error.
TMUX_VER_RAW="$(tmux -V 2>/dev/null)"
TMUX_VER="$(printf '%s' "$TMUX_VER_RAW" | sed -nE 's/^tmux (next-)?([0-9]+)\.([0-9]+).*/\2 \3/p')"
[ -n "$TMUX_VER" ] || die "cannot parse the tmux version from '${TMUX_VER_RAW:-<nothing>}'"
# shellcheck disable=SC2086
set -- $TMUX_VER
[ "$1" -ge 3 ] 2>/dev/null || die "tmux $TMUX_VER_RAW is too old — this demo needs 3.0 or newer for 'split-window -e' and 'set-hook client-attached'"

# Adopt mode needs something to adopt. Forced without $TMUX there is no ambient
# server at all, and the failure would otherwise be a confusing "no server
# running" from the first tmux command.
if [ "$TMUX_MODE" = "adopt" ]; then
  [ -n "${TMUX:-}" ] || die "RC_DEMO_TMUX=adopt, but \$TMUX is not set — there is no ambient tmux to adopt.
       Run it from inside tmux, or use RC_DEMO_TMUX=own for a private server."
  [ -n "${TMUX_PANE:-}" ] || die "\$TMUX is set but \$TMUX_PANE is not, so this script cannot tell which pane it
       is in — and adopt mode splits THAT pane. Use RC_DEMO_TMUX=own."
  if [ "$LAYOUT" = "windows" ]; then
    die "RC_DEMO_LAYOUT=windows and adopt mode contradict each other: adopt mode splits the
       window you are in and never creates one. Use RC_DEMO_TMUX=own for the window layout."
  fi
  if [ "$DRIVER_PLACE" = "window" ]; then
    die "RC_DEMO_DRIVER=window and adopt mode contradict each other: the driver IS the pane you
       typed in. Use RC_DEMO_TMUX=own for the window layout."
  fi
fi

# The PREFIX KEY, asked for rather than assumed, in both modes. `Ctrl-B` is only
# the default: a `set -g prefix C-a` in ~/.tmux.conf is read by the ambient
# server AND by a private `-L` one, so a banner that hardcoded Ctrl-B would send
# half its readers to a key that does nothing.
prefix_label() {
  local p
  p="$(tm show-options -gv prefix 2>/dev/null)"
  case "$p" in
    ""|none|None) printf 'the prefix key' ;;
    C-*) printf 'Ctrl-%s' "$(printf '%s' "${p#C-}" | tr '[:lower:]' '[:upper:]')" ;;
    M-*) printf 'Alt-%s'  "$(printf '%s' "${p#M-}" | tr '[:lower:]' '[:upper:]')" ;;
    *)   printf '%s' "$p" ;;
  esac
}

[ -x "$ROOT/magmux" ] || die "$ROOT/magmux is not built or not executable — run 'go build -o magmux ./cmd/magmux' (or 'task demo:rc', which does)"

# THE DRIVER IS A GO BINARY, BUILT HERE, EVERY TIME.
#
# It is a Bubble Tea TUI in `demo/rc/driver/`, which is its OWN Go module with
# its own go.mod and a `replace` back to this repository — magmux's root module
# has zero third-party dependencies and that is a property of the project, not
# an accident, so a TUI library may not be spent on a demo. `go list -m all` at
# the root still answers x/sys and x/term, and `go test ./...` does not descend
# into a nested module.
#
# BUILT, NOT `go run`, AND BUILT ON EVERY START. `go run` recompiles and then
# links a throwaway binary into a temporary directory each time the driver
# starts, which is the slow half twice over — and `task demo:rc:drive` in a
# second terminal starts it again. A binary cached across runs is worse the
# other way: a stale driver against a changed repository is a demo that shows
# yesterday's behaviour with no sign that it is doing so. `go build` is the
# answer to both, because Go's own build cache makes an unchanged rebuild a
# couple of hundred milliseconds and a changed one correct. The output lands in
# .task-grids/, which is gitignored and outside every run's state directory, so
# a second terminal can use the same binary and a teardown does not remove it
# from under a driver that is still running.
DRIVER_BIN="$ROOT/.task-grids/rc-driver"
mkdir -p "$ROOT/.task-grids"
if ! DRIVER_BUILD="$( (cd "$ROOT/demo/rc/driver" && go build -o "$DRIVER_BIN" . ) 2>&1 )"; then
  die "the driver did not build — demo/rc/driver is a Go module of its own:
$(printf '%s\n' "$DRIVER_BUILD" | sed 's/^/       /')"
fi
[ -x "$DRIVER_BIN" ] || die "go build reported success but $DRIVER_BIN is not executable"

# The unix socket path, measured BEFORE anything starts.
#
# `sockdir.sunPathMax` is 100 (sockdir/sockdir.go), a deliberate margin under
# the smaller of the two real sun_path limits — 104 on darwin, 108 on linux.
# Overrun it and magmux does not fail: it falls back to /tmp with a warning on
# a stderr this launcher has redirected to a file, so the symptom is a socket
# nobody expected in a directory the teardown does not clean. Measure it here,
# name the limit, and refuse.
SOCK_PATH="$STATE/magmux-$ID.sock"
if [ "${#SOCK_PATH}" -gt 100 ]; then
  die "the socket path would be ${#SOCK_PATH} bytes and the limit is 100 (sockdir/sockdir.go: sunPathMax):
       $SOCK_PATH
       set RC_DEMO_STATE to something shorter, e.g. RC_DEMO_STATE=/tmp/$ID"
fi

# ── 2. the geometry, the height budget, and the LAYOUT DECISION ────────────
#
# FIRST, and deliberately before the state directory, the static server, magmux
# or a single pane: the only cheap refusal is an early one. A layout that cannot
# work is refused here, where nothing has been started and there is therefore
# nothing to clean up — and a layout that can only work in a smaller shape is
# DEGRADED here, once, where the decision is still a decision rather than a
# post-mortem.
#
# OWN MODE: a DETACHED session has no client to take its size from, so it would
# default to 80x24 and the colour card would clip; `tput` against this terminal
# is the best guess available, and a client attaching later resizes it anyway.
#
# ADOPT MODE: the size is a FACT rather than a guess. What is being subdivided
# is not the window, it is the LAUNCHER'S OWN PANE — the operator may already
# have splits of their own — so the budget is `#{pane_height}` of this pane, on
# a real client.
if [ "$TMUX_MODE" = "adopt" ]; then
  COLS="${RC_DEMO_COLS:-$(ambient '#{pane_width}')}"
  ROWS="${RC_DEMO_ROWS:-$(ambient '#{pane_height}')}"
else
  COLS="${RC_DEMO_COLS:-$(tput cols 2>/dev/null || echo 120)}"
  ROWS="${RC_DEMO_ROWS:-$(tput lines 2>/dev/null || echo 40)}"
fi
# An unreadable or absent answer is not a reason to die — it is a reason to use
# the same fallback the `tput` branch has always had.
case "$COLS" in ''|*[!0-9]*) COLS=120 ;; esac
case "$ROWS" in ''|*[!0-9]*) ROWS=40 ;; esac

# ── MAGMUX'S OWN FLOOR, IN MAGMUX'S OWN NUMBERS ────────────────────────────
#
#     minPaneRows = 3      mux/dynpanes.go:16 — every pane, always
#     minPaneCols = 20     mux/dynpanes.go:17
#     status row  = 1      statusRowsLocked (mux/chrome.go:102), reserved by
#                          buildGrid before it lays a single pane out
#                          (mux/grid.go: `availH < minPaneRows` → refuse)
#
# so the magmux PANE — which runs one `-e` session — needs 3 + 1 = 4 rows and
# 20 columns, and that floor is HARD: magmux does not clamp a creation it was
# asked for, it refuses it ("CREATION refuses; RESHAPE clamps", CLAUDE.md). On a
# 3-row pane it prints
#
#     cannot lay out 1 panes in 80x2: every pane needs at least 3 rows and 20 columns
#
# and EXITS — after it has bound its listener and written its token file, which
# it then removes on the way out. So a layout magmux will refuse does not look
# like a layout problem to anybody downstream: it looks like a missing token.
# That is why this launcher must not build one. Building it anyway was this
# file's own bug, and the report that came back was about file permissions.
MAGMUX_MIN_PANE_ROWS=3
MAGMUX_MIN_PANE_COLS=20
MAGMUX_STATUS_ROWS=1
MAGMUX_FLOOR_ROWS=$(( MAGMUX_MIN_PANE_ROWS + MAGMUX_STATUS_ROWS ))

# ── THE ESCAPE HATCH: magmux WITHOUT a tmux pane (RC_DEMO_GEOM) ────────────
#
# *** THIS IS THE EXCEPTION, NEVER THE DEFAULT, AND IT IS NEVER CHOSEN FOR YOU.
# *** The demo is a SPLIT TERMINAL. magmux runs in a tmux pane at the real size
# *** of the real window, which is what the block above measures and what the
# *** proportions below hand it.
#
# It exists for one case: a terminal genuinely too small to hold a pane worth
# looking at, where no split can help — 80 columns is 80 columns however it is
# divided. magmux supports the answer directly (CLAUDE.md, "Headless
# invariants"): `--headless` is FORCED when stdin is not a tty, and a headless
# run takes its geometry from COLUMNS/LINES, else 80x24. So magmux can be
# started as an ordinary background process with `</dev/null` at a size of its
# own, and there is then no magmux pane at all — the tmux panes are the driver
# and, if asked for, the mirror.
#
#   unset / off           the DEFAULT: magmux in a tmux pane, at the real size
#   RC_DEMO_GEOM=120x36   run headless at exactly that size
#   RC_DEMO_GEOM=on       run headless at the default size below
#
# THE TRADE, and it is why this is not the default: with no magmux pane there is
# nothing to type into by hand. The driver's menu (item 1) is how the session is
# driven, and the banner and the README say so rather than leaving it to be
# discovered.
#
# VALIDATION IS MANDATORY, because magmux's own failure mode here is SILENT.
# `headlessSize` (mux/mux.go:448) ignores a COLUMNS/LINES pair that is zero,
# negative, unparseable, over 10000 a side or over 4,000,000 cells, and uses
# 80x24 with a note on a stderr this launcher has redirected to a file — so an
# out-of-bounds request would look exactly like a demo that ignored it. The
# bounds below are the DEMO'S and sit well inside magmux's: a keyframe is every
# row on the wire, and 500x200 is already ~200 KB of JSON per resync.
#
# IT NAMES THE PANE, NOT MAGMUX'S TERMINAL. `RC_DEMO_GEOM=120x36` means the
# watched pane — the thing the browser draws and the watch reply reports — is
# 120x36, so magmux is given one row MORE for the status bar it reserves before
# it lays a single pane out (statusRowsLocked, mux/chrome.go:102). It is the
# same +1 the mirror pane gets for the same reason, stated once here: a knob
# whose number is a row off what the chrome then reads looks like a bug in the
# thing it configures.
GEOM_MIN_COLS="$MAGMUX_MIN_PANE_COLS"   # minPaneCols; magmux refuses less
GEOM_MAX_COLS=500
GEOM_MIN_ROWS="$MAGMUX_MIN_PANE_ROWS"   # minPaneRows; the status row is added on
GEOM_MAX_ROWS=200
# What `on`, and the automatic choice, ask for. 120x36 is a comfortable modern
# terminal: wide enough that nothing a shell prints wraps, tall enough that the
# colour card and a few commands are on screen at once.
GEOM_DEFAULT="120x36"
GEOM=0
GEOM_COLS=0
GEOM_ROWS=0
geom_refuse() { # $1 = the value as given, $2 = what is wrong with it
  die "RC_DEMO_GEOM=$1 — $2.
       It must be COLSxROWS, with $GEOM_MIN_COLS <= COLS <= $GEOM_MAX_COLS and $GEOM_MIN_ROWS <= ROWS <= $GEOM_MAX_ROWS
       (the low end is magmux's own floor — minPaneCols $MAGMUX_MIN_PANE_COLS, minPaneRows $MAGMUX_MIN_PANE_ROWS — and
       the high end is this demo's, well inside magmux's
       4,000,000-cell cap, because a keyframe is every row on the wire).
       'on' asks for $GEOM_DEFAULT; unset — the default — gives magmux a tmux
       pane at the real size of this window, which is what the demo is for.
       Refused here rather than passed on: magmux IGNORES an out-of-range
       COLUMNS/LINES and uses 80x24 with a note on a stderr this launcher
       redirects to a file, so a bad value would look like a demo that ignored
       you. Nothing has been started, so there is nothing to clean up."
}
case "${RC_DEMO_GEOM:-off}" in
  # UNSET IS OFF, and there is deliberately no `auto`: a launcher that chose
  # synthetic geometry on its own — at some column threshold — would silently
  # change the demo's shape depending on the window it was started in, take away
  # the pane a person types into, and be irreproducible from the command that
  # started it. The split terminal is the demo; this is the exception, asked for
  # by name.
  off|0|no|none|auto|"")
    ;;
  on|1|yes|true)
    GEOM=1
    GEOM_COLS="${GEOM_DEFAULT%x*}"
    GEOM_ROWS="${GEOM_DEFAULT#*x}"
    ;;
  *)
    _g="$RC_DEMO_GEOM"
    case "$_g" in
      *x*) ;;
      *) geom_refuse "$_g" "that is not a COLSxROWS pair" ;;
    esac
    GEOM_COLS="${_g%%x*}"
    GEOM_ROWS="${_g#*x}"
    case "$GEOM_COLS" in ''|*[!0-9]*) geom_refuse "$_g" "the column count is not a number" ;; esac
    case "$GEOM_ROWS" in ''|*[!0-9]*) geom_refuse "$_g" "the row count is not a number" ;; esac
    # Leading zeros and 64-bit arithmetic both behave here because the two
    # halves are known to be digits by now.
    [ "$GEOM_COLS" -ge "$GEOM_MIN_COLS" ] 2>/dev/null || geom_refuse "$_g" "$GEOM_COLS columns is below the floor"
    [ "$GEOM_COLS" -le "$GEOM_MAX_COLS" ] 2>/dev/null || geom_refuse "$_g" "$GEOM_COLS columns is over the ceiling"
    [ "$GEOM_ROWS" -ge "$GEOM_MIN_ROWS" ] 2>/dev/null || geom_refuse "$_g" "$GEOM_ROWS rows is below the floor"
    [ "$GEOM_ROWS" -le "$GEOM_MAX_ROWS" ] 2>/dev/null || geom_refuse "$_g" "$GEOM_ROWS rows is over the ceiling"
    GEOM=1
    ;;
esac

# ── THE ARITHMETIC, so the next person can check it rather than trust it ───
#
# TWO PANES — THE DEFAULT — with R rows to share and one divider:
#
#     driver  D = 14 rows          the compact menu, its prompt and one answer
#     magmux  A = R - 1 - D        EVERYTHING ELSE. On 50 rows that is 35.
#
#   smallest comfortable window = D + 1 + MIN = 14 + 1 + 8 = 23 rows.
#
# THREE PANES (RC_DEMO_MIRROR=1), two dividers:
#
#     mirror  M = A + 1            ONE ROW TALLER THAN MAGMUX, deliberately:
#                                  it spends a row on its own status bar and
#                                  magmux spends one on its own, so equal panes
#                                  leave the mirror a row short of the screen it
#                                  is mirroring — and it then CLIPS and says so
#                                  in red for the whole life of the demo (R-2)
#     magmux  A = (R - 2 - D - 1) / 2
#
#   smallest comfortable window = D + 2 + MIN + (MIN + 1) = 33 rows, and on 50
#   rows magmux gets 16 — which is the whole reason the mirror is now opt-in.
#
# SYNTHETIC GEOMETRY (RC_DEMO_GEOM) has no magmux pane, so magmux is not in the
# budget at all:
#
#     adopt, no mirror   nothing is split; the driver IS the operator's pane
#     own,   no mirror   one pane, the driver, in a session of its own
#     + mirror           M = MIN + 1 above the driver, one divider — and the
#                        mirror CLIPS a screen bigger than its pane, which it
#                        says in its own status bar
#
# MIN_MAGMUX_ROWS (8) is the COMFORT floor — below it the colour card is not
# worth looking at — and it is comfortably above MAGMUX_FLOOR_ROWS (4), which is
# where magmux itself refuses. Both are used: the comfort floor decides WHICH
# layout to build, and magmux's floor is the thing no built layout may violate.
#
# `windows` subdivides nothing: every surface is a window of the full terminal,
# so the only budget there is magmux's own.
if [ "$LAYOUT" = "windows" ]; then STACKED=0; else STACKED=1; fi

need_rows() { # $1 = 1 if the mirror pane is wanted; echoes the rows that layout needs
  local n="$MIN_MAGMUX_ROWS"
  if [ "$GEOM" = "1" ]; then
    # magmux is not on this screen. The driver's pane is all that is always
    # needed, and in adopt mode even that is the operator's own pane rather than
    # anything this launcher creates.
    n=0
    if [ "$STACKED" = "1" ] && [ "$1" = "1" ]; then n=$(( DRIVER_ROWS + 1 + MIN_MAGMUX_ROWS + 1 )); fi
    printf '%s' "$n"
    return
  fi
  if [ "$STACKED" = "1" ]; then
    n=$(( n + DRIVER_ROWS + 1 ))
    if [ "$1" = "1" ]; then n=$(( n + MIN_MAGMUX_ROWS + 1 + 1 )); fi
  fi
  printf '%s' "$n"
}
NEED_3="$(need_rows 1)"
NEED_2="$(need_rows 0)"
# The documented escape hatch, and it now moves a floor rather than silencing a
# warning: it replaces the requirement the launcher tests the window against.
if [ -n "${RC_DEMO_MIN_ROWS:-}" ]; then
  NEED_3="$RC_DEMO_MIN_ROWS"
  if [ "$NEED_2" -gt "$NEED_3" ]; then NEED_2="$NEED_3"; fi
fi

# A NAMED SHAPE IS NOT OVERRIDDEN; A REQUESTED SURFACE IS.
#
# `RC_DEMO_LAYOUT=panes` names a SHAPE — three stacked panes — and is built as
# named, with a warning that magmux may refuse it. That is batch 6's rule and it
# is unchanged: an operator who spells out the layout gets it rather than a
# quiet substitution, which is what makes the misattribution guard meaningful.
#
# `RC_DEMO_MIRROR=1` is different now that the mirror is opt-in: it asks for a
# SURFACE, not a geometry, and on a window that cannot hold it the honest answer
# is to say so and drop it — the browser tab is the second client either way, so
# nothing about the claim is lost, whereas building it anyway would take the
# rows out of magmux, which is the one thing this demo is about. Anyone who
# wants it built regardless has `RC_DEMO_LAYOUT=panes RC_DEMO_MIRROR=1`, which
# names the shape, or `RC_DEMO_MIN_ROWS=N`, which moves the floor.
FORCED_SHAPE=0
if [ "$WANT_MIRROR" = "1" ] && [ "${RC_DEMO_LAYOUT+set}" = "set" ] && [ "$LAYOUT" = "panes" ]; then FORCED_SHAPE=1; fi

if [ "$COLS" -lt "$MAGMUX_MIN_PANE_COLS" ]; then
  if [ "$GEOM" = "1" ]; then
    # magmux's own columns are synthetic here and are already validated, so this
    # is about the surfaces that ARE in this terminal: the driver's menu and,
    # if asked for, the mirror.
    die "this space is $COLS columns, and the driver's menu needs at least $MAGMUX_MIN_PANE_COLS to be
       readable at all. magmux itself would be fine — RC_DEMO_GEOM gives it ${GEOM_COLS}x${GEOM_ROWS}
       of its own — but there would be nothing usable to drive it from.
       Nothing has been started, so there is nothing to clean up.
       Make the terminal wider, or set RC_DEMO_COLS."
  fi
  die "this space is $COLS columns and magmux needs at least $MAGMUX_MIN_PANE_COLS for one pane
       (minPaneCols, mux/dynpanes.go:17) — below that magmux refuses the layout and exits.
       Nothing has been started, so there is nothing to clean up.
       Make the terminal wider, set RC_DEMO_COLS, or take magmux out of this
       terminal altogether with RC_DEMO_GEOM=${GEOM_DEFAULT}."
fi

NEED="$NEED_3"
if [ "$WANT_MIRROR" = "0" ]; then NEED="$NEED_2"; fi
if [ "$ROWS" -lt "$NEED" ]; then
  if [ "$FORCED_SHAPE" = "1" ]; then
    warn "$ROWS rows, and the layout you asked for by name wants $NEED. Building it anyway."
    warn "  MAGMUX MAY REFUSE IT: its pane needs $MAGMUX_FLOOR_ROWS rows (minPaneRows $MAGMUX_MIN_PANE_ROWS + its status row)"
    warn "  and it exits rather than clamping. If that happens this launcher stops and"
    warn "  prints magmux's own reason — unset RC_DEMO_LAYOUT/RC_DEMO_MIRROR to let it degrade."
  elif [ "$WANT_MIRROR" = "1" ] && [ "$ROWS" -ge "$NEED_2" ]; then
    # THE DEGRADE, and it is the common path on a short window. The browser tab
    # is a second, independent client painting the same frames through the same
    # decoder, so the demo's claim — one hub, two surfaces — survives the mirror
    # pane going. Said as a decision, because it is one.
    WANT_MIRROR=0
    NEED="$NEED_2"
    note "$ROWS rows: three panes want $NEED_3, so this run builds TWO — magmux on top, the"
    note "  driver below — and drops the terminal mirror, which is the default shape anyway."
    note "  The browser tab is the second client, so one hub still drives two surfaces."
    note "  A ${NEED_3}-row window gets the mirror pane back; RC_DEMO_LAYOUT=panes builds it here."
  else
    die "this space is $ROWS rows and the smallest layout needs $NEED_2.
       two panes    magmux $MIN_MAGMUX_ROWS + a divider + driver $DRIVER_ROWS             = $NEED_2 rows
       three panes  and the mirror ($(( MIN_MAGMUX_ROWS + 1 ))) + a divider on top of that = $NEED_3 rows
       magmux itself refuses a pane under $MAGMUX_FLOOR_ROWS rows or $MAGMUX_MIN_PANE_COLS columns (minPaneRows +
       its status row, mux/dynpanes.go:16) and EXITS, so a layout that short is
       not built here rather than being built and refused three steps later.
       Nothing has been started, so there is nothing to clean up.
       Fixes: RC_DEMO_GEOM=${GEOM_DEFAULT}, which takes magmux out of this window
       entirely and leaves only the driver in it; a taller window; or a smaller
       menu with RC_DEMO_DRIVER_ROWS=N."
  fi
fi

# The actual split sizes, from the same arithmetic. The clamps below are only
# ever reached under an explicit override — the decision above has refused or
# degraded every other shape — and they exist so tmux is never handed a `-l 0`,
# which is an error rather than a picture.
D_ROWS="$DRIVER_ROWS"
if [ "$GEOM" = "1" ]; then
  # No magmux pane, so there is no A. The mirror — if it was asked for — takes
  # the rows above the driver and the driver keeps the rest, which in adopt mode
  # is the operator's own pane shrinking by that much. A_ROWS stays 0 and is
  # never passed to a split; the final assertion below skips it for the same
  # reason.
  A_ROWS=0
  if [ "$WANT_MIRROR" = "1" ]; then
    M_ROWS=$(( ROWS - 1 - D_ROWS ))
    # Never taller than the screen it mirrors plus its own status row: a mirror
    # pane with blank rows under the picture is height taken from the driver for
    # nothing.
    if [ "$M_ROWS" -gt $(( GEOM_ROWS + 1 )) ]; then M_ROWS=$(( GEOM_ROWS + 1 )); fi
    if [ "$M_ROWS" -lt "$MIN_MAGMUX_ROWS" ]; then M_ROWS="$MIN_MAGMUX_ROWS"; fi
    D_ROWS=$(( ROWS - 1 - M_ROWS ))
    if [ "$D_ROWS" -lt 1 ]; then D_ROWS=1; fi
  else
    M_ROWS=0
  fi
elif [ "$STACKED" = "0" ]; then
  # Nothing is subdivided: each surface is a window of the whole terminal, and
  # these three numbers are never passed to a split.
  A_ROWS="$ROWS"
  M_ROWS="$ROWS"
elif [ "$WANT_MIRROR" = "1" ]; then
  rest=$(( ROWS - 2 - D_ROWS ))
  if [ "$rest" -lt 6 ]; then
    rest=6
    D_ROWS=$(( ROWS - 2 - rest ))
  fi
  A_ROWS=$(( (rest - 1) / 2 ))
  M_ROWS=$(( A_ROWS + 1 ))
  # THE SPARE ROW GOES TO THE DRIVER, and that is what keeps `M = A + 1` exact.
  # `rest` is even half the time, and handing the remainder to magmux instead
  # would make the mirror the same height as the pane it is mirroring — one row
  # short once its own status bar is drawn, which is the permanent red `CLIPPED`
  # banner of risk R-2.
  D_ROWS=$(( D_ROWS + rest - A_ROWS - M_ROWS ))
else
  A_ROWS=$(( ROWS - 1 - D_ROWS ))
  if [ "$A_ROWS" -lt "$MAGMUX_FLOOR_ROWS" ]; then
    A_ROWS="$MAGMUX_FLOOR_ROWS"
    D_ROWS=$(( ROWS - 1 - A_ROWS ))
  fi
  M_ROWS=0
fi

# EVERY PANE OF A BUILT LAYOUT CLEARS MAGMUX'S OWN FLOOR. This is the assertion
# the decision above exists to satisfy, stated on the FINAL numbers rather than
# on the inputs, so no future clamp, knob or rounding can slip underneath it.
#
# Under RC_DEMO_GEOM there is no magmux pane to clear anything: magmux's size is
# GEOM_COLS x GEOM_ROWS, validated against the same two floors up where it was
# parsed, and A_ROWS is 0 by construction rather than by shortfall.
if [ "$GEOM" = "1" ]; then
  if [ "$D_ROWS" -lt 1 ]; then
    die "the layout leaves the driver $D_ROWS rows in $ROWS. Nothing has been started, so there is
       nothing to clean up. Drop the mirror (unset RC_DEMO_MIRROR), lower
       RC_DEMO_DRIVER_ROWS (now $DRIVER_ROWS), or use a taller window."
  fi
elif [ "$A_ROWS" -lt "$MAGMUX_FLOOR_ROWS" ] || [ "$D_ROWS" -lt 1 ]; then
  if [ "$FORCED_SHAPE" = "1" ]; then
    warn "this layout leaves magmux $A_ROWS rows and it needs $MAGMUX_FLOOR_ROWS. You asked for it by name, so"
    warn "  it is being built — expect magmux to refuse the layout and exit, with its own"
    warn "  reason printed here."
  else
    die "the layout works out at magmux $A_ROWS rows and driver $D_ROWS in $ROWS — and magmux refuses
       anything under $MAGMUX_FLOOR_ROWS rows for a pane (minPaneRows $MAGMUX_MIN_PANE_ROWS + its status row).
       Nothing has been started, so there is nothing to clean up.
       Make the window taller, or lower RC_DEMO_DRIVER_ROWS (now $DRIVER_ROWS)."
  fi
fi

# The driver adapts its "where to look" hints to where it actually is; telling
# somebody to look at the pane above when the mirror is switched off is the one
# kind of instruction worse than none.
if [ "$WANT_MIRROR" = "0" ]; then
  MIRROR_PLACE="none"
elif [ "$LAYOUT" = "windows" ]; then
  MIRROR_PLACE="window"
else
  MIRROR_PLACE="pane"
fi

# SAID OUT LOUD, BEFORE ANYTHING STARTS. This shape has no magmux pane, so
# there is nothing to type into by hand — that is a real trade and not a detail,
# and it is said here as well as in the banner because the banner is the last
# thing printed and this is a decision the operator already made.
if [ "$GEOM" = "1" ]; then
  warn "RC_DEMO_GEOM=${GEOM_COLS}x${GEOM_ROWS} — magmux runs HEADLESS at that size and gets NO tmux pane."
  warn "  NOTHING TO TYPE INTO BY HAND: the driver's menu (item 1) is how this session is driven."
  warn "  Unset RC_DEMO_GEOM for the normal shape: magmux in a pane of this window, ${COLS} columns wide."
fi

# ── 3. a fresh state directory ──────────────────────────────────────────────
# Wiped, not merged: a stale magmux.url from a previous run would be served to
# the page as if it were this run's, and the browser would connect to a port
# nothing is listening on and report an auth failure.
rm -rf "$STATE"
mkdir -p "$STATE" || die "cannot create $STATE"

# Everything stop.sh needs to finish this job if the launcher is killed hard.
#
# Re-written after every pane this demo creates, because a pane that exists and
# is not written down is a pane nothing will ever remove. `kill -9` is the case
# stop.sh exists for, so the file has to be true at every instant, not at the
# end.
#
# The SERVER INCARNATION (`#{pid}`) is in here for one reason: pane and window
# ids restart at 0 when a tmux server restarts, so an id from a previous server
# names a live pane belonging to somebody else. stop.sh refuses to act when the
# pid does not match, which is the same two-fact rule `kill_demo_panes` uses.
write_tmux_state() {
  {
    printf 'mode=%s\n' "$TMUX_MODE"
    printf 'panes=%s\n' "${DEMO_PANES[*]:-}"
    printf 'mark_option=%s\n' "$MARK_OPT"
    printf 'mark=%s\n' "$ID"
    printf 'socket_path=%s\n' "$(tm display-message -p '#{socket_path}' 2>/dev/null)"
    printf 'server_pid=%s\n' "$(tm display-message -p '#{pid}' 2>/dev/null)"
    printf 'session=%s\n' "$SESSION"
    printf 'socket_label=%s\n' "$TMUX_SOCK"
  } >"$TMUX_STATE"
}

# Stamp a pane as this run's, so that teardown — here or in stop.sh — can prove
# a pane is ours before killing it.
mark_pane() { # $1 = pane id
  tm set-option -p -t "$1" "$MARK_OPT" "$ID" >/dev/null 2>&1
}

# Kill the panes this launcher created in the OPERATOR'S window, and refuse to
# kill anything else.
#
# Two facts must agree: the `#{pane_id}` captured at creation, and the pane
# option `mark_pane` set on it. An id alone is not enough across a tmux server
# restart, and killing somebody's editor because an id was recycled is the worst
# bug this file could have. The operator's own pane is never in the list — it is
# not ours, we only borrowed room from it, and it goes back to full height by
# itself the moment ours are gone.
kill_demo_panes() {
  local p mark
  for p in "${DEMO_PANES[@]:-}"; do
    [ -n "$p" ] || continue
    mark="$(tm display-message -p -t "$p" "#{$MARK_OPT}" 2>/dev/null)" || continue
    [ "$mark" = "$ID" ] || continue
    tm kill-pane -t "$p" 2>/dev/null
  done
}

# ── teardown: the ONLY place anything is killed ─────────────────────────────
cleanup() {
  trap - EXIT INT TERM
  [ -n "$SERVE_PID" ] && kill "$SERVE_PID" 2>/dev/null
  # Under RC_DEMO_GEOM magmux is a CHILD OF THIS PROCESS rather than a pane, so
  # nothing else can reap it: no tmux pane dies with the session, no
  # `kill-server` reaches it. `server.sh` execs, so this pid is magmux's own and
  # a TERM lands on the thing it was aimed at — magmux then takes its panes'
  # children with it. Killed BEFORE the state directory goes, so its socket and
  # token are removed by the same `rm -rf` that always removed them.
  [ -n "$MAGMUX_PID" ] && kill "$MAGMUX_PID" 2>/dev/null
  if [ "$TMUX_MODE" = "own" ]; then
    tm kill-session -t "$SESSION" 2>/dev/null
    # …and the private server with it, so not even an idle tmux daemon survives.
    tm kill-server 2>/dev/null
  else
    # A GUEST. Our panes and nothing else: no kill-window, no kill-session, no
    # kill-server, and no option left set on anything of the operator's.
    kill_demo_panes
  fi
  # Both token files, by name, before the directory goes: the view token is a
  # live feed of a shell and magmux's own session token can type into one.
  rm -f "$VIEW_TOKEN" "$SESSION_TOKEN"
  if [ "${RC_DEMO_KEEP:-0}" = "1" ]; then
    note "RC_DEMO_KEEP=1 — leaving $STATE (tokens removed)"
  else
    rm -rf "$STATE"
  fi
}
trap cleanup EXIT INT TERM

# ── 4. the static server ────────────────────────────────────────────────────
# First, because magmux's --allow-origin must name its port. It mints the view
# token at 0600, builds the browser bundle from the SHARED client source, binds,
# and prints exactly one `ready {…}` line — in that order, which is magmux's own
# security order: an accepting port implies a final token file.
: >"$SERVE_LOG"
# `--id` is what lets serve.ts find magmux's SESSION token — `$STATE/magmux-$ID.token`
# — for the browser page's driver panel. It reads that file PER REQUEST and it
# does not exist yet: magmux has not started. RC_DEMO_WEB_DRIVER=0 is inherited
# from this launcher's environment and removes the endpoint and the panel; see
# serve.ts's header for the escalation the panel is.
"$BUN_BIN" "$ROOT/demo/rc/serve.ts" --state "$STATE" --id "$ID" >"$SERVE_LOG" 2>&1 &
SERVE_PID=$!
printf '%s\n' "$SERVE_PID" >"$SERVE_PIDFILE"

# ── 5. wait for it, with a deadline ────────────────────────────────────────
wait_for_line() { # $1 = file, $2 = ERE, $3 = seconds
  local deadline=$(( $(date +%s) + $3 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    grep -Eq "$2" "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

if ! wait_for_line "$SERVE_LOG" '^ready \{' "$READY_DEADLINE"; then
  say ""
  say "${R}demo:rc: the static server never became ready within ${READY_DEADLINE}s.${N}"
  say "${B}serve.log${N}"
  sed 's/^/  /' "$SERVE_LOG" >&2
  exit 1
fi

# The ready line is JSON, so it is parsed as JSON. bun is already a hard
# requirement, and a sed over a credential-bearing line is how a config path
# quietly becomes half a config path.
ready_field() { # $1 = field name
  "$BUN_BIN" -e 'const t = await Bun.file(process.argv[1]).text();
const m = t.match(/^ready (\{.*\})$/m);
if (!m) process.exit(1);
const v = JSON.parse(m[1])[process.argv[2]];
if (v === undefined || v === null) process.exit(1);
process.stdout.write(String(v));' "$SERVE_LOG" "$1"
}

WEB_PORT="$(ready_field port)"   || die "the ready line carries no port:$(sed 's/^/\n  /' "$SERVE_LOG")"
CONFIG_PATH="$(ready_field configPath)" || die "the ready line carries no configPath:$(sed 's/^/\n  /' "$SERVE_LOG")"
WEB_ORIGIN="http://127.0.0.1:$WEB_PORT"
# The same string `serve.ts` puts in its own `url` field; built here from the
# two parsed fields so the launcher never opens a URL it did not derive.
PAGE_URL="$WEB_ORIGIN/?c=$CONFIG_PATH"

note "static server on $WEB_ORIGIN (pid $SERVE_PID)"

# The three variables `server.sh` needs, handed to the pane with `tmux -e`
# rather than exported.
#
# An export reaches a child of THIS process, and in adopt mode the pane's parent
# is not this process — it is a tmux server started long before the demo, with
# the operator's environment, which inherits nothing of ours. `server.sh` would
# exit with "RC_DEMO_STATE is not set" and the pane would show that instead of
# magmux. `-e` sets the variable on the pane itself and is true in both modes
# (tmux 3.0, which the preflight above already requires). PATH rides along for
# the same reason: the `bun` in the `--plugin` command is resolved inside that
# pane, by whatever PATH it was given.
PANE_ENV=(
  -e "RC_DEMO_STATE=$STATE"
  -e "RC_DEMO_ID=$ID"
  -e "RC_DEMO_WEB_PORT=$WEB_PORT"
  -e "RC_DEMO_MIRROR_PLACE=$MIRROR_PLACE"
  -e "PATH=$PATH"
)

# ── 6. magmux's own surface ────────────────────────────────────────────────
#
# Every target below is a PANE ID (`%7`), captured with `-P -F '#{pane_id}'` at
# creation, and never a `session:window.pane` index. The operator's ~/.tmux.conf
# is read even by a `-L` server, and `base-index 1` / `pane-base-index 1` are
# common enough to be a coin toss — with them set, `$SESSION:0.0` is "can't find
# window: 0" and the demo dies at the split. A pane id is assigned by the
# server, is stable for the pane's life, and survives renumbering.
#
# THE SIZE IS THE REAL WINDOW'S, AND IT IS APPLIED AT CREATION. `new-session -d`
# has no client to take a size from and defaults to 80x24, so a session built
# without `-x`/`-y` gives magmux an 80-column pane that a later attach reflows —
# and everything watching it, the browser included, is shown an 80-column
# terminal because the SOURCE was 80 columns. `-x "$COLS" -y "$ROWS"` is the
# measured terminal, so the panes are laid out at the right size the first time.
PANE_A=""
if [ "$GEOM" = "1" ]; then
  # THE ESCAPE HATCH, asked for by name: no pane at all. magmux is an ordinary
  # background child of this launcher with `</dev/null`, which is what forces
  # `--headless` (CLAUDE.md: headless is written in init() when stdin is not a
  # tty), and COLUMNS/LINES are its geometry. `server.sh` needs its three
  # variables in the ENVIRONMENT here rather than on a tmux pane, so they are
  # passed with `env` — one place, same three names, so the two paths cannot
  # drift. Its own pre-exec errors go to magmux.out; magmux's own stderr still
  # goes to magmux.err, which is the readiness channel either way.
  # LINES is the PANE's rows plus the status row magmux reserves, so the watched
  # pane comes out at exactly the size that was asked for.
  env RC_DEMO_STATE="$STATE" RC_DEMO_ID="$ID" RC_DEMO_WEB_PORT="$WEB_PORT" \
      COLUMNS="$GEOM_COLS" LINES="$(( GEOM_ROWS + MAGMUX_STATUS_ROWS ))" \
      "$ROOT/demo/rc/server.sh" </dev/null >"$STATE/magmux.out" 2>&1 &
  MAGMUX_PID=$!
  note "magmux started headless, pane ${GEOM_COLS}x${GEOM_ROWS} (pid $MAGMUX_PID) — no tmux pane"
elif [ "$TMUX_MODE" = "own" ]; then
  tm kill-session -t "$SESSION" 2>/dev/null
  PANE_A="$(tm new-session -d -s "$SESSION" -n magmux -c "$ROOT" \
    -x "$COLS" -y "$ROWS" "${PANE_ENV[@]}" -P -F '#{pane_id}' "$ROOT/demo/rc/server.sh")" \
    || die "tmux could not create the session"
  [ -n "$PANE_A" ] || die "tmux created the session but named no pane"
else
  # ABOVE the operator's pane (`-b`), and WITHOUT taking focus (`-d`): the pane
  # they typed in is already theirs and stealing the cursor from under them is
  # the rudest thing a guest can do. `-l $A_ROWS` sizes the NEW pane, and
  # A_ROWS is everything the driver and the optional mirror do not need.
  PANE_A="$(tm split-window -b -v -d -t "$TMUX_PANE" -l "$A_ROWS" -c "$ROOT" \
    "${PANE_ENV[@]}" -P -F '#{pane_id}' "$ROOT/demo/rc/server.sh")" \
    || die "tmux could not split your pane for magmux"
  [ -n "$PANE_A" ] || die "tmux split the pane but named no id"
fi
if [ -n "$PANE_A" ]; then
  DEMO_PANES+=("$PANE_A")
  mark_pane "$PANE_A"
  write_tmux_state

  # `remain-on-exit on` for every pane: a crash STAYS on the screen with its exit
  # status instead of the pane vanishing. That is what makes the demo's failure
  # behaviour visible rather than merely true — a pane that disappears is
  # indistinguishable from one that was never created. `-p` in adopt mode, so the
  # option lands on OUR pane and not on the operator's whole window.
  if [ "$TMUX_MODE" = "own" ]; then
    tm set-option -w -t "$PANE_A" remain-on-exit on >/dev/null
  else
    tm set-option -p -t "$PANE_A" remain-on-exit on >/dev/null 2>&1
  fi
fi

if [ "$TMUX_MODE" = "own" ] && [ -n "$PANE_A" ]; then
  # *** MEASURED, AND THE FAILURE IS INVISIBLE IN CODE REVIEW ***
  #
  # `set-option destroy-unattached on` beside `new-session -d` destroys the
  # session AND the whole tmux server within about three seconds — before
  # anything has attached. On tmux 3.7c:
  #
  #     $ tmux -L probe new-session -d -s p 'sleep 12'
  #     $ tmux -L probe set-option -t p destroy-unattached on
  #     $ sleep 3; tmux -L probe list-sessions
  #     no server running on /private/tmp/tmux-501/probe
  #
  # Every step below — the readiness poll, the split, opening the browser — runs
  # inside that window, so the demo would have died on its first run and the
  # corpse would have read as "magmux crashed" while magmux.err showed a
  # perfectly healthy startup. Setting it from a hook that fires only once a
  # CLIENT HAS ATTACHED keeps the property the option exists for (a `kill -9` of
  # this launcher still cannot leave a session behind) and none of the damage.
  #
  # IT IS SCOPED TO THIS MODE AND MUST STAY THERE. In adopt mode the session is
  # the operator's, and this option would destroy their work the moment they
  # detached from it.
  tm set-hook -t "$SESSION" client-attached 'set-option destroy-unattached on' >/dev/null
fi

# ── 7. wait for magmux to announce its port, and CHECK IT IS STILL THERE ───
#
# READINESS MUST MEAN "IT IS UP", NOT "IT WAS UP FOR A MOMENT". magmux binds
# before it builds its layout, so `listening on` is a promise about tokens and a
# port and about nothing that happens afterwards: a layout magmux refuses — a
# pane under minPaneRows — makes it print its reason and exit MILLISECONDS after
# that line, taking the generated token file with it (it is documented as
# removed at exit). Everything downstream then fails at whatever it touches
# first, which is the token, and the operator is sent to look at credentials and
# file modes with the real answer sitting unread in $STATE/magmux.err.
#
# So the liveness of the process is asked TWICE — here, and again immediately
# before the driver, which is the consumer that needs the token — and when it is
# gone the answer printed is magmux's OWN, verbatim.
magmux_gone() { # 0 = magmux has exited
  local dead
  if [ "$GEOM" = "1" ]; then
    # No pane to ask about: magmux is this process's own child, and `kill -0` is
    # the same question tmux's `#{pane_dead}` answers below. A reaped child
    # answers "no such process", which is also gone.
    kill -0 "$MAGMUX_PID" 2>/dev/null || return 0
    [ -s "$SESSION_TOKEN" ] || return 0
    return 1
  fi
  # `remain-on-exit on` keeps a dead pane on screen with its status, so tmux
  # still answers for it; a pane that is not there at all answers nothing, and
  # both of those are "gone".
  dead="$(tm display-message -p -t "$PANE_A" '#{pane_dead}' 2>/dev/null)"
  [ -z "$dead" ] && return 0
  [ "$dead" = "1" ] && return 0
  # The second, independent fact: magmux writes its token BEFORE it binds and
  # removes it at exit, so a missing token beside a live-looking pane is a
  # magmux that has gone (and is exactly the file the driver reads next).
  [ -s "$SESSION_TOKEN" ] || return 0
  return 1
}

require_magmux_alive() { # $1 = when, in words, for the first line
  # ONE settle. The refusal this exists for happens microseconds after the line
  # the readiness poll matched, and tmux needs a moment to notice the process
  # left; 200ms buys a deterministic answer for a launcher that is already
  # waiting on panes, and costs a fifth of a second on a healthy run.
  if magmux_gone; then sleep 0.2; else return 0; fi
  magmux_gone || return 0
  say ""
  say "${R}demo:rc: magmux exited $1.${N}"
  say "${R}  It had already bound its listener — which is what the readiness poll saw — and${N}"
  say "${R}  it removes the token file it wrote on the way out. So the next thing to read${N}"
  say "${R}  that token reports a missing file; that is a SYMPTOM. The cause is below, in${N}"
  say "${R}  magmux's own words.${N}"
  say ""
  say "${B}magmux's own reason ($MAGMUX_ERR)${N}"
  if [ -s "$MAGMUX_ERR" ]; then
    sed 's/^/  /' "$MAGMUX_ERR" >&2
  elif [ "$GEOM" = "1" ] && [ -s "$STATE/magmux.out" ]; then
    # Headless, magmux never got as far as its own stderr redirect, so what
    # there is to say is on server.sh's own output.
    sed 's/^/  /' "$STATE/magmux.out" >&2
  else
    say "  (the file is empty — magmux exited without saying anything)"
  fi
  say ""
  exit 1
}

# The documented readiness contract, read through the line magmux prints to
# announce it: tokens resolved and written, TLS pair loaded, and only THEN the
# bind (mux/remote.go:16-24). An accepting port implies a complete token file,
# which is why the next step can write the URL out without checking anything
# else.
if ! wait_for_line "$MAGMUX_ERR" 'magmux: listening on ' "$READY_DEADLINE"; then
  say ""
  say "${R}demo:rc: magmux never announced a listener within ${READY_DEADLINE}s.${N}"
  say "${B}magmux.err${N}"
  sed 's/^/  /' "$MAGMUX_ERR" 2>/dev/null >&2 || say "  (no $MAGMUX_ERR at all — magmux may not have started)"
  exit 1
fi
MAGMUX_URL="$(sed -nE 's/.*magmux: listening on ([^[:space:]]+).*/\1/p' "$MAGMUX_ERR" | head -1)"
[ -n "$MAGMUX_URL" ] || die "could not read magmux's URL out of $MAGMUX_ERR"
require_magmux_alive "immediately after announcing its listener"

# ── 8. hand magmux's URL to the browser page ───────────────────────────────
# THE CHANNEL. `serve.ts` reads this file on every request to its config
# endpoint and answers 503 until it exists; the page learns magmux's
# kernel-chosen port from it and from nowhere else. Written atomically, because
# the page may be fetching the config at this exact moment and half a URL is
# worse than a 503.
printf '%s\n' "$MAGMUX_URL" >"$URL_FILE.tmp" && mv "$URL_FILE.tmp" "$URL_FILE"
note "magmux on $MAGMUX_URL (url written to $URL_FILE)"

# ── 9. the mirror ──────────────────────────────────────────────────────────
# Created only AFTER magmux is up, so it never opens on a refused connection.
# It holds the READ-ONLY view token — read off disk, never on a command line,
# because a value on a command line is in every `ps` listing on the machine.
PANE_B=""
MIRROR_WIN=""
CLI_CMD="$BUN_BIN $ROOT/demo/rc/cli.ts --url $MAGMUX_URL --token-file $VIEW_TOKEN"

# In own-server mode the session is normally created WITH magmux in it, so by
# here it exists. Under RC_DEMO_GEOM there is no magmux pane and the first pane
# this demo creates is whichever of the mirror and the driver comes first — so
# session creation moves here, once, behind one function. The `client-attached`
# hook rides with it for the reason its own comment gives: set on a detached
# session it destroys the session and the server within about three seconds.
own_new_session() { # $1 = window name, $2 = command; echoes the pane id
  local pid
  tm kill-session -t "$SESSION" 2>/dev/null
  pid="$(tm new-session -d -s "$SESSION" -n "$1" -c "$ROOT" \
    -x "$COLS" -y "$ROWS" "${PANE_ENV[@]}" -P -F '#{pane_id}' "$2")" || return 1
  [ -n "$pid" ] || return 1
  tm set-option -w -t "$pid" remain-on-exit on >/dev/null
  tm set-hook -t "$SESSION" client-attached 'set-option destroy-unattached on' >/dev/null
  printf '%s' "$pid"
}

if [ "$WANT_MIRROR" = "1" ]; then
  if [ "$GEOM" = "1" ] && [ "$TMUX_MODE" = "own" ]; then
    # No magmux pane to split: the mirror IS the session's first pane, and the
    # driver splits it below.
    PANE_B="$(own_new_session viewer "$CLI_CMD")" || die "tmux could not create the session for the mirror"
  elif [ "$TMUX_MODE" = "adopt" ]; then
    # Above the driver — i.e. between magmux and the operator's own pane —
    # again without taking focus.
    PANE_B="$(tm split-window -b -v -d -t "$TMUX_PANE" -l "$M_ROWS" -c "$ROOT" \
      -P -F '#{pane_id}' "$CLI_CMD")" || die "tmux could not split your pane for the mirror"
    tm set-option -p -t "$PANE_B" remain-on-exit on >/dev/null 2>&1
  elif [ "$LAYOUT" = "windows" ]; then
    # The literal separate-terminals reading, for anyone who asks for it.
    PANE_B="$(tm new-window -t "$SESSION" -n viewer -c "$ROOT" \
      -P -F '#{pane_id}' "$CLI_CMD")" || die "tmux could not create the viewer window"
    tm set-option -w -t "$PANE_B" remain-on-exit on >/dev/null
  else
    PANE_B="$(tm split-window -v -t "$PANE_A" -l "$(( M_ROWS + D_ROWS + 1 ))" -c "$ROOT" \
      -P -F '#{pane_id}' "$CLI_CMD")" || die "tmux could not split the window"
  fi
  [ -n "$PANE_B" ] || die "tmux created the viewer but named no pane"
  DEMO_PANES+=("$PANE_B")
  mark_pane "$PANE_B"
  write_tmux_state
  MIRROR_WIN="$(tm display-message -p -t "$PANE_B" '#{window_index}' 2>/dev/null)"
else
  note "no terminal mirror pane (RC_DEMO_MIRROR=1 adds one) — the browser tab is the mirror,"
  note "  and every row it would have cost goes to magmux instead"
fi

# ── 10. the DRIVER ─────────────────────────────────────────────────────────
# The thing a human touches. It holds the full session token and every mirror
# holds the read-only one, so it can type into the session and the surfaces
# watching it provably cannot — which is the contrast the whole demo is for.
#
# In ADOPT mode it is not a pane this script creates at all: it is this process,
# in the operator's own pane, run in the foreground at the bottom of this file.
#
# THE SECOND LIVENESS GATE, and the one that matters most: the driver's first
# act is to read the session token, and the token is removed when magmux exits.
# Between the poll above and this line the mirror pane was created and magmux
# drew its first frames — time enough for a layout refusal, a port conflict or a
# plugin that took magmux down with it. Asked here, the answer is magmux's own
# reason; asked nowhere, the answer is "cannot read the session token", which is
# three steps downstream and points at the wrong thing.
require_magmux_alive "after it came up, before the driver could read its token"

DRIVE_CMD_ARGS=(
  --state "$STATE" --id "$ID" --url "$MAGMUX_URL"
  --mirror "$MIRROR_PLACE" --mirror-key "${MIRROR_WIN:-1}"
  --prefix "$(prefix_label)"
)
PANE_C=""
if [ "$TMUX_MODE" = "own" ]; then
  DRIVE_CMD="$DRIVER_BIN ${DRIVE_CMD_ARGS[*]} --place $DRIVER_PLACE --quit-hint 'Ctrl-C the launcher'"
  if [ -z "$PANE_A" ] && [ -z "$PANE_B" ]; then
    # RC_DEMO_GEOM with no mirror: nothing has been created yet, so the driver
    # is the session's only pane and there is nothing to split.
    PANE_C="$(own_new_session driver "$DRIVE_CMD")" || die "tmux could not create the session for the driver"
  elif [ "$DRIVER_PLACE" = "pane" ] && [ "$LAYOUT" != "windows" ]; then
    PANE_C="$(tm split-window -v -t "${PANE_B:-$PANE_A}" -l "$D_ROWS" -c "$ROOT" \
      -P -F '#{pane_id}' "$DRIVE_CMD")" || die "tmux could not split for the driver"
  else
    PANE_C="$(tm new-window -t "$SESSION" -n driver -c "$ROOT" \
      -P -F '#{pane_id}' "$DRIVE_CMD")" || die "tmux could not create the driver window"
    tm set-option -w -t "$PANE_C" remain-on-exit on >/dev/null
  fi
  [ -n "$PANE_C" ] || die "tmux created the driver but named no pane"
  DEMO_PANES+=("$PANE_C")
  mark_pane "$PANE_C"
  write_tmux_state
  # FOCUS IT. A menu you have to go and find is a menu nobody uses, and the
  # first thing a human should be able to do is press 1. (Adopt mode never does
  # this: the driver is already the pane they are looking at.)
  tm select-window -t "$PANE_C" >/dev/null
  tm select-pane -t "$PANE_C" >/dev/null
fi

# The window numbers are ASKED FOR, never assumed. `base-index 1` in the
# operator's ~/.tmux.conf is read even by a `-L` server and is common enough to
# be a coin toss, so a banner that hardcoded window 2 would send half its
# readers to the wrong window — which is worse than saying nothing, because they
# would conclude the driver had not started.
WIN_A="$(tm display-message -p -t "$PANE_A" '#{window_index}' 2>/dev/null)"
WIN_C="$([ -n "$PANE_C" ] && tm display-message -p -t "$PANE_C" '#{window_index}' 2>/dev/null)"
PREFIX="$(prefix_label)"

# ── 11. the browser ────────────────────────────────────────────────────────
# The URL is PRINTED either way, so a machine with no usable browser (risk R-4)
# still has a working demo one paste away.
if [ "${NO_BROWSER:-0}" = "1" ]; then
  note "NO_BROWSER=1 — not opening a browser"
else
  case "$(uname -s)" in
    Darwin) open "$PAGE_URL" >/dev/null 2>&1 || note "could not open a browser; the URL is below" ;;
    Linux)  command -v xdg-open >/dev/null 2>&1 && { xdg-open "$PAGE_URL" >/dev/null 2>&1 & } || note "no xdg-open; the URL is below" ;;
    *)      note "unknown platform $(uname -s); the URL is below" ;;
  esac
fi

# ── 12. the banner ─────────────────────────────────────────────────────────
printf '\n'
printf '  %smagmux remote control%s — one hub, two mirrors, one public API\n' "$B" "$N"
printf '\n'

if [ "$TMUX_MODE" = "adopt" ]; then
  # Short, because there is nowhere to go and nothing to find: everything it
  # describes is on this screen, and the driver's own menu is about to print
  # underneath it.
  if [ "$GEOM" = "1" ]; then
    printf '    %s(no magmux pane)%s  RC_DEMO_GEOM — magmux is headless at %sx%s, listening on %s\n' "$Y" "$N" "$GEOM_COLS" "$GEOM_ROWS" "$MAGMUX_URL"
  elif [ "$WANT_MIRROR" = "1" ]; then
    printf '    %spane above%s        magmux itself — %s rows of it — listening on %s\n' "$B" "$N" "$A_ROWS" "$MAGMUX_URL"
    printf '    %sand above that%s    a terminal client: WebSocket → the shared decoder → ANSI\n' "$B" "$N"
  else
    printf '    %spane above%s        magmux itself — %sx%s, all the room this window had —\n' "$B" "$N" "$COLS" "$A_ROWS"
    printf '                      listening on %s\n' "$MAGMUX_URL"
    printf '    %s(no mirror pane)%s  the browser tab is the mirror; RC_DEMO_MIRROR=1 adds one here\n' "$G" "$N"
  fi
  printf '    %sbrowser%s           the same decoder, the same core, painting DOM:\n' "$B" "$N"
  printf '                      %s%s%s\n' "$G" "$PAGE_URL" "$N"
  printf '    %sTHIS PANE%s         the DRIVER, starting now. Press a number, then Enter.\n' "$Y$B" "$N"
  printf '\n'
  printf '    The driver holds the %sFULL SESSION TOKEN%s; every mirror holds the\n' "$B" "$N"
  printf '    %sREAD-ONLY%s view token — items 8 and 9 make magmux say so itself, with a\n' "$B" "$N"
  if [ "$GEOM" = "1" ]; then
    printf '    real 403 off the wire. %sThere is no magmux pane to type into by hand%s —\n' "$Y$B" "$N"
    printf '    this menu is how the session is driven. Unset RC_DEMO_GEOM for a pane.\n'
  else
    printf '    real 403 off the wire. You can also type into the magmux pane by hand\n'
    printf '    (%s ↑); nothing in this demo self-drives.\n' "$PREFIX"
  fi
  printf '\n'
  printf '  %sq or Ctrl-C HERE ends the demo and gives this pane back.%s\n' "$Y$B" "$N"
  printf '     %sThe panes above are removed, magmux and the static server stop, and both%s\n' "$Y" "$N"
  printf '     %stokens go with them. Your window, session and tmux server are untouched —%s\n' "$Y" "$N"
  printf '     %sthe demo is a guest here. Detaching does NOT end it.%s\n' "$Y" "$N"
  printf '\n'
  # The stable line the automated check waits for, and the one an operator sees
  # last before the menu.
  printf '  %sdemo:rc: ready — the demo is running in this window%s\n' "$G" "$N"
else
  if [ -n "$PANE_C" ] && [ "$DRIVER_PLACE" = "pane" ] && [ "$LAYOUT" != "windows" ]; then
    printf '    %sBOTTOM PANE — START HERE%s  the DRIVER: a menu of real interactions.\n' "$Y$B" "$N"
    printf '                Press a number, then Enter. It is already focused.\n'
  else
    printf '    %sWINDOW %s (%s %s) — START HERE%s  the DRIVER: a menu of real\n' "$Y$B" "$WIN_C" "$PREFIX" "$WIN_C" "$N"
    printf '                interactions. Press a number, then Enter. It is already focused.\n'
  fi
  printf '\n'
  if [ "$GEOM" = "1" ]; then
    printf '    %s(no magmux pane)%s  RC_DEMO_GEOM — magmux is headless at %sx%s, on %s\n' "$Y" "$N" "$GEOM_COLS" "$GEOM_ROWS" "$MAGMUX_URL"
    [ -n "$PANE_B" ] && printf '    %stop pane%s    a terminal client: WebSocket → the shared decoder → ANSI\n' "$B" "$N"
  elif [ "$LAYOUT" = "windows" ]; then
    printf '    %swindow %s%s    magmux itself, listening on %s\n' "$B" "$WIN_A" "$N" "$MAGMUX_URL"
    [ -n "$PANE_B" ] && printf '    %swindow %s%s    a terminal client: WebSocket → the shared decoder → ANSI\n' "$B" "$MIRROR_WIN" "$N"
  else
    printf '    %stop pane%s    magmux itself — %sx%s — listening on %s\n' "$B" "$N" "$COLS" "$A_ROWS" "$MAGMUX_URL"
    if [ -n "$PANE_B" ]; then
      printf '    %smiddle pane%s a terminal client: WebSocket → the shared decoder → ANSI\n' "$B" "$N"
    else
      printf '    %s(no mirror)%s  the browser tab is the mirror; RC_DEMO_MIRROR=1 adds a pane here\n' "$G" "$N"
    fi
  fi
  printf '    %sbrowser%s     the same decoder, the same core, painting DOM:\n' "$B" "$N"
  printf '                %s%s%s\n' "$G" "$PAGE_URL" "$N"
  printf '\n'
  printf '    The driver holds the %sFULL SESSION TOKEN%s; every mirror holds the\n' "$B" "$N"
  printf '    %sREAD-ONLY%s view token. So the menu can type into the shell and the\n' "$B" "$N"
  printf '    surfaces watching it cannot — menu items 8 and 9 make magmux say so\n'
  if [ "$GEOM" = "1" ]; then
    printf '    itself, with a real 403 off the wire. %sThere is no magmux pane to type\n' "$Y$B"
    printf '    into by hand%s — this menu is how the session is driven.\n' "$N"
  else
    printf '    itself, with a real 403 off the wire. You can also just type into the\n'
    printf '    top pane by hand; nothing in this demo self-drives.\n'
  fi
  printf '\n'
  printf '  %s⚠  %s d DETACHES, AND DETACHING ENDS THE DEMO.%s\n' "$Y$B" "$PREFIX" "$N"
  printf '     %sThis launcher owns the teardown, so when it stops, everything stops:%s\n' "$Y" "$N"
  printf '     %smagmux, the static server, the tmux session and the two tokens.%s\n' "$Y" "$N"
  printf '     %sTo quit deliberately: %s d, or Ctrl-C here. To keep a session%s\n' "$Y" "$PREFIX" "$N"
  printf '     %srunning, this is the wrong tool — run magmux yourself.%s\n' "$Y" "$N"
  printf '\n'
fi

# ── 13. hand over ──────────────────────────────────────────────────────────
if [ "$TMUX_MODE" = "adopt" ]; then
  # THE DRIVER, IN THE FOREGROUND, IN THIS PANE. Not backgrounded: this process
  # owns the teardown, so the driver returning — `q`, EOF on a fed stdin, or
  # Ctrl-C, which reaches both of us — must be the moment the trap runs. One
  # process, one lifetime, one teardown.
  "$DRIVER_BIN" "${DRIVE_CMD_ARGS[@]}" \
    --place pane --quit-hint "Ctrl-C here"
  exit 0
fi

if [ "${NO_ATTACH:-0}" = "1" ]; then
  # The non-interactive shape, and the one the automated evidence uses: build
  # the whole demo, attach nothing, and hold the session open so THIS process
  # still owns the teardown. Everything is inspectable from outside with
  # `tmux -L <socket> capture-pane`.
  printf '  %sNO_ATTACH=1 — holding the session open. Inspect it with:%s\n' "$G" "$N"
  if [ -n "$PANE_A" ]; then
    printf '    %stmux -L %s capture-pane -p -t %s%s   (magmux)\n' "$G" "$TMUX_SOCK" "$PANE_A" "$N"
  else
    printf '    %s(magmux is headless at %sx%s — no pane; watch it over the API instead)%s\n' "$G" "$GEOM_COLS" "$GEOM_ROWS" "$N"
  fi
  [ -n "$PANE_B" ] && printf '    %stmux -L %s capture-pane -p -t %s%s   (the viewer)\n' "$G" "$TMUX_SOCK" "$PANE_B" "$N"
  printf '    %stmux -L %s capture-pane -p -t %s%s   (the driver)\n' "$G" "$TMUX_SOCK" "$PANE_C" "$N"
  printf '    %s%s --state %s --id %s%s   (a driver of your own)\n' "$G" "$DRIVER_BIN" "$STATE" "$ID" "$N"
  printf '    %s%s --state %s --id %s --run 1%s   (one action, as plain text)\n' "$G" "$DRIVER_BIN" "$STATE" "$ID" "$N"
  printf '    %sCtrl-C, or demo/rc/stop.sh, to end it.%s\n\n' "$G" "$N"
  while tm has-session -t "$SESSION" 2>/dev/null; do sleep 0.5; done
  exit 0
fi

# `env -u TMUX` (inside `tm`): attaching to our private server from inside
# somebody else's tmux is legitimate here — it is a different server, not a
# nested session — but tmux refuses on the TMUX variable alone unless it is
# cleared. In practice this branch is only reached with $TMUX unset or
# RC_DEMO_TMUX=own.
tm attach-session -t "$SESSION"
