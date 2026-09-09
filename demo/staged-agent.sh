#!/usr/bin/env bash
# A staged agent pane, for screenshots and for demoing the control panel
# without spending model budget.
#
# It is NOT Claude Code. What makes it useful is that it is not a mock either:
# it files a REAL Claude Code transcript under
# $HOME/.claude/projects/<encoded-cwd>/staged.jsonl, in the exact JSONL shape
# controller_claude.go parses. So magmux attaches a real ClaudeCodeController
# to this pane, discovers the transcript, and every `◀ IN` row in the control
# panel is a turn magmux itself observed — never a row the demo wrote.
#
# showcase.sh symlinks this onto PATH as `claude`, because two other pieces of
# real magmux keyed on that name have to fire:
#   - claudeCodeFactory (controller_claude.go) attaches a controller only to a
#     pane whose command mentions `claude`;
#   - extractClaudeCwd parses the `cd <dir> &&` prefix to find the project dir.
#
# Instructions arrive on stdin, so a `send` over the socket drives it.
set -u

# This script writes a Claude Code transcript into $HOME/.claude/projects/,
# and it is only safe because showcase.sh repoints HOME at a scratch directory
# first. Run directly, it would write into the REAL ~/.claude — and not
# harmlessly: the staged transcript would be the newest file in the developer's
# own project directory, which is exactly what controller_claude.go's mtime and
# content-matching discovery looks for. A live pane in the same cwd could be
# handed the staged turns, and a mis-discovered transcript strands a controller
# in `starting` silently and forever.
#
# So refuse rather than trust the caller. demo/README.md promises this
# isolation; this check is what makes the promise true instead of merely
# documented.
if [ "${MAGMUX_DEMO_HOME:-}" != "1" ]; then
  echo "demo/staged-agent.sh: refusing to run directly." >&2
  echo "  It files a Claude Code transcript under \$HOME/.claude/projects/ and" >&2
  echo "  needs HOME repointed at a scratch directory first." >&2
  echo "  Run demo/showcase.sh (or 'task demo'), which sets that up." >&2
  exit 1
fi

PROJ="$HOME/.claude/projects/$(printf '%s' "$PWD" | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$PROJ"
TR="$PROJ/staged.jsonl"
: > "$TR"

MODEL="claude-opus-5"
NAME="$(basename "$PWD")"
STEP_SLEEP=0.5

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
esc() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# ── transcript writers: the four entry shapes applyLine cares about ────────
tr_user() {
  printf '{"type":"user","cwd":"%s","timestamp":"%s","message":{"role":"user","content":"%s"}}\n' \
    "$(esc "$PWD")" "$(now)" "$(esc "$1")" >> "$TR"; }
tr_tool() {
  printf '{"type":"assistant","timestamp":"%s","message":{"role":"assistant","model":"%s","content":[{"type":"tool_use","id":"t%s","name":"%s","input":{"command":"%s"}}]}}\n' \
    "$(now)" "$MODEL" "$RANDOM" "$(esc "$1")" "$(esc "$2")" >> "$TR"; }
tr_text() {
  printf '{"type":"assistant","timestamp":"%s","message":{"role":"assistant","model":"%s","content":[{"type":"text","text":"%s"}]}}\n' \
    "$(now)" "$MODEL" "$(esc "$1")" >> "$TR"; }
# stop_hook_summary is what moves the controller to awaiting_input, which is
# what makes pollControllers file an IN row. Without it the pane looks busy
# forever, exactly as a real session with no Stop hook does.
tr_stop() {
  printf '{"type":"system","subtype":"stop_hook_summary","timestamp":"%s"}\n' "$(now)" >> "$TR"; }

# ── screen: colours chosen to read on a LIGHT background ───────────────────
C_DIM=$'\033[38;5;245m'; C_OFF=$'\033[0m'; C_B=$'\033[1m'
C_ACC=$'\033[38;5;25m';  C_OK=$'\033[38;5;28m'; C_TOOL=$'\033[38;5;130m'

tool()  { printf '  %s⏺%s %s%s%s  %s\n' "$C_TOOL" "$C_OFF" "$C_B" "$1" "$C_OFF" "$C_DIM$2$C_OFF"; sleep "$STEP_SLEEP"; }
done_() { printf '    %s└ %s%s\n' "$C_OK" "$1" "$C_OFF"; }
idle()  { printf '\n%s waiting for the controller%s\n' "$C_DIM" "$C_OFF"; }

banner() {
  printf '%s╭─%s claude code %s%s· %s ·%s\n' "$C_ACC" "$C_OFF" "$C_B" "$C_ACC" "$NAME" "$C_OFF"
  printf '%s│%s  %s   %sworker/%s%s\n' "$C_ACC" "$C_OFF" "$MODEL" "$C_DIM" "$NAME" "$C_OFF"
  printf '%s╰─%s\n' "$C_ACC" "$C_OFF"
}

# turn "<instruction>" "TOOL|INPUT|RESULT" ... "<final reply>"
turn() {
  local prompt="$1"; shift
  printf '\n%s>%s %s\n' "$C_ACC$C_B" "$C_OFF" "$prompt"
  tr_user "$prompt"
  sleep 0.4
  while [ $# -gt 1 ]; do
    IFS='|' read -r t inp res <<< "$1"
    tool "$t" "$inp"; tr_tool "$t" "$inp"
    [ -n "$res" ] && done_ "$res"
    shift
  done
  printf '\n  %s\n' "$1"
  tr_text "$1"; tr_stop
}

banner; sleep 0.6

# The opening turn, so no pane is ever photographed empty.
case "$NAME" in
  reviewer) turn "review the scrollback capture path for lock-order bugs" \
      "Read|capture.go|318 lines" "Grep|treeMu.RLock in capture.go|2 hits" \
      "capture.go:118 walks a row past len(row) on a shrunk ring." ;;
  tests)    turn "run the scrollback suite and report failures" \
      "Bash|go test -run Scrollback ./...|ok  0.412s" "Bash|go test -run Allocation ./...|ok  0.207s" \
      "12 passed, 0 failed. Allocation test holds at 0 allocs/op." ;;
  docs)     turn "check CLAUDE.md matches the scrollback invariants" \
      "Read|CLAUDE.md|1 match" \
      "viewRow is documented; the len(row) bound the reviewer found is not." ;;
  *)        turn "introduce yourself" "Bash|hostname|ok" "Staged agent in $NAME." ;;
esac
idle

# Driven turns are slower on purpose. The controller sends a new round every
# few seconds, so at any moment some panes are mid-turn and some are idle
# wearing their ✓ DONE overlay — a screenshot needs that mixed state, not
# three finished panes.
STEP_SLEEP=3.5
while IFS= read -r line; do
  [ -z "$line" ] && continue
  case "$NAME" in
    reviewer) turn "$line" "Read|capture.go:100-140|41 lines" "Edit|capture.go|+3 -1" \
        "Bash|go build ./...|ok" "Bash|go vet ./capture.go|clean" \
        "Bounded the walk by len(row) in rowText. Build and vet are clean." ;;
    tests)    turn "$line" "Bash|go test ./... -count=1|47 ok" "Bash|go test -race ./...|ok  6.2s" \
        "Bash|go test -bench Scrollback|0 allocs/op" \
        "47 pass, race clean, benchmark still allocation-free." ;;
    docs)     turn "$line" "Read|CLAUDE.md|scrollback section" "Edit|CLAUDE.md|+4" \
        "Added the len(row) bound to the scrollback invariants." ;;
    *)        turn "$line" "Bash|true|ok" "Acknowledged." ;;
  esac
  idle
done
