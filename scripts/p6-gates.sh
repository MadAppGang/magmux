#!/bin/sh
# P6 gates. Run from the worktree root; the suite runs ALONE.
#
#   sh scripts/p6-gates.sh > ai-docs/sessions/<session>/validation/p6-gates.txt 2>&1
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

echo "=== the MCP suite, verbose (the P6 cases and the nine static tools together)"
go test -race -count=1 -v ./mcp/
echo "exit=$?"
echo

echo "=== the client suite, verbose (ops/call/watch and the plugin-event ring)"
go test -race -count=1 -v ./client/
echo "exit=$?"
echo

echo "=== end to end against a REAL magmux and a REAL plugin (timings are logged)"
go test -race -count=1 -v -run TestMCPServerDrivesARealMagmux ./mcp/
echo "exit=$?"
echo

echo "=== the import direction still holds (mcp must not import mux)"
go test -race -count=1 -run TestImportDirection -v ./cmd/magmux/
echo "exit=$?"
echo

echo "=== go list -deps ./mcp | grep MadAppGang   (mux must NOT appear)"
go list -deps ./mcp | grep MadAppGang
echo
