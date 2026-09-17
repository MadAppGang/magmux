#!/bin/sh
# P7 smoke: the --firebase FLAG, through the real binary.
#
# The Go tests drive the adapter directly. This is the only thing that exercises
# the CLI wiring — flag parsing, prepareFirebase before init(), startFirebase
# after the layout, and the final flush in shutdownSocket — against a real
# database.
#
#   sh scripts/p7-smoke.sh
set -u

DIR="$(cd "$(dirname "$0")/.." && pwd)"
LOG=/tmp/magmux-p7-smoke-emulator.log
OUT=/tmp/magmux-p7-smoke-magmux.log
BASE=http://127.0.0.1:9000
NS=demo-magmux-default-rtdb
HOST=smoke-host
CFG=/tmp/magmux-p7-smoke-config.json

if [ -x /opt/homebrew/opt/openjdk@21/bin/java ]; then
  JAVA_HOME=/opt/homebrew/opt/openjdk@21
  PATH="$JAVA_HOME/bin:$PATH"
  export JAVA_HOME PATH
fi

echo "=== build"
cd "$DIR" || exit 1
go build -o /tmp/magmux-p7-smoke ./cmd/magmux || exit 1
echo ok

cat >"$CFG" <<JSON
{"root":"magmux","host":"$HOST",
 "emulator":{"host":"127.0.0.1:9000","ns":"$NS"},
 "frameFps":2,
 "commands":{"enabled":false}}
JSON

cd "$DIR/examples/firebase" || exit 1
firebase emulators:start --only database --project demo-magmux >"$LOG" 2>&1 &
EMU=$!
trap 'kill -TERM -$EMU 2>/dev/null; kill $EMU 2>/dev/null' EXIT INT TERM

i=0
while [ $i -lt 300 ]; do
  if curl -fsS "$BASE/.json?ns=$NS" -H 'Authorization: Bearer owner' >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1))
  sleep 1
done
echo "=== emulator ready after ${i}s"

echo
echo "=== magmux --headless -w -e ... --firebase $CFG"
cd "$DIR" || exit 1
/tmp/magmux-p7-smoke --headless -w --id p7smoke \
  -e 'echo MAGMUX_P7_MIRROR_OK; sleep 1' --label smoke \
  --firebase "$CFG" </dev/null >"$OUT" 2>&1
echo "exit=$?"
echo "--- magmux stderr/stdout"
cat "$OUT"

echo
echo "=== the sessions this host wrote"
curl -sS "$BASE/magmux/hosts/$HOST/sessions.json?ns=$NS&shallow=true" \
  -H 'Authorization: Bearer owner' -w '\n'

echo
echo "=== the whole mirrored session"
curl -sS "$BASE/magmux/hosts/$HOST.json?ns=$NS" -H 'Authorization: Bearer owner' \
  | python3 -m json.tool 2>/dev/null || curl -sS "$BASE/magmux/hosts/$HOST.json?ns=$NS" -H 'Authorization: Bearer owner'
echo
