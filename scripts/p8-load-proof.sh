#!/bin/sh
# The proof for the three load-sensitive tests: the whole suite, three times,
# WITH deliberate parallel load on every core.
#
# The load is two kinds on purpose. `yes > /dev/null` per core takes the CPU
# away, which is what makes a test tuned to an idle laptop fail. A continuous
# `go build ./...` in a scratch GOCACHE takes the CPU AND the disk, and it
# competes for the same toolchain locks the test binaries need — which is much
# closer to what a developer's machine is doing when these tests actually flake.
#
#   sh scripts/p8-load-proof.sh [runs] > .../validation/p8-load-proof.txt 2>&1
set -u

RUNS=${1:-3}
NCPU=$(sysctl -n hw.ncpu 2>/dev/null || nproc)
CACHE=/tmp/magmux-p8-loadcache

echo "=== load: $NCPU spinners + a continuous go build ./... on $NCPU cores"
echo "=== uname: $(uname -a)"
echo

i=0
while [ "$i" -lt "$NCPU" ]; do
	yes >/dev/null 2>&1 &
	i=$((i + 1))
done

(
	while :; do
		GOCACHE=$CACHE go build ./... >/dev/null 2>&1
		sleep 1
	done
) &

# shellcheck disable=SC2064
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$CACHE"' EXIT INT TERM

echo "=== load average before the first run"
uptime
echo

fails=0
k=0
while [ "$k" -lt "$RUNS" ]; do
	k=$((k + 1))
	echo "=================================================================="
	echo "=== run $k/$RUNS   go test -race -count=1 ./...   UNDER LOAD"
	start=$(date +%s)
	go test -race -count=1 ./... 2>&1
	rc=$?
	end=$(date +%s)
	echo "run $k exit=$rc elapsed=$((end - start))s"
	[ "$rc" -eq 0 ] || fails=$((fails + 1))
	echo
	# TestControllerSnapshotReachesAwaitingInput is the fourth: it was not on the
	# list, and the first run of this very script under load is what found it.
	echo "--- the four tests this phase fixed, verbose, in the same run"
	go test -race -count=1 -v \
		-run 'TestPanelVisibilityAtStartup|TestOneShotSendSurvivesClose|TestControllerSnapshotReachesAwaitingInput' ./mux/ 2>&1 |
		grep -E '^(=== RUN|--- (PASS|FAIL|SKIP)|ok|FAIL|PASS)|sockdir_test'
	go test -race -count=1 -v -run 'TestReapStaleSocketsAtScale' ./sockdir/ 2>&1 |
		grep -E '^(=== RUN|--- (PASS|FAIL|SKIP)|ok|FAIL|PASS)|swept'
	echo
	echo "--- load average at the end of run $k"
	uptime
	echo
done

echo "=================================================================="
echo "=== $((RUNS - fails))/$RUNS full-suite runs passed under load"
exit "$fails"
