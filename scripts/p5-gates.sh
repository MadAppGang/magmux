#!/bin/sh
# P5 gates. Run from the worktree root; the suite runs ALONE.
#
#   sh scripts/p5-gates.sh > ai-docs/sessions/<session>/validation/p5-gates.txt 2>&1
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

echo "=== go list -deps ./plugin | grep MadAppGang   (mux must NOT appear)"
go list -deps ./plugin | grep MadAppGang
echo

echo "=== the plugin suites, verbose"
go test -race -count=1 -v ./plugin/ ./protocol/
echo "exit=$?"
echo

echo "=== the plugin SDK, verbose"
go test -race -count=1 -v -run 'TestPluginSDK|TestNewPluginRefuses' ./client/
echo "exit=$?"
echo

echo "=== the socket-level plugin cases, verbose"
go test -race -count=1 -v \
  -run 'TestPluginRegistrationIsValidated|TestPluginInvokeAnswersTimesOutAndCancels|TestPluginEventsAreGated|TestPluginSelfOpenPaneNeedsRegistration|TestPluginSnapshotAndResultsAgree|TestPluginDeathUnregistersAndFailsInFlight|TestPluginDeathLeavesThePaneObservedByTheTerminal|TestSpawnedPluginLeavesNoOrphan|TestSpawnedPluginLogsToItsOwnFile|TestViewOpGrantsOnePluginOpToViewers' \
  ./mux/
echo "exit=$?"
echo

echo "=== the ticket-runner demo end to end (timings are logged)"
go test -race -count=1 -v -run TestTicketRunnerDemoDrivesASessionFromBothTransports ./mux/
echo "exit=$?"
echo

echo "=== ps: no plugin orphans left behind by this run (want 0)"
# The bracket keeps grep's own command line from matching itself, which would
# report one orphan on a machine that has none.
ps -Ao pid=,command= | grep -c 'magmux-orphan-prob[e]'
echo
