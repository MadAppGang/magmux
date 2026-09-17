#!/usr/bin/env bash
# Pane A's command: the magmux under demonstration, with its whole flag set in
# one readable place.
#
# It is a separate file rather than a string inside `demo.sh` for one reason:
# these flags ARE the demo's security posture, and a reviewer should be able to
# read them without reading a launcher. `demo/rc/selftest.ts` asserts the two
# that a typo would silently weaken (`--view-token-file` and both
# `--allow-origin` values), so a mistake here fails a check rather than a demo.
#
# It is started by `demo/rc/demo.sh` and takes its three variable parts from the
# environment, because two of them are kernel-chosen ports and a directory that
# does not exist until the launcher makes it:
#
#   RC_DEMO_STATE      the run's state directory: tokens, logs, the url file
#   RC_DEMO_ID         magmux's --id, which makes the socket path knowable
#   RC_DEMO_WEB_PORT   the static server's port, which --allow-origin must name
#
# `exec`, so this script leaves no shell between tmux and magmux: the pane's
# process IS magmux, `remain-on-exit` shows magmux's own exit, and a signal
# reaches the thing it was aimed at.
set -u

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

need() {
  if [ -z "${2:-}" ]; then
    printf 'demo/rc/server.sh: %s is not set — start this through demo/rc/demo.sh\n' "$1" >&2
    exit 1
  fi
}
need RC_DEMO_STATE "${RC_DEMO_STATE:-}"
need RC_DEMO_ID "${RC_DEMO_ID:-}"
need RC_DEMO_WEB_PORT "${RC_DEMO_WEB_PORT:-}"

STATE="$RC_DEMO_STATE"
ID="$RC_DEMO_ID"
WEB="$RC_DEMO_WEB_PORT"

# cwd is the repo, so `-e demo/rc/welcome.sh` resolves and the pane's shell
# opens somewhere a reader recognises.
cd "$ROOT" || exit 1

if [ ! -x ./magmux ]; then
  printf 'demo/rc/server.sh: ./magmux is not built — run `go build -o magmux ./cmd/magmux`\n' >&2
  exit 1
fi

# stderr goes to a FILE, not to the pane. That is the readiness channel: magmux
# prints `magmux: listening on <url>` there once the tokens are written, the TLS
# pair is loaded and the listener has bound — in that order — so the line is a
# guarantee and not a hint (mux/remote.go:16-24, 236). The launcher polls the
# file for it and writes the URL where the browser page can reach it. It also
# keeps magmux's own startup chatter off a screen the demo is about to mirror.
#
#   --listen 127.0.0.1:0   loopback, kernel-chosen port, so two demos can run
#                          side by side and neither is reachable off the machine
#   --id                   makes the socket path knowable before magmux runs
#   --view-token-file      the READ-ONLY credential both clients hold. It is
#                          never generated — magmux refuses to invent a
#                          read-only capability — so `serve.ts` mints the 0600
#                          file before anything binds
#   --sock-dir $STATE      one directory to remove at exit; also keeps this
#                          demo's socket out of the operator's /tmp
#   --allow-origin ×2      the static server's origin under both spellings a
#                          browser may use. magmux refuses any other Origin
#                          (transport/httpapi/server.go:343)
#   --plugin …             the ticket-runner example, so the driver's menu has
#                          a real plugin op to call (`ticket.run_ticket`). Its
#                          log goes to {sock-dir}/magmux-{id}.plugin-ticket.log
#                          — magmux never lets a plugin write to the terminal,
#                          which is what keeps it out of a pane the demo is
#                          about to mirror. A plugin that fails to start is
#                          reported and skipped, not fatal, and the driver's
#                          item 10 checks `ops` and says so rather than erroring.
#
#                          The trailing `--demo-id "$ID"` is ignored by the
#                          plugin (its SDK reads only MAGMUX_SOCK and
#                          MAGMUX_PLUGIN_TOKEN from the environment). It is
#                          there so the plugin process carries this run's unique
#                          id in its argv like everything else this demo starts,
#                          which is the handle stop.sh, the selftest and the
#                          driver's item 12 all use — and the reason none of them
#                          has to `pkill -f magmux`.
#   -e … --label demo      one session pane, running the colour card and then
#                          an interactive shell
exec ./magmux \
  --listen 127.0.0.1:0 \
  --id "$ID" \
  --view-token-file "$STATE/view.token" \
  --sock-dir "$STATE" \
  --allow-origin "http://127.0.0.1:$WEB" \
  --allow-origin "http://localhost:$WEB" \
  --plugin "bun $ROOT/examples/plugins/ticket-runner/main.ts --demo-id $ID" \
  -e demo/rc/welcome.sh --label demo \
  2> "$STATE/magmux.err"
