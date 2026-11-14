#!/usr/bin/env bash
#
# magmux agents demo — the FOCUS action (R5.1) and NOTIFICATIONS (R4.4), by hand.
#
# Run it through the Taskfile, which is the documented entry point:
#
#     task agents:demo                 start it
#     task agents:status -- waiting    make the session ask for a human
#     task agents:focus                move magmux's focus to its pane
#     task agents:down                 remove everything it made
#
# Every subcommand below is also callable directly as `demo/agents.sh <cmd>`;
# the tasks are thin wrappers, the same shape as demo/showcase.sh.
#
# WHY A STAGED SESSION. The notifier fires for every session the registry
# holds, and this machine runs 40+ of them, so a notification test against the
# live registry announces unrelated sessions at you and proves nothing about
# the one you meant to watch. This demo stages its own registry under
# /tmp/magmux-agents-demo and points magmux at it with CLAUDE_CONFIG_DIR, so
# exactly one session exists and every banner is about it.
#
# WHAT IS REAL AND WHAT IS STAGED, in the same terms as demo/README.md:
# the SESSION is staged — one record this script writes and rewrites, holding
# the pid of its own pane process. Everything else is the real thing. The
# registry source, the fsnotify watch, the per-session actor, the notify stage,
# the audit writer, the socket pane table, the ancestry walk and the actuation
# gate are all the shipped code paths, reached exactly as a live session
# reaches them.
#
# WHAT IT TOUCHES, AND NOTHING ELSE:
#
#     /tmp/magmux-agents-demo/cc/sessions/<pid>.json        the staged record
#     /tmp/magmux-agents-demo/cc/projects/-tmp/<sid>.jsonl  optional transcript
#     /tmp/magmux-agents-demo/cfg/magmux/sessions.json      the R6 policy
#     /tmp/magmux-agents-demo/state/magmux/…                the audit trail
#     /tmp/magmux-agents-demo/sock/                         its own socket
#
# Nothing under ~/.claude is read, written or listed. Your own
# ~/.config/magmux/sessions.json is not read: XDG_CONFIG_HOME is redirected.
# Your real audit trail in ~/.local/state/magmux/ is not appended to. The only
# pid written into a record is this script's own pane process, so the liveness
# probe asks about a process the demo started and no other.
#
# /tmp rather than the repo for the sandbox, for two reasons that are both the
# code's: an AF_UNIX socket path must be short, and the audit writer refuses
# any path with a `.claude` element — a worktree checkout lives under one.

set -euo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd -- "$HERE/.." && pwd)"
BIN="${MAGMUX:-$ROOT/magmux}"
ME="$HERE/agents.sh"

BASE=/tmp/magmux-agents-demo
CC="$BASE/cc"
SESS="$CC/sessions"
PROJ="$CC/projects"
CFG="$BASE/cfg"
STATE="$BASE/state"
SOCKD="$BASE/sock"
AUDIT="$STATE/magmux/sessions-audit.jsonl"
PIDFILE="$BASE/session.pid"

SID=1ab1ab1a-0000-4000-8000-0000000001ab
NAME=magmux-demo
# EncodeProjectDir("/tmp"). Transcript discovery scans every project directory
# and addresses the file by session id, so the name only has to be plausible.
SLUG=-tmp

die() { printf '%s\n' "$*" >&2; exit 1; }

# demo_run executes a command against the sandbox. These five variables are the
# whole of the isolation.
demo_run() {
	env MAGMUX_AGENTS=1 \
		CLAUDE_CONFIG_DIR="$CC" \
		XDG_CONFIG_HOME="$CFG" \
		XDG_STATE_HOME="$STATE" \
		MAGMUX_SOCK_DIR="$SOCKD" \
		"$@"
}

# write_record writes the registry record IN PLACE, the way Claude Code writes
# it. Both timestamps move together and are epoch MILLISECONDS, which is what
# the real records carry and what statusSince is read from. Moving them is what
# makes this a new EPISODE: one notification is sent per episode, and an
# episode is identified by the instant the session entered its current status.
write_record() {
	local pid=$1 status=$2 reason=${3:-input} now waiting=''
	now=$(( $(date +%s) * 1000 ))
	if [ "$status" = waiting ]; then
		case $reason in
		permission) waiting=',"waitingFor":"permission prompt"' ;;
		*)          waiting=',"waitingFor":"input needed"' ;;
		esac
	fi
	mkdir -p "$SESS"
	printf '{"pid":%d,"sessionId":"%s","cwd":"/tmp","name":"%s","nameSource":"user","status":"%s","statusUpdatedAt":%d,"updatedAt":%d,"peerProtocol":1,"peerFeatures":[]%s}\n' \
		"$pid" "$SID" "$NAME" "$status" "$now" "$now" "$waiting" > "$SESS/$pid.json"
}

read_pid() {
	[ -f "$PIDFILE" ] || die "the demo is not running: start it with \`task agents:demo\`"
	cat "$PIDFILE"
}

cmd_up() {
	local delay=15s quiet=''
	while [ $# -gt 0 ]; do
		case $1 in
		--delay) delay=${2:?--delay needs a duration like 15s}; shift 2 ;;
		--quiet) quiet=${2:?--quiet needs a window like 22:00-08:00}; shift 2 ;;
		*) die "up: unknown option $1" ;;
		esac
	done
	[ -x "$BIN" ] || die "no binary at $BIN
build one:  (cd $ROOT && go build -o magmux ./cmd/magmux)"

	rm -rf "$BASE"
	mkdir -p "$SESS" "$PROJ" "$CFG/magmux" "$STATE" "$SOCKD"

	local quiet_json='[]'
	[ -n "$quiet" ] && quiet_json="[\"$quiet\"]"
	cat > "$CFG/magmux/sessions.json" <<EOF
{
  "notify": { "enabled": true, "on": ["waiting", "error"], "delay": "$delay", "quiet": $quiet_json },
  "actions": { "navigate": "always", "approve": "ask", "send": "ask", "lifecycle": "ask" }
}
EOF

	printf 'magmux agents demo\n'
	printf '  binary        %s\n' "$BIN"
	printf '  sandbox       %s   (your ~/.claude and ~/.config are untouched)\n' "$BASE"
	printf '  notify.delay  %s\n' "$delay"
	printf '  notify.quiet  %s\n' "${quiet:-none, so it fires at any hour}"
	printf '\n'
	printf 'three panes: the staged session, a console to type in, the live audit trail.\n'
	printf 'in the console pane, run:\n'
	printf '    task agents:status -- waiting    then wait for the banner\n'
	printf '    task agents:focus                then watch focus jump to the session pane\n'
	printf 'press Ctrl-G a for the attention view, Ctrl-G q to quit, then task agents:down\n\n'
	sleep 1

	exec env MAGMUX_AGENTS=1 \
		CLAUDE_CONFIG_DIR="$CC" \
		XDG_CONFIG_HOME="$CFG" \
		XDG_STATE_HOME="$STATE" \
		MAGMUX_SOCK_DIR="$SOCKD" \
		"$BIN" \
		-e "'$ME' pane" --label session \
		-e "cd '$ROOT' && exec '${SHELL:-/bin/zsh}' -l" --label console \
		-e "'$ME' audit --follow" --label audit
}

# cmd_pane runs INSIDE a magmux pane: it is the staged session, and it stays
# alive. The record carries THIS process's pid, which is what makes both
# inclusion gates answer yes — the liveness probe finds the process, and the
# pane gate walks up from it and reaches the pane's own child.
cmd_pane() {
	mkdir -p "$BASE"
	printf '%d\n' "$$" > "$PIDFILE"
	write_record "$$" idle
	printf 'this pane IS the staged session\n'
	printf '  pid         %d\n' "$$"
	printf '  session id  %s\n' "$SID"
	printf '  name        %s\n' "$NAME"
	printf '  record      %s\n\n' "$SESS/$$.json"
	printf 'it starts idle. change it from the console pane:\n'
	printf '    task agents:status -- waiting   a human is needed (notification trigger)\n'
	printf '    task agents:status -- busy      it is working\n'
	printf '    task agents:status -- idle      it has nothing outstanding\n'
	trap 'exit 0' TERM INT
	while :; do sleep 3600; done
}

cmd_status() {
	local status=${1:-} reason=input pid
	case $status in
	idle|busy|waiting) ;;
	*) die "status: want one of idle, busy, waiting" ;;
	esac
	shift
	while [ $# -gt 0 ]; do
		case $1 in
		--reason) reason=${2:?--reason needs input or permission}; shift 2 ;;
		*) die "status: unknown option $1" ;;
		esac
	done
	pid=$(read_pid)
	write_record "$pid" "$status" "$reason"
	printf '%s  %s is now %s\n' "$(date +%H:%M:%S)" "$NAME" "$status"
	if [ "$status" = waiting ]; then
		printf 'the panel row must turn YES at once. the banner follows after notify.delay,\n'
		printf 'unless a quiet window covers now — then the trail records one refused line.\n'
	fi
}

# cmd_errors turns the `error` trigger on or off by staging a transcript. The
# error count comes from records flagged isApiErrorMessage, never from a tool
# result's own is_error: every one of 497 observed occurrences of that second
# flag was an error the session RECOVERED from.
cmd_errors() {
	local state=${1:-} dir="$PROJ/$SLUG" file
	file="$dir/$SID.jsonl"
	case $state in
	off)
		rm -f "$file"
		printf "transcript removed; ERR goes back to '-', which is 'nobody counted'\\n"
		;;
	on)
		mkdir -p "$dir"
		local now
		now=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
		{
			printf '{"type":"assistant","sessionId":"%s","timestamp":"%s","agentColor":"orange","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}\n' "$SID" "$now"
			printf '{"type":"assistant","sessionId":"%s","timestamp":"%s","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error: 500 staged by the demo"}]}}\n' "$SID" "$now"
		} > "$file"
		printf 'staged %s\n' "$file"
		printf 'ERR must become 1 in `task agents:table`\n'
		;;
	*) die "errors: want on or off" ;;
	esac
}

cmd_table() {
	if [ "${1:-}" = --json ]; then
		demo_run "$BIN" agents --once --json
	else
		demo_run "$BIN" agents --once
	fi
}

cmd_focus() {
	printf 'focus the CONSOLE pane first, or you will not see anything move.\n'
	demo_run "$BIN" agents focus "$NAME"
}

# show_audit renders the trail. Lines are keyed <writer pid>#<seq>: `seq`
# counts per WRITER, not per file, so the magmux and each `agents focus` both
# start at 1 in one trail and an outcome's `ref` resolves within its own pid.
show_audit() {
	if command -v jq >/dev/null 2>&1; then
		jq -r '
			"\(.pid)#\(.seq)  \(.event)  \(.verb)  tier=\(.tier // "-")  by=\(.requestedBy // "-")  \(.target.name // .target.id // "-")"
			+ (if .policy.rule then "\n      rule: \(.policy.rule)" else "" end)
			+ (if .outcome then "\n      " + (if .outcome.done then "done" elif .outcome.refused then "refused" else "failed" end)
			     + ": " + ((.outcome.detail // .outcome.error) // "") else "" end)'
	else
		cat
	fi
}

cmd_audit() {
	local follow=no
	case ${1:-} in
	-f|--follow) follow=yes ;;
	'') ;;
	*) die "audit: unknown option $1" ;;
	esac
	printf 'audit trail: %s\n' "$AUDIT"
	printf 'lines are <writer pid>#<seq>. verb=notify with tier=- and by=- is a\n'
	printf 'NOTIFICATION: nothing asked for it and it changes nothing. verb=focus\n'
	printf 'with tier=navigate and by=cli is an ACTION somebody requested.\n\n'
	if [ "$follow" = yes ]; then
		while [ ! -f "$AUDIT" ]; do sleep 0.5; done
		tail -n +1 -f "$AUDIT" | show_audit
		return
	fi
	if [ -f "$AUDIT" ]; then
		show_audit < "$AUDIT"
	else
		printf '(nothing recorded yet)\n'
	fi
}

cmd_down() {
	if [ -d "$BASE" ]; then
		rm -rf "$BASE"
		printf 'removed %s\n' "$BASE"
	else
		printf 'nothing to remove at %s\n' "$BASE"
	fi
}

usage() {
	cat <<'EOF'
magmux agents demo — the focus action and notifications, by hand

Through the Taskfile:

  task agents:demo [-- --delay 15s --quiet 22:00-08:00]
  task agents:status -- waiting|busy|idle [--reason input|permission]
  task agents:errors -- on|off
  task agents:table [-- --json]
  task agents:focus
  task agents:audit
  task agents:down

Directly:

  demo/agents.sh up|status|errors|table|focus|audit|down
  demo/agents.sh pane      (internal: magmux runs this as the staged session)

See demo/agents.md for what each check proves.
EOF
}

case ${1:-} in
up)     shift; cmd_up "$@" ;;
pane)   shift; cmd_pane "$@" ;;
status) shift; cmd_status "$@" ;;
errors) shift; cmd_errors "$@" ;;
table)  shift; cmd_table "$@" ;;
focus)  shift; cmd_focus "$@" ;;
audit)  shift; cmd_audit "$@" ;;
down)   shift; cmd_down "$@" ;;
-h|--help|help|'') usage ;;
*) die "unknown command: $1
run demo/agents.sh --help" ;;
esac
