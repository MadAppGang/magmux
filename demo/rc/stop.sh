#!/usr/bin/env bash
# The documented escape hatch: stop a demo whose launcher is gone.
#
#   demo/rc/stop.sh              stop the default demo (RC_DEMO_ID=rc-demo)
#   RC_DEMO_ID=other …           stop a second one
#
# `demo.sh` owns teardown and does it on EXIT, INT and TERM, so this script is
# for the one case that trap cannot cover: the launcher was `kill -9`'d, or the
# terminal it was in was closed hard. It is IDEMPOTENT and SILENT about nothing
# — it says what it found and what it did — so it is safe to run when nothing is
# up, which is the state it is usually run in by someone who is not sure.
#
# It kills by SESSION NAME, by PANE ID and by PIDFILE, never by pattern: a
# `pkill -f magmux` in a demo script is how somebody loses the magmux they were
# working in.
#
# ── IT IS AS CAREFUL AS THE LAUNCHER, AND IN ADOPT MODE MORE SO ────────────
#
# `demo.sh` leaves `$STATE/tmux.state` behind saying which of the two modes it
# used, and that file is the only thing this script trusts:
#
#   own    a PRIVATE tmux server (`tmux -L magmux-<id>`) with nothing of the
#          operator's on it, so the session and the server both go.
#
#   adopt  the operator's OWN server, session and window, with two panes of
#          ours borrowed from one of their panes. Only those PANES are killed,
#          each proven ours by two facts that must agree: the `#{pane_id}`
#          demo.sh wrote down, and the pane option demo.sh set on it. NEVER
#          kill-window, NEVER kill-session, NEVER kill-server — the demo was a
#          guest and this is the part where it leaves without breaking anything.
#
# With no state file at all it falls back to the private-server reading, which
# is what every demo before adopt mode existed wrote.
set -u

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ID="${RC_DEMO_ID:-rc-demo}"
STATE="${RC_DEMO_STATE:-$ROOT/.task-grids/$ID}"
SESSION="magmux-$ID"
TMUX_SOCK="magmux-$ID"
TMUX_STATE="$STATE/tmux.state"

G=$'\033[90m'; N=$'\033[0m'
did=0
note() { printf '%s\n' "demo:rc:stop: $*" >&2; did=1; }

# ── 1. what the launcher left behind ───────────────────────────────────────
mode="own"
panes=""
mark_option="@rc_demo"
mark="$ID"
socket_path=""
server_pid=""
if [ -f "$TMUX_STATE" ]; then
  # Parsed line by line with no `eval` and no `source`: this file names things
  # that get killed, and a state file is still a file.
  while IFS='=' read -r k v; do
    case "$k" in
      mode)        mode="$v" ;;
      panes)       panes="$v" ;;
      mark_option) mark_option="$v" ;;
      mark)        mark="$v" ;;
      socket_path) socket_path="$v" ;;
      server_pid)  server_pid="$v" ;;
      session)     SESSION="$v" ;;
      socket_label) TMUX_SOCK="$v" ;;
    esac
  done <"$TMUX_STATE"
fi

if [ "$mode" = "adopt" ]; then
  # Reach the operator's server by its SOCKET PATH, recorded at launch. `-S`
  # rather than `-L`, and rather than relying on $TMUX, because this script may
  # well be run from a different terminal — or from outside tmux entirely —
  # when the launcher's pane is the thing that died.
  tms() { tmux -S "$socket_path" "$@"; }

  if [ -z "$socket_path" ] || [ ! -S "$socket_path" ]; then
    note "the demo recorded an adopted tmux server at ${socket_path:-<nothing>}, and there is no socket there now — nothing to kill"
  else
    # THE SERVER INCARNATION. Pane ids restart at %0 when a tmux server
    # restarts, so an id from a dead server is a live id belonging to somebody
    # else's shell. A pid that does not match means: do nothing, and say so.
    now_pid="$(tms display-message -p '#{pid}' 2>/dev/null)"
    if [ -n "$server_pid" ] && [ -n "$now_pid" ] && [ "$server_pid" != "$now_pid" ]; then
      note "the tmux server on $socket_path has restarted since the demo ran (pid $now_pid, not $server_pid) — refusing to kill panes by stale ids"
    else
      for p in $panes; do
        [ -n "$p" ] || continue
        got="$(tms display-message -p -t "$p" "#{$mark_option}" 2>/dev/null)"
        if [ "$got" = "$mark" ]; then
          tms kill-pane -t "$p" 2>/dev/null && note "killed the demo's pane $p (your window, session and tmux server are untouched)"
        fi
      done
    fi
  fi
else
  # 1b. the tmux session, and the private server it lives on. `kill-server`
  #     rather than only `kill-session` because the server is this demo's own —
  #     created with `tmux -L magmux-<id>` — so nothing of the operator's is on
  #     it.
  if tmux -L "$TMUX_SOCK" has-session -t "$SESSION" 2>/dev/null; then
    tmux -L "$TMUX_SOCK" kill-session -t "$SESSION" 2>/dev/null
    note "killed the tmux session $SESSION (and with it magmux and the terminal client)"
  fi
  tmux -L "$TMUX_SOCK" kill-server 2>/dev/null && note "killed the private tmux server $TMUX_SOCK"
fi

# ── 2. the static server, by the pid it wrote down ─────────────────────────
# It has a ppid watchdog of its own and exits when it is orphaned, so this is
# usually already done; doing it anyway is what makes the script trustworthy
# rather than probably fine.
if [ -f "$STATE/serve.pid" ]; then
  pid="$(cat "$STATE/serve.pid" 2>/dev/null)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null
    note "killed the static server (pid $pid)"
  fi
fi

# ── 3. the credentials, by name, before the directory ──────────────────────
# So a failure to remove the directory still cannot leave a token behind.
for t in "$STATE/view.token" "$STATE/magmux-$ID.token"; do
  [ -e "$t" ] && rm -f "$t" && note "removed $t"
done

if [ -d "$STATE" ]; then
  rm -rf "$STATE" && note "removed $STATE"
fi

[ "$did" = "1" ] || printf '%sdemo:rc:stop: nothing was running — no session %s, no %s%s\n' "$G" "$SESSION" "$STATE" "$N" >&2
exit 0
