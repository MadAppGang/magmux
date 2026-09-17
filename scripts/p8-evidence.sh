#!/bin/sh
# Save one evidence file per validation criterion, under the session's
# validation/ directory, with the names validation-criteria.md asks for.
#
# Each case runs in its own process against the SAME prebuilt binary, so the
# transcripts are of one build.
#
#   sh scripts/p8-evidence.sh ai-docs/sessions/<session>/validation
set -u

OUT=${1:?usage: p8-evidence.sh <validation-dir>}
mkdir -p "$OUT"

go build -o magmux ./cmd/magmux || exit 1
MAGMUX_BIN="$(pwd)/magmux"
export MAGMUX_BIN MAGMUX_FIREBASE_EMULATOR=1

run() {
	file=$1
	case=$2
	printf '=== %s\n' "$case"
	bun "test/rc/$case" >"$OUT/$file" 2>&1
	rc=$?
	printf '    exit=%s -> %s\n' "$rc" "$OUT/$file"
	grep '^__RC__' "$OUT/$file" | sed 's/^/    /'
	return $rc
}

fails=0
run http.txt case1-http.ts || fails=$((fails + 1))
run ws.txt case2-ws.ts || fails=$((fails + 1))
run firebase.txt case3-firebase.ts || fails=$((fails + 1))
run mcp.txt case4-mcp.ts || fails=$((fails + 1))
run plugin.txt case5-plugin.ts || fails=$((fails + 1))
run slow-client.txt case6-slow-client.ts || fails=$((fails + 1))

echo
echo "=== $fails criteria failed"
exit "$fails"
