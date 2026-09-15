#!/bin/sh
# P4 gates. Run from the worktree root; the suite runs ALONE.
#
#   sh scripts/p4-gates.sh > ai-docs/sessions/<session>/validation/p4-gates.txt 2>&1
set -u

echo "=== gofmt -l ."
gofmt -l .
echo "exit=$?"
echo

echo "=== go build ./..."
go build ./...
echo "exit=$?"
echo

echo "=== go vet ./..."
go vet ./...
echo "exit=$?"
echo

echo "=== go test -race -count=1 ./...   (suite ALONE)"
go test -race -count=1 ./...
echo "exit=$?"
echo

echo "=== test count"
go test -list 'Test|Fuzz|Example' ./... 2>/dev/null | grep -cE '^(Test|Fuzz|Benchmark|Example)'
echo

echo "=== go test -race -count=1 -run TestImportDirection -v ./cmd/magmux/"
go test -race -count=1 -run TestImportDirection -v ./cmd/magmux/
echo "exit=$?"
echo

echo "=== go list -deps per guarded package (mux must NOT appear)"
go list -deps ./auth ./transport/ws ./transport/sse ./transport/httpapi | grep MadAppGang
echo

echo "=== FuzzWSReadFrame, 30s"
go test -run FuzzWSReadFrame -fuzz=FuzzWSReadFrame -fuzztime=30s ./transport/ws/
echo "exit=$?"
echo

echo "=== FuzzClosePayload, 15s"
go test -run FuzzClosePayload -fuzz=FuzzClosePayload -fuzztime=15s ./transport/ws/
echo "exit=$?"
echo

echo "=== FuzzSSERead, 30s"
go test -run FuzzSSERead -fuzz=FuzzSSERead -fuzztime=30s ./transport/sse/
echo "exit=$?"
echo

echo "=== the end-to-end cases, verbose (latency is logged)"
go test -race -count=1 -v \
  -run 'TestRemoteRESTDrivesARealSession|TestRemoteWebSocketWatchesAndTypes|TestRemoteWebSocketRefusesAViewTokenTheInputOp|TestRemoteSSEDeliversTheAggregateFirst|TestFinalizeTornWriteSSE|TestHeadlessWithListenStillWritesNothingToStdout|TestBadTLSHandshakeWritesNothingToTheTerminal|TestEnvTokenIsUsedAndNeverWritten|TestNonLoopbackWithoutTLSWarnsBeforeInit|TestBadRemoteConfigurationExitsBeforeInit|TestGeneratedTokenFileIsRemovedAtExit|TestListenWithNoTokenSourceIsStillProtected|TestPaneEnvCarriesNoSecrets' \
  ./mux/
echo "exit=$?"
echo

echo "=== the http/ws/sse/auth suites, verbose"
go test -race -count=1 -v ./auth/ ./transport/... ./sockdir/
echo "exit=$?"
