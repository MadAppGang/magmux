#!/usr/bin/env bash
# Staged multi-agent + controller showcase, for screenshots.
#
#   demo/showcase.sh            three agent panes, panel visible, light theme
#   THEME=dark demo/showcase.sh dark palette instead
#   PANES=4 demo/showcase.sh    add a fourth agent pane
#   STAGGER=4 demo/showcase.sh  drive it harder (default 7s between steps)
#
# Run it in the terminal you want to photograph. Give it about 40 seconds to
# fill the panel, then shoot whenever you like: the controller keeps driving
# for roughly two minutes, so there is always work in flight. Ctrl-G q quits,
# Ctrl-G p hides and shows the panel.
#
# Nothing here touches your real ~/.claude: HOME is repointed at a scratch
# directory, so the staged transcripts and the controller that reads them are
# fully isolated from your live sessions.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MAGMUX="$ROOT/magmux"
STATE="$ROOT/.task-grids/demo"
SOCK_ID="${SOCK_ID:-demo}"
SOCK="${MAGMUX_SOCK_DIR:-/tmp}/magmux-$SOCK_ID.sock"
THEME="${THEME:-light}"
PANES="${PANES:-3}"

if [ ! -x "$MAGMUX" ]; then
  echo "demo: $MAGMUX not built — run 'go build -o magmux .' first" >&2
  exit 1
fi

ALL_AGENTS=(reviewer tests docs planner)
if [ "$PANES" -lt 1 ] || [ "$PANES" -gt ${#ALL_AGENTS[@]} ]; then
  echo "demo: PANES must be 1..${#ALL_AGENTS[@]}" >&2
  exit 1
fi
AGENTS=("${ALL_AGENTS[@]:0:$PANES}")

# Fresh state every run: a stale transcript would be discovered by mtime and
# the panes would open mid-conversation.
rm -rf "$STATE"
mkdir -p "$STATE/bin" "$STATE/home/.claude"
for a in "${AGENTS[@]}"; do mkdir -p "$STATE/work/$a"; done

# The staged agent has to be called `claude` on PATH: claudeCodeFactory
# attaches a controller by matching that name in the pane's command.
ln -sf "$ROOT/demo/staged-agent.sh" "$STATE/bin/claude"

rm -f "$SOCK"

# The controller starts once the socket exists. --id makes that path knowable
# before magmux runs, which is the whole reason the flag exists.
(
  for _ in $(seq 1 60); do [ -S "$SOCK" ] && break; sleep 0.25; done
  [ -S "$SOCK" ] || exit 0
  sleep 3
  PANES="$PANES" "$ROOT/demo/staged-controller.sh" "$SOCK"
) >"$STATE/controller.log" 2>&1 &
DRIVER=$!

args=()
for a in "${AGENTS[@]}"; do
  args+=(-e "cd $STATE/work/$a && claude --dangerously-skip-permissions" --label "$a")
done

# NOT exec: an exec would replace this shell and discard the trap, leaving the
# driver behind when magmux quits. A driver still holding this --id path would
# inject into the next run.
trap 'kill $DRIVER 2>/dev/null; wait $DRIVER 2>/dev/null' EXIT INT TERM

env HOME="$STATE/home" PATH="$STATE/bin:$PATH" \
  "$MAGMUX" --id "$SOCK_ID" -c --theme "$THEME" "${args[@]}"
