#!/bin/sh
# P3 gates, run from the worktree root. Output is captured verbatim into
# ai-docs/.../validation/p3-gates.txt.
set -u
cd "$(dirname "$0")/.." || exit 1

echo "=== go version ==="
go version
echo
echo "=== test -z \"\$(gofmt -l .)\" ==="
out=$(gofmt -l .)
if [ -z "$out" ]; then echo "PASS: no file needs gofmt"; else echo "FAIL:"; echo "$out"; fi
echo
echo "=== go build ./... ==="
if go build ./...; then echo "PASS"; else echo "FAIL"; fi
echo
echo "=== go vet ./... ==="
if go vet ./...; then echo "PASS"; else echo "FAIL"; fi
echo
echo "=== go test -race -count=1 ./...   (the suite, ALONE) ==="
go test -race -count=1 ./... 2>&1 | grep -v '^\[?'
echo
echo "=== test count ==="
go test -list '.*' ./... 2>/dev/null | grep -c '^\(Test\|Fuzz\|Example\)'
