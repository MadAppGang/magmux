#!/bin/sh
# P8 gates. Run from the worktree root; the suite runs ALONE.
#
#   sh scripts/p8-gates.sh > ai-docs/sessions/<session>/validation/p8-gates.txt 2>&1
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

echo "=== the three load-sensitive tests, verbose"
go test -race -count=1 -v \
	-run 'TestPanelVisibilityAtStartup|TestOneShotSendSurvivesClose' ./mux/
echo "exit=$?"
go test -race -count=1 -v -run 'TestReapStaleSocketsAtScale' ./sockdir/
echo "exit=$?"
echo

echo "=== the streaming regression P8 found (notify mode dropped a rate-limited change)"
go test -race -count=1 -v \
	-run 'TestNotifyModeDoesNotDropTheChangeItRateLimited|TestNotifyModeSendsNewsNotScreens|TestIdleWatchedPaneCostsNothing' ./mux/
echo "exit=$?"
echo

echo "=== the import direction still holds"
go test -race -count=1 -run TestImportDirection -v ./cmd/magmux/
echo "exit=$?"
echo

echo "=== the demo plugin still compiles"
bun build --target=bun examples/plugins/ticket-runner/main.ts >/dev/null
echo "exit=$?"
echo
