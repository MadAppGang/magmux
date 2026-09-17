#!/bin/sh
# P7 gates. Run from the worktree root; the suite runs ALONE.
#
#   sh scripts/p7-gates.sh > ai-docs/sessions/<session>/validation/p7-gates.txt 2>&1
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

echo "=== the firebase suite, verbose"
go test -race -count=1 -v ./transport/firebase/
echo "exit=$?"
echo

echo "=== the import direction still holds (transport/... must not import mux)"
go test -race -count=1 -run TestImportDirection -v ./cmd/magmux/
echo "exit=$?"
echo

echo "=== go list -deps ./transport/firebase | grep MadAppGang   (mux must NOT appear)"
go list -deps ./transport/firebase | grep MadAppGang
echo

echo "=== the EMULATOR end-to-end case, against the real RTDB emulator"
MAGMUX_FIREBASE_EMULATOR=1 go test -count=1 -timeout 600s -v -run TestEmulatorEndToEnd ./transport/firebase/
echo "exit=$?"
echo
