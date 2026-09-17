#!/bin/sh
# P7 spike: confirm the RTDB emulator's admin bypass and its `?auth=` ID-token
# shape before the e2e test depends on either.
#
#   sh scripts/p7-emulator-spike.sh
set -u

DIR="$(cd "$(dirname "$0")/.." && pwd)"
LOG=/tmp/magmux-p7-emulator.log
BASE=http://127.0.0.1:9000
NS=demo-magmux-default-rtdb

# firebase-tools 15.19.1 refuses a JDK older than 21, and the `java` on PATH
# here is 17. Point at the 21 keg explicitly rather than changing the machine's
# default, which is not this script's business.
if [ -x /opt/homebrew/opt/openjdk@21/bin/java ]; then
  JAVA_HOME=/opt/homebrew/opt/openjdk@21
  PATH="$JAVA_HOME/bin:$PATH"
  export JAVA_HOME PATH
fi

cd "$DIR/examples/firebase" || exit 1
firebase emulators:start --only database --project demo-magmux >"$LOG" 2>&1 &
EMU=$!
trap 'kill -TERM -$EMU 2>/dev/null; kill $EMU 2>/dev/null' EXIT INT TERM

echo "waiting for the emulator (pid $EMU); log: $LOG"
i=0
while [ $i -lt 240 ]; do
  if curl -fsS "$BASE/.json?ns=$NS" -H 'Authorization: Bearer owner' >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1))
  sleep 1
done
echo "ready after ${i}s"

OWNER_TOKEN=$(node "$DIR/scripts/p7-idtoken.js" owner-uid-1 demo-magmux)
OTHER_TOKEN=$(node "$DIR/scripts/p7-idtoken.js" not-an-owner demo-magmux)
echo "owner token: $OWNER_TOKEN"

echo
echo "=== 1. admin write of the owner list"
curl -sS -X PUT "$BASE/magmux/hosts/spike-host/owners.json?ns=$NS" \
  -H 'Authorization: Bearer owner' -d '{"owner-uid-1":true}' -w '\nHTTP %{http_code}\n'

echo
echo "=== 2. owner READ of the owner list, ?auth="
curl -sS "$BASE/magmux/hosts/spike-host/owners.json?ns=$NS&auth=$OWNER_TOKEN" -w '\nHTTP %{http_code}\n'

echo
echo "=== 3. owner READ of the owner list, Authorization: Bearer <idtoken>"
curl -sS "$BASE/magmux/hosts/spike-host/owners.json?ns=$NS" \
  -H "Authorization: Bearer $OWNER_TOKEN" -w '\nHTTP %{http_code}\n'

echo
echo "=== 4. owner READ, ?access_token="
curl -sS "$BASE/magmux/hosts/spike-host/owners.json?ns=$NS&access_token=$OWNER_TOKEN" -w '\nHTTP %{http_code}\n'

echo
echo "=== 5. non-owner READ, ?auth= (must be refused)"
curl -sS "$BASE/magmux/hosts/spike-host/owners.json?ns=$NS&auth=$OTHER_TOKEN" -w '\nHTTP %{http_code}\n'

CMD='{"uid":"owner-uid-1","ts":1780000000000,"nonce":"aaaaaaaaaaaaaaaa1234","op":"list","args":"{}","sig":"0000000000000000000000000000000000000000000000000000000000000000"}'

echo
echo "=== 6. owner WRITE of a command, ?auth= (must succeed)"
curl -sS -X PUT "$BASE/magmux/hosts/spike-host/sessions/s1/commands/-Nspike1.json?ns=$NS&auth=$OWNER_TOKEN" \
  -d "$CMD" -w '\nHTTP %{http_code}\n'

echo
echo "=== 7. non-owner WRITE, ?auth= (must be refused BY THE RULES)"
curl -sS -X PUT "$BASE/magmux/hosts/spike-host/sessions/s1/commands/-Nspike2.json?ns=$NS&auth=$OTHER_TOKEN" \
  -d '{"uid":"not-an-owner","ts":1780000000000,"nonce":"bbbbbbbbbbbbbbbb1234","op":"list","args":"{}","sig":"0000000000000000000000000000000000000000000000000000000000000000"}' \
  -w '\nHTTP %{http_code}\n'

echo
echo "=== 8. owner WRITE claiming somebody else's uid (must fail .validate)"
curl -sS -X PUT "$BASE/magmux/hosts/spike-host/sessions/s1/commands/-Nspike3.json?ns=$NS&auth=$OWNER_TOKEN" \
  -d '{"uid":"somebody-else","ts":1780000000000,"nonce":"cccccccccccccccc1234","op":"list","args":"{}","sig":"0000000000000000000000000000000000000000000000000000000000000000"}' \
  -w '\nHTTP %{http_code}\n'

echo
echo "=== 9. owner OVERWRITE of an existing command (create-only; must be refused)"
curl -sS -X PUT "$BASE/magmux/hosts/spike-host/sessions/s1/commands/-Nspike1.json?ns=$NS&auth=$OWNER_TOKEN" \
  -d "$CMD" -w '\nHTTP %{http_code}\n'

echo
echo "=== 10. owner WRITE anywhere else in the session (must be refused)"
curl -sS -X PUT "$BASE/magmux/hosts/spike-host/sessions/s1/meta.json?ns=$NS&auth=$OWNER_TOKEN" \
  -d '{"alive":false}' -w '\nHTTP %{http_code}\n'

echo
echo "=== 11. unauthenticated WRITE (must be refused)"
curl -sS -X PUT "$BASE/magmux/hosts/spike-host/sessions/s1/commands/-Nspike4.json?ns=$NS" \
  -d "$CMD" -w '\nHTTP %{http_code}\n'

echo
echo "=== database-debug.log tail"
tail -40 database-debug.log 2>/dev/null
